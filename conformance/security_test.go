package conformance

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/security"
	"github.com/dscsystems/go-dnp3/objects"
	"github.com/dscsystems/go-dnp3/outstation"
)

func establishKeys(t *testing.T, h *harness, key []byte) (control, monitor []byte) {
	t.Helper()
	r := h.request(app.FuncAuthRequest, app.ObjectHeader{Group: 120, Variation: 4, Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8), Range: app.Range{Spec: app.RangeCount8, Count: 1}, Data: []byte{1, 0}})
	if r.Header.Func != app.FuncAuthResponse || len(r.Objects) != 1 {
		t.Fatal(r)
	}
	data, err := app.FirstFreeFormatObject(r.Objects[0])
	if err != nil {
		t.Fatal(err)
	}
	status, err := objects.ParseAuthKeyStatus(data)
	if err != nil {
		t.Fatal(err)
	}
	control = bytes.Repeat([]byte{3}, 32)
	monitor = bytes.Repeat([]byte{4}, 32)
	plain := binary.LittleEndian.AppendUint16(nil, 32)
	plain = append(plain, control...)
	plain = append(plain, monitor...)
	status.MAC = nil
	plain = objects.AppendAuthKeyStatus(plain, status)
	for len(plain)%8 != 0 {
		plain = append(plain, 0)
	}
	wrapped, err := security.Wrap(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	hdr, _ := app.FreeFormat(120, 6, objects.AppendAuthKeyChange(nil, objects.AuthKeyChange{Sequence: status.Sequence, User: 1, Wrapped: wrapped}))
	r = h.request(app.FuncAuthRequest, hdr)
	data, err = app.FirstFreeFormatObject(r.Objects[0])
	if err != nil {
		t.Fatal(err)
	}
	status, err = objects.ParseAuthKeyStatus(data)
	if err != nil || status.Status != 1 {
		t.Fatalf("%+v: %v", status, err)
	}
	return
}

func TestAuthenticationRejectsBadMACReplayAndExpiredChallenge(t *testing.T) {
	for _, mode := range []string{"bad MAC", "expired", "replay"} {
		t.Run(mode, func(t *testing.T) {
			key := make([]byte, 16)
			_, _ = rand.Read(key)
			var calls atomic.Int32
			timeout := time.Second
			if mode == "expired" {
				timeout = 20 * time.Millisecond
			}
			h := newHarness(t, outstation.Config{
				Attributes:           []dnp3.Attribute{objects.StringAttribute(247, "before")},
				WritableAttributes:   []outstation.AttributeID{{Set: 0, Variation: 247}},
				AttributeWrite:       func(dnp3.Attribute) bool { calls.Add(1); return true },
				SecureAuthentication: &outstation.SecureAuthenticationConfig{Users: map[uint16][]byte{1: key}, ReplyTimeout: timeout},
			}, nil)
			control, _ := establishKeys(t, h, key)
			hdr := app.ReadRange(0, 247, 0, 0)
			hdr.Data, _ = objects.AppendAttribute(nil, objects.StringAttribute(247, "after"))
			r := h.request(app.FuncWrite, hdr)
			if r.Header.Func != app.FuncAuthResponse || len(r.Objects) != 1 || r.Objects[0].Variation != 1 {
				t.Fatal(r)
			}
			challengeRaw := app.BuildResponse(nil, r.Header.Control, r.Header.Func, r.Header.IIN, r.Objects...)
			original := app.BuildRequest(nil, app.Control{Fir: true, Fin: true, Seq: r.Header.Control.Seq}, app.FuncWrite, hdr)
			data, err := app.FirstFreeFormatObject(r.Objects[0])
			if err != nil {
				t.Fatal(err)
			}
			challenge, err := objects.ParseAuthChallenge(data)
			if err != nil {
				t.Fatal(err)
			}
			mac := security.MAC(control, challengeRaw, original)
			if mode == "bad MAC" {
				mac[0] ^= 1
			}
			if mode == "expired" {
				time.Sleep(40 * time.Millisecond)
			}
			reply, _ := app.FreeFormat(120, 2, objects.AppendAuthReply(nil, objects.AuthReply{Sequence: challenge.Sequence, User: 1, MAC: mac}))
			r = h.request(app.FuncAuthRequest, reply)
			if mode == "replay" {
				if r.Header.Func != app.FuncResponse || calls.Load() != 1 {
					t.Fatal(r, calls.Load())
				}
				r = h.request(app.FuncAuthRequest, reply)
				if calls.Load() != 1 {
					t.Fatal("replayed write", calls.Load())
				}
			} else if calls.Load() != 0 {
				t.Fatal("unauthenticated write", calls.Load())
			}
			if r.Header.Func != app.FuncAuthResponse || len(r.Objects) != 1 || r.Objects[0].Variation != 7 {
				t.Fatal(r)
			}
		})
	}
}
