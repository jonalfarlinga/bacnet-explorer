package sim

import (
	"fmt"
	"math"
	"time"

	"github.com/jonalfarlinga/bacnet/objects"
)

const (
	vendorID   = 999
	vendorName = "Simulated Controls"
)

// Short names for the object types used below.
const (
	typeAI  = objects.ObjectTypeAnalogInput
	typeAO  = objects.ObjectTypeAnalogOutput
	typeAV  = objects.ObjectTypeAnalogValue
	typeBI  = objects.ObjectTypeBinaryInput
	typeBO  = objects.ObjectTypeBinaryOutput
	typeBV  = objects.ObjectTypeBinaryValue
	typeMSV = objects.ObjectTypeMultiStateValue
)

func one(t *objects.Object) func() []*objects.Object {
	return func() []*objects.Object { return []*objects.Object{t} }
}

func text(s string) func() []*objects.Object { return one(objects.EncString(s)) }
func enum(v uint32) func() []*objects.Object { return one(objects.EncEnumerated(v)) }
func number(v uint) func() []*objects.Object { return one(objects.EncUnsignedInteger(v)) }
func flag(v bool) func() []*objects.Object   { return one(objects.EncBoolean(v)) }
func bits(v ...bool) func() []*objects.Object {
	return one(objects.EncBitString(v))
}

// wave is a value that drifts around mid by up to swing, on a period of its
// own so that no two points move together.
func wave(mid, swing float64, seed int) func() float64 {
	period := 40 + float64(seed%7)*13
	return func() float64 {
		t := float64(time.Now().UnixMilli()) / 1000
		return mid + swing*math.Sin(2*math.Pi*t/period+float64(seed))
	}
}

func steady(v float64) func() float64 { return func() float64 { return v } }

// add puts an object in the device, with the properties every object has
// ahead of the ones given.
func (d *Device) add(typ uint16, inst uint32, name, description string, props ...property) *object {
	id := objects.ObjectIdentifier{ObjectType: typ, InstanceNumber: inst}
	o := &object{id: id}
	o.props = append(o.props,
		property{id: objects.PropertyIdObjectIdentifier, get: one(objectID(id))},
		property{id: objects.PropertyIdObjectName, get: text(name)},
		property{id: objects.PropertyIdObjectType, get: enum(uint32(typ))},
		property{id: objects.PropertyIdDescription, get: text(description)},
	)
	o.props = append(o.props, props...)
	d.objs = append(d.objs, o)
	return o
}

// point is the properties of an object with a present value.
func point(value func() []*objects.Object, inAlarm bool) []property {
	eventState := uint32(0)
	if inAlarm {
		eventState = 2 // offnormal
	}
	return []property{
		{id: objects.PropertyIdPresentValue, get: value},
		{id: objects.PropertyIdStatusFlags, get: bits(inAlarm, false, false, false)},
		{id: objects.PropertyIdEventState, get: enum(eventState)},
		{id: objects.PropertyIdReliability, get: enum(0)},
		{id: objects.PropertyIdOutOfService, get: flag(false)},
	}
}

func (d *Device) analog(typ uint16, inst uint32, name, description string, units uint16, value func() float64) {
	real := func() []*objects.Object { return []*objects.Object{objects.EncReal(float32(value()))} }
	props := append(point(real, false), property{id: objects.PropertyIdUnits, get: enum(uint32(units))})
	if typ != typeAI {
		props = append(props,
			property{id: objects.PropertyIdPriorityArray, array: true, get: func() []*objects.Object {
				slots := make([]*objects.Object, 16)
				for i := range slots {
					slots[i] = objects.EncNull()
				}
				slots[15] = objects.EncReal(float32(value()))
				return slots
			}},
			property{id: objects.PropertyIdRelinquishDefault, get: real},
		)
	}
	d.add(typ, inst, name, description, props...)
}

func (d *Device) binary(typ uint16, inst uint32, name, description string, active bool, inactiveText, activeText string, inAlarm bool) {
	v := uint32(0)
	if active {
		v = 1
	}
	props := append(point(enum(v), inAlarm),
		property{id: objects.PropertyIdPolarity, get: enum(0)},
		property{id: objects.PropertyIdInactiveText, get: text(inactiveText)},
		property{id: objects.PropertyIdActiveText, get: text(activeText)},
	)
	d.add(typ, inst, name, description, props...)
}

func (d *Device) multiState(inst uint32, name, description string, state uint, states ...string) {
	props := append(point(number(state), false),
		property{id: objects.PropertyIdNumberOfStates, get: number(uint(len(states)))},
		property{id: objects.PropertyIdStateText, array: true, get: func() []*objects.Object {
			list := make([]*objects.Object, len(states))
			for i, s := range states {
				list[i] = objects.EncString(s)
			}
			return list
		}},
	)
	d.add(typeMSV, inst, name, description, props...)
}

