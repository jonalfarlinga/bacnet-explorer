package explorer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fake is a source whose nodes the test feeds in.
type fake struct {
	events   chan Event
	open     bool
	children map[NodeID][]Node
	// nodes is what the fake has announced or added, so that it can say a
	// node's children are read.
	nodes    map[NodeID]Node
	restored []Node

	mu    sync.Mutex
	reads []read // every children read asked of the fake, in order
}

type read struct {
	id    NodeID
	paced bool
}

func newFake() *fake {
	return &fake{events: make(chan Event, 64), children: map[NodeID][]Node{}, nodes: map[NodeID]Node{}}
}

func (f *fake) Describe() Description {
	return Description{Kind: "fake", Title: "Fake", AddForm: []Field{{Key: "name", Type: "text"}}}
}
func (f *fake) Open(ctx context.Context, settings Values) error { f.open = true; return nil }
func (f *fake) Close() error                                    { f.open = false; return nil }
func (f *fake) Restore(ctx context.Context, nodes []Node) error {
	for _, n := range nodes {
		f.nodes[n.ID] = n
	}
	f.restored = append(f.restored, nodes...)
	return nil
}
func (f *fake) Discover(ctx context.Context) error { return ErrNotSupported }
func (f *fake) Add(ctx context.Context, form Values) (Node, error) {
	n := Node{ID: NodeID("fake:" + form["name"]), Kind: KindDevice, Label: form["name"], Children: ChildrenState{State: ChildrenUnknown}}
	f.nodes[n.ID] = n
	return n, nil
}
func (f *fake) Children(ctx context.Context, id NodeID, opts ChildrenOptions) error {
	f.mu.Lock()
	f.reads = append(f.reads, read{id, opts.Limit != nil})
	f.mu.Unlock()
	if opts.Limit != nil {
		if err := opts.Limit.Wait(ctx, len(f.children[id])); err != nil {
			return err
		}
	}
	f.events <- Event{Type: "children", Data: ChildrenEvent{Parent: id, Replace: true, Nodes: f.children[id]}}
	n := f.nodes[id]
	n.Children = ChildrenState{State: ChildrenReady, Total: len(f.children[id])}
	f.nodes[id] = n
	f.events <- Event{Type: "node", Data: n}
	return nil
}
func (f *fake) Detail(ctx context.Context, id NodeID) (Detail, error) {
	return Detail{}, ErrNotSupported
}
func (f *fake) Read(ctx context.Context, ids []NodeID) ([]Reading, error) {
	out := make([]Reading, len(ids))
	for i := range ids {
		out[i] = Reading{Text: string(ids[i]), Quality: QualityGood}
	}
	return out, nil
}
func (f *fake) Events() <-chan Event { return f.events }
func (f *fake) Status() Status       { return Status{Open: f.open} }

func next(t *testing.T, ch <-chan Event, typ string) Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case e := <-ch:
			if e.Type == typ {
				return e
			}
		case <-deadline:
			t.Fatalf("no %q event", typ)
		}
	}
}

