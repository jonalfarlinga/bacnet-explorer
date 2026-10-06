// Package sim runs simulated BACnet/IP devices on the loopback interface, for
// trying the explorer and testing its client without a network. The devices
// answer Who-Is, ReadProperty and ReadPropertyMultiple, segment replies that
// are too long for one APDU, and include a router with devices behind it, a
// device that cannot segment and one without ReadPropertyMultiple.
package sim

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"sync"

	"github.com/jonalfarlinga/bacnet"
	"github.com/jonalfarlinga/bacnet/objects"
	"github.com/jonalfarlinga/bacnet/plumbing"
	"github.com/jonalfarlinga/bacnet/services"
)

const wildcardInstance = 4194303

// Segmentation values of a device.
const (
	SegBoth = 0
	SegNone = 3
)

// Device is one simulated device.
type Device struct {
	Instance     uint32
	Name         string
	Model        string
	Description  string
	MaxAPDU      uint16
	Segmentation uint8
	// NoRPM makes the device reject ReadPropertyMultiple.
	NoRPM bool
	// Net and MAC place the device behind the router that is its station's
	// first device. Net is 0 for a device with a BACnet/IP address of its own.
	Net uint16
	MAC []byte

	objs []*object
}

type object struct {
	id    objects.ObjectIdentifier
	props []property
}

type property struct {
	id uint16
	// array marks a property whose elements can be read by index.
	array bool
	get   func() []*objects.Object
}

// Network is a set of running stations. Each station is one UDP socket: a
// device, and the devices routed through it if it is a router.
type Network struct {
	stations []*station
	wg       sync.WaitGroup
}

type station struct {
	conn    *net.UDPConn
	devices []*Device

	mu sync.Mutex
	// sending holds the segments of replies that are still going out.
	sending map[txKey]*segmented
}

type txKey struct {
	peer   netip.AddrPort
	invoke uint8
}

type segmented struct {
	dev      *Device
	service  uint8
	segments [][]byte
}

// segmentWindow is the window size the devices propose.
const segmentWindow = 4

// Start runs the default set of devices.
func Start() (*Network, error) {
	return StartDevices(defaultStations())
}

// StartDevices runs the given stations: each is the devices that share one
// socket, the first being the one at the socket's own address.
func StartDevices(stations [][]*Device) (*Network, error) {
	n := &Network{}
	for _, devices := range stations {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			n.Close()
			return nil, err
		}
		n.stations = append(n.stations, &station{conn: conn, devices: devices, sending: map[txKey]*segmented{}})
	}
	for _, st := range n.stations {
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			n.serve(st)
		}()
	}
	return n, nil
}

// Addr is where a Who-Is reaches every device: the simulated network has no
// broadcast address, so the first station passes a Who-Is on to the others.
func (n *Network) Addr() netip.AddrPort {
	return n.stations[0].addr()
}

// Close stops the devices.
func (n *Network) Close() {
	for _, st := range n.stations {
		st.conn.Close()
	}
	n.wg.Wait()
}

func (st *station) addr() netip.AddrPort {
	return st.conn.LocalAddr().(*net.UDPAddr).AddrPort()
}

func (n *Network) serve(st *station) {
	buf := make([]byte, 2048)
	for {
		size, src, err := st.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		msg, err := bacnet.Parse(buf[:size])
		if err != nil {
			continue
		}
		switch m := msg.(type) {
		case *services.UnconfirmedWhoIs:
			for _, other := range n.stations {
				other.iAm(src, m.NPDU)
			}
		case *services.ConfirmedReadProperty:
			if dev := st.target(m.NPDU); dev != nil {
				st.read(src, dev, m.APDU)
			}
		case *services.SegmentACK:
			st.continueReply(src, m.APDU)
		}
	}
}

// target finds the device a request is addressed to.
func (st *station) target(npdu *plumbing.NPDU) *Device {
	if npdu.Control&0x20 == 0 {
		return st.devices[0]
	}
	for _, d := range st.devices {
		if d.Net == npdu.DNET && bytes.Equal(d.MAC, npdu.DADR) {
			return d
		}
	}
	return nil
}

