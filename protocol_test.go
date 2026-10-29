package switchbot

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestWithPassword(t *testing.T) {
	// CRC-32 (IEEE) of "pass" is 0xCE70D424.
	pw := encodePassword("pass")
	if want := []byte{0xce, 0x70, 0xd4, 0x24}; !bytes.Equal(pw, want) {
		t.Fatalf("encodePassword = % X, want % X", pw, want)
	}

	tests := []struct {
		name string
		req  []byte
		want []byte
	}{
		{"press", request(cmdAction, actionPress), []byte{0x57, 0x11, 0xce, 0x70, 0xd4, 0x24, 0x00}},
		{"status", request(cmdGetBasicInfo), []byte{0x57, 0x12, 0xce, 0x70, 0xd4, 0x24}},
		{"hold", request(cmdExtended, extSetHoldTime, 5), []byte{0x57, 0x1f, 0xce, 0x70, 0xd4, 0x24, 0x08, 0x05}},
	}
	for _, tt := range tests {
		if got := withPassword(tt.req, pw); !bytes.Equal(got, tt.want) {
			t.Errorf("%s: withPassword = % X, want % X", tt.name, got, tt.want)
		}
	}

	on := request(cmdAction, actionOn)
	if got := withPassword(on, encodePassword("")); !bytes.Equal(got, on) {
		t.Errorf("no password: got % X, want % X", got, on)
	}
}

func TestCheckReply(t *testing.T) {
	tests := []struct {
		reply []byte
		want  error
	}{
		{[]byte{0x01}, nil},
		{[]byte{0x01, 0xff, 0x00}, nil},
		{nil, ErrNoResponse},
		{[]byte{0x02}, ErrActionFailed},
		{[]byte{0x03}, ErrBusy},
		{[]byte{0x05, 0x48, 0xc0}, ErrUnsupported},
		{[]byte{0x06}, ErrLowBattery},
		{[]byte{0x07}, ErrPasswordRequired},
		{[]byte{0x08}, ErrNoPasswordSet},
		{[]byte{0x09}, ErrWrongPassword},
	}
	for _, tt := range tests {
		err := checkReply(tt.reply)
		if !errors.Is(err, tt.want) || (tt.want == nil) != (err == nil) {
			t.Errorf("checkReply(% X) = %v, want %v", tt.reply, err, tt.want)
		}
	}

	var serr *StatusError
	if err := checkReply([]byte{0x0c}); !errors.As(err, &serr) || serr.Code != 0x0c {
		t.Errorf("checkReply(0C) = %v, want *StatusError with code 0x0C", err)
	}
}

