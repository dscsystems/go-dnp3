package dnp3

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Device attributes — group 0 — are what a device says about itself: who made
// it, what it is, what firmware it runs, and how much of DNP3 it implements.
//
// They are unlike every other object in the protocol in one way that shapes
// this whole file: the variation is not an encoding, it is the identity of the
// attribute. Group 0 variation 242 is not "software version encoded one way"
// against some other variation encoding it differently — 242 *is* the software
// version. So there is no codec table to generate, and an attribute a device
// invents for itself parses exactly as well as one the standard named.
//
// The value carries its own type and length, which is what makes that
// possible, and what lets a master display a device's private attributes
// without knowing anything about them.

// AttributeType is the encoding of an attribute's value, from the standard's
// attribute data type codes.
type AttributeType uint8

// Attribute data types.
const (
	AttrVisibleString AttributeType = 1
	AttrUnsignedInt   AttributeType = 2
	AttrSignedInt     AttributeType = 3
	AttrFloat         AttributeType = 4
	AttrOctetString   AttributeType = 5
	AttrBitString     AttributeType = 6
	AttrTime          AttributeType = 7
)

func (t AttributeType) String() string {
	switch t {
	case AttrVisibleString:
		return "string"
	case AttrUnsignedInt:
		return "uint"
	case AttrSignedInt:
		return "int"
	case AttrFloat:
		return "float"
	case AttrOctetString:
		return "octets"
	case AttrBitString:
		return "bits"
	case AttrTime:
		return "time"
	default:
		return fmt.Sprintf("AttributeType(%d)", uint8(t))
	}
}

// Attribute set and variation numbers with a meaning fixed by the standard.
const (
	// AttrSetStandard is attribute set 0, the one the standard defines. A
	// device may keep private attributes in other sets.
	AttrSetStandard uint8 = 0

	// AttrAll is the variation a master reads to ask for every attribute a
	// device has, rather than naming them one at a time. It appears only in
	// requests.
	AttrAll uint8 = 254

	// AttrList asks which attributes the device implements.
	AttrList uint8 = 255
)

// Attribute is one thing a device says about itself.
//
// Exactly one of the value fields carries the value, chosen by Type. They are
// separate fields rather than an interface because the overwhelmingly common
// thing to do with an attribute is print it, and the second most common is to
// read one number out of it.
type Attribute struct {
	// Set is the attribute set, and Variation identifies the attribute within
	// it. Together they are the attribute's name on the wire.
	Set       uint8
	Variation uint8
	Type      AttributeType

	Text   string
	Number int64
	Real   float64
	Octets []byte
	Time   time.Time
}

// Value renders the attribute's value as text, whatever its type.
func (a Attribute) Value() string {
	switch a.Type {
	case AttrVisibleString:
		return a.Text
	case AttrUnsignedInt, AttrSignedInt:
		return strconv.FormatInt(a.Number, 10)
	case AttrFloat:
		return strconv.FormatFloat(a.Real, 'g', -1, 64)
	case AttrTime:
		if a.Time.IsZero() {
			return "—"
		}
		return a.Time.Format(time.RFC3339)
	case AttrOctetString, AttrBitString:
		return octetText(a.Octets)
	default:
		return octetText(a.Octets)
	}
}

// octetText renders octets as text when they are printable and as hex when
// they are not, which is what a device that packs a version number into an
// octet string needs.
func octetText(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	printable := true
	for _, c := range b {
		if c < 0x20 || c > 0x7E {
			printable = false
			break
		}
	}
	if printable {
		return string(b)
	}

	var sb strings.Builder
	for i, c := range b {
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%02X", c)
	}
	return sb.String()
}

// Name returns what the attribute is called, or a placeholder naming its
// number for one this package does not know.
func (a Attribute) Name() string {
	if a.Set == AttrSetStandard {
		if n, ok := attributeNames[a.Variation]; ok {
			return n
		}
	}
	if a.Set != AttrSetStandard {
		return fmt.Sprintf("set %d attribute %d", a.Set, a.Variation)
	}
	return fmt.Sprintf("attribute %d", a.Variation)
}

func (a Attribute) String() string { return a.Name() + ": " + a.Value() }

// attributeNames are the standard set's attributes.
//
// These names are for display and nothing else. The wire carries numbers, this
// package never routes on a name, and an entry that is wrong mislabels a row
// in a listing without affecting a single octet — which is the only reason it
// is safe to ship a table transcribed from the standard's set 0 rather than
// one verified against a device.
//
// A device's own attributes, and any set other than 0, come back numbered.
var attributeNames = map[uint8]string{
	196: "secure authentication statistics per association",
	197: "number of security statistics per association",
	198: "user-specific attributes supported",
	199: "master-defined data set prototypes",
	200: "outstation-defined data set prototypes",
	201: "master-defined data sets",
	202: "outstation-defined data sets",
	203: "max binary outputs per request",
	204: "local timing accuracy",
	205: "duration of time accuracy",
	206: "analog output events supported",
	207: "max analog output index",
	208: "number of analog outputs",
	209: "binary output events supported",
	210: "max binary output index",
	211: "number of binary outputs",
	212: "frozen counter events supported",
	213: "frozen counters supported",
	214: "counter events supported",
	215: "max counter index",
	216: "number of counters",
	217: "frozen analog inputs supported",
	218: "analog input events supported",
	219: "max analog input index",
	220: "number of analog inputs",
	221: "double-bit binary input events supported",
	222: "max double-bit binary input index",
	223: "number of double-bit binary inputs",
	224: "binary input events supported",
	225: "max binary input index",
	226: "number of binary inputs",
	227: "max transmit fragment size",
	228: "max receive fragment size",
	242: "software version",
	243: "hardware version",
	244: "owner name",
	245: "location",
	246: "ID code",
	247: "device name",
	248: "serial number",
	249: "subset level and conformance",
	250: "product name and model",
	252: "manufacturer name",

	AttrAll:  "all attributes",
	AttrList: "list of attributes",
}

// AttributeName returns the standard set's name for a variation, and whether
// there is one. It is exported so a tool can label an attribute it has only
// the number of.
func AttributeName(variation uint8) (string, bool) {
	n, ok := attributeNames[variation]
	return n, ok
}
