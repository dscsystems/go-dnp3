package master

import (
	"context"
	"fmt"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
)

// Device attributes answer the first question anyone has about an unfamiliar
// device: what is it? Vendor, model, firmware, serial number, and how many
// points of each kind it has — read out of the device rather than off a
// drawing that may describe the one it replaced.
//
// The read is one request. A master asks for variation 254, "all attributes",
// and the outstation answers with one object header per attribute it has: the
// variation names the attribute and the range names the set.

// ReadAttributes reads every device attribute in the standard set.
//
// A device that does not implement attributes answers with
// NO_FUNC_CODE_SUPPORT or an object-unknown indication, which comes back as
// [dnp3.ErrNotSupported] — worth distinguishing from a device that has none,
// which answers with an empty list.
func (s *Session) ReadAttributes(ctx context.Context) ([]dnp3.Attribute, error) {
	return s.ReadAttributeSet(ctx, dnp3.AttrSetStandard)
}

// ReadAttributeSet reads every attribute in one set. Set 0 is the standard's;
// a device may keep its own in others.
func (s *Session) ReadAttributeSet(ctx context.Context, set uint8) ([]dnp3.Attribute, error) {
	return s.readAttributes(ctx, set, dnp3.AttrAll)
}

// ReadAttribute reads one named attribute.
//
// Reading them all is usually better: it is the same one request, and a device
// answers it without the master having to know what to ask for.
func (s *Session) ReadAttribute(ctx context.Context, set, variation uint8) (dnp3.Attribute, error) {
	if variation == dnp3.AttrAll {
		return dnp3.Attribute{}, fmt.Errorf(
			"master: %w: variation %d asks for all attributes; use ReadAttributeSet",
			dnp3.ErrBadConfig, variation)
	}

	attrs, err := s.readAttributes(ctx, set, variation)
	if err != nil {
		return dnp3.Attribute{}, err
	}
	if len(attrs) == 0 {
		return dnp3.Attribute{}, fmt.Errorf(
			"master: the outstation reported no attribute %d in set %d", variation, set)
	}
	return attrs[0], nil
}

func (s *Session) readAttributes(ctx context.Context, set, variation uint8) ([]dnp3.Attribute, error) {
	var (
		attrs   []dnp3.Attribute
		failure error
	)

	t := newAttributeTask(set, variation, &attrs, &failure)
	if err := s.run(ctx, t); err != nil {
		return nil, err
	}
	return attrs, failure
}

// newAttributeTask reads attributes into out.
func newAttributeTask(set, variation uint8, out *[]dnp3.Attribute, failure *error) *task {
	return &task{
		name:     "read-attributes",
		funcCode: app.FuncRead,
		priority: priorityCommand,
		build: func(b *app.Builder) error {
			// The range is the attribute set, not a point index: group 0 is the
			// one place where an index means something else entirely.
			return b.AddObject(app.ObjectHeader{
				Group:     0,
				Variation: variation,
				Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeStartStop8),
				Range: app.Range{
					Spec:  app.RangeStartStop8,
					Start: uint32(set), Stop: uint32(set), Count: 1,
				},
			})
		},
		onFragment: func(frag app.Fragment) {
			for _, h := range frag.Objects {
				if h.Group != 0 {
					continue
				}
				got, err := decodeAttributes(h)
				if err != nil {
					*failure = err
					return
				}
				*out = append(*out, got...)
			}
		},
		onDone: func(iin app.IIN) {
			switch {
			case iin.Has(app.IINNoFuncCodeSupport):
				*failure = fmt.Errorf("master: reading device attributes: %w", dnp3.ErrNotSupported)
			case iin.Has(app.IINObjectUnknown) && len(*out) == 0:
				// The device understood the request and has nothing to answer
				// it with, which for an attribute read means it does not keep
				// the one that was asked for.
				*failure = fmt.Errorf(
					"master: the outstation does not have that attribute: %w", dnp3.ErrNotSupported)
			}
		},
	}
}

// decodeAttributes pulls the attributes out of one object header.
//
// The header's range gives the set, and its count says how many sets the one
// variation is being reported for — normally one, since a device usually keeps
// its attributes in set 0 alone.
func decodeAttributes(h app.ObjectHeader) ([]dnp3.Attribute, error) {
	count := int(h.Count())
	if count == 0 {
		count = 1
	}

	out := make([]dnp3.Attribute, 0, count)
	off := 0
	for i := range count {
		if off >= len(h.Data) {
			break
		}
		set := uint8(h.Range.IndexOf(uint32(i)))

		a, n, err := objects.ParseAttribute(set, h.Variation, h.Data[off:])
		if err != nil {
			return nil, fmt.Errorf("master: device attribute: %w", err)
		}
		out = append(out, a)
		off += n
	}
	return out, nil
}
