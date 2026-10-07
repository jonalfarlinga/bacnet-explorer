package client

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/jonalfarlinga/bacnet/internal/sim"
	"github.com/jonalfarlinga/bacnet/objects"
	"github.com/jonalfarlinga/bacnet/plumbing"
)

// A Router-Busy-To-Network holds requests to the network until the router
// says it is available again.
func TestRouterBusyHoldsRequests(t *testing.T) {
	network, err := sim.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(network.Close)
	c, err := Open(Config{
		Local:     netip.MustParseAddrPort("127.0.0.1:0"),
		Broadcast: network.Addr(),
		Timeout:   time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })

	dnet := uint16(2001)
	routers, err := c.WhoIsRouterToNetwork(context.Background(), &dnet)
	if err != nil || len(routers) != 1 {
		t.Fatalf("routers for network 2001: %+v, %v", routers, err)
	}
	router := routers[0].Router
	say := func(msgType uint8) {
		c.dispatch(router, plumbing.NewNetworkMessage(
			plumbing.NewBVLC(plumbing.BVLCFuncUnicast), plumbing.NewNPDU(false, false, false, false),
			msgType, plumbing.NetworksData(2001),
		))
	}
	vav := Address{Net: 2001, MAC: []byte{5}}
	obj := ObjectID{Type: objects.ObjectTypeDevice, Instance: 2101}

	say(plumbing.NetMsgRouterBusyToNetwork)
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := c.ReadProperty(context.Background(), vav, obj, objects.PropertyIdObjectName)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the read finished with %v while the router was busy", err)
	case <-time.After(200 * time.Millisecond):
	}
	say(plumbing.NetMsgRouterAvailableToNetwork)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the read failed after the router became available: %v", err)
		}
		if d := time.Since(start); d < 200*time.Millisecond {
			t.Errorf("the read finished after %s, before the router was available", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the read did not finish after the router became available")
	}

	// A hold that outlasts the caller's patience ends with the context's
	// error, not a timeout.
	say(plumbing.NetMsgRouterBusyToNetwork)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.ReadProperty(ctx, vav, obj, objects.PropertyIdObjectName); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a read held by a busy router gave %v, want the context's error", err)
	}
}
