// Package survey takes stock of IP addresses before a BACnet device is
// added at one: which have a BACnet device, which have some other host,
// and which are vacant.
//
// It tries every address as the explorer's daily sweep does, with one
// ReadProperty of the Device object's identifier through the wildcard
// instance, after a broadcast Who-Is. A device that answers, or that
// declines as one older than the wildcard instance does, or whose I-Am is
// heard, is a BACnet device, and is read for its name, vendor and model.
//
// Whether anything is at the other addresses is told two ways. On the
// adapter's own subnet the operating system's neighbor table says: sending
// to an address there makes the system resolve it by ARP, which a host
// answers whether or not it answers anything else, so an address with a MAC
// in the table is a host's, and one without is vacant. Beyond the subnet,
// reached through a router, each address is pinged as well: a reply is a
// host, and a router's answer that the host is unreachable means it found
// nobody there by ARP, so the address is vacant. An address that answers
// nothing at all is only silent: vacant, or a host that ignores both.
package survey

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jonalfarlinga/bacnet/client"
	"github.com/jonalfarlinga/bacnet/objects"
)

// DefaultPerSec is how many addresses a survey tries in a second.
const DefaultPerSec = 10

// MaxAddresses is the most a survey takes on: four subnets of 65536, which
// is seven hours at the default pace.
const MaxAddresses = 4 << 16

// How often a running survey reports how far it is, and how often it reads
// the neighbor table while it runs, so that a long survey's early entries
// are kept before the system drops them.
const (
	progressEvery  = 2 * time.Second
	neighborsEvery = 5 * time.Second
	// identifyAtOnce is how many devices are read at once once the
	// addresses are all tried.
	identifyAtOnce = 8
)

// Options is what a survey runs with.
type Options struct {
	// Interface is the adapter the survey goes out through, and whose
	// subnet it covers unless Ranges says otherwise.
	Interface client.Interface
	// Port is the devices' BACnet/IP port; 0 is 47808.
	Port uint16
	// Ranges is the addresses to survey in place of the adapter's subnet:
	// parts of it, or addresses beyond it that the adapter reaches through
	// a router.
	Ranges []Range
	// PerSec is how many addresses are tried in a second; 0 is DefaultPerSec.
	PerSec float64
	// Timeout is how long each address is waited on; 0 is 3 seconds.
	Timeout time.Duration
	Logger  *slog.Logger
	// Progress, when set, is told every two seconds how far the survey is.
	Progress func(Progress)
}

// Progress is how far a running survey is.
type Progress struct {
	Tried, Total int
	// At is the address tried most recently.
	At netip.Addr
	// Devices and Hosts are what has been found so far.
	Devices, Hosts int
	// Identifying is set once every address is tried, while the devices
	// found are read for their names.
	Identifying bool
}

// Range is a run of addresses, both ends included.
type Range struct{ From, To netip.Addr }

func (r Range) String() string {
	if r.From == r.To {
		return r.From.String()
	}
	return r.From.String() + "-" + r.To.String()
}

// ParseRanges reads ranges as a user writes them: "192.168.1.20-192.168.1.60",
// or one address, or several with commas.
func ParseRanges(s string) ([]Range, error) {
	var ranges []Range
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		first, last, isRange := strings.Cut(part, "-")
		from, err := netip.ParseAddr(strings.TrimSpace(first))
		if err != nil || !from.Is4() {
			return nil, fmt.Errorf("%q is not an IPv4 address", strings.TrimSpace(first))
		}
		to := from
		if isRange {
			if to, err = netip.ParseAddr(strings.TrimSpace(last)); err != nil || !to.Is4() {
				return nil, fmt.Errorf("%q is not an IPv4 address", strings.TrimSpace(last))
			}
		}
		if to.Less(from) {
			return nil, fmt.Errorf("the range %s is backwards", part)
		}
		ranges = append(ranges, Range{from, to})
	}
	return ranges, nil
}

// Runs joins sorted addresses into the ranges of consecutive ones.
func Runs(addrs []netip.Addr) []Range {
	var runs []Range
	for _, a := range addrs {
		if n := len(runs); n > 0 && runs[n-1].To.Next() == a {
			runs[n-1].To = a
			continue
		}
		runs = append(runs, Range{a, a})
	}
	return runs
}

