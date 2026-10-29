package switchbot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"tinygo.org/x/bluetooth"
)

// adapter is the part of *bluetooth.Adapter the Client uses. Tests replace
// it with a fake.
type adapter interface {
	Enable() error
	Scan(callback func(*bluetooth.Adapter, bluetooth.ScanResult)) error
	StopScan() error
	Connect(address bluetooth.Address, params bluetooth.ConnectionParams) (device, error)
	SetConnectionStateHandler(func(bluetooth.Device, bluetooth.ConnectionState))
}

// checkTimeout limits how long DiscoverBots spends connecting to one device
// to find out whether it is a Bot.
const checkTimeout = 10 * time.Second

// Client finds and controls SwitchBot Bots through one Bluetooth adapter.
// Create one with [NewClient]. A Client is safe for concurrent use.
type Client struct {
	adapter     adapter
	log         *slog.Logger
	scanHook    func(bluetooth.ScanResult)
	password    []byte
	retries     int
	idleTimeout time.Duration

	// findScan is how long to scan for a Bot that can't be connected to by
	// address. Zero means don't scan: Windows connects by address whether
	// or not it has seen the device, so scanning wouldn't help.
	findScan time.Duration

	// dial opens a connection. It is c.gattDial except in tests.
	dial func(ctx context.Context, addr bluetooth.Address) (link, error)

	// mu serializes scanning and connecting, which most Bluetooth stacks
	// can only do one at a time.
	mu sync.Mutex

	botsMu sync.Mutex
	bots   map[string]*Bot // by addrKey

	linksMu sync.Mutex
	links   map[string]*gattLink // by addrKey, for disconnect events
}

// Option configures a [Client].
type Option func(*Client)

// WithLogger makes the Client log what it does: scanning, connecting,
// commands sent and responses received. Most messages are at debug level.
// By default nothing is logged.
func WithLogger(logger *slog.Logger) Option {
	return func(c *Client) { c.log = logger }
}

// WithPassword sets the password your Bots have in the SwitchBot app. Bots
// without a password reject commands that carry one (ErrNoPasswordSet), so
// use a separate Client for those.
func WithPassword(password string) Option {
	return func(c *Client) { c.password = encodePassword(password) }
}

// WithIdleTimeout sets how long a Bot stays connected after its last
// command. The default is one minute. Zero keeps Bots connected until
// [Bot.Close] or [Client.Close]. The next command reconnects either way.
func WithIdleTimeout(d time.Duration) Option {
	return func(c *Client) { c.idleTimeout = max(d, 0) }
}

// WithRetries sets how many more times a command is tried when connecting
// fails, the command could not be sent, or the Bot is busy. The default is
// 2. A command is only retried when the Bot cannot have carried it out, so
// a press is never repeated by accident.
func WithRetries(n int) Option {
	return func(c *Client) { c.retries = max(n, 0) }
}

// WithAdapter makes the Client use adapter instead of
// bluetooth.DefaultAdapter. The Client installs its own connection state
// handler on it.
func WithAdapter(adapter *bluetooth.Adapter) Option {
	return func(c *Client) { c.adapter = btAdapter{adapter} }
}

// WithScanHook calls fn for every Bluetooth advertisement received while
// scanning, from any device. It is meant for debugging. fn runs on the
// Bluetooth stack's goroutine and must not block or call methods of the
// Client.
func WithScanHook(fn func(bluetooth.ScanResult)) Option {
	return func(c *Client) { c.scanHook = fn }
}

// NewClient enables the Bluetooth adapter and returns a Client that uses it.
// Call [Client.Close] when done, to disconnect from all Bots.
func NewClient(opts ...Option) (*Client, error) {
	c := newClient(btAdapter{bluetooth.DefaultAdapter}, opts...)
	c.log.Debug("enabling Bluetooth adapter")
	if err := c.adapter.Enable(); err != nil {
		return nil, fmt.Errorf("switchbot: enable Bluetooth adapter: %w", err)
	}
	c.adapter.SetConnectionStateHandler(c.onConnectionState)
	return c, nil
}

