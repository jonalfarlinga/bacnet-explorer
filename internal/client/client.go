// Package client is the BACnet/IP client behind the explorer: one UDP socket,
// a read loop that matches replies to requests, and the read services the
// explorer needs. It is the part of the client in docs/library-upgrade-spec.md
// §7 that browsing takes, kept internal until that package exists.
//
// It is read-only. Everything it sends goes through one function that refuses
// any service except Who-Is, ReadProperty, ReadPropertyMultiple and the
// Segment-ACKs a segmented reply needs. It has no device identity: it never
// sends an I-Am and never answers a request.
package client

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/jonalfarlinga/bacnet"
	"github.com/jonalfarlinga/bacnet/objects"
	"github.com/jonalfarlinga/bacnet/plumbing"
	"github.com/jonalfarlinga/bacnet/services"
)

// Address is where a device is reached.
type Address struct {
	// IP is the device's BACnet/IP address, or that of the router in front
	// of it.
	IP netip.AddrPort
	// Net is the device's network number, 0 for a device at IP itself.
	Net uint16
	// MAC is the device's address on Net.
	MAC []byte
}

// Routed reports whether the device is behind a router.
func (a Address) Routed() bool { return a.Net != 0 }

func (a Address) String() string {
	if !a.Routed() {
		return a.IP.String()
	}
	return fmt.Sprintf("network %d MAC %x via %s", a.Net, a.MAC, a.IP)
}

func (a Address) key() string {
	if !a.Routed() {
		return a.IP.String()
	}
	return fmt.Sprintf("%s/%d/%x", a.IP, a.Net, a.MAC)
}

// Device is what an I-Am says about a device.
type Device struct {
	Instance     uint32
	Addr         Address
	MaxAPDU      uint16
	Segmentation uint8 // BACnetSegmentation: 0 both, 1 transmit, 2 receive, 3 none
	VendorID     uint16
}

// ObjectID identifies an object in a device.
type ObjectID struct {
	Type     uint16
	Instance uint32
}

// Config says where the client listens and where its broadcasts go.
type Config struct {
	// Local is this machine's address on the BACnet network. Datagrams from
	// it are the client's own broadcasts coming back, and are dropped.
	Local netip.AddrPort
	// BindAny binds the wildcard address on Local's port instead of Local.
	// A socket bound to one address does not receive broadcasts on Linux or
	// macOS; on Windows it does, and binding it keeps the other adapters out.
	BindAny bool
	// Broadcast is where Who-Is goes.
	Broadcast netip.AddrPort
	// Subnet, when valid, limits what is accepted to datagrams from it.
	Subnet netip.Prefix

	Timeout time.Duration // wait for each reply; 3s if not set
	Retries int           // resends of a request that got no reply

	// OnIAm is called from the read loop for every I-Am heard. It must not
	// block.
	OnIAm func(Device)
}

var (
	ErrTimeout           = errors.New("no reply from the device")
	ErrClosed            = errors.New("client closed")
	ErrServiceNotAllowed = errors.New("not a read-only service")
)

// maxReassembled bounds a segmented reply.
const maxReassembled = 4 << 20

// Client owns the socket.
type Client struct {
	cfg  Config
	conn *net.UDPConn

	mu      sync.Mutex
	pending map[txKey]chan inbound
	peers   map[netip.AddrPort]*peer

	closed chan struct{}
	done   chan struct{}
}

type txKey struct {
	peer   string
	invoke uint8
}

// peer is the state kept per BACnet/IP address. Requests to one address go
// out one at a time, which also covers every device behind one router.
type peer struct {
	busy   chan struct{}
	invoke uint8
}

// inbound is a reply PDU on its way to the request that waits for it, or the
// reason there will be none.
type inbound struct {
	apdu *plumbing.APDU
	err  error
}