// Device is a BACnet device found at an address.
type Device struct {
	Addr netip.Addr
	// MAC is from the neighbor table, for a device on the adapter's
	// subnet; empty beyond it, or when the table had none.
	MAC string
	// Instance is the device's instance number, once it has said which
	// device it is; Unnamed is set when it answered as a BACnet device but
	// would not say, not even asked alone with a Who-Is.
	Instance uint32
	Unnamed  bool
	// Name, Vendor and Model are what it answered to the reads for them.
	Name, Vendor, Model string
	// Routed is, for a router, the devices heard behind it, counted by
	// network number.
	Routed map[uint16]int
}

// Host is an address with a host at it that is not a BACnet device.
type Host struct {
	Addr netip.Addr
	// MAC is from the neighbor table, for a host on the adapter's subnet.
	MAC string
	// Ping is set for a host beyond the subnet, which answered a ping.
	Ping bool
}

// Result is what a survey found.
type Result struct {
	// Subnet is the adapter's: what is surveyed when no ranges are given,
	// or the /16 around the adapter's address when its subnet is larger,
	// and where the neighbor table tells vacant from occupied.
	Subnet netip.Prefix
	// Local is this machine's address on it, which is not tried.
	Local netip.Addr
	// Port is the devices' port, and From the address and port the survey
	// sent from: a port of its own when Port on the adapter was in use,
	// in which case I-Ams broadcast to Port are not heard.
	Port uint16
	From netip.AddrPort
	// Ranges is what was asked for; none is the subnet.
	Ranges []Range
	// Total is the addresses to try, Tried how many were, fewer when the
	// survey was stopped, and Beyond how many of those tried are beyond the
	// adapter's subnet.
	Total, Tried, Beyond int
	Took                 time.Duration
	Stopped              bool

	Devices []Device
	Hosts   []Host
	// Vacant is the addresses found to have nobody at them: on the subnet,
	// no neighbor answered for them; beyond it, a router answered the ping
	// that the host is unreachable, having found nobody there by ARP.
	Vacant []netip.Addr
	// Unrouted is addresses beyond the subnet that a router answered the
	// ping that there is no route to: no subnet there.
	Unrouted []netip.Addr
	// Routers counts the addresses each router answered for, as unreachable
	// or unrouted; an invalid key is a router that could not be told. By is
	// the router that answered for each such address.
	Routers map[netip.Addr]int
	By      map[netip.Addr]netip.Addr
	// Silent is addresses that nothing answered for. Beyond the subnet that
	// is any address whose router keeps quiet about who is not there:
	// vacant, or a host that answers neither BACnet nor a ping. On the
	// subnet it happens only when the neighbor table could not be read, and
	// NeighborsErr says why.
	Silent       []netip.Addr
	NeighborsErr error
	// PingErr is set when pings could not be sent at all; beyond the
	// subnet, only BACnet devices are then told from silence.
	PingErr error
}

// knock is what one address answered.
type knock struct {
	// bacnet is set when a BACnet device answered: with its instance, in
	// which case named is set too, or declining to say which it is.
	bacnet, named bool
	instance      uint32
	// unnamed is set when a device that declined did not announce itself
	// either, asked alone.
	unnamed             bool
	name, vendor, model string
}

// iam is what the I-Ams from an address said: the instance of the device
// at it, when it announced itself, and the devices behind it, by network.
type iam struct {
	known    bool
	instance uint32
	routed   map[uint16]int
}

// echo is what a ping to an address got back: a reply from the address,
// or a router's word, from, that the host is not there, or that there is
// no network there. None of them is silence.
type echo struct {
	alive         bool
	noHost, noNet bool
	from          netip.Addr
}

// survey is one run.
type survey struct {
	c       *client.Client
	subnet  netip.Prefix
	port    uint16
	timeout time.Duration
	perSec  float64
	// lookup reads the neighbor table, and ping pings one address.
	lookup   func() (map[netip.Addr]string, error)
	ping     func(context.Context, netip.Addr, time.Duration) (echo, error)
	progress func(Progress)

	mu           sync.Mutex
	total, tried int
	at           netip.Addr
	identifying  bool
	knocked      map[netip.Addr]knock
	iams         map[netip.Addr]iam
	macs         map[netip.Addr]string
	echoes       map[netip.Addr]echo
	neighborsErr error
	pingErr      error
}

