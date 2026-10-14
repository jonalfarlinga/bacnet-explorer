package bacnet

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jonalfarlinga/bacnet/client"
	"github.com/jonalfarlinga/bacnet-explorer/internal/explorer"
	"github.com/jonalfarlinga/bacnet/objects"
)

// object is what is known about one object of a device.
type object struct {
	id     client.ObjectID
	loaded bool // its name and value have been asked for
	name   string
	descr  string
	value  string
	number *float64
	units  string
	flags  []string
	err    string
}

// Children implements explorer.Source: for a device, it reads the object
// list and then the name and value of each object, publishing them as it
// goes. Networks and routers have nothing to read: their children are the
// devices that announced themselves. With a limit, each object read, and
// each element of an object list read one at a time, waits its turn.
func (s *Source) Children(ctx context.Context, id explorer.NodeID, opts explorer.ChildrenOptions) error {
	instance, obj, ok := parseID(id)
	if !ok {
		return nil
	}
	if obj != nil {
		return explorer.ErrNotSupported
	}

	s.mu.Lock()
	d, c := s.devices[instance], s.c
	if d == nil || c == nil {
		s.mu.Unlock()
		if c == nil {
			return explorer.ErrNotOpen
		}
		return explorer.ErrUnknownNode
	}
	if d.cancel != nil {
		d.cancel()
	}
	rctx, cancel := context.WithCancel(ctx)
	d.cancel = cancel
	d.objs, d.index = nil, nil
	d.children = explorer.ChildrenState{State: explorer.ChildrenLoading}
	dev := d.dev
	n := s.deviceNode(instance, d)
	s.mu.Unlock()

	s.emit(explorer.Event{Type: "node", Data: n})
	s.emit(explorer.Event{Type: "children", Data: explorer.ChildrenEvent{Parent: id, Replace: true, Nodes: []explorer.Node{}}})
	go s.enumerate(rctx, c, dev, opts.Limit)
	return nil
}

// update changes a device under the lock, if ctx is still the reading in
// charge of it, and publishes its node.
func (s *Source) update(ctx context.Context, instance uint32, change func(d *device)) bool {
	s.mu.Lock()
	d := s.devices[instance]
	if d == nil || ctx.Err() != nil {
		s.mu.Unlock()
		return false
	}
	change(d)
	n := s.deviceNode(instance, d)
	s.mu.Unlock()
	s.emit(explorer.Event{Type: "node", Data: n})
	return true
}

func (s *Source) enumerate(ctx context.Context, c *client.Client, dev client.Device, limit explorer.RateLimit) {
	fail := func(err error) {
		s.update(ctx, dev.Instance, func(d *device) {
			d.children.State = explorer.ChildrenError
			d.children.Error = err.Error()
		})
	}
	wait := func(n int) error {
		if limit == nil {
			return nil
		}
		return limit.Wait(ctx, n)
	}

	var lastProgress time.Time
	if err := wait(1); err != nil {
		fail(err)
		return
	}
	ids, err := c.ReadObjectList(ctx, dev, func(done, total int) {
		// An object list read one element at a time is paced like the
		// objects themselves; the callback runs in the reading loop.
		wait(1)
		if time.Since(lastProgress) < 250*time.Millisecond {
			return
		}
		lastProgress = time.Now()
		s.update(ctx, dev.Instance, func(d *device) { d.children.Total = total })
	})
	if err != nil {
		fail(fmt.Errorf("reading the object list: %w", err))
		return
	}
	sort.Slice(ids, func(i, j int) bool {
		if ids[i].Type != ids[j].Type {
			return ids[i].Type < ids[j].Type
		}
		return ids[i].Instance < ids[j].Instance
	})

	s.mu.Lock()
	d := s.devices[dev.Instance]
	if d == nil || ctx.Err() != nil {
		s.mu.Unlock()
		return
	}
	d.objs = make([]object, len(ids))
	d.index = make(map[client.ObjectID]int, len(ids))
	nodes := make([]explorer.Node, len(ids))
	for i, id := range ids {
		d.objs[i] = object{id: id}
		d.index[id] = i
		nodes[i] = s.objectNode(dev.Instance, i, d.objs[i])
	}
	d.children.Total = len(ids)
	n := s.deviceNode(dev.Instance, d)
	s.mu.Unlock()
	s.emit(explorer.Event{Type: "node", Data: n})
	s.emit(explorer.Event{Type: "children", Data: explorer.ChildrenEvent{Parent: devID(dev.Instance), Replace: true, Nodes: nodes}})

	r := &objectReader{s: s, c: c, ctx: ctx, dev: dev}
	batch := batchSize(dev)
	for start := 0; start < len(ids); start += batch {
		end := min(start+batch, len(ids))
		if err := wait(end - start); err != nil {
			fail(err)
			return
		}
		if err := r.read(ids[start:end]); err != nil {
			fail(err)
			return
		}
	}
	s.update(ctx, dev.Instance, func(d *device) { d.children.State = explorer.ChildrenReady })
}

