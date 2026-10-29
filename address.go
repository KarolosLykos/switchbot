//go:build !darwin

package switchbot

import (
	"fmt"
	"strings"

	"tinygo.org/x/bluetooth"
)

// parseAddress parses a MAC address such as "AA:BB:CC:DD:EE:FF". Lowercase
// and "-" separators are accepted too.
func parseAddress(s string) (bluetooth.Address, error) {
	norm := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), "-", ":"))
	mac, err := bluetooth.ParseMAC(norm)
	if err != nil {
		return bluetooth.Address{}, fmt.Errorf("switchbot: invalid address %q: want a MAC address like AA:BB:CC:DD:EE:FF", s)
	}
	return bluetooth.Address{MACAddress: bluetooth.NewMACAddress(mac, false)}, nil
}
