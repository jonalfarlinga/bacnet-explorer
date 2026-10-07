// Package bacnet is the explorer's BACnet/IP source: it finds devices with
// a Who-Is or by their address, reads their objects and properties through
// the client, and renders what it reads. Its node IDs are
// "bacnet:dev/<instance>" for a device, "bacnet:dev/<instance>/obj/<type>/
// <instance>" for an object, "bacnet:net/<number>" for a network behind a
// router, and "bacnet:router/<address>" for a router that has not announced
// itself.
package bacnet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jonalfarlinga/bacnet/client"
	"github.com/jonalfarlinga/bacnet/client/sim"
	"github.com/jonalfarlinga/bacnet/internal/explorer"
)

// Kind is the source's kind, and the prefix of its node IDs.
const Kind = "bacnet"

// Options is what the source is built with.
type Options struct {
	Timeout  time.Duration
	Retries  int
	Logger   *slog.Logger
	OnPacket func(client.PacketEvent)
	// Sim, when set, is browsed instead of a network. The source then has
	// no settings.
	Sim *sim.Network
}

// Source is the BACnet/IP source. One is registered per explorer.
type Source struct {
	opts   Options
	events chan explorer.Event

	mu   sync.Mutex
	c    *client.Client
	cfg  client.Config
	name string // what the socket is on: an adapter, or the simulation
	// ctx lives while the client does; the reads run under it.
	ctx  context.Context
	stop context.CancelFunc

	devices map[uint32]*device
	// routers is the routers that devices behind them have named, which
	// have not announced themselves, by address.
	routers map[netip.AddrPort]bool
	// networks is the router each known network is behind.
	networks map[uint16]netip.AddrPort

	// infoSlots bounds how many devices are being identified at once.
	infoSlots chan struct{}
}

// New builds the source.
func New(opts Options) *Source {
	if opts.Timeout <= 0 {
		opts.Timeout = 3 * time.Second
	}
	return &Source{
		opts:      opts,
		events:    make(chan explorer.Event, 4096),
		devices:   map[uint32]*device{},
		routers:   map[netip.AddrPort]bool{},
		networks:  map[uint16]netip.AddrPort{},
		infoSlots: make(chan struct{}, 4),
	}
}

// emit passes an event to the registry. It must not be called with s.mu
// held: the registry may be asking the source for its status meanwhile.
func (s *Source) emit(e explorer.Event) {
	s.mu.Lock()
	ctx := s.ctx
	s.mu.Unlock()
	if ctx == nil {
		return
	}
	select {
	case s.events <- e:
	case <-ctx.Done():
	}
}

// Events implements explorer.Source.
func (s *Source) Events() <-chan explorer.Event { return s.events }

// Describe implements explorer.Source.
func (s *Source) Describe() explorer.Description {
	d := explorer.Description{
		Kind:  Kind,
		Title: "BACnet/IP",
		Blurb: "The explorer broadcasts a Who-Is on the adapter you pick and lists the devices that answer. " +
			"It reads from devices and never writes to them.",
		AddForm: []explorer.Field{
			{Key: "address", Label: "IP address", Hint: "IP, or IP:port when the port is not 47808; for a device behind a router, the router's", Type: "text", Required: true},
			{Key: "instance", Label: "Device instance", Hint: "read from the device if left out", Type: "number"},
			{Key: "network", Label: "Network number", Hint: "for a device behind a router", Type: "number"},
			{Key: "mac", Label: "MAC", Hint: "the device's address on that network, in hex", Type: "text"},
		},
		CanDiscover:  true,
		DiscoverHint: "Broadcast a Who-Is and add the devices that answer",
	}
	if s.opts.Sim != nil {
		return d
	}
	adapter := explorer.Field{Key: "adapter", Label: "Network adapter", Type: "select", Required: true}
	if list, err := client.Interfaces(); err == nil {
		for _, i := range list {
			adapter.Options = append(adapter.Options, explorer.Option{
				Value: i.Name, Label: i.Name, Hint: fmt.Sprintf("%s · Who-Is goes to %s", i.Addr, i.Broadcast),
			})
		}
	}
	d.Settings = []explorer.Field{
		adapter,
		{Key: "port", Label: "UDP port", Type: "number", Default: strconv.Itoa(client.DefaultPort)},
		{Key: "bbmd", Label: "BBMD", Hint: "register as a foreign device with the BBMD at this IP, to reach a network on another subnet", Type: "text"},
		{Key: "bbmd_ttl", Label: "Registration TTL", Hint: "renewed halfway through", Type: "text", Default: "5m"},
	}
	return d
}

