package explorer

import (
	"context"
	"testing"
	"time"
)

// The walk reads every reached device in turn, never-walked first, paced,
// and leaves the time of each walk on the device. Removing a point forgets
// it.
func TestWalkAndRemove(t *testing.T) {
	dir := t.TempDir()
	st, _ := OpenStore(dir)
	reg := New()
	reg.flushAfter = 20 * time.Millisecond
	reg.UseStore(st)
	f := newFake()
	reg.Register(f)
	ctx := context.Background()
	reg.Open(ctx, "fake", nil)
	events, cancel := reg.Subscribe()
	defer cancel()

	// Three devices: one walked long ago, one never, one unreached.
	long := time.Now().Add(-48 * time.Hour)
	f.events <- Event{Type: "node", Data: Node{ID: "fake:old", Kind: KindDevice, Label: "old", Walked: long, Children: ChildrenState{State: ChildrenUnknown}}}
	f.events <- Event{Type: "node", Data: Node{ID: "fake:fresh", Kind: KindDevice, Label: "fresh", Children: ChildrenState{State: ChildrenUnknown}}}
	for i := 0; i < 2; i++ {
		next(t, events, "node")
	}
	reg.mu.Lock()
	reg.nodes["fake:gone"] = &node{Node: Node{ID: "fake:gone", Source: "fake", Kind: KindDevice, Label: "gone", Children: ChildrenState{State: ChildrenUnknown}}}
	reg.mu.Unlock()
	f.nodes["fake:old"] = Node{ID: "fake:old", Kind: KindDevice, Label: "old"}
	f.nodes["fake:fresh"] = Node{ID: "fake:fresh", Kind: KindDevice, Label: "fresh"}
	f.children["fake:old"] = []Node{{ID: "fake:old/1", Kind: KindPoint, Label: "o1"}}
	f.children["fake:fresh"] = []Node{{ID: "fake:fresh/1", Kind: KindPoint, Label: "f1"}, {ID: "fake:fresh/2", Kind: KindPoint, Label: "f2"}}

	reg.StartWalk(WalkConfig{PointsPerMinute: 6000})
	waitUntil(t, "both devices to be walked", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.reads) >= 2
	})
	f.mu.Lock()
	reads := append([]read(nil), f.reads...)
	f.mu.Unlock()
	if reads[0].id != "fake:fresh" || reads[1].id != "fake:old" || !reads[0].paced || !reads[1].paced {
		t.Errorf("the walk read %+v, want fresh (never walked) then old, both paced", reads)
	}
	waitUntil(t, "the walk to be recorded", func() bool { n, _ := reg.Node("fake:old"); return n.Walked.After(long) })
	waitUntil(t, "the walk to count", func() bool { return reg.WalkStatus().Done >= 2 })
	time.Sleep(50 * time.Millisecond)
	f.mu.Lock()
	n := len(f.reads)
	f.mu.Unlock()
	if n != 2 {
		t.Errorf("the walk made %d reads, want 2: the unreached device and the walked ones are not due", n)
	}
	reg.StopWalk()
	if st := reg.WalkStatus(); st.Enabled {
		t.Error("the walk reports enabled after being stopped")
	}

	// Removing a point forgets it, in the store too.
	if _, err := reg.Classify([]NodeID{"fake:fresh/1"}, TrackTracked); err != nil {
		t.Fatal(err)
	}
	if err := reg.Remove([]NodeID{"fake:fresh/1", "fake:fresh/2"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Remove([]NodeID{"fake:fresh"}); err == nil {
		t.Error("a device was removed")
	}
	if _, ok := reg.Node("fake:fresh/1"); ok {
		t.Error("the removed point is still there")
	}
	reg.Flush()
	registry, temp, _ := st.Load()
	for _, n := range append(registry, temp...) {
		if n.ID == "fake:fresh/1" || n.ID == "fake:fresh/2" {
			t.Errorf("the store still holds %s", n.ID)
		}
	}
	reg.CloseAll()
}

func TestRateLimit(t *testing.T) {
	l := NewRateLimit(60, 5).(*rateLimit)
	now := time.Unix(0, 0)
	var slept time.Duration
	l.now = func() time.Time { return now }
	l.sleep = func(ctx context.Context, d time.Duration) error { slept += d; now = now.Add(d); return nil }
	l.lastFill = now
	ctx := context.Background()

	// The burst goes at once; the next reads wait a second each at 60 a
	// minute.
	l.Wait(ctx, 5)
	if slept != 0 {
		t.Errorf("the burst waited %s", slept)
	}
	l.Wait(ctx, 1)
	if slept != time.Second {
		t.Errorf("one read past the burst waited %s, want 1s", slept)
	}
	l.Wait(ctx, 12)
	if slept != 13*time.Second {
		t.Errorf("a batch of 12 waited %s in all, want 13s", slept)
	}
	// A quiet minute refills the burst, no more.
	now = now.Add(time.Minute)
	l.Wait(ctx, 5)
	if slept != 13*time.Second {
		t.Errorf("after a quiet minute a burst waited %s", slept-13*time.Second)
	}
}