// Open binds the socket and starts listening.
func Open(cfg Config) (*Client, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 3 * time.Second
	}
	if cfg.Retries < 0 {
		cfg.Retries = 0
	}
	bind := cfg.Local
	if cfg.BindAny {
		bind = netip.AddrPortFrom(netip.IPv4Unspecified(), cfg.Local.Port())
	}
	conn, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(bind))
	if err != nil {
		return nil, err
	}
	if !cfg.Local.IsValid() || cfg.Local.Port() == 0 {
		// An ephemeral port: learn which one.
		if la, ok := conn.LocalAddr().(*net.UDPAddr); ok {
			cfg.Local = netip.AddrPortFrom(la.AddrPort().Addr().Unmap(), la.AddrPort().Port())
		}
	}
	c := &Client{
		cfg:     cfg,
		conn:    conn,
		pending: map[txKey]chan inbound{},
		peers:   map[netip.AddrPort]*peer{},
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// Local returns the address the client sends from.
func (c *Client) Local() netip.AddrPort { return c.cfg.Local }

// Close stops the client. Requests in flight fail with ErrClosed.
func (c *Client) Close() error {
	select {
	case <-c.closed:
		return nil
	default:
	}
	close(c.closed)
	err := c.conn.Close()
	<-c.done
	return err
}

// ---- sending ---------------------------------------------------------------

// allowed reports whether an APDU is one this client may send.
func allowed(a *plumbing.APDU) bool {
	switch a.Type {
	case plumbing.ConfirmedReq:
		return a.Service == services.ServiceConfirmedReadProperty ||
			a.Service == services.ServiceConfirmedReadPropMultiple
	case plumbing.UnConfirmedReq:
		return a.Service == services.ServiceUnconfirmedWhoIs && len(a.Objects) == 0
	case plumbing.SegmentAck:
		return true
	}
	return false
}

// send marshals one APDU and sends it. It is the only place the client writes
// to the network.
func (c *Client) send(bvlcFunc uint8, to netip.AddrPort, npdu *plumbing.NPDU, a *plumbing.APDU) error {
	if !allowed(a) {
		return fmt.Errorf("PDU type %d service %d: %w", a.Type, a.Service, ErrServiceNotAllowed)
	}
	bvlc := plumbing.NewBVLC(bvlcFunc)
	total := bvlc.MarshalLen() + npdu.MarshalLen() + a.MarshalLen()
	if total > 0xFFFF {
		return fmt.Errorf("frame of %d bytes is too long", total)
	}
	b := make([]byte, total)
	bvlc.Length = uint16(total)
	if err := bvlc.MarshalTo(b); err != nil {
		return err
	}
	offset := bvlc.MarshalLen()
	if err := npdu.MarshalTo(b[offset:]); err != nil {
		return err
	}
	offset += npdu.MarshalLen()
	if err := a.MarshalTo(b[offset:]); err != nil {
		return err
	}
	_, err := c.conn.WriteToUDPAddrPort(b, to)
	return err
}

// npduTo builds the NPDU of a message for one device.
func npduTo(to Address, expectingReply bool) *plumbing.NPDU {
	npdu := plumbing.NewNPDU(false, to.Routed(), false, expectingReply)
	if to.Routed() {
		npdu.DNET = to.Net
		npdu.DLEN = uint8(len(to.MAC))
		npdu.DADR = to.MAC
		npdu.Hop = 255
	}
	return npdu
}

// WhoIs broadcasts one Who-Is addressed to every network, so that devices
// behind routers answer as well. The I-Ams it draws arrive through OnIAm.
func (c *Client) WhoIs() error {
	npdu := plumbing.NewNPDU(false, true, false, false)
	npdu.DNET = 0xFFFF
	npdu.Hop = 255
	whoIs := plumbing.NewAPDU(plumbing.UnConfirmedReq, services.ServiceUnconfirmedWhoIs, nil)
	return c.send(plumbing.BVLCFuncBroadcast, c.cfg.Broadcast, npdu, whoIs)
}

// ---- receiving -------------------------------------------------------------

func (c *Client) readLoop() {
	defer close(c.done)
	defer c.failAll(ErrClosed)

	// Nothing parsed from buf aliases it, so it is reused.
	buf := make([]byte, 2048)
	failures := 0
	for {
		n, src, err := c.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			select {
			case <-c.closed:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Windows reports an ICMP port-unreachable for an earlier send as
			// a read error; the socket is still good. An error that keeps
			// coming back is not that, and must not spin.
			if failures++; failures > 100 {
				time.Sleep(50 * time.Millisecond)
			}
			continue
		}
		failures = 0
		src = netip.AddrPortFrom(src.Addr().Unmap(), src.Port())
		if src == c.cfg.Local {
			continue
		}
		if c.cfg.Subnet.IsValid() && !c.cfg.Subnet.Contains(src.Addr()) {
			continue
		}
		msg, err := bacnet.ParseWithFallback(buf[:n])
		if err != nil {
			continue
		}
		c.dispatch(src, msg)
	}
}

func (c *Client) dispatch(src netip.AddrPort, msg plumbing.BACnet) {
	switch m := msg.(type) {
	case *services.UnconfirmedIAm:
		if c.cfg.OnIAm == nil {
			return
		}
		d, err := m.Decode()
		if err != nil || d.DeviceType != objects.ObjectTypeDevice {
			return
		}
		addr := sourceOf(src, m.NPDU)
		if origin, ok := m.BVLC.OriginAddrPort(); ok {
			// Forwarded by a BBMD: the device is at the origin.
			addr.IP = origin
		}
		c.cfg.OnIAm(Device{
			Instance:     d.InstanceNum,
			Addr:         addr,
			MaxAPDU:      d.MaxAPDULength,
			Segmentation: d.SegmentationSupported,
			VendorID:     d.VendorId,
		})
	case *services.ComplexACK:
		c.deliver(src, m.NPDU, m.APDU)
	case *services.Error:
		c.deliver(src, m.NPDU, m.APDU)
	case *services.Reject:
		c.deliver(src, m.NPDU, m.APDU)
	case *services.Abort:
		c.deliver(src, m.NPDU, m.APDU)
	case *services.SimpleACK:
		c.deliver(src, m.NPDU, m.APDU)
	case *plumbing.NetworkMessage:
		// A router that cannot reach a network says so instead of forwarding
		// the request.
		if reason, dnet, err := m.RejectReason(); err == nil {
			c.failNet(src, dnet, fmt.Errorf("the router at %s rejected network %d: %s", src, dnet, routerRejectName(reason)))
		}
	}
}

// sourceOf gives the address of the device that sent a message.
func sourceOf(src netip.AddrPort, npdu *plumbing.NPDU) Address {
	addr := Address{IP: src}
	if npdu.Control&0x08 != 0 {
		addr.Net = npdu.SNET
		addr.MAC = npdu.SADR
	}
	return addr
}

func (c *Client) deliver(src netip.AddrPort, npdu *plumbing.NPDU, a *plumbing.APDU) {
	key := txKey{peer: sourceOf(src, npdu).key(), invoke: a.InvokeID}
	c.mu.Lock()
	ch := c.pending[key]
	c.mu.Unlock()
	if ch == nil {
		return // a late reply, or something the device sent on its own
	}
	select {
	case ch <- inbound{apdu: a}:
	default:
	}
}

func (c *Client) failNet(router netip.AddrPort, dnet uint16, err error) {
	prefix := fmt.Sprintf("%s/%d/", router, dnet)
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, ch := range c.pending {
		if len(key.peer) > len(prefix) && key.peer[:len(prefix)] == prefix {
			select {
			case ch <- inbound{err: err}:
			default:
			}
		}
	}
}

func (c *Client) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ch := range c.pending {
		select {
		case ch <- inbound{err: err}:
		default:
		}
	}
}

