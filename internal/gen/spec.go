// Command gen reads objects/spec/dnp3_objects.yaml and emits the object
// codecs, the descriptor registry, and the size table the framing layer needs.
//
// It exists because the group/variation matrix is a few hundred near-identical
// encodings. Hand-writing them produces hundreds of files that differ by a
// field width, and every Level 4 addition means touching all of them. One
// declarative table and a generator means adding a variation is one line.
//
// Run it with `make generate`. The output is committed, so consumers never run
// the generator, and CI fails if the committed files drift from the spec.
package main

import (
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

// Field is one wire field of an object.
type Field struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
}

// Object is one group/variation entry from the spec.
type Object struct {
	Group     uint8   `yaml:"group"`
	Variation uint8   `yaml:"variation"`
	Name      string  `yaml:"name"`
	Level     int     `yaml:"level"`
	Kind      string  `yaml:"kind"`
	Measure   string  `yaml:"measurement"`
	Packed    int     `yaml:"packed"`
	Variable  bool    `yaml:"variable"`
	LengthIsV bool    `yaml:"length_is_variation"`
	Fields    []Field `yaml:"fields"`
}

// Spec is the whole file.
type Spec struct {
	Objects []Object `yaml:"objects"`
}

// fieldBits maps a field type to its width on the wire.
var fieldBits = map[string]int{
	"flags":  8,
	"u8":     8,
	"status": 8,
	"i16":    16,
	"u16":    16,
	"i32":    32,
	"u32":    32,
	"f32":    32,
	"f64":    64,
	"time48": 48,
	"time16": 16,
}

// validMeasurements are the measurement types a variation may decode into.
// "none" means the object is not a measurement — a command, a time, a class.
var validMeasurements = map[string]string{
	"binary":        "Binary",
	"doublebit":     "DoubleBitBinary",
	"counter":       "Counter",
	"frozencounter": "FrozenCounter",
	"analog":        "Analog",
	"binaryoutput":  "BinaryOutputStatus",
	"analogoutput":  "AnalogOutputStatus",
	"none":          "",
}

var validKinds = map[string]string{
	"static":        "KindStatic",
	"event":         "KindEvent",
	"command":       "KindCommand",
	"command_event": "KindCommandEvent",
	"time":          "KindTime",
	"class":         "KindClass",
	"indication":    "KindIndication",
	"deadband":      "KindDeadband",
	"string":        "KindString",
	"file":          "KindFile",
	"attribute":     "KindAttribute",
}

// SizeBits returns the object's encoded width, and whether it is fixed.
func (o Object) SizeBits() (int, bool) {
	if o.Variable || o.LengthIsV {
		return 0, false
	}
	if o.Packed > 0 {
		return o.Packed, true
	}
	total := 0
	for _, f := range o.Fields {
		total += fieldBits[f.Type]
	}
	return total, true
}

// GoName is the identifier stem used for generated functions.
func (o Object) GoName() string { return fmt.Sprintf("G%dV%d", o.Group, o.Variation) }

// field roles, derived from the field types rather than declared separately.

func (o Object) flagsField() (Field, bool)   { return o.findType("flags") }
func (o Object) absTimeField() (Field, bool) { return o.findType("time48") }
func (o Object) relTimeField() (Field, bool) { return o.findType("time16") }

func (o Object) findType(t string) (Field, bool) {
	for _, f := range o.Fields {
		if f.Type == t {
			return f, true
		}
	}
	return Field{}, false
}

// valueField returns the numeric field carrying the measurement's value.
func (o Object) valueField() (Field, int, bool) {
	off := 0
	for _, f := range o.Fields {
		switch f.Type {
		case "i16", "u16", "i32", "u32", "f32", "f64":
			if f.Name == "Value" {
				return f, off / 8, true
			}
		}
		off += fieldBits[f.Type]
	}
	return Field{}, 0, false
}

// offsetOf returns the octet offset of a named field.
func (o Object) offsetOf(name string) int {
	off := 0
	for _, f := range o.Fields {
		if f.Name == name {
			return off / 8
		}
		off += fieldBits[f.Type]
	}
	return -1
}

// IsMeasurement reports whether the object decodes into one of the measurement
// types, which is the set the generator emits codecs for.
func (o Object) IsMeasurement() bool {
	return o.Measure != "" && o.Measure != "none" && o.Packed == 0 && !o.Variable && !o.LengthIsV
}

// loadSpec reads and validates the spec.
func loadSpec(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var s Spec
	dec := yaml.NewDecoder(bytesReader(data))
	dec.KnownFields(true) // a typo in a field name must fail, not be ignored
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	seen := map[uint16]string{}
	for i, o := range s.Objects {
		where := fmt.Sprintf("%s: entry %d (g%dv%d)", path, i, o.Group, o.Variation)

		if o.Name == "" {
			return nil, fmt.Errorf("%s: missing name", where)
		}
		if _, ok := validKinds[o.Kind]; !ok {
			return nil, fmt.Errorf("%s: unknown kind %q", where, o.Kind)
		}
		if _, ok := validMeasurements[o.Measure]; !ok {
			return nil, fmt.Errorf("%s: unknown measurement %q", where, o.Measure)
		}
		for _, f := range o.Fields {
			if _, ok := fieldBits[f.Type]; !ok {
				return nil, fmt.Errorf("%s: field %s has unknown type %q", where, f.Name, f.Type)
			}
		}
		if o.Packed > 0 && len(o.Fields) > 0 {
			return nil, fmt.Errorf("%s: a packed object cannot also declare fields", where)
		}

		// A measurement that is neither packed nor variable must have a value
		// the decoder can find. Binary types are the exception: their state
		// rides in the flags octet.
		if o.IsMeasurement() {
			_, _, hasValue := o.valueField()
			isBinaryLike := o.Measure == "binary" || o.Measure == "doublebit" || o.Measure == "binaryoutput"
			if !hasValue && !isBinaryLike {
				return nil, fmt.Errorf("%s: measurement %q has no Value field", where, o.Measure)
			}
			if isBinaryLike {
				if _, ok := o.flagsField(); !ok {
					return nil, fmt.Errorf("%s: binary measurement has no Flags field to carry its state", where)
				}
			}
		}

		key := uint16(o.Group)<<8 | uint16(o.Variation)
		if prev, dup := seen[key]; dup {
			return nil, fmt.Errorf("%s: duplicate of %s", where, prev)
		}
		seen[key] = o.Name
	}

	sort.Slice(s.Objects, func(i, j int) bool {
		a, b := s.Objects[i], s.Objects[j]
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		return a.Variation < b.Variation
	})
	return &s, nil
}
