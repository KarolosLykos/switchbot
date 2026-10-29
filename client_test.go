//go:build linux || windows

package switchbot

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tinygo.org/x/bluetooth"
)

// fakeAdapter replays a fixed list of advertisements, one every 5ms, until
// StopScan is called. A new scan starts with a fresh stop channel.
type fakeAdapter struct {
	ads     []bluetooth.ScanResult
	connect func(bluetooth.Address, bluetooth.ConnectionParams) (device, error)

	mu   sync.Mutex
	stop chan struct{}
}

func (f *fakeAdapter) Enable() error { return nil }

func (f *fakeAdapter) SetConnectionStateHandler(func(bluetooth.Device, bluetooth.ConnectionState)) {}

func (f *fakeAdapter) Scan(cb func(*bluetooth.Adapter, bluetooth.ScanResult)) error {
	f.mu.Lock()
	stop := make(chan struct{})
	f.stop = stop
	f.mu.Unlock()

	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for i := 0; ; i++ {
		select {
		case <-stop:
			return nil
		case <-t.C:
		}
		if len(f.ads) > 0 {
			cb(nil, f.ads[i%len(f.ads)])
		}
	}
}

func (f *fakeAdapter) StopScan() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stop != nil {
		select {
		case <-f.stop:
		default:
			close(f.stop)
		}
	}
	return nil
}

func (f *fakeAdapter) Connect(addr bluetooth.Address, params bluetooth.ConnectionParams) (device, error) {
	if f.connect == nil {
		return nil, errors.New("fake adapter cannot connect")
	}
	return f.connect(addr, params)
}

// fakeAd is an advertisement payload.
type fakeAd struct {
	name    string
	mfr     []bluetooth.ManufacturerDataElement
	svcData []bluetooth.ServiceDataElement
}

func (a fakeAd) LocalName() string                                     { return a.name }
func (a fakeAd) HasServiceUUID(bluetooth.UUID) bool                    { return false }
func (a fakeAd) ServiceUUIDs() []bluetooth.UUID                        { return nil }
func (a fakeAd) Bytes() []byte                                         { return nil }
func (a fakeAd) ManufacturerData() []bluetooth.ManufacturerDataElement { return a.mfr }
func (a fakeAd) ServiceData() []bluetooth.ServiceDataElement           { return a.svcData }

func ad(t *testing.T, mac string, payload fakeAd) bluetooth.ScanResult {
	t.Helper()
	a, err := parseAddress(mac)
	if err != nil {
		t.Fatal(err)
	}
	return bluetooth.ScanResult{Address: a, RSSI: -60, AdvertisementPayload: payload}
}

var (
	advertisedBot = fakeAd{svcData: []bluetooth.ServiceDataElement{
		{UUID: bluetooth.New16BitUUID(0xfd3d), Data: []byte{'H', 0xc0, 80}},
	}}
	advertisedMeter = fakeAd{svcData: []bluetooth.ServiceDataElement{
		{UUID: bluetooth.New16BitUUID(0xfd3d), Data: []byte{'T', 0x00, 80}},
	}}
	// What Windows sees from Bots with firmware 6.8: only the company ID.
	untyped = fakeAd{mfr: []bluetooth.ManufacturerDataElement{
		{CompanyID: switchBotCompanyID, Data: []byte{0xec, 0x6f, 0x05, 0xc6, 0x3e, 0x8d, 0x02, 0x0c}},
	}}
	namedBot   = fakeAd{name: "WoHand"}
	headphones = fakeAd{name: "Headphones"}
)

const (
	macBot      = "AA:BB:CC:DD:EE:01" // advertises that it is a Bot
	macMeter    = "AA:BB:CC:DD:EE:02" // advertises that it is a Meter
	macBot44    = "AA:BB:CC:DD:EE:03" // untyped; firmware 4.4 Bot, reports type H
	macHub      = "AA:BB:CC:DD:EE:04" // untyped; reports type L
	macUnreach  = "AA:BB:CC:DD:EE:05" // untyped; can't be connected to
	macNamedBot = "AA:BB:CC:DD:EE:06" // advertises the name WoHand
	macBot68    = "AA:BB:CC:DD:EE:07" // untyped; firmware 6.8 Bot, no type byte
	macLamp     = "AA:BB:CC:DD:EE:08" // untyped; no type byte, not shaped like a Bot
	macOther    = "11:22:33:44:55:66" // not a SwitchBot device
)

