package bacnet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/jonalfarlinga/bacnet/client"
	"github.com/jonalfarlinga/bacnet/client/sim"
	"github.com/jonalfarlinga/bacnet-explorer/internal/explorer"
)

func TestParseSweepRanges(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"", "[]"},
		{" , ", "[]"},
		{"0.1-255.254", "[0.1-255.254]"},
		{"4.17", "[4.17]"},
		{" 9.1 - 11.254 ,4.1-4.254", "[4.1-4.254 9.1-11.254]"},
		// Ranges that overlap or touch are one, so that no address is tried twice.
		{"4.1-4.200, 4.100-5.10, 5.11-5.20, 7.0", "[4.1-5.20 7.0]"},
		{"0.0-255.255, 3.3", "[0.0-255.255]"},
	} {
		got, err := parseSweepRanges(tc.in)
		if err != nil || fmt.Sprint(got) != tc.want {
			t.Errorf("%q gave %v, %v, want %s", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"4", "4.256", "10.20.4.1-10.20.4.9", "4.9-4.1", "4.1-", "a.b", "4.1-4.9-5.1", "-1.2"} {
		if got, err := parseSweepRanges(bad); err == nil {
			t.Errorf("%q was read as %v", bad, got)
		}
	}
}

func TestSweepPlan(t *testing.T) {
	adapter := func(prefix, broadcast string) client.Interface {
		return client.Interface{Name: "test", Addr: netip.MustParsePrefix(prefix), Broadcast: netip.MustParseAddr(broadcast)}
	}
	tried := func(p *sweepPlan) map[string]bool {
		got := map[string]bool{}
		p.each(func(a netip.AddrPort) bool {
			if a.Port() != 47808 {
				t.Errorf("%v is not on the port given", a)
			}
			got[a.Addr().String()] = true
			return true
		})
		return got
	}

	// No ranges, no sweep.
	if p, err := newSweepPlan(adapter("10.20.4.7/24", "10.20.4.255"), 47808, " ", "02:00"); p != nil || err != nil {
		t.Errorf("no ranges gave %v, %v, want no plan", p, err)
	}
	if _, err := newSweepPlan(adapter("10.20.4.7/24", "10.20.4.255"), 47808, "4.1-4.9", "25:00"); err == nil {
		t.Error("a sweep at 25:00 was accepted")
	}

	// An adapter on a subnet of 256, sweeping its own subnet and the next:
	// the first two bytes are the adapter's, and left out are its own
	// address, its subnet's two ends, and the next subnet's likely two.
	p, err := newSweepPlan(adapter("10.20.4.7/24", "10.20.4.255"), 47808, "4.0-5.255", "")
	if err != nil {
		t.Fatal(err)
	}
	got := tried(p)
	for _, out := range []string{"10.20.4.0", "10.20.4.7", "10.20.4.255", "10.20.5.0", "10.20.5.255"} {
		if got[out] {
			t.Errorf("%s is tried", out)
		}
	}
	if len(got) != 507 || p.count() != 507 || !got["10.20.4.1"] || !got["10.20.5.254"] {
		t.Errorf("%d addresses tried, counted as %d, want the 507 hosts of 10.20.4.0/23", len(got), p.count())
	}
	if p.String() != "10.20.4.0-10.20.5.255" || p.clock.String() != "daily at 02:00" {
		t.Errorf("the plan reads %q, %q", p, p.clock)
	}

	// On a subnet of 65536 every address but the two ends is a host's.
	p, err = newSweepPlan(adapter("172.16.9.9/16", "172.16.255.255"), 47808, "0.0-255.255", "now")
	if err != nil {
		t.Fatal(err)
	}
	got = tried(p)
	if len(got) != 65533 || got["172.16.0.0"] || got["172.16.255.255"] || got["172.16.9.9"] || !got["172.16.3.255"] || !got["172.16.4.0"] {
		t.Errorf("%d addresses tried on a /16, want all 65533 hosts but the adapter", len(got))
	}
}

func TestSweepClock(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	two, err := parseSweepClock("02:00")
	if err != nil {
		t.Fatal(err)
	}
	if got := two.next(at("2026-10-08 01:30"), time.Time{}); !got.Equal(at("2026-10-08 02:00")) {
		t.Errorf("before two, the next sweep is at %v", got)
	}
	// A sweep that started on time is not started again the same night.
	if got := two.next(at("2026-10-08 02:00"), at("2026-10-08 02:00")); !got.Equal(at("2026-10-09 02:00")) {
		t.Errorf("at two, the next sweep is at %v", got)
	}
	if got := two.next(at("2026-12-31 23:59"), time.Time{}); !got.Equal(at("2027-01-01 02:00")) {
		t.Errorf("on the last night of the year, the next sweep is at %v", got)
	}

	now, err := parseSweepClock("now")
	if err != nil {
		t.Fatal(err)
	}
	if got := now.next(at("2026-10-08 14:00"), time.Time{}); !got.Equal(at("2026-10-08 14:00").Add(sweepSettle)) {
		t.Errorf("the first sweep on opening is at %v, want it once the Who-Is has been answered", got)
	}
	if got := now.next(at("2026-10-08 15:10"), at("2026-10-08 14:00")); !got.Equal(at("2026-10-09 14:00")) {
		t.Errorf("the sweep after one on opening is at %v", got)
	}
	for _, bad := range []string{"2", "24:00", "noon"} {
		if _, err := parseSweepClock(bad); err == nil {
			t.Errorf("%q was read as a time of day", bad)
		}
	}
}

// sweepOn opens a source that hears no I-Am, with a sweep of 127.0.0.1 to
// 127.0.0.3 on the port of one simulated station: the station is at the
// first address, and nobody at the other two.
func sweepOn(t *testing.T, network *sim.Network, station int) (*Source, *sweeper) {
	t.Helper()
	s := New(Options{Timeout: 300 * time.Millisecond})
	t.Cleanup(func() { s.Close() })
	err := s.use("test", client.Config{
		Local:     netip.MustParseAddrPort("127.0.0.1:0"),
		Broadcast: netip.MustParseAddrPort("127.0.0.1:9"),
		Timeout:   300 * time.Millisecond,
		Retries:   2,
	})
	if err != nil {
		t.Fatal(err)
	}
	w := &sweeper{perSec: 1000, plan: &sweepPlan{
		prefix: [2]byte{127, 0}, ranges: []hostRange{{1, 3}}, port: network.Addrs()[station].Port(),
		local: netip.MustParseAddr("127.0.0.9"), subnet: netip.MustParsePrefix("127.0.0.0/8"),
		broadcast: netip.MustParseAddr("127.255.255.255"),
	}}
	s.mu.Lock()
	s.sweep = w
	s.mu.Unlock()
	return s, w
}

func statusLine(s *Source, label string) string {
	for _, l := range s.Status().Background {
		if l.Label == label {
			return l.Value
		}
	}
	return ""
}

// A sweep lists the device that answers at one of its addresses, as one
// added by its address is, and leaves its address alone the next day.
func TestSweep(t *testing.T) {
	network, err := sim.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(network.Close)
	s, w := sweepOn(t, network, 3)
	ctx := context.Background()

	start := time.Now()
	s.sweepOnce(ctx, s.c, w)
	// Nobody is at two of the addresses: each is waited on once, together.
	if d := time.Since(start); d > 800*time.Millisecond {
		t.Errorf("the sweep of three addresses took %s: an address that did not answer was tried again", d)
	}
	if r := w.last; r == nil || r.total != 3 || r.tried != 3 || r.listed != 0 || r.found != 1 || len(r.unnamed) != 0 || r.stopped {
		t.Fatalf("the sweep counted %+v, want three tried and one found", r)
	}
	s.mu.Lock()
	d := s.devices[4000]
	n := len(s.devices)
	s.mu.Unlock()
	if n != 1 || d == nil || !d.added || d.dev.Addr.IP != network.Addrs()[3] || d.dev.MaxAPDU != 206 {
		t.Fatalf("the sweep listed %d devices, with device 4000 as %+v", n, d)
	}
	waitFor(t, "the device found to be named", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.devices[4000].name == "BOILER-1"
	})
	if got := statusLine(s, "Sweep"); got != "127.0.0.1-127.0.0.3, daily at 00:00" {
		t.Errorf("the status names the sweep as %q", got)
	}
	// The admin is told when it started and the last address it tried.
	got := statusLine(s, "Last sweep")
	if !strings.HasPrefix(got, "started "+w.last.started.Format("2 Jan 15:04:05")+", last address 127.0.0.3, 3 addresses in ") ||
		!strings.HasSuffix(got, ": 1 device found") {
		t.Errorf("the status says of the last sweep: %q", got)
	}
	// The client keeps nothing of the addresses where nobody was.
	if peers := s.c.Stats().Peers; len(peers) != 1 {
		t.Errorf("the client counts %v, want the one device that answered", peers)
	}

	// The address of a listed device is not tried again: the device gets
	// nothing from the next sweep.
	before := s.c.Stats().Peers[network.Addrs()[3].String()].Sent
	s.sweepOnce(ctx, s.c, w)
	if r := w.last; r.total != 2 || r.tried != 2 || r.listed != 1 || r.found != 0 {
		t.Errorf("the second sweep counted %+v, want two tried and the listed address left alone", r)
	}
	if after := s.c.Stats().Peers[network.Addrs()[3].String()].Sent; after != before {
		t.Errorf("the listed device was sent %d requests by the second sweep", after-before)
	}
	if got := statusLine(s, "Last sweep"); !strings.HasSuffix(got, ": 0 devices found; 1 address left alone, listed already") {
		t.Errorf("the status says of the second sweep: %q", got)
	}

	// A sweep that the closing of the source cuts short says so.
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	s.sweepOnce(stopped, s.c, w)
	if r := w.last; r.tried != 0 || !r.stopped {
		t.Errorf("a sweep under a context that had ended counted %+v", r)
	}
}