// iAm answers a Who-Is for each device of the station it reaches.
func (st *station) iAm(to netip.AddrPort, npdu *plumbing.NPDU) {
	for _, d := range st.devices {
		if npdu.Control&0x20 != 0 && npdu.DNET != 0xFFFF && npdu.DNET != d.Net {
			continue
		}
		if npdu.Control&0x20 == 0 && d.Net != 0 {
			continue // a local Who-Is does not cross the router
		}
		st.send(to, d, plumbing.NewAPDU(plumbing.UnConfirmedReq, services.ServiceUnconfirmedIAm,
			services.IAmObjects(d.Instance, d.MaxAPDU, d.Segmentation, vendorID)))
	}
}

// send sends one APDU from a device.
func (st *station) send(to netip.AddrPort, from *Device, a *plumbing.APDU) {
	bvlc := plumbing.NewBVLC(plumbing.BVLCFuncUnicast)
	npdu := plumbing.NewNPDU(false, false, from.Net != 0, false)
	if from.Net != 0 {
		npdu.SNET = from.Net
		npdu.SLEN = uint8(len(from.MAC))
		npdu.SADR = from.MAC
	}
	b := make([]byte, bvlc.MarshalLen()+npdu.MarshalLen()+a.MarshalLen())
	bvlc.Length = uint16(len(b))
	bvlc.MarshalTo(b)
	npdu.MarshalTo(b[bvlc.MarshalLen():])
	if err := a.MarshalTo(b[bvlc.MarshalLen()+npdu.MarshalLen():]); err != nil {
		return
	}
	st.conn.WriteToUDPAddrPort(b, to)
}

// ---- reads -----------------------------------------------------------------

// Sizes of the max-APDU codes of a confirmed request.
var maxAPDUSizes = []int{50, 128, 206, 480, 1024, 1476}

func (st *station) read(to netip.AddrPort, dev *Device, req *plumbing.APDU) {
	tags := make([]*objects.Object, 0, len(req.Objects))
	for _, p := range req.Objects {
		if t, ok := p.(*objects.Object); ok {
			tags = append(tags, t)
		}
	}

	var (
		reply []*objects.Object
		fault *plumbing.APDU
	)
	switch {
	case req.Service == services.ServiceConfirmedReadPropMultiple && dev.NoRPM:
		fault = &plumbing.APDU{Type: plumbing.Reject, Reason: objects.RejectReasonUnrecognizedService}
	case req.Service == services.ServiceConfirmedReadPropMultiple:
		reply = dev.readMultiple(tags)
	default:
		var class, code uint8
		if reply, class, code = dev.readOne(tags); reply == nil {
			fault = plumbing.NewAPDU(plumbing.Error, req.Service, services.ErrorObjects(class, code))
		}
	}
	if reply == nil && fault == nil {
		fault = &plumbing.APDU{Type: plumbing.Reject, Reason: objects.RejectReasonMissingRequiredParameter}
	}
	if fault != nil {
		fault.InvokeID = req.InvokeID
		st.send(to, dev, fault)
		return
	}

	var payload []byte
	for _, t := range reply {
		b, err := t.MarshalBinary()
		if err != nil {
			return
		}
		payload = append(payload, b...)
	}

	limit := int(dev.MaxAPDU)
	if int(req.MaxSize) < len(maxAPDUSizes) {
		limit = min(limit, maxAPDUSizes[req.MaxSize])
	}
	if 3+len(payload) <= limit {
		ack := plumbing.NewAPDU(plumbing.ComplexAck, req.Service, nil)
		ack.InvokeID = req.InvokeID
		for _, t := range reply {
			ack.Objects = append(ack.Objects, t)
		}
		st.send(to, dev, ack)
		return
	}
	if dev.Segmentation != SegBoth || !req.SegmentedResponseAccepted() {
		st.send(to, dev, &plumbing.APDU{
			Type: plumbing.Abort, Flags: 0x1, InvokeID: req.InvokeID,
			Reason: objects.AbortReasonSegmentationNotSupported,
		})
		return
	}

	// The first segment goes out alone; the rest follow the acknowledgements.
	tx := &segmented{dev: dev, service: req.Service}
	for chunk := limit - 5; len(payload) > 0; {
		n := min(chunk, len(payload))
		tx.segments = append(tx.segments, payload[:n])
		payload = payload[n:]
	}
	key := txKey{peer: to, invoke: req.InvokeID}
	st.mu.Lock()
	st.sending[key] = tx
	st.mu.Unlock()
	st.sendSegments(key, tx, 0, 1)
}

