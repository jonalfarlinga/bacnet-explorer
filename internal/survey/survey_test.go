package survey

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/jonalfarlinga/bacnet/client"
	"github.com/jonalfarlinga/bacnet/client/sim"
)

func TestParseRanges(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "[]"},
		{"192.168.1.5", "[192.168.1.5]"},
		{" 192.168.1.20 - 192.168.1.60 , 192.168.1.200", "[192.168.1.20-192.168.1.60 192.168.1.200]"},
	} {
		got, err := ParseRanges(tc.in)
		if err != nil || fmt.Sprint(got) != tc.want {
			t.Errorf("%q gave %v, %v, want %s", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"192.168.1", "192.168.1.60-192.168.1.20", "fe80::1", "4.1-4.9"} {
		if got, err := ParseRanges(bad); err == nil {
			t.Errorf("%q was read as %v", bad, got)
		}
	}
}

func TestAddresses(t *testing.T) {
	subnet := netip.MustParsePrefix("192.168.1.0/24")
	local := netip.MustParseAddr("192.168.1.101")
	all, err := addresses(subnet, local, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 253 || all[0] != netip.MustParseAddr("192.168.1.1") || all[252] != netip.MustParseAddr("192.168.1.254") {
		t.Errorf("the whole subnet is %d addresses from %s to %s, want the 253 hosts but this machine", len(all), all[0], all[len(all)-1])
	}
	for _, a := range all {
		if a == local {
			t.Error("this machine's own address is tried")
		}
	}

	// Ranges that overlap are tried once, and the subnet's own address and
	// its broadcast address are not hosts.
	ranges, _ := ParseRanges("192.168.1.0-192.168.1.3, 192.168.1.2-192.168.1.4, 192.168.1.255")
	some, err := addresses(subnet, local, ranges)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(some); got != "[192.168.1.1 192.168.1.2 192.168.1.3 192.168.1.4]" {
		t.Errorf("the ranges gave %s", got)
	}

	// Beyond the subnet the addresses ending in 0 and 255 are left out,
	// the subnets there not being known.
	ranges, _ = ParseRanges("10.117.0.254-10.117.1.2")
	beyond, err := addresses(subnet, local, ranges)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(beyond); got != "[10.117.0.254 10.117.1.1 10.117.1.2]" {
		t.Errorf("the range beyond the subnet gave %s", got)
	}
	if _, err := addresses(subnet, local, []Range{{netip.MustParseAddr("10.0.0.0"), netip.MustParseAddr("10.4.255.255")}}); err == nil {
		t.Error("a range of more than four subnets of 65536 was accepted")
	}

	// A /30 has two hosts; a /8 is bounded to the /16 around the adapter.
	small, _ := addresses(netip.MustParsePrefix("10.0.0.0/30"), netip.MustParseAddr("10.0.0.1"), nil)
	if got := fmt.Sprint(small); got != "[10.0.0.2]" {
		t.Errorf("a /30 gave %s", got)
	}
	if got := last(netip.MustParsePrefix("172.16.9.9/16")); got != netip.MustParseAddr("172.16.255.255") {
		t.Errorf("the last address of a /16 is %s", got)
	}
}

func TestRuns(t *testing.T) {
	addrs := []netip.Addr{
		netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.3"), netip.MustParseAddr("10.0.0.4"),
		netip.MustParseAddr("10.0.0.9"),
		netip.MustParseAddr("10.0.0.254"), netip.MustParseAddr("10.0.0.255"), netip.MustParseAddr("10.0.1.0"),
	}
	if got := fmt.Sprint(Runs(addrs)); got != "[10.0.0.2-10.0.0.4 10.0.0.9 10.0.0.254-10.0.1.0]" {
		t.Errorf("the runs are %s", got)
	}
	if got := Runs(nil); got != nil {
		t.Errorf("no addresses gave %v", got)
	}
}

// surveyOf opens a client that hears no broadcast and sets a survey of the
// loopback subnet over 127.0.0.1 to 127.0.0.3 on the port of one simulated
// station: a device is at the first address and nobody at the other two.
// The neighbor table is faked: 127.0.0.2 is a host without BACnet,
// 127.0.0.3 is in no table. Pings fail the test: nothing is pinged on the
// subnet.
func surveyOf(t *testing.T, network *sim.Network, station int) (*survey, []netip.Addr) {
	t.Helper()
	s := newSurvey(netip.MustParsePrefix("127.0.0.0/8"), network.Addrs()[station].Port(), 300*time.Millisecond, 1000)
	c, err := client.Open(client.Config{
		Local:     netip.MustParseAddrPort("127.0.0.1:0"),
		Broadcast: netip.MustParseAddrPort("127.0.0.1:9"),
		Timeout:   300 * time.Millisecond,
		ReadOnly:  true,
		OnIAm:     s.hear,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	s.c = c
	s.lookup = func() (map[netip.Addr]string, error) {
		return map[netip.Addr]string{netip.MustParseAddr("127.0.0.2"): "00:c0:e4:0d:e3:ed"}, nil
	}
	s.ping = func(_ context.Context, a netip.Addr, _ time.Duration) (echo, error) {
		t.Errorf("%s, on the subnet, was pinged", a)
		return echo{}, nil
	}
	return s, []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.2"), netip.MustParseAddr("127.0.0.3")}
}

// The device that answers is listed with its name, the host that only
// answers ARP as another host, and the address in neither as vacant.
func TestSurvey(t *testing.T) {
	network, err := sim.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(network.Close)
	s, addrs := surveyOf(t, network, 3)
	ctx := context.Background()

	var reports []Progress
	s.progress = func(p Progress) { reports = append(reports, p) }
	start := time.Now()
	s.try(ctx, addrs)
	// Nobody is at two of the addresses: each is waited on once, together.
	if d := time.Since(start); d > 800*time.Millisecond {
		t.Errorf("trying three addresses took %s: an address that did not answer was tried again", d)
	}
	s.identify(ctx)
	res := s.result(addrs)

	if res.Tried != 3 || res.Total != 3 || res.Beyond != 0 || res.NeighborsErr != nil || res.PingErr != nil {
		t.Errorf("the survey counted %+v", res)
	}
	if len(res.Devices) != 1 || res.Devices[0].Addr != addrs[0] || res.Devices[0].Instance != 4000 ||
		res.Devices[0].Name != "BOILER-1" || res.Devices[0].Vendor == "" || res.Devices[0].Unnamed {
		t.Errorf("the devices are %+v, want device 4000, BOILER-1, at %s", res.Devices, addrs[0])
	}
	if len(res.Hosts) != 1 || res.Hosts[0].Addr != addrs[1] || res.Hosts[0].MAC != "00:c0:e4:0d:e3:ed" || res.Hosts[0].Ping {
		t.Errorf("the other hosts are %+v, want %s with its MAC", res.Hosts, addrs[1])
	}
	if fmt.Sprint(res.Vacant) != "[127.0.0.3]" || len(res.Silent) != 0 || len(res.Unrouted) != 0 {
		t.Errorf("the vacant addresses are %v and the silent %v, want 127.0.0.3 vacant", res.Vacant, res.Silent)
	}
	// Progress was reported at the start and when the reads began.
	if n := len(reports); n < 2 || reports[0].Total != 3 || !reports[n-1].Identifying || reports[n-1].Devices != 1 {
		t.Errorf("progress was reported as %+v", reports)
	}
}

// Beyond the adapter's subnet the neighbor table says nothing, and the
// pings do: a reply is a host, a router's host-unreachable is a vacant
// address, its net-unreachable an unrouted one, and silence is silence.
func TestSurveyBeyond(t *testing.T) {
	network, err := sim.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(network.Close)
	s, _ := surveyOf(t, network, 3)
	s.subnet = netip.MustParsePrefix("10.22.117.32/27")
	router := netip.MustParseAddr("10.22.117.33")
	addrs := []netip.Addr{
		netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.2"), netip.MustParseAddr("127.0.0.3"),
		netip.MustParseAddr("127.0.0.4"), netip.MustParseAddr("127.0.0.5"),
	}
	s.ping = func(_ context.Context, a netip.Addr, _ time.Duration) (echo, error) {
		switch a.As4()[3] {
		case 1, 2:
			return echo{alive: true, from: a}, nil
		case 3:
			return echo{noHost: true, from: router}, nil
		case 4:
			return echo{noNet: true, from: router}, nil
		}
		return echo{}, nil
	}
	ctx := context.Background()
	s.try(ctx, addrs)
	s.identify(ctx)
	res := s.result(addrs)

	if res.Tried != 5 || res.Beyond != 5 || res.PingErr != nil {
		t.Errorf("the survey counted %+v", res)
	}
	// The device answers the ping too, and is a device all the same.
	if len(res.Devices) != 1 || res.Devices[0].Instance != 4000 || res.Devices[0].MAC != "" {
		t.Errorf("the devices are %+v", res.Devices)
	}
	if len(res.Hosts) != 1 || res.Hosts[0].Addr != addrs[1] || !res.Hosts[0].Ping || res.Hosts[0].MAC != "" {
		t.Errorf("the other hosts are %+v, want 127.0.0.2 by its ping", res.Hosts)
	}
	if fmt.Sprint(res.Vacant) != "[127.0.0.3]" || fmt.Sprint(res.Unrouted) != "[127.0.0.4]" || fmt.Sprint(res.Silent) != "[127.0.0.5]" {
		t.Errorf("vacant %v, unrouted %v, silent %v", res.Vacant, res.Unrouted, res.Silent)
	}
	if res.Routers[router] != 2 || len(res.Routers) != 1 {
		t.Errorf("the routers answered for %v, want %s for 2", res.Routers, router)
	}
}

// When pings cannot be sent at all, the survey says so, pings no more, and
// everything beyond the subnet that is not a BACnet device is silent.
func TestSurveyWithoutPings(t *testing.T) {
	network, err := sim.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(network.Close)
	s, addrs := surveyOf(t, network, 3)
	s.subnet = netip.MustParsePrefix("10.22.117.32/27")
	pings := 0
	s.ping = func(context.Context, netip.Addr, time.Duration) (echo, error) {
		s.mu.Lock()
		pings++
		s.mu.Unlock()
		return echo{}, errors.New("no ICMP")
	}
	ctx := context.Background()
	s.try(ctx, addrs)
	s.identify(ctx)
	res := s.result(addrs)
	if res.PingErr == nil || len(res.Devices) != 1 || len(res.Hosts) != 0 || fmt.Sprint(res.Silent) != "[127.0.0.2 127.0.0.3]" {
		t.Errorf("without pings the survey gave %+v", res)
	}
	// At the test's pace all three were in flight before the first failed.
	if pings > 3 {
		t.Errorf("%d pings were sent after the first failed", pings)
	}
}

// A device older than the wildcard instance answers without saying which
// device it is. Asked alone, it does, and is listed by its I-Am.
func TestSurveyUnnamed(t *testing.T) {
	stations := sim.Default()
	stations[3][0].NoWildcard = true
	network, err := sim.StartDevices(stations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(network.Close)
	s, addrs := surveyOf(t, network, 3)
	ctx := context.Background()

	s.try(ctx, addrs)
	s.mu.Lock()
	k := s.knocked[addrs[0]]
	s.mu.Unlock()
	if !k.bacnet || k.named {
		t.Fatalf("the address answered %+v, want a BACnet device that would not say which", k)
	}
	s.identify(ctx)
	res := s.result(addrs)
	if len(res.Devices) != 1 || res.Devices[0].Unnamed || res.Devices[0].Instance != 4000 || res.Devices[0].Name != "BOILER-1" {
		t.Errorf("the devices are %+v, want device 4000 named by its I-Am", res.Devices)
	}

	// One that stays silent to the Who-Is too is listed as such.
	network.Silence(3, true)
	s2, _ := surveyOf(t, network, 3)
	s2.mu.Lock()
	s2.knocked[addrs[0]] = knock{bacnet: true}
	s2.mu.Unlock()
	s2.identify(ctx)
	if res := s2.result(addrs[:1]); len(res.Devices) != 1 || !res.Devices[0].Unnamed {
		t.Errorf("a device that never said which it is was listed as %+v", res.Devices)
	}
}

// When the neighbor table cannot be read, the addresses on the subnet
// without a BACnet device are silent rather than vacant.
func TestSurveyWithoutNeighbors(t *testing.T) {
	network, err := sim.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(network.Close)
	s, addrs := surveyOf(t, network, 3)
	s.lookup = func() (map[netip.Addr]string, error) { return nil, errors.New("no arp") }
	ctx := context.Background()
	s.try(ctx, addrs)
	s.identify(ctx)
	res := s.result(addrs)
	if res.NeighborsErr == nil || len(res.Vacant) != 0 || fmt.Sprint(res.Silent) != "[127.0.0.2 127.0.0.3]" || len(res.Devices) != 1 {
		t.Errorf("without the neighbor table the survey gave %+v", res)
	}
}

// A survey stopped part way reports what it tried, and the rest as untried.
func TestSurveyStopped(t *testing.T) {
	network, err := sim.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(network.Close)
	s, addrs := surveyOf(t, network, 3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.try(ctx, addrs)
	s.identify(ctx)
	if res := s.result(addrs); res.Tried != 0 || res.Total != 3 || len(res.Vacant) != 0 || len(res.Devices) != 0 {
		t.Errorf("a survey under a context that had ended gave %+v", res)
	}
}

// Run itself, on a loopback "adapter": the subnet is bounded to a /16, this
// machine's address is left out, and the result is in order.
func TestRun(t *testing.T) {
	network, err := sim.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(network.Close)
	ifc := client.Interface{Name: "lo", Addr: netip.MustParsePrefix("127.0.0.9/8"), Broadcast: netip.MustParseAddr("127.255.255.255")}
	ranges, _ := ParseRanges("127.0.0.1-127.0.0.3, 127.0.0.9")
	res, err := Run(context.Background(), Options{
		Interface: ifc, Port: network.Addrs()[3].Port(), Ranges: ranges, PerSec: 1000, Timeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Subnet != netip.MustParsePrefix("127.0.0.0/16") || res.Local != ifc.Addr.Addr() || res.Total != 3 || res.Tried != 3 || res.Beyond != 0 || res.Stopped {
		t.Errorf("the survey reports %+v", res)
	}
	if len(res.Devices) != 1 || res.Devices[0].Instance != 4000 {
		t.Errorf("the devices are %+v", res.Devices)
	}
	if fmt.Sprint(res.Ranges) != fmt.Sprint(ranges) {
		t.Errorf("the result names the ranges as %v", res.Ranges)
	}
}

// The system's ping, as the survey uses it: this machine answers, and an
// address nobody has answers nothing within the wait.
func TestPing(t *testing.T) {
	if testing.Short() {
		t.Skip("pings the loopback address and waits on one that nobody has")
	}
	ctx := context.Background()
	e, err := ping(ctx, netip.MustParseAddr("127.0.0.1"), time.Second)
	if err != nil || !e.alive {
		t.Errorf("a ping to loopback gave %+v, %v", e, err)
	}
	// 192.0.2.0/24 is reserved for documentation and routed nowhere.
	start := time.Now()
	e, err = ping(ctx, netip.MustParseAddr("192.0.2.1"), 500*time.Millisecond)
	if err != nil || e.alive {
		t.Errorf("a ping to a documentation address gave %+v, %v", e, err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the ping that nobody answered took %s, want it back within the wait", d)
	}
}
