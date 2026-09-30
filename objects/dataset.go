package objects

import (
	"fmt"

	"github.com/dscsystems/go-dnp3"
)

// DatasetElement is one g85v1/g86v1 descriptor element. Ancillary contains
// its identifier, UUID, name, prototype reference, or other code-specific data.
type DatasetElement struct {
	Code, DataType, MaxLength uint8
	Ancillary                 []byte
}

// ParseDatasetDescriptor decodes the self-delimiting descriptor elements.
func ParseDatasetDescriptor(data []byte) ([]DatasetElement, error) {
	var out []DatasetElement
	for len(data) > 0 {
		n := int(data[0])
		if n < 3 || n >= len(data) {
			return nil, fmt.Errorf("%w: dataset descriptor element length", dnp3.ErrMalformed)
		}
		out = append(out, DatasetElement{Code: data[1], DataType: data[2], MaxLength: data[3], Ancillary: append([]byte(nil), data[4:n+1]...)})
		data = data[n+1:]
	}
	return out, nil
}

// AppendDatasetDescriptor encodes elements, refusing an ancillary value that
// cannot fit the one-octet element length.
func AppendDatasetDescriptor(dst []byte, elements []DatasetElement) ([]byte, error) {
	for _, e := range elements {
		if len(e.Ancillary) > 252 {
			return nil, fmt.Errorf("%w: dataset ancillary value too long", dnp3.ErrBadConfig)
		}
		dst = append(dst, byte(3+len(e.Ancillary)), e.Code, e.DataType, e.MaxLength)
		dst = append(dst, e.Ancillary...)
	}
	return dst, nil
}