var statusReplies = map[string][]byte{
	macBot:      {0x01, 0x5f, 0x44, 0x64, 0x00, 0x00, 0x00, 0x62, 0x00, 0x00, 0x00, 0x00, 0x00},
	macNamedBot: {0x01, 0x5f, 0x44, 0x64, 0x00, 0x00, 0x00, 0x62, 0x00, 0x00, 0x00, 0x00, 0x00},
	macBot68:    {0x01, 0x5b, 0x44, 0x64, 0x00, 0x00, 0x00, 0x5e, 0x00, 0x11, 0x00, 0x00, 0x00},
	macBot44:    {0x01, 0x64, 0x2c, 0x64, 0x00, 0x00, 0x00, 0xa1, 0x00, 0x00, 0x00, 'H', 0x00},
	macHub:      {0x01, 0x64, 0x2c, 0x64, 0x00, 0x00, 0x00, 0xa1, 0x00, 0x00, 0x00, 'L', 0x00},
	macLamp:     {0x01, 0x50, 0x10, 0x32, 0x00, 0x00, 0x00, 0x00, 0x00, 0x42, 0x00, 0x00, 0x00},
}

func allAds(t *testing.T) []bluetooth.ScanResult {
	return []bluetooth.ScanResult{
		ad(t, macBot, advertisedBot),
		ad(t, macMeter, advertisedMeter),
		ad(t, macBot44, untyped),
		ad(t, macHub, untyped),
		ad(t, macUnreach, untyped),
		ad(t, macNamedBot, namedBot),
		ad(t, macBot68, untyped),
		ad(t, macLamp, untyped),
		ad(t, macOther, headphones),
	}
}

// response is how a fake device reacts to one request.
type response struct {
	reply    []byte
	writeErr bool // the write fails
	silent   bool // no response
	drop     bool // the connection drops instead of answering
}

// fakeNet simulates the devices' side of connections.
type fakeNet struct {
	mu       sync.Mutex
	dials    map[string]int
	dialErrs map[string]int      // how many dials to fail per device
	requests map[string][][]byte // requests that reached each device
	links    map[string]*fakeLink
	handle   func(mac string, req []byte) response
}

func newFakeNet() *fakeNet {
	n := &fakeNet{
		dials:    map[string]int{},
		dialErrs: map[string]int{},
		requests: map[string][][]byte{},
		links:    map[string]*fakeLink{},
	}
	n.handle = n.defaultHandle
	return n
}

func (n *fakeNet) defaultHandle(mac string, req []byte) response {
	if req[1]&0x0f == cmdGetBasicInfo {
		return response{reply: statusReplies[mac]}
	}
	return response{reply: []byte{statusOK}}
}

func (n *fakeNet) dial(_ context.Context, addr bluetooth.Address) (link, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	mac := addr.String()
	n.dials[mac]++
	if mac == macUnreach {
		return nil, errors.New("unreachable")
	}
	if n.dialErrs[mac] > 0 {
		n.dialErrs[mac]--
		return nil, errors.New("connection attempt timed out")
	}
	l := &fakeLink{net: n, mac: mac, in: make(chan []byte, 4), lost: make(chan struct{})}
	n.links[mac] = l
	return l, nil
}

func (n *fakeNet) count(mac string) (dials, requests int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.dials[mac], len(n.requests[mac])
}

func (n *fakeNet) sent(mac string) [][]byte {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([][]byte(nil), n.requests[mac]...)
}

// dropConnection makes the device's current connection drop.
func (n *fakeNet) dropConnection(mac string) {
	n.mu.Lock()
	l := n.links[mac]
	n.mu.Unlock()
	l.markLost()
}

type fakeLink struct {
	net    *fakeNet
	mac    string
	in     chan []byte
	lost   chan struct{}
	once   sync.Once
	closed atomic.Bool
}

func (l *fakeLink) write(req []byte) error {
	select {
	case <-l.lost:
		return errWrite
	default:
	}
	r := l.net.handle(l.mac, req)
	if r.writeErr {
		return errWrite
	}
	l.net.mu.Lock()
	l.net.requests[l.mac] = append(l.net.requests[l.mac], append([]byte(nil), req...))
	l.net.mu.Unlock()
	switch {
	case r.drop:
		l.markLost()
	case !r.silent:
		l.in <- r.reply
	}
	return nil
}

func (l *fakeLink) replies() <-chan []byte { return l.in }
func (l *fakeLink) done() <-chan struct{}  { return l.lost }
func (l *fakeLink) markLost()              { l.once.Do(func() { close(l.lost) }) }
func (l *fakeLink) close() error           { l.closed.Store(true); l.markLost(); return nil }

