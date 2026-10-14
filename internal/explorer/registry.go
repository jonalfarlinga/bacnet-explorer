package explorer

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Registry holds the sources and everything they have found. Its methods
// are safe for concurrent use.
type Registry struct {
	mu      sync.Mutex
	sources map[string]*entry
	order   []string // kinds, in the order registered
	nodes   map[NodeID]*node
	subs    map[chan Event]struct{}

	// store, when set, keeps the nodes across runs. inRegistry marks the
	// nodes that belong in the registry file: the points the admin has
	// decided about, and their devices. Everything else that is not a
	// group goes to the temp file, which is rewritten flushAfter the last
	// change.
	store      *Store
	inRegistry map[NodeID]bool
	flushAfter time.Duration
	flush      *time.Timer

	walk *walker
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
		sources:    map[string]*entry{},
		nodes:      map[NodeID]*node{},
		subs:       map[chan Event]struct{}{},
		inRegistry: map[NodeID]bool{},
		flushAfter: time.Second,
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

// ---- the store -------------------------------------------------------------

// UseStore loads what the store holds and keeps the registry in it from
// then on. The nodes loaded start unreached, and are handed to each source
// when it opens. It is called before any source is open.
func (r *Registry) UseStore(st *Store) error {
	registry, temp, err := st.Load()
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.store = st
	for _, n := range registry {
		kind, _, _ := strings.Cut(string(n.ID), ":")
		r.upsert(kind, n, false)
		r.inRegistry[n.ID] = true
	}
	for _, n := range temp {
		kind, _, _ := strings.Cut(string(n.ID), ":")
		if _, known := r.nodes[n.ID]; !known {
			r.upsert(kind, n, false)
		}
	}
	return nil
}

// StoreInfo describes the store, and false when there is none.
func (r *Registry) StoreInfo() (StoreInfo, bool) {
	r.mu.Lock()
	st := r.store
	r.mu.Unlock()
	if st == nil {
		return StoreInfo{}, false
	}
	return st.Info(), true
}

// markDirty schedules a rewrite of the temp file. The caller holds r.mu.
func (r *Registry) markDirty() {
	if r.store == nil {
		return
	}
	if r.flush == nil {
		r.flush = time.AfterFunc(r.flushAfter, r.writeTemp)
	} else {
		r.flush.Reset(r.flushAfter)
	}
}

// writeTemp writes everything that is not in the registry file, and not a
// group, to the temp file, with the devices those nodes are under, so that
// the file stands on its own.
func (r *Registry) writeTemp() {
	r.mu.Lock()
	st := r.store
	include := map[NodeID]bool{}
	for id, n := range r.nodes {
		if n.Kind == KindGroup || r.inRegistry[id] {
			continue
		}
		include[id] = true
		for p := r.nodes[n.Parent]; p != nil; p = r.nodes[p.Parent] {
			if p.Kind == KindDevice {
				include[p.ID] = true
			}
		}
	}
	nodes := make([]Node, 0, len(include))
	for id := range include {
		nodes = append(nodes, r.nodes[id].Node)
	}
	r.mu.Unlock()
	if st == nil {
		return
	}
	sortNodes(nodes)
	st.WriteTemp(nodes)
}

// Flush writes the temp file now, instead of after the usual delay.
func (r *Registry) Flush() {
	r.mu.Lock()
	if r.flush != nil {
		r.flush.Stop()
	}
	r.mu.Unlock()
	r.writeTemp()
}

// Classify records the admin's decision about some points, and writes the
// registry file anew with every decision so far. The points' devices go
// into it with them. It returns the points as they now are.
func (r *Registry) Classify(ids []NodeID, track Track) ([]Node, error) {
	if track != TrackTracked && track != TrackSidelined {
		return nil, fmt.Errorf("a point can be tracked or sidelined, not %q", track)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Node
	for _, id := range ids {
		n := r.nodes[id]
		if n == nil {
			return nil, fmt.Errorf("%w: %s", ErrUnknownNode, id)
		}
		if n.Kind != KindPoint {
			return nil, fmt.Errorf("%s is not a point", id)
		}
		n.Track = track
		r.inRegistry[id] = true
		for p := r.nodes[n.Parent]; p != nil; p = r.nodes[p.Parent] {
			if p.Kind == KindDevice {
				r.inRegistry[p.ID] = true
			}
		}
		out = append(out, n.Node)
		r.publish(Event{Type: "node", Data: n.Node})
	}
	if _, err := r.writeRegistry(); err != nil {
		return nil, err
	}
	return out, nil
}

// writeRegistry writes the registry file anew, with every decision as it
// now stands, and returns its name. Without a store it does nothing. The
// caller holds r.mu.
func (r *Registry) writeRegistry() (string, error) {
	if r.store == nil {
		return "", nil
	}
	var keep []Node
	for id, n := range r.nodes {
		if r.inRegistry[id] {
			keep = append(keep, n.Node)
		}
	}
	sortNodes(keep)
	name, err := r.store.WriteRegistry(keep)
	if err != nil {
		return "", err
	}
	r.markDirty()
	return name, nil
}

// Registries describes every registry file, the latest first: the one in
// force, and the history behind it.
func (r *Registry) Registries() ([]RegistryInfo, error) {
	r.mu.Lock()
	st := r.store
	r.mu.Unlock()
	if st == nil {
		return nil, ErrNoStore
	}
	return st.Registries()
}

// RegistryFile reads one registry file, for the admin to look at.
func (r *Registry) RegistryFile(name string) (RegistryFile, error) {
	r.mu.Lock()
	st := r.store
	r.mu.Unlock()
	if st == nil {
		return RegistryFile{}, ErrNoStore
	}
	return st.ReadRegistry(name)
}

// RestoreRegistry puts a registry file of the history back in force: its
// decisions are the decisions again. A point decided about since, and not
// in the file, is new again; a point removed since comes back, unreached.
// What is known of a node now is kept; only the decision is taken from the
// file. No file is changed: what is restored is written as a new registry
// file, whose name is returned, so the history keeps the registry it
// replaced.
func (r *Registry) RestoreRegistry(name string) (string, error) {
	r.mu.Lock()
	st := r.store
	r.mu.Unlock()
	if st == nil {
		return "", ErrNoStore
	}
	file, err := st.ReadRegistry(name)
	if err != nil {
		return "", err
	}
	if file.InForce {
		return name, nil
	}

	r.mu.Lock()
	inFile := make(map[NodeID]bool, len(file.Nodes))
	for _, n := range file.Nodes {
		inFile[n.ID] = true
	}
	for id := range r.inRegistry {
		if n := r.nodes[id]; n != nil && n.Kind == KindPoint && !inFile[id] {
			n.Track = TrackNew
			r.publish(Event{Type: "node", Data: n.Node})
		}
	}
	r.inRegistry = make(map[NodeID]bool, len(file.Nodes))
	// The nodes that come back, for their sources.
	back := map[string][]Node{}
	for _, n := range file.Nodes {
		kind, _, _ := strings.Cut(string(n.ID), ":")
		r.inRegistry[n.ID] = true
		known := r.nodes[n.ID]
		switch {
		case known == nil:
			r.upsert(kind, n, false)
			known = r.nodes[n.ID]
			back[kind] = append(back[kind], known.Node)
		case n.Kind == KindPoint && n.Track != "" && n.Track != known.Track:
			known.Track = n.Track
		default:
			continue
		}
		r.publish(Event{Type: "node", Data: known.Node})
	}
	written, err := r.writeRegistry()
	type handing struct {
		e     *entry
		nodes []Node
	}
	var hand []handing
	for kind, nodes := range back {
		if e := r.sources[kind]; e != nil && e.open {
			hand = append(hand, handing{e, nodes})
		}
	}
	r.mu.Unlock()
	if err != nil {
		return "", err
	}

	for _, h := range hand {
		sortNodes(h.nodes)
		if err := h.e.src.Restore(h.e.ctx, h.nodes); err != nil {
			return written, err
		}
	}
	return written, nil
}

// ---- sources ---------------------------------------------------------------

// Open starts a source with the settings given, and hands it what the store
// holds of its nodes. A source that is open is closed first.
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
	var restore []Node
	for _, n := range r.nodes {
		if n.Source == kind && n.Kind != KindGroup {
			restore = append(restore, n.Node)
		}
	}
	r.mu.Unlock()
	go r.pump(kind, e)
	if len(restore) > 0 {
		sortNodes(restore)
		if err := e.src.Restore(e.ctx, restore); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.publish(Event{Type: "source", Data: SourceInfo{Description: e.src.Describe(), Status: e.src.Status()}})
	r.mu.Unlock()
	return nil
}

// Close stops a source. What it found stays listed, unreached, if there is
// a store; otherwise it is gone.
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
	r.publish(Event{Type: "source", Data: SourceInfo{Description: e.src.Describe(), Status: e.src.Status()}})
	r.mu.Unlock()
}

// CloseAll stops the walk and every source, and writes the temp file.
func (r *Registry) CloseAll() {
	r.StopWalk()
	for _, info := range r.Sources() {
		r.Close(info.Kind)
	}
	r.Flush()
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

// take applies one event from a source and passes it on. A node a source
// speaks of has been reached.
func (r *Registry) take(kind string, e *entry, ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch ev.Type {
	case "node":
		n, ok := ev.Data.(Node)
		if !ok {
			return
		}
		r.upsert(kind, n, true)
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
			r.upsert(kind, ce.Nodes[i], true)
			ce.Nodes[i] = r.nodes[ce.Nodes[i].ID].Node
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
	case "source":
		r.publish(Event{Type: "source", Data: SourceInfo{Description: e.src.Describe(), Status: e.src.Status()}})
	}
}

// upsert stores a node, keeping the children revision, the decision and
// the native data of one already known. The caller holds r.mu.
func (r *Registry) upsert(kind string, n Node, reached bool) {
	n.Source = kind
	n.Reached = reached
	if n.Children.State == "" {
		n.Children.State = ChildrenNone
	}
	old := r.nodes[n.ID]
	if old != nil {
		if n.Track == "" {
			n.Track = old.Track
		}
		if n.Native == nil {
			n.Native = old.Native
		}
		if !reached && old.Reached {
			n.Reached = true
		}
	}
	if n.Kind == KindPoint && n.Track == "" {
		n.Track = TrackNew
	}
	if old != nil && n.Walked.IsZero() {
		n.Walked = old.Walked
	}
	if n.Kind != KindGroup {
		if old == nil || old.Track != n.Track || old.Label != n.Label || old.Parent != n.Parent ||
			string(old.Native) != string(n.Native) || !old.Walked.Equal(n.Walked) {
			r.markDirty()
		}
	}
	if old != nil {
		old.Node = n
		return
	}
	r.nodes[n.ID] = &node{Node: n}
}

// dropChildren removes what is under a node, with everything under that:
// only the children that came in a list, with listedOnly, or every child.
// A point the admin has decided about is never dropped by a list that no
// longer has it: it stays, unreached, for the admin to see. The caller
// holds r.mu.
func (r *Registry) dropChildren(parent NodeID, listedOnly bool) {
	for id, n := range r.nodes {
		if n.Parent != parent || (listedOnly && !n.Listed) {
			continue
		}
		if listedOnly && (n.Track == TrackTracked || n.Track == TrackSidelined) {
			n.Reached = false
			r.publish(Event{Type: "node", Data: n.Node})
			continue
		}
		r.dropChildren(id, false)
		delete(r.nodes, id)
	}
}

// forget marks everything a source found as unreached, and drops its
// groups, which the source makes again. Without a store the nodes go too:
// there is nothing to bring them back. Subscribers get the list anew. The
// caller holds r.mu.
func (r *Registry) forget(kind string) {
	for id, n := range r.nodes {
		if n.Source != kind {
			continue
		}
		if n.Kind == KindGroup || r.store == nil {
			delete(r.nodes, id)
			continue
		}
		n.Reached = false
	}
	r.publish(Event{Type: "reset"})
	r.publish(Event{Type: "nodes", Data: r.nodesLocked()})
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
	r.upsert(kind, n, true)
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
	return r.children(id, refresh, ChildrenOptions{})
}

func (r *Registry) children(id NodeID, refresh bool, opts ChildrenOptions) (uint64, []Node, error) {
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
		if err := e.src.Children(ctx, id, opts); err != nil {
			return rev, nil, err
		}
	}
	sortNodes(list)
	return rev, list, nil
}

// Remove forgets some points, for ones that have moved or gone: they leave
// the registry file and the temp file, and the tree. A point the device
// still has comes back as new the next time its device is read.
func (r *Registry) Remove(ids []NodeID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		n := r.nodes[id]
		if n == nil {
			continue
		}
		if n.Kind != KindPoint {
			return fmt.Errorf("%s is not a point", id)
		}
		r.dropChildren(id, false)
		delete(r.nodes, id)
		delete(r.inRegistry, id)
		r.publish(Event{Type: "remove", Data: id})
	}
	_, err := r.writeRegistry()
	return err
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
