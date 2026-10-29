package switchbot

import (
	"errors"
	"fmt"
)

var (
	// ErrNotFound means the Bot could not be found to connect to. It may be
	// out of range, or connected to another app.
	ErrNotFound = errors.New("switchbot: Bot not found")

	// ErrNotABot means the device at that address is not a Bot.
	ErrNotABot = errors.New("switchbot: device is not a Bot")

	// ErrNoResponse means the Bot did not answer in time. The command may or
	// may not have been carried out.
	ErrNoResponse = errors.New("switchbot: no response from the Bot")

	// ErrConnectionLost means the connection dropped while waiting for the
	// Bot's answer. The command may or may not have been carried out. The
	// next command reconnects.
	ErrConnectionLost = errors.New("switchbot: connection to the Bot lost")

	// ErrServiceNotFound means the device does not offer SwitchBot's
	// Bluetooth service.
	ErrServiceNotFound = errors.New("switchbot: SwitchBot Bluetooth service not found")
)

// Errors matched by a [*StatusError] with errors.Is.
var (
	ErrActionFailed     = errors.New("switchbot: the Bot could not carry out the action")
	ErrBusy             = errors.New("switchbot: the Bot is busy")
	ErrUnsupported      = errors.New("switchbot: command not supported in the Bot's current mode")
	ErrLowBattery       = errors.New("switchbot: the Bot's battery is too low")
	ErrPasswordRequired = errors.New("switchbot: the Bot has a password; use WithPassword")
	ErrNoPasswordSet    = errors.New("switchbot: a password was given but the Bot has none")
	ErrWrongPassword    = errors.New("switchbot: wrong password")
)

var statusErrors = map[byte]error{
	statusFailed:        ErrActionFailed,
	statusBusy:          ErrBusy,
	statusUnsupported:   ErrUnsupported,
	statusLowBattery:    ErrLowBattery,
	statusEncrypted:     ErrPasswordRequired,
	statusUnencrypted:   ErrNoPasswordSet,
	statusWrongPassword: ErrWrongPassword,
}

var statusText = map[byte]string{
	statusVersion: "protocol version not supported",
	0x0a:          "encryption method not supported",
	0x0b:          "no mesh device nearby",
	0x0c:          "could not connect to the network",
}

// StatusError is returned when a Bot answers a command with a status other
// than OK. Use errors.Is with ErrUnsupported, ErrBusy, ErrLowBattery,
// ErrActionFailed, ErrPasswordRequired, ErrNoPasswordSet or
// ErrWrongPassword to check for common cases.
type StatusError struct {
	Code byte // status code sent by the Bot
}

func (e *StatusError) Error() string {
	if err, ok := statusErrors[e.Code]; ok {
		return fmt.Sprintf("%v (status 0x%02X)", err, e.Code)
	}
	if text, ok := statusText[e.Code]; ok {
		return fmt.Sprintf("switchbot: %s (status 0x%02X)", text, e.Code)
	}
	return fmt.Sprintf("switchbot: command failed with status 0x%02X", e.Code)
}

// Unwrap returns the ErrXxx value matching the status code, if there is one.
func (e *StatusError) Unwrap() error {
	return statusErrors[e.Code]
}
