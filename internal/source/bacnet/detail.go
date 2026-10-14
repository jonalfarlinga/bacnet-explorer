package bacnet

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jonalfarlinga/bacnet/client"
	"github.com/jonalfarlinga/bacnet-explorer/internal/explorer"
	"github.com/jonalfarlinga/bacnet/objects"
)

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

// Detail implements explorer.Source: every property of an object, or of a
// device's Device object, read now.
func (s *Source) Detail(ctx context.Context, id explorer.NodeID) (explorer.Detail, error) {
	instance, obj, ok := parseID(id)
	if !ok {
		return explorer.Detail{}, explorer.ErrNotSupported
	}
	if obj == nil {
		obj = &client.ObjectID{Type: objects.ObjectTypeDevice, Instance: instance}
	}
	s.mu.Lock()
	d, c := s.devices[instance], s.c
	var dev client.Device
	if d != nil {
		dev = d.dev
	}
	s.mu.Unlock()
	if c == nil {
		return explorer.Detail{}, explorer.ErrNotOpen
	}
	if d == nil {
		return explorer.Detail{}, explorer.ErrUnknownNode
	}
	props, err := s.properties(ctx, c, dev, *obj)
	if err != nil {
		return explorer.Detail{}, err
	}
	return explorer.Detail{Rows: props, ReadAt: time.Now()}, nil
}

func (s *Source) properties(ctx context.Context, c *client.Client, dev client.Device, obj client.ObjectID) ([]explorer.DetailRow, error) {
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
func renderProperties(objType uint16, props []client.PropertyResult, guessed bool) []explorer.DetailRow {
	rows := make([]explorer.DetailRow, 0, len(props))
	for _, p := range props {
		row := explorer.DetailRow{Name: propertyName(p.ID)}
		if p.Index != nil {
			row.Name += fmt.Sprintf(" [%d]", *p.Index)
		}
		if p.Err != nil {
			if guessed {
				continue
			}
			row.Error = p.Err.Error()
		} else {
			row.Value, row.Items = formatProperty(objType, p.ID, p.Values)
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

// Read implements explorer.Source: the present value and status flags of
// some points, read now, in ReadPropertyMultiple batches per device.
func (s *Source) Read(ctx context.Context, ids []explorer.NodeID) ([]explorer.Reading, error) {
	s.mu.Lock()
	c := s.c
	s.mu.Unlock()
	if c == nil {
		return nil, explorer.ErrNotOpen
	}

	out := make([]explorer.Reading, len(ids))
	byDevice := map[uint32][]want{}
	for i, id := range ids {
		instance, obj, ok := parseID(id)
		if !ok || obj == nil {
			out[i] = explorer.Reading{Quality: explorer.QualityError, Text: "not a point", At: time.Now()}
			continue
		}
		byDevice[instance] = append(byDevice[instance], want{i, *obj})
	}

	for instance, wants := range byDevice {
		s.mu.Lock()
		d := s.devices[instance]
		s.mu.Unlock()
		if d == nil {
			for _, w := range wants {
				out[w.index] = explorer.Reading{Quality: explorer.QualityError, Text: "unknown device", At: time.Now()}
			}
			continue
		}
		batch := batchSize(d.dev)
		for start := 0; start < len(wants); start += batch {
			end := min(start+batch, len(wants))
			if err := s.readBatch(ctx, c, d.dev, wants[start:end], func(i int, r explorer.Reading) { out[i] = r }); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// want is one point to read: where its reading goes, and the object.
type want struct {
	index int
	obj   client.ObjectID
}

// readBatch reads the value of some objects of one device, with
// ReadPropertyMultiple where it works and ReadProperty otherwise.
func (s *Source) readBatch(ctx context.Context, c *client.Client, dev client.Device, wants []want, set func(int, explorer.Reading)) error {
	valueProps := func(t uint16) []uint16 {
		props := []uint16{objects.PropertyIdPresentValue, objects.PropertyIdStatusFlags}
		if hasUnits(t) {
			props = append(props, objects.PropertyIdUnits)
		}
		return props
	}
	toReading := func(o object) explorer.Reading {
		r := explorer.Reading{Text: o.value, Number: o.number, Units: o.units, Flags: o.flags, Quality: explorer.QualityGood, At: time.Now()}
		if o.err != "" || o.value == "" {
			r.Quality = explorer.QualityError
			if o.err != "" {
				r.Text = o.err
			}
		}
		return r
	}

	specs := make([]client.ReadSpec, len(wants))
	for i, w := range wants {
		specs[i] = client.ReadSpec{Object: w.obj, Props: valueProps(w.obj.Type)}
	}
	results, err := c.ReadMany(ctx, dev, specs)
	if err != nil {
		return err
	}
	for i, w := range wants {
		o := object{id: w.obj}
		for _, p := range results[i].Props {
			applyValue(&o, p.ID, p.Values, p.Err)
		}
		set(w.index, toReading(o))
	}
	return nil
}

// applyValue is applyProperty for a value read: an error on the present
// value is the point's error.
func applyValue(o *object, prop uint32, values []client.Value, err error) {
	if err != nil && prop == uint32(objects.PropertyIdPresentValue) {
		o.err = err.Error()
		return
	}
	applyProperty(o, prop, values, err)
}
