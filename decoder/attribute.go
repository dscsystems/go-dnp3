package decoder

import (
	"fmt"

	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
)

// A device attribute renders as what it says rather than as a value at an
// index, because that is what it is: the variation names the attribute and the
// range names the set, so a capture reads "product name and model: RTU-9000"
// instead of a number nobody can look up mid-investigation.

// decodeAttributes renders the group 0 objects a header carries.
func decodeAttributes(h app.ObjectHeader) ([]Value, bool) {
	count := int(h.Count())
	if count == 0 {
		count = 1
	}

	out := make([]Value, 0, count)
	off := 0
	for i := range count {
		if off >= len(h.Data) {
			break
		}
		set := uint8(h.Range.IndexOf(uint32(i)))

		a, n, err := objects.ParseAttribute(set, h.Variation, h.Data[off:])
		if err != nil {
			// A capture exists to show what arrived. An attribute that will
			// not decode is the most interesting thing on the line when it
			// happens, so it is reported rather than dropped.
			out = append(out, Value{
				Index: uint16(h.Variation),
				Value: "malformed: " + err.Error(),
			})
			break
		}
		off += n

		// The index column carries the variation, which for group 0 is the
		// attribute's identity — the nearest thing it has to a point index.
		out = append(out, Value{
			Index: uint16(a.Variation),
			Value: fmt.Sprintf("%s = %s [%s]", a.Name(), a.Value(), a.Type),
		})
	}
	return out, len(out) > 0
}
