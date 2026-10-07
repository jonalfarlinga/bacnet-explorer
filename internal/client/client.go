// Package client is the BACnet/IP client behind the explorer: one UDP socket,
// a read loop that matches replies to requests, and the read services the
// explorer needs. It is the part of the client in docs/library-upgrade-spec.md
// §7 that browsing takes, kept internal until that package exists.
//
// It reaches devices on the local subnet, behind BACnet routers (it learns
// which router serves each network), and on remote IP subnets through a BBMD
// that it registers with as a foreign device.
//
// It is read-only. Everything it sends goes through one function that refuses
// any service except Who-Is, ReadProperty, ReadPropertyMultiple, the
// Segment-ACKs a segmented reply needs, Who-Is-Router-To-Network, and the
// BBMD functions Register-Foreign-Device, Read-Foreign-Device-Table and
// Read-Broadcast-Distribution-Table. It has no device identity: it never
// sends an I-Am and never answers a request.
package client

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
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
	// of it. For a routed device it may be left unset: the client then uses
	// the router it has learned for Net, or finds one.
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
	if !a.IP.IsValid() {
		return fmt.Sprintf("network %d MAC %x", a.Net, a.MAC)
	}
	return fmt.Sprintf("network %d MAC %x via %s", a.Net, a.MAC, a.IP)
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
	// Exclude drops datagrams from these subnets: those of the machine's
	// other adapters, which a socket bound with BindAny hears as well.
	// Anything else is accepted, since a device on another IP subnet may
	// answer directly, or through a BBMD.
	Exclude []netip.Prefix

	Timeout time.Duration // wait for each reply; 3s if not set
	Retries int           // resends of a request that got no reply

	// OnPacket is called from the read loop for every datagram received,
	// and from the sender for every one sent, before anything is done with
	// it. It must not block. Data is the datagram, which the receiver may
	// keep.
	OnPacket func(PacketEvent)

	// ForeignDevice registers the client with a BBMD on another IP subnet.
	// Broadcasts then go to the BBMD as Distribute-Broadcast-To-Network,
	// which it broadcasts on its subnet, and the broadcasts of that subnet
	// come back forwarded by it.
	ForeignDevice *ForeignDeviceConfig

	// Logger gets what the client refuses to send, the routes it learns and
	// drops, and the foreign-device registration. nil discards it all.
	Logger *slog.Logger

	// OnIAm is called from the read loop for every I-Am heard. It must not
	// block.
	OnIAm func(Device)
}

// PacketEvent is one datagram sent or received.
type PacketEvent struct {
	Time time.Time
	// Inbound is true for a datagram received, from Peer; false for one
	// sent, to Peer.
	Inbound bool
	Peer    netip.AddrPort
	Data    []byte
}

var (
	ErrTimeout           = errors.New("no reply from the device")
	ErrClosed            = errors.New("client closed")
	ErrServiceNotAllowed = errors.New("not a read-only service")
	// ErrNetworkUnreachable is a router's Reject-Message-To-Network: it
	// does not reach the network the request was for.
	ErrNetworkUnreachable = errors.New("no router reaches the network")
)

// maxReassembled bounds a segmented reply.
const maxReassembled = 4 << 20

// Client owns the socket.
type Client struct {
	cfg  Config
	conn *net.UDPConn
	log  *slog.Logger

	mu      sync.Mutex
	pending map[txKey]chan inbound
	peers   map[netip.AddrPort]*peer
	// routes is the router for each network number, learned from
	// I-Am-Router-To-Network and from the source of routed messages.
	routes map[uint16]netip.AddrPort
	// busy holds, for each network a router has said it is busy for, when
	// the hold ends on its own.
	busy map[uint16]time.Time
	// available is closed, and replaced, when a router says it is available
	// again, to wake what waits on busy.
	available chan struct{}
	// routerWatch is the Who-Is-Router-To-Network queries waiting for
	// I-Am-Router-To-Network.
	routerWatch map[*routerWatcher]struct{}
	// bvlcWait is the BBMD requests waiting for a BVLC-only reply.
	bvlcWait map[bvlcKey]chan plumbing.BACnet

	fd *foreign // nil unless ForeignDevice is configured

	closed chan struct{}
	done   chan struct{}
}

