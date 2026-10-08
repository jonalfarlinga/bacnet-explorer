package bacnet

import (
	"context"
	"fmt"
	"math"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jonalfarlinga/bacnet/client"
	"github.com/jonalfarlinga/bacnet/internal/explorer"
)

// The sweep finds the devices that neither answer a Who-Is nor were listed
// by hand: once a day it tries every address in the ranges the admin gave,
// with one read each, and lists what answers. It is off until ranges are
// given, it never leaves the adapter's own first two bytes, a range being
// over the last two only, and it leaves alone every address a device is
// listed at already.

// DefaultSweepPerSec is how many addresses a sweep tries in a second.
const DefaultSweepPerSec = 10

// How often a running sweep tells the UI how far it is, how many addresses
// that answered without a name its status lists, and how long a sweep on
// opening gives the Who-Is first, so that a device that announces itself is
// not listed as one that did not.
const (
	sweepProgressEvery = 5 * time.Second
	sweepUnnamedShown  = 8
	sweepSettle        = 10 * time.Second
)

// hostRange is a run of addresses by their last two bytes, both ends
// included.
type hostRange struct{ from, to uint16 }

func (r hostRange) String() string {
	if r.from == r.to {
		return hostString(r.from)
	}
	return hostString(r.from) + "-" + hostString(r.to)
}

func hostString(h uint16) string {
	return fmt.Sprintf("%d.%d", h>>8, h&0xFF)
}

// parseHost reads the last two bytes of an address, as "4.17".
func parseHost(s string) (uint16, error) {
	hi, lo, ok := strings.Cut(strings.TrimSpace(s), ".")
	a, err1 := strconv.ParseUint(hi, 10, 8)
	b, err2 := strconv.ParseUint(lo, 10, 8)
	if !ok || err1 != nil || err2 != nil {
		return 0, fmt.Errorf("%q is not the last two bytes of an address, as 4.17", strings.TrimSpace(s))
	}
	return uint16(a)<<8 | uint16(b), nil
}

// parseSweepRanges reads the ranges of a sweep as the admin writes them:
// "0.1-255.254", or several with commas, each the last two bytes of the
// first and the last address, or of one address alone. Ranges that touch or
// overlap are joined, so that no address is tried twice. Nothing gives no
// ranges: the sweep is off.
func parseSweepRanges(s string) ([]hostRange, error) {
	var ranges []hostRange
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		first, last, isRange := strings.Cut(part, "-")
		from, err := parseHost(first)
		if err != nil {
			return nil, err
		}
		to := from
		if isRange {
			if to, err = parseHost(last); err != nil {
				return nil, err
			}
		}
		if to < from {
			return nil, fmt.Errorf("the range %s is backwards", part)
		}
		ranges = append(ranges, hostRange{from, to})
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].from < ranges[j].from })
	var joined []hostRange
	for _, r := range ranges {
		if n := len(joined); n > 0 && uint32(r.from) <= uint32(joined[n-1].to)+1 {
			joined[n-1].to = max(joined[n-1].to, r.to)
			continue
		}
		joined = append(joined, r)
	}
	return joined, nil
}

// sweepClock is when in the day a sweep starts.
type sweepClock struct {
	// atOpen starts one once the source is open, and then a day after each
	// start, instead of at a time of day.
	atOpen       bool
	hour, minute int
}

// parseSweepClock reads a time of day as "02:00", or "now".
func parseSweepClock(s string) (sweepClock, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		s = "02:00"
	}
	if s == "now" {
		return sweepClock{atOpen: true}, nil
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		return sweepClock{}, fmt.Errorf("sweep time %q is not a time of day, as 02:00, or \"now\"", s)
	}
	return sweepClock{hour: t.Hour(), minute: t.Minute()}, nil
}

func (k sweepClock) String() string {
	if k.atOpen {
		return "on opening, then daily"
	}
	return fmt.Sprintf("daily at %02d:%02d", k.hour, k.minute)
}

// next is when the sweep after now starts, the last one having started at
// last; zero if there was none.
func (k sweepClock) next(now, last time.Time) time.Time {
	if k.atOpen {
		if last.IsZero() {
			return now.Add(sweepSettle)
		}
		if due := last.Add(24 * time.Hour); due.After(now) {
			return due
		}
		return now
	}
	at := time.Date(now.Year(), now.Month(), now.Day(), k.hour, k.minute, 0, 0, now.Location())
	if !at.After(now) {
		at = time.Date(now.Year(), now.Month(), now.Day()+1, k.hour, k.minute, 0, 0, now.Location())
	}
	return at
}

// sweepPlan is what a sweep tries, and when.
type sweepPlan struct {
	// prefix is the first two bytes of every address tried: the adapter's.
	prefix [2]byte
	ranges []hostRange
	port   uint16
	clock  sweepClock
	// local, subnet and broadcast are the adapter's own, which decide what
	// is left out.
	local     netip.Addr
	subnet    netip.Prefix
	broadcast netip.Addr
}

