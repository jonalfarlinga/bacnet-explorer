package bacnet

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/jonalfarlinga/bacnet/client"
	"github.com/jonalfarlinga/bacnet/objects"
)

// Property identifiers the objects package has no constant for.
const propPropertyList = 371

var objectTypeNames = map[uint16]string{
	0: "analog-input", 1: "analog-output", 2: "analog-value", 3: "binary-input", 4: "binary-output",
	5: "binary-value", 6: "calendar", 7: "command", 8: "device", 9: "event-enrollment", 10: "file",
	11: "group", 12: "loop", 13: "multi-state-input", 14: "multi-state-output", 15: "notification-class",
	16: "program", 17: "schedule", 18: "averaging", 19: "multi-state-value", 20: "trend-log",
	21: "life-safety-point", 22: "life-safety-zone", 23: "accumulator", 24: "pulse-converter",
	25: "event-log", 26: "global-group", 27: "trend-log-multiple", 28: "load-control", 29: "structured-view",
	30: "access-door", 31: "timer", 32: "access-credential", 33: "access-point", 34: "access-rights",
	35: "access-user", 36: "access-zone", 37: "credential-data-input", 38: "network-security",
	39: "bitstring-value", 40: "characterstring-value", 41: "date-pattern-value", 42: "date-value",
	43: "datetime-pattern-value", 44: "datetime-value", 45: "integer-value", 46: "large-analog-value",
	47: "octetstring-value", 48: "positive-integer-value", 49: "time-pattern-value", 50: "time-value",
	51: "notification-forwarder", 52: "alert-enrollment", 53: "channel", 54: "lighting-output",
	55: "binary-lighting-output", 56: "network-port", 57: "elevator-group", 58: "escalator", 59: "lift",
}

func objectTypeName(t uint16) string {
	if name, ok := objectTypeNames[t]; ok {
		return name
	}
	if t >= 128 {
		return fmt.Sprintf("proprietary-%d", t)
	}
	return fmt.Sprintf("type-%d", t)
}

// objectTypeAbbr gives the short form of an object type: AI, MSV, DEV.
func objectTypeAbbr(t uint16) string {
	switch t {
	case objects.ObjectTypeDevice:
		return "DEV"
	case objects.ObjectTypeFile:
		return "FILE"
	case objects.ObjectTypeLoop:
		return "LOOP"
	}
	name, ok := objectTypeNames[t]
	if !ok {
		return strconv.Itoa(int(t))
	}
	words := strings.Split(name, "-")
	if len(words) == 1 {
		return strings.ToUpper(name[:min(3, len(name))])
	}
	var sb strings.Builder
	for _, w := range words {
		sb.WriteString(strings.ToUpper(w[:1]))
	}
	return sb.String()
}

// hasPresentValue tells the object types whose Present_Value and
// Status_Flags are shown beside the object.
func hasPresentValue(t uint16) bool {
	switch t {
	case objects.ObjectTypeAnalogInput, objects.ObjectTypeAnalogOutput, objects.ObjectTypeAnalogValue,
		objects.ObjectTypeBinaryInput, objects.ObjectTypeBinaryOutput, objects.ObjectTypeBinaryValue,
		objects.ObjectTypeMultiStateInput, objects.ObjectTypeMultiStateOutput, objects.ObjectTypeMultiStateValue,
		objects.ObjectTypeLoop, objects.ObjectTypeAccumulator, objects.ObjectTypePulseConverter,
		39, 40, 45, 46, 48:
		return true
	}
	return false
}

func hasUnits(t uint16) bool {
	switch t {
	case objects.ObjectTypeAnalogInput, objects.ObjectTypeAnalogOutput, objects.ObjectTypeAnalogValue,
		objects.ObjectTypeAccumulator, objects.ObjectTypePulseConverter, 45, 46, 48:
		return true
	}
	return false
}

func isBinary(t uint16) bool {
	return t == objects.ObjectTypeBinaryInput || t == objects.ObjectTypeBinaryOutput || t == objects.ObjectTypeBinaryValue
}

