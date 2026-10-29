package switchbot

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"time"

	"tinygo.org/x/bluetooth"
)

// See https://github.com/OpenWonderLabs/SwitchBotAPI-BLE for the protocol.

var (
	// SwitchBot's GATT service and characteristics.
	serviceUUID = mustUUID("cba20d00-224d-11e6-9fb8-0002a5d5c51b")
	writeUUID   = mustUUID("cba20002-224d-11e6-9fb8-0002a5d5c51b") // requests are written here
	notifyUUID  = mustUUID("cba20003-224d-11e6-9fb8-0002a5d5c51b") // responses arrive here

	// 16-bit UUIDs SwitchBot devices use for their advertised service data.
	// Firmware since Bot v6.4 uses 0xfd3d; older firmware uses 0x000d (some
	// stacks report it as 0x0d00).
	serviceDataUUIDs = []bluetooth.UUID{
		bluetooth.New16BitUUID(0xfd3d),
		bluetooth.New16BitUUID(0x000d),
		bluetooth.New16BitUUID(0x0d00),
	}
)

// switchBotCompanyID is the Bluetooth SIG company ID (Woan Technology) in
// SwitchBot advertisements' manufacturer data.
const switchBotCompanyID uint16 = 0x0969

// botLocalName is the name a Bot advertises.
const botLocalName = "WoHand"

// deviceTypeBot is the device type byte of a Bot, in advertised service data
// and in the basic info response.
const deviceTypeBot = 'H'

// Requests are: magic, header (low 4 bits: command), payload.
const magic = 0x57

// Commands.
const (
	cmdAction       = 0x01
	cmdGetBasicInfo = 0x02
	cmdSetBasicInfo = 0x03
	cmdExtended     = 0x0f
)

// Payloads.
const (
	actionPress   = 0x00 // push and pull back
	actionOn      = 0x01
	actionOff     = 0x02
	actionArmDown = 0x03 // push and stay down
	actionArmUp   = 0x04 // pull back

	extSetHoldTime = 0x08

	// strength is the first byte of a set-mode payload, which the
	// documentation says must always be 100. Bots also report it in their
	// basic info.
	strength = 100

	modeSwitchBit   = 0x10
	modeInvertedBit = 0x01
)

// Response status codes.
const (
	statusOK            = 0x01
	statusFailed        = 0x02
	statusBusy          = 0x03
	statusVersion       = 0x04
	statusUnsupported   = 0x05
	statusLowBattery    = 0x06
	statusEncrypted     = 0x07
	statusUnencrypted   = 0x08
	statusWrongPassword = 0x09
)

func request(cmd byte, payload ...byte) []byte {
	return append([]byte{magic, cmd}, payload...)
}

// commandName describes a request for logs.
func commandName(req []byte) string {
	if len(req) < 2 {
		return "unknown"
	}
	switch req[1] & 0x0f {
	case cmdGetBasicInfo:
		return "status"
	case cmdSetBasicInfo:
		return "set mode"
	case cmdExtended:
		if len(req) > 2 && req[2] == extSetHoldTime {
			return "set hold time"
		}
	case cmdAction:
		if len(req) > 2 {
			switch req[2] {
			case actionPress:
				return "press"
			case actionOn:
				return "turn on"
			case actionOff:
				return "turn off"
			case actionArmDown:
				return "arm down"
			case actionArmUp:
				return "arm up"
			}
		}
	}
	return "unknown"
}

// encodePassword returns the 4-byte form of a Bot password: the CRC-32
// (IEEE) of the password, big-endian. An empty password means no password.
func encodePassword(password string) []byte {
	if password == "" {
		return nil
	}
	return binary.BigEndian.AppendUint32(nil, crc32.ChecksumIEEE([]byte(password)))
}

// withPassword turns a request into the form a password-protected Bot
// expects: encryption mode 1 in bits 5:4 of the header, followed by the
// encoded password. With no password the request is returned unchanged.
func withPassword(req, encodedPassword []byte) []byte {
	if len(encodedPassword) == 0 {
		return req
	}
	out := make([]byte, 0, len(req)+len(encodedPassword))
	out = append(out, req[0], 0x10|req[1])
	out = append(out, encodedPassword...)
	return append(out, req[2:]...)
}

// checkReply returns nil if reply reports success, and the matching error
// otherwise.
func checkReply(reply []byte) error {
	if len(reply) == 0 {
		return ErrNoResponse
	}
	if reply[0] != statusOK {
		return &StatusError{Code: reply[0]}
	}
	return nil
}

// Mode is a Bot's operating mode.
type Mode int

