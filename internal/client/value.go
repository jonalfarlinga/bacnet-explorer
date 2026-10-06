package client

import (
	"encoding/binary"
	"fmt"
	"math"
	"unicode/utf16"

	"github.com/jonalfarlinga/bacnet/objects"
)

// Kind tells what a Value holds.
type Kind uint8

const (
	KindNull Kind = iota
	KindBoolean
	KindUnsigned
	KindSigned
	KindReal
	KindDouble
	KindOctets
	KindString
	KindBits
	KindEnumerated
	KindDate
	KindTime
	KindObjectID
	// KindContext is a context-tagged primitive. Its type depends on the
	// property it belongs to, so it is kept as bytes.
	KindContext
	// KindConstructed is what an opening and closing tag pair encloses.
	KindConstructed
)

// Value is one decoded BACnet value. Which field is set depends on Kind.
type Value struct {
	Kind Kind
	Tag  uint8 // context tag number, for KindContext and KindConstructed

	Bool   bool
	Uint   uint64  // KindUnsigned, KindEnumerated
	Int    int64   // KindSigned
	Float  float64 // KindReal, KindDouble
	Str    string
	Bytes  []byte // KindOctets, KindContext, and the 4 bytes of a date or time
	Bits   []bool
	Object ObjectID
	Items  []Value // KindConstructed
}

// decodeValues decodes a run of tags, such as the contents of a property
// value, into values. Nested opening and closing tags become KindConstructed.
func decodeValues(tags []*objects.Object) ([]Value, error) {
	values, rest, err := decodeUntilClose(tags)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("closing tag %d without an opening tag", rest[0].TagNumber)
	}
	return values, nil
}

// decodeUntilClose decodes tags up to a closing tag that nothing in the run
// opened, and returns what is left from that tag on.
func decodeUntilClose(tags []*objects.Object) ([]Value, []*objects.Object, error) {
	var values []Value
	for len(tags) > 0 {
		t := tags[0]
		switch t.Kind {
		case objects.TagClosing:
			return values, tags, nil
		case objects.TagOpening:
			items, rest, err := decodeUntilClose(tags[1:])
			if err != nil {
				return nil, nil, err
			}
			if len(rest) == 0 || rest[0].TagNumber != t.TagNumber {
				return nil, nil, fmt.Errorf("opening tag %d is not closed", t.TagNumber)
			}
			values = append(values, Value{Kind: KindConstructed, Tag: t.TagNumber, Items: items})
			tags = rest[1:]
		default:
			values = append(values, decodePrimitive(t))
			tags = tags[1:]
		}
	}
	return values, nil, nil
}

func decodePrimitive(t *objects.Object) Value {
	if t.TagClass {
		return Value{Kind: KindContext, Tag: t.TagNumber, Bytes: t.Data}
	}
	d := t.Data
	switch t.TagNumber {
	case objects.TagNull:
		return Value{Kind: KindNull}
	case objects.TagBoolean:
		return Value{Kind: KindBoolean, Bool: t.Length == 1}
	case objects.TagUnsignedInteger:
		if len(d) >= 1 && len(d) <= 8 {
			return Value{Kind: KindUnsigned, Uint: beUint(d)}
		}
	case objects.TagSignedInteger:
		if len(d) >= 1 && len(d) <= 8 {
			shift := uint(64 - 8*len(d))
			return Value{Kind: KindSigned, Int: int64(beUint(d)<<shift) >> shift}
		}
	case objects.TagReal:
		if len(d) == 4 {
			return Value{Kind: KindReal, Float: float64(math.Float32frombits(binary.BigEndian.Uint32(d)))}
		}
	case objects.TagDouble:
		if len(d) == 8 {
			return Value{Kind: KindDouble, Float: math.Float64frombits(binary.BigEndian.Uint64(d))}
		}
	case objects.TagOctetString:
		return Value{Kind: KindOctets, Bytes: d}
	case objects.TagCharacterString:
		if len(d) >= 1 {
			return Value{Kind: KindString, Str: decodeString(d[0], d[1:])}
		}
	case objects.TagBitString:
		if bits, err := objects.DecBitString(t); err == nil {
			return Value{Kind: KindBits, Bits: bits}
		}
	case objects.TagEnumerated:
		if len(d) >= 1 && len(d) <= 4 {
			return Value{Kind: KindEnumerated, Uint: beUint(d)}
		}
	case objects.TagDate:
		if len(d) == 4 {
			return Value{Kind: KindDate, Bytes: d}
		}
	case objects.TagTime:
		if len(d) == 4 {
			return Value{Kind: KindTime, Bytes: d}
		}
	case objects.TagBACnetObjectIdentifier:
		if len(d) == 4 {
			return Value{Kind: KindObjectID, Object: objectIDOf(d)}
		}
	}
	// A reserved tag, or data of the wrong length for its type.
	return Value{Kind: KindOctets, Bytes: d}
}

func beUint(d []byte) uint64 {
	var v uint64
	for _, b := range d {
		v = v<<8 | uint64(b)
	}
	return v
}

func objectIDOf(d []byte) ObjectID {
	v := binary.BigEndian.Uint32(d)
	return ObjectID{Type: uint16(v >> 22), Instance: v & 0x3FFFFF}
}

// decodeString decodes the character sets that turn up in practice: UTF-8
// (0), UCS-2 (4) and ISO 8859-1 (5). Anything else is taken as UTF-8.
func decodeString(charset uint8, d []byte) string {
	switch charset {
	case 4:
		units := make([]uint16, len(d)/2)
		for i := range units {
			units[i] = binary.BigEndian.Uint16(d[2*i:])
		}
		return string(utf16.Decode(units))
	case 5:
		runes := make([]rune, len(d))
		for i, b := range d {
			runes[i] = rune(b)
		}
		return string(runes)
	}
	return string(d)
}