func TestRegistry(t *testing.T) {
	reg := New()
	f := newFake()
	reg.Register(f)
	ctx := context.Background()

	if _, err := reg.Add(ctx, "fake", Values{"name": "a"}); !errors.Is(err, ErrNotOpen) {
		t.Errorf("adding to a closed source gave %v", err)
	}
	if err := reg.Open(ctx, "fake", nil); err != nil {
		t.Fatal(err)
	}
	events, cancel := reg.Subscribe()
	defer cancel()
	next(t, events, "nodes")

	// A node fed in is stored with its source, and passed on.
	f.events <- Event{Type: "node", Data: Node{ID: "fake:r", Kind: KindGroup, Label: "Router", Children: ChildrenState{State: ChildrenReady}}}
	f.events <- Event{Type: "node", Data: Node{ID: "fake:n", Parent: "fake:r", Kind: KindGroup, Label: "Net"}}
	next(t, events, "node")
	next(t, events, "node")
	if n, ok := reg.Node("fake:n"); !ok || n.Source != "fake" || n.Parent != "fake:r" || n.Children.State != ChildrenNone {
		t.Errorf("stored %+v", n)
	}

	// Re-parenting moves a node; removing a node takes what is under it.
	added, err := reg.Add(ctx, "fake", Values{"name": "d"})
	if err != nil {
		t.Fatal(err)
	}
	next(t, events, "node")
	f.events <- Event{Type: "node", Data: Node{ID: "fake:n", Parent: added.ID, Kind: KindGroup, Label: "Net"}}
	f.events <- Event{Type: "remove", Data: NodeID("fake:r")}
	next(t, events, "remove")
	if _, ok := reg.Node("fake:r"); ok {
		t.Error("the removed node is still there")
	}
	if n, ok := reg.Node("fake:n"); !ok || n.Parent != added.ID {
		t.Errorf("the moved node is %+v, want it under %s", n, added.ID)
	}

	// Children: the first call starts the read, and each list bumps the
	// revision.
	f.children[added.ID] = []Node{{ID: "fake:d/1", Kind: KindPoint, Label: "p1"}, {ID: "fake:d/2", Kind: KindPoint, Label: "p2", Order: -1}}
	rev, list, err := reg.Children(added.ID, false)
	if err != nil || rev != 0 || len(list) != 1 {
		t.Fatalf("first children call gave rev %d, %d nodes, %v; want the group already there", rev, len(list), err)
	}
	ce := next(t, events, "children").Data.(ChildrenEvent)
	if ce.Rev != 1 || !ce.Replace || len(ce.Nodes) != 2 {
		t.Errorf("children event %+v", ce)
	}
	rev, list, _ = reg.Children(added.ID, false)
	if rev != 1 || len(list) != 3 || list[0].ID != "fake:d/2" {
		t.Errorf("children are rev %d %+v, want the 2 listed and the group, sorted by order", rev, list)
	}
	// The group under the node was announced, not listed: the list leaves
	// it alone.
	if _, ok := reg.Node("fake:n"); !ok {
		t.Error("the replace dropped the group that the source had announced under the node")
	}
	if n, _ := reg.Node("fake:d/1"); !n.Listed {
		t.Error("a node from a list is not marked as listed")
	}
	rev, list, _ = reg.Children(added.ID, false)
	if len(list) != 3 {
		t.Errorf("children are %+v, want the 2 listed and the announced group", list)
	}
	// A second list replaces the listed children and keeps the group.
	f.children[added.ID] = []Node{{ID: "fake:d/3", Kind: KindPoint, Label: "p3"}}
	reg.Children(added.ID, true)
	next(t, events, "children")
	rev, list, _ = reg.Children(added.ID, false)
	if rev != 2 || len(list) != 2 {
		t.Errorf("after a second list, children are rev %d %+v, want p3 and the group", rev, list)
	}

	// Read goes to the source by prefix.
	readings, err := reg.Read(ctx, []NodeID{"fake:d/1", "other:x"})
	if err != nil || len(readings) != 2 || readings[0].Quality != QualityGood || readings[1].Quality != QualityError {
		t.Errorf("read %+v, %v", readings, err)
	}

	// Closing forgets everything, there being no store.
	reg.Close("fake")
	next(t, events, "reset")
	if n := len(reg.Nodes()); n != 0 {
		t.Errorf("%d nodes after closing", n)
	}
	if _, _, err := reg.Children(added.ID, false); !errors.Is(err, ErrUnknownNode) {
		t.Errorf("children of a forgotten node gave %v", err)
	}
}

