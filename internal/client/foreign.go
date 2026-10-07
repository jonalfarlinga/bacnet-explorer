package client

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/jonalfarlinga/bacnet/plumbing"
)

// DefaultTTL is how long a foreign-device registration lasts when the
// configuration does not say.
const DefaultTTL = 5 * time.Minute

// Backoff between registration attempts after one fails.
const (
	fdBackoffMin = 5 * time.Second
	fdBackoffMax = 5 * time.Minute
)

// ForeignDeviceConfig names the BBMD a client on another IP subnet
// registers with.
type ForeignDeviceConfig struct {
	BBMD netip.AddrPort
	// TTL is how long each registration lasts; the client registers again
	// at half of it. DefaultTTL if not set. It is sent in whole seconds.
	TTL time.Duration
}

// FDState is where the foreign-device registration stands.
type FDState uint8

const (
	// FDRegistering means a registration is on its way to the BBMD.
	FDRegistering FDState = iota
	// FDRegistered means the BBMD accepted the last registration, and
	// forwards broadcasts until it expires.
	FDRegistered
	// FDFailed means the BBMD refused the last registration or did not
	// answer. The client tries again at NextAttempt.
	FDFailed
)

func (s FDState) String() string {
	switch s {
	case FDRegistering:
		return "registering"
	case FDRegistered:
		return "registered"
	case FDFailed:
		return "failed"
	}
	return fmt.Sprintf("state %d", uint8(s))
}

// FDStatus is the state of the client's registration with its BBMD.
type FDStatus struct {
	BBMD  netip.AddrPort
	State FDState
	// LastResult is what the last registration attempt came to: nil when
	// the BBMD accepted it, its NAK as a *BVLCResultError, or ErrTimeout.
	LastResult error
	// NextAttempt is when the next registration goes out: halfway through
	// the TTL after a success, after a growing backoff after a failure.
	NextAttempt time.Time
	// Registrations counts the registrations the BBMD has accepted.
	Registrations int
}

// BVLCResultError is a BBMD's NAK to a BVLC function.
type BVLCResultError struct{ Code uint16 }

func (e *BVLCResultError) Error() string {
	if name, ok := plumbing.BVLCResultCodeMap[e.Code]; ok {
		return "BBMD: " + name
	}
	return fmt.Sprintf("BBMD: result %#04x", e.Code)
}

// foreign is the registration state machine.
type foreign struct {
	cfg ForeignDeviceConfig

	mu     sync.Mutex
	status FDStatus
	// kick asks the registration loop to register now, when the BBMD
	// refuses a Distribute-Broadcast-To-Network because the registration
	// lapsed.
	kick chan struct{}
}

func newForeign(cfg ForeignDeviceConfig) *foreign {
	return &foreign{
		cfg:    cfg,
		status: FDStatus{BBMD: cfg.BBMD, State: FDRegistering},
		kick:   make(chan struct{}, 1),
	}
}

// ForeignDeviceStatus returns the registration state, and false when the
// client is not configured as a foreign device.
func (c *Client) ForeignDeviceStatus() (FDStatus, bool) {
	if c.fd == nil {
		return FDStatus{}, false
	}
	c.fd.mu.Lock()
	defer c.fd.mu.Unlock()
	return c.fd.status, true
}

// registerLoop keeps the client registered with the BBMD until it closes.
func (c *Client) registerLoop() {
	fd := c.fd
	ttlSeconds := int64(fd.cfg.TTL / time.Second)
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}
	if ttlSeconds > 0xFFFF {
		ttlSeconds = 0xFFFF
	}
	ttl := time.Duration(ttlSeconds) * time.Second
	backoff := fdBackoffMin

	for {
		fd.mu.Lock()
		fd.status.State = FDRegistering
		fd.mu.Unlock()

		err := c.register(uint16(ttlSeconds))
		var wait time.Duration
		fd.mu.Lock()
		fd.status.LastResult = err
		if err == nil {
			fd.status.State = FDRegistered
			fd.status.Registrations++
			backoff = fdBackoffMin
			wait = ttl / 2
		} else {
			fd.status.State = FDFailed
			wait = backoff
			backoff = min(2*backoff, fdBackoffMax)
		}
		fd.status.NextAttempt = time.Now().Add(wait)
		state := fd.status.State
		fd.mu.Unlock()
		if err != nil {
			c.log.Warn("foreign device registration failed", "bbmd", fd.cfg.BBMD, "error", err, "retry", wait)
		} else {
			c.log.Info("foreign device registered", "bbmd", fd.cfg.BBMD, "ttl", ttl, "state", state)
		}

		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-fd.kick:
			timer.Stop()
		case <-c.closed:
			timer.Stop()
			return
		}
	}
}

