package client

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/jonalfarlinga/bacnet/objects"
	"github.com/jonalfarlinga/bacnet/plumbing"
	"github.com/jonalfarlinga/bacnet/services"
)

// The client must refuse every service that is not a read.
func TestSendRefusesAllButReads(t *testing.T) {
	c, err := Open(Config{Local: netip.MustParseAddrPort("127.0.0.1:0")})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Anything that did get through would go to the client's own socket.
	to := Address{IP: c.Local()}

	for service := 0; service < 256; service++ {
		want := service == int(services.ServiceConfirmedReadProperty) ||
			service == int(services.ServiceConfirmedReadPropMultiple)
		a := plumbing.NewAPDU(plumbing.ConfirmedReq, uint8(service), nil)
		err := c.sendAPDU(plumbing.BVLCFuncUnicast, to.IP, npduTo(to, true), a)
		if got := !errors.Is(err, ErrServiceNotAllowed); got != want {
			t.Errorf("confirmed service %d: sent %t, want %t", service, got, want)
		}
	}
	for service := 0; service < 256; service++ {
		want := service == int(services.ServiceUnconfirmedWhoIs)
		a := plumbing.NewAPDU(plumbing.UnConfirmedReq, uint8(service), nil)
		err := c.sendAPDU(plumbing.BVLCFuncUnicast, to.IP, npduTo(to, false), a)
		if got := !errors.Is(err, ErrServiceNotAllowed); got != want {
			t.Errorf("unconfirmed service %d: sent %t, want %t", service, got, want)
		}
	}
	for _, pduType := range []uint8{plumbing.SimpleAck, plumbing.ComplexAck, plumbing.Error, plumbing.Reject, plumbing.Abort} {
		err := c.sendAPDU(plumbing.BVLCFuncUnicast, to.IP, npduTo(to, false), &plumbing.APDU{Type: pduType})
		if !errors.Is(err, ErrServiceNotAllowed) {
			t.Errorf("PDU type %d was sent: the client answers nothing", pduType)
		}
	}

	// A read may go out as a unicast, a broadcast or a distribute-broadcast,
	// and in no other envelope: the client is not a BBMD.
	read := plumbing.NewAPDU(plumbing.ConfirmedReq, services.ServiceConfirmedReadProperty, nil)
	for function := 0; function < 256; function++ {
		want := function == plumbing.BVLCFuncUnicast || function == plumbing.BVLCFuncBroadcast ||
			function == plumbing.BVLCFuncDistributeBroadcastToNetwork
		err := c.sendAPDU(uint8(function), to.IP, npduTo(to, true), read)
		if got := !errors.Is(err, ErrServiceNotAllowed); got != want {
			t.Errorf("BVLC function %#x: sent %t, want %t", function, got, want)
		}
	}

	// Of the network layer messages, only Who-Is-Router-To-Network.
	for msgType := 0; msgType < 256; msgType++ {
		want := msgType == int(plumbing.NetMsgWhoIsRouterToNetwork)
		m := plumbing.NewNetworkMessage(plumbing.NewBVLC(plumbing.BVLCFuncUnicast),
			plumbing.NewNPDU(false, false, false, false), uint8(msgType), nil)
		err := c.send(to.IP, m)
		if got := !errors.Is(err, ErrServiceNotAllowed); got != want {
			t.Errorf("network layer message %#x: sent %t, want %t", msgType, got, want)
		}
	}

	// Of the BVLC functions, only the registration and the table reads.
	for _, tc := range []struct {
		msg  plumbing.BACnet
		want bool
	}{
		{plumbing.NewRegisterForeignDevice(60), true},
		{plumbing.NewReadForeignDeviceTable(), true},
		{plumbing.NewReadBroadcastDistributionTable(), true},
		{plumbing.NewBVLCResult(0), false},
		{plumbing.NewReadForeignDeviceTableAck(nil), false},
		{plumbing.NewReadBroadcastDistributionTableAck(nil), false},
	} {
		err := c.send(to.IP, tc.msg)
		if got := !errors.Is(err, ErrServiceNotAllowed); got != tc.want {
			t.Errorf("%T: sent %t, want %t", tc.msg, got, tc.want)
		}
	}
}

