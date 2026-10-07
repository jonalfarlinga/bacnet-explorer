package client_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/jonalfarlinga/bacnet/internal/client"
	"github.com/jonalfarlinga/bacnet/internal/sim"
)

// A device that no Who-Is reaches is described by reading it.
func TestProbe(t *testing.T) {
	network, err := sim.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(network.Close)
	// Nothing to broadcast to: the client hears no I-Am.
	c, err := client.Open(client.Config{
		Local:     netip.MustParseAddrPort("127.0.0.1:0"),
		Broadcast: netip.MustParseAddrPort("127.0.0.1:9"),
		Timeout:   time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ctx := context.Background()
	stations := network.Addrs()

	for _, tc := range []struct {
		name     string
		addr     client.Address
		instance uint32
		want     client.Device
	}{
		{"by address alone", client.Address{IP: stations[0]}, 0,
			client.Device{Instance: 1001, Addr: client.Address{IP: stations[0]}, MaxAPDU: 1476, Segmentation: sim.SegBoth, VendorID: 999}},
		{"with the instance given", client.Address{IP: stations[0]}, 1001,
			client.Device{Instance: 1001, Addr: client.Address{IP: stations[0]}, MaxAPDU: 1476, Segmentation: sim.SegBoth, VendorID: 999}},
		{"without ReadPropertyMultiple", client.Address{IP: stations[3]}, 0,
			client.Device{Instance: 4000, Addr: client.Address{IP: stations[3]}, MaxAPDU: 206, Segmentation: sim.SegNone, VendorID: 999}},
		{"behind a router", client.Address{IP: stations[1], Net: 2001, MAC: []byte{5}}, 0,
			client.Device{Instance: 2101, Addr: client.Address{IP: stations[1], Net: 2001, MAC: []byte{5}}, MaxAPDU: 480, Segmentation: sim.SegNone, VendorID: 999}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.Probe(ctx, tc.addr, tc.instance)
			if err != nil {
				t.Fatal(err)
			}
			if got.Instance != tc.want.Instance || got.MaxAPDU != tc.want.MaxAPDU ||
				got.Segmentation != tc.want.Segmentation || got.VendorID != tc.want.VendorID ||
				got.Addr.IP != tc.want.Addr.IP || got.Addr.Net != tc.want.Addr.Net {
				t.Errorf("probed %+v, want %+v", got, tc.want)
			}
		})
	}

	t.Run("nobody there", func(t *testing.T) {
		_, err := c.Probe(ctx, client.Address{IP: netip.MustParseAddrPort("127.0.0.1:9")}, 0)
		if !errors.Is(err, client.ErrTimeout) {
			t.Errorf("probing nobody gave %v, want a timeout", err)
		}
	})
}
