package objects

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/dscsystems/go-dnp3"
)

// ActivationStatus reports one configuration file's activation result.
type ActivationStatus struct {
	Code uint8
	Text string
}

// ActivationResult is the group 91 variation 1 response to ACTIVATE_CONFIG.
type ActivationResult struct {
	Delay    time.Duration
	Statuses []ActivationStatus
}

func AppendActivationResult(dst []byte, v ActivationResult) ([]byte, error) {
	ms := v.Delay / time.Millisecond
	if ms < 0 || ms > 0xffffffff || len(v.Statuses) > 255 {
		return nil, dnp3.ErrBadConfig
	}
	dst = binary.LittleEndian.AppendUint32(dst, uint32(ms))
	dst = append(dst, byte(len(v.Statuses)))
	for _, s := range v.Statuses {
		if len(s.Text) > 254 {
			return nil, dnp3.ErrBadConfig
		}
		dst = append(dst, byte(len(s.Text)+1), s.Code)
		dst = append(dst, s.Text...)
	}
	return dst, nil
}
func ParseActivationResult(data []byte) (ActivationResult, error) {
	if len(data) < 5 {
		return ActivationResult{}, dnp3.ErrMalformed
	}
	out := ActivationResult{Delay: time.Duration(binary.LittleEndian.Uint32(data)) * time.Millisecond}
	n := int(data[4])
	data = data[5:]
	for range n {
		if len(data) < 2 || data[0] == 0 || int(data[0]) >= len(data) {
			return ActivationResult{}, fmt.Errorf("%w: activation status length", dnp3.ErrMalformed)
		}
		length := int(data[0])
		out.Statuses = append(out.Statuses, ActivationStatus{Code: data[1], Text: string(data[2 : 1+length])})
		data = data[1+length:]
	}
	if len(data) != 0 {
		return ActivationResult{}, dnp3.ErrMalformed
	}
	return out, nil
}