// propertyName turns the objects package's "ObjectName" into "Object name".
func propertyName(id uint32) string {
	if id == propPropertyList {
		return "Property list"
	}
	name, ok := "", false
	if id <= 0xFFFF {
		name, ok = objects.PropertyMap[uint16(id)]
	}
	if !ok {
		if id >= 512 {
			return fmt.Sprintf("Proprietary %d", id)
		}
		return fmt.Sprintf("Property %d", id)
	}
	var sb strings.Builder
	for i, r := range name {
		if i > 0 && unicode.IsUpper(r) {
			sb.WriteByte(' ')
			r = unicode.ToLower(r)
		}
		sb.WriteRune(r)
	}
	return acronyms.Replace(sb.String())
}

var acronyms = strings.NewReplacer(
	"Apdu", "APDU", "apdu", "APDU", "Cov ", "COV ", "cov ", "COV ", "Utc", "UTC", "utc", "UTC",
	"Vt ", "VT ", " vt ", " VT ",
)

var unitSymbols = map[uint16]string{
	objects.UnitNoUnits:                 "",
	objects.UnitDegreeCelsius:           "°C",
	objects.UnitDegreeFahrenheit:        "°F",
	objects.UnitKelvin:                  "K",
	objects.UnitDeltaDegreesFahrenheit:  "Δ°F",
	objects.UnitDeltaDegreesKelvin:      "ΔK",
	objects.UnitPercent:                 "%",
	objects.UnitPercentRelativeHumidity: "%RH",
	objects.UnitPartsPerMillion:         "ppm",
	objects.UnitPartsPerBillion:         "ppb",
	objects.UnitMilliampere:             "mA",
	objects.UnitAmpere:                  "A",
	objects.UnitVolt:                    "V",
	objects.UnitWatt:                    "W",
	objects.UnitKilowatt:                "kW",
	objects.UnitKilowattHour:            "kWh",
	objects.UnitHorsepower:              "hp",
	objects.UnitTonsRefrigeration:       "tons",
	objects.UnitHertz:                   "Hz",
	objects.UnitPascal:                  "Pa",
	objects.UnitKilopascal:              "kPa",
	objects.UnitPoundForcePerSquareInch: "psi",
	objects.UnitInchOfWater:             "inH₂O",
	objects.UnitCubicFootPerMinute:      "cfm",
	objects.UnitLitrePerSecond:          "L/s",
	objects.UnitLitrePerMinute:          "L/min",
	objects.UnitUSGallonPerMinute:       "gpm",
	objects.UnitCubicMetrePerHour:       "m³/h",
	objects.UnitFootPerMinute:           "ft/min",
	objects.UnitMetrePerSecond:          "m/s",
	objects.UnitRevolutionsPerMinute:    "rpm",
	objects.UnitHour:                    "h",
	objects.UnitMinuteUnitOfTime:        "min",
	objects.UnitSecondUnitOfTime:        "s",
	objects.UnitMillisecond:             "ms",
}

// unitName gives the symbol for a unit where there is a usual one, and the
// unit's name otherwise.
func unitName(v uint64, symbol bool) string {
	if v > 0xFFFF {
		return fmt.Sprintf("unit %d", v)
	}
	if s, ok := unitSymbols[uint16(v)]; ok && symbol {
		return s
	}
	if name, ok := objects.UnitMap[uint16(v)]; ok {
		return strings.ToLower(name)
	}
	return fmt.Sprintf("unit %d", v)
}