// txKey identifies a confirmed request by its peer and invoke ID. A routed
// device is known by its network and MAC rather than the router's IP, so
// that its reply matches whichever router carries it.
type txKey struct {
	ip     netip.AddrPort
	net    uint16
	mac    string
	invoke uint8
}

func keyOf(a Address, invoke uint8) txKey {
	if a.Routed() {
		return txKey{net: a.Net, mac: string(a.MAC), invoke: invoke}
	}
	return txKey{ip: a.IP, invoke: invoke}
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
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.ForeignDevice != nil {
		if !cfg.ForeignDevice.BBMD.IsValid() || cfg.ForeignDevice.BBMD.Port() == 0 {
			return nil, fmt.Errorf("foreign device: BBMD address %s is not an address and port", cfg.ForeignDevice.BBMD)
		}
		if cfg.ForeignDevice.TTL <= 0 {
			cfg.ForeignDevice.TTL = DefaultTTL
		}
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
		cfg:         cfg,
		conn:        conn,
		log:         cfg.Logger,
		pending:     map[txKey]chan inbound{},
		peers:       map[netip.AddrPort]*peer{},
		routes:      map[uint16]netip.AddrPort{},
		busy:        map[uint16]time.Time{},
		available:   make(chan struct{}),
		routerWatch: map[*routerWatcher]struct{}{},
		bvlcWait:    map[bvlcKey]chan plumbing.BACnet{},
		closed:      make(chan struct{}),
		done:        make(chan struct{}),
	}
	if cfg.ForeignDevice != nil {
		c.fd = newForeign(*cfg.ForeignDevice)
	}
	go c.readLoop()
	if c.fd != nil {
		go c.registerLoop()
	}
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

// frame is an NPDU with an APDU in a BVLC envelope: the unit in which
// application services are sent.
type frame struct {
	bvlc *plumbing.BVLC
	npdu *plumbing.NPDU
	apdu *plumbing.APDU
}

func (f *frame) MarshalLen() int {
	return f.bvlc.MarshalLen() + f.npdu.MarshalLen() + f.apdu.MarshalLen()
}

func (f *frame) MarshalTo(b []byte) error {
	total := f.MarshalLen()
	if total > 0xFFFF {
		return fmt.Errorf("frame of %d bytes is too long", total)
	}
	f.bvlc.Length = uint16(total)
	if err := f.bvlc.MarshalTo(b); err != nil {
		return err
	}
	offset := f.bvlc.MarshalLen()
	if err := f.npdu.MarshalTo(b[offset:]); err != nil {
		return err
	}
	offset += f.npdu.MarshalLen()
	return f.apdu.MarshalTo(b[offset:])
}

func (f *frame) MarshalBinary() ([]byte, error) {
	b := make([]byte, f.MarshalLen())
	if err := f.MarshalTo(b); err != nil {
		return nil, err
	}
	return b, nil
}

func (f *frame) UnmarshalBinary([]byte) error { return errors.New("a frame is only sent") }
func (f *frame) GetType() uint8               { return f.apdu.Type }
func (f *frame) GetService() uint8            { return f.apdu.Service }

// allowed reports whether a message is one this client may send: a read
// service or a Segment-ACK in a unicast, broadcast or distribute envelope, a
// Who-Is-Router-To-Network, or one of the BBMD functions that change nothing
// but the client's own registration.
func allowed(msg plumbing.BACnet) error {
	switch m := msg.(type) {
	case *frame:
		if err := envelopeAllowed(m.bvlc.Function); err != nil {
			return err
		}
		if !apduAllowed(m.apdu) {
			return fmt.Errorf("PDU type %d service %d: %w", m.apdu.Type, m.apdu.Service, ErrServiceNotAllowed)
		}
		return nil
	case *plumbing.NetworkMessage:
		if err := envelopeAllowed(m.BVLC.Function); err != nil {
			return err
		}
		if m.Type != plumbing.NetMsgWhoIsRouterToNetwork {
			return fmt.Errorf("network layer message %#x: %w", m.Type, ErrServiceNotAllowed)
		}
		return nil
	case *plumbing.RegisterForeignDevice, *plumbing.ReadForeignDeviceTable, *plumbing.ReadBroadcastDistributionTable:
		return nil
	}
	return fmt.Errorf("%T: %w", msg, ErrServiceNotAllowed)
}

func envelopeAllowed(function uint8) error {
	switch function {
	case plumbing.BVLCFuncUnicast, plumbing.BVLCFuncBroadcast, plumbing.BVLCFuncDistributeBroadcastToNetwork:
		return nil
	}
	return fmt.Errorf("BVLC function %#x: %w", function, ErrServiceNotAllowed)
}

func apduAllowed(a *plumbing.APDU) bool {
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

// send marshals one message and sends it. It is the only place the client
// writes to the network.
func (c *Client) send(to netip.AddrPort, msg plumbing.BACnet) error {
	if err := allowed(msg); err != nil {
		c.log.Warn("refused to send", "to", to, "error", err)
		return err
	}
	b, err := msg.MarshalBinary()
	if err != nil {
		return err
	}
	if c.cfg.OnPacket != nil {
		c.cfg.OnPacket(PacketEvent{Time: time.Now(), Peer: to, Data: b})
	}
	_, err = c.conn.WriteToUDPAddrPort(b, to)
	return err
}

// sendAPDU sends one APDU in the given envelope.
func (c *Client) sendAPDU(bvlcFunc uint8, to netip.AddrPort, npdu *plumbing.NPDU, a *plumbing.APDU) error {
	return c.send(to, &frame{bvlc: plumbing.NewBVLC(bvlcFunc), npdu: npdu, apdu: a})
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

// broadcastTarget is where a broadcast goes: the subnet's broadcast
// address, or the BBMD as a Distribute-Broadcast-To-Network when the client
// is a foreign device.
func (c *Client) broadcastTarget() (netip.AddrPort, uint8) {
	if c.fd != nil {
		return c.fd.cfg.BBMD, plumbing.BVLCFuncDistributeBroadcastToNetwork
	}
	return c.cfg.Broadcast, plumbing.BVLCFuncBroadcast
}

// WhoIs broadcasts one Who-Is addressed to every network, so that devices
// behind routers answer as well. The I-Ams it draws arrive through OnIAm.
func (c *Client) WhoIs() error {
	npdu := plumbing.NewNPDU(false, true, false, false)
	npdu.DNET = 0xFFFF
	npdu.Hop = 255
	whoIs := plumbing.NewAPDU(plumbing.UnConfirmedReq, services.ServiceUnconfirmedWhoIs, nil)
	to, function := c.broadcastTarget()
	return c.sendAPDU(function, to, npdu, whoIs)
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
		if c.cfg.OnPacket != nil {
			c.cfg.OnPacket(PacketEvent{Time: time.Now(), Inbound: true, Peer: src, Data: append([]byte(nil), buf[:n]...)})
		}

		if src == c.cfg.Local {
			continue // the client's own broadcast coming back
		}
		if c.excluded(src.Addr()) {
			c.log.Debug("dropped a datagram from another adapter's subnet", "from", src)
			continue
		}

		// A message a BBMD forwarded comes from the device at its origin;
		// the BBMD is only the carrier.
		var hdr plumbing.BVLC
		if err := hdr.UnmarshalBinary(buf[:n]); err != nil {
			c.log.Debug("dropped a datagram that is not BACnet/IP", "from", src, "error", err)
			continue
		}
		if origin, ok := hdr.OriginAddrPort(); ok {
			if origin == c.cfg.Local {
				continue // the client's own broadcast, back from the BBMD
			}
			src = origin
		}
		msg, err := bacnet.ParseWithFallback(buf[:n])
		if err != nil {
			c.log.Debug("dropped a datagram that could not be parsed", "from", src, "error", err, "bytes", fmt.Sprintf("%x", buf[:n]))
			continue
		}
		c.dispatch(src, msg)
	}
}

func (c *Client) excluded(addr netip.Addr) bool {
	for _, p := range c.cfg.Exclude {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func (c *Client) dispatch(src netip.AddrPort, msg plumbing.BACnet) {
	switch m := msg.(type) {
	case *services.UnconfirmedIAm:
		c.noteSource(src, m.NPDU)
		d, err := m.Decode()
		if err != nil {
			c.log.Warn("dropped an I-Am that could not be decoded", "from", sourceOf(src, m.NPDU), "error", err)
			return
		}
		if d.DeviceType != objects.ObjectTypeDevice {
			c.log.Warn("dropped an I-Am for an object that is not a device", "from", sourceOf(src, m.NPDU), "type", d.DeviceType, "instance", d.InstanceNum)
			return
		}
		c.log.Debug("I-Am", "device", d.InstanceNum, "from", sourceOf(src, m.NPDU))
		if c.cfg.OnIAm == nil {
			return
		}
		c.cfg.OnIAm(Device{
			Instance:     d.InstanceNum,
			Addr:         sourceOf(src, m.NPDU),
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
		c.networkMessage(src, m)
	case *plumbing.BVLCResult:
		c.bvlcResult(src, m)
	case *plumbing.ReadForeignDeviceTableAck:
		c.bvlcReply(src, waitFDT, m)
	case *plumbing.ReadBroadcastDistributionTableAck:
		c.bvlcReply(src, waitBDT, m)
	default:
		c.log.Debug("ignored a message", "from", src, "type", fmt.Sprintf("%T", msg), "service", msg.GetService())
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

// noteSource learns from a message with a source network that the BACnet/IP
// address it came from is the router for that network.
func (c *Client) noteSource(src netip.AddrPort, npdu *plumbing.NPDU) {
	if npdu.Control&0x08 != 0 && npdu.SNET != 0 && npdu.SNET != 0xFFFF {
		c.learnRoute(npdu.SNET, src)
	}
}

func (c *Client) deliver(src netip.AddrPort, npdu *plumbing.NPDU, a *plumbing.APDU) {
	c.noteSource(src, npdu)
	key := keyOf(sourceOf(src, npdu), a.InvokeID)
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

// failNet ends every request in flight to the devices of one network.
func (c *Client) failNet(dnet uint16, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, ch := range c.pending {
		if key.net == dnet {
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
	via, function, err := c.resolve(ctx, to)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	p := c.peers[via]
	if p == nil {
		p = &peer{busy: make(chan struct{}, 1)}
		c.peers[via] = p
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

	key := keyOf(to, req.InvokeID)
	ch := make(chan inbound, 64)
	c.mu.Lock()
	c.pending[key] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}()

	// Segment-ACKs go back the way the request went.
	to.IP = via
	for attempt := 0; attempt <= c.cfg.Retries; attempt++ {
		if to.Routed() {
			if err := c.waitAvailable(ctx, to.Net); err != nil {
				return nil, err
			}
		}
		if err := c.sendAPDU(function, via, npduTo(to, true), req); err != nil {
			return nil, err
		}
		tags, err := c.await(ctx, ch, to, req.InvokeID)
		if !errors.Is(err, errNoReply) {
			return tags, err
		}
	}
	if to.Routed() && function == plumbing.BVLCFuncUnicast {
		// The router may have moved: find it again next time.
		c.dropRoute(to.Net, via)
	}
	return nil, ErrTimeout
}

// resolve says where a request for to goes: the device's own address, the
// router learned for its network, or failing both, every router at once.
func (c *Client) resolve(ctx context.Context, to Address) (netip.AddrPort, uint8, error) {
	if !to.Routed() {
		if !to.IP.IsValid() {
			return netip.AddrPort{}, 0, errors.New("no address to send to")
		}
		return to.IP, plumbing.BVLCFuncUnicast, nil
	}
	if router, ok := c.route(to.Net); ok {
		return router, plumbing.BVLCFuncUnicast, nil
	}
	if to.IP.IsValid() {
		return to.IP, plumbing.BVLCFuncUnicast, nil
	}
	routers, err := c.whoIsRouter(ctx, &to.Net, true)
	if err != nil {
		return netip.AddrPort{}, 0, err
	}
	if len(routers) > 0 {
		return routers[0].Router, plumbing.BVLCFuncUnicast, nil
	}
	// No router has answered for the network. Broadcast the request with
	// the network set, and learn the router from the reply.
	bcast, function := c.broadcastTarget()
	return bcast, function, nil
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
	c.sendAPDU(plumbing.BVLCFuncUnicast, to.IP, npduTo(to, false), ack)
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
