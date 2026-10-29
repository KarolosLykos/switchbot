package switchbot

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"tinygo.org/x/bluetooth"
)

const (
	// replyTimeout is how long to wait for a Bot to answer a command.
	replyTimeout = 5 * time.Second
	// retryDelay is multiplied by the attempt number between retries.
	retryDelay = 500 * time.Millisecond
)

// Bot is a SwitchBot Bot. Get one from [Client.Bot] or
// [Client.DiscoverBots].
//
// The first command connects to the Bot; later commands reuse the
// connection, and reconnect if it was lost. A Bot is safe for concurrent
// use; its commands run one at a time.
type Bot struct {
	client *Client
	addr   bluetooth.Address

	mu       sync.Mutex
	rssi     int16 // 0 if never seen while scanning
	link     link  // nil when not connected
	verified bool  // known to be a Bot
	idle     *time.Timer
	lastUsed time.Time
}

// Address returns the Bot's Bluetooth address: a MAC address such as
// "AA:BB:CC:DD:EE:FF" on Windows and Linux, a UUID on macOS. Pass it to
// [Client.Bot] to get the same Bot later without scanning.
func (b *Bot) Address() string { return b.addr.String() }

// RSSI returns the signal strength in dBm when the Bot was last seen by
// [Client.DiscoverBots], or 0 if it hasn't been. Below about -85 dBm the Bot
// may be too far away to connect to reliably.
func (b *Bot) RSSI() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return int(b.rssi)
}

// Connected reports whether the Bot is currently connected.
func (b *Bot) Connected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.alive()
}

func (b *Bot) String() string {
	if rssi := b.RSSI(); rssi != 0 {
		return fmt.Sprintf("Bot %s (RSSI %d dBm)", b.Address(), rssi)
	}
	return "Bot " + b.Address()
}

// TurnOn turns the switch on. The Bot must be in [ModeSwitch], otherwise
// it returns [ErrUnsupported]; see [Bot.SetMode].
func (b *Bot) TurnOn(ctx context.Context) error {
	return b.action(ctx, actionOn)
}

// TurnOff turns the switch off. The Bot must be in [ModeSwitch], otherwise
// it returns [ErrUnsupported]; see [Bot.SetMode].
func (b *Bot) TurnOff(ctx context.Context) error {
	return b.action(ctx, actionOff)
}

// Press pushes the button and pulls back. The Bot must be in [ModePress],
// otherwise it returns [ErrUnsupported]; see [Bot.SetMode].
func (b *Bot) Press(ctx context.Context) error {
	return b.action(ctx, actionPress)
}

// ArmDown pushes the arm down and leaves it there.
func (b *Bot) ArmDown(ctx context.Context) error {
	return b.action(ctx, actionArmDown)
}

// ArmUp pulls the arm back up.
func (b *Bot) ArmUp(ctx context.Context) error {
	return b.action(ctx, actionArmUp)
}

// Status asks the Bot for its battery level, firmware version and settings.
func (b *Bot) Status(ctx context.Context) (Status, error) {
	reply, err := b.do(ctx, request(cmdGetBasicInfo))
	if err != nil {
		return Status{}, err
	}
	info, err := parseBasicInfo(reply)
	return info.Status, err
}

// SetMode switches the Bot between [ModePress] and [ModeSwitch]. The arm
// direction (see [Bot.SetInverted]) is kept.
func (b *Bot) SetMode(ctx context.Context, mode Mode) error {
	if mode != ModePress && mode != ModeSwitch {
		return fmt.Errorf("switchbot: invalid mode %v", mode)
	}
	return b.updateMode(ctx, func(st *Status) { st.Mode = mode })
}

// SetInverted sets whether the arm's direction is inverted, for switches
// mounted the other way round. The mode is kept.
func (b *Bot) SetInverted(ctx context.Context, inverted bool) error {
	return b.updateMode(ctx, func(st *Status) { st.Inverted = inverted })
}

// updateMode reads the Bot's mode settings, changes them with change and
// writes them back, without other commands to this Bot in between.
func (b *Bot) updateMode(ctx context.Context, change func(*Status)) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	defer b.touch()

	reply, err := b.doLocked(ctx, request(cmdGetBasicInfo))
	if err != nil {
		return err
	}
	info, err := parseBasicInfo(reply)
	if err != nil {
		return err
	}
	st := info.Status
	change(&st)

	var m byte
	if st.Mode == ModeSwitch {
		m |= modeSwitchBit
	}
	if st.Inverted {
		m |= modeInvertedBit
	}
	reply, err = b.doLocked(ctx, request(cmdSetBasicInfo, strength, m))
	if err != nil {
		return err
	}
	return checkReply(reply)
}

// SetHoldTime sets how long a press holds the button down, from 0 to 255
// seconds, in whole seconds.
func (b *Bot) SetHoldTime(ctx context.Context, d time.Duration) error {
	if d < 0 || d > 255*time.Second {
		return fmt.Errorf("switchbot: hold time %s out of range 0s-255s", d)
	}
	secs := byte(d.Round(time.Second) / time.Second)
	return b.command(ctx, request(cmdExtended, extSetHoldTime, secs))
}

// Connect connects to the Bot now instead of on its first command, and
// checks that the device is a Bot. Calling it is optional.
func (b *Bot) Connect(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	defer b.touch()
	return b.withRetries(ctx, func() error { return b.ensureLink(ctx) })
}

// Close disconnects from the Bot. The Bot can still be used afterwards; its
// next command reconnects.
func (b *Bot) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.idle != nil {
		b.idle.Stop()
	}
	return b.dropLink("closed")
}