// Open implements explorer.Source.
func (s *Source) Open(ctx context.Context, settings explorer.Values) error {
	var (
		name string
		cfg  client.Config
	)
	if s.opts.Sim != nil {
		name = "Simulated devices"
		cfg = client.Config{Local: netip.MustParseAddrPort("127.0.0.1:0"), Broadcast: s.opts.Sim.Addr()}
	} else {
		adapter := strings.TrimSpace(settings["adapter"])
		if adapter == "" {
			return errors.New("no network adapter was chosen")
		}
		ifc, err := client.FindInterface(adapter)
		if err != nil {
			return err
		}
		port := uint64(client.DefaultPort)
		if p := strings.TrimSpace(settings["port"]); p != "" {
			if port, err = strconv.ParseUint(p, 10, 16); err != nil || port == 0 {
				return fmt.Errorf("port %q is not a UDP port", p)
			}
		}
		name = ifc.Name
		cfg = ifc.Config(uint16(port))
		if bbmd := strings.TrimSpace(settings["bbmd"]); bbmd != "" {
			addr, err := parseAddrPort(bbmd, uint16(port))
			if err != nil {
				return fmt.Errorf("BBMD: %w", err)
			}
			fd := &client.ForeignDeviceConfig{BBMD: addr, TTL: client.DefaultTTL}
			if ttl := strings.TrimSpace(settings["bbmd_ttl"]); ttl != "" {
				if fd.TTL, err = time.ParseDuration(ttl); err != nil {
					return fmt.Errorf("registration TTL: %w", err)
				}
			}
			cfg.ForeignDevice = fd
		}
	}
	cfg.Timeout, cfg.Retries = s.opts.Timeout, s.opts.Retries
	cfg.Logger, cfg.OnPacket = s.opts.Logger, s.opts.OnPacket
	return s.use(name, cfg)
}

// use opens a client with cfg in place of the current one, forgets every
// device, and sends a Who-Is.
func (s *Source) use(name string, cfg client.Config) error {
	s.Close()

	cfg.OnIAm = s.onIAm
	c, err := client.Open(cfg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.c, s.cfg, s.name = c, cfg, name
	s.ctx, s.stop = context.WithCancel(context.Background())
	s.mu.Unlock()
	s.emit(explorer.Event{Type: "reset"})
	s.emit(explorer.Event{Type: "source"})
	return c.WhoIs()
}

// Close implements explorer.Source.
func (s *Source) Close() error {
	s.mu.Lock()
	c := s.c
	if s.stop != nil {
		s.stop()
	}
	s.c, s.ctx, s.stop = nil, nil, nil
	s.devices = map[uint32]*device{}
	s.routers = map[netip.AddrPort]bool{}
	s.networks = map[uint16]netip.AddrPort{}
	s.mu.Unlock()
	if c != nil {
		return c.Close()
	}
	return nil
}

// Status implements explorer.Source.
func (s *Source) Status() explorer.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c == nil {
		return explorer.Status{}
	}
	st := explorer.Status{Open: true}
	local := s.c.Local().String()
	switch {
	case s.opts.Sim != nil:
		st.Summary = "Simulated devices"
		st.Lines = []explorer.Fact{{Label: "Local", Value: local, Mono: true}}
	case s.cfg.ForeignDevice != nil:
		fd, _ := s.c.ForeignDeviceStatus()
		st.Summary = fmt.Sprintf("BACnet/IP on %s, foreign device of %s", s.name, fd.BBMD)
		state := fd.State.String()
		if fd.LastResult != nil {
			state += ": " + fd.LastResult.Error()
		}
		st.Lines = []explorer.Fact{
			{Label: "Adapter", Value: s.name},
			{Label: "Local", Value: local, Mono: true},
			{Label: "BBMD", Value: fd.BBMD.String(), Mono: true},
			{Label: "Registration", Value: state},
		}
	default:
		st.Summary = fmt.Sprintf("BACnet/IP on %s", s.name)
		st.Lines = []explorer.Fact{
			{Label: "Adapter", Value: s.name},
			{Label: "Local", Value: local, Mono: true},
			{Label: "Broadcast", Value: s.cfg.Broadcast.String(), Mono: true},
		}
	}
	if routes := s.c.Routes(); len(routes) > 0 {
		st.Lines = append(st.Lines, explorer.Fact{Label: "Routes", Value: fmt.Sprintf("%d networks", len(routes))})
	}
	return st
}

// Discover implements explorer.Source: it sends a Who-Is. Devices that
// answer are added as their I-Ams arrive.
func (s *Source) Discover(ctx context.Context) error {
	s.mu.Lock()
	c := s.c
	s.mu.Unlock()
	if c == nil {
		return explorer.ErrNotOpen
	}
	return c.WhoIs()
}

// Add implements explorer.Source: it lists the device at an address that no
// Who-Is reaches, by reading what its I-Am would have said.
func (s *Source) Add(ctx context.Context, form explorer.Values) (explorer.Node, error) {
	s.mu.Lock()
	c := s.c
	s.mu.Unlock()
	if c == nil {
		return explorer.Node{}, explorer.ErrNotOpen
	}
	var network uint64
	if v := strings.TrimSpace(form["network"]); v != "" {
		var err error
		if network, err = strconv.ParseUint(v, 10, 16); err != nil {
			return explorer.Node{}, fmt.Errorf("network number %q is not a number", v)
		}
	}
	addr, err := ParseAddress(form["address"], uint16(network), form["mac"])
	if err != nil {
		return explorer.Node{}, err
	}
	var instance uint64
	if v := strings.TrimSpace(form["instance"]); v != "" {
		if instance, err = strconv.ParseUint(v, 10, 32); err != nil || instance > client.WildcardInstance {
			return explorer.Node{}, fmt.Errorf("device instance %q is not one", v)
		}
	}
	dev, err := c.Probe(ctx, addr, uint32(instance))
	if err != nil {
		return explorer.Node{}, fmt.Errorf("device at %v: %w", addr, err)
	}
	return s.record(dev, true), nil
}

// parseAddrPort reads an IP with an optional port.
func parseAddrPort(s string, defaultPort uint16) (netip.AddrPort, error) {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap, nil
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("%q is not an IP address or IP:port", s)
	}
	return netip.AddrPortFrom(addr, defaultPort), nil
}