func (st *station) sendSegments(key txKey, tx *segmented, from, count int) {
	for i := from; i < from+count && i < len(tx.segments); i++ {
		seg := &plumbing.APDU{
			Type:               plumbing.ComplexAck,
			Flags:              0x8, // segmented
			InvokeID:           key.invoke,
			SequenceNumber:     uint8(i),
			ProposedWindowSize: segmentWindow,
			Service:            tx.service,
			Payload:            tx.segments[i],
		}
		if i < len(tx.segments)-1 {
			seg.Flags |= 0x4 // more follow
		}
		st.send(key.peer, tx.dev, seg)
	}
}

// continueReply sends the segments that follow the one a Segment-ACK
// acknowledges.
func (st *station) continueReply(from netip.AddrPort, ack *plumbing.APDU) {
	key := txKey{peer: from, invoke: ack.InvokeID}
	st.mu.Lock()
	tx := st.sending[key]
	next := int(ack.SequenceNumber) + 1
	if tx != nil && next >= len(tx.segments) {
		delete(st.sending, key)
		tx = nil
	}
	st.mu.Unlock()
	if tx != nil {
		st.sendSegments(key, tx, next, segmentWindow)
	}
}

func (d *Device) find(id objects.ObjectIdentifier) *object {
	if id.ObjectType == objects.ObjectTypeDevice && id.InstanceNumber == wildcardInstance {
		id.InstanceNumber = d.Instance
	}
	for _, o := range d.objs {
		if o.id == id {
			return o
		}
	}
	return nil
}

func (o *object) find(id uint16) *property {
	for i := range o.props {
		if o.props[i].id == id {
			return &o.props[i]
		}
	}
	return nil
}

// value returns a property's value, or one element of it. A nil result comes
// with the error to report.
func (p *property) value(index *uint32) (tags []*objects.Object, class, code uint8) {
	all := p.get()
	switch {
	case index == nil:
		return all, 0, 0
	case !p.array:
		return nil, objects.ErrorClassProperty, 50 // property is not an array
	case *index == 0:
		return []*objects.Object{objects.EncUnsignedInteger(uint(len(all)))}, 0, 0
	case int(*index) > len(all):
		return nil, objects.ErrorClassProperty, 42 // invalid array index
	}
	return all[*index-1 : *index], 0, 0
}

func uintOf(t *objects.Object) uint32 {
	var v uint32
	for _, b := range t.Data {
		v = v<<8 | uint32(b)
	}
	return v
}

func context(t *objects.Object, number uint8) bool {
	return t.TagClass && t.Kind == objects.TagPrimitive && t.TagNumber == number
}

