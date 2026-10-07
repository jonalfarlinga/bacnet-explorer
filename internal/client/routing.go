package client

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"time"

	"github.com/jonalfarlinga/bacnet/plumbing"
)

// routerBusyHold is how long a Router-Busy-To-Network holds requests to the
// network when no Router-Available-To-Network follows.
const routerBusyHold = 30 * time.Second

// RouterInfo is what a router said it reaches, in an I-Am-Router-To-Network.
type RouterInfo struct {
	Router   netip.AddrPort
	Networks []uint16
}

// routerWatcher is one Who-Is-Router-To-Network waiting for answers.
type routerWatcher struct {
	ch chan RouterInfo
}

// Routes returns the router the client has learned for each network number.
func (c *Client) Routes() map[uint16]netip.AddrPort {
	c.mu.Lock()
	defer c.mu.Unlock()
	routes := make(map[uint16]netip.AddrPort, len(c.routes))
	for net, router := range c.routes {
		routes[net] = router
	}
	return routes
}

func (c *Client) route(net uint16) (netip.AddrPort, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	router, ok := c.routes[net]
	return router, ok
}

func (c *Client) learnRoute(net uint16, router netip.AddrPort) {
	c.mu.Lock()
	old, had := c.routes[net]
	c.routes[net] = router
	c.mu.Unlock()
	if !had || old != router {
		c.log.Info("route learned", "network", net, "router", router)
	}
}

// dropRoute forgets the router for a network, if it is still the one given.
func (c *Client) dropRoute(net uint16, router netip.AddrPort) {
	c.mu.Lock()
	dropped := c.routes[net] == router
	if dropped {
		delete(c.routes, net)
	}
	c.mu.Unlock()
	if dropped {
		c.log.Info("route dropped", "network", net, "router", router)
	}
}

// networkMessage handles what routers say.
func (c *Client) networkMessage(src netip.AddrPort, m *plumbing.NetworkMessage) {
	c.noteSource(src, m.NPDU)
	switch m.Type {
	case plumbing.NetMsgIAmRouterToNetwork:
		nets, err := m.Networks()
		if err != nil {
			return
		}
		for _, net := range nets {
			c.learnRoute(net, src)
		}
		info := RouterInfo{Router: src, Networks: nets}
		c.mu.Lock()
		for w := range c.routerWatch {
			select {
			case w.ch <- info:
			default:
			}
		}
		c.mu.Unlock()
	case plumbing.NetMsgRejectMessageToNetwork:
		// The router cannot reach the network a request was for.
		reason, dnet, err := m.RejectReason()
		if err != nil {
			return
		}
		c.dropRoute(dnet, src)
		c.failNet(dnet, fmt.Errorf("%w: the router at %s rejected network %d: %s",
			ErrNetworkUnreachable, src, dnet, routerRejectName(reason)))
	case plumbing.NetMsgRouterBusyToNetwork:
		nets, err := m.Networks()
		if err != nil {
			return
		}
		c.setBusy(nets, time.Now().Add(routerBusyHold))
		c.log.Info("router busy", "router", src, "networks", nets)
	case plumbing.NetMsgRouterAvailableToNetwork:
		nets, err := m.Networks()
		if err != nil {
			return
		}
		c.setAvailable(nets)
		c.log.Info("router available", "router", src, "networks", nets)
	}
}

// setBusy holds new requests to the networks until the time given.
func (c *Client) setBusy(nets []uint16, until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for net, t := range c.busy {
		if !t.After(now) {
			delete(c.busy, net)
		}
	}
	for _, net := range nets {
		c.busy[net] = until
	}
}

// setAvailable lifts the hold on the networks and wakes what waits on it.
func (c *Client) setAvailable(nets []uint16) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, net := range nets {
		delete(c.busy, net)
	}
	close(c.available)
	c.available = make(chan struct{})
}

// waitAvailable returns once no router has said it is busy for the network.
func (c *Client) waitAvailable(ctx context.Context, net uint16) error {
	for {
		c.mu.Lock()
		until := c.busy[net]
		available := c.available
		c.mu.Unlock()
		wait := time.Until(until)
		if wait <= 0 {
			return nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-available:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-c.closed:
			timer.Stop()
			return ErrClosed
		}
		timer.Stop()
	}
}

// WhoIsRouterToNetwork asks which routers are reachable, or with dnet, which
// router serves that network. It collects the answers until ctx ends or the
// client's timeout passes, whichever is first, and remembers the routes they
// give for the requests that follow.
func (c *Client) WhoIsRouterToNetwork(ctx context.Context, dnet *uint16) ([]RouterInfo, error) {
	return c.whoIsRouter(ctx, dnet, false)
}

// whoIsRouter sends one Who-Is-Router-To-Network and collects the answers.
// With first, it returns as soon as a router answers for dnet.
func (c *Client) whoIsRouter(ctx context.Context, dnet *uint16, first bool) ([]RouterInfo, error) {
	w := &routerWatcher{ch: make(chan RouterInfo, 16)}
	c.mu.Lock()
	c.routerWatch[w] = struct{}{}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.routerWatch, w)
		c.mu.Unlock()
	}()

	var data []byte
	if dnet != nil {
		data = plumbing.NetworksData(*dnet)
	}
	to, function := c.broadcastTarget()
	m := plumbing.NewNetworkMessage(
		plumbing.NewBVLC(function),
		plumbing.NewNPDU(false, false, false, false),
		plumbing.NetMsgWhoIsRouterToNetwork,
		data,
	)
	if err := c.send(to, m); err != nil {
		return nil, err
	}

	timer := time.NewTimer(c.cfg.Timeout)
	defer timer.Stop()
	found := map[netip.AddrPort]*RouterInfo{}
	for done := false; !done; {
		select {
		case info := <-w.ch:
			r := found[info.Router]
			if r == nil {
				r = &RouterInfo{Router: info.Router}
				found[info.Router] = r
			}
			r.Networks = mergeNetworks(r.Networks, info.Networks)
			if first && dnet != nil && contains(info.Networks, *dnet) {
				return []RouterInfo{*r}, nil
			}
		case <-timer.C:
			done = true
		case <-ctx.Done():
			if len(found) == 0 {
				return nil, ctx.Err()
			}
			done = true
		case <-c.closed:
			return nil, ErrClosed
		}
	}

	routers := make([]RouterInfo, 0, len(found))
	for _, r := range found {
		routers = append(routers, *r)
	}
	sort.Slice(routers, func(i, j int) bool { return routers[i].Router.Compare(routers[j].Router) < 0 })
	return routers, nil
}

func contains(nets []uint16, net uint16) bool {
	for _, n := range nets {
		if n == net {
			return true
		}
	}
	return false
}

func mergeNetworks(have, add []uint16) []uint16 {
	for _, n := range add {
		if !contains(have, n) {
			have = append(have, n)
		}
	}
	sort.Slice(have, func(i, j int) bool { return have[i] < have[j] })
	return have
}
