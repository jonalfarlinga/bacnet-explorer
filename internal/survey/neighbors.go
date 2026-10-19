package survey

import (
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
)

// The operating system keeps a neighbor table: the addresses on its subnets
// that it has resolved to a MAC by ARP lately. Sending anything to an
// address on a subnet makes it resolve the address first, and a host there
// answers the ARP whatever else it ignores, so after the survey has sent to
// every address, the table says which of them have a host. An address that
// nothing answered for is not in it: the system keeps such an entry as
// incomplete or unreachable, without a MAC, and the listing leaves it out
// or shows it without one.

// neighbors reads the neighbor table: Linux from /proc/net/arp, Windows and
// macOS from arp -a.
func neighbors() (map[netip.Addr]string, error) {
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile("/proc/net/arp")
		if err != nil {
			return nil, err
		}
		return parseNeighbors(string(b)), nil
	}
	out, err := exec.Command("arp", "-a").Output()
	if err != nil {
		return nil, fmt.Errorf("arp -a: %w", err)
	}
	return parseNeighbors(string(out)), nil
}

var macPattern = regexp.MustCompile(`^[0-9A-Fa-f]{1,2}([-:][0-9A-Fa-f]{1,2}){5}$`)

// parseNeighbors reads the table as any of the three print it: a line per
// entry, with the IPv4 address and the MAC somewhere on it. Entries without
// a usable MAC are left out: one not resolved yet or found unreachable
// ("(incomplete)", "<incomplete>", or all zeros on Linux), and the
// broadcast and multicast entries, which are no host's.
func parseNeighbors(text string) map[netip.Addr]string {
	table := map[netip.Addr]string{}
	for _, line := range strings.Split(text, "\n") {
		var ip netip.Addr
		var mac string
		for _, field := range strings.Fields(line) {
			if !ip.IsValid() {
				if a, err := netip.ParseAddr(strings.Trim(field, "()")); err == nil && a.Is4() {
					ip = a
					continue
				}
			}
			if mac == "" && macPattern.MatchString(field) {
				mac = normalizeMAC(field)
			}
		}
		if !ip.IsValid() || mac == "" || !isHostMAC(mac) {
			continue
		}
		table[ip] = mac
	}
	return table
}

// normalizeMAC writes a MAC as six lower-case pairs with colons, whatever
// the system printed.
func normalizeMAC(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ':' || r == '-' })
	for i, p := range parts {
		if len(p) == 1 {
			p = "0" + p
		}
		parts[i] = strings.ToLower(p)
	}
	return strings.Join(parts, ":")
}

// isHostMAC reports whether a MAC is one host's: not all zeros, not the
// broadcast address, and not a multicast address, whose first byte is odd.
func isHostMAC(mac string) bool {
	switch mac {
	case "00:00:00:00:00:00", "ff:ff:ff:ff:ff:ff":
		return false
	}
	var first byte
	fmt.Sscanf(mac[:2], "%02x", &first)
	return first&1 == 0
}