// finish adds the Device object, which lists the others, and puts it first.
func (d *Device) finish() *Device {
	now := func() time.Time { return time.Now() }
	services := make([]bool, 40)
	services[12], services[26], services[34] = true, true, true // readProperty, i-Am, who-Is
	services[14] = !d.NoRPM                                     // readPropertyMultiple
	types := make([]bool, 30)
	for _, o := range d.objs {
		if int(o.id.ObjectType) < len(types) {
			types[o.id.ObjectType] = true
		}
	}
	types[objects.ObjectTypeDevice] = true

	dev := d.add(objects.ObjectTypeDevice, d.Instance, d.Name, d.Description,
		property{id: objects.PropertyIdSystemStatus, get: enum(0)},
		property{id: objects.PropertyIdVendorName, get: text(vendorName)},
		property{id: objects.PropertyIdVendorIdentifier, get: number(vendorID)},
		property{id: objects.PropertyIdModelName, get: text(d.Model)},
		property{id: objects.PropertyIdFirmwareRevision, get: text("sim 1.0")},
		property{id: objects.PropertyIdApplicationSoftwareVersion, get: text("1.0")},
		property{id: objects.PropertyIdLocation, get: text("Loopback")},
		property{id: objects.PropertyIdProtocolVersion, get: number(1)},
		property{id: objects.PropertyIdProtocolRevision, get: number(14)},
		property{id: objects.PropertyIdProtocolServicesSupported, get: bits(services...)},
		property{id: objects.PropertyIdProtocolObjectTypesSupported, get: bits(types...)},
		property{id: objects.PropertyIdMaxApduLengthAccepted, get: number(uint(d.MaxAPDU))},
		property{id: objects.PropertyIdSegmentationSupported, get: enum(uint32(d.Segmentation))},
		property{id: objects.PropertyIdLocalDate, get: func() []*objects.Object {
			t := now()
			weekday := byte(t.Weekday())
			if weekday == 0 {
				weekday = 7
			}
			data := []byte{byte(t.Year() - 1900), byte(t.Month()), byte(t.Day()), weekday}
			return []*objects.Object{{TagNumber: objects.TagDate, Data: data, Length: 4}}
		}},
		property{id: objects.PropertyIdLocalTime, get: func() []*objects.Object {
			t := now()
			data := []byte{byte(t.Hour()), byte(t.Minute()), byte(t.Second()), byte(t.Nanosecond() / 1e7)}
			return []*objects.Object{{TagNumber: objects.TagTime, Data: data, Length: 4}}
		}},
		property{id: objects.PropertyIdObjectList, array: true, get: func() []*objects.Object {
			list := make([]*objects.Object, len(d.objs))
			for i, o := range d.objs {
				list[i] = objectID(o.id)
			}
			return list
		}},
	)
	d.objs = append([]*object{dev}, d.objs[:len(d.objs)-1]...)
	return d
}

func defaultStations() [][]*Device {
	return [][]*Device{
		{airHandler()},
		{plantRouter(), terminalUnit(2101, 5, "VAV-2-01"), terminalUnit(2102, 6, "VAV-2-02"), terminalUnit(2107, 12, "FCU-2-07")},
		{meterGateway()},
		{legacyPanel()},
	}
}

