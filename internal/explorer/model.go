// Package explorer is the core of the explorer: the model of what is shown,
// the sources that fill it, and the registry that holds what they have
// found. It knows no protocol. A source owns everything protocol-shaped:
// how it is configured, how it finds and reads things, how it renders a
// value, and what its node IDs mean.
package explorer

import (
	"context"
	"errors"
	"time"
)

// NodeID identifies a node. It is "<source kind>:<rest>", opaque beyond the
// prefix, and stable: it is what a URL, a selection and a stored sample
// refer to.
type NodeID string

// Kind says what a node is.
type Kind string

const (
	// KindDevice is a thing with an address that can be read: a controller,
	// a meter, a gateway.
	KindDevice Kind = "device"
	// KindGroup is a container the source uses to arrange the tree: a
	// network behind a router, a register block.
	KindGroup Kind = "group"
	// KindPoint is a value that can be read, and historized.
	KindPoint Kind = "point"
)

// Family is the broad type of a point's value, which decides how it is drawn
// and charted.
type Family string

const (
	FamilyAnalog     Family = "analog"
	FamilyBinary     Family = "binary"
	FamilyMultiState Family = "multistate"
	FamilyText       Family = "text"
	FamilyOther      Family = "other"
)

// Quality says how much a reading is worth.
type Quality string

const (
	QualityGood     Quality = "good"
	QualityStale    Quality = "stale"
	QualityCommFail Quality = "comm-fail"
	QualityError    Quality = "error"
)

// Node is anything shown in the tree.
type Node struct {
	ID     NodeID `json:"id"`
	Parent NodeID `json:"parent,omitempty"` // "" for a root of the source
	Source string `json:"source"`           // the kind of the source it came from
	Kind   Kind   `json:"kind"`

	Label    string `json:"label"`
	Sublabel string `json:"sublabel,omitempty"` // beside the label, small: an instance, a count
	Meta     string `json:"meta,omitempty"`     // at the right edge, small: an address
	Tag      string `json:"tag,omitempty"`      // the chip's text: AI, DEV, HR
	Family   Family `json:"family,omitempty"`
	// Group arranges siblings: those that share a Group are shown under one
	// heading of that name.
	Group string `json:"group,omitempty"`
	// Order sorts siblings; ties go by label.
	Order int `json:"order"`

	// Facts are what the detail pane shows about the node, in order.
	Facts []Fact `json:"facts,omitempty"`
	// Reading is the node's current value, for a point.
	Reading *Reading `json:"reading,omitempty"`
	// Children says whether the node has children and how far their reading
	// has got.
	Children ChildrenState `json:"children"`
	// Listed is set by the registry on a node that came in a children list,
	// read on demand, as opposed to one the source announced on its own. A
	// list that replaces a node's children replaces only the listed ones.
	Listed bool `json:"listed,omitempty"`
	// Pending is set while the node's own description is still being read:
	// its label may yet change.
	Pending bool `json:"pending,omitempty"`
	// Error is why the node itself could not be read.
	Error string `json:"error,omitempty"`
}

// Fact is one line of the detail pane.
type Fact struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Mono  bool   `json:"mono,omitempty"` // an address, a number: fixed-width
}

// Reading is a point's value as the source rendered it, with what the
// historian stores beside it.
type Reading struct {
	Text   string   `json:"text"`
	Number *float64 `json:"number,omitempty"`
	Units  string   `json:"units,omitempty"`
	// Flags are quality words the source chose: in alarm, fault, overridden.
	Flags   []string  `json:"flags,omitempty"`
	Quality Quality   `json:"quality"`
	At      time.Time `json:"at"`
}

// ChildrenState is how far the reading of a node's children has got.
type ChildrenState struct {
	// State is none (the node has no children by nature), unknown (not read
	// yet), loading, ready or error.
	State string `json:"state"`
	Total int    `json:"total"`
	Done  int    `json:"done"` // children whose description has been read
	Error string `json:"error,omitempty"`
}

const (
	ChildrenNone    = "none"
	ChildrenUnknown = "unknown"
	ChildrenLoading = "loading"
	ChildrenReady   = "ready"
	ChildrenError   = "error"
)

