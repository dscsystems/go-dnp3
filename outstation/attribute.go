package outstation

import (
	"cmp"
	"io"
	"slices"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/stack"
	"github.com/dscsystems/go-dnp3/objects"
)

// Device attributes are the outstation's answer to "what are you?". A master
// commissioning an unfamiliar panel reads them instead of trusting a drawing,
// so an outstation that reports none makes itself harder to install than it
// needs to be.
//
// Two kinds are served. What the application configures — vendor, model,
// serial number, the things only it can know — and what the session can work
// out for itself, which is the point counts and the fragment sizes it was
// built with. Deriving the second kind means they cannot drift from the
// database they describe.

// attributeKey identifies an attribute on the wire.
type attributeKey struct {
	set       uint8
	variation uint8
}

// attributeStore is what the outstation will answer with.
type attributeStore map[attributeKey]dnp3.Attribute

// buildAttributes assembles the store from the configuration and from what the
// session knows about itself.
//
// Configured attributes win. A device that has been told its own serial number
// should report that rather than anything this package inferred.
func buildAttributes(cfg Config) attributeStore {
	store := attributeStore{}

	for _, a := range derivedAttributes(cfg) {
		store[attributeKey{a.Set, a.Variation}] = a
	}
	for _, a := range cfg.Attributes {
		store[attributeKey{a.Set, a.Variation}] = a
	}
	return store
}

// derivedAttributes are the ones the session can answer from its own
// configuration.
//
// Only the counts and the fragment sizes: those are facts about this session,
// and a master reading them gets numbers that match the database it is about
// to poll. Everything else about a device — who made it, what it is called —
// is the application's to say.
func derivedAttributes(cfg Config) []dnp3.Attribute {
	db := cfg.Database

	counts := []struct {
		variation uint8
		n         int
	}{
		{AttrBinaryInputCount, db.Binary},
		{AttrDoubleBitInputCount, db.DoubleBitBinary},
		{AttrCounterCount, db.Counter},
		{AttrAnalogInputCount, db.Analog},
		{AttrBinaryOutputCount, db.BinaryOutputStatus},
		{AttrAnalogOutputCount, db.AnalogOutputStatus},
	}

	out := make([]dnp3.Attribute, 0, len(counts)+2)
	for _, c := range counts {
		if c.n <= 0 {
			// A point type the device does not have is left unreported rather
			// than reported as zero: "none" and "I did not say" are different
			// answers, and only one of them is this session's to give.
			continue
		}
		out = append(out, objects.UintAttribute(c.variation, uint64(c.n)))
	}

	out = append(out,
		objects.UintAttribute(AttrMaxTxFragment, uint64(cfg.MaxTxFragment)),
		objects.UintAttribute(AttrMaxRxFragment, uint64(cfg.MaxRxFragment)),
	)
	return out
}

// Variations this package reports on its own behalf.
//
// They are named here rather than in the dnp3 package's display table because
// this is code that has to be right: a number used to answer a request is not
// a label. See the note on [dnp3.AttributeName].
const (
	AttrBinaryOutputCount   uint8 = 211
	AttrCounterCount        uint8 = 216
	AttrAnalogInputCount    uint8 = 220
	AttrDoubleBitInputCount uint8 = 223
	AttrBinaryInputCount    uint8 = 226
	AttrMaxTxFragment       uint8 = 227
	AttrMaxRxFragment       uint8 = 228
	AttrAnalogOutputCount   uint8 = 208
)

// attributesFor returns what answers one request, sorted so two reads of the
// same device produce the same fragment.
func (s *Session) attributesFor(set, variation uint8) []dnp3.Attribute {
	if variation == dnp3.AttrAll {
		var out []dnp3.Attribute
		for key, a := range s.attributes {
			if key.set == set {
				out = append(out, a)
			}
		}
		slices.SortFunc(out, func(a, b dnp3.Attribute) int {
			return cmp.Compare(a.Variation, b.Variation)
		})
		return out
	}

	if a, ok := s.attributes[attributeKey{set, variation}]; ok {
		return []dnp3.Attribute{a}
	}
	return nil
}

// onAttributeRead answers a read of group 0.
//
// Each attribute goes in its own object header, because the variation is what
// names it: a response carrying six attributes carries six headers.
func (s *Session) onAttributeRead(w io.Writer, r stack.Received, frag app.Fragment, h app.ObjectHeader) error {
	// The range is the attribute set rather than a point index.
	set := uint8(0)
	if h.Range.Spec.IsStartStop() {
		set = uint8(h.Range.Start)
	}

	if h.Variation == dnp3.AttrList {
		// Reporting which attributes exist is a distinct encoding this
		// implementation does not have, and answering it with the attributes
		// themselves would be a different answer to the question asked.
		s.iin = s.iin.Set(app.IINObjectUnknown)
		s.log.Debug("attribute list request refused; not implemented")
		return s.respond(w, r, frag.Header, nil)
	}

	attrs := s.attributesFor(set, h.Variation)
	if len(attrs) == 0 {
		s.iin = s.iin.Set(app.IINObjectUnknown)
		s.log.Debug("no such device attribute", "set", set, "variation", h.Variation)
		return s.respond(w, r, frag.Header, nil)
	}

	var body []byte
	for _, a := range attrs {
		value, err := objects.AppendAttribute(nil, a)
		if err != nil {
			// A value this device cannot encode is a configuration mistake,
			// and reporting the rest beats failing the whole read.
			s.log.Warn("device attribute could not be encoded",
				"variation", a.Variation, "err", err)
			s.iin = s.iin.Set(app.IINParameterError)
			continue
		}

		body = app.AppendObjectHeader(body, app.ObjectHeader{
			Group:     0,
			Variation: a.Variation,
			Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeStartStop8),
			Range: app.Range{
				Spec:  app.RangeStartStop8,
				Start: uint32(a.Set), Stop: uint32(a.Set), Count: 1,
			},
			Data: value,
		})
	}

	s.bump(func(st *Stats) { st.AttributesRead++ })
	return s.respond(w, r, frag.Header, body)
}

// attributeHeader returns the group 0 header a request carries, if any.
func attributeHeader(frag app.Fragment) (app.ObjectHeader, bool) {
	for _, h := range frag.Objects {
		if h.Group == 0 {
			return h, true
		}
	}
	return app.ObjectHeader{}, false
}