// airHandler is an ordinary controller: everything fits in one APDU.
func airHandler() *Device {
	d := &Device{
		Instance: 1001, Name: "AHU-1", Model: "SIM-AHU", Description: "Air handler, level 1",
		MaxAPDU: 1476, Segmentation: SegBoth,
	}
	d.analog(typeAI, 0, "AHU1.SAT", "Supply air temperature", objects.UnitDegreeFahrenheit, wave(55.2, 1.5, 1))
	d.analog(typeAI, 1, "AHU1.RAT", "Return air temperature", objects.UnitDegreeFahrenheit, wave(72.4, 0.8, 2))
	d.analog(typeAI, 2, "AHU1.MAT", "Mixed air temperature", objects.UnitDegreeFahrenheit, wave(63.1, 2.2, 3))
	d.analog(typeAI, 3, "AHU1.OAT", "Outside air temperature", objects.UnitDegreeFahrenheit, wave(81.6, 0.4, 4))
	d.analog(typeAI, 4, "AHU1.RAH", "Return air humidity", objects.UnitPercentRelativeHumidity, wave(46, 3, 5))
	d.analog(typeAI, 5, "AHU1.DSP", "Duct static pressure", objects.UnitInchOfWater, wave(1.25, 0.08, 6))
	d.analog(typeAI, 6, "AHU1.CO2", "Return air CO2", objects.UnitPartsPerMillion, wave(640, 55, 7))
	d.analog(typeAO, 0, "AHU1.CHW.VLV", "Chilled water valve", objects.UnitPercent, wave(42, 9, 8))
	d.analog(typeAO, 1, "AHU1.HW.VLV", "Hot water valve", objects.UnitPercent, steady(0))
	d.analog(typeAO, 2, "AHU1.OA.DMP", "Outside air damper", objects.UnitPercent, wave(25, 5, 9))
	d.analog(typeAO, 3, "AHU1.SF.SPD", "Supply fan speed", objects.UnitPercent, wave(71, 4, 10))
	d.analog(typeAV, 0, "AHU1.SAT.SP", "Supply air temperature setpoint", objects.UnitDegreeFahrenheit, steady(55))
	d.analog(typeAV, 1, "AHU1.DSP.SP", "Duct static pressure setpoint", objects.UnitInchOfWater, steady(1.25))
	d.analog(typeAV, 2, "AHU1.OA.MIN", "Minimum outside air", objects.UnitPercent, steady(20))
	d.binary(typeBI, 0, "AHU1.SF.STS", "Supply fan status", true, "Off", "On", false)
	d.binary(typeBI, 1, "AHU1.FLT.DP", "Filter differential pressure switch", true, "Clean", "Dirty", true)
	d.binary(typeBI, 2, "AHU1.FRZ", "Freeze stat", false, "Normal", "Tripped", false)
	d.binary(typeBO, 0, "AHU1.SF.SS", "Supply fan start/stop", true, "Stop", "Start", false)
	d.binary(typeBV, 0, "AHU1.OCC", "Occupied", true, "Unoccupied", "Occupied", false)
	d.multiState(0, "AHU1.MODE", "Operating mode", 2, "Off", "Cooling", "Heating", "Economizer")
	d.add(objects.ObjectTypeSchedule, 0, "AHU1.SCHED", "Occupancy schedule")
	d.add(objects.ObjectTypeTrendLog, 0, "AHU1.SAT.TL", "Supply air temperature, 5 minutes")
	d.add(objects.ObjectTypeTrendLog, 1, "AHU1.DSP.TL", "Duct static pressure, 5 minutes")
	d.add(objects.ObjectTypeNotificationClass, 0, "AHU1.ALARMS", "Alarm routing")
	return d.finish()
}

// plantRouter has an object list too long for one of its APDUs, which it
// segments, and three devices on an MS/TP network behind it.
func plantRouter() *Device {
	d := &Device{
		Instance: 2000, Name: "CHW-PLANT", Model: "SIM-ROUTER", Description: "Chilled water plant controller and MS/TP router",
		MaxAPDU: 480, Segmentation: SegBoth,
	}
	for i := uint32(0); i < 4; i++ {
		n := i + 1
		d.analog(typeAI, i*10, fmt.Sprintf("CH%d.CHWS.T", n), fmt.Sprintf("Chiller %d supply temperature", n), objects.UnitDegreeFahrenheit, wave(44, 0.6, int(i)))
		d.analog(typeAI, i*10+1, fmt.Sprintf("CH%d.CHWR.T", n), fmt.Sprintf("Chiller %d return temperature", n), objects.UnitDegreeFahrenheit, wave(54.5, 1.1, int(i)+3))
		d.analog(typeAI, i*10+2, fmt.Sprintf("CH%d.KW", n), fmt.Sprintf("Chiller %d power", n), objects.UnitKilowatt, wave(212, 30, int(i)+5))
		d.analog(typeAI, i*10+3, fmt.Sprintf("CH%d.FLOW", n), fmt.Sprintf("Chiller %d flow", n), objects.UnitUSGallonPerMinute, wave(880, 40, int(i)+8))
		d.analog(typeAV, i*10, fmt.Sprintf("CH%d.CHWS.SP", n), fmt.Sprintf("Chiller %d supply setpoint", n), objects.UnitDegreeFahrenheit, steady(44))
		d.binary(typeBI, i, fmt.Sprintf("CH%d.STS", n), fmt.Sprintf("Chiller %d status", n), i != 3, "Off", "Running", false)
		d.binary(typeBO, i, fmt.Sprintf("CH%d.ENA", n), fmt.Sprintf("Chiller %d enable", n), i != 3, "Disabled", "Enabled", false)
		d.binary(typeBI, 10+i, fmt.Sprintf("CH%d.ALM", n), fmt.Sprintf("Chiller %d general alarm", n), i == 3, "Normal", "Alarm", i == 3)
	}
	for i := uint32(0); i < 90; i++ {
		d.analog(typeAV, 100+i, fmt.Sprintf("PLANT.CALC.%02d", i), "Plant calculation", objects.UnitNoUnits, wave(float64(i), 0.5, int(i)))
	}
	d.multiState(0, "PLANT.STAGE", "Plant stage", 3, "Off", "Stage 1", "Stage 2", "Stage 3", "Stage 4")
	return d.finish()
}

