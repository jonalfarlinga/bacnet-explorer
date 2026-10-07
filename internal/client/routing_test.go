package client_test

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

// readName reads a device's object-name through the client.
func readName(t *testing.T, c *client.Client, to client.Address, instance uint32) string {
	t.Helper()
	values, err := c.ReadProperty(context.Background(), to,
		client.ObjectID{Type: objects.ObjectTypeDevice, Instance: instance}, objects.PropertyIdObjectName)
	if err != nil {
		t.Fatalf("reading the name of device %d at %v: %v", instance, to, err)
	}
	if len(values) != 1 || values[0].Kind != client.KindString {
		t.Fatalf("the name of device %d is %+v, want one string", instance, values)
	}
	return values[0].Str
}

func TestWhoIsRouterToNetwork(t *testing.T) {
	c, devices := discover(t)
	ctx := context.Background()

	routers, err := c.WhoIsRouterToNetwork(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(routers) != 1 || routers[0].Router != devices[2000].Addr.IP {
		t.Fatalf("routers %+v, want the plant router at %v alone", routers, devices[2000].Addr.IP)
	}
	if nets := routers[0].Networks; len(nets) != 1 || nets[0] != 2001 {
		t.Errorf("the router reaches %v, want network 2001", nets)
	}

	dnet := uint16(2001)
	routers, err = c.WhoIsRouterToNetwork(ctx, &dnet)
	if err != nil || len(routers) != 1 {
		t.Errorf("asking for network 2001 gave %+v, %v; want the one router", routers, err)
	}

	dnet = 2009
	routers, err = c.WhoIsRouterToNetwork(ctx, &dnet)
	if err != nil || len(routers) != 0 {
		t.Errorf("asking for network 2009 gave %+v, %v; want nobody", routers, err)
	}
}

// The I-Ams of routed devices teach the client which router serves their
// network, so a device can be addressed by network and MAC alone.
func TestRouteLearnedFromIAm(t *testing.T) {
	c, devices := discover(t)

	routes := c.Routes()
	if routes[2001] != devices[2000].Addr.IP {
		t.Fatalf("the route to network 2001 is %v, want the plant router at %v", routes[2001], devices[2000].Addr.IP)
	}
	if name := readName(t, c, client.Address{Net: 2001, MAC: []byte{5}}, 2101); name != "VAV-2-01" {
		t.Errorf("device 2101 is named %q, want VAV-2-01", name)
	}
}

// A client that has heard no I-Am asks which router serves the network.
func TestRouterFoundOnDemand(t *testing.T) {
	network, err := sim.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(network.Close)
	c, err := client.Open(client.Config{
		Local:     netip.MustParseAddrPort("127.0.0.1:0"),
		Broadcast: network.Addr(),
		Timeout:   time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })

	if len(c.Routes()) != 0 {
		t.Fatalf("a new client knows routes %v", c.Routes())
	}
	if name := readName(t, c, client.Address{Net: 2001, MAC: []byte{6}}, 2102); name != "VAV-2-02" {
		t.Errorf("device 2102 is named %q, want VAV-2-02", name)
	}
	if _, ok := c.Routes()[2001]; !ok {
		t.Errorf("the router for network 2001 was not remembered: %v", c.Routes())
	}
}

// A router's Reject-Message-To-Network fails the request at once instead of
// letting it time out, and the route is forgotten.
func TestUnknownNetworkIsRejected(t *testing.T) {
	c, devices := discover(t)
	router := devices[2000].Addr.IP

	start := time.Now()
	_, err := c.ReadProperty(context.Background(), client.Address{IP: router, Net: 2009, MAC: []byte{1}},
		client.ObjectID{Type: objects.ObjectTypeDevice, Instance: 2901}, objects.PropertyIdObjectName)
	if !errors.Is(err, client.ErrNetworkUnreachable) {
		t.Errorf("reading through network 2009 gave %v, want the router's rejection", err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("the rejection took %s to come back", d)
	}
	if _, ok := c.Routes()[2009]; ok {
		t.Errorf("a route to network 2009 is remembered: %v", c.Routes())
	}
}
