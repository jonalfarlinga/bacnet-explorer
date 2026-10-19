package survey

import (
	"fmt"
	"testing"
)

func TestParseNeighbors(t *testing.T) {
	windows := `
Interface: 192.168.1.101 --- 0xc
  Internet Address      Physical Address      Type
  192.168.1.1           a4-5e-60-c1-2b-7f     dynamic
  192.168.1.100         00-c0-e4-0d-e3-ed     dynamic
  192.168.1.255         ff-ff-ff-ff-ff-ff     static
  224.0.0.22            01-00-5e-00-00-16     static
  239.255.255.250       01-00-5e-7f-ff-fa     static
`
	mac := `
? (192.168.1.1) at a4:5e:60:c1:2b:7f on en0 ifscope [ethernet]
? (192.168.1.5) at (incomplete) on en0 ifscope [ethernet]
? (192.168.1.100) at 0:c0:e4:d:e3:ed on en0 ifscope [ethernet]
? (192.168.1.255) at ff:ff:ff:ff:ff:ff on en0 ifscope [ethernet]
? (224.0.0.251) at 1:0:5e:0:0:fb on en0 ifscope permanent [ethernet]
`
	linux := `IP address       HW type     Flags       HW address            Mask     Device
192.168.1.1      0x1         0x2         a4:5e:60:c1:2b:7f     *        eth0
192.168.1.5      0x1         0x0         00:00:00:00:00:00     *        eth0
192.168.1.100    0x1         0x2         00:c0:e4:0d:e3:ed     *        eth0
`
	want := "map[192.168.1.1:a4:5e:60:c1:2b:7f 192.168.1.100:00:c0:e4:0d:e3:ed]"
	for name, text := range map[string]string{"windows": windows, "macos": mac, "linux": linux} {
		if got := fmt.Sprint(parseNeighbors(text)); got != want {
			t.Errorf("%s: read as %s, want %s", name, got, want)
		}
	}
}