func newSurvey(subnet netip.Prefix, port uint16, timeout time.Duration, perSec float64) *survey {
	return &survey{
		subnet: subnet, port: port, timeout: timeout, perSec: perSec, lookup: neighbors, ping: ping,
		knocked: map[netip.Addr]knock{}, iams: map[netip.Addr]iam{}, macs: map[netip.Addr]string{}, echoes: map[netip.Addr]echo{},
	}
}

// inUse reports whether err is a bind to an address that is taken. The error
// is a different one on each operating system, so it goes by the text.
func inUse(err error) bool {
	text := err.Error()
	return strings.Contains(text, "address already in use") || strings.Contains(text, "Only one usage of each socket address")
}

// Run surveys the adapter's subnet, or the ranges given. A survey stopped
// through ctx returns what it found by then, with Stopped set, and ctx's
// error.
func Run(ctx context.Context, opts Options) (*Result, error) {
	ifc := opts.Interface
	if !ifc.Addr.IsValid() || !ifc.Addr.Addr().Is4() {
		return nil, errors.New("the survey needs an adapter with an IPv4 address")
	}
	port := opts.Port
	if port == 0 {
		port = client.DefaultPort
	}
	perSec := opts.PerSec
	if perSec <= 0 {
		perSec = DefaultPerSec
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	subnet := ifc.Addr.Masked()
	if subnet.Bits() < 16 {
		subnet = netip.PrefixFrom(ifc.Addr.Addr(), 16).Masked()
	}
	addrs, err := addresses(subnet, ifc.Addr.Addr(), opts.Ranges)
	if err != nil {
		return nil, err
	}

	s := newSurvey(subnet, port, timeout, perSec)
	s.progress = opts.Progress
	open := func(p uint16) (*client.Client, error) {
		cfg := ifc.Config(p)
		cfg.Timeout, cfg.Retries, cfg.ReadOnly = timeout, 0, true
		// The survey paces itself; the client's own cap would hold the
		// pace asked for down.
		cfg.MaxRequestsPerSec = -1
		cfg.Logger, cfg.OnIAm = opts.Logger, s.hear
		return client.Open(cfg)
	}
	c, err := open(port)
	if err != nil && inUse(err) {
		c, err = open(0)
	}
	if err != nil {
		return nil, err
	}
	defer c.Close()
	s.c = c
	if err := c.WhoIs(); err != nil && opts.Logger != nil {
		opts.Logger.Warn("the Who-Is was not sent", "err", err)
	}

	started := time.Now()
	s.try(ctx, addrs)
	s.identify(ctx)
	res := s.result(addrs)
	res.Subnet, res.Local, res.Port, res.From, res.Ranges = subnet, ifc.Addr.Addr(), port, c.Local(), opts.Ranges
	res.Took = time.Since(started)
	res.Stopped = ctx.Err() != nil
	return res, ctx.Err()
}

// last is the last address of a prefix: its broadcast address.
func last(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	for i := p.Bits(); i < 32; i++ {
		b[i/8] |= 0x80 >> (i % 8)
	}
	return netip.AddrFrom4(b)
}

// addresses is what the survey tries, in order: every host address of the
// subnet, or of the ranges, but this machine's own. On the subnet, its own
// address and its broadcast address are no host's. Beyond it the subnets
// are not known, so the addresses that end in 0 and 255 are left out,
// which are those of a subnet of 256.
func addresses(subnet netip.Prefix, local netip.Addr, ranges []Range) ([]netip.Addr, error) {
	first, bcast := subnet.Addr(), last(subnet)
	if len(ranges) == 0 {
		ranges = []Range{{first, bcast}}
	}
	var addrs []netip.Addr
	for _, r := range ranges {
		for a := r.From; a.Compare(r.To) <= 0; a = a.Next() {
			if a == local {
				continue
			}
			if subnet.Contains(a) {
				if a == first || a == bcast {
					continue
				}
			} else if b := a.As4()[3]; b == 0 || b == 255 {
				continue
			}
			addrs = append(addrs, a)
			if len(addrs) > MaxAddresses {
				return nil, fmt.Errorf("the ranges hold more than %d addresses, the most a survey takes on", MaxAddresses)
			}
		}
	}
	slices.SortFunc(addrs, netip.Addr.Compare)
	return slices.Compact(addrs), nil
}

// hear takes an I-Am: the device at an address, or one behind a router
// there.
func (s *survey) hear(dev client.Device) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ip := dev.Addr.IP.Addr()
	if !ip.IsValid() {
		return
	}
	entry := s.iams[ip]
	if dev.Addr.Routed() {
		if entry.routed == nil {
			entry.routed = map[uint16]int{}
		}
		entry.routed[dev.Addr.Net]++
	} else {
		entry.known, entry.instance = true, dev.Instance
	}
	s.iams[ip] = entry
}

