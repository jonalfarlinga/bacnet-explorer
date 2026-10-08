package bacnet

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/jonalfarlinga/bacnet/client"
	"github.com/jonalfarlinga/bacnet/client/sim"
	"github.com/jonalfarlinga/bacnet/internal/explorer"
	"github.com/jonalfarlinga/bacnet/objects"
)

// start runs the source on the simulated devices, in a registry, and waits
// until all 7 have answered and been identified.
func start(t *testing.T) (*explorer.Registry, <-chan explorer.Event) {
	t.Helper()
	network, err := sim.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(network.Close)

	reg := explorer.New()
	t.Cleanup(reg.CloseAll)
	// The simulated devices answer at once; the client's rate cap would
	// only slow the test down.
	reg.Register(New(Options{Timeout: time.Second, Sim: network, MaxRequestsPerSec: -1}))
	events, cancel := reg.Subscribe()
	t.Cleanup(cancel)
	if err := reg.Open(context.Background(), Kind, nil); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "7 named devices", func() bool {
		named := 0
		for _, n := range reg.Nodes() {
			if n.Kind == explorer.KindDevice && !n.Pending {
				named++
			}
		}
		return named == 7
	})
	return reg, events
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func nodeOf(reg *explorer.Registry, id explorer.NodeID) explorer.Node {
	n, _ := reg.Node(id)
	return n
}

func fact(n explorer.Node, label string) string {
	for _, f := range n.Facts {
		if f.Label == label {
			return f.Value
		}
	}
	return ""
}

func TestDevicesAreIdentified(t *testing.T) {
	reg, _ := start(t)

	if d := nodeOf(reg, devID(4000)); d.Label != "BOILER-1" || fact(d, "Model") != "SIM-LEGACY" {
		t.Errorf("the device without ReadPropertyMultiple is %+v, want its name and model read one by one", d)
	}
	d := nodeOf(reg, devID(2102))
	if d.Parent != netID(2001) || d.Meta != "MAC 6" {
		t.Errorf("device 2102 is %+v, want MAC 6 under network 2001", d)
	}
	if n := nodeOf(reg, netID(2001)); n.Parent != devID(2000) || n.Kind != explorer.KindGroup {
		t.Errorf("network 2001 is %+v, want a group under device 2000", n)
	}
	if d.Children.State != explorer.ChildrenUnknown {
		t.Errorf("children of a device nobody opened are %q, want unknown", d.Children.State)
	}
	// The router announced itself, so no stand-in for it is listed.
	for _, n := range reg.Nodes() {
		if n.Label == "Router" {
			t.Errorf("a router stand-in %v is listed although device 2000 answered", n.ID)
		}
	}
}

func TestChildren(t *testing.T) {
	reg, events := start(t)

	for _, tc := range []struct {
		name  string
		id    uint32
		total int
	}{
		{"a device with ReadPropertyMultiple", 1001, 25},
		{"a device that cannot segment", 3000, 241},
		{"a device without ReadPropertyMultiple", 4000, 7},
		{"a device behind a router", 2107, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := reg.Children(devID(tc.id), false); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "the objects", func() bool {
				state := nodeOf(reg, devID(tc.id)).Children.State
				return state == explorer.ChildrenReady || state == explorer.ChildrenError
			})
			if st := nodeOf(reg, devID(tc.id)).Children; st.State != explorer.ChildrenReady || st.Total != tc.total || st.Done != tc.total {
				t.Fatalf("children are %+v, want all %d ready", st, tc.total)
			}
			_, objs, _ := reg.Children(devID(tc.id), false)
			if len(objs) != tc.total {
				t.Fatalf("%d children, want %d", len(objs), tc.total)
			}
			for _, o := range objs {
				if o.Pending || o.Label == "" || o.Kind != explorer.KindPoint || o.Group == "" {
					t.Errorf("%s was left as %+v", o.ID, o)
				}
				if o.Tag == "AI" && (o.Reading == nil || o.Reading.Text == "" || o.Reading.Units == "" || o.Reading.Number == nil) {
					t.Errorf("analog input %q has reading %+v", o.Label, o.Reading)
				}
			}
		})
	}

	// The events for a node's children count up by one from each full list,
	// which is what lets a subscriber notice one it missed.
	last := map[explorer.NodeID]uint64{}
	for len(events) > 0 {
		e := <-events
		if ce, ok := e.Data.(explorer.ChildrenEvent); ok {
			if rev, seen := last[ce.Parent]; seen && ce.Rev != rev+1 {
				t.Errorf("%s: revision %d follows %d", ce.Parent, ce.Rev, rev)
			}
			last[ce.Parent] = ce.Rev
		}
	}
	if len(last) != 4 {
		t.Errorf("children events for %d devices, want 4", len(last))
	}
}

