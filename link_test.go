//go:build linux || windows

package switchbot

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"tinygo.org/x/bluetooth"
)

// fakeDevice simulates a Bot's GATT side: it answers requests written to
// its write characteristic on its notify characteristic.
type fakeDevice struct {
	charErr     error  // finding the characteristics fails
	notifyErr   error  // subscribing fails
	writeErr    error  // writing fails
	onSubscribe func() // runs while subscribing

	mu           sync.Mutex
	notify       func([]byte)
	written      [][]byte
	disconnected bool
}

func (d *fakeDevice) characteristics() (write, notify characteristic, err error) {
	if d.charErr != nil {
		return nil, nil, d.charErr
	}
	return writeChar{d}, notifyChar{d}, nil
}

func (d *fakeDevice) disconnect() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disconnected = true
	return nil
}

func (d *fakeDevice) isDisconnected() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.disconnected
}

func (d *fakeDevice) sent() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][]byte(nil), d.written...)
}

type writeChar struct{ d *fakeDevice }

func (c writeChar) WriteWithoutResponse(p []byte) (int, error) {
	d := c.d
	if d.writeErr != nil {
		return 0, d.writeErr
	}
	d.mu.Lock()
	d.written = append(d.written, append([]byte(nil), p...))
	notify := d.notify
	d.mu.Unlock()
	reply := []byte{statusOK}
	if p[1]&0x0f == cmdGetBasicInfo {
		reply = statusReplies[macBot68]
	}
	notify(reply)
	return len(p), nil
}

func (c writeChar) EnableNotifications(func([]byte)) error {
	return errors.New("write characteristic does not notify")
}

type notifyChar struct{ d *fakeDevice }

func (c notifyChar) WriteWithoutResponse([]byte) (int, error) {
	return 0, errors.New("notify characteristic is not writable")
}

func (c notifyChar) EnableNotifications(callback func([]byte)) error {
	if c.d.onSubscribe != nil {
		c.d.onSubscribe()
	}
	if c.d.notifyErr != nil {
		return c.d.notifyErr
	}
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	c.d.notify = callback
	return nil
}

// gattTestClient returns a Client that connects through the real GATT code
// to the devices newDevice returns.
func gattTestClient(t *testing.T, newDevice func() *fakeDevice, opts ...Option) (*Client, *fakeAdapter, *[]*fakeDevice) {
	t.Helper()
	var (
		mu      sync.Mutex
		devices []*fakeDevice
	)
	a := &fakeAdapter{ads: allAds(t)}
	a.connect = func(bluetooth.Address, bluetooth.ConnectionParams) (device, error) {
		d := newDevice()
		mu.Lock()
		devices = append(devices, d)
		mu.Unlock()
		return d, nil
	}
	c := newClient(a, opts...)
	c.findScan = 0
	t.Cleanup(func() { _ = c.Close() })
	return c, a, &devices
}

func disconnectEvent(t *testing.T, c *Client, mac string) {
	t.Helper()
	addr, err := parseAddress(mac)
	if err != nil {
		t.Fatal(err)
	}
	c.onConnectionState(bluetooth.Device{Address: addr}, bluetooth.ConnectionState{Connected: false})
}

func TestGATTCommands(t *testing.T) {
	c, _, devices := gattTestClient(t, func() *fakeDevice { return &fakeDevice{} })
	bot := mustBot(t, c, macBot68)
	ctx := context.Background()

	if err := bot.TurnOn(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := bot.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Battery != 91 {
		t.Errorf("Battery = %d, want 91", st.Battery)
	}
	if len(*devices) != 1 {
		t.Fatalf("connected %d times, want 1", len(*devices))
	}
	d := (*devices)[0]
	want := [][]byte{{0x57, 0x02}, {0x57, 0x01, 0x01}, {0x57, 0x02}}
	got := d.sent()
	if len(got) != len(want) {
		t.Fatalf("sent % X, want % X", got, want)
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("request %d = % X, want % X", i, got[i], want[i])
		}
	}

	if err := bot.Close(); err != nil {
		t.Fatal(err)
	}
	if !d.isDisconnected() {
		t.Error("Close did not disconnect the device")
	}
}

func TestGATTDisconnectEvent(t *testing.T) {
	c, _, devices := gattTestClient(t, func() *fakeDevice { return &fakeDevice{} })
	bot := mustBot(t, c, macBot68)
	ctx := context.Background()

	if err := bot.Press(ctx); err != nil {
		t.Fatal(err)
	}
	addr, _ := parseAddress(macBot68)
	c.onConnectionState(bluetooth.Device{Address: addr}, bluetooth.ConnectionState{Connected: true})
	if !bot.Connected() {
		t.Fatal("a connect event marked the Bot disconnected")
	}
	disconnectEvent(t, c, macBot68)
	disconnectEvent(t, c, macBot68) // reported twice
	disconnectEvent(t, c, macBot)   // unknown device
	if bot.Connected() {
		t.Fatal("Bot still connected after a disconnect event")
	}

	if err := bot.Press(ctx); err != nil {
		t.Fatal(err)
	}
	if len(*devices) != 2 {
		t.Errorf("connected %d times, want 2", len(*devices))
	}
	if !(*devices)[0].isDisconnected() {
		t.Error("lost connection not released")
	}
}