// try sends to every address at the survey's pace, pinging those beyond
// the subnet as well, reads the neighbor table while it does and once more
// at the end, and reports progress.
func (s *survey) try(ctx context.Context, addrs []netip.Addr) {
	s.mu.Lock()
	s.total = len(addrs)
	s.mu.Unlock()
	s.report()

	done := make(chan struct{})
	var background sync.WaitGroup
	background.Add(1)
	go func() {
		defer background.Done()
		progress := time.NewTicker(progressEvery)
		defer progress.Stop()
		neighbors := time.NewTicker(neighborsEvery)
		defer neighbors.Stop()
		for {
			select {
			case <-done:
				return
			case <-progress.C:
				s.report()
			case <-neighbors.C:
				s.sample()
			}
		}
	}()

	// An address that does not answer holds its place for the timeout, so
	// this many are waited on at once at the pace asked for.
	pace := time.NewTicker(time.Duration(float64(time.Second) / s.perSec))
	defer pace.Stop()
	inFlight := make(chan struct{}, max(4, int(math.Ceil(s.perSec*s.timeout.Seconds()))+1))
	var tries sync.WaitGroup
	for _, a := range addrs {
		select {
		case <-pace.C:
		case <-ctx.Done():
		}
		select {
		case inFlight <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		tries.Add(1)
		go func() {
			defer tries.Done()
			defer func() { <-inFlight }()
			if s.subnet.Contains(a) || !s.pinging() {
				s.knock(ctx, a)
				return
			}
			var both sync.WaitGroup
			both.Add(1)
			go func() {
				defer both.Done()
				s.echo(ctx, a)
			}()
			s.knock(ctx, a)
			both.Wait()
		}()
	}
	tries.Wait()
	close(done)
	background.Wait()
	s.sample()
}

// pinging reports whether addresses beyond the subnet are pinged: not once
// a ping has failed to go at all.
func (s *survey) pinging() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ping != nil && s.pingErr == nil
}

// knock tries one address. Nothing is recorded for one whose try the
// stopping of the survey cut short.
func (s *survey) knock(ctx context.Context, a netip.Addr) {
	to := netip.AddrPortFrom(a, s.port)
	instance, err := s.c.Knock(ctx, to)
	if ctx.Err() != nil {
		return
	}
	var k knock
	switch {
	case err == nil:
		k.bacnet, k.named, k.instance = true, true, instance
	case client.Declined(err):
		// A BACnet device, and one that is not read through the wildcard
		// instance. Asked alone who it is, it may announce itself.
		k.bacnet = true
		s.c.WhoIsAt(to)
	}
	s.mu.Lock()
	s.knocked[a] = k
	s.tried++
	s.at = a
	s.mu.Unlock()
}

// echo pings one address beyond the subnet and keeps what came back. A
// ping that could not be sent at all ends the pinging.
func (s *survey) echo(ctx context.Context, a netip.Addr) {
	e, err := s.ping(ctx, a, s.timeout)
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if s.pingErr == nil {
			s.pingErr = err
		}
		return
	}
	s.echoes[a] = e
}

// sample reads the neighbor table and keeps what it says.
func (s *survey) sample() {
	table, err := s.lookup()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.neighborsErr = err
	for a, mac := range table {
		s.macs[a] = mac
	}
}

func (s *survey) report() {
	if s.progress == nil {
		return
	}
	s.mu.Lock()
	p := Progress{Tried: s.tried, Total: s.total, At: s.at, Identifying: s.identifying}
	for a, k := range s.knocked {
		switch {
		case k.bacnet || s.iams[a].known:
			p.Devices++
		case s.macs[a] != "" || s.echoes[a].alive:
			p.Hosts++
		}
	}
	s.mu.Unlock()
	s.progress(p)
}