func (b *Bot) action(ctx context.Context, action byte) error {
	return b.command(ctx, request(cmdAction, action))
}

// command sends req and checks the Bot's status in its response.
func (b *Bot) command(ctx context.Context, req []byte) error {
	reply, err := b.do(ctx, req)
	if err != nil {
		return err
	}
	return checkReply(reply)
}

// do sends req, connecting first if needed, and returns the response.
func (b *Bot) do(ctx context.Context, req []byte) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	defer b.touch()
	return b.doLocked(ctx, req)
}

// doLocked is do with b.mu already held. A busy Bot is asked again; if it
// stays busy, the error is ErrBusy.
func (b *Bot) doLocked(ctx context.Context, req []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var reply []byte
	err := b.withRetries(ctx, func() error {
		if err := b.ensureLink(ctx); err != nil {
			return err
		}
		var err error
		reply, err = b.exchange(ctx, commandName(req), req)
		if err == nil && len(reply) > 0 && reply[0] == statusBusy {
			return &StatusError{Code: statusBusy}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return reply, nil
}

// withRetries runs fn, running it again after a short wait when it fails
// in a way that means the Bot cannot have carried out the command.
// b.mu must be held.
func (b *Bot) withRetries(ctx context.Context, fn func() error) error {
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil || attempt >= b.client.retries || ctx.Err() != nil || !retryable(err) {
			return err
		}
		wait := time.Duration(attempt+1) * retryDelay
		b.client.log.Debug("retrying", "address", b.Address(), "err", err, "attempt", attempt+1, "wait", wait)
		if err := sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// retryable reports whether err means the command did not reach the Bot or
// the Bot refused it without acting.
func retryable(err error) bool {
	var cerr *connectError
	return errors.As(err, &cerr) || errors.Is(err, errWrite) || errors.Is(err, ErrBusy)
}

// connectError wraps a failure to connect.
type connectError struct{ err error }

func (e *connectError) Error() string { return e.err.Error() }
func (e *connectError) Unwrap() error { return e.err }

// ensureLink connects if not connected, and checks that the device is a
// Bot. b.mu must be held.
func (b *Bot) ensureLink(ctx context.Context) error {
	if b.alive() {
		return nil
	}
	if b.link != nil {
		// Release the lost connection before making a new one.
		_ = b.dropLink("connection lost; reconnecting")
	}

	l, err := b.client.dial(ctx, b.addr)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrServiceNotFound) {
			return err
		}
		return &connectError{err}
	}
	b.link = l
	if b.verified {
		return nil
	}

	reply, err := b.exchange(ctx, "check device type", request(cmdGetBasicInfo))
	if err == nil {
		var info basicInfo
		if info, err = parseBasicInfo(reply); err == nil && !info.isBot() {
			err = ErrNotABot
		}
	}
	if err != nil {
		_ = b.dropLink("not verified")
		if errors.Is(err, ErrConnectionLost) || errors.Is(err, ErrNoResponse) {
			// The check only reads, so it is safe to try again.
			return &connectError{err}
		}
		return err
	}
	b.client.log.Debug("verified device is a Bot", "address", b.Address())
	b.verified = true
	return nil
}

// alive reports whether the Bot has a live connection. b.mu must be held.
func (b *Bot) alive() bool {
	return b.link != nil && isOpen(b.link)
}

// exchange writes req on the current connection and waits for the
// response. b.mu must be held.
func (b *Bot) exchange(ctx context.Context, name string, req []byte) ([]byte, error) {
	log := b.client.log.With("address", b.Address(), "command", name)
	l := b.link

	// Throw away responses that arrived after an earlier command gave up.
	for drained := false; !drained; {
		select {
		case <-l.replies():
		default:
			drained = true
		}
	}

	// Log the request before the password is added, so it never shows up in logs.
	if len(b.client.password) > 0 {
		log.Debug("sending", "bytes", fmt.Sprintf("% X", req), "withPassword", true)
	} else {
		log.Debug("sending", "bytes", fmt.Sprintf("% X", req))
	}
	start := time.Now()
	if err := l.write(withPassword(req, b.client.password)); err != nil {
		_ = b.dropLink("write failed")
		if !errors.Is(err, errWrite) {
			err = fmt.Errorf("%w: %w", errWrite, err)
		}
		return nil, err
	}

	timer := time.NewTimer(replyTimeout)
	defer timer.Stop()
	select {
	case r := <-l.replies():
		log.Debug("received", "bytes", fmt.Sprintf("% X", r), "took", time.Since(start).Round(time.Millisecond))
		return r, nil
	case <-l.done():
		_ = b.dropLink("connection lost")
		return nil, ErrConnectionLost
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		// The connection may be dead without the stack noticing; start
		// afresh next time.
		_ = b.dropLink("no response")
		return nil, ErrNoResponse
	}
}

// dropLink disconnects. b.mu must be held.
func (b *Bot) dropLink(reason string) error {
	if b.link == nil {
		return nil
	}
	l := b.link
	b.link = nil
	b.client.log.Debug("disconnecting", "address", b.Address(), "reason", reason)
	return l.close()
}

// touch records that the Bot was just used and restarts the idle timer.
// b.mu must be held.
func (b *Bot) touch() {
	b.lastUsed = time.Now()
	d := b.client.idleTimeout
	if d <= 0 || b.link == nil {
		return
	}
	if b.idle == nil {
		b.idle = time.AfterFunc(d, b.disconnectIfIdle)
	} else {
		b.idle.Reset(d)
	}
}

func (b *Bot) disconnectIfIdle() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if time.Since(b.lastUsed) >= b.client.idleTimeout {
		_ = b.dropLink("idle")
	}
}

// sleep waits for d, or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