// batchSize is how many objects to ask about in one ReadPropertyMultiple. A
// reply carries roughly 100 bytes for each, mostly its name and description.
func batchSize(dev client.Device) int {
	n := (int(dev.MaxAPDU) - 40) / 110
	return max(1, min(n, 12))
}

// objectReader reads the name and value of a device's objects.
type objectReader struct {
	s   *Source
	c   *client.Client
	ctx context.Context
	dev client.Device
}

func objectProps(t uint16) []uint16 {
	props := []uint16{objects.PropertyIdObjectName, objects.PropertyIdDescription}
	if hasPresentValue(t) {
		props = append(props, objects.PropertyIdPresentValue, objects.PropertyIdStatusFlags)
	}
	if hasUnits(t) {
		props = append(props, objects.PropertyIdUnits)
	}
	return props
}

// read reads a batch of objects and publishes them. The client sizes the
// requests to the device and falls back to one property at a time where it
// has to, so an error here means the device has stopped answering.
func (r *objectReader) read(ids []client.ObjectID) error {
	if r.ctx.Err() != nil {
		return r.ctx.Err()
	}
	specs := make([]client.ReadSpec, len(ids))
	for i, id := range ids {
		specs[i] = client.ReadSpec{Object: id, Props: objectProps(id.Type)}
	}
	results, err := r.c.ReadMany(r.ctx, r.dev, specs)
	if err != nil {
		return err
	}
	loaded := make([]object, 0, len(results))
	for _, res := range results {
		o := object{id: res.Object, loaded: true}
		for _, p := range res.Props {
			applyProperty(&o, p.ID, p.Values, p.Err)
		}
		loaded = append(loaded, o)
	}
	r.publish(loaded)
	return nil
}

