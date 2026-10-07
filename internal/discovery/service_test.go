package discovery

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/jonalfarlinga/bacnet/internal/client"
	"github.com/jonalfarlinga/bacnet/internal/sim"
	"github.com/jonalfarlinga/bacnet/objects"
)

// start runs a service on the simulated devices and waits until all 7 have
// answered and been identified.
func start(t *testing.T) (*Service, <-chan Event) {
	t.Helper()
	network, err := sim.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(network.Close)

	s := New()
	t.Cleanup(s.Close)
	events, cancel := s.Subscribe()
	t.Cleanup(cancel)
	err = s.Use("test", client.Config{
		Local:     netip.MustParseAddrPort("127.0.0.1:0"),
		Broadcast: network.Addr(),
		Timeout:   time.Second,
	}, true)
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "7 named devices", func() bool {
		named := 0
		for _, d := range s.Devices() {
			if d.Name != "" {
				named++
			}
		}
		return named == 7
	})
	return s, events
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

func deviceOf(s *Service, id uint32) Device {
	for _, d := range s.Devices() {
		if d.ID == id {
			return d
		}
	}
	return Device{}
}

func TestDevicesAreIdentified(t *testing.T) {
	s, _ := start(t)

	if d := deviceOf(s, 4000); d.Name != "BOILER-1" || d.ModelName != "SIM-LEGACY" {
		t.Errorf("the device without ReadPropertyMultiple is %+v, want its name and model read one by one", d)
	}
	d := deviceOf(s, 2102)
	if d.Network != 2001 || d.MAC != "6" || d.Address != deviceOf(s, 2000).Address {
		t.Errorf("device 2102 is %+v, want network 2001 MAC 6 behind device 2000", d)
	}
	if d.Objects.State != "idle" {
		t.Errorf("objects of a device nobody opened are %q, want idle", d.Objects.State)
	}
}

func TestObjects(t *testing.T) {
	s, events := start(t)

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
			if _, _, err := s.Objects(tc.id, false); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "the objects", func() bool {
				state := deviceOf(s, tc.id).Objects.State
				return state == "ready" || state == "error"
			})
			if st := deviceOf(s, tc.id).Objects; st.State != "ready" || st.Total != tc.total || st.Done != tc.total {
				t.Fatalf("objects are %+v, want all %d ready", st, tc.total)
			}
			_, objs, _ := s.Objects(tc.id, false)
			for _, o := range objs {
				if !o.Loaded || o.Name == "" {
					t.Errorf("%s:%d was left as %+v", o.TypeName, o.Instance, o)
				}
				if o.Type == objects.ObjectTypeAnalogInput && (o.Value == "" || o.Units == "") {
					t.Errorf("analog input %q has value %q and units %q", o.Name, o.Value, o.Units)
				}
			}
		})
	}

	// The events for a device's objects count up by one from each full list,
	// which is what lets a subscriber notice one it missed.
	last := map[uint32]uint64{}
	for len(events) > 0 {
		e := <-events
		if oe, ok := e.Data.(ObjectsEvent); ok {
			if rev, seen := last[oe.DeviceID]; seen && oe.Rev != rev+1 {
				t.Errorf("device %d: revision %d follows %d", oe.DeviceID, oe.Rev, rev)
			}
			last[oe.DeviceID] = oe.Rev
		}
	}
	if len(last) != 4 {
		t.Errorf("object events for %d devices, want 4", len(last))
	}
}

func TestObjectsShowAlarmAndBinaryState(t *testing.T) {
	s, _ := start(t)
	s.Objects(1001, false)
	waitFor(t, "the objects", func() bool { return deviceOf(s, 1001).Objects.State == "ready" })

	_, objs, _ := s.Objects(1001, false)
	for _, o := range objs {
		if o.Name == "AHU1.FLT.DP" {
			if o.Value != "active" || len(o.Flags) != 1 || o.Flags[0] != "in alarm" {
				t.Errorf("the filter switch is %+v, want active and in alarm", o)
			}
			return
		}
	}
	t.Error("no filter switch among the objects")
}

func TestProperties(t *testing.T) {
	s, _ := start(t)
	ctx := context.Background()
	value := func(props []Property, name string) string {
		for _, p := range props {
			if p.Name == name {
				return p.Value
			}
		}
		return "(missing)"
	}

	// With "all", from a device that segments the answer.
	props, err := s.Properties(ctx, 2000, client.ObjectID{Type: objects.ObjectTypeDevice, Instance: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if got := value(props, "Segmentation supported"); got != "both" {
		t.Errorf("segmentation-supported reads %q, want both", got)
	}
	if got := value(props, "Protocol services supported"); got != "readProperty, readPropertyMultiple, i-Am, who-Is" {
		t.Errorf("protocol-services-supported reads %q", got)
	}
	for _, p := range props {
		if p.Name == "Object list" && len(p.Items) != 124 {
			t.Errorf("object-list has %d items, want 124", len(p.Items))
		}
	}

	// One property at a time, from a device with no ReadPropertyMultiple; the
	// ones the object does not have are left out.
	props, err = s.Properties(ctx, 4000, client.ObjectID{Type: objects.ObjectTypeBinaryInput, Instance: 0})
	if err != nil {
		t.Fatal(err)
	}
	if got := value(props, "Present value"); got != "active" {
		t.Errorf("present-value of a binary input reads %q, want active", got)
	}
	if got := value(props, "Active text"); got != "Firing" {
		t.Errorf("active-text reads %q, want Firing", got)
	}
	if got := value(props, "Units"); got != "(missing)" {
		t.Errorf("a binary input has units %q", got)
	}

	if _, err := s.Properties(ctx, 12345, client.ObjectID{}); err != ErrUnknownDevice {
		t.Errorf("a device that is not there gave %v", err)
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
}

// A device that no Who-Is reaches is added by its address and identified
// like any other.
func TestAddByAddress(t *testing.T) {
	network, err := sim.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(network.Close)

	s := New()
	t.Cleanup(s.Close)
	// The Who-Is goes nowhere: no device announces itself.
	err = s.Use("test", client.Config{
		Local:     netip.MustParseAddrPort("127.0.0.1:0"),
		Broadcast: netip.MustParseAddrPort("127.0.0.1:9"),
		Timeout:   time.Second,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(s.Devices()); n != 0 {
		t.Fatalf("%d devices answered a Who-Is to nobody", n)
	}

	stations := network.Addrs()
	added, err := s.Add(context.Background(), client.Address{IP: stations[3]}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if added.ID != 4000 || !added.Added || added.Address != stations[3].String() || added.MaxAPDU != 206 {
		t.Errorf("added %+v, want device 4000 at %v with a 206-byte APDU", added, stations[3])
	}
	waitFor(t, "the added device to be named", func() bool { return deviceOf(s, 4000).Name != "" })
	if d := deviceOf(s, 4000); d.Name != "BOILER-1" || !d.Added {
		t.Errorf("the added device is %+v, want BOILER-1, marked as added", d)
	}

	if _, err := s.Add(context.Background(), client.Address{IP: netip.MustParseAddrPort("127.0.0.1:9")}, 0); !errors.Is(err, client.ErrTimeout) {
		t.Errorf("adding nobody gave %v, want a timeout", err)
	}
}
