package switchbot

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tinygo.org/x/bluetooth"
)

// findScanTime is how long to scan for a Bot that can't be connected to by
// address alone (needed on macOS, and on Linux for devices BlueZ hasn't
// seen yet).
const findScanTime = 10 * time.Second

// link is an open connection to a device.
type link interface {
	// write sends a request without waiting for the response.
	write(req []byte) error
	// replies delivers the device's responses.
	replies() <-chan []byte
	// done is closed when the connection is lost.
	done() <-chan struct{}
	// close disconnects.
	close() error
}

// gattLink is a link over a GATT connection.
type gattLink struct {
	dev  bluetooth.Device
	char *bluetooth.DeviceCharacteristic
	in   chan []byte
	lost chan struct{}
	once sync.Once
}

// write sends req once. There is deliberately no fallback to a write with
// response: if the first write failed only after reaching the Bot, a second
// one would repeat the action.
func (l *gattLink) write(req []byte) error {
	if _, err := l.char.WriteWithoutResponse(req); err != nil {
		return fmt.Errorf("%w: %w", errWrite, err)
	}
	return nil
}

func (l *gattLink) replies() <-chan []byte { return l.in }
func (l *gattLink) done() <-chan struct{}  { return l.lost }
func (l *gattLink) markLost()              { l.once.Do(func() { close(l.lost) }) }

func (l *gattLink) close() error {
	l.markLost()
	return l.dev.Disconnect()
}

// gattDial connects to the device at addr and subscribes to its responses.
func (c *Client) gattDial(ctx context.Context, addr bluetooth.Address) (link, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	dev, err := c.connect(ctx, addr)
	if err != nil {
		return nil, err
	}
	l := &gattLink{dev: dev, in: make(chan []byte, 4), lost: make(chan struct{})}
	c.linksMu.Lock()
	c.links[addrKey(addr)] = l
	c.linksMu.Unlock()

	// Windows sometimes fails service discovery on a fresh connection; such
	// errors are returned as they are, so the caller can retry. Only a
	// device that really lacks the service gets ErrServiceNotFound.
	write, notify, err := findCharacteristics(dev)
	if err == nil {
		err = notify.EnableNotifications(func(buf []byte) {
			select {
			case l.in <- append([]byte(nil), buf...):
			default: // nobody is waiting; drop it
			}
		})
		if err != nil {
			err = fmt.Errorf("subscribe to responses: %w", err)
		}
	}
	if err == nil && !isOpen(l) {
		err = errors.New("connection dropped during setup")
	}
	if err != nil {
		_ = l.close()
		return nil, err
	}
	l.char = write
	return l, nil
}

// connect connects to addr, first directly and, if that fails, after
// scanning for the device. c.mu must be held.
func (c *Client) connect(ctx context.Context, addr bluetooth.Address) (bluetooth.Device, error) {
	log := c.log.With("address", addr.String())
	params := func() bluetooth.ConnectionParams {
		var p bluetooth.ConnectionParams
		if deadline, ok := ctx.Deadline(); ok {
			p.ConnectionTimeout = bluetooth.NewDuration(time.Until(deadline))
		}
		return p
	}

	log.Debug("connecting")
	start := time.Now()
	dev, err := c.adapter.Connect(addr, params())
	if err == nil {
		log.Debug("connected", "took", time.Since(start).Round(time.Millisecond))
		return dev, nil
	}
	if ctx.Err() != nil {
		return bluetooth.Device{}, ctx.Err()
	}
	if runtime.GOOS == "windows" {
		// Windows connects by address whether or not it has seen the
		// device, so scanning first would not help.
		return bluetooth.Device{}, fmt.Errorf("switchbot: connect to %s: %w", addr.String(), err)
	}

	log.Debug("could not connect by address; scanning for the Bot", "err", err, "scanFor", findScanTime)
	var found atomic.Bool
	scanErr := c.scanLocked(ctx, findScanTime, func(s sighting) bool {
		if strings.EqualFold(s.addr.String(), addr.String()) {
			found.Store(true)
			return false
		}
		return true
	})
	if scanErr != nil {
		return bluetooth.Device{}, scanErr
	}
	if !found.Load() {
		return bluetooth.Device{}, fmt.Errorf("%w: %s (last connection error: %v)", ErrNotFound, addr.String(), err)
	}

	log.Debug("Bot seen; connecting")
	start = time.Now()
	dev, err = c.adapter.Connect(addr, params())
	if err != nil {
		return bluetooth.Device{}, fmt.Errorf("switchbot: connect to %s: %w", addr.String(), err)
	}
	log.Debug("connected", "took", time.Since(start).Round(time.Millisecond))
	return dev, nil
}

// onConnectionState is called by the Bluetooth stack when a connection
// changes state. Links are looked up by address; this is safe because a
// lost link is always Disconnected before a new one is made, which stops
// the stack reporting events for the old connection.
func (c *Client) onConnectionState(dev bluetooth.Device, state bluetooth.ConnectionState) {
	if state.Connected {
		return
	}
	c.linksMu.Lock()
	l := c.links[addrKey(dev.Address)]
	c.linksMu.Unlock()
	if l != nil && isOpen(l) { // the stack may report the same disconnect twice
		c.log.Debug("disconnected", "address", dev.Address.String(), "reason", state.Err)
		l.markLost()
	}
}

// findCharacteristics finds the characteristic requests are written to and
// the one responses are notified on.
func findCharacteristics(dev bluetooth.Device) (write, notify *bluetooth.DeviceCharacteristic, err error) {
	svcs, err := dev.DiscoverServices([]bluetooth.UUID{serviceUUID})
	if err != nil {
		return nil, nil, fmt.Errorf("discover services: %w", err)
	}
	if len(svcs) == 0 {
		return nil, nil, ErrServiceNotFound
	}
	chars, err := svcs[0].DiscoverCharacteristics([]bluetooth.UUID{writeUUID, notifyUUID})
	if err != nil {
		return nil, nil, fmt.Errorf("discover characteristics: %w", err)
	}
	for i := range chars {
		switch chars[i].UUID() {
		case writeUUID:
			write = &chars[i]
		case notifyUUID:
			notify = &chars[i]
		}
	}
	if write == nil || notify == nil {
		return nil, nil, fmt.Errorf("%w: characteristics missing", ErrServiceNotFound)
	}
	return write, notify, nil
}

func addrKey(a bluetooth.Address) string { return strings.ToUpper(a.String()) }

// isOpen reports whether l has not been lost.
func isOpen(l link) bool {
	select {
	case <-l.done():
		return false
	default:
		return true
	}
}