// terminalUnit is a small device behind the plant router.
func terminalUnit(instance uint32, mac byte, name string) *Device {
	d := &Device{
		Instance: instance, Name: name, Model: "SIM-TEC", Description: "Terminal unit on MS/TP",
		MaxAPDU: 480, Segmentation: SegNone, Net: 2001, MAC: []byte{mac},
	}
	seed := int(mac)
	d.analog(typeAI, 0, name+".ZN.T", "Zone temperature", objects.UnitDegreeFahrenheit, wave(72, 1.2, seed))
	d.analog(typeAI, 1, name+".DA.T", "Discharge air temperature", objects.UnitDegreeFahrenheit, wave(58, 2.5, seed+1))
	d.analog(typeAI, 2, name+".FLOW", "Airflow", objects.UnitCubicFootPerMinute, wave(410, 60, seed+2))
	d.analog(typeAO, 0, name+".DMP", "Damper position", objects.UnitPercent, wave(48, 12, seed+3))
	d.analog(typeAO, 1, name+".RH.VLV", "Reheat valve", objects.UnitPercent, steady(0))
	d.analog(typeAV, 0, name+".CLG.SP", "Cooling setpoint", objects.UnitDegreeFahrenheit, steady(74))
	d.analog(typeAV, 1, name+".HTG.SP", "Heating setpoint", objects.UnitDegreeFahrenheit, steady(70))
	d.binary(typeBV, 0, name+".OCC", "Occupied", true, "Unoccupied", "Occupied", false)
	return d.finish()
}

// meterGateway has a long object list and cannot segment, so its object list
// has to be read one element at a time.
func meterGateway() *Device {
	d := &Device{
		Instance: 3000, Name: "METER-GW", Model: "SIM-GATEWAY", Description: "Electric meter gateway",
		MaxAPDU: 480, Segmentation: SegNone,
	}
	for m := uint32(0); m < 60; m++ {
		n := m + 1
		d.analog(typeAI, m*4, fmt.Sprintf("MTR%02d.KW", n), fmt.Sprintf("Meter %d demand", n), objects.UnitKilowatt, wave(18+float64(m), 4, int(m)))
		d.analog(typeAI, m*4+1, fmt.Sprintf("MTR%02d.KWH", n), fmt.Sprintf("Meter %d energy", n), objects.UnitKilowattHour, steady(120000+float64(m)*7919))
		d.analog(typeAI, m*4+2, fmt.Sprintf("MTR%02d.V", n), fmt.Sprintf("Meter %d voltage", n), objects.UnitVolt, wave(480, 3, int(m)+1))
		d.analog(typeAI, m*4+3, fmt.Sprintf("MTR%02d.A", n), fmt.Sprintf("Meter %d current", n), objects.UnitAmpere, wave(31+float64(m), 5, int(m)+2))
	}
	return d.finish()
}

// legacyPanel has no ReadPropertyMultiple.
func legacyPanel() *Device {
	d := &Device{
		Instance: 4000, Name: "BOILER-1", Model: "SIM-LEGACY", Description: "Boiler panel without ReadPropertyMultiple",
		MaxAPDU: 206, Segmentation: SegNone, NoRPM: true,
	}
	d.analog(typeAI, 0, "B1.HWS.T", "Hot water supply temperature", objects.UnitDegreeFahrenheit, wave(168, 3, 1))
	d.analog(typeAI, 1, "B1.HWR.T", "Hot water return temperature", objects.UnitDegreeFahrenheit, wave(148, 3, 2))
	d.analog(typeAO, 0, "B1.FIRE", "Firing rate", objects.UnitPercent, wave(55, 15, 3))
	d.analog(typeAV, 0, "B1.HWS.SP", "Hot water supply setpoint", objects.UnitDegreeFahrenheit, steady(170))
	d.binary(typeBI, 0, "B1.STS", "Boiler status", true, "Off", "Firing", false)
	d.binary(typeBO, 0, "B1.ENA", "Boiler enable", true, "Disabled", "Enabled", false)
	return d.finish()
}
