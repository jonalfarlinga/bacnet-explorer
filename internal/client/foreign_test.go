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
	"github.com/jonalfarlinga/bacnet/plumbing"
)

// foreignClient opens a client registered with the BBMD of a remote site.
func foreignClient(t *testing.T, refuse bool) (*client.Client, netip.AddrPort, func() map[uint32]client.Device) {
	t.Helper()
	network, err := sim.StartDevices(sim.RemoteSite(refuse))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(network.Close)

	var mu sync.Mutex
	found := map[uint32]client.Device{}
	c, err := client.Open(client.Config{
		Local:         netip.MustParseAddrPort("127.0.0.1:0"),
		Timeout:       time.Second,
		Retries:       1,
		ForeignDevice: &client.ForeignDeviceConfig{BBMD: network.Addr(), TTL: 2 * time.Second},
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
	snapshot := func() map[uint32]client.Device {
		mu.Lock()
		defer mu.Unlock()
		devices := map[uint32]client.Device{}
		for k, v := range found {
			devices[k] = v
		}
		return devices
	}
	return c, network.Addr(), snapshot
}

func TestForeignDevice(t *testing.T) {
	c, bbmd, found := foreignClient(t, false)
	ctx := context.Background()

	waitFor(t, "the registration", func() bool {
		st, _ := c.ForeignDeviceStatus()
		return st.State == client.FDRegistered
	})
	st, ok := c.ForeignDeviceStatus()
	if !ok || st.BBMD != bbmd || st.LastResult != nil || st.Registrations != 1 {
		t.Errorf("status %+v, want one registration with %v", st, bbmd)
	}

	// The Who-Is goes to the BBMD, which broadcasts it on its subnet, and
	// every I-Am comes back forwarded by it.
	if err := c.WhoIs(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "5 devices", func() bool { return len(found()) == 5 })
	devices := found()

	// Each device is at its own address, not the BBMD's, except the BBMD
	// itself.
	if ahu := devices[1001]; ahu.Addr.IP == bbmd || ahu.Addr.Routed() {
		t.Errorf("the air handler is at %v, want its own address", ahu.Addr)
	}
	if d := devices[5000]; d.Addr.IP != bbmd {
		t.Errorf("the BBMD's device is at %v, want %v", d.Addr, bbmd)
	}
	if vav := devices[2101]; vav.Addr.Net != 2001 || vav.Addr.IP != devices[2000].Addr.IP {
		t.Errorf("device 2101 is at %v, want network 2001 through the router at %v", vav.Addr, devices[2000].Addr.IP)
	}
	if routes := c.Routes(); routes[2001] != devices[2000].Addr.IP {
		t.Errorf("the route to network 2001 is %v, want the router at %v", routes[2001], devices[2000].Addr.IP)
	}

	// Reads go to the devices by unicast.
	if name := readName(t, c, devices[1001].Addr, 1001); name != sim.RemoteSite(false)[1][0].Name {
		t.Errorf("device 1001 is named %q", name)
	}
	if name := readName(t, c, devices[2101].Addr, 2101); name != "VAV-2-01" {
		t.Errorf("device 2101 is named %q, want VAV-2-01", name)
	}

	// The BBMD's tables.
	fdt, err := c.ReadFDT(ctx, bbmd)
	if err != nil {
		t.Fatal(err)
	}
	if len(fdt) != 1 || fdt[0].IP != c.Local().Addr().As4() || fdt[0].Port != c.Local().Port() || fdt[0].TTL != 2 {
		t.Errorf("the FDT is %+v, want this client at %v for 2 seconds", fdt, c.Local())
	}
	bdt, err := c.ReadBDT(ctx, bbmd)
	if err != nil {
		t.Fatal(err)
	}
	if len(bdt) != 1 || bdt[0].AddrPort() != bbmd {
		t.Errorf("the BDT is %+v, want the BBMD itself", bdt)
	}

	// The registration is renewed halfway through its TTL.
	waitFor(t, "a second registration", func() bool {
		st, _ := c.ForeignDeviceStatus()
		return st.Registrations >= 2
	})
}

// A BBMD that refuses the registration leaves the client failed, trying
// again after a backoff, and its broadcasts reach nobody.
func TestForeignDeviceRefused(t *testing.T) {
	c, bbmd, found := foreignClient(t, true)

	waitFor(t, "the refusal", func() bool {
		st, _ := c.ForeignDeviceStatus()
		return st.State == client.FDFailed
	})
	st, _ := c.ForeignDeviceStatus()
	var re *client.BVLCResultError
	if !errors.As(st.LastResult, &re) || re.Code != plumbing.BVLCResultRegisterForeignDeviceNAK {
		t.Errorf("the registration failed with %v, want the BBMD's NAK", st.LastResult)
	}
	if wait := time.Until(st.NextAttempt); wait < 3*time.Second || wait > 6*time.Second {
		t.Errorf("the next attempt is in %s, want about 5 seconds", wait)
	}

	if err := c.WhoIs(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if devices := found(); len(devices) != 0 {
		t.Errorf("devices %v answered a Who-Is from an unregistered client", devices)
	}

	// The table reads need no registration.
	fdt, err := c.ReadFDT(context.Background(), bbmd)
	if err != nil || len(fdt) != 0 {
		t.Errorf("the FDT is %+v, %v; want empty", fdt, err)
	}
}