// newSweepPlan makes the plan for an adapter from the settings, or nil when
// no ranges are given.
func newSweepPlan(ifc client.Interface, port uint16, ranges, at string) (*sweepPlan, error) {
	rs, err := parseSweepRanges(ranges)
	if err != nil {
		return nil, fmt.Errorf("sweep: %w", err)
	}
	if len(rs) == 0 {
		return nil, nil
	}
	clock, err := parseSweepClock(at)
	if err != nil {
		return nil, err
	}
	b := ifc.Addr.Addr().As4()
	return &sweepPlan{
		prefix: [2]byte{b[0], b[1]}, ranges: rs, port: port, clock: clock,
		local: ifc.Addr.Addr(), subnet: ifc.Addr.Masked(), broadcast: ifc.Broadcast,
	}, nil
}

func (p *sweepPlan) addr(host uint16) netip.Addr {
	return netip.AddrFrom4([4]byte{p.prefix[0], p.prefix[1], byte(host >> 8), byte(host)})
}

// skip reports whether an address in a range is left out: this machine's
// own, and those that are not a host's. In the adapter's subnet that is the
// subnet's own address and its broadcast address. Outside it the subnets
// are not known, so the addresses that end in 0 and 255 are left out, which
// are those of a subnet of 256.
func (p *sweepPlan) skip(a netip.Addr) bool {
	if a == p.local {
		return true
	}
	if p.subnet.Contains(a) {
		return a == p.subnet.Addr() || a == p.broadcast
	}
	last := a.As4()[3]
	return last == 0 || last == 255
}

// each calls try for every address the sweep tries, in order, until try
// returns false.
func (p *sweepPlan) each(try func(netip.AddrPort) bool) {
	for _, r := range p.ranges {
		for h := uint32(r.from); h <= uint32(r.to); h++ {
			a := p.addr(uint16(h))
			if p.skip(a) {
				continue
			}
			if !try(netip.AddrPortFrom(a, p.port)) {
				return
			}
		}
	}
}

func (p *sweepPlan) count() int {
	n := 0
	p.each(func(netip.AddrPort) bool { n++; return true })
	return n
}

// String names the addresses, as "10.20.0.1-10.20.255.254".
func (p *sweepPlan) String() string {
	parts := make([]string, len(p.ranges))
	for i, r := range p.ranges {
		parts[i] = p.addr(r.from).String()
		if r.to != r.from {
			parts[i] += "-" + p.addr(r.to).String()
		}
	}
	return strings.Join(parts, ", ")
}

// sweepRun counts one sweep.
type sweepRun struct {
	started time.Time
	took    time.Duration // zero while it runs
	total   int           // addresses to try
	tried   int
	// listed is the addresses left alone because a device is listed there
	// already, heard in this run or kept in the store from an earlier one.
	listed int
	// found is the devices that answered and were listed for it.
	found int
	// unnamed is the addresses that answered as a BACnet device does and
	// would not say which device they are.
	unnamed []netip.AddrPort
	// stopped is set on a sweep that the closing of the source cut short.
	stopped bool
}