func newTestClient(t *testing.T, opts ...Option) (*Client, *fakeNet) {
	t.Helper()
	n := newFakeNet()
	c := newClient(&fakeAdapter{ads: allAds(t)}, opts...)
	c.dial = n.dial
	t.Cleanup(func() { _ = c.Close() })
	return c, n
}

func mustBot(t *testing.T, c *Client, mac string) *Bot {
	t.Helper()
	b, err := c.Bot(mac)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBotByAddressReusesConnection(t *testing.T) {
	c, n := newTestClient(t)
	bot := mustBot(t, c, macBot68)
	ctx := context.Background()

	if bot.Connected() {
		t.Fatal("Bot connected before its first command")
	}
	for _, cmd := range []func(context.Context) error{bot.TurnOn, bot.TurnOff, bot.TurnOn} {
		if err := cmd(ctx); err != nil {
			t.Fatal(err)
		}
	}
	st, err := bot.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Battery != 91 || st.Mode != ModeSwitch || !st.Inverted {
		t.Errorf("Status = %+v", st)
	}

	dials, _ := n.count(macBot68)
	if dials != 1 {
		t.Errorf("connected %d times, want 1", dials)
	}
	want := [][]byte{
		{0x57, 0x02}, // check that it is a Bot, once
		{0x57, 0x01, 0x01},
		{0x57, 0x01, 0x02},
		{0x57, 0x01, 0x01},
		{0x57, 0x02},
	}
	assertSent(t, n, macBot68, want)
	if !bot.Connected() {
		t.Error("Bot not connected after commands")
	}
}

func assertSent(t *testing.T, n *fakeNet, mac string, want [][]byte) {
	t.Helper()
	got := n.sent(mac)
	if len(got) != len(want) {
		t.Fatalf("sent %d requests % X, want %d % X", len(got), got, len(want), want)
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("request %d = % X, want % X", i, got[i], want[i])
		}
	}
}

func TestBotSameAddressSameBot(t *testing.T) {
	c, _ := newTestClient(t)
	if mustBot(t, c, "aa:bb:cc:dd:ee:07") != mustBot(t, c, macBot68) {
		t.Error("Bot returned different values for the same address")
	}
}

func TestBotInvalidAddress(t *testing.T) {
	c, _ := newTestClient(t)
	if _, err := c.Bot("kitchen"); err == nil {
		t.Error("want error for invalid address")
	}
}

func TestBotNotABot(t *testing.T) {
	for _, mac := range []string{macHub, macLamp} {
		c, n := newTestClient(t)
		err := mustBot(t, c, mac).TurnOn(context.Background())
		if !errors.Is(err, ErrNotABot) {
			t.Errorf("%s: err = %v, want ErrNotABot", mac, err)
		}
		assertSent(t, n, mac, [][]byte{{0x57, 0x02}}) // the command itself is never sent
	}
}

func TestReconnectAfterConnectionDrops(t *testing.T) {
	c, n := newTestClient(t)
	bot := mustBot(t, c, macBot)
	ctx := context.Background()

	if err := bot.TurnOn(ctx); err != nil {
		t.Fatal(err)
	}
	n.dropConnection(macBot)
	if bot.Connected() {
		t.Error("Connected after the connection dropped")
	}
	if err := bot.TurnOff(ctx); err != nil {
		t.Fatalf("TurnOff after drop: %v", err)
	}
	if dials, _ := n.count(macBot); dials != 2 {
		t.Errorf("connected %d times, want 2", dials)
	}
	// Verified once; not again after reconnecting.
	assertSent(t, n, macBot, [][]byte{{0x57, 0x02}, {0x57, 0x01, 0x01}, {0x57, 0x01, 0x02}})
}

func TestWriteFailureReconnectsAndRetries(t *testing.T) {
	c, n := newTestClient(t)
	bot := mustBot(t, c, macBot)
	ctx := context.Background()
	if err := bot.Connect(ctx); err != nil {
		t.Fatal(err)
	}

	failed := false
	n.handle = func(mac string, req []byte) response {
		if !failed && req[1] == cmdAction {
			failed = true
			return response{writeErr: true}
		}
		return n.defaultHandle(mac, req)
	}
	if err := bot.TurnOn(ctx); err != nil {
		t.Fatalf("TurnOn: %v", err)
	}
	if dials, _ := n.count(macBot); dials != 2 {
		t.Errorf("connected %d times, want 2", dials)
	}
	assertSent(t, n, macBot, [][]byte{{0x57, 0x02}, {0x57, 0x01, 0x01}}) // delivered once
}

