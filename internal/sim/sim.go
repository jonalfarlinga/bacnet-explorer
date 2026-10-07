// Package sim runs simulated BACnet/IP devices on the loopback interface, for
// trying the explorer and testing its client without a network. The devices
// answer Who-Is, ReadProperty and ReadPropertyMultiple, segment replies that
// are too long for one APDU, and include a router with devices behind it, a
// device that cannot segment and one without ReadPropertyMultiple. The
// router answers Who-Is-Router-To-Network and rejects requests for networks
// it does not have. A station can be a BBMD, which registers foreign devices
// and forwards broadcasts to and from them.
//
// Loopback has no broadcast address, so a frame sent as an
// Original-Broadcast-NPDU to any station is heard by every station, as a
// subnet broadcast would be.
package sim

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"sync"
	"time"

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
	// BBMD makes the device's station a BBMD as well: it registers foreign
	// devices, answers the table reads, and forwards broadcasts between its
	// subnet (every station) and the foreign devices.
	BBMD bool
	// RefuseForeign makes a BBMD NAK every registration.
	RefuseForeign bool

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
	// fdt is the Foreign Device Table of a BBMD station: when each
	// registration expires.
	fdt map[netip.AddrPort]fdEntry
}

type fdEntry struct {
	ttl     uint16
	expires time.Time
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
		n.stations = append(n.stations, &station{
			conn: conn, devices: devices,
			sending: map[txKey]*segmented{}, fdt: map[netip.AddrPort]fdEntry{},
		})
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

// Addr is the address of the first station. A broadcast sent to it reaches
// every station, and when the first station is a BBMD, a foreign device
// registers there.
func (n *Network) Addr() netip.AddrPort {
	return n.stations[0].addr()
}

// Addrs is the address of every station, in the order they were given.
func (n *Network) Addrs() []netip.AddrPort {
	addrs := make([]netip.AddrPort, len(n.stations))
	for i, st := range n.stations {
		addrs[i] = st.addr()
	}
	return addrs
}

// Close stops the devices.
func (n *Network) Close() {
	for _, st := range n.stations {
		st.conn.Close()
	}
	n.wg.Wait()
}

func (st *station) addr() netip.AddrPort {
	ap := st.conn.LocalAddr().(*net.UDPAddr).AddrPort()
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

func (st *station) bbmd() bool { return st.devices[0].BBMD }

func (n *Network) serve(st *station) {
	buf := make([]byte, 2048)
	for {
		size, src, err := st.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		src = netip.AddrPortFrom(src.Addr().Unmap(), src.Port())
		raw := append([]byte(nil), buf[:size]...)
		var hdr plumbing.BVLC
		if hdr.UnmarshalBinary(raw) != nil {
			continue
		}
		switch hdr.Function {
		case plumbing.BVLCFuncBroadcast:
			// Stands in for the subnet broadcast: every station hears it.
			for _, other := range n.stations {
				other.handle(src, raw)
			}
		case plumbing.BVLCFuncDistributeBroadcastToNetwork:
			st.distribute(n, src, raw)
		default:
			st.handle(src, raw)
		}
	}
}

// handle answers one frame addressed to the station.
func (st *station) handle(src netip.AddrPort, raw []byte) {
	msg, err := bacnet.Parse(raw)
	if err != nil {
		return
	}
	switch m := msg.(type) {
	case *services.UnconfirmedWhoIs:
		st.iAm(src, m.NPDU)
	case *services.UnconfirmedIAm:
		// A broadcast on the subnet: a BBMD passes it to its foreign
		// devices.
		st.forwardToForeign(src, raw)
	case *services.ConfirmedReadProperty:
		if dev := st.target(m.NPDU); dev != nil {
			st.read(src, dev, m.APDU)
		} else if m.NPDU.Control&0x20 != 0 && len(st.nets()) > 0 {
			st.rejectNetwork(src, m.NPDU.DNET)
		}
	case *services.SegmentACK:
		st.continueReply(src, m.APDU)
	case *plumbing.NetworkMessage:
		st.networkMessage(src, m)
	case *plumbing.RegisterForeignDevice:
		st.register(src, m.TTL)
	case *plumbing.ReadForeignDeviceTable:
		st.readFDT(src)
	case *plumbing.ReadBroadcastDistributionTable:
		st.readBDT(src)
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

// nets lists the networks the station routes to.
func (st *station) nets() []uint16 {
	var nets []uint16
	for _, d := range st.devices {
		if d.Net != 0 && !containsNet(nets, d.Net) {
			nets = append(nets, d.Net)
		}
	}
	return nets
}

func containsNet(nets []uint16, net uint16) bool {
	for _, n := range nets {
		if n == net {
			return true
		}
	}
	return false
}

// ---- routing ---------------------------------------------------------------

// networkMessage answers a Who-Is-Router-To-Network when the station is a
// router for the network asked about, or for any network when none is.
func (st *station) networkMessage(src netip.AddrPort, m *plumbing.NetworkMessage) {
	if m.Type != plumbing.NetMsgWhoIsRouterToNetwork {
		return
	}
	nets := st.nets()
	if len(nets) == 0 {
		return
	}
	asked, err := m.Networks()
	if err != nil {
		return
	}
	if len(asked) == 1 && !containsNet(nets, asked[0]) {
		return
	}
	st.sendNetworkMessage(src, plumbing.NetMsgIAmRouterToNetwork, plumbing.NetworksData(nets...))
}

// rejectNetwork tells the sender the station routes to no such network.
func (st *station) rejectNetwork(src netip.AddrPort, dnet uint16) {
	data := []byte{1, 0, 0} // not directly connected and no route known
	binary.BigEndian.PutUint16(data[1:], dnet)
	st.sendNetworkMessage(src, plumbing.NetMsgRejectMessageToNetwork, data)
}

func (st *station) sendNetworkMessage(to netip.AddrPort, msgType uint8, data []byte) {
	m := plumbing.NewNetworkMessage(
		plumbing.NewBVLC(plumbing.BVLCFuncUnicast),
		plumbing.NewNPDU(false, false, false, false),
		msgType, data,
	)
	if b, err := m.MarshalBinary(); err == nil {
		st.conn.WriteToUDPAddrPort(b, to)
	}
}

// ---- BBMD ------------------------------------------------------------------

// register answers a Register-Foreign-Device.
func (st *station) register(src netip.AddrPort, ttl uint16) {
	if !st.bbmd() {
		return
	}
	if st.devices[0].RefuseForeign {
		st.result(src, plumbing.BVLCResultRegisterForeignDeviceNAK)
		return
	}
	st.mu.Lock()
	st.fdt[src] = fdEntry{ttl: ttl, expires: time.Now().Add(time.Duration(ttl)*time.Second + 30*time.Second)}
	st.mu.Unlock()
	st.result(src, plumbing.BVLCResultSuccess)
}

// foreign lists the registrations that have not expired.
func (st *station) foreign() map[netip.AddrPort]fdEntry {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	for addr, e := range st.fdt {
		if !e.expires.After(now) {
			delete(st.fdt, addr)
		}
	}
	live := make(map[netip.AddrPort]fdEntry, len(st.fdt))
	for addr, e := range st.fdt {
		live[addr] = e
	}
	return live
}

func (st *station) readFDT(src netip.AddrPort) {
	if !st.bbmd() {
		return
	}
	var entries []plumbing.FDTEntry
	now := time.Now()
	for addr, e := range st.foreign() {
		entries = append(entries, plumbing.FDTEntry{
			IP: addr.Addr().As4(), Port: addr.Port(), TTL: e.ttl,
			RemainingTime: uint16(max(0, e.expires.Sub(now)/time.Second)),
		})
	}
	st.sendBVLC(src, plumbing.NewReadForeignDeviceTableAck(entries))
}

func (st *station) readBDT(src netip.AddrPort) {
	if !st.bbmd() {
		return
	}
	self := st.addr()
	st.sendBVLC(src, plumbing.NewReadBroadcastDistributionTableAck([]plumbing.BDTEntry{
		{IP: self.Addr().As4(), Port: self.Port(), Mask: [4]byte{255, 255, 255, 255}},
	}))
}

func (st *station) result(to netip.AddrPort, code uint16) {
	st.sendBVLC(to, plumbing.NewBVLCResult(code))
}

func (st *station) sendBVLC(to netip.AddrPort, msg plumbing.BACnet) {
	if b, err := msg.MarshalBinary(); err == nil {
		st.conn.WriteToUDPAddrPort(b, to)
	}
}

// distribute broadcasts, on behalf of a registered foreign device, the NPDU
// of a Distribute-Broadcast-To-Network: to every station of the subnet and
// to the other foreign devices, as a Forwarded-NPDU from the sender.
func (st *station) distribute(n *Network, src netip.AddrPort, raw []byte) {
	if !st.bbmd() {
		return
	}
	foreign := st.foreign()
	if _, ok := foreign[src]; !ok {
		st.result(src, plumbing.BVLCResultDistributeBroadcastToNetworkNAK)
		return
	}
	fwd := forwarded(raw, src)
	for _, other := range n.stations {
		st.conn.WriteToUDPAddrPort(fwd, other.addr())
	}
	for addr := range foreign {
		if addr != src {
			st.conn.WriteToUDPAddrPort(fwd, addr)
		}
	}
}

// forwardToForeign passes a broadcast heard on the subnet to every foreign
// device, as a Forwarded-NPDU from its sender.
func (st *station) forwardToForeign(src netip.AddrPort, raw []byte) {
	if !st.bbmd() {
		return
	}
	fwd := forwarded(raw, src)
	for addr := range st.foreign() {
		if addr != src {
			st.conn.WriteToUDPAddrPort(fwd, addr)
		}
	}
}

// forwarded re-envelopes a frame as a Forwarded-NPDU from origin.
func forwarded(raw []byte, origin netip.AddrPort) []byte {
	var hdr plumbing.BVLC
	if hdr.UnmarshalBinary(raw) != nil {
		return nil
	}
	body := raw[hdr.MarshalLen():]
	f := plumbing.NewBVLC(plumbing.BVLCFuncForwardedNPDU)
	ip := origin.Addr().As4()
	copy(f.Origin[:4], ip[:])
	binary.BigEndian.PutUint16(f.Origin[4:], origin.Port())
	f.Length = uint16(f.MarshalLen() + len(body))
	out := make([]byte, f.MarshalLen()+len(body))
	f.MarshalTo(out)
	copy(out[f.MarshalLen():], body)
	return out
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
