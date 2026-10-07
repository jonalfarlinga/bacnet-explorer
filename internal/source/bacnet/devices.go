package bacnet

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/jonalfarlinga/bacnet/client"
	"github.com/jonalfarlinga/bacnet/internal/explorer"
	"github.com/jonalfarlinga/bacnet/objects"
)

// device is what is known about one device.
type device struct {
	dev   client.Device
	added bool // listed by its address, not heard

	// What identify read, and whether it has run.
	identified                                                             bool
	name, vendorName, modelName, firmware, software, location, description string
	infoErr                                                                string

	children explorer.ChildrenState
	objs     []object
	index    map[client.ObjectID]int
	cancel   context.CancelFunc // ends the reading of its objects
}

// native is what a device node carries so that the source can reach and
// show the device again after a restart, without hearing from it.
type native struct {
	Instance     uint32 `json:"instance"`
	IP           string `json:"ip"`
	Net          uint16 `json:"net,omitempty"`
	MAC          string `json:"mac,omitempty"` // hex
	MaxAPDU      uint16 `json:"maxApdu"`
	Segmentation uint8  `json:"segmentation"`
	VendorID     uint16 `json:"vendorId"`
	Added        bool   `json:"added,omitempty"`

	Name        string `json:"name,omitempty"`
	VendorName  string `json:"vendorName,omitempty"`
	ModelName   string `json:"modelName,omitempty"`
	Firmware    string `json:"firmware,omitempty"`
	Software    string `json:"software,omitempty"`
	Location    string `json:"location,omitempty"`
	Description string `json:"description,omitempty"`
}

func (d *device) native() native {
	return native{
		Instance: d.dev.Instance, IP: d.dev.Addr.IP.String(), Net: d.dev.Addr.Net, MAC: hex.EncodeToString(d.dev.Addr.MAC),
		MaxAPDU: d.dev.MaxAPDU, Segmentation: d.dev.Segmentation, VendorID: d.dev.VendorID, Added: d.added,
		Name: d.name, VendorName: d.vendorName, ModelName: d.modelName, Firmware: d.firmware,
		Software: d.software, Location: d.location, Description: d.description,
	}
}

// restore makes the device record a native describes.
func (n native) restore() (*device, error) {
	ip, err := netip.ParseAddrPort(n.IP)
	if err != nil {
		return nil, fmt.Errorf("device %d: address %q: %w", n.Instance, n.IP, err)
	}
	mac, err := hex.DecodeString(n.MAC)
	if err != nil {
		return nil, fmt.Errorf("device %d: MAC %q: %w", n.Instance, n.MAC, err)
	}
	if n.Net == 0 {
		mac = nil
	}
	return &device{
		dev: client.Device{
			Instance: n.Instance, Addr: client.Address{IP: ip, Net: n.Net, MAC: mac},
			MaxAPDU: n.MaxAPDU, Segmentation: n.Segmentation, VendorID: n.VendorID,
		},
		added: n.Added, identified: true,
		name: n.Name, vendorName: n.VendorName, modelName: n.ModelName, firmware: n.Firmware,
		software: n.Software, location: n.Location, description: n.Description,
		children: explorer.ChildrenState{State: explorer.ChildrenUnknown},
	}, nil
}

// Restore implements explorer.Source: devices the store held are known
// again, by address, so that their objects can be read and their networks
// placed. One that was listed by address is read again now, since no Who-Is
// will reach it; the others are heard when they answer the Who-Is.
func (s *Source) Restore(ctx context.Context, nodes []explorer.Node) error {
	var (
		events  []explorer.Event
		reprobe []client.Device
	)
	s.mu.Lock()
	c := s.c
	for _, n := range nodes {
		if n.Kind != explorer.KindDevice || len(n.Native) == 0 {
			continue
		}
		var nat native
		if err := json.Unmarshal(n.Native, &nat); err != nil {
			continue
		}
		if _, known := s.devices[nat.Instance]; known {
			continue
		}
		d, err := nat.restore()
		if err != nil {
			continue
		}
		s.devices[nat.Instance] = d
		if d.dev.Addr.Routed() {
			events = append(events, s.placeNetwork(d.dev.Addr.Net, d.dev.Addr.IP)...)
		}
		if d.added {
			reprobe = append(reprobe, d.dev)
		}
	}
	s.mu.Unlock()
	for _, e := range events {
		s.emit(e)
	}
	if c != nil {
		for _, dev := range reprobe {
			go func() {
				if probed, err := c.Probe(ctx, dev.Addr, dev.Instance); err == nil {
					s.record(probed, true)
				}
			}()
		}
	}
	return nil
}