// readOne answers a ReadProperty: object identifier [0], property [1] and
// perhaps an array index [2].
func (d *Device) readOne(req []*objects.Object) (reply []*objects.Object, class, code uint8) {
	if len(req) < 2 || !context(req[0], 0) || len(req[0].Data) != 4 || !context(req[1], 1) {
		return nil, objects.ErrorClassServices, 9 // inconsistent parameters
	}
	id, _ := objects.DecObjectIdentifier(req[0])
	propID := uintOf(req[1])
	var index *uint32
	if len(req) > 2 && context(req[2], 2) {
		i := uintOf(req[2])
		index = &i
	}

	o := d.find(id)
	if o == nil {
		return nil, objects.ErrorClassObject, objects.ErrorCodeUnknownObject
	}
	var p *property
	if propID <= 0xFFFF {
		p = o.find(uint16(propID))
	}
	if p == nil {
		return nil, objects.ErrorClassProperty, 32 // unknown property
	}
	value, class, code := p.value(index)
	if value == nil {
		return nil, class, code
	}

	reply = append(reply,
		objects.EncObjectIdentifier(true, 0, o.id.ObjectType, o.id.InstanceNumber),
		objects.ContextTag(1, objects.EncUnsignedInteger(uint(propID))))
	if index != nil {
		reply = append(reply, objects.ContextTag(2, objects.EncUnsignedInteger(uint(*index))))
	}
	reply = append(reply, objects.EncOpeningTag(3))
	reply = append(reply, value...)
	return append(reply, objects.EncClosingTag(3)), 0, 0
}

// readMultiple answers a ReadPropertyMultiple: for each object its identifier
// [0] and, between tags 1, properties [0] with perhaps an array index [1].
// It returns nil for a request it cannot make sense of.
func (d *Device) readMultiple(req []*objects.Object) []*objects.Object {
	var reply []*objects.Object
	for len(req) > 0 {
		if len(req) < 3 || !context(req[0], 0) || len(req[0].Data) != 4 ||
			req[1].Kind != objects.TagOpening || req[1].TagNumber != 1 {
			return nil
		}
		id, _ := objects.DecObjectIdentifier(req[0])
		o := d.find(id)
		req = req[2:]

		reply = append(reply,
			objects.EncObjectIdentifier(true, 0, id.ObjectType, id.InstanceNumber), objects.EncOpeningTag(1))
		for {
			if len(req) == 0 {
				return nil
			}
			if req[0].Kind == objects.TagClosing {
				req = req[1:]
				break
			}
			if !context(req[0], 0) {
				return nil
			}
			propID := uintOf(req[0])
			req = req[1:]
			var index *uint32
			if len(req) > 0 && context(req[0], 1) {
				i := uintOf(req[0])
				index = &i
				req = req[1:]
			}

			if o != nil && propID == uint32(objects.PropertyIdAll) {
				for i := range o.props {
					reply = appendResult(reply, uint32(o.props[i].id), nil, o.props[i].get(), 0, 0)
				}
				continue
			}
			var (
				value       []*objects.Object
				class, code = objects.ErrorClassObject, objects.ErrorCodeUnknownObject
			)
			if o != nil {
				class, code = objects.ErrorClassProperty, 32 // unknown property
				if p := o.find(uint16(propID)); p != nil && propID <= 0xFFFF {
					value, class, code = p.value(index)
				}
			}
			reply = appendResult(reply, propID, index, value, class, code)
		}
		reply = append(reply, objects.EncClosingTag(1))
	}
	return reply
}

// appendResult adds one property of a ReadPropertyMultiple ack: its value
// between tags 4, or the error between tags 5 when value is nil.
func appendResult(reply []*objects.Object, propID uint32, index *uint32, value []*objects.Object, class, code uint8) []*objects.Object {
	reply = append(reply, objects.ContextTag(2, objects.EncUnsignedInteger(uint(propID))))
	if index != nil {
		reply = append(reply, objects.ContextTag(3, objects.EncUnsignedInteger(uint(*index))))
	}
	if value == nil {
		return append(reply, objects.EncOpeningTag(5),
			objects.EncEnumerated(uint32(class)), objects.EncEnumerated(uint32(code)), objects.EncClosingTag(5))
	}
	reply = append(reply, objects.EncOpeningTag(4))
	reply = append(reply, value...)
	return append(reply, objects.EncClosingTag(4))
}

// objectID encodes an application-tagged object identifier.
func objectID(id objects.ObjectIdentifier) *objects.Object {
	data := make([]byte, 4)
	binary.BigEndian.PutUint32(data, uint32(id.ObjectType)<<22|id.InstanceNumber)
	return &objects.Object{TagNumber: objects.TagBACnetObjectIdentifier, Data: data, Length: 4}
}
