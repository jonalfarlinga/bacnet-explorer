package client_test

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/jonalfarlinga/bacnet/internal/client"
	"github.com/jonalfarlinga/bacnet/internal/sim"
	"github.com/jonalfarlinga/bacnet/objects"
)

// discover opens a client on the simulated devices and returns it with the
// devices that answered its Who-Is, by instance.
func discover(t *testing.T) (*client.Client, map[uint32]client.Device) {
	t.Helper()
	network, err := sim.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(network.Close)

	var mu sync.Mutex
	found := map[uint32]client.Device{}
	c, err := client.Open(client.Config{
		Local:     netip.MustParseAddrPort("127.0.0.1:0"),
		Broadcast: network.Addr(),
		Timeout:   time.Second,
		Retries:   1,
		OnIAm: func(d client.Device) {
			mu.Lock()
			found[d.Instance] = d
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })

	if err := c.WhoIs(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(found)
		mu.Unlock()
		if n == 7 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d devices answered the Who-Is, want 7", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	devices := map[uint32]client.Device{}
	for k, v := range found {
		devices[k] = v
	}
	return c, devices
}

func TestWhoIsFindsRoutedDevices(t *testing.T) {
	_, devices := discover(t)

	router, vav := devices[2000], devices[2101]
	if router.Addr.Routed() {
		t.Errorf("the router is at %v, want an address of its own", router.Addr)
	}
	if vav.Addr.Net != 2001 || len(vav.Addr.MAC) != 1 || vav.Addr.MAC[0] != 5 {
		t.Errorf("device 2101 is at %v, want network 2001 MAC 05", vav.Addr)
	}
	if vav.Addr.IP != router.Addr.IP {
		t.Errorf("device 2101 is reached through %v, want the router at %v", vav.Addr.IP, router.Addr.IP)
	}
	if vav.MaxAPDU != 480 || vav.Segmentation != sim.SegNone {
		t.Errorf("device 2101: max APDU %d segmentation %d, want 480 and none", vav.MaxAPDU, vav.Segmentation)
	}
}

func TestReadObjectList(t *testing.T) {
	c, devices := discover(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		instance uint32
		want     int
		byIndex  bool
	}{
		{"in one APDU", 1001, 25, false},
		{"segmented", 2000, 124, false},
		{"element by element from a device that cannot segment", 3000, 241, true},
		{"from a device behind a router", 2101, 9, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := 0
			list, err := c.ReadObjectList(ctx, devices[tc.instance], func(done, total int) { steps++ })
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != tc.want {
				t.Errorf("%d objects, want %d", len(list), tc.want)
			}
			if (steps > 0) != tc.byIndex {
				t.Errorf("%d elements were read by index, want by index: %t", steps, tc.byIndex)
			}
			if len(list) > 0 && list[0] != (client.ObjectID{Type: objects.ObjectTypeDevice, Instance: tc.instance}) {
				t.Errorf("first object is %v, want the Device object", list[0])
			}
		})
	}
}

func TestReadPropertyMultiple(t *testing.T) {
	c, devices := discover(t)
	ai := client.ObjectID{Type: objects.ObjectTypeAnalogInput, Instance: 0}
	bi := client.ObjectID{Type: objects.ObjectTypeBinaryInput, Instance: 1}

	results, err := c.ReadPropertyMultiple(context.Background(), devices[1001].Addr, []client.ReadSpec{
		{Object: ai, Props: []uint16{objects.PropertyIdObjectName, objects.PropertyIdPresentValue, objects.PropertyIdStateText}},
		{Object: bi, Props: []uint16{objects.PropertyIdStatusFlags}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Object != ai || results[1].Object != bi {
		t.Fatalf("results for %+v, want %v and %v", results, ai, bi)
	}

	props := results[0].Props
	if len(props) != 3 {
		t.Fatalf("%d properties of the analog input, want 3", len(props))
	}
	if v := props[0].Values; len(v) != 1 || v[0].Kind != client.KindString || v[0].Str != "AHU1.SAT" {
		t.Errorf("object-name is %+v, want AHU1.SAT", v)
	}
	if v := props[1].Values; len(v) != 1 || v[0].Kind != client.KindReal || v[0].Float < 50 || v[0].Float > 60 {
		t.Errorf("present-value is %+v, want a real number near 55", v)
	}
	var be *client.BACnetError
	if !errors.As(props[2].Err, &be) || be.Code != client.ErrorCodeUnknownProperty {
		t.Errorf("state-text of an analog input gave %v, want unknown property", props[2].Err)
	}

	flags := results[1].Props[0].Values
	if len(flags) != 1 || flags[0].Kind != client.KindBits || len(flags[0].Bits) != 4 || !flags[0].Bits[0] {
		t.Errorf("status-flags is %+v, want 4 bits with in-alarm set", flags)
	}
}

func TestReadAllPropertiesSegmented(t *testing.T) {
	c, devices := discover(t)
	dev := devices[2000]

	// The Device object's properties include its object list, which alone is
	// longer than the device's 480-byte APDU.
	results, err := c.ReadPropertyMultiple(context.Background(), dev.Addr, []client.ReadSpec{
		{Object: client.ObjectID{Type: objects.ObjectTypeDevice, Instance: dev.Instance}, Props: []uint16{objects.PropertyIdAll}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range results[0].Props {
		if p.ID == uint32(objects.PropertyIdObjectList) {
			if len(p.Values) != 124 {
				t.Errorf("object-list has %d elements, want 124", len(p.Values))
			}
			return
		}
	}
	t.Error("no object-list among the properties")
}

func TestDeviceDeclines(t *testing.T) {
	c, devices := discover(t)
	ctx := context.Background()

	// No ReadPropertyMultiple in device 4000.
	_, err := c.ReadPropertyMultiple(ctx, devices[4000].Addr, []client.ReadSpec{
		{Object: client.ObjectID{Type: objects.ObjectTypeDevice, Instance: 4000}, Props: []uint16{objects.PropertyIdObjectName}},
	})
	var re *client.RejectError
	if !errors.As(err, &re) || !client.Declined(err) {
		t.Errorf("ReadPropertyMultiple gave %v, want a reject", err)
	}

	_, err = c.ReadProperty(ctx, devices[4000].Addr, client.ObjectID{Type: objects.ObjectTypeAnalogInput, Instance: 99}, objects.PropertyIdPresentValue)
	var be *client.BACnetError
	if !errors.As(err, &be) || be.Code != client.ErrorCodeUnknownObject {
		t.Errorf("reading an object that is not there gave %v, want unknown object", err)
	}

	// A device that cannot segment aborts a reply that is too long.
	_, err = c.ReadProperty(ctx, devices[3000].Addr, client.ObjectID{Type: objects.ObjectTypeDevice, Instance: 3000}, objects.PropertyIdObjectList)
	var ae *client.AbortError
	if !errors.As(err, &ae) || ae.Reason != objects.AbortReasonSegmentationNotSupported {
		t.Errorf("reading a long object list gave %v, want an abort", err)
	}
}

func TestTimeout(t *testing.T) {
	c, devices := discover(t)
	// Nothing listens on the discard port.
	gone := client.Address{IP: netip.AddrPortFrom(devices[1001].Addr.IP.Addr(), 9)}
	start := time.Now()
	_, err := c.ReadProperty(context.Background(), gone, client.ObjectID{Type: objects.ObjectTypeDevice, Instance: 1}, objects.PropertyIdObjectName)
	if !errors.Is(err, client.ErrTimeout) {
		t.Errorf("reading from nobody gave %v, want a timeout", err)
	}
	// One try and one resend, a second each.
	if d := time.Since(start); d < 1900*time.Millisecond || d > 3*time.Second {
		t.Errorf("gave up after %s, want 2 tries of a second", d)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.ReadProperty(ctx, gone, client.ObjectID{}, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a cancelled read gave %v, want the context's error", err)
	}
}
