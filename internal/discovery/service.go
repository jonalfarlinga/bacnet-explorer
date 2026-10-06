// Package discovery keeps what the explorer knows about the BACnet network:
// the devices that have answered a Who-Is, and the objects of the devices the
// user has opened. It reads from the network on demand and publishes what it
// learns as events.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jonalfarlinga/bacnet/internal/client"
	"github.com/jonalfarlinga/bacnet/objects"
)

// Transport describes the socket in use.
type Transport struct {
	Name      string `json:"name"`
	Local     string `json:"local"`
	Broadcast string `json:"broadcast"`
	Simulated bool   `json:"simulated"`
}

// Device is a device that has announced itself, with what has been read from
// it since.
type Device struct {
	ID uint32 `json:"id"` // device instance

	// Address is the device's BACnet/IP address, or that of the router in
	// front of it when Network is not 0.
	Address string `json:"address"`
	Network uint16 `json:"network"`
	MAC     string `json:"mac,omitempty"`

	MaxAPDU      uint16 `json:"maxApdu"`
	Segmentation string `json:"segmentation"`
	VendorID     uint16 `json:"vendorId"`

	Name        string `json:"name,omitempty"`
	VendorName  string `json:"vendorName,omitempty"`
	ModelName   string `json:"modelName,omitempty"`
	Firmware    string `json:"firmware,omitempty"`
	Software    string `json:"software,omitempty"`
	Location    string `json:"location,omitempty"`
	Description string `json:"description,omitempty"`
	// InfoError is why the properties above could not be read.
	InfoError string `json:"infoError,omitempty"`

	Objects ObjectsState `json:"objects"`
}

// ObjectsState is how far the reading of a device's objects has got.
type ObjectsState struct {
	State string `json:"state"` // idle, loading, ready or error
	Total int    `json:"total"`
	Done  int    `json:"done"` // objects whose name and value have been read
	Error string `json:"error,omitempty"`
}

// Object is one object of a device.
type Object struct {
	Type     uint16 `json:"type"`
	TypeName string `json:"typeName"`
	Abbr     string `json:"abbr"`
	Instance uint32 `json:"instance"`

	Name        string   `json:"name,omitempty"`
	Description string   `json:"description,omitempty"`
	Value       string   `json:"value,omitempty"`
	Units       string   `json:"units,omitempty"`
	Flags       []string `json:"flags,omitempty"` // status flags that are set
	// Loaded is set once the properties above have been asked for.
	Loaded bool   `json:"loaded"`
	Error  string `json:"error,omitempty"`
}

// Property is one property of an object, rendered for display.
type Property struct {
	ID    uint32   `json:"id"`
	Name  string   `json:"name"`
	Index *uint32  `json:"index,omitempty"`
	Value string   `json:"value"`
	Items []string `json:"items,omitempty"` // the elements of an array or list
	Error string   `json:"error,omitempty"`
}

// Event is something the service learned. Type is "reset" (the transport
// changed: everything known before is gone), "devices" (Data is every Device
// known, sent when a subscription starts), "device" (Data is a Device) or
// "objects" (Data is an ObjectsEvent).
type Event struct {
	Type string
	Data any
}

// ObjectsEvent carries objects of one device that are new or have changed.
type ObjectsEvent struct {
	DeviceID uint32 `json:"deviceId"`
	// Rev counts the changes to the device's objects. An event applies to
	// the objects as they were at Rev-1, unless Replace is set.
	Rev     uint64   `json:"rev"`
	Replace bool     `json:"replace"` // Objects is the whole list
	Objects []Object `json:"objects"`
}

var (
	ErrNoTransport   = errors.New("no network adapter is selected")
	ErrUnknownDevice = errors.New("no such device")
)

// Service is the registry. Its methods are safe for concurrent use.
type Service struct {
	mu        sync.Mutex
	client    *client.Client
	transport *Transport
	stop      context.CancelFunc // ends the reads of the current transport
	ctx       context.Context
	devices   map[uint32]*device
	subs      map[chan Event]struct{}

	// infoSlots bounds how many devices are being identified at once.
	infoSlots chan struct{}
}

type device struct {
	dev    client.Device
	view   Device
	objs   []Object
	rev    uint64 // counts the changes to objs
	index  map[client.ObjectID]int
	cancel context.CancelFunc // ends the reading of its objects
}

func New() *Service {
	return &Service{
		devices:   map[uint32]*device{},
		subs:      map[chan Event]struct{}{},
		infoSlots: make(chan struct{}, 4),
	}
}