func TestConnectionLostWhileWaitingIsNotRetried(t *testing.T) {
	c, n := newTestClient(t)
	bot := mustBot(t, c, macBot)
	ctx := context.Background()
	if err := bot.Connect(ctx); err != nil {
		t.Fatal(err)
	}

	n.handle = func(string, []byte) response { return response{drop: true} }
	if err := bot.Press(ctx); !errors.Is(err, ErrConnectionLost) {
		t.Fatalf("Press err = %v, want ErrConnectionLost", err)
	}
	if _, reqs := n.count(macBot); reqs != 2 {
		t.Errorf("%d requests, want 2 (verify + one press, never repeated)", reqs)
	}

	n.handle = n.defaultHandle
	if err := bot.Press(ctx); err != nil {
		t.Fatalf("Press after reconnect: %v", err)
	}
}

func TestNoResponse(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the reply timeout")
	}
	c, n := newTestClient(t)
	bot := mustBot(t, c, macBot)
	ctx := context.Background()
	if err := bot.Connect(ctx); err != nil {
		t.Fatal(err)
	}

	n.handle = func(string, []byte) response { return response{silent: true} }
	if err := bot.Press(ctx); !errors.Is(err, ErrNoResponse) {
		t.Fatalf("Press err = %v, want ErrNoResponse", err)
	}
	if bot.Connected() {
		t.Error("still connected after no response; want a fresh connection next time")
	}
}

func TestBusyIsRetried(t *testing.T) {
	c, n := newTestClient(t)
	bot := mustBot(t, c, macBot)
	busy := 2
	n.handle = func(mac string, req []byte) response {
		if req[1] == cmdAction && busy > 0 {
			busy--
			return response{reply: []byte{statusBusy}}
		}
		return n.defaultHandle(mac, req)
	}
	if err := bot.Press(context.Background()); err != nil {
		t.Fatalf("Press: %v", err)
	}

	busy = 10
	if err := bot.Press(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("Press err = %v, want ErrBusy after retries", err)
	}
}

func TestConnectFailureIsRetried(t *testing.T) {
	c, n := newTestClient(t, WithRetries(2))
	n.dialErrs[macBot] = 2
	if err := mustBot(t, c, macBot).Press(context.Background()); err != nil {
		t.Fatalf("Press: %v", err)
	}
	if dials, _ := n.count(macBot); dials != 3 {
		t.Errorf("connected %d times, want 3", dials)
	}

	c, n = newTestClient(t, WithRetries(1))
	n.dialErrs[macBot] = 2
	if err := mustBot(t, c, macBot).Press(context.Background()); err == nil {
		t.Fatal("want error when connecting keeps failing")
	}
}