func TestChildrenShowAlarmAndBinaryState(t *testing.T) {
	reg, _ := start(t)
	reg.Children(devID(1001), false)
	waitFor(t, "the objects", func() bool { return nodeOf(reg, devID(1001)).Children.State == explorer.ChildrenReady })

	_, objs, _ := reg.Children(devID(1001), false)
	for _, o := range objs {
		if o.Label == "AHU1.FLT.DP" {
			if o.Reading == nil || o.Reading.Text != "active" || len(o.Reading.Flags) != 1 || o.Reading.Flags[0] != "in alarm" {
				t.Errorf("the filter switch is %+v, want active and in alarm", o.Reading)
			}
			if o.Family != explorer.FamilyBinary {
				t.Errorf("the filter switch is of family %q, want binary", o.Family)
			}
			return
		}
	}
	t.Error("no filter switch among the objects")
}

func TestDetail(t *testing.T) {
	reg, _ := start(t)
	ctx := context.Background()
	value := func(d explorer.Detail, name string) string {
		for _, r := range d.Rows {
			if r.Name == name {
				return r.Value
			}
		}
		return "(missing)"
	}

	// A device's detail is its Device object, with "all", from a device
	// that segments the answer.
	d, err := reg.Detail(ctx, devID(2000))
	if err != nil {
		t.Fatal(err)
	}
	if got := value(d, "Segmentation supported"); got != "both" {
		t.Errorf("segmentation-supported reads %q, want both", got)
	}
	if got := value(d, "Protocol services supported"); got != "subscribeCOV, readProperty, readPropertyMultiple, i-Am, who-Is" {
		t.Errorf("protocol-services-supported reads %q", got)
	}
	for _, r := range d.Rows {
		if r.Name == "Object list" && len(r.Items) != 124 {
			t.Errorf("object-list has %d items, want 124", len(r.Items))
		}
	}

	// One property at a time, from a device with no ReadPropertyMultiple; the
	// ones the object does not have are left out.
	d, err = reg.Detail(ctx, objID(4000, client.ObjectID{Type: objects.ObjectTypeBinaryInput, Instance: 0}))
	if err != nil {
		t.Fatal(err)
	}
	if got := value(d, "Present value"); got != "active" {
		t.Errorf("present-value of a binary input reads %q, want active", got)
	}
	if got := value(d, "Active text"); got != "Firing" {
		t.Errorf("active-text reads %q, want Firing", got)
	}
	if got := value(d, "Units"); got != "(missing)" {
		t.Errorf("a binary input has units %q", got)
	}

	if _, err := reg.Detail(ctx, devID(12345)); !errors.Is(err, explorer.ErrUnknownNode) {
		t.Errorf("a device that is not there gave %v", err)
	}
	if _, err := reg.Detail(ctx, netID(2001)); !errors.Is(err, explorer.ErrNotSupported) {
		t.Errorf("a network gave %v, want not supported", err)
	}
}

