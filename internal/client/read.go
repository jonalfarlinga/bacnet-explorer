package client

import (
	"context"
	"errors"
	"fmt"

	"github.com/jonalfarlinga/bacnet/objects"
	"github.com/jonalfarlinga/bacnet/services"
)

// WildcardInstance in a Device object identifier means "this device".
const WildcardInstance = 4194303

// ReadSpec asks for some properties of one object.
type ReadSpec struct {
	Object ObjectID
	Props  []uint16
}

// PropertyResult is one property of a ReadPropertyMultiple reply: its value,
// or the error the device gave for it.
type PropertyResult struct {
	ID     uint32
	Index  *uint32
	Values []Value
	Err    error
}

// ObjectResult is the properties read from one object.
type ObjectResult struct {
	Object ObjectID
	Props  []PropertyResult
}

// ReadProperty reads one property. A property that is an array or a list
// comes back as several values.
func (c *Client) ReadProperty(ctx context.Context, to Address, obj ObjectID, prop uint16) ([]Value, error) {
	return c.readProperty(ctx, to, services.ConfirmedReadPropertyObjects(obj.Type, obj.Instance, prop))
}

// ReadPropertyIndex reads one element of an array property. Element 0 is the
// number of elements.
func (c *Client) ReadPropertyIndex(ctx context.Context, to Address, obj ObjectID, prop uint16, index uint32) ([]Value, error) {
	return c.readProperty(ctx, to, services.ConfirmedReadPropertyIndexedObjects(obj.Type, obj.Instance, prop, index))
}

func (c *Client) readProperty(ctx context.Context, to Address, payload []objects.APDUPayload) ([]Value, error) {
	tags, err := c.confirmed(ctx, to, services.ServiceConfirmedReadProperty, payload)
	if err != nil {
		return nil, err
	}
	// Object identifier [0], property [1] and perhaps an array index [2],
	// then the value between opening and closing tag 3.
	for i, t := range tags {
		if t.Kind != objects.TagOpening {
			continue
		}
		if t.TagNumber != 3 {
			break
		}
		values, rest, err := decodeUntilClose(tags[i+1:])
		if err != nil {
			return nil, fmt.Errorf("decoding the reply: %w", err)
		}
		if len(rest) != 1 || rest[0].TagNumber != 3 {
			break
		}
		return values, nil
	}
	return nil, errors.New("decoding the reply: no property value in it")
}

// ReadPropertyMultiple reads properties of several objects in one request.
// The reply has to fit in what the device can send, so the caller sizes its
// batches to the device.
func (c *Client) ReadPropertyMultiple(ctx context.Context, to Address, specs []ReadSpec) ([]ObjectResult, error) {
	var payload []objects.APDUPayload
	for _, s := range specs {
		payload = append(payload,
			services.ConfirmedReadPropertyMultipleObjects(s.Object.Type, s.Object.Instance, s.Props)...)
	}
	tags, err := c.confirmed(ctx, to, services.ServiceConfirmedReadPropMultiple, payload)
	if err != nil {
		return nil, err
	}
	results, err := decodeRPM(tags)
	if err != nil {
		return nil, fmt.Errorf("decoding the reply: %w", err)
	}
	return results, nil
}