func TestIdleTimeoutDisconnects(t *testing.T) {
	c, n := newTestClient(t, WithIdleTimeout(300*time.Millisecond))
	bot := mustBot(t, c, macBot)
	ctx := context.Background()

	if err := bot.TurnOn(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if err := bot.TurnOff(ctx); err != nil { // resets the idle timer
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // 300ms since TurnOn, 150ms since TurnOff
	if !bot.Connected() {
		t.Fatal("disconnected before being idle for the timeout")
	}
	time.Sleep(500 * time.Millisecond)
	if bot.Connected() {
		t.Fatal("still connected after the idle timeout")
	}
	if err := bot.TurnOn(ctx); err != nil {
		t.Fatal(err)
	}
	if dials, _ := n.count(macBot); dials != 2 {
		t.Errorf("connected %d times, want 2", dials)
	}
}

func TestCloseDisconnects(t *testing.T) {
	c, n := newTestClient(t)
	bot := mustBot(t, c, macBot)
	ctx := context.Background()

	if err := bot.TurnOn(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if bot.Connected() {
		t.Fatal("connected after Client.Close")
	}
	if err := bot.TurnOff(ctx); err != nil {
		t.Fatal(err)
	}
	if dials, _ := n.count(macBot); dials != 2 {
		t.Errorf("connected %d times, want 2", dials)
	}
}

func TestCommandBytes(t *testing.T) {
	c, n := newTestClient(t)
	bot := mustBot(t, c, macBot68) // switch mode, inverted
	ctx := context.Background()

	steps := []struct {
		name string
		do   func() error
		want [][]byte
	}{
		{"press", func() error { return bot.Press(ctx) }, [][]byte{{0x57, 0x01, 0x00}}},
		{"arm down", func() error { return bot.ArmDown(ctx) }, [][]byte{{0x57, 0x01, 0x03}}},
		{"arm up", func() error { return bot.ArmUp(ctx) }, [][]byte{{0x57, 0x01, 0x04}}},
		// SetMode keeps the inverted flag the Bot reports.
		{"press mode", func() error { return bot.SetMode(ctx, ModePress) }, [][]byte{{0x57, 0x02}, {0x57, 0x03, 100, 0x01}}},
		// SetInverted keeps the mode the Bot reports (switch).
		{"not inverted", func() error { return bot.SetInverted(ctx, false) }, [][]byte{{0x57, 0x02}, {0x57, 0x03, 100, 0x10}}},
		{"hold time", func() error { return bot.SetHoldTime(ctx, 3*time.Second) }, [][]byte{{0x57, 0x0f, 0x08, 3}}},
	}
	want := [][]byte{{0x57, 0x02}} // verification
	for _, s := range steps {
		if err := s.do(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		want = append(want, s.want...)
	}
	assertSent(t, n, macBot68, want)

	if err := bot.SetHoldTime(ctx, 300*time.Second); err == nil {
		t.Error("hold time 300s: want error")
	}
	if err := bot.SetMode(ctx, Mode(7)); err == nil {
		t.Error("invalid mode: want error")
	}
}

func TestPassword(t *testing.T) {
	c, n := newTestClient(t, WithPassword("pass"))
	if err := mustBot(t, c, macBot).TurnOn(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSent(t, n, macBot, [][]byte{
		{0x57, 0x12, 0xce, 0x70, 0xd4, 0x24},
		{0x57, 0x11, 0xce, 0x70, 0xd4, 0x24, 0x01},
	})
}

func TestStatusErrors(t *testing.T) {
	c, n := newTestClient(t)
	bot := mustBot(t, c, macBot)
	if err := bot.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	n.handle = func(string, []byte) response { return response{reply: []byte{0x05, 0x48, 0xc0}} }
	if err := bot.Press(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Press err = %v, want ErrUnsupported", err)
	}
	n.handle = func(string, []byte) response { return response{reply: []byte{0x07}} }
	if _, err := bot.Status(context.Background()); !errors.Is(err, ErrPasswordRequired) {
		t.Errorf("Status err = %v, want ErrPasswordRequired", err)
	}
}

func TestDiscoverBots(t *testing.T) {
	c, n := newTestClient(t, WithRetries(0))

	const d = 100 * time.Millisecond
	start := time.Now()
	bots, err := c.DiscoverBots(context.Background(), d)
	if err != nil {
		t.Fatalf("DiscoverBots: %v", err)
	}
	if elapsed := time.Since(start); elapsed < d || elapsed > d+time.Second {
		t.Errorf("DiscoverBots took %s, want about %s", elapsed, d)
	}

	want := []string{macBot, macBot44, macNamedBot, macBot68}
	if len(bots) != len(want) {
		t.Fatalf("DiscoverBots = %v, want %v", bots, want)
	}
	for i := range want {
		if bots[i].Address() != want[i] {
			t.Fatalf("DiscoverBots = %v, want %v", bots, want)
		}
	}

	// Devices that advertised their type are not connected to.
	for _, mac := range []string{macBot, macMeter, macNamedBot, macOther} {
		if dials, _ := n.count(mac); dials != 0 {
			t.Errorf("%s: connected %d times during discovery, want 0", mac, dials)
		}
	}
	// Bots found by connecting stay connected, so the next command is instant.
	if !bots[3].Connected() {
		t.Error("Bot checked during discovery was disconnected")
	}
	if err := bots[3].TurnOn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dials, _ := n.count(macBot68); dials != 1 {
		t.Errorf("connected %d times, want 1", dials)
	}
	// Discovered Bots are the same values Client.Bot returns.
	if mustBot(t, c, macBot68) != bots[3] {
		t.Error("Client.Bot returned a different value than DiscoverBots")
	}
	if bots[0].RSSI() != -60 {
		t.Errorf("RSSI = %d, want -60", bots[0].RSSI())
	}
}

func TestDiscoverBotsCancelled(t *testing.T) {
	c, n := newTestClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(80*time.Millisecond, cancel)
	start := time.Now()
	bots, err := c.DiscoverBots(ctx, 10*time.Second)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("kept going after cancel (%s)", time.Since(start))
	}
	if len(bots) == 0 {
		t.Error("want the Bots that advertised themselves before the cancel")
	}
	if dials, _ := n.count(macBot68); dials != 0 {
		t.Error("connected to devices after cancel")
	}
}

func TestDiscoverBotsInvalidDuration(t *testing.T) {
	c, _ := newTestClient(t)
	if _, err := c.DiscoverBots(context.Background(), 0); err == nil {
		t.Fatal("zero duration: want error")
	}
}

func TestConcurrentCommands(t *testing.T) {
	c, n := newTestClient(t)
	bot := mustBot(t, c, macBot)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := bot.Press(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if dials, reqs := n.count(macBot); dials != 1 || reqs != 21 {
		t.Errorf("dials=%d requests=%d, want 1 and 21", dials, reqs)
	}
}

func TestParseAddress(t *testing.T) {
	for _, s := range []string{"AA:BB:CC:DD:EE:FF", "aa:bb:cc:dd:ee:ff"} {
		a, err := parseAddress(s)
		if err != nil {
			t.Errorf("parseAddress(%q): %v", s, err)
			continue
		}
		if a.String() != "AA:BB:CC:DD:EE:FF" {
			t.Errorf("parseAddress(%q) = %s", s, a.String())
		}
	}
	for _, s := range []string{"", "kitchen", "AA:BB:CC"} {
		if _, err := parseAddress(s); err == nil {
			t.Errorf("parseAddress(%q): want error", s)
		}
	}
}

func TestVerificationRetriedWhenConnectionDrops(t *testing.T) {
	c, n := newTestClient(t)
	dropped := false
	n.handle = func(mac string, req []byte) response {
		if !dropped && req[1] == cmdGetBasicInfo {
			dropped = true
			return response{drop: true}
		}
		return n.defaultHandle(mac, req)
	}
	if err := mustBot(t, c, macBot).TurnOn(context.Background()); err != nil {
		t.Fatalf("TurnOn: %v", err)
	}
	if dials, _ := n.count(macBot); dials != 2 {
		t.Errorf("connected %d times, want 2", dials)
	}
	assertSent(t, n, macBot, [][]byte{{0x57, 0x02}, {0x57, 0x02}, {0x57, 0x01, 0x01}})
}

func TestLostConnectionIsReleased(t *testing.T) {
	c, n := newTestClient(t)
	bot := mustBot(t, c, macBot)
	ctx := context.Background()
	if err := bot.TurnOn(ctx); err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	old := n.links[macBot]
	n.mu.Unlock()

	n.dropConnection(macBot)
	if err := bot.TurnOff(ctx); err != nil {
		t.Fatal(err)
	}
	if !old.closed.Load() {
		t.Error("lost connection was not closed before reconnecting")
	}

	n.mu.Lock()
	old = n.links[macBot]
	n.mu.Unlock()
	n.handle = func(string, []byte) response { return response{drop: true} }
	if err := bot.Press(ctx); !errors.Is(err, ErrConnectionLost) {
		t.Fatalf("Press err = %v, want ErrConnectionLost", err)
	}
	if !old.closed.Load() {
		t.Error("connection lost while waiting was not closed")
	}
}

func TestBusyDuringVerification(t *testing.T) {
	c, n := newTestClient(t, WithRetries(1))
	n.handle = func(string, []byte) response { return response{reply: []byte{statusBusy}} }
	if err := mustBot(t, c, macBot).TurnOn(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("TurnOn err = %v, want ErrBusy", err)
	}
}

func TestSetModeAndSetInvertedConcurrently(t *testing.T) {
	c, n := newTestClient(t)
	var mu sync.Mutex
	mode := byte(0x00) // press mode, not inverted
	n.handle = func(mac string, req []byte) response {
		mu.Lock()
		defer mu.Unlock()
		switch req[1] & 0x0f {
		case cmdGetBasicInfo:
			r := append([]byte(nil), statusReplies[macBot]...)
			r[9] = mode
			return response{reply: r}
		case cmdSetBasicInfo:
			mode = req[3]
		}
		return response{reply: []byte{statusOK}}
	}
	bot := mustBot(t, c, macBot)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		mu.Lock()
		mode = 0x00
		mu.Unlock()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = bot.SetMode(ctx, ModeSwitch) }()
		go func() { defer wg.Done(); _ = bot.SetInverted(ctx, true) }()
		wg.Wait()
		mu.Lock()
		got := mode
		mu.Unlock()
		if got != 0x11 {
			t.Fatalf("mode byte = %#02x, want 0x11 (switch + inverted): one change was lost", got)
		}
	}
}
