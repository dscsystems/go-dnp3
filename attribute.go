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

	// AttrAttributeList is a list of (variation, properties) pairs: the
	// answer to [AttrList]. Its length octet counts octets, two per entry.
	AttrAttributeList AttributeType = 254
	// AttrExtAttributeList is the same list when it runs past 255 octets:
	// the length octet then counts octets beyond the first 256.
	AttrExtAttributeList AttributeType = 255
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
	case AttrAttributeList, AttrExtAttributeList:
		return "list"
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
	case AttrAttributeList, AttrExtAttributeList:
		items := a.List()
		parts := make([]string, len(items))
		for i, it := range items {
			parts[i] = strconv.Itoa(int(it.Variation))
			if it.Writable {
				parts[i] += "(w)"
			}
		}
		return strings.Join(parts, " ")
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

// AttributeListItem is one entry of a device's list of attributes: which
// variation it implements, and whether a master may write it.
type AttributeListItem struct {
	Variation uint8
	Writable  bool
}

// attrPropWritable is the property bit that marks an attribute writable.
const attrPropWritable = 0x01

// List decodes an attribute list. It returns nil for any other type.
func (a Attribute) List() []AttributeListItem {
	if a.Type != AttrAttributeList && a.Type != AttrExtAttributeList {
		return nil
	}
	out := make([]AttributeListItem, 0, len(a.Octets)/2)
	for i := 0; i+1 < len(a.Octets); i += 2 {
		out = append(out, AttributeListItem{
			Variation: a.Octets[i],
			Writable:  a.Octets[i+1]&attrPropWritable != 0,
		})
	}
	return out
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
// These names are for display and nothing else. The wire carries numbers, and
// this package never routes on a name. The numbering is IEEE 1815-2012's set 0,
// the same table Wireshark's DNP3 dissector uses.
//
// A device's own attributes, and any set other than 0, come back numbered.
var attributeNames = map[uint8]string{
	196: "configuration ID",
	197: "configuration version",
	198: "configuration build date",
	199: "configuration last change date",
	200: "configuration signature",
	201: "configuration signature algorithm",
	202: "master resource ID (mRID)",
	203: "device location altitude",
	204: "device location longitude",
	205: "device location latitude",
	206: "secondary operator name",
	207: "primary operator name",
	208: "system name",
	209: "secure authentication version",
	210: "number of security statistics per association",
	211: "user-specific attribute sets",
	212: "master-defined data set prototypes",
	213: "outstation-defined data set prototypes",
	214: "master-defined data sets",
	215: "outstation-defined data sets",
	216: "max binary outputs per request",
	217: "local timing accuracy",
	218: "duration of time accuracy",
	219: "analog output events supported",
	220: "max analog output index",
	221: "number of analog outputs",
	222: "binary output events supported",
	223: "max binary output index",
	224: "number of binary outputs",
	225: "frozen counter events supported",
	226: "frozen counters supported",
	227: "counter events supported",
	228: "max counter index",
	229: "number of counters",
	230: "frozen analog inputs supported",
	231: "analog input events supported",
	232: "max analog input index",
	233: "number of analog inputs",
	234: "double-bit binary input events supported",
	235: "max double-bit binary input index",
	236: "number of double-bit binary inputs",
	237: "binary input events supported",
	238: "max binary input index",
	239: "number of binary inputs",
	240: "max transmit fragment size",
	241: "max receive fragment size",
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