const (
	// ModePress: the Bot pushes the button and pulls back. Use [Bot.Press].
	ModePress Mode = iota
	// ModeSwitch: the Bot remembers on/off, for switches that are pushed one
	// way to turn on and the other way to turn off. Use [Bot.TurnOn] and
	// [Bot.TurnOff].
	ModeSwitch
)

func (m Mode) String() string {
	switch m {
	case ModePress:
		return "press"
	case ModeSwitch:
		return "switch"
	}
	return fmt.Sprintf("Mode(%d)", int(m))
}

// Status holds the settings and state a Bot reports.
type Status struct {
	Battery  int    // percent
	Firmware string // e.g. "6.8"
	Mode     Mode
	Inverted bool          // the arm's direction is inverted
	HoldTime time.Duration // how long a press holds the button down
	Timers   int           // number of timers set in the SwitchBot app
}

// basicInfo is the decoded response to a get-basic-info request.
type basicInfo struct {
	Status
	deviceType byte // 0 if the device left it out
	looksBot   bool // the response has a Bot's layout
}

// parseBasicInfo decodes the response to a get-basic-info request:
// status, battery, firmware, strength, ADC (2), motor calibration (2),
// timers, mode, hold time, device type, service data byte 1.
func parseBasicInfo(reply []byte) (basicInfo, error) {
	if err := checkReply(reply); err != nil {
		return basicInfo{}, err
	}
	if len(reply) < 11 {
		return basicInfo{}, fmt.Errorf("switchbot: status response too short (% X)", reply)
	}
	info := basicInfo{Status: Status{
		Battery:  int(reply[1]),
		Firmware: fmt.Sprintf("%d.%d", reply[2]/10, reply[2]%10),
		Timers:   int(reply[8]),
		Inverted: reply[9]&modeInvertedBit != 0,
		HoldTime: time.Duration(reply[10]) * time.Second,
	}}
	if reply[9]&modeSwitchBit != 0 {
		info.Mode = ModeSwitch
	}
	if len(reply) >= 12 {
		info.deviceType = reply[11] & 0x7f
	}
	// A Bot always reports strength 100, and its mode byte has only the
	// mode and inverted bits.
	info.looksBot = reply[3] == strength && reply[9]&^(modeSwitchBit|modeInvertedBit) == 0
	return info, nil
}

// isBot reports whether a get-basic-info response comes from a Bot. Bots
// with older firmware report their type; newer firmware (seen on 6.8)
// leaves it at 0, so then the response's layout decides.
func (i basicInfo) isBot() bool {
	if i.deviceType != 0 {
		return i.deviceType == deviceTypeBot
	}
	return i.looksBot
}

// kind is what an advertisement says about a device.
type kind int

const (
	kindNotSwitchBot kind = iota
	kindBot
	kindOtherSwitchBot // a SwitchBot device that is not a Bot
	kindUnknown        // a SwitchBot device that did not advertise its type
)

func (k kind) String() string {
	switch k {
	case kindBot:
		return "Bot"
	case kindOtherSwitchBot:
		return "other SwitchBot device"
	case kindUnknown:
		return "SwitchBot device, type not advertised"
	}
	return "not a SwitchBot device"
}

// sighting is what one advertisement tells about a device.
type sighting struct {
	addr bluetooth.Address
	rssi int16
	kind kind
}

// classify works out from an advertisement whether it comes from a Bot.
func classify(r bluetooth.ScanResult) sighting {
	s := sighting{addr: r.Address, rssi: r.RSSI}
	for _, sd := range r.ServiceData() {
		if !isServiceDataUUID(sd.UUID) || len(sd.Data) == 0 {
			continue
		}
		// Byte 0: bit 7 encryption, bits 6:0 device type.
		if sd.Data[0]&0x7f == deviceTypeBot {
			s.kind = kindBot
		} else {
			s.kind = kindOtherSwitchBot
		}
		return s
	}
	if r.LocalName() == botLocalName {
		s.kind = kindBot
		return s
	}
	if r.HasServiceUUID(serviceUUID) {
		s.kind = kindUnknown
		return s
	}
	for _, md := range r.ManufacturerData() {
		if md.CompanyID == switchBotCompanyID {
			s.kind = kindUnknown
			return s
		}
	}
	return s
}

func isServiceDataUUID(u bluetooth.UUID) bool {
	for _, v := range serviceDataUUIDs {
		if u == v {
			return true
		}
	}
	return false
}

func mustUUID(s string) bluetooth.UUID {
	u, err := bluetooth.ParseUUID(s)
	if err != nil {
		panic(err)
	}
	return u
}

// errWrite marks a request that could not be written, so it never reached
// the Bot and can safely be sent again.
var errWrite = errors.New("switchbot: write request")