// Enumerations by the property that holds them.
var enumNames = map[uint32][]string{
	uint32(objects.PropertyIdEventState): {
		"normal", "fault", "offnormal", "high-limit", "low-limit", "life-safety-alarm",
	},
	uint32(objects.PropertyIdReliability): {
		"no fault detected", "no sensor", "over range", "under range", "open loop", "shorted loop",
		"no output", "unreliable other", "process error", "multi-state fault", "configuration error",
		"", "communication failure", "member fault", "monitored object fault", "tripped",
	},
	uint32(objects.PropertyIdSegmentationSupported): {"both", "transmit only", "receive only", "none"},
	uint32(objects.PropertyIdSystemStatus): {
		"operational", "operational read-only", "download required", "download in progress",
		"non-operational", "backup in progress",
	},
	uint32(objects.PropertyIdPolarity):   {"normal", "reverse"},
	uint32(objects.PropertyIdNotifyType): {"alarm", "event", "ack-notification"},
}

// Bit strings by the property that holds them.
var bitNames = map[uint32][]string{
	uint32(objects.PropertyIdStatusFlags):      {"in alarm", "fault", "overridden", "out of service"},
	uint32(objects.PropertyIdEventEnable):      {"to-offnormal", "to-fault", "to-normal"},
	uint32(objects.PropertyIdAckedTransitions): {"to-offnormal", "to-fault", "to-normal"},
	uint32(objects.PropertyIdLimitEnable):      {"low limit", "high limit"},
	uint32(objects.PropertyIdProtocolServicesSupported): {
		"acknowledgeAlarm", "confirmedCOVNotification", "confirmedEventNotification", "getAlarmSummary",
		"getEnrollmentSummary", "subscribeCOV", "atomicReadFile", "atomicWriteFile", "addListElement",
		"removeListElement", "createObject", "deleteObject", "readProperty", "readPropertyConditional",
		"readPropertyMultiple", "writeProperty", "writePropertyMultiple", "deviceCommunicationControl",
		"confirmedPrivateTransfer", "confirmedTextMessage", "reinitializeDevice", "vtOpen", "vtClose",
		"vtData", "authenticate", "requestKey", "i-Am", "i-Have", "unconfirmedCOVNotification",
		"unconfirmedEventNotification", "unconfirmedPrivateTransfer", "unconfirmedTextMessage",
		"timeSynchronization", "who-Has", "who-Is", "readRange", "utcTimeSynchronization",
		"lifeSafetyOperation", "subscribeCOVProperty", "getEventInformation", "writeGroup",
	},
}

// setBits names the bits that are set in a bit string.
func setBits(prop uint32, bits []bool) ([]string, bool) {
	var set []string
	switch names, known := bitNames[prop]; {
	case known:
		for i, on := range bits {
			if on && i < len(names) {
				set = append(set, names[i])
			} else if on {
				set = append(set, fmt.Sprintf("bit %d", i))
			}
		}
	case prop == uint32(objects.PropertyIdProtocolObjectTypesSupported):
		for i, on := range bits {
			if on {
				set = append(set, objectTypeName(uint16(i)))
			}
		}
	default:
		return nil, false
	}
	return set, true
}

// formatProperty renders a property value for display. A value of several
// elements, as an array or a list has, is returned as items; text is then
// the items joined.
func formatProperty(objType uint16, prop uint32, values []client.Value) (text string, items []string) {
	// A bit string with named bits reads better as a list of those set.
	if len(values) == 1 && values[0].Kind == client.KindBits {
		if set, ok := setBits(prop, values[0].Bits); ok {
			if len(set) == 0 {
				return "none", nil
			}
			if len(set) <= 4 {
				return strings.Join(set, ", "), nil
			}
			return strings.Join(set, ", "), set
		}
	}

	parts := formatValues(objType, prop, values)
	switch len(parts) {
	case 0:
		return "empty", nil
	case 1:
		return parts[0], nil
	}
	return strings.Join(parts, ", "), parts
}

func formatValues(objType uint16, prop uint32, values []client.Value) []string {
	parts := make([]string, 0, len(values))
	for i := 0; i < len(values); i++ {
		v := values[i]
		// A date followed by a time is one date and time.
		if v.Kind == client.KindDate && i+1 < len(values) && values[i+1].Kind == client.KindTime {
			parts = append(parts, formatDate(v.Bytes)+" "+formatTime(values[i+1].Bytes))
			i++
			continue
		}
		parts = append(parts, formatValue(objType, prop, v))
	}
	return parts
}