// A device the store holds from an earlier run is in the registry before it
// has answered in this one: the sweep leaves its address alone all the same.
func TestSweepSkipsWhatTheStoreHolds(t *testing.T) {
	network, err := sim.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(network.Close)
	s, w := sweepOn(t, network, 3)
	at := network.Addrs()[3]
	kept, err := json.Marshal(native{Instance: 4000, IP: at.String(), MaxAPDU: 206, Segmentation: 3})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Restore(context.Background(), []explorer.Node{{ID: devID(4000), Kind: explorer.KindDevice, Native: kept}})
	if err != nil {
		t.Fatal(err)
	}

	s.sweepOnce(context.Background(), s.c, w)
	if r := w.last; r.total != 2 || r.tried != 2 || r.listed != 1 || r.found != 0 {
		t.Errorf("the sweep counted %+v, want two tried and the kept device's address left alone", r)
	}
	if peers := s.c.Stats().Peers; len(peers) != 0 {
		t.Errorf("the client sent to %v, want nothing sent to the kept device", peers)
	}
}

// A device older than the wildcard instance answers the sweep without
// saying which device it is. Asked alone, it does, and is listed.
func TestSweepUnnamed(t *testing.T) {
	stations := sim.Default()
	stations[3][0].NoWildcard = true
	network, err := sim.StartDevices(stations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(network.Close)
	s, w := sweepOn(t, network, 3)

	s.sweepOnce(context.Background(), s.c, w)
	if r := w.last; r.tried != 3 || r.found != 0 || len(r.unnamed) != 1 || r.unnamed[0] != network.Addrs()[3] {
		t.Fatalf("the sweep counted %+v, want one address that would not say which device it is", r)
	}
	waitFor(t, "the device to say which it is", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.devices[4000] != nil
	})
	s.mu.Lock()
	added := s.devices[4000].added
	s.mu.Unlock()
	if added {
		t.Error("the device that announced itself is listed as one that did not")
	}
	// Once it is listed, the status no longer names its address.
	if got := statusLine(s, "Last sweep"); strings.Contains(got, "would not say") {
		t.Errorf("the status still says of the last sweep: %q", got)
	}

	// While a sweep runs, the status says when it started and where it is.
	started := time.Date(2026, 10, 12, 2, 0, 0, 0, time.Local)
	going := sweepRun{started: started, total: 65022, tried: 1203, found: 2, at: netip.MustParseAddr("10.20.4.180")}
	if got := going.String(); got != "started 12 Oct 02:00:00, now at 10.20.4.180, 1203 of 65022 addresses tried: 2 devices found" {
		t.Errorf("a running sweep reads %q", got)
	}

	// One that stays silent to the Who-Is is named for the admin.
	run := sweepRun{started: time.Now(), took: time.Minute, total: 9, tried: 9, unnamed: []netip.AddrPort{netip.MustParseAddrPort("10.20.3.4:47808")}}
	if got := run.String(); !strings.HasSuffix(got, "9 addresses in 1m0s: 0 devices found; 1 would not say which device they are: 10.20.3.4") {
		t.Errorf("a sweep with an unnamed address reads %q", got)
	}
}

// With no ranges given there is no sweep, and the settings offer one.
func TestSweepIsOffUnlessAsked(t *testing.T) {
	s := New(Options{})
	var keys []string
	for _, f := range s.Describe().Settings {
		keys = append(keys, f.Key)
		if f.Key == "sweep" && (f.Default != "" || f.Required) {
			t.Errorf("the sweep setting is %+v, want it empty unless filled in", f)
		}
	}
	if got := strings.Join(keys, " "); !strings.Contains(got, "sweep sweep_at") {
		t.Errorf("the settings are %q, want the sweep among them", got)
	}
}