// Use opens a client with cfg in place of the current one, forgets every
// device, and sends a Who-Is.
func (s *Service) Use(name string, cfg client.Config, simulated bool) error {
	s.Close()

	cfg.OnIAm = s.onIAm
	c, err := client.Open(cfg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.client = c
	s.ctx, s.stop = context.WithCancel(context.Background())
	s.transport = &Transport{
		Name:      name,
		Local:     c.Local().String(),
		Broadcast: cfg.Broadcast.String(),
		Simulated: simulated,
	}
	s.publish(Event{Type: "reset"})
	s.mu.Unlock()

	return c.WhoIs()
}

// Close closes the client, if there is one.
func (s *Service) Close() {
	s.mu.Lock()
	c := s.client
	if s.stop != nil {
		s.stop()
	}
	s.client, s.transport, s.stop = nil, nil, nil
	s.devices = map[uint32]*device{}
	if c != nil {
		s.publish(Event{Type: "reset"})
	}
	s.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

// Transport returns the socket in use, or nil.
func (s *Service) Transport() *Transport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transport
}

// Scan sends a Who-Is. Devices that answer are added as their I-Ams arrive.
func (s *Service) Scan() error {
	s.mu.Lock()
	c := s.client
	s.mu.Unlock()
	if c == nil {
		return ErrNoTransport
	}
	return c.WhoIs()
}

// Devices returns every device known, ordered by instance.
func (s *Service) Devices() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.devicesLocked()
}

func (s *Service) devicesLocked() []Device {
	list := make([]Device, 0, len(s.devices))
	for _, d := range s.devices {
		list = append(list, d.view)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list
}

// Subscribe returns a channel of events and a function that ends the
// subscription. The first event is "devices", with every device known at
// that moment. A subscriber that falls too far behind has its channel closed,
// and should subscribe again.
func (s *Service) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	s.mu.Lock()
	ch <- Event{Type: "devices", Data: s.devicesLocked()}
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
		s.mu.Unlock()
	}
}

// publish sends an event to every subscriber. The caller holds s.mu.
func (s *Service) publish(e Event) {
	for ch := range s.subs {
		select {
		case ch <- e:
		default:
			delete(s.subs, ch)
			close(ch)
		}
	}
}

// ---- devices ---------------------------------------------------------------

var segmentationNames = []string{"both", "transmit only", "receive only", "none"}

// onIAm runs in the client's read loop.
func (s *Service) onIAm(dev client.Device) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return
	}

	d, known := s.devices[dev.Instance]
	if !known {
		d = &device{view: Device{ID: dev.Instance, Objects: ObjectsState{State: "idle"}}}
		s.devices[dev.Instance] = d
	}
	before := d.view
	d.dev = dev
	d.view.Address = dev.Addr.IP.String()
	d.view.Network = dev.Addr.Net
	d.view.MAC = ""
	if dev.Addr.Routed() {
		d.view.MAC = client.MACString(dev.Addr.MAC)
	}
	d.view.MaxAPDU = dev.MaxAPDU
	d.view.VendorID = dev.VendorID
	d.view.Segmentation = fmt.Sprint(dev.Segmentation)
	if int(dev.Segmentation) < len(segmentationNames) {
		d.view.Segmentation = segmentationNames[dev.Segmentation]
	}

	if !known || d.view != before {
		s.publish(Event{Type: "device", Data: d.view})
	}
	if !known {
		go s.identify(s.ctx, s.client, dev)
	}
}

