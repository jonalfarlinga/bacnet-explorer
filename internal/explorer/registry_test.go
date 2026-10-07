package explorer

import (
	"context"
	"errors"
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
	nodes map[NodeID]Node
}

func newFake() *fake {
	return &fake{events: make(chan Event, 64), children: map[NodeID][]Node{}, nodes: map[NodeID]Node{}}
}

func (f *fake) Describe() Description {
	return Description{Kind: "fake", Title: "Fake", AddForm: []Field{{Key: "name", Type: "text"}}}
}
func (f *fake) Open(ctx context.Context, settings Values) error { f.open = true; return nil }
func (f *fake) Close() error                                    { f.open = false; return nil }
func (f *fake) Discover(ctx context.Context) error              { return ErrNotSupported }
func (f *fake) Add(ctx context.Context, form Values) (Node, error) {
	n := Node{ID: NodeID("fake:" + form["name"]), Kind: KindDevice, Label: form["name"], Children: ChildrenState{State: ChildrenUnknown}}
	f.nodes[n.ID] = n
	return n, nil
}
func (f *fake) Children(ctx context.Context, id NodeID) error {
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

	// Closing forgets everything.
	reg.Close("fake")
	next(t, events, "reset")
	if n := len(reg.Nodes()); n != 0 {
		t.Errorf("%d nodes after closing", n)
	}
	if _, _, err := reg.Children(added.ID, false); !errors.Is(err, ErrUnknownNode) {
		t.Errorf("children of a forgotten node gave %v", err)
	}
}