func TestParseBasicInfo(t *testing.T) {
	tests := []struct {
		name  string
		reply []byte
		want  Status
		isBot bool
	}{
		{
			// Example response in SwitchBot's BLE documentation (firmware 4.4).
			"documented example",
			[]byte{0x01, 0x64, 0x2c, 0x64, 0x00, 0x00, 0x00, 0xa1, 0x00, 0x00, 0x00, 0x48, 0x00},
			Status{Battery: 100, Firmware: "4.4", Mode: ModePress},
			true,
		},
		{
			// Real Bot, firmware 6.8, press mode: device type left at 0.
			"firmware 6.8 press",
			[]byte{0x01, 0x5f, 0x44, 0x64, 0x00, 0x00, 0x00, 0x62, 0x00, 0x00, 0x00, 0x00, 0x00},
			Status{Battery: 95, Firmware: "6.8", Mode: ModePress},
			true,
		},
		{
			// Real Bot, firmware 6.8, switch mode, inverted.
			"firmware 6.8 switch inverted",
			[]byte{0x01, 0x5b, 0x44, 0x64, 0x00, 0x00, 0x00, 0x5e, 0x00, 0x11, 0x00, 0x00, 0x00},
			Status{Battery: 91, Firmware: "6.8", Mode: ModeSwitch, Inverted: true},
			true,
		},
		{
			"hold time and timers",
			[]byte{0x01, 87, 63, 100, 0, 0, 0, 0, 2, 0x10, 5},
			Status{Battery: 87, Firmware: "6.3", Mode: ModeSwitch, HoldTime: 5 * time.Second, Timers: 2},
			true,
		},
		{
			"another device type",
			[]byte{0x01, 0x64, 0x2c, 0x64, 0x00, 0x00, 0x00, 0xa1, 0x00, 0x00, 0x00, 'L', 0x00},
			Status{Battery: 100, Firmware: "4.4", Mode: ModePress},
			false,
		},
		{
			"no type, not shaped like a Bot",
			[]byte{0x01, 0x50, 0x10, 0x32, 0x00, 0x00, 0x00, 0x00, 0x00, 0x42, 0x00, 0x00, 0x00},
			Status{Battery: 80, Firmware: "1.6"},
			false,
		},
	}
	for _, tt := range tests {
		got, err := parseBasicInfo(tt.reply)
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if got.Status != tt.want || got.isBot() != tt.isBot {
			t.Errorf("%s: got %+v isBot=%v, want %+v isBot=%v", tt.name, got.Status, got.isBot(), tt.want, tt.isBot)
		}
	}

	if _, err := parseBasicInfo([]byte{0x01, 87}); err == nil {
		t.Error("short response: want error")
	}
	if _, err := parseBasicInfo([]byte{0x07}); !errors.Is(err, ErrPasswordRequired) {
		t.Errorf("password response: got %v, want ErrPasswordRequired", err)
	}
}

func TestStatusErrorText(t *testing.T) {
	tests := []struct {
		code byte
		want string
	}{
		{0x03, "switchbot: the Bot is busy (status 0x03)"},
		{0x0b, "switchbot: no mesh device nearby (status 0x0B)"},
		{0x7e, "switchbot: command failed with status 0x7E"},
	}
	for _, tt := range tests {
		if got := (&StatusError{Code: tt.code}).Error(); got != tt.want {
			t.Errorf("StatusError{0x%02X} = %q, want %q", tt.code, got, tt.want)
		}
	}
}

func TestModeString(t *testing.T) {
	for m, want := range map[Mode]string{ModePress: "press", ModeSwitch: "switch", Mode(7): "Mode(7)"} {
		if got := m.String(); got != want {
			t.Errorf("Mode(%d).String() = %q, want %q", int(m), got, want)
		}
	}
}

func TestKindString(t *testing.T) {
	for k, want := range map[kind]string{
		kindBot:            "Bot",
		kindOtherSwitchBot: "other SwitchBot device",
		kindUnknown:        "SwitchBot device, type not advertised",
		kindNotSwitchBot:   "not a SwitchBot device",
	} {
		if got := k.String(); got != want {
			t.Errorf("kind(%d).String() = %q, want %q", int(k), got, want)
		}
	}
}

func TestCommandName(t *testing.T) {
	tests := []struct {
		req  []byte
		want string
	}{
		{request(cmdAction, actionPress), "press"},
		{request(cmdAction, actionOn), "turn on"},
		{request(cmdAction, actionOff), "turn off"},
		{request(cmdAction, actionArmDown), "arm down"},
		{request(cmdAction, actionArmUp), "arm up"},
		{request(cmdAction, 0x7f), "unknown"},
		{request(cmdGetBasicInfo), "status"},
		{request(cmdSetBasicInfo, 100, 0x10), "set mode"},
		{request(cmdExtended, extSetHoldTime, 1), "set hold time"},
		{request(cmdExtended, 0x01), "unknown"},
		{[]byte{magic}, "unknown"},
	}
	for _, tt := range tests {
		if got := commandName(tt.req); got != tt.want {
			t.Errorf("commandName(% X) = %q, want %q", tt.req, got, tt.want)
		}
	}
}