func TestDecodeRPM(t *testing.T) {
	// Two objects: a value, a value at an array index, an error, and a
	// constructed value with a nested one.
	tags := []*objects.Object{
		objects.EncObjectIdentifier(true, 0, objects.ObjectTypeAnalogInput, 7),
		objects.EncOpeningTag(1),
		objects.ContextTag(2, objects.EncUnsignedInteger(uint(objects.PropertyIdPresentValue))),
		objects.EncOpeningTag(4), objects.EncReal(21.5), objects.EncClosingTag(4),
		objects.ContextTag(2, objects.EncUnsignedInteger(uint(objects.PropertyIdPriorityArray))),
		objects.ContextTag(3, objects.EncUnsignedInteger(16)),
		objects.EncOpeningTag(4), objects.EncNull(), objects.EncClosingTag(4),
		objects.ContextTag(2, objects.EncUnsignedInteger(uint(objects.PropertyIdStateText))),
		objects.EncOpeningTag(5), objects.EncEnumerated(2), objects.EncEnumerated(32), objects.EncClosingTag(5),
		objects.EncClosingTag(1),

		objects.EncObjectIdentifier(true, 0, objects.ObjectTypeDevice, 9),
		objects.EncOpeningTag(1),
		objects.ContextTag(2, objects.EncUnsignedInteger(uint(objects.PropertyIdLastRestoreTime))),
		objects.EncOpeningTag(4),
		objects.EncOpeningTag(2),
		objects.EncOpeningTag(4), objects.EncString("inner"), objects.EncClosingTag(4),
		objects.EncClosingTag(2),
		objects.EncClosingTag(4),
		objects.EncClosingTag(1),
	}
	results, err := decodeRPM(tags)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || len(results[0].Props) != 3 || len(results[1].Props) != 1 {
		t.Fatalf("decoded %+v, want 2 objects with 3 and 1 properties", results)
	}

	p := results[0].Props
	if p[0].Values[0].Kind != KindReal || p[0].Values[0].Float != 21.5 {
		t.Errorf("present-value is %+v, want 21.5", p[0].Values)
	}
	if p[1].Index == nil || *p[1].Index != 16 || p[1].Values[0].Kind != KindNull {
		t.Errorf("priority-array is %+v at index %v, want null at 16", p[1].Values, p[1].Index)
	}
	var be *BACnetError
	if !errors.As(p[2].Err, &be) || be.Class != 2 || be.Code != ErrorCodeUnknownProperty {
		t.Errorf("state-text gave %v, want property: unknown property", p[2].Err)
	}

	outer := results[1].Props[0].Values
	if len(outer) != 1 || outer[0].Kind != KindConstructed || outer[0].Tag != 2 {
		t.Fatalf("constructed value is %+v, want one value under tag 2", outer)
	}
	inner := outer[0].Items
	if len(inner) != 1 || inner[0].Kind != KindConstructed || inner[0].Tag != 4 || inner[0].Items[0].Str != "inner" {
		t.Errorf("nested value is %+v, want a string under tag 4", inner)
	}

	for name, cut := range map[string][]*objects.Object{
		"unclosed results": tags[:6],
		"unclosed value":   tags[:5],
		"no results":       tags[:1],
	} {
		if _, err := decodeRPM(cut); err == nil {
			t.Errorf("%s: decoded without an error", name)
		}
	}
}

func TestDecodePrimitive(t *testing.T) {
	for _, tc := range []struct {
		name string
		tag  *objects.Object
		want func(Value) bool
	}{
		{"negative integer", objects.EncSignedInteger(-300), func(v Value) bool { return v.Kind == KindSigned && v.Int == -300 }},
		{"unsigned of 8 bytes", &objects.Object{TagNumber: objects.TagUnsignedInteger, Data: []byte{1, 0, 0, 0, 0, 0, 0, 0}, Length: 8},
			func(v Value) bool { return v.Kind == KindUnsigned && v.Uint == 1<<56 }},
		{"UCS-2 string", &objects.Object{TagNumber: objects.TagCharacterString, Data: []byte{4, 0, 'O', 0, 'K'}, Length: 5},
			func(v Value) bool { return v.Kind == KindString && v.Str == "OK" }},
		{"Latin-1 string", &objects.Object{TagNumber: objects.TagCharacterString, Data: []byte{5, 0xB0, 'F'}, Length: 3},
			func(v Value) bool { return v.Kind == KindString && v.Str == "°F" }},
		{"boolean", objects.EncBoolean(true), func(v Value) bool { return v.Kind == KindBoolean && v.Bool }},
		{"real of the wrong length", &objects.Object{TagNumber: objects.TagReal, Data: []byte{1, 2}, Length: 2},
			func(v Value) bool { return v.Kind == KindOctets && len(v.Bytes) == 2 }},
	} {
		if got := decodePrimitive(tc.tag); !tc.want(got) {
			t.Errorf("%s decoded as %+v", tc.name, got)
		}
	}
}