func TestGATTSetupFailures(t *testing.T) {
	var c *Client
	tests := []struct {
		name   string
		device func() *fakeDevice
		want   string
		is     error
	}{
		{"no service", func() *fakeDevice { return &fakeDevice{charErr: ErrServiceNotFound} }, "", ErrServiceNotFound},
		{"subscribe fails", func() *fakeDevice { return &fakeDevice{notifyErr: errors.New("denied")} }, "subscribe to responses: denied", nil},
		{"dropped during setup", func() *fakeDevice {
			return &fakeDevice{onSubscribe: func() { disconnectEvent(t, c, macBot68) }}
		}, "connection dropped during setup", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var devices *[]*fakeDevice
			c, _, devices = gattTestClient(t, tt.device, WithRetries(0))
			err := mustBot(t, c, macBot68).Connect(context.Background())
			if err == nil {
				t.Fatal("Connect succeeded")
			}
			if tt.want != "" && !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Errorf("err = %v, want %v", err, tt.is)
			}
			if !(*devices)[0].isDisconnected() {
				t.Error("failed connection not closed")
			}
		})
	}
}

func TestGATTWriteError(t *testing.T) {
	c, _, devices := gattTestClient(t, func() *fakeDevice {
		return &fakeDevice{writeErr: errors.New("radio off")}
	}, WithRetries(1))
	err := mustBot(t, c, macBot68).Connect(context.Background())
	if !errors.Is(err, errWrite) {
		t.Fatalf("err = %v, want errWrite", err)
	}
	if len(*devices) != 2 {
		t.Errorf("connected %d times, want 2 (one retry)", len(*devices))
	}
}

func TestConnectByAddressFailsWithoutScan(t *testing.T) {
	a := &fakeAdapter{ads: allAds(t)}
	c := newClient(a)
	c.findScan = 0
	addr, _ := parseAddress(macBot68)

	c.mu.Lock()
	_, err := c.connect(context.Background(), addr)
	c.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "fake adapter cannot connect") {
		t.Errorf("err = %v", err)
	}
}

func TestConnectScansWhenAddressFails(t *testing.T) {
	a := &fakeAdapter{ads: allAds(t)}
	attempts := 0
	var timeout bluetooth.Duration
	a.connect = func(_ bluetooth.Address, p bluetooth.ConnectionParams) (device, error) {
		attempts++
		timeout = p.ConnectionTimeout
		if attempts == 1 {
			return nil, errors.New("device not known")
		}
		return &fakeDevice{}, nil
	}
	c := newClient(a)
	c.findScan = 5 * time.Second
	addr, _ := parseAddress(macBot68)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.mu.Lock()
	dev, err := c.connect(ctx, addr)
	c.mu.Unlock()
	if err != nil || dev == nil {
		t.Fatalf("connect = %v, %v", dev, err)
	}
	if attempts != 2 {
		t.Errorf("tried %d times, want 2", attempts)
	}
	if timeout == 0 {
		t.Error("context deadline not passed as connection timeout")
	}
}

func TestConnectScanFailures(t *testing.T) {
	addr, _ := parseAddress("AA:BB:CC:DD:EE:99") // never advertises

	t.Run("not found", func(t *testing.T) {
		c := newClient(&fakeAdapter{ads: allAds(t)})
		c.findScan = 50 * time.Millisecond
		c.mu.Lock()
		_, err := c.connect(context.Background(), addr)
		c.mu.Unlock()
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("still fails after scan", func(t *testing.T) {
		c := newClient(&fakeAdapter{ads: allAds(t)})
		c.findScan = time.Second
		seen, _ := parseAddress(macBot68)
		c.mu.Lock()
		_, err := c.connect(context.Background(), seen)
		c.mu.Unlock()
		if err == nil || !strings.Contains(err.Error(), "connect to") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		c := newClient(&fakeAdapter{ads: allAds(t)})
		c.findScan = time.Second
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		c.mu.Lock()
		_, err := c.connect(ctx, addr)
		c.mu.Unlock()
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	})
}

func TestWithAdapter(t *testing.T) {
	c := newClient(nil, WithAdapter(bluetooth.DefaultAdapter))
	if _, ok := c.adapter.(btAdapter); !ok {
		t.Errorf("adapter is %T, want btAdapter", c.adapter)
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestWithLoggerAndScanHook(t *testing.T) {
	var logs syncBuffer
	var mu sync.Mutex
	hooked := 0
	c, _ := newTestClient(t,
		WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		WithScanHook(func(bluetooth.ScanResult) { mu.Lock(); hooked++; mu.Unlock() }),
	)
	if _, err := c.DiscoverBots(context.Background(), 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hooked == 0 {
		t.Error("scan hook never called")
	}
	for _, want := range []string{"scan started", "found Bot", "discovery finished", "sending"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %q", want)
		}
	}
}

func TestBotString(t *testing.T) {
	c, _ := newTestClient(t)
	bot := mustBot(t, c, macBot68)
	if got, want := bot.String(), "Bot "+macBot68; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	bots, err := c.DiscoverBots(context.Background(), 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bots {
		if !strings.Contains(b.String(), "RSSI -60 dBm") {
			t.Errorf("String() = %q, want the RSSI", b.String())
		}
	}
}

func TestConnectErrorUnwraps(t *testing.T) {
	inner := errors.New("timed out")
	err := error(&connectError{inner})
	if err.Error() != "timed out" || !errors.Is(err, inner) {
		t.Errorf("connectError = %v", err)
	}
}
