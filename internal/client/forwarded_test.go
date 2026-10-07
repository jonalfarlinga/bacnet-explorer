package client_test

import (
	"encoding/binary"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/jonalfarlinga/bacnet/internal/client"
	"github.com/jonalfarlinga/bacnet/plumbing"
	"github.com/jonalfarlinga/bacnet/services"
)

// An I-Am that a BBMD forwards from a device on another IP subnet is taken,
// and attributed to the device, even though the client only accepts the
// local adapter's traffic. A datagram from another adapter's subnet is not.
func TestForwardedIAmFromAnotherSubnet(t *testing.T) {
	var (
		mu    sync.Mutex
		heard []client.Device
		seen  []client.PacketEvent
	)
	c, err := client.Open(client.Config{
		Local:   netip.MustParseAddrPort("127.0.0.1:0"),
		Exclude: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		OnIAm: func(d client.Device) {
			mu.Lock()
			heard = append(heard, d)
			mu.Unlock()
		},
		OnPacket: func(e client.PacketEvent) {
			mu.Lock()
			seen = append(seen, e)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })

	// The BBMD, on the local subnet, forwards the I-Am of a device at
	// 10.0.124.3 that heard the Who-Is on its own subnet.
	bbmd, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer bbmd.Close()
	origin := netip.MustParseAddrPort("10.0.124.3:47808")
	bvlc := plumbing.NewBVLC(plumbing.BVLCFuncForwardedNPDU)
	ip := origin.Addr().As4()
	copy(bvlc.Origin[:4], ip[:])
	binary.BigEndian.PutUint16(bvlc.Origin[4:], origin.Port())
	npdu := plumbing.NewNPDU(false, false, false, false)
	iAm := plumbing.NewAPDU(plumbing.UnConfirmedReq, services.ServiceUnconfirmedIAm,
		services.IAmObjects(125, 1476, 0, 260))
	b := make([]byte, bvlc.MarshalLen()+npdu.MarshalLen()+iAm.MarshalLen())
	bvlc.Length = uint16(len(b))
	bvlc.MarshalTo(b)
	npdu.MarshalTo(b[bvlc.MarshalLen():])
	if err := iAm.MarshalTo(b[bvlc.MarshalLen()+npdu.MarshalLen():]); err != nil {
		t.Fatal(err)
	}
	if _, err := bbmd.WriteToUDPAddrPort(b, c.Local()); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(heard)
		mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(heard) != 1 || heard[0].Instance != 125 || heard[0].Addr.IP != origin {
		t.Fatalf("heard %+v, want device 125 at %v", heard, origin)
	}
	if len(seen) != 1 || !seen[0].Inbound || seen[0].Peer.Addr() != netip.MustParseAddr("127.0.0.1") || string(seen[0].Data) != string(b) {
		t.Errorf("the packet tap saw %+v, want the one datagram from the BBMD", seen)
	}
}