// ---- node IDs --------------------------------------------------------------

func devID(instance uint32) explorer.NodeID {
	return explorer.NodeID(fmt.Sprintf("%s:dev/%d", Kind, instance))
}

func objID(instance uint32, id client.ObjectID) explorer.NodeID {
	return explorer.NodeID(fmt.Sprintf("%s:dev/%d/obj/%d/%d", Kind, instance, id.Type, id.Instance))
}

func netID(network uint16) explorer.NodeID {
	return explorer.NodeID(fmt.Sprintf("%s:net/%d", Kind, network))
}

func routerID(router netip.AddrPort) explorer.NodeID {
	return explorer.NodeID(fmt.Sprintf("%s:router/%s", Kind, router))
}

// parseID reads a node ID of this source. For a device it gives the
// instance; for an object, the instance and the object.
func parseID(id explorer.NodeID) (instance uint32, obj *client.ObjectID, ok bool) {
	rest, found := strings.CutPrefix(string(id), Kind+":dev/")
	if !found {
		return 0, nil, false
	}
	parts := strings.Split(rest, "/")
	inst, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return 0, nil, false
	}
	switch len(parts) {
	case 1:
		return uint32(inst), nil, true
	case 4:
		if parts[1] != "obj" {
			return 0, nil, false
		}
		typ, err1 := strconv.ParseUint(parts[2], 10, 16)
		oi, err2 := strconv.ParseUint(parts[3], 10, 32)
		if err1 != nil || err2 != nil {
			return 0, nil, false
		}
		return uint32(inst), &client.ObjectID{Type: uint16(typ), Instance: uint32(oi)}, true
	}
	return 0, nil, false
}

// ---- learning of devices ---------------------------------------------------

// onIAm runs in the client's read loop.
func (s *Source) onIAm(dev client.Device) {
	s.record(dev, false)
}

// record takes in what is known about a device, from its I-Am or from
// reading it, and returns its node.
func (s *Source) record(dev client.Device, added bool) explorer.Node {
	s.mu.Lock()
	if s.c == nil {
		s.mu.Unlock()
		return explorer.Node{}
	}
	d, known := s.devices[dev.Instance]
	if !known {
		d = &device{children: explorer.ChildrenState{State: explorer.ChildrenUnknown}}
		s.devices[dev.Instance] = d
	}
	d.dev = dev
	d.added = d.added || added

	// A routed device hangs off its network, which hangs off its router.
	var events []explorer.Event
	if dev.Addr.Routed() {
		events = append(events, s.placeNetwork(dev.Addr.Net, dev.Addr.IP)...)
	} else if s.routers[dev.Addr.IP] {
		// The router has announced itself: its networks move under it, and
		// the stand-in for it goes.
		delete(s.routers, dev.Addr.IP)
		for net, router := range s.networks {
			if router == dev.Addr.IP {
				events = append(events, explorer.Event{Type: "node", Data: s.networkNode(net, router)})
			}
		}
		events = append(events, explorer.Event{Type: "remove", Data: routerID(dev.Addr.IP)})
	}
	n := s.deviceNode(dev.Instance, d)
	events = append(events, explorer.Event{Type: "node", Data: n})
	ctx, c := s.ctx, s.c
	s.mu.Unlock()

	for _, e := range events {
		s.emit(e)
	}
	if !known {
		go s.identify(ctx, c, dev)
	}
	return n
}

// placeNetwork makes sure the network, and the router it is behind, have
// nodes. The caller holds s.mu.
func (s *Source) placeNetwork(network uint16, router netip.AddrPort) []explorer.Event {
	var events []explorer.Event
	if old, ok := s.networks[network]; ok && old == router {
		return nil
	}
	s.networks[network] = router
	if !s.routerKnown(router) && !s.routers[router] {
		s.routers[router] = true
		events = append(events, explorer.Event{Type: "node", Data: explorer.Node{
			ID: routerID(router), Kind: explorer.KindGroup, Label: "Router", Sublabel: router.String(),
			Order: 1 << 30, Children: explorer.ChildrenState{State: explorer.ChildrenReady},
		}})
	}
	return append(events, explorer.Event{Type: "node", Data: s.networkNode(network, router)})
}

// routerKnown reports whether a device at the address has announced itself.
// The caller holds s.mu.
func (s *Source) routerKnown(router netip.AddrPort) bool {
	for _, d := range s.devices {
		if !d.dev.Addr.Routed() && d.dev.Addr.IP == router {
			return true
		}
	}
	return false
}