// register sends one Register-Foreign-Device and waits for the BBMD's
// answer.
func (c *Client) register(ttl uint16) error {
	bbmd := c.fd.cfg.BBMD
	ch, done := c.bvlcWaiter(bbmd, waitRegister)
	defer done()
	if err := c.send(bbmd, plumbing.NewRegisterForeignDevice(ttl)); err != nil {
		return err
	}
	timer := time.NewTimer(c.cfg.Timeout)
	defer timer.Stop()
	select {
	case msg := <-ch:
		if r, ok := msg.(*plumbing.BVLCResult); ok && r.ResultCode != plumbing.BVLCResultSuccess {
			return &BVLCResultError{Code: r.ResultCode}
		}
		return nil
	case <-timer.C:
		return ErrTimeout
	case <-c.closed:
		return ErrClosed
	}
}

// ---- BVLC-only exchanges ---------------------------------------------------

// What a BVLC-only reply answers.
const (
	waitRegister uint8 = iota
	waitFDT
	waitBDT
)

type bvlcKey struct {
	from netip.AddrPort
	kind uint8
}

// bvlcWaiter registers for the reply to one BBMD request. One request of
// each kind is outstanding per BBMD; a second one takes the channel over.
func (c *Client) bvlcWaiter(from netip.AddrPort, kind uint8) (chan plumbing.BACnet, func()) {
	key := bvlcKey{from: from, kind: kind}
	ch := make(chan plumbing.BACnet, 1)
	c.mu.Lock()
	c.bvlcWait[key] = ch
	c.mu.Unlock()
	return ch, func() {
		c.mu.Lock()
		if c.bvlcWait[key] == ch {
			delete(c.bvlcWait, key)
		}
		c.mu.Unlock()
	}
}

func (c *Client) bvlcReply(from netip.AddrPort, kind uint8, msg plumbing.BACnet) {
	c.mu.Lock()
	ch := c.bvlcWait[bvlcKey{from: from, kind: kind}]
	c.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- msg:
	default:
	}
}

// bvlcResult routes a BVLC-Result to the request it answers. The code says
// which, as the result itself names no request.
func (c *Client) bvlcResult(src netip.AddrPort, r *plumbing.BVLCResult) {
	switch r.ResultCode {
	case plumbing.BVLCResultSuccess, plumbing.BVLCResultRegisterForeignDeviceNAK:
		c.bvlcReply(src, waitRegister, r)
	case plumbing.BVLCResultReadForeignDeviceTableNAK:
		c.bvlcReply(src, waitFDT, r)
	case plumbing.BVLCResultReadBroadcastDistributionNAK:
		c.bvlcReply(src, waitBDT, r)
	case plumbing.BVLCResultDistributeBroadcastToNetworkNAK:
		// The BBMD no longer has the registration: renew it now.
		c.log.Warn("BBMD refused a broadcast", "bbmd", src)
		if c.fd != nil && src == c.fd.cfg.BBMD {
			select {
			case c.fd.kick <- struct{}{}:
			default:
			}
		}
	}
}

// ReadFDT reads a BBMD's Foreign Device Table: the devices registered with
// it, and how long each registration has left.
func (c *Client) ReadFDT(ctx context.Context, bbmd netip.AddrPort) ([]plumbing.FDTEntry, error) {
	msg, err := c.bvlcExchange(ctx, bbmd, waitFDT, plumbing.NewReadForeignDeviceTable())
	if err != nil {
		return nil, err
	}
	ack, ok := msg.(*plumbing.ReadForeignDeviceTableAck)
	if !ok {
		return nil, fmt.Errorf("%T in reply to Read-Foreign-Device-Table", msg)
	}
	return ack.Entries, nil
}

// ReadBDT reads a BBMD's Broadcast Distribution Table: the BBMDs it passes
// broadcasts on to.
func (c *Client) ReadBDT(ctx context.Context, bbmd netip.AddrPort) ([]plumbing.BDTEntry, error) {
	msg, err := c.bvlcExchange(ctx, bbmd, waitBDT, plumbing.NewReadBroadcastDistributionTable())
	if err != nil {
		return nil, err
	}
	ack, ok := msg.(*plumbing.ReadBroadcastDistributionTableAck)
	if !ok {
		return nil, fmt.Errorf("%T in reply to Read-Broadcast-Distribution-Table", msg)
	}
	return ack.Entries, nil
}

// bvlcExchange sends one BVLC-only request to a BBMD and returns its reply,
// or the NAK it gave as a *BVLCResultError.
func (c *Client) bvlcExchange(ctx context.Context, bbmd netip.AddrPort, kind uint8, req plumbing.BACnet) (plumbing.BACnet, error) {
	ch, done := c.bvlcWaiter(bbmd, kind)
	defer done()
	for attempt := 0; attempt <= c.cfg.Retries; attempt++ {
		if err := c.send(bbmd, req); err != nil {
			return nil, err
		}
		timer := time.NewTimer(c.cfg.Timeout)
		select {
		case msg := <-ch:
			timer.Stop()
			if r, ok := msg.(*plumbing.BVLCResult); ok {
				return nil, &BVLCResultError{Code: r.ResultCode}
			}
			return msg, nil
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-c.closed:
			timer.Stop()
			return nil, ErrClosed
		}
	}
	return nil, ErrTimeout
}