// ---- confirmed requests ----------------------------------------------------

// errNoReply is a timeout before any part of a reply arrived, when resending
// the request is safe.
var errNoReply = errors.New("no reply yet")

// confirmed sends one read request and returns the tags of the ComplexACK
// that answers it, reassembled if it came in segments. A request that gets no
// reply is resent with the same invoke ID.
func (c *Client) confirmed(ctx context.Context, to Address, service uint8, payload []objects.APDUPayload) ([]*objects.Object, error) {
	c.mu.Lock()
	p := c.peers[to.IP]
	if p == nil {
		p = &peer{busy: make(chan struct{}, 1)}
		c.peers[to.IP] = p
	}
	c.mu.Unlock()

	select {
	case p.busy <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, ErrClosed
	}
	defer func() { <-p.busy }()

	p.invoke++
	req := plumbing.NewAPDU(plumbing.ConfirmedReq, service, payload)
	req.SetAPDUFlags(true, false, false) // segmented reply accepted
	req.MaxSeg = 7                       // more than 64 segments
	req.MaxSize = 5                      // up to 1476 bytes
	req.InvokeID = p.invoke

	key := txKey{peer: to.key(), invoke: req.InvokeID}
	ch := make(chan inbound, 64)
	c.mu.Lock()
	c.pending[key] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}()

	for attempt := 0; attempt <= c.cfg.Retries; attempt++ {
		if err := c.send(plumbing.BVLCFuncUnicast, to.IP, npduTo(to, true), req); err != nil {
			return nil, err
		}
		tags, err := c.await(ctx, ch, to, req.InvokeID)
		if !errors.Is(err, errNoReply) {
			return tags, err
		}
	}
	return nil, ErrTimeout
}

