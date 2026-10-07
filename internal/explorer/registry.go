package explorer

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Registry holds the sources and everything they have found. Its methods
// are safe for concurrent use.
type Registry struct {
	mu      sync.Mutex
	sources map[string]*entry
	order   []string // kinds, in the order registered
	nodes   map[NodeID]*node
	subs    map[chan Event]struct{}
}

type entry struct {
	src  Source
	open bool
	// ctx lives while the source is open; the source's background reads
	// run under it.
	ctx  context.Context
	stop context.CancelFunc
	// pumpDone is closed when the goroutine that takes the source's events
	// has stopped.
	pumpDone chan struct{}
}

type node struct {
	Node
	// rev counts the changes to the node's children.
	rev uint64
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		sources: map[string]*entry{},
		nodes:   map[NodeID]*node{},
		subs:    map[chan Event]struct{}{},
	}
}

// Register makes a source available to open. A source is known by the kind
// its description gives.
func (r *Registry) Register(src Source) {
	kind := src.Describe().Kind
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sources[kind]; !ok {
		r.order = append(r.order, kind)
	}
	r.sources[kind] = &entry{src: src}
}

func (r *Registry) entry(kind string) (*entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.sources[kind]
	if e == nil {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	return e, nil
}

func (r *Registry) openEntry(kind string) (*entry, error) {
	e, err := r.entry(kind)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	open := e.open
	r.mu.Unlock()
	if !open {
		return nil, fmt.Errorf("%w: %s", ErrNotOpen, kind)
	}
	return e, nil
}

// Sources lists every registered source, open or not.
func (r *Registry) Sources() []SourceInfo {
	r.mu.Lock()
	kinds := append([]string(nil), r.order...)
	r.mu.Unlock()
	list := make([]SourceInfo, 0, len(kinds))
	for _, kind := range kinds {
		e, err := r.entry(kind)
		if err != nil {
			continue
		}
		list = append(list, SourceInfo{Description: e.src.Describe(), Status: e.src.Status()})
	}
	return list
}

// Open starts a source with the settings given. A source that is open is
// closed first: everything it had found is gone.
func (r *Registry) Open(ctx context.Context, kind string, settings Values) error {
	e, err := r.entry(kind)
	if err != nil {
		return err
	}
	r.closeEntry(kind, e)
	if err := e.src.Open(ctx, settings); err != nil {
		return err
	}
	r.mu.Lock()
	e.open = true
	e.ctx, e.stop = context.WithCancel(context.Background())
	e.pumpDone = make(chan struct{})
	r.mu.Unlock()
	go r.pump(kind, e)
	r.mu.Lock()
	r.publish(Event{Type: "source", Data: SourceInfo{Description: e.src.Describe(), Status: e.src.Status()}})
	r.mu.Unlock()
	return nil
}

// Close stops a source. Everything it had found is gone.
func (r *Registry) Close(kind string) error {
	e, err := r.entry(kind)
	if err != nil {
		return err
	}
	r.closeEntry(kind, e)
	return nil
}

func (r *Registry) closeEntry(kind string, e *entry) {
	r.mu.Lock()
	if !e.open {
		r.mu.Unlock()
		return
	}
	e.open = false
	e.stop()
	pumpDone := e.pumpDone
	r.mu.Unlock()

	e.src.Close()
	<-pumpDone

	r.mu.Lock()
	r.forget(kind)
	r.publish(Event{Type: "reset"})
	r.publish(Event{Type: "source", Data: SourceInfo{Description: e.src.Describe(), Status: e.src.Status()}})
	r.mu.Unlock()
}

// CloseAll stops every source.
func (r *Registry) CloseAll() {
	for _, info := range r.Sources() {
		r.Close(info.Kind)
	}
}

// pump takes a source's events into the registry until the source is
// closed.
func (r *Registry) pump(kind string, e *entry) {
	defer close(e.pumpDone)
	events := e.src.Events()
	for {
		select {
		case <-e.ctx.Done():
			return
		case ev := <-events:
			r.take(kind, e, ev)
		}
	}
}

// take applies one event from a source and passes it on.
func (r *Registry) take(kind string, e *entry, ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch ev.Type {
	case "node":
		n, ok := ev.Data.(Node)
		if !ok {
			return
		}
		r.upsert(kind, n)
		r.publish(Event{Type: "node", Data: r.nodes[n.ID].Node})
	case "children":
		ce, ok := ev.Data.(ChildrenEvent)
		if !ok {
			return
		}
		parent := r.nodes[ce.Parent]
		if parent == nil {
			return
		}
		if ce.Replace {
			r.dropChildren(ce.Parent, true)
		}
		for i := range ce.Nodes {
			ce.Nodes[i].Parent = ce.Parent
			ce.Nodes[i].Listed = true
			r.upsert(kind, ce.Nodes[i])
		}
		parent.rev++
		ce.Rev = parent.rev
		r.publish(Event{Type: "children", Data: ce})
	case "remove":
		id, ok := ev.Data.(NodeID)
		if ok && r.nodes[id] != nil {
			r.dropChildren(id, false)
			delete(r.nodes, id)
			r.publish(Event{Type: "remove", Data: id})
		}
	case "reset":
		r.forget(kind)
		r.publish(Event{Type: "reset"})
	case "source":
		r.publish(Event{Type: "source", Data: SourceInfo{Description: e.src.Describe(), Status: e.src.Status()}})
	}
}

// upsert stores a node, keeping the children revision of one already known.
// The caller holds r.mu.
func (r *Registry) upsert(kind string, n Node) {
	n.Source = kind
	if n.Children.State == "" {
		n.Children.State = ChildrenNone
	}
	if old := r.nodes[n.ID]; old != nil {
		old.Node = n
		return
	}
	r.nodes[n.ID] = &node{Node: n}
}

// dropChildren removes what is under a node, with everything under that:
// only the children that came in a list, with listedOnly, or every child.
// The caller holds r.mu.
func (r *Registry) dropChildren(parent NodeID, listedOnly bool) {
	for id, n := range r.nodes {
		if n.Parent == parent && (n.Listed || !listedOnly) {
			r.dropChildren(id, false)
			delete(r.nodes, id)
		}
	}
}

// forget removes everything a source found. The caller holds r.mu.
func (r *Registry) forget(kind string) {
	for id, n := range r.nodes {
		if n.Source == kind {
			delete(r.nodes, id)
		}
	}
}

// ---- reading ---------------------------------------------------------------

// Discover asks a source to look for devices.
func (r *Registry) Discover(ctx context.Context, kind string) error {
	e, err := r.openEntry(kind)
	if err != nil {
		return err
	}
	return e.src.Discover(ctx)
}

// Add lists a node from a filled-in form.
func (r *Registry) Add(ctx context.Context, kind string, form Values) (Node, error) {
	e, err := r.openEntry(kind)
	if err != nil {
		return Node{}, err
	}
	n, err := e.src.Add(ctx, form)
	if err != nil {
		return Node{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upsert(kind, n)
	stored := r.nodes[n.ID].Node
	r.publish(Event{Type: "node", Data: stored})
	return stored, nil
}

// Nodes returns every node known, roots and read children alike.
func (r *Registry) Nodes() []Node {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nodesLocked()
}

func (r *Registry) nodesLocked() []Node {
	list := make([]Node, 0, len(r.nodes))
	for _, n := range r.nodes {
		list = append(list, n.Node)
	}
	sortNodes(list)
	return list
}

// Node returns one node.
func (r *Registry) Node(id NodeID) (Node, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.nodes[id]
	if n == nil {
		return Node{}, false
	}
	return n.Node, true
}

// Children returns the children of a node as far as they have been read,
// and the revision they are at. The first call for a node starts reading
// them, as does any call with refresh set; what is read arrives as events.
func (r *Registry) Children(id NodeID, refresh bool) (uint64, []Node, error) {
	r.mu.Lock()
	n := r.nodes[id]
	if n == nil {
		r.mu.Unlock()
		return 0, nil, ErrUnknownNode
	}
	e := r.sources[n.Source]
	state := n.Children.State
	start := e != nil && e.open && (state == ChildrenUnknown || (refresh && state != ChildrenLoading && state != ChildrenNone))
	var ctx context.Context
	if start {
		ctx = e.ctx
	}
	rev := n.rev
	var list []Node
	for _, c := range r.nodes {
		if c.Parent == id {
			list = append(list, c.Node)
		}
	}
	r.mu.Unlock()

	if start {
		if err := e.src.Children(ctx, id); err != nil {
			return rev, nil, err
		}
	}
	sortNodes(list)
	return rev, list, nil
}

// Detail reads everything behind a node, now. The node need not have been
// listed: its ID says which source it belongs to, and the source decides
// whether it knows it.
func (r *Registry) Detail(ctx context.Context, id NodeID) (Detail, error) {
	kind, _, ok := strings.Cut(string(id), ":")
	if !ok {
		return Detail{}, ErrUnknownNode
	}
	e, err := r.openEntry(kind)
	if err != nil {
		return Detail{}, err
	}
	return e.src.Detail(ctx, id)
}

// Read reads the current values of several points, each from its source.
func (r *Registry) Read(ctx context.Context, ids []NodeID) ([]Reading, error) {
	byKind := map[string][]int{}
	for i, id := range ids {
		kind, _, _ := strings.Cut(string(id), ":")
		byKind[kind] = append(byKind[kind], i)
	}
	out := make([]Reading, len(ids))
	for kind, indexes := range byKind {
		e, err := r.openEntry(kind)
		if err != nil {
			for _, i := range indexes {
				out[i] = Reading{Quality: QualityError, Text: err.Error()}
			}
			continue
		}
		part := make([]NodeID, len(indexes))
		for j, i := range indexes {
			part[j] = ids[i]
		}
		readings, err := e.src.Read(ctx, part)
		if err != nil {
			return nil, err
		}
		for j, i := range indexes {
			if j < len(readings) {
				out[i] = readings[j]
			}
		}
	}
	return out, nil
}

// ---- events ----------------------------------------------------------------

// Subscribe returns a channel of events and a function that ends the
// subscription. The first event is "nodes", with everything known at that
// moment. A subscriber that falls too far behind has its channel closed,
// and should subscribe again.
func (r *Registry) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 512)
	r.mu.Lock()
	ch <- Event{Type: "nodes", Data: r.nodesLocked()}
	r.subs[ch] = struct{}{}
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		if _, ok := r.subs[ch]; ok {
			delete(r.subs, ch)
			close(ch)
		}
		r.mu.Unlock()
	}
}

// publish sends an event to every subscriber. The caller holds r.mu.
func (r *Registry) publish(e Event) {
	for ch := range r.subs {
		select {
		case ch <- e:
		default:
			delete(r.subs, ch)
			close(ch)
		}
	}
}

func sortNodes(list []Node) {
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.Parent != b.Parent {
			return a.Parent < b.Parent
		}
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		return a.ID < b.ID
	})
}