// gone reports whether err means the device is not answering, or that nobody
// is waiting for the answer any more. Any other error is the device's answer
// to one request, and the next request is worth making.
func gone(err error) bool {
	return errors.Is(err, client.ErrTimeout) || errors.Is(err, client.ErrClosed) || errors.Is(err, client.ErrSourceDenied) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// applyProperty sets the field of o that a property read fills.
func applyProperty(o *object, prop uint32, values []client.Value, err error) {
	if err != nil {
		if prop == uint32(objects.PropertyIdObjectName) {
			o.err = err.Error()
		}
		return
	}
	if len(values) != 1 {
		return
	}
	v := values[0]
	switch prop {
	case uint32(objects.PropertyIdObjectName):
		o.name = v.Str
	case uint32(objects.PropertyIdDescription):
		o.descr = v.Str
	case uint32(objects.PropertyIdPresentValue):
		if v.Kind == client.KindReal || v.Kind == client.KindDouble {
			o.value = shortNumber(v.Float)
		} else {
			o.value = formatValue(o.id.Type, prop, v)
		}
		o.number = numberOf(v)
	case uint32(objects.PropertyIdUnits):
		if v.Kind == client.KindEnumerated {
			o.units = unitName(v.Uint, true)
		}
	case uint32(objects.PropertyIdStatusFlags):
		if v.Kind == client.KindBits {
			o.flags, _ = setBits(prop, v.Bits)
		}
	}
}

// numberOf is a value as a number, for the kinds that are one.
func numberOf(v client.Value) *float64 {
	var f float64
	switch v.Kind {
	case client.KindReal, client.KindDouble:
		f = v.Float
	case client.KindUnsigned, client.KindEnumerated:
		f = float64(v.Uint)
	case client.KindSigned:
		f = float64(v.Int)
	case client.KindBoolean:
		if v.Bool {
			f = 1
		}
	default:
		return nil
	}
	return &f
}

// publish stores what was read about some objects and publishes their
// nodes.
func (r *objectReader) publish(objs []object) {
	r.s.mu.Lock()
	d := r.s.devices[r.dev.Instance]
	if d == nil || r.ctx.Err() != nil {
		r.s.mu.Unlock()
		return
	}
	nodes := make([]explorer.Node, 0, len(objs))
	for _, o := range objs {
		i, ok := d.index[o.id]
		if !ok {
			continue
		}
		d.objs[i] = o
		nodes = append(nodes, r.s.objectNode(r.dev.Instance, i, o))
	}
	d.children.Done += len(nodes)
	n := r.s.deviceNode(r.dev.Instance, d)
	r.s.mu.Unlock()
	r.s.emit(explorer.Event{Type: "children", Data: explorer.ChildrenEvent{Parent: devID(r.dev.Instance), Nodes: nodes}})
	r.s.emit(explorer.Event{Type: "node", Data: n})
}

// objectNode renders an object as a point.
func (s *Source) objectNode(instance uint32, order int, o object) explorer.Node {
	typ := o.id.Type
	typeName := objectTypeName(typ)
	n := explorer.Node{
		ID:       objID(instance, o.id),
		Parent:   devID(instance),
		Kind:     explorer.KindPoint,
		Label:    o.name,
		Sublabel: fmt.Sprint(o.id.Instance),
		Tag:      objectTypeAbbr(typ),
		Family:   family(typ),
		Group:    titleCase(typeName),
		Order:    order,
		Children: explorer.ChildrenState{State: explorer.ChildrenNone},
		Pending:  !o.loaded,
		Error:    o.err,
		Facts: nonEmpty([]explorer.Fact{
			{Label: "Type", Value: fmt.Sprintf("%s (%d)", typeName, typ)},
			{Label: "Description", Value: o.descr},
		}),
	}
	if n.Label == "" {
		n.Label = typeName
	}
	if o.value != "" {
		n.Reading = &explorer.Reading{
			Text: o.value, Number: o.number, Units: o.units, Flags: o.flags,
			Quality: explorer.QualityGood, At: time.Now(),
		}
	}
	return n
}

// family is the broad kind of an object type's value.
func family(t uint16) explorer.Family {
	switch t {
	case objects.ObjectTypeAnalogInput, objects.ObjectTypeAnalogOutput, objects.ObjectTypeAnalogValue,
		objects.ObjectTypeAccumulator, objects.ObjectTypePulseConverter, objects.ObjectTypeLoop, 45, 46, 48:
		return explorer.FamilyAnalog
	case objects.ObjectTypeBinaryInput, objects.ObjectTypeBinaryOutput, objects.ObjectTypeBinaryValue:
		return explorer.FamilyBinary
	case objects.ObjectTypeMultiStateInput, objects.ObjectTypeMultiStateOutput, objects.ObjectTypeMultiStateValue:
		return explorer.FamilyMultiState
	case 40:
		return explorer.FamilyText
	}
	return explorer.FamilyOther
}

// titleCase turns "analog-input" into "Analog Input".
func titleCase(name string) string {
	words := strings.Split(name, "-")
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}
