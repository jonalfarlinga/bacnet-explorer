//go:build windows

package survey

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// On Windows a ping goes through the ICMP API of the IP helper, which
// needs no privilege, as ping.exe does; a raw socket would need an
// administrator. Each ping opens a handle of its own, so that many can be
// in flight at once from as many goroutines.
var (
	iphlpapi        = syscall.NewLazyDLL("iphlpapi.dll")
	icmpCreateFile  = iphlpapi.NewProc("IcmpCreateFile")
	icmpCloseHandle = iphlpapi.NewProc("IcmpCloseHandle")
	icmpSendEcho    = iphlpapi.NewProc("IcmpSendEcho")
)

// The IP_STATUS values of an echo reply, from ipexport.h: those told
// apart, and the range that is a status rather than a failure of the call.
const (
	ipSuccess             = 0
	ipStatusBase          = 11000
	ipDestNetUnreachable  = 11002
	ipDestHostUnreachable = 11003
	ipStatusLast          = 11050
)

// echoReply is ICMP_ECHO_REPLY, what IcmpSendEcho writes for each reply:
// who answered, with what status.
type echoReply struct {
	address  uint32
	status   uint32
	rtt      uint32
	dataSize uint16
	reserved uint16
	data     uintptr
	options  struct {
		ttl, tos, flags, optionsSize uint8
		optionsData                  uintptr
	}
}

func ping(ctx context.Context, to netip.Addr, timeout time.Duration) (echo, error) {
	h, _, err := icmpCreateFile.Call()
	if h == uintptr(syscall.InvalidHandle) {
		return echo{}, fmt.Errorf("IcmpCreateFile: %w", err)
	}
	defer icmpCloseHandle.Call(h)

	// IPAddr is the address's four bytes in network order, read as the
	// machine's own integer.
	dest := to.As4()
	request := []byte("bacnet-explorer survey")
	// Room for the reply, the request echoed back, and the eight bytes an
	// ICMP error carries.
	reply := make([]byte, unsafe.Sizeof(echoReply{})+uintptr(len(request))+8)
	n, _, err := icmpSendEcho.Call(h,
		uintptr(binary.LittleEndian.Uint32(dest[:])),
		uintptr(unsafe.Pointer(&request[0])), uintptr(len(request)),
		0,
		uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)),
		uintptr(timeout.Milliseconds()))
	runtime.KeepAlive(request)
	var status uint32
	var from netip.Addr
	if n == 0 {
		// No reply: the error is the status, or the call itself failed.
		errno, ok := err.(syscall.Errno)
		if !ok || errno < ipStatusBase || errno > ipStatusLast {
			return echo{}, fmt.Errorf("IcmpSendEcho: %w", err)
		}
		status = uint32(errno)
	} else {
		r := (*echoReply)(unsafe.Pointer(&reply[0]))
		status = r.status
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], r.address)
		from = netip.AddrFrom4(b)
	}
	runtime.KeepAlive(reply)
	switch status {
	case ipSuccess:
		return echo{alive: true, from: from}, nil
	case ipDestHostUnreachable:
		return echo{noHost: true, from: from}, nil
	case ipDestNetUnreachable:
		return echo{noNet: true, from: from}, nil
	}
	return echo{}, nil
}
