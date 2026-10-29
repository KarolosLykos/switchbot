package switchbot

import (
	"fmt"

	"tinygo.org/x/bluetooth"
)

// parseAddress parses a macOS Bluetooth device UUID. macOS does not expose
// MAC addresses; use the address DiscoverBots reports.
func parseAddress(s string) (bluetooth.Address, error) {
	u, err := bluetooth.ParseUUID(s)
	if err != nil {
		return bluetooth.Address{}, fmt.Errorf("switchbot: invalid address %q: on macOS, use the UUID DiscoverBots reports", s)
	}
	return bluetooth.Address{UUID: u}, nil
}