// identify reads the properties that name a device.
func (s *Service) identify(ctx context.Context, c *client.Client, dev client.Device) {
	select {
	case s.infoSlots <- struct{}{}:
		defer func() { <-s.infoSlots }()
	case <-ctx.Done():
		return
	}

	props := []uint16{
		objects.PropertyIdObjectName, objects.PropertyIdVendorName, objects.PropertyIdModelName,
		objects.PropertyIdFirmwareRevision, objects.PropertyIdApplicationSoftwareVersion,
		objects.PropertyIdLocation, objects.PropertyIdDescription,
	}
	obj := client.ObjectID{Type: objects.ObjectTypeDevice, Instance: dev.Instance}
	got := map[uint32]string{}
	var failure error

	results, err := c.ReadPropertyMultiple(ctx, dev.Addr, []client.ReadSpec{{Object: obj, Props: props}})
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
		// No ReadPropertyMultiple: the name alone is worth three requests.
		for _, prop := range props[:3] {
			values, err := c.ReadProperty(ctx, dev.Addr, obj, prop)
			if err != nil && !client.Declined(err) {
				failure = err
				break
			}
			if err == nil && len(values) == 1 && values[0].Kind == client.KindString {
				got[uint32(prop)] = values[0].Str
			}
		}
	default:
		failure = err
	}
	if ctx.Err() != nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.devices[dev.Instance]
	if d == nil || s.ctx != ctx {
		return
	}
	d.view.Name = got[uint32(objects.PropertyIdObjectName)]
	d.view.VendorName = got[uint32(objects.PropertyIdVendorName)]
	d.view.ModelName = got[uint32(objects.PropertyIdModelName)]
	d.view.Firmware = got[uint32(objects.PropertyIdFirmwareRevision)]
	d.view.Software = got[uint32(objects.PropertyIdApplicationSoftwareVersion)]
	d.view.Location = got[uint32(objects.PropertyIdLocation)]
	d.view.Description = got[uint32(objects.PropertyIdDescription)]
	d.view.InfoError = ""
	if failure != nil {
		d.view.InfoError = failure.Error()
	}
	s.publish(Event{Type: "device", Data: d.view})
}

// ---- objects ---------------------------------------------------------------

// Objects returns the objects of a device as far as they have been read, and
// the revision they are at. The first call for a device starts reading them,
// as does any call with refresh set; what is read arrives as events.
func (s *Service) Objects(id uint32, refresh bool) (uint64, []Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.devices[id]
	if d == nil {
		return 0, nil, ErrUnknownDevice
	}
	if s.client == nil {
		return 0, nil, ErrNoTransport
	}

	state := d.view.Objects.State
	if state == "idle" || (refresh && state != "loading") {
		if d.cancel != nil {
			d.cancel()
		}
		ctx, cancel := context.WithCancel(s.ctx)
		d.cancel = cancel
		d.objs, d.index = nil, nil
		d.view.Objects = ObjectsState{State: "loading"}
		s.publish(Event{Type: "device", Data: d.view})
		s.publishObjects(d, true, []Object{})
		go s.enumerate(ctx, s.client, d.dev)
	}
	return d.rev, append([]Object{}, d.objs...), nil
}

// publishObjects announces a change to a device's objects. The caller holds
// s.mu and has already made the change.
func (s *Service) publishObjects(d *device, replace bool, objs []Object) {
	d.rev++
	s.publish(Event{Type: "objects", Data: ObjectsEvent{
		DeviceID: d.view.ID, Rev: d.rev, Replace: replace, Objects: objs,
	}})
}

// update changes a device under the lock, if ctx is still the reading in
// charge of it, and publishes the device.
func (s *Service) update(ctx context.Context, id uint32, change func(d *device)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.devices[id]
	if d == nil || ctx.Err() != nil {
		return false
	}
	change(d)
	s.publish(Event{Type: "device", Data: d.view})
	return true
}