func (r sweepRun) String() string {
	text := fmt.Sprintf("%d of %d addresses tried", r.tried, r.total)
	if r.took > 0 {
		text = fmt.Sprintf("%s, %d addresses in %s", r.started.Format("2 Jan 15:04"), r.tried, r.took.Round(time.Second))
		if r.stopped {
			text += ", cut short"
		}
	}
	text += fmt.Sprintf(": %s found", plural(r.found, "device"))
	if r.listed > 0 {
		text += fmt.Sprintf("; %s left alone, listed already", plural(r.listed, "address"))
	}
	if n := len(r.unnamed); n > 0 {
		shown := r.unnamed[:min(n, sweepUnnamedShown)]
		list := make([]string, len(shown))
		for i, a := range shown {
			list[i] = a.Addr().String()
		}
		text += fmt.Sprintf("; %d would not say which device they are: %s", n, strings.Join(list, ", "))
		if n > len(shown) {
			text += ", …"
		}
	}
	return text
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	if strings.HasSuffix(noun, "s") {
		noun += "e"
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// sweeper is the sweep of an open source.
type sweeper struct {
	plan *sweepPlan
	// perSec is the addresses tried in a second.
	perSec float64

	mu      sync.Mutex
	next    time.Time
	running *sweepRun
	last    *sweepRun
}

// lines is what the source's status says of the sweep. listed reports
// whether a device is listed at an address: one that would not say which
// device it is, and has since been heard from, is no longer named.
func (w *sweeper) lines(listed func(netip.AddrPort) bool) []explorer.Fact {
	w.mu.Lock()
	defer w.mu.Unlock()
	text := func(run *sweepRun) string {
		r := *run
		r.unnamed = nil
		for _, a := range run.unnamed {
			if !listed(a) {
				r.unnamed = append(r.unnamed, a)
			}
		}
		return r.String()
	}
	lines := []explorer.Fact{{Label: "Sweep", Value: fmt.Sprintf("%s, %s", w.plan, w.plan.clock)}}
	if w.running != nil {
		lines = append(lines, explorer.Fact{Label: "Sweep now", Value: text(w.running)})
	} else if !w.next.IsZero() {
		lines = append(lines, explorer.Fact{Label: "Next sweep", Value: w.next.Format("2 Jan 15:04")})
	}
	if w.last != nil {
		lines = append(lines, explorer.Fact{Label: "Last sweep", Value: text(w.last)})
	}
	return lines
}

// startSweep runs the plan's sweeps for as long as the source stays open.
func (s *Source) startSweep(plan *sweepPlan) {
	perSec := s.opts.SweepPerSec
	if perSec <= 0 {
		perSec = DefaultSweepPerSec
	}
	w := &sweeper{plan: plan, perSec: perSec}
	s.mu.Lock()
	ctx, c := s.ctx, s.c
	if c == nil {
		s.mu.Unlock()
		return
	}
	s.sweep = w
	s.mu.Unlock()
	go s.runSweeps(ctx, c, w)
}

func (s *Source) runSweeps(ctx context.Context, c *client.Client, w *sweeper) {
	var last time.Time
	for {
		next := w.plan.clock.next(time.Now(), last)
		w.mu.Lock()
		w.next = next
		w.mu.Unlock()
		s.emit(explorer.Event{Type: "source"})
		// The wait goes by the clock, a minute at a time: a machine that
		// slept, or whose clock was set, still starts at the time of day.
		for wait := time.Until(next); wait > 0; wait = time.Until(next) {
			if sleep(ctx, min(wait, time.Minute)) != nil {
				return
			}
		}
		if ctx.Err() != nil {
			return
		}
		last = time.Now()
		s.sweepOnce(ctx, c, w)
		if ctx.Err() != nil {
			return
		}
	}
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

// sweepOnce tries every address of the plan once, at the sweeper's pace,
// and lists the devices that answer. An address a device is listed at is
// not tried: the device is known, and read by other means.
func (s *Source) sweepOnce(ctx context.Context, c *client.Client, w *sweeper) {
	w.mu.Lock()
	w.running = &sweepRun{started: time.Now(), total: w.plan.count()}
	w.mu.Unlock()
	s.emit(explorer.Event{Type: "source"})

	// Progress, for whoever is watching.
	progress, watched := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for sleep(progress, sweepProgressEvery) == nil {
			s.emit(explorer.Event{Type: "source"})
		}
	}()

	// An address that does not answer holds its place for the client's
	// timeout, so this many are waited on at once at the pace asked for.
	limit := explorer.NewRateLimit(w.perSec*60, 1)
	inFlight := make(chan struct{}, max(4, int(math.Ceil(w.perSec*s.opts.Timeout.Seconds()))+1))
	var knocks sync.WaitGroup
	w.plan.each(func(addr netip.AddrPort) bool {
		s.mu.Lock()
		listed := s.deviceAt(addr)
		s.mu.Unlock()
		if listed {
			w.mu.Lock()
			w.running.listed++
			w.running.total--
			w.mu.Unlock()
			return ctx.Err() == nil
		}
		if limit.Wait(ctx, 1) != nil {
			return false
		}
		select {
		case inFlight <- struct{}{}:
		case <-ctx.Done():
			return false
		}
		knocks.Add(1)
		go func() {
			defer knocks.Done()
			defer func() { <-inFlight }()
			s.knock(ctx, c, w, addr)
		}()
		return true
	})
	knocks.Wait()
	watched()
	wg.Wait()

	w.mu.Lock()
	run := w.running
	run.took = max(time.Since(run.started), time.Millisecond)
	run.stopped = ctx.Err() != nil
	w.last, w.running = run, nil
	w.mu.Unlock()
	s.emit(explorer.Event{Type: "source"})
}

// knock tries one address. A device that answers is read as one added by
// its address is, unless it has been listed at that address meanwhile.
func (s *Source) knock(ctx context.Context, c *client.Client, w *sweeper, addr netip.AddrPort) {
	instance, err := c.Knock(ctx, addr)
	if ctx.Err() != nil {
		return
	}
	var found, unnamed bool
	switch {
	case err == nil:
		s.mu.Lock()
		d := s.devices[instance]
		listed := d != nil && !d.dev.Addr.Routed() && d.dev.Addr.IP == addr
		s.mu.Unlock()
		if !listed {
			if dev, err := c.Probe(ctx, client.Address{IP: addr}, instance); err == nil {
				s.record(dev, true)
				found = true
			}
		}
	case client.Declined(err):
		// A BACnet device, and one that is not read through the wildcard
		// instance. Asked alone who it is, it is listed if its I-Am comes
		// back here; if not, the admin has its address to go by.
		unnamed = true
		c.WhoIsAt(addr)
	}
	w.mu.Lock()
	w.running.tried++
	if found {
		w.running.found++
	}
	if unnamed {
		w.running.unnamed = append(w.running.unnamed, addr)
	}
	w.mu.Unlock()
}

// deviceAt reports whether a device is listed at the address itself, not
// behind a router there. The caller holds s.mu.
func (s *Source) deviceAt(addr netip.AddrPort) bool {
	for _, d := range s.devices {
		if !d.dev.Addr.Routed() && d.dev.Addr.IP == addr {
			return true
		}
	}
	return false
}