// routerNode is the node a network hangs off: the router's device if it has
// announced itself, the stand-in otherwise. The caller holds s.mu.
func (s *Source) routerNode(router netip.AddrPort) explorer.NodeID {
	for inst, d := range s.devices {
		if !d.dev.Addr.Routed() && d.dev.Addr.IP == router {
			return devID(inst)
		}
	}
	return routerID(router)
}

func (s *Source) networkNode(network uint16, router netip.AddrPort) explorer.Node {
	return explorer.Node{
		ID: netID(network), Parent: s.routerNode(router), Kind: explorer.KindGroup,
		Label: fmt.Sprintf("Network %d", network), Order: -1<<20 + int(network), // ahead of the router's own points
		Children: explorer.ChildrenState{State: explorer.ChildrenReady},
	}
}

// deviceNode renders a device. The caller holds s.mu.
func (s *Source) deviceNode(instance uint32, d *device) explorer.Node {
	addr := d.dev.Addr
	n := explorer.Node{
		ID:       devID(instance),
		Kind:     explorer.KindDevice,
		Label:    d.name,
		Sublabel: "#" + strconv.FormatUint(uint64(instance), 10),
		Order:    int(instance),
		Children: d.children,
		Pending:  !d.identified,
		Error:    d.infoErr,
	}
	if n.Label == "" {
		n.Label = fmt.Sprintf("Device %d", instance)
	}
	address := explorer.Fact{Label: "Address", Value: addr.IP.String(), Mono: true}
	if addr.Routed() {
		n.Parent = netID(addr.Net)
		n.Meta = "MAC " + client.MACString(addr.MAC)
		address.Value = fmt.Sprintf("network %d, MAC %s, through the router at %s", addr.Net, client.MACString(addr.MAC), addr.IP)
		address.Mono = false
	} else {
		n.Meta = addr.IP.String()
	}
	n.Facts = append(n.Facts, address)
	if d.added {
		n.Facts = append(n.Facts, explorer.Fact{Label: "Listed", Value: "by its address; it did not answer the Who-Is"})
	}
	vendor := fmt.Sprintf("Vendor %d", d.dev.VendorID)
	if d.vendorName != "" {
		vendor = fmt.Sprintf("%s (%d)", d.vendorName, d.dev.VendorID)
	}
	n.Facts = append(n.Facts,
		explorer.Fact{Label: "Vendor", Value: vendor},
		explorer.Fact{Label: "Model", Value: d.modelName},
		explorer.Fact{Label: "Firmware", Value: d.firmware},
		explorer.Fact{Label: "Application software", Value: d.software},
		explorer.Fact{Label: "Location", Value: d.location},
		explorer.Fact{Label: "Description", Value: d.description},
		explorer.Fact{Label: "Max APDU", Value: fmt.Sprintf("%d bytes", d.dev.MaxAPDU)},
		explorer.Fact{Label: "Segmentation", Value: segmentationName(d.dev.Segmentation)},
	)
	n.Facts = nonEmpty(n.Facts)
	if b, err := json.Marshal(d.native()); err == nil {
		n.Native = b
	}
	return n
}

func nonEmpty(facts []explorer.Fact) []explorer.Fact {
	out := facts[:0]
	for _, f := range facts {
		if f.Value != "" {
			out = append(out, f)
		}
	}
	return out
}

var segmentationNames = []string{"both", "transmit only", "receive only", "none"}

func segmentationName(v uint8) string {
	if int(v) < len(segmentationNames) {
		return segmentationNames[v]
	}
	return fmt.Sprint(v)
}

// identify reads the properties that name a device.
func (s *Source) identify(ctx context.Context, c *client.Client, dev client.Device) {
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
	d := s.devices[dev.Instance]
	if d == nil {
		s.mu.Unlock()
		return
	}
	d.identified = true
	d.name = got[uint32(objects.PropertyIdObjectName)]
	d.vendorName = got[uint32(objects.PropertyIdVendorName)]
	d.modelName = got[uint32(objects.PropertyIdModelName)]
	d.firmware = got[uint32(objects.PropertyIdFirmwareRevision)]
	d.software = got[uint32(objects.PropertyIdApplicationSoftwareVersion)]
	d.location = got[uint32(objects.PropertyIdLocation)]
	d.description = got[uint32(objects.PropertyIdDescription)]
	d.infoErr = ""
	if failure != nil {
		d.infoErr = "could not read the device's name and model: " + failure.Error()
	}
	n := s.deviceNode(dev.Instance, d)
	s.mu.Unlock()
	s.emit(explorer.Event{Type: "node", Data: n})
}
