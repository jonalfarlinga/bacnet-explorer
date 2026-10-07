package explorer

import (
	"context"
	"sort"
	"sync"
	"time"
)

// WalkConfig says how the background walk reads.
type WalkConfig struct {
	// PointsPerMinute is the budget: the walk never touches more nodes a
	// minute than this, across every device. 0 turns the walk off.
	PointsPerMinute float64
	// Again is how old a device's last walk must be before it is walked
	// again. Default a day.
	Again time.Duration
}

// WalkStatus is where the walk stands, for the admin.
type WalkStatus struct {
	Enabled         bool      `json:"enabled"`
	PointsPerMinute float64   `json:"pointsPerMinute"`
	Current         NodeID    `json:"current,omitempty"`
	CurrentLabel    string    `json:"currentLabel,omitempty"`
	Done            int       `json:"done"` // devices walked this run
	Due             int       `json:"due"`  // devices waiting their turn
	Last            time.Time `json:"last,omitempty"`
	LastError       string    `json:"lastError,omitempty"`
	// Waiting says why nothing is being read: a user's own read is in
	// progress, or no device is due.
	Waiting string `json:"waiting,omitempty"`
}

// walker reads every device's children in the background, one device at
// a time, paced by a point budget so that it never crowds the reads a
// user or the data consumers make. It stands aside while any other read is
// in progress.
type walker struct {
	cfg   WalkConfig
	limit RateLimit
	stop  context.CancelFunc
	done  chan struct{}

	mu     sync.Mutex
	status WalkStatus
	// walked is when each device was walked in this run, so that a device
	// whose walk failed is not tried again at once.
	walked map[NodeID]time.Time
}

// How long the walk waits when nothing is due, or another read is in
// progress, and how long it gives one device's read before moving on.
const (
	walkIdle      = 30 * time.Second
	walkStandBack = 5 * time.Second
	walkMaxRead   = 30 * time.Minute
	walkRetry     = 10 * time.Minute
)

// StartWalk starts the background walk. Starting it again replaces it.
func (r *Registry) StartWalk(cfg WalkConfig) {
	r.StopWalk()
	if cfg.PointsPerMinute <= 0 {
		return
	}
	if cfg.Again <= 0 {
		cfg.Again = 24 * time.Hour
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &walker{
		cfg:    cfg,
		limit:  NewRateLimit(cfg.PointsPerMinute, 12),
		stop:   cancel,
		done:   make(chan struct{}),
		status: WalkStatus{Enabled: true, PointsPerMinute: cfg.PointsPerMinute},
		walked: map[NodeID]time.Time{},
	}
	r.mu.Lock()
	r.walk = w
	r.mu.Unlock()
	go r.runWalk(ctx, w)
}

// StopWalk ends the background walk, waiting for the device being read.
func (r *Registry) StopWalk() {
	r.mu.Lock()
	w := r.walk
	r.walk = nil
	r.mu.Unlock()
	if w != nil {
		w.stop()
		<-w.done
	}
}

// WalkStatus reports where the walk stands.
func (r *Registry) WalkStatus() WalkStatus {
	r.mu.Lock()
	w := r.walk
	r.mu.Unlock()
	if w == nil {
		return WalkStatus{}
	}
	w.mu.Lock()
	st := w.status
	w.mu.Unlock()
	st.Due = len(r.due(w)) // takes the walker's lock itself
	return st
}

func (w *walker) set(change func(st *WalkStatus)) {
	w.mu.Lock()
	change(&w.status)
	w.mu.Unlock()
}

// due lists the devices the walk may read now, most deserving first: never
// walked, then longest ago. Only reached devices of open sources, since the
// rest would not answer.
func (r *Registry) due(w *walker) []Node {
	r.mu.Lock()
	defer r.mu.Unlock()
	w.mu.Lock()
	walked := make(map[NodeID]time.Time, len(w.walked))
	for id, t := range w.walked {
		walked[id] = t
	}
	w.mu.Unlock()
	now := time.Now()
	var list []Node
	for _, n := range r.nodes {
		if n.Kind != KindDevice || !n.Reached || n.Children.State == ChildrenNone || n.Children.State == ChildrenLoading {
			continue
		}
		if e := r.sources[n.Source]; e == nil || !e.open {
			continue
		}
		if t, ok := walked[n.ID]; ok && now.Sub(t) < walkRetry {
			continue
		}
		if !n.Walked.IsZero() && now.Sub(n.Walked) < w.cfg.Again {
			continue
		}
		list = append(list, n.Node)
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.Walked.IsZero() != b.Walked.IsZero() {
			return a.Walked.IsZero()
		}
		if !a.Walked.Equal(b.Walked) {
			return a.Walked.Before(b.Walked)
		}
		return a.ID < b.ID
	})
	return list
}

// reading reports whether any device is being read for someone else.
func (r *Registry) reading() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range r.nodes {
		if n.Kind == KindDevice && n.Children.State == ChildrenLoading {
			return true
		}
	}
	return false
}

func (r *Registry) runWalk(ctx context.Context, w *walker) {
	defer close(w.done)
	for {
		if ctx.Err() != nil {
			return
		}
		if r.reading() {
			w.set(func(st *WalkStatus) { st.Waiting = "a read is in progress" })
			if sleepCtx(ctx, walkStandBack) != nil {
				return
			}
			continue
		}
		due := r.due(w)
		if len(due) == 0 {
			w.set(func(st *WalkStatus) { st.Waiting = "no device is due"; st.Current = ""; st.CurrentLabel = "" })
			if sleepCtx(ctx, walkIdle) != nil {
				return
			}
			continue
		}
		dev := due[0]
		w.set(func(st *WalkStatus) { st.Waiting = ""; st.Current = dev.ID; st.CurrentLabel = dev.Label })
		err := r.walkOne(ctx, w, dev.ID)
		w.mu.Lock()
		w.walked[dev.ID] = time.Now()
		w.status.Last = time.Now()
		w.status.Current, w.status.CurrentLabel = "", ""
		if err != nil {
			w.status.LastError = dev.Label + ": " + err.Error()
		} else {
			w.status.Done++
			w.status.LastError = ""
		}
		w.mu.Unlock()
	}
}

// walkOne reads one device's children, paced, and waits for the read to
// end. On success the device's Walked time is set and kept.
func (r *Registry) walkOne(ctx context.Context, w *walker, id NodeID) error {
	if _, _, err := r.children(id, true, ChildrenOptions{Limit: w.limit}); err != nil {
		return err
	}
	deadline := time.Now().Add(walkMaxRead)
	for {
		if err := sleepCtx(ctx, 250*time.Millisecond); err != nil {
			return err
		}
		n, ok := r.Node(id)
		if !ok {
			return ErrUnknownNode
		}
		switch n.Children.State {
		case ChildrenReady:
			r.mu.Lock()
			if stored := r.nodes[id]; stored != nil {
				stored.Walked = time.Now()
				r.markDirty()
				r.publish(Event{Type: "node", Data: stored.Node})
			}
			r.mu.Unlock()
			return nil
		case ChildrenError:
			return errorOf(n)
		}
		if time.Now().After(deadline) {
			return context.DeadlineExceeded
		}
	}
}

type walkError string

func (e walkError) Error() string { return string(e) }

func errorOf(n Node) error {
	if n.Children.Error != "" {
		return walkError(n.Children.Error)
	}
	return walkError("could not be read")
}