// identify reads the devices found for their names, several at once.
func (s *survey) identify(ctx context.Context) {
	s.mu.Lock()
	s.identifying = true
	var found []netip.Addr
	for a, k := range s.knocked {
		if k.bacnet || s.iams[a].known {
			found = append(found, a)
		}
	}
	s.mu.Unlock()
	s.report()

	// A device that declined to say which it is was sent a Who-Is alone;
	// its I-Am gets the timeout to arrive, from here.
	deadline := time.Now().Add(s.timeout)
	slots := make(chan struct{}, identifyAtOnce)
	var wg sync.WaitGroup
	for _, a := range found {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			s.identifyOne(ctx, a, deadline)
		}()
	}
	wg.Wait()
}

// identifyOne learns which device is at an address, from its answer to the
// knock or from its I-Am, waiting until deadline for the one the Who-Is
// sent to it alone may bring, and then reads its name, vendor and model.
func (s *survey) identifyOne(ctx context.Context, a netip.Addr, deadline time.Time) {
	s.mu.Lock()
	k, heard := s.knocked[a], s.iams[a]
	s.mu.Unlock()
	for !k.named && !heard.known && time.Now().Before(deadline) {
		if sleep(ctx, 100*time.Millisecond) != nil {
			return
		}
		s.mu.Lock()
		heard = s.iams[a]
		s.mu.Unlock()
	}
	if !k.named && heard.known {
		k.named, k.instance = true, heard.instance
	}
	if !k.named {
		k.unnamed = true
		s.mu.Lock()
		s.knocked[a] = k
		s.mu.Unlock()
		return
	}

	to := client.Address{IP: netip.AddrPortFrom(a, s.port)}
	obj := client.ObjectID{Type: objects.ObjectTypeDevice, Instance: k.instance}
	props := []uint16{objects.PropertyIdObjectName, objects.PropertyIdVendorName, objects.PropertyIdModelName}
	got := map[uint32]string{}
	results, err := s.c.ReadPropertyMultiple(ctx, to, []client.ReadSpec{{Object: obj, Props: props}})
	switch {
	case err == nil:
		for _, r := range results {
			for _, p := range r.Props {
				if p.Err == nil && len(p.Values) == 1 && p.Values[0].Kind == client.KindString {
					got[p.ID] = p.Values[0].Str
				}
			}
		}
	case client.Declined(err):
		// No ReadPropertyMultiple: one request each.
		for _, prop := range props {
			values, err := s.c.ReadProperty(ctx, to, obj, prop)
			if err != nil && !client.Declined(err) {
				break
			}
			if err == nil && len(values) == 1 && values[0].Kind == client.KindString {
				got[uint32(prop)] = values[0].Str
			}
		}
	}
	k.name = got[uint32(objects.PropertyIdObjectName)]
	k.vendor = got[uint32(objects.PropertyIdVendorName)]
	k.model = got[uint32(objects.PropertyIdModelName)]
	s.mu.Lock()
	s.knocked[a] = k
	s.mu.Unlock()
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// result sorts what was found into the lists, in address order. An
// address the survey was stopped before trying is in none of them.
func (s *survey) result(addrs []netip.Addr) *Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := &Result{
		Total: len(addrs), Tried: len(s.knocked), Routers: map[netip.Addr]int{}, By: map[netip.Addr]netip.Addr{},
		NeighborsErr: s.neighborsErr, PingErr: s.pingErr,
	}
	for _, a := range addrs {
		k, tried := s.knocked[a]
		if !tried {
			continue
		}
		heard := s.iams[a]
		on := s.subnet.Contains(a)
		if !on {
			res.Beyond++
		}
		mac := ""
		if on {
			mac = s.macs[a]
		}
		e := s.echoes[a]
		switch {
		case k.bacnet || heard.known:
			res.Devices = append(res.Devices, Device{
				Addr: a, MAC: mac, Instance: k.instance, Unnamed: k.unnamed,
				Name: k.name, Vendor: k.vendor, Model: k.model, Routed: heard.routed,
			})
		case on && mac != "":
			res.Hosts = append(res.Hosts, Host{Addr: a, MAC: mac})
		case on && s.neighborsErr == nil:
			res.Vacant = append(res.Vacant, a)
		case on:
			res.Silent = append(res.Silent, a)
		case e.alive:
			res.Hosts = append(res.Hosts, Host{Addr: a, Ping: true})
		case e.noHost:
			res.Vacant = append(res.Vacant, a)
			res.Routers[e.from]++
			res.By[a] = e.from
		case e.noNet:
			res.Unrouted = append(res.Unrouted, a)
			res.Routers[e.from]++
			res.By[a] = e.from
		default:
			res.Silent = append(res.Silent, a)
		}
	}
	return res
}
