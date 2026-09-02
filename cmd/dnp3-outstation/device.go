package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/objects"
)

// What the device says about itself over group 0.
//
// A master pointed at an unfamiliar panel reads these before it reads a single
// point, so a simulator that reports nothing is a simulator that cannot be
// used to develop that part of a master. The defaults describe this program
// honestly: it is a simulator, and a master that reads it should be able to
// tell.
//
// The well-known attributes are named here rather than numbered, because
// nobody configuring a device should have to know that the manufacturer's name
// is variation 252. Anything else goes in the extras list by number.

// Standard set attribute numbers this tool reports by name.
const (
	attrSoftwareVersion uint8 = 242
	attrHardwareVersion uint8 = 243
	attrOwner           uint8 = 244
	attrLocation        uint8 = 245
	attrIDCode          uint8 = 246
	attrDeviceName      uint8 = 247
	attrSerialNumber    uint8 = 248
	attrSubsetLevel     uint8 = 249
	attrProductName     uint8 = 250
	attrVendorName      uint8 = 252
)

// DeviceYAML is the `device:` section.
type DeviceYAML struct {
	Vendor   string `yaml:"vendor"`
	Model    string `yaml:"model"`
	Version  string `yaml:"version"`
	Hardware string `yaml:"hardware"`
	Serial   string `yaml:"serial"`
	Name     string `yaml:"name"`
	Location string `yaml:"location"`
	Owner    string `yaml:"owner"`
	// IDCode is the user-assigned number a utility puts on the asset.
	IDCode int `yaml:"id_code"`
	// Subset is the DNP3 subset level the device claims. Zero omits it rather
	// than claiming level 0, because a conformance claim is not something to
	// report by accident.
	Subset int `yaml:"subset"`

	// Disabled stops the outstation reporting attributes at all, which is how
	// a master's handling of a device without them gets tested.
	Disabled bool `yaml:"disabled"`

	// Extra carries attributes this section does not name, including a
	// device's own in sets other than the standard one.
	Extra []AttributeYAML `yaml:"attributes"`
}

// AttributeYAML is one attribute given by number.
type AttributeYAML struct {
	Set       uint8  `yaml:"set"`
	Variation uint8  `yaml:"variation"`
	Type      string `yaml:"type"` // string, uint, int, float; default string
	Value     string `yaml:"value"`
}

// DefaultDevice describes this simulator.
func DefaultDevice() DeviceYAML {
	return DeviceYAML{
		Vendor:   "DSC Systems",
		Model:    "dnp3-outstation (simulator)",
		Version:  dnp3.Version,
		Hardware: "none — this is software",
		Serial:   "SIM-0000-0001",
		Name:     "SIM-RTU",
		Location: "simulated substation",
		Subset:   2,
	}
}

// attributes turns the section into what the outstation serves.
//
// The point counts and fragment sizes are left out: the outstation derives
// those from its own database, and a number configured here that disagreed
// with the database would be worse than no number at all.
func (d DeviceYAML) attributes() ([]dnp3.Attribute, error) {
	if d.Disabled {
		return nil, nil
	}

	var out []dnp3.Attribute
	add := func(variation uint8, text string) {
		if text != "" {
			out = append(out, objects.StringAttribute(variation, text))
		}
	}

	add(attrVendorName, d.Vendor)
	add(attrProductName, d.Model)
	add(attrSoftwareVersion, d.Version)
	add(attrHardwareVersion, d.Hardware)
	add(attrSerialNumber, d.Serial)
	add(attrDeviceName, d.Name)
	add(attrLocation, d.Location)
	add(attrOwner, d.Owner)

	if d.IDCode != 0 {
		out = append(out, objects.UintAttribute(attrIDCode, uint64(d.IDCode)))
	}
	if d.Subset != 0 {
		out = append(out, objects.UintAttribute(attrSubsetLevel, uint64(d.Subset)))
	}

	for _, e := range d.Extra {
		a, err := e.attribute()
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// attribute converts one numbered entry.
func (e AttributeYAML) attribute() (dnp3.Attribute, error) {
	switch strings.ToLower(strings.TrimSpace(e.Type)) {
	case "", "string":
		a := objects.StringAttribute(e.Variation, e.Value)
		a.Set = e.Set
		return a, nil

	case "uint":
		n, err := strconv.ParseUint(e.Value, 10, 64)
		if err != nil {
			return dnp3.Attribute{}, fmt.Errorf(
				"attribute %d: %q is not an unsigned number", e.Variation, e.Value)
		}
		a := objects.UintAttribute(e.Variation, n)
		a.Set = e.Set
		return a, nil

	case "int":
		n, err := strconv.ParseInt(e.Value, 10, 64)
		if err != nil {
			return dnp3.Attribute{}, fmt.Errorf(
				"attribute %d: %q is not a number", e.Variation, e.Value)
		}
		a := objects.IntAttribute(e.Variation, n)
		a.Set = e.Set
		return a, nil

	case "float":
		f, err := strconv.ParseFloat(e.Value, 64)
		if err != nil {
			return dnp3.Attribute{}, fmt.Errorf(
				"attribute %d: %q is not a number", e.Variation, e.Value)
		}
		return dnp3.Attribute{
			Set: e.Set, Variation: e.Variation, Type: dnp3.AttrFloat, Real: f,
		}, nil

	default:
		return dnp3.Attribute{}, fmt.Errorf(
			"attribute %d: unknown type %q; use string, uint, int or float",
			e.Variation, e.Type)
	}
}

// describe renders the attributes for the startup banner, so whoever points a
// master at this device can see what it will answer with.
func describeAttributes(attrs []dnp3.Attribute) string {
	if len(attrs) == 0 {
		return "  Device attributes: none — the outstation reports only its point counts\n"
	}

	var b strings.Builder
	b.WriteString("\n  Device attributes (group 0)\n")
	for _, a := range attrs {
		b.WriteString(fmt.Sprintf("    %3d  %-28s %s\n", a.Variation, a.Name(), a.Value()))
	}
	return b.String()
}
