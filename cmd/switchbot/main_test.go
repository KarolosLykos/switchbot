package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/karoloslykos/switchbot"
	"tinygo.org/x/bluetooth"
)

// fakeBot records the calls made to it.
type fakeBot struct {
	calls []string
	err   error // returned by every command
}

func (b *fakeBot) record(call string) error {
	b.calls = append(b.calls, call)
	return b.err
}

func (b *fakeBot) Address() string               { return "AA:BB:CC:DD:EE:FF" }
func (b *fakeBot) TurnOn(context.Context) error  { return b.record("on") }
func (b *fakeBot) TurnOff(context.Context) error { return b.record("off") }
func (b *fakeBot) Press(context.Context) error   { return b.record("press") }
func (b *fakeBot) ArmDown(context.Context) error { return b.record("down") }
func (b *fakeBot) ArmUp(context.Context) error   { return b.record("up") }
func (b *fakeBot) SetHoldTime(_ context.Context, d time.Duration) error {
	return b.record("hold " + d.String())
}

func (b *fakeBot) SetMode(_ context.Context, m switchbot.Mode) error {
	return b.record("mode " + m.String())
}

func (b *fakeBot) SetInverted(_ context.Context, inverted bool) error {
	if inverted {
		return b.record("invert on")
	}
	return b.record("invert off")
}

func (b *fakeBot) Status(context.Context) (switchbot.Status, error) {
	if err := b.record("status"); err != nil {
		return switchbot.Status{}, err
	}
	return switchbot.Status{Battery: 91, Firmware: "6.8", Mode: switchbot.ModeSwitch, HoldTime: 2 * time.Second}, nil
}

func TestCommand(t *testing.T) {
	tests := []struct {
		args []string
		call string
		out  string
	}{
		{[]string{"on"}, "on", "OK\n"},
		{[]string{"off"}, "off", "OK\n"},
		{[]string{"press"}, "press", "OK\n"},
		{[]string{"p"}, "press", "OK\n"},
		{[]string{"down"}, "down", "OK\n"},
		{[]string{"up"}, "up", "OK\n"},
		{[]string{"mode", "press"}, "mode press", "OK\n"},
		{[]string{"mode", "switch"}, "mode switch", "OK\n"},
		{[]string{"invert", "on"}, "invert on", "OK\n"},
		{[]string{"invert", "off"}, "invert off", "OK\n"},
		{[]string{"hold", "3"}, "hold 3s", "OK\n"},
		{[]string{"s"}, "status", "battery 91%  firmware 6.8  mode switch  inverted false  hold 2s  timers 0\n"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			b := &fakeBot{}
			var out bytes.Buffer
			if err := command(context.Background(), &out, b, tt.args); err != nil {
				t.Fatal(err)
			}
			if len(b.calls) != 1 || b.calls[0] != tt.call {
				t.Errorf("calls = %q, want [%q]", b.calls, tt.call)
			}
			if out.String() != tt.out {
				t.Errorf("output = %q, want %q", out.String(), tt.out)
			}
		})
	}
}

func TestCommandUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"mode"}, {"mode", "toggle"}, {"invert", "maybe"}, {"hold", "long"}, {"dance"},
	} {
		b := &fakeBot{}
		err := command(context.Background(), &bytes.Buffer{}, b, args)
		if err == nil {
			t.Errorf("%q: want error", args)
		}
		if len(b.calls) != 0 {
			t.Errorf("%q: sent %q to the Bot", args, b.calls)
		}
	}
}

func TestCommandErrors(t *testing.T) {
	b := &fakeBot{err: &switchbot.StatusError{Code: 0x05}}
	err := command(context.Background(), &bytes.Buffer{}, b, []string{"on"})
	if !errors.Is(err, switchbot.ErrUnsupported) || !strings.Contains(err.Error(), "switch mode") {
		t.Errorf("err = %v, want ErrUnsupported with a hint", err)
	}

	b = &fakeBot{err: switchbot.ErrNoResponse}
	if err := command(context.Background(), &bytes.Buffer{}, b, []string{"status"}); !errors.Is(err, switchbot.ErrNoResponse) {
		t.Errorf("status err = %v", err)
	}
	if err := command(context.Background(), &bytes.Buffer{}, b, []string{"press"}); !errors.Is(err, switchbot.ErrNoResponse) {
		t.Errorf("press err = %v", err)
	}
}

