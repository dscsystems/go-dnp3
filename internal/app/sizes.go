package app

// The size table itself is generated from objects/spec/dnp3_objects.yaml into
// zz_generated_sizes.go. This file holds only the lookup logic around it.
//
// The table lives in this package rather than being reached through the
// objects package because the framing layer must not depend on the codecs:
// app defines the ObjectSizer interface, objects implements the codecs, and
// both are generated from the same spec so there is still exactly one source
// of truth.

// The go:generate directive lives in the objects package, which emits this
// file too. Declaring it in both places would run the generator twice.

// gv packs a group and variation into the generated table's key.
func gv(group, variation uint8) uint16 {
	return uint16(group)<<8 | uint16(variation)
}

// SpecSizer resolves object sizes from the generated spec table.
type SpecSizer struct{}

// SizeBits implements [ObjectSizer].
func (SpecSizer) SizeBits(group, variation uint8) (int, bool) {
	if lengthIsVariationGroups[group] {
		// For these groups the variation number *is* the octet length, which
		// makes them self-describing without a size prefix. Variation zero
		// means "any length" and appears only in requests.
		return int(variation) * 8, true
	}
	if variableGroups[group] {
		// Genuinely variable-length. Reporting unknown makes the parser say so
		// rather than guess, and pushes it onto the size-prefix path.
		return 0, false
	}

	bits, ok := generatedSizes[gv(group, variation)]
	return bits, ok
}

// Known reports whether the sizer recognises a group and variation.
func (s SpecSizer) Known(group, variation uint8) bool {
	_, ok := s.SizeBits(group, variation)
	return ok
}

// DefaultSizer is the sizer used when a caller supplies none.
var DefaultSizer ObjectSizer = SpecSizer{}