// Detail is everything behind one node, read now: the properties of an
// object, the registers of a unit.
type Detail struct {
	Rows   []DetailRow `json:"rows"`
	ReadAt time.Time   `json:"readAt"`
}

// DetailRow is one line of a Detail.
type DetailRow struct {
	Name  string   `json:"name"`
	Value string   `json:"value"`
	Items []string `json:"items,omitempty"` // the elements of a list, when there are several
	Error string   `json:"error,omitempty"`
}

// Field is one input of a form the UI draws for a source: a setting, or a
// line of the add-by-hand form.
type Field struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Hint     string   `json:"hint,omitempty"`
	Type     string   `json:"type"` // text, number, select
	Options  []Option `json:"options,omitempty"`
	Required bool     `json:"required,omitempty"`
	Default  string   `json:"default,omitempty"`
}

// Option is one choice of a select field.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Hint  string `json:"hint,omitempty"`
}

// Values is a filled-in form.
type Values map[string]string

// Description says what a source is and how it is configured, so that the
// UI can draw its forms without knowing the protocol.
type Description struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Blurb string `json:"blurb,omitempty"` // a sentence on what opening it does
	// Settings are what Open takes. Empty when there is nothing to choose.
	Settings []Field `json:"settings"`
	// AddForm is what Add takes. Empty when nodes cannot be added by hand.
	AddForm []Field `json:"addForm"`
	// CanDiscover says the source can look for devices on its own.
	CanDiscover  bool   `json:"canDiscover"`
	DiscoverHint string `json:"discoverHint,omitempty"`
}

// Status is the state of an open source.
type Status struct {
	Open    bool   `json:"open"`
	Summary string `json:"summary,omitempty"`
	Lines   []Fact `json:"lines,omitempty"`
}

// Event is something a source learned, or the registry passing it on.
type Event struct {
	// Type is one of: node (Data is a Node), children (Data is a
	// ChildrenEvent), remove (Data is the NodeID of a node that is gone,
	// with everything under it), reset (the source started over:
	// everything it had found is gone), source (Data is a SourceInfo; its
	// status changed), and from the registry to a subscriber on
	// subscribing, nodes (Data is every Node known).
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
}

// ChildrenEvent carries some or all of the children of a node. Each one
// takes the parent's children to a revision; a subscriber that sees a
// revision it did not expect has missed one and fetches the list again.
type ChildrenEvent struct {
	Parent NodeID `json:"parent"`
	Rev    uint64 `json:"rev"`
	// Replace means Nodes is the whole list: anything under Parent before
	// is gone.
	Replace bool   `json:"replace"`
	Nodes   []Node `json:"nodes"`
}

// SourceInfo is a registered source as the API shows it.
type SourceInfo struct {
	Description
	Status Status `json:"status"`
}

// Source is a protocol, or anything else nodes come from.
type Source interface {
	// Describe says how the source is configured. It may be called before
	// Open, and its options may be recomputed on each call.
	Describe() Description
	// Open starts the source with the settings the user gave. Opening an
	// open source starts it over.
	Open(ctx context.Context, settings Values) error
	// Close stops it. What it had found is gone.
	Close() error

	// Discover looks for devices on its own. A source that cannot returns
	// ErrNotSupported.
	Discover(ctx context.Context) error
	// Add lists a node from a filled-in form, for one that Discover does
	// not reach.
	Add(ctx context.Context, form Values) (Node, error)

	// Children starts reading the nodes under a node. They arrive as
	// children events, and the node's own Children state as node events,
	// until the state is ready or error. Reading again replaces the list.
	Children(ctx context.Context, id NodeID) error
	// Detail reads everything behind one node, now.
	Detail(ctx context.Context, id NodeID) (Detail, error)
	// Read reads the current values of several points in one go. The
	// readings come back in the order of the IDs; a point that could not be
	// read has a Quality that says so.
	Read(ctx context.Context, ids []NodeID) ([]Reading, error)

	// Events delivers what the source learns. The channel is never closed.
	Events() <-chan Event
	Status() Status
}

var (
	ErrNotSupported = errors.New("the source does not support this")
	ErrUnknownNode  = errors.New("no such node")
	ErrUnknownKind  = errors.New("no such source")
	ErrNotOpen      = errors.New("the source is not open")
)