func TestRead(t *testing.T) {
	reg, _ := start(t)
	ai := objID(1001, client.ObjectID{Type: objects.ObjectTypeAnalogInput, Instance: 0})
	bi := objID(1001, client.ObjectID{Type: objects.ObjectTypeBinaryInput, Instance: 1})
	gone := objID(1001, client.ObjectID{Type: objects.ObjectTypeAnalogInput, Instance: 99})
	readings, err := reg.Read(context.Background(), []explorer.NodeID{ai, bi, gone, devID(1001)})
	if err != nil {
		t.Fatal(err)
	}
	if len(readings) != 4 {
		t.Fatalf("%d readings, want 4", len(readings))
	}
	if r := readings[0]; r.Quality != explorer.QualityGood || r.Number == nil || *r.Number < 50 || *r.Number > 60 || r.Units != "°F" {
		t.Errorf("the analog input read %+v, want a temperature near 55 °F", r)
	}
	if r := readings[1]; r.Quality != explorer.QualityGood || r.Text != "active" || len(r.Flags) != 1 {
		t.Errorf("the binary input read %+v, want active and in alarm", r)
	}
	if r := readings[2]; r.Quality != explorer.QualityError {
		t.Errorf("an object that is not there read %+v, want an error", r)
	}
	if r := readings[3]; r.Quality != explorer.QualityError {
		t.Errorf("a device read %+v, want an error: it is not a point", r)
	}
}

// A device that no Who-Is reaches is added by its address and identified
// like any other.
func TestAddByAddress(t *testing.T) {
	network, err := sim.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(network.Close)

	// The Who-Is goes nowhere: no device announces itself.
	s := New(Options{Timeout: time.Second})
	t.Cleanup(func() { s.Close() })
	err = s.use("test", client.Config{
		Local:     netip.MustParseAddrPort("127.0.0.1:0"),
		Broadcast: netip.MustParseAddrPort("127.0.0.1:9"),
		Timeout:   time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	s.mu.Lock()
	n := len(s.devices)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d devices answered a Who-Is to nobody", n)
	}

	stations := network.Addrs()
	added, err := s.Add(context.Background(), explorer.Values{"address": stations[3].String()})
	if err != nil {
		t.Fatal(err)
	}
	if added.ID != devID(4000) || fact(added, "Listed") == "" || added.Meta != stations[3].String() || fact(added, "Max APDU") != "206 bytes" {
		t.Errorf("added %+v, want device 4000 at %v with a 206-byte APDU, listed by address", added, stations[3])
	}
	waitFor(t, "the added device to be named", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.devices[4000].name != ""
	})

	if _, err := s.Add(context.Background(), explorer.Values{"address": "127.0.0.1:9"}); !errors.Is(err, client.ErrTimeout) {
		t.Errorf("adding nobody gave %v, want a timeout", err)
	}
	if _, err := s.Add(context.Background(), explorer.Values{"address": "nonsense"}); err == nil {
		t.Error("adding a non-address did not fail")
	}
}

func TestFormatProperty(t *testing.T) {
	for _, tc := range []struct {
		name    string
		objType uint16
		prop    uint16
		values  []client.Value
		want    string
		items   int
	}{
		{"units", 0, objects.PropertyIdUnits, []client.Value{{Kind: client.KindEnumerated, Uint: uint64(objects.UnitDegreeFahrenheit)}}, "degree fahrenheit", 0},
		{"date and time", 8, objects.PropertyIdLocalDate,
			[]client.Value{{Kind: client.KindDate, Bytes: []byte{126, 10, 5, 1}}, {Kind: client.KindTime, Bytes: []byte{14, 3, 22, 0}}},
			"2026-10-05 14:03:22", 0},
		{"wildcard date", 8, objects.PropertyIdLocalDate, []client.Value{{Kind: client.KindDate, Bytes: []byte{255, 255, 32, 255}}}, "*-*-last", 0},
		{"array", 2, objects.PropertyIdPriorityArray,
			[]client.Value{{Kind: client.KindNull}, {Kind: client.KindReal, Float: 55}}, "null, 55", 2},
		{"empty list", 8, objects.PropertyIdDeviceAddressBinding, nil, "empty", 0},
		{"real without float32 noise", 0, objects.PropertyIdPresentValue, []client.Value{{Kind: client.KindReal, Float: float64(float32(72.4))}}, "72.4", 0},
		{"unknown bit string", 0, objects.PropertyIdPresentValue, []client.Value{{Kind: client.KindBits, Bits: []bool{true, false, true}}}, "101", 0},
		{"object reference", 0, objects.PropertyIdObjectIdentifier, []client.Value{{Kind: client.KindObjectID, Object: client.ObjectID{Type: 20, Instance: 3}}}, "trend-log:3", 0},
	} {
		text, items := formatProperty(tc.objType, uint32(tc.prop), tc.values)
		if text != tc.want || len(items) != tc.items {
			t.Errorf("%s: %q with %d items, want %q with %d", tc.name, text, len(items), tc.want, tc.items)
		}
	}

	if got := propertyName(uint32(objects.PropertyIdMaxApduLengthAccepted)); got != "Max APDU length accepted" {
		t.Errorf("property 62 is named %q", got)
	}
	if got := shortNumber(-0.0001); got != "0" {
		t.Errorf("a tiny negative number is shown as %q", got)
	}
	if got := titleCase("multi-state-value"); got != "Multi State Value" {
		t.Errorf("title case gave %q", got)
	}
}