// With a store, what is found goes to the temp file, a decision moves it to
// a new registry file, and a new registry starts from both, unreached,
// handing the source what it held.
func TestRegistryStore(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	reg := New()
	reg.flushAfter = 20 * time.Millisecond
	if err := reg.UseStore(st); err != nil {
		t.Fatal(err)
	}
	f := newFake()
	reg.Register(f)
	if err := reg.Open(ctx, "fake", nil); err != nil {
		t.Fatal(err)
	}
	dev, _ := reg.Add(ctx, "fake", Values{"name": "d"})
	f.children[dev.ID] = []Node{
		{ID: "fake:d/1", Kind: KindPoint, Label: "p1", Native: []byte(`{"reg":1}`)},
		{ID: "fake:d/2", Kind: KindPoint, Label: "p2"},
	}
	reg.Children(dev.ID, false)
	waitUntil(t, "the points", func() bool { n, ok := reg.Node("fake:d/2"); return ok && n.Track == TrackNew && n.Reached })
	reg.Flush()
	registry, temp, err := st.Load()
	if err != nil || len(registry) != 0 || len(temp) != 3 {
		t.Fatalf("after discovery the store holds %d registry and %d temp nodes, %v; want 0 and 3", len(registry), len(temp), err)
	}

	// A decision.
	decided, err := reg.Classify([]NodeID{"fake:d/1"}, TrackTracked)
	if err != nil || len(decided) != 1 || decided[0].Track != TrackTracked {
		t.Fatalf("classify gave %+v, %v", decided, err)
	}
	if _, err := reg.Classify([]NodeID{dev.ID}, TrackSidelined); err == nil {
		t.Error("a device was classified")
	}
	reg.Flush()
	registry, temp, err = st.Load()
	if err != nil || len(registry) != 2 || len(temp) != 2 {
		t.Fatalf("after a decision the store holds %d registry and %d temp nodes, %v; want the point with its device, and the device with the other point", len(registry), len(temp), err)
	}
	if info := st.Info(); info.Registries != 1 || info.Latest == "" {
		t.Errorf("store info %+v", info)
	}

	// A list that no longer has a decided point keeps it, unreached.
	f.children[dev.ID] = []Node{{ID: "fake:d/2", Kind: KindPoint, Label: "p2"}}
	reg.Children(dev.ID, true)
	waitUntil(t, "the second list", func() bool { n, ok := reg.Node("fake:d/1"); return ok && !n.Reached })
	if n, _ := reg.Node("fake:d/1"); n.Track != TrackTracked || !sameJSON(n.Native, `{"reg":1}`) {
		t.Errorf("the decided point is %+v after the list dropped it", n)
	}
	reg.CloseAll()

	// A new run.
	reg2 := New()
	if err := reg2.UseStore(st); err != nil {
		t.Fatal(err)
	}
	nodes := reg2.Nodes()
	if len(nodes) != 3 {
		t.Fatalf("a new registry loaded %d nodes, want 3", len(nodes))
	}
	for _, n := range nodes {
		if n.Reached {
			t.Errorf("%s is reached before any source is open", n.ID)
		}
	}
	if n, _ := reg2.Node("fake:d/1"); n.Track != TrackTracked || n.Source != "fake" {
		t.Errorf("loaded %+v, want the tracked point", n)
	}
	f2 := newFake()
	reg2.Register(f2)
	if err := reg2.Open(ctx, "fake", nil); err != nil {
		t.Fatal(err)
	}
	if len(f2.restored) != 3 {
		t.Errorf("the source was handed %d nodes, want 3", len(f2.restored))
	}
	// The source speaks of the device: it is reached, and keeps its decision
	// and native data though the source sent neither.
	f2.events <- Event{Type: "node", Data: Node{ID: "fake:d/1", Parent: dev.ID, Kind: KindPoint, Label: "p1 again"}}
	waitUntil(t, "the point to be reached", func() bool { n, _ := reg2.Node("fake:d/1"); return n.Reached })
	if n, _ := reg2.Node("fake:d/1"); n.Track != TrackTracked || !sameJSON(n.Native, `{"reg":1}`) || n.Label != "p1 again" {
		t.Errorf("after contact the point is %+v", n)
	}
	reg2.CloseAll()
}

func waitUntil(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func sameJSON(a []byte, b string) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal([]byte(b), &y) == nil && fmt.Sprint(x) == fmt.Sprint(y)
}