func (s *Service) enumerate(ctx context.Context, c *client.Client, dev client.Device) {
	fail := func(err error) {
		s.update(ctx, dev.Instance, func(d *device) {
			d.view.Objects.State = "error"
			d.view.Objects.Error = err.Error()
		})
	}

	var lastProgress time.Time
	ids, err := c.ReadObjectList(ctx, dev, func(done, total int) {
		if time.Since(lastProgress) < 250*time.Millisecond {
			return
		}
		lastProgress = time.Now()
		s.update(ctx, dev.Instance, func(d *device) { d.view.Objects.Total = total })
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
	d.objs = make([]Object, len(ids))
	d.index = make(map[client.ObjectID]int, len(ids))
	for i, id := range ids {
		d.objs[i] = Object{
			Type: id.Type, TypeName: objectTypeName(id.Type), Abbr: objectTypeAbbr(id.Type), Instance: id.Instance,
		}
		d.index[id] = i
	}
	d.view.Objects.Total = len(ids)
	s.publish(Event{Type: "device", Data: d.view})
	s.publishObjects(d, true, append([]Object{}, d.objs...))
	s.mu.Unlock()

	r := &objectReader{s: s, c: c, ctx: ctx, dev: dev}
	batch := batchSize(dev)
	for start := 0; start < len(ids); start += batch {
		end := min(start+batch, len(ids))
		if err := r.read(ids[start:end]); err != nil {
			fail(err)
			return
		}
	}
	s.update(ctx, dev.Instance, func(d *device) { d.view.Objects.State = "ready" })
}

// batchSize is how many objects to ask about in one ReadPropertyMultiple. A
// reply carries roughly 100 bytes for each, mostly its name and description.
func batchSize(dev client.Device) int {
	n := (int(dev.MaxAPDU) - 40) / 110
	return max(1, min(n, 12))
}

// objectReader reads the name and value of a device's objects.
type objectReader struct {
	s   *Service
	c   *client.Client
	ctx context.Context
	dev client.Device
	// noRPM is set once the device has turned down a ReadPropertyMultiple for
	// a single object: the rest are read one property at a time.
	noRPM bool
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

// read reads a batch of objects and publishes them. It returns an error only
// when the device has stopped answering.
func (r *objectReader) read(ids []client.ObjectID) error {
	if r.ctx.Err() != nil {
		return r.ctx.Err()
	}
	if r.noRPM {
		for _, id := range ids {
			if err := r.readSingly(id); err != nil {
				return err
			}
		}
		return nil
	}

	specs := make([]client.ReadSpec, len(ids))
	for i, id := range ids {
		specs[i] = client.ReadSpec{Object: id, Props: objectProps(id.Type)}
	}
	results, err := r.c.ReadPropertyMultiple(r.ctx, r.dev.Addr, specs)
	switch {
	case err == nil:
		loaded := make([]Object, 0, len(results))
		for _, res := range results {
			o := Object{Loaded: true}
			for _, p := range res.Props {
				applyProperty(&o, res.Object.Type, p.ID, p.Values, p.Err)
			}
			loaded = append(loaded, r.merge(res.Object, o))
		}
		r.publish(loaded)
		return nil
	case gone(err):
		return err
	case len(ids) > 1:
		// The device declined, or sent something unreadable; most often the
		// reply would not fit. Ask for less at a time.
		half := len(ids) / 2
		if err := r.read(ids[:half]); err != nil {
			return err
		}
		return r.read(ids[half:])
	}
	if client.Declined(err) {
		r.noRPM = true
	}
	return r.readSingly(ids[0])
}

// gone reports whether err means the device is not answering, or that nobody
// is waiting for the answer any more. Any other error is the device's answer
// to one request, and the next request is worth making.
func gone(err error) bool {
	return errors.Is(err, client.ErrTimeout) || errors.Is(err, client.ErrClosed) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// readSingly reads an object with one ReadProperty for each property.
func (r *objectReader) readSingly(id client.ObjectID) error {
	o := Object{Loaded: true}
	for _, prop := range objectProps(id.Type) {
		if prop == objects.PropertyIdDescription {
			continue // not worth a request of its own
		}
		values, err := r.c.ReadProperty(r.ctx, r.dev.Addr, id, prop)
		if gone(err) {
			return err
		}
		applyProperty(&o, id.Type, uint32(prop), values, err)
	}
	r.publish([]Object{r.merge(id, o)})
	return nil
}

// applyProperty sets the field of o that a property read fills.
func applyProperty(o *Object, objType uint16, prop uint32, values []client.Value, err error) {
	if err != nil {
		if prop == uint32(objects.PropertyIdObjectName) {
			o.Error = err.Error()
		}
		return
	}
	if len(values) != 1 {
		return
	}
	v := values[0]
	switch prop {
	case uint32(objects.PropertyIdObjectName):
		o.Name = v.Str
	case uint32(objects.PropertyIdDescription):
		o.Description = v.Str
	case uint32(objects.PropertyIdPresentValue):
		if v.Kind == client.KindReal || v.Kind == client.KindDouble {
			o.Value = shortNumber(v.Float)
		} else {
			o.Value = formatValue(objType, prop, v)
		}
	case uint32(objects.PropertyIdUnits):
		if v.Kind == client.KindEnumerated {
			o.Units = unitName(v.Uint, true)
		}
	case uint32(objects.PropertyIdStatusFlags):
		if v.Kind == client.KindBits {
			o.Flags, _ = setBits(prop, v.Bits)
		}
	}
}

// merge stores what was read about an object and returns the stored object.
func (r *objectReader) merge(id client.ObjectID, o Object) Object {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	o.Type, o.TypeName, o.Abbr, o.Instance = id.Type, objectTypeName(id.Type), objectTypeAbbr(id.Type), id.Instance
	d := r.s.devices[r.dev.Instance]
	if d == nil || r.ctx.Err() != nil {
		return o
	}
	if i, ok := d.index[id]; ok {
		d.objs[i] = o
	}
	return o
}

func (r *objectReader) publish(objs []Object) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	d := r.s.devices[r.dev.Instance]
	if d == nil || r.ctx.Err() != nil {
		return
	}
	d.view.Objects.Done += len(objs)
	r.s.publishObjects(d, false, objs)
	r.s.publish(Event{Type: "device", Data: d.view})
}

// ---- properties ------------------------------------------------------------

// fallbackProps are asked for when a device will not list an object's
// properties itself. Those the object does not have are left out of the
// result.
var fallbackProps = []uint16{
	objects.PropertyIdObjectIdentifier, objects.PropertyIdObjectName, objects.PropertyIdObjectType,
	objects.PropertyIdDescription, objects.PropertyIdPresentValue, objects.PropertyIdStatusFlags,
	objects.PropertyIdEventState, objects.PropertyIdReliability, objects.PropertyIdOutOfService,
	objects.PropertyIdUnits, objects.PropertyIdPriorityArray, objects.PropertyIdRelinquishDefault,
	objects.PropertyIdPolarity, objects.PropertyIdActiveText, objects.PropertyIdInactiveText,
	objects.PropertyIdNumberOfStates, objects.PropertyIdStateText, objects.PropertyIdCovIncrement,
	objects.PropertyIdMinPresValue, objects.PropertyIdMaxPresValue, objects.PropertyIdResolution,
	objects.PropertyIdDeviceType, objects.PropertyIdVendorName, objects.PropertyIdVendorIdentifier,
	objects.PropertyIdModelName, objects.PropertyIdFirmwareRevision,
	objects.PropertyIdApplicationSoftwareVersion, objects.PropertyIdLocation,
	objects.PropertyIdProtocolVersion, objects.PropertyIdProtocolRevision,
	objects.PropertyIdSystemStatus, objects.PropertyIdMaxApduLengthAccepted,
	objects.PropertyIdSegmentationSupported, objects.PropertyIdProtocolServicesSupported,
	objects.PropertyIdProtocolObjectTypesSupported,
}

// Properties reads every property of an object from the device, now.
func (s *Service) Properties(ctx context.Context, id uint32, obj client.ObjectID) ([]Property, error) {
	s.mu.Lock()
	d, c := s.devices[id], s.client
	var dev client.Device
	if d != nil {
		dev = d.dev
	}
	s.mu.Unlock()
	if c == nil {
		return nil, ErrNoTransport
	}
	if d == nil {
		return nil, ErrUnknownDevice
	}

	// Property "all" in one request, where the device will do that.
	results, err := c.ReadPropertyMultiple(ctx, dev.Addr, []client.ReadSpec{
		{Object: obj, Props: []uint16{objects.PropertyIdAll}},
	})
	if err == nil && len(results) == 1 && !allFailed(results[0].Props) {
		return renderProperties(obj.Type, results[0].Props, false), nil
	}
	if gone(err) {
		return nil, err
	}

	// Otherwise the usual ones, together if it has ReadPropertyMultiple.
	results, err = c.ReadPropertyMultiple(ctx, dev.Addr, []client.ReadSpec{{Object: obj, Props: fallbackProps}})
	if err == nil && len(results) == 1 {
		return renderProperties(obj.Type, results[0].Props, true), nil
	}
	if gone(err) {
		return nil, err
	}

	var props []client.PropertyResult
	for _, prop := range fallbackProps {
		values, err := c.ReadProperty(ctx, dev.Addr, obj, prop)
		if gone(err) {
			return nil, err
		}
		props = append(props, client.PropertyResult{ID: uint32(prop), Values: values, Err: err})
	}
	return renderProperties(obj.Type, props, true), nil
}

func allFailed(props []client.PropertyResult) bool {
	for _, p := range props {
		if p.Err == nil {
			return false
		}
	}
	return true
}

// renderProperties formats what was read. With guessed set, the properties
// were a guess at what the object has, so those it turned down are dropped.
func renderProperties(objType uint16, props []client.PropertyResult, guessed bool) []Property {
	list := make([]Property, 0, len(props))
	for _, p := range props {
		out := Property{ID: p.ID, Name: propertyName(p.ID), Index: p.Index}
		if p.Err != nil {
			if guessed {
				continue
			}
			out.Error = p.Err.Error()
		} else {
			out.Value, out.Items = formatProperty(objType, p.ID, p.Values)
		}
		list = append(list, out)
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}