func TestParseID(t *testing.T) {
	inst, obj, ok := parseID(objID(910200, client.ObjectID{Type: 0, Instance: 3}))
	if !ok || inst != 910200 || obj == nil || obj.Type != 0 || obj.Instance != 3 {
		t.Errorf("parsed %d %v %t", inst, obj, ok)
	}
	inst, obj, ok = parseID(devID(7))
	if !ok || inst != 7 || obj != nil {
		t.Errorf("parsed %d %v %t", inst, obj, ok)
	}
	for _, bad := range []explorer.NodeID{"modbus:dev/1", "bacnet:net/2001", "bacnet:dev/x", "bacnet:dev/1/obj/0"} {
		if _, _, ok := parseID(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

// A device the store held is known again after a restart: one listed by
// address is read again at once, and keeps what was read of it.
func TestRestore(t *testing.T) {
	network, err := sim.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(network.Close)
	nowhere := client.Config{
		Local:     netip.MustParseAddrPort("127.0.0.1:0"),
		Broadcast: netip.MustParseAddrPort("127.0.0.1:9"),
		Timeout:   time.Second,
	}

	// The first run lists a device by address.
	s1 := New(Options{Timeout: time.Second})
	if err := s1.use("test", nowhere); err != nil {
		t.Fatal(err)
	}
	stations := network.Addrs()
	added, err := s1.Add(context.Background(), explorer.Values{"address": stations[3].String()})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the device to be named", func() bool {
		s1.mu.Lock()
		defer s1.mu.Unlock()
		return s1.devices[4000].name != ""
	})
	s1.mu.Lock()
	saved := s1.deviceNode(4000, s1.devices[4000])
	s1.mu.Unlock()
	s1.Close()
	if len(saved.Native) == 0 {
		t.Fatal("the device node carries nothing to restore it from")
	}

	// The second run is handed the node and reads the device again without
	// a Who-Is.
	s2 := New(Options{Timeout: time.Second})
	if err := s2.use("test", nowhere); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s2.Close() })
	if err := s2.Restore(context.Background(), []explorer.Node{saved, added}); err != nil {
		t.Fatal(err)
	}
	s2.mu.Lock()
	d := s2.devices[4000]
	s2.mu.Unlock()
	if d == nil || d.name != "BOILER-1" || !d.added || d.dev.Addr.IP != stations[3] {
		t.Fatalf("restored %+v, want BOILER-1 at %v, listed by address", d, stations[3])
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case e := <-s2.events:
			if n, ok := e.Data.(explorer.Node); ok && n.ID == devID(4000) {
				if n.Label != "BOILER-1" || fact(n, "Model") != "SIM-LEGACY" {
					t.Errorf("after contact the device is %+v", n)
				}
				return
			}
		case <-deadline:
			t.Fatal("the restored device was not read again")
		}
	}
}