// decodeRPM decodes a ReadPropertyMultiple ack: for each object its
// identifier [0] and, between tags 1, a run of property [2], perhaps array
// index [3], and either a value between tags 4 or an error between tags 5.
func decodeRPM(tags []*objects.Object) ([]ObjectResult, error) {
	var results []ObjectResult
	for len(tags) > 0 {
		if len(tags) < 2 || !isContext(tags[0], 0) || len(tags[0].Data) != 4 || !isOpening(tags[1], 1) {
			return nil, errors.New("expected an object identifier and its results")
		}
		res := ObjectResult{Object: objectIDOf(tags[0].Data)}
		tags = tags[2:]

		for {
			if len(tags) == 0 {
				return nil, errors.New("results are not closed")
			}
			if tags[0].Kind == objects.TagClosing && tags[0].TagNumber == 1 {
				tags = tags[1:]
				break
			}
			if !isContext(tags[0], 2) || len(tags[0].Data) == 0 || len(tags[0].Data) > 4 {
				return nil, errors.New("expected a property identifier")
			}
			prop := PropertyResult{ID: uint32(beUint(tags[0].Data))}
			tags = tags[1:]
			if len(tags) > 0 && isContext(tags[0], 3) && len(tags[0].Data) >= 1 && len(tags[0].Data) <= 4 {
				index := uint32(beUint(tags[0].Data))
				prop.Index = &index
				tags = tags[1:]
			}
			if len(tags) == 0 || tags[0].Kind != objects.TagOpening || (tags[0].TagNumber != 4 && tags[0].TagNumber != 5) {
				return nil, errors.New("expected a property value or error")
			}
			opened := tags[0].TagNumber
			values, rest, err := decodeUntilClose(tags[1:])
			if err != nil {
				return nil, err
			}
			if len(rest) == 0 || rest[0].TagNumber != opened {
				return nil, errors.New("a property value is not closed")
			}
			tags = rest[1:]

			if opened == 4 {
				prop.Values = values
			} else {
				e := &BACnetError{}
				if len(values) == 2 {
					e.Class, e.Code = uint32(values[0].Uint), uint32(values[1].Uint)
				}
				prop.Err = e
			}
			res.Props = append(res.Props, prop)
		}
		results = append(results, res)
	}
	return results, nil
}

func isContext(t *objects.Object, number uint8) bool {
	return t.TagClass && t.Kind == objects.TagPrimitive && t.TagNumber == number
}

func isOpening(t *objects.Object, number uint8) bool {
	return t.Kind == objects.TagOpening && t.TagNumber == number
}

// ReadObjectList reads the identifiers of every object in a device. It asks
// for the whole array first and, where the device declines that, as one that
// cannot segment does for a long list, reads it element by element. progress,
// if not nil, is told how many of how many it has in that case.
func (c *Client) ReadObjectList(ctx context.Context, dev Device, progress func(done, total int)) ([]ObjectID, error) {
	device := ObjectID{Type: objects.ObjectTypeDevice, Instance: dev.Instance}
	values, err := c.ReadProperty(ctx, dev.Addr, device, objects.PropertyIdObjectList)
	if err == nil {
		return objectIDs(values), nil
	}
	if !Declined(err) {
		return nil, err
	}

	values, err = c.ReadPropertyIndex(ctx, dev.Addr, device, objects.PropertyIdObjectList, 0)
	if err != nil {
		return nil, err
	}
	if len(values) != 1 || values[0].Kind != KindUnsigned {
		return nil, errors.New("the object list's length is not a number")
	}
	total := int(values[0].Uint)
	list := make([]ObjectID, 0, total)
	for i := 1; i <= total; i++ {
		values, err := c.ReadPropertyIndex(ctx, dev.Addr, device, objects.PropertyIdObjectList, uint32(i))
		if err != nil {
			return nil, fmt.Errorf("object list element %d: %w", i, err)
		}
		list = append(list, objectIDs(values)...)
		if progress != nil {
			progress(i, total)
		}
	}
	return list, nil
}

func objectIDs(values []Value) []ObjectID {
	ids := make([]ObjectID, 0, len(values))
	for _, v := range values {
		if v.Kind == KindObjectID {
			ids = append(ids, v.Object)
		}
	}
	return ids
}

var errorClassNames = []string{
	"device", "object", "property", "resources", "security", "services", "vt", "communication",
}

// ErrorClassName names a BACnetErrorClass.
func ErrorClassName(v uint32) string {
	if int(v) < len(errorClassNames) {
		return errorClassNames[v]
	}
	return fmt.Sprintf("class %d", v)
}

// Error codes worth telling apart from the rest.
const (
	ErrorCodeUnknownObject   = 31
	ErrorCodeUnknownProperty = 32
)

var errorCodeNames = map[uint32]string{
	0: "other", 2: "configuration in progress", 3: "device busy", 9: "inconsistent parameters",
	25: "operational problem", 27: "read access denied", 29: "service request denied", 30: "timeout",
	31: "unknown object", 32: "unknown property", 37: "value out of range", 40: "write access denied",
	42: "invalid array index", 44: "not a COV property", 45: "optional functionality not supported",
	50: "property is not an array",
}

// ErrorCodeName names a BACnetErrorCode.
func ErrorCodeName(v uint32) string {
	if name, ok := errorCodeNames[v]; ok {
		return name
	}
	return fmt.Sprintf("code %d", v)
}
