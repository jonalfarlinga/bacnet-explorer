package client

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"runtime"
)

// DefaultPort is the BACnet/IP port, 0xBAC0.
const DefaultPort = 47808

// Interface is a network adapter the client can use: one that is up, can
// broadcast, and has an IPv4 address.
type Interface struct {
	Name      string
	Addr      netip.Prefix // the adapter's address and the size of its subnet
	Broadcast netip.Addr
}

// Interfaces lists the adapters the client can use. An adapter with several
// IPv4 addresses is listed once for each.
func Interfaces() ([]Interface, error) {
	all, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var list []Interface
	for _, ifc := range all {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagBroadcast == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			ones, bits := ipnet.Mask.Size()
			if ip4 == nil || bits != 32 || ones > 30 {
				continue
			}
			addr := netip.AddrFrom4([4]byte(ip4))
			host := ^uint32(0) >> ones
			bcast := binary.BigEndian.Uint32(ip4) | host
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], bcast)
			list = append(list, Interface{
				Name:      ifc.Name,
				Addr:      netip.PrefixFrom(addr, ones),
				Broadcast: netip.AddrFrom4(b),
			})
		}
	}
	return list, nil
}

// FindInterface picks the adapter with the given name or IPv4 address.
func FindInterface(nameOrIP string) (Interface, error) {
	list, err := Interfaces()
	if err != nil {
		return Interface{}, err
	}
	for _, ifc := range list {
		if ifc.Name == nameOrIP || ifc.Addr.Addr().String() == nameOrIP {
			return ifc, nil
		}
	}
	return Interface{}, fmt.Errorf("no usable adapter is named or addressed %q", nameOrIP)
}

// Config returns the configuration that listens on the adapter and
// broadcasts to its subnet. Where the socket has to be bound to every
// address to hear broadcasts, the subnets of the other adapters are excluded
// so that only this adapter's traffic is taken.
func (i Interface) Config(port uint16) Config {
	cfg := Config{
		Local:     netip.AddrPortFrom(i.Addr.Addr(), port),
		BindAny:   runtime.GOOS != "windows",
		Broadcast: netip.AddrPortFrom(i.Broadcast, port),
	}
	if cfg.BindAny {
		if list, err := Interfaces(); err == nil {
			mine := i.Addr.Masked()
			for _, other := range list {
				if subnet := other.Addr.Masked(); subnet != mine {
					cfg.Exclude = append(cfg.Exclude, subnet)
				}
			}
		}
	}
	return cfg
}