func TestInteractive(t *testing.T) {
	b := &fakeBot{}
	in := bufio.NewScanner(strings.NewReader("on\n\nMODE Press\nbogus\nq\noff\n"))
	var out bytes.Buffer
	if err := interactive(context.Background(), in, &out, b); err != nil {
		t.Fatal(err)
	}
	if want := []string{"on", "mode press"}; strings.Join(b.calls, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %q, want %q (nothing after q)", b.calls, want)
	}
	for _, want := range []string{"Controlling AA:BB:CC:DD:EE:FF", "OK", `error: unknown command "bogus"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestInteractiveEndOfInput(t *testing.T) {
	b := &fakeBot{}
	in := bufio.NewScanner(strings.NewReader("press"))
	if err := interactive(context.Background(), in, &bytes.Buffer{}, b); err != nil {
		t.Fatal(err)
	}
	if len(b.calls) != 1 {
		t.Errorf("calls = %q", b.calls)
	}
}

type name string

func (n name) String() string { return string(n) }

func TestChoose(t *testing.T) {
	bots := []name{"first", "second", "third"}
	tests := []struct {
		input   string
		want    name
		wantErr string
	}{
		{"2\n", "second", ""},
		{" 3 \n", "third", ""},
		{"0\n", "", "invalid choice"},
		{"4\n", "", "invalid choice"},
		{"two\n", "", "invalid choice"},
		{"", "", "no Bot chosen"},
	}
	for _, tt := range tests {
		var out bytes.Buffer
		got, err := choose(bufio.NewScanner(strings.NewReader(tt.input)), &out, bots)
		if tt.wantErr != "" {
			if err == nil || err.Error() != tt.wantErr {
				t.Errorf("input %q: err = %v, want %q", tt.input, err, tt.wantErr)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("input %q: got %q, %v, want %q", tt.input, got, err, tt.want)
		}
		if !strings.Contains(out.String(), "[3] third") {
			t.Errorf("list not printed: %q", out.String())
		}
	}
}

func TestChooseOnlyOne(t *testing.T) {
	got, err := choose(bufio.NewScanner(strings.NewReader("")), &bytes.Buffer{}, []name{"only"})
	if err != nil || got != "only" {
		t.Errorf("got %q, %v", got, err)
	}
}

type fakeAd struct {
	name string
	mfr  []bluetooth.ManufacturerDataElement
	svc  []bluetooth.UUID
	data []bluetooth.ServiceDataElement
}

func (a fakeAd) LocalName() string                                     { return a.name }
func (a fakeAd) HasServiceUUID(bluetooth.UUID) bool                    { return false }
func (a fakeAd) ServiceUUIDs() []bluetooth.UUID                        { return a.svc }
func (a fakeAd) Bytes() []byte                                         { return nil }
func (a fakeAd) ManufacturerData() []bluetooth.ManufacturerDataElement { return a.mfr }
func (a fakeAd) ServiceData() []bluetooth.ServiceDataElement           { return a.data }

func TestPrintOnce(t *testing.T) {
	r := bluetooth.ScanResult{
		RSSI: -60,
		AdvertisementPayload: fakeAd{
			name: "WoHand",
			mfr:  []bluetooth.ManufacturerDataElement{{CompanyID: 0x0969, Data: []byte{1, 2}}},
			svc:  []bluetooth.UUID{bluetooth.New16BitUUID(0x180f)},
			data: []bluetooth.ServiceDataElement{{UUID: bluetooth.New16BitUUID(0xfd3d), Data: []byte{'H'}}},
		},
	}

	var out bytes.Buffer
	hook := printOnce(&out)
	hook(r)
	hook(r)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("printed %d lines, want 1:\n%s", len(lines), out.String())
	}
	for _, want := range []string{"RSSI  -60", `name "WoHand"`, "mfr 0x0969=01 02", "svc 0000180f", "svcdata 0000fd3d", "=48"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line lacks %q: %s", want, lines[0])
		}
	}
}