func formatValue(objType uint16, prop uint32, v client.Value) string {
	switch v.Kind {
	case client.KindNull:
		return "null"
	case client.KindBoolean:
		return strconv.FormatBool(v.Bool)
	case client.KindUnsigned:
		return strconv.FormatUint(v.Uint, 10)
	case client.KindSigned:
		return strconv.FormatInt(v.Int, 10)
	case client.KindReal:
		return strconv.FormatFloat(v.Float, 'f', -1, 32)
	case client.KindDouble:
		return strconv.FormatFloat(v.Float, 'f', -1, 64)
	case client.KindOctets:
		if len(v.Bytes) == 0 {
			return "empty"
		}
		return hex.EncodeToString(v.Bytes)
	case client.KindString:
		return v.Str
	case client.KindBits:
		if set, ok := setBits(prop, v.Bits); ok {
			if len(set) == 0 {
				return "none"
			}
			return strings.Join(set, ", ")
		}
		var sb strings.Builder
		for _, bit := range v.Bits {
			if bit {
				sb.WriteByte('1')
			} else {
				sb.WriteByte('0')
			}
		}
		return sb.String()
	case client.KindEnumerated:
		return formatEnum(objType, prop, v.Uint)
	case client.KindDate:
		return formatDate(v.Bytes)
	case client.KindTime:
		return formatTime(v.Bytes)
	case client.KindObjectID:
		return fmt.Sprintf("%s:%d", objectTypeName(v.Object.Type), v.Object.Instance)
	case client.KindContext:
		return fmt.Sprintf("[%d] %s", v.Tag, hex.EncodeToString(v.Bytes))
	case client.KindConstructed:
		return fmt.Sprintf("[%d] { %s }", v.Tag, strings.Join(formatValues(objType, prop, v.Items), ", "))
	}
	return "?"
}

func formatEnum(objType uint16, prop uint32, v uint64) string {
	switch prop {
	case uint32(objects.PropertyIdUnits):
		return unitName(v, false)
	case uint32(objects.PropertyIdObjectType):
		return objectTypeName(uint16(v))
	case uint32(objects.PropertyIdPresentValue), uint32(objects.PropertyIdRelinquishDefault),
		uint32(objects.PropertyIdPriorityArray), uint32(objects.PropertyIdAlarmValue):
		if isBinary(objType) && v <= 1 {
			return []string{"inactive", "active"}[v]
		}
	}
	if names, ok := enumNames[prop]; ok && v < uint64(len(names)) && names[v] != "" {
		return names[v]
	}
	return strconv.FormatUint(v, 10)
}

// shortNumber trims a real number to at most 3 decimals, for the tree.
func shortNumber(f float64) string {
	s := strconv.FormatFloat(f, 'f', 3, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "-0" {
		s = "0"
	}
	return s
}

func formatDate(d []byte) string {
	if len(d) != 4 {
		return hex.EncodeToString(d)
	}
	year, month, day := "*", "*", "*"
	if d[0] != 0xFF {
		year = strconv.Itoa(1900 + int(d[0]))
	}
	switch {
	case d[1] == 13:
		month = "odd"
	case d[1] == 14:
		month = "even"
	case d[1] != 0xFF:
		month = fmt.Sprintf("%02d", d[1])
	}
	switch {
	case d[2] == 32:
		day = "last"
	case d[2] != 0xFF:
		day = fmt.Sprintf("%02d", d[2])
	}
	return year + "-" + month + "-" + day
}

func formatTime(d []byte) string {
	if len(d) != 4 {
		return hex.EncodeToString(d)
	}
	field := func(b byte) string {
		if b == 0xFF {
			return "*"
		}
		return fmt.Sprintf("%02d", b)
	}
	s := field(d[0]) + ":" + field(d[1]) + ":" + field(d[2])
	if d[3] != 0xFF && d[3] != 0 {
		s += "." + fmt.Sprintf("%02d", d[3])
	}
	return s
}
