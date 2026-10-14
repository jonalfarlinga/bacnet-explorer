package bacnet

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/jonalfarlinga/bacnet/client"
)

// ParseAddress reads a device address as a user writes it: an IP with an
// optional port, 47808 if left out, and for a device behind a router, its
// network number and MAC in hex.
func ParseAddress(address string, network uint16, mac string) (client.Address, error) {
	return parseAddress(address, network, mac, client.DefaultPort)
}

// parseAddress is ParseAddress with the port a bare IP means.
func parseAddress(address string, network uint16, mac string, defaultPort uint16) (client.Address, error) {
	ip, err := parseAddrPort(strings.TrimSpace(address), defaultPort)
	if err != nil {
		return client.Address{}, err
	}
	addr := client.Address{IP: ip, Net: network}
	if network != 0 {
		b, err := hex.DecodeString(strings.TrimSpace(mac))
		if err != nil || len(b) == 0 || len(b) > 8 {
			return client.Address{}, fmt.Errorf("MAC %q is not 1 to 8 bytes in hex", mac)
		}
		addr.MAC = b
	} else if strings.TrimSpace(mac) != "" {
		return client.Address{}, errors.New("a MAC goes with a network number")
	}
	return addr, nil
}