func newClient(a adapter, opts ...Option) *Client {
	c := &Client{
		adapter:     a,
		retries:     2,
		idleTimeout: time.Minute,
		bots:        map[string]*Bot{},
		links:       map[string]*gattLink{},
	}
	if runtime.GOOS != "windows" {
		c.findScan = findScanTime
	}
	c.dial = c.gattDial
	for _, opt := range opts {
		opt(c)
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	return c
}

// Bot returns the Bot with the given address, without scanning. Nothing is
// sent until its first command, which connects and checks that the device
// really is a Bot (otherwise the command returns [ErrNotABot]).
//
// The address is a MAC address such as "AA:BB:CC:DD:EE:FF" on Windows and
// Linux, and the UUID [Client.DiscoverBots] reports on macOS. Calling Bot
// again with the same address returns the same *Bot.
func (c *Client) Bot(address string) (*Bot, error) {
	addr, err := parseAddress(address)
	if err != nil {
		return nil, err
	}
	return c.bot(addr), nil
}

// bot returns the Bot for addr, creating it if needed.
func (c *Client) bot(addr bluetooth.Address) *Bot {
	c.botsMu.Lock()
	defer c.botsMu.Unlock()
	key := addrKey(addr)
	b, ok := c.bots[key]
	if !ok {
		b = &Bot{client: c, addr: addr}
		c.bots[key] = b
	}
	return b
}

// isVerified reports whether addr is already known to be a Bot.
func (c *Client) isVerified(addr bluetooth.Address) bool {
	c.botsMu.Lock()
	b := c.bots[addrKey(addr)]
	c.botsMu.Unlock()
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.verified
}

// Close disconnects from all Bots. The Client can still be used afterwards;
// Bots reconnect on their next command.
func (c *Client) Close() error {
	c.botsMu.Lock()
	bots := make([]*Bot, 0, len(c.bots))
	for _, b := range c.bots {
		bots = append(bots, b)
	}
	c.botsMu.Unlock()

	var errs []error
	for _, b := range bots {
		errs = append(errs, b.Close())
	}
	return errors.Join(errs...)
}

// DiscoverBots scans for the given duration and returns every Bot in range,
// in the order they were first seen:
//
//	bots, err := client.DiscoverBots(ctx, 10*time.Second)
//
// Only Bots are returned. Devices that don't advertise their type (common on
// Windows) are connected to after the scan to find out; the Bots among them
// stay connected, so their first command is fast. Each check takes at most
// 10 seconds; devices that can't be reached in that time (usually ones at
// the edge of range) are left out and logged.
//
// If ctx is cancelled first, DiscoverBots returns ctx.Err() together with
// the Bots found so far.
func (c *Client) DiscoverBots(ctx context.Context, duration time.Duration) ([]*Bot, error) {
	var (
		mu    sync.Mutex
		index = map[string]int{} // addrKey -> index in seen
		seen  []sighting
	)
	c.mu.Lock()
	err := c.scanLocked(ctx, duration, func(s sighting) bool {
		key := addrKey(s.addr)
		mu.Lock()
		defer mu.Unlock()
		i, ok := index[key]
		switch {
		case !ok:
			c.log.Debug("found SwitchBot device", "address", s.addr.String(), "rssi", s.rssi, "type", s.kind.String())
			index[key] = len(seen)
			seen = append(seen, s)
		case seen[i].kind == kindUnknown && s.kind != kindUnknown:
			seen[i] = s
		}
		return true
	})
	c.mu.Unlock()

	mu.Lock()
	defer mu.Unlock()

	// Devices that didn't advertise their type have to be connected to.
	toCheck := 0
	for _, s := range seen {
		if s.kind == kindUnknown && !c.isVerified(s.addr) {
			toCheck++
		}
	}
	c.log.Debug("scan finished", "switchBotDevices", len(seen), "needConnectionToIdentify", toCheck)

	var bots []*Bot
	checked := 0
	for _, s := range seen {
		switch s.kind {
		case kindOtherSwitchBot:
			c.log.Debug("skipping SwitchBot device that is not a Bot", "address", s.addr.String())
			continue
		case kindUnknown:
			if err != nil {
				continue // cancelled: don't connect to anything
			}
		}
		b := c.bot(s.addr)
		b.mu.Lock()
		b.rssi = s.rssi
		if s.kind == kindBot {
			b.verified = true
		}
		verified := b.verified
		b.mu.Unlock()

		if !verified {
			checked++
			c.log.Debug(fmt.Sprintf("checking device %d of %d: is it a Bot?", checked, toCheck),
				"address", s.addr.String(), "rssi", s.rssi, "timeout", checkTimeout)
			// Limit the time per device, so one at the edge of range can't
			// hold up discovery.
			cctx, cancel := context.WithTimeout(ctx, checkTimeout)
			cerr := b.Connect(cctx)
			cancel()
			switch {
			case errors.Is(cerr, ErrNotABot):
				c.log.Debug("skipping SwitchBot device that is not a Bot", "address", s.addr.String())
				continue
			case cerr != nil && ctx.Err() != nil:
				err = ctx.Err()
				continue
			case cerr != nil:
				c.log.Warn("could not check whether device is a Bot; skipping it (it may be out of range)",
					"address", s.addr.String(), "rssi", s.rssi, "err", cerr)
				continue
			}
		}
		c.log.Debug("found Bot", "address", s.addr.String())
		bots = append(bots, b)
	}
	c.log.Debug("discovery finished", "bots", len(bots))
	return bots, err
}

// scanLocked scans for SwitchBot devices for the given duration, calling fn
// for every advertisement from one, until fn returns false. It returns
// ctx.Err() if ctx is cancelled first. c.mu must be held.
func (c *Client) scanLocked(ctx context.Context, duration time.Duration, fn func(sighting) bool) error {
	if duration <= 0 {
		return fmt.Errorf("switchbot: scan duration must be positive, got %s", duration)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	c.log.Debug("scan started", "duration", duration)
	defer c.log.Debug("scan stopped")

	end := time.Now().Add(duration)
	timer := time.NewTimer(duration)
	defer timer.Stop()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
		case <-timer.C:
		case <-done:
			return
		}
		_ = c.adapter.StopScan() // fails harmlessly if the scan already stopped
	}()

	var stopped atomic.Bool // set when fn asks to stop
	err := c.adapter.Scan(func(_ *bluetooth.Adapter, r bluetooth.ScanResult) {
		if stopped.Load() || ctx.Err() != nil || !time.Now().Before(end) {
			// Also stop from here in case the goroutine's StopScan ran
			// before the scan had started.
			_ = c.adapter.StopScan()
			return
		}
		if c.scanHook != nil {
			c.scanHook(r)
		}
		if s := classify(r); s.kind != kindNotSwitchBot && !fn(s) {
			stopped.Store(true)
			_ = c.adapter.StopScan()
		}
	})
	switch {
	case err != nil:
		return fmt.Errorf("switchbot: scan: %w", err)
	case stopped.Load():
		return nil
	case ctx.Err() != nil:
		c.log.Debug("scan cancelled", "err", ctx.Err())
		return ctx.Err()
	}
	return nil
}