func (c *Client) await(ctx context.Context, ch chan inbound, to Address, invokeID uint8) ([]*objects.Object, error) {
	var (
		data     []byte
		nextSeq  uint8
		sinceAck uint8
		segments int
	)
	timer := time.NewTimer(c.cfg.Timeout)
	defer timer.Stop()
	for {
		var in inbound
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.closed:
			return nil, ErrClosed
		case <-timer.C:
			if segments > 0 {
				return nil, fmt.Errorf("%w: the reply stopped after %d segments", ErrTimeout, segments)
			}
			return nil, errNoReply
		case in = <-ch:
		}
		if in.err != nil {
			return nil, in.err
		}

		a := in.apdu
		switch a.Type {
		case plumbing.Error:
			return nil, decodeError(a.Objects)
		case plumbing.Reject:
			return nil, &RejectError{Reason: a.Reason}
		case plumbing.Abort:
			return nil, &AbortError{Reason: a.Reason}
		case plumbing.SimpleAck:
			return nil, errors.New("SimpleACK in reply to a read request")
		case plumbing.ComplexAck:
		default:
			continue
		}

		if !a.IsSegmented() {
			return toTags(a.Objects)
		}

		if a.SequenceNumber != nextSeq {
			// A duplicate or a gap: ask again from the last one in order.
			c.segmentAck(to, invokeID, nextSeq-1, a.ProposedWindowSize, true)
			continue
		}
		data = append(data, a.Payload...)
		if len(data) > maxReassembled {
			return nil, fmt.Errorf("the reply is longer than %d bytes", maxReassembled)
		}
		nextSeq++
		sinceAck++
		segments++

		// The first segment is acknowledged alone, then each full window,
		// then the last segment.
		more := a.HasMoreSegments()
		if a.SequenceNumber == 0 || sinceAck >= a.ProposedWindowSize || !more {
			c.segmentAck(to, invokeID, a.SequenceNumber, a.ProposedWindowSize, false)
			sinceAck = 0
		}
		if more {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(c.cfg.Timeout)
			continue
		}

		var tags []*objects.Object
		for offset := 0; offset < len(data); {
			tag, n, err := objects.DecodeTag(data[offset:])
			if err != nil {
				return nil, fmt.Errorf("decoding the reassembled reply: %w", err)
			}
			tags = append(tags, tag)
			offset += n
		}
		return tags, nil
	}
}

func (c *Client) segmentAck(to Address, invokeID, sequence, window uint8, negative bool) {
	ack := &plumbing.APDU{
		Type:             plumbing.SegmentAck,
		InvokeID:         invokeID,
		SequenceNumber:   sequence,
		ActualWindowSize: window,
	}
	if negative {
		ack.Flags = 0x2
	}
	c.send(plumbing.BVLCFuncUnicast, to.IP, npduTo(to, false), ack)
}

func toTags(payload []objects.APDUPayload) ([]*objects.Object, error) {
	tags := make([]*objects.Object, 0, len(payload))
	for _, p := range payload {
		tag, ok := p.(*objects.Object)
		if !ok {
			return nil, fmt.Errorf("unexpected payload %T in a reply", p)
		}
		tags = append(tags, tag)
	}
	return tags, nil
}

// ---- errors ----------------------------------------------------------------

// BACnetError is a device's Error reply, or the error it gave for one
// property of a ReadPropertyMultiple.
type BACnetError struct{ Class, Code uint32 }

func (e *BACnetError) Error() string {
	return fmt.Sprintf("%s: %s", ErrorClassName(e.Class), ErrorCodeName(e.Code))
}

// RejectError is a device's Reject reply.
type RejectError struct{ Reason uint8 }

func (e *RejectError) Error() string {
	return "rejected: " + reasonName(objects.RejectReasonMap, e.Reason)
}

// AbortError is a device's Abort reply.
type AbortError struct{ Reason uint8 }

func (e *AbortError) Error() string {
	return "aborted: " + reasonName(objects.AbortReasonMap, e.Reason)
}

// Declined reports whether err is the device saying no, as opposed to a
// timeout or a reply that could not be read.
func Declined(err error) bool {
	var (
		be *BACnetError
		re *RejectError
		ae *AbortError
	)
	return errors.As(err, &be) || errors.As(err, &re) || errors.As(err, &ae)
}

func decodeError(payload []objects.APDUPayload) error {
	e := &BACnetError{}
	if len(payload) >= 2 {
		if class, err := objects.DecEnumerated(payload[0]); err == nil {
			e.Class = class
		}
		if code, err := objects.DecEnumerated(payload[1]); err == nil {
			e.Code = code
		}
	}
	return e
}

func reasonName(names map[uint8]string, v uint8) string {
	if name, ok := names[v]; ok {
		return name
	}
	return fmt.Sprintf("reason %d", v)
}

func routerRejectName(reason uint8) string {
	names := []string{
		"other error", "not directly connected to it and no route known", "router busy",
		"unknown network layer message", "message too long", "security error", "addressing error",
	}
	if int(reason) < len(names) {
		return names[reason]
	}
	return fmt.Sprintf("reason %d", reason)
}

// MACString renders a MAC address the way it is usually written: an IP
// address and port when it is a BACnet/IP one, a station number when it is
// one byte, hex otherwise.
func MACString(mac []byte) string {
	switch {
	case len(mac) == 1:
		return fmt.Sprint(mac[0])
	case len(mac) == 6 && mac[4] == 0xBA && mac[5]&0xF0 == 0xC0:
		return fmt.Sprintf("%s:%d", net.IP(mac[:4]), uint16(mac[4])<<8|uint16(mac[5]))
	}
	return hex.EncodeToString(mac)
}
