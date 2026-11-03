package dnp3_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/security"
	"github.com/dscsystems/go-dnp3/internal/stack"
	"github.com/dscsystems/go-dnp3/master"
	"github.com/dscsystems/go-dnp3/objects"
	"github.com/dscsystems/go-dnp3/outstation"
)

func securePair(t *testing.T, masterKey, outKey []byte, authorize func(uint16, uint8) bool) (*master.Session, *outstation.Session) {
	t.Helper()
	mch, och := channel.Pipe()
	out := outstation.New(outstation.Config{LocalAddr: 10, RemoteAddr: 1,
		SecureAuthentication: &outstation.SecureAuthenticationConfig{Users: map[uint16][]byte{1: outKey}, Authorize: authorize},
		Attributes:           []dnp3.Attribute{objects.StringAttribute(247, "before")}, WritableAttributes: []outstation.AttributeID{{Set: 0, Variation: 247}}}, nil, nil)
	m := master.New(master.Config{LocalAddr: 1, RemoteAddr: 10, ResponseTimeout: time.Second, SecureAuthentication: &master.SecureAuthenticationConfig{User: 1, UpdateKey: masterKey}}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = out.Run(ctx, och) }()
	go func() { defer wg.Done(); _ = m.Run(ctx, mch) }()
	t.Cleanup(func() { cancel(); _ = mch.Close(); _ = och.Close(); wg.Wait() })
	waitFor(t, 3*time.Second, func() bool { return m.Connected() })
	return m, out
}

func TestSecureAuthenticationKeyExchangeAndWrite(t *testing.T) {
	key := bytes.Repeat([]byte{0x71}, 16)
	m, _ := securePair(t, key, key, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := m.WriteAttribute(ctx, objects.StringAttribute(247, "after")); err != nil {
		t.Fatal(err)
	}
	v, err := m.ReadAttribute(ctx, 0, 247)
	if err != nil || v.Text != "after" {
		t.Fatalf("attribute %+v: %v", v, err)
	}
}

func TestSecureAuthenticationWrongUpdateKeyCannotWrite(t *testing.T) {
	m, _ := securePair(t, bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{2}, 16), nil)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := m.WriteAttribute(ctx, objects.StringAttribute(247, "after")); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestSecureAuthenticationAuthorizationRefusal(t *testing.T) {
	key := bytes.Repeat([]byte{0x71}, 16)
	m, _ := securePair(t, key, key, func(_ uint16, f uint8) bool { return f != 2 })
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := m.WriteAttribute(ctx, objects.StringAttribute(247, "after")); err == nil || errors.Is(err, dnp3.ErrTimeout) {
		t.Fatalf("authorization error: %v", err)
	}
}

func TestSecureControlsIncludingNoReply(t *testing.T) {
	for _, mode := range []string{"select-operate", "no-reply"} {
		t.Run(mode, func(t *testing.T) {
			key := bytes.Repeat([]byte{0x32}, 32)
			mch, och := channel.Pipe()
			breaker := newBreaker()
			out := outstation.New(outstation.Config{LocalAddr: 10, RemoteAddr: 1, Database: outstation.DatabaseConfig{BinaryOutputStatus: 1},
				SecureAuthentication: &outstation.SecureAuthenticationConfig{Users: map[uint16][]byte{1: key}, MaxMessages: 1}}, nil, breaker)
			m := master.New(master.Config{LocalAddr: 1, RemoteAddr: 10, ResponseTimeout: time.Second,
				SecureAuthentication: &master.SecureAuthenticationConfig{User: 1, UpdateKey: key, MaxMessages: 1}}, nil)
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); _ = out.Run(ctx, och) }()
			go func() { defer wg.Done(); _ = m.Run(ctx, mch) }()
			t.Cleanup(func() { cancel(); _ = mch.Close(); _ = och.Close(); wg.Wait() })
			waitFor(t, 3*time.Second, func() bool { return m.Connected() })
			if mode == "select-operate" {
				result, err := m.SelectAndOperate(ctx, master.LatchOn(0))
				if err != nil || !result.OK() {
					t.Fatal(result, err)
				}
			} else if err := m.DirectOperateNoReply(ctx, master.LatchOn(0)); err != nil {
				t.Fatal(err)
			}
			waitFor(t, time.Second, func() bool { return breaker.isClosed(0) })
		})
	}
}

// An outstation may challenge with as few as four octets of challenge data
// (IEEE 1815-2012 A.45.1), and some do. The master used to accept exactly 32.
// The outstation here is scripted on the wire, so the challenge length is the
// test's choice rather than this library's.
func TestSecureControlAnswersAShortChallenge(t *testing.T) {
	key := bytes.Repeat([]byte{0x47}, 16)
	mch, och := channel.Pipe()
	m := master.New(master.Config{LocalAddr: 1, RemoteAddr: 10, ResponseTimeout: time.Second,
		SecureAuthentication: &master.SecureAuthenticationConfig{User: 1, UpdateKey: key}}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = m.Run(ctx, mch) }()
	conn, err := och.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	replied := make(chan bool, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		st := stack.New(stack.Config{LocalAddr: 10, RemoteAddr: 1, IsMaster: false})
		var control, monitor, challenge, challenged []byte
		answer := func(r stack.Received, seq uint8, fc app.FuncCode, body []byte) {
			frag := app.AppendHeader(nil, app.Header{Control: app.Control{Fir: true, Fin: true, Seq: seq}, Func: fc})
			_ = st.SendTo(conn, r.Source, append(frag, body...))
		}
		keyStatus := func(r stack.Received, seq uint8, status uint8, mac []byte) {
			h, _ := app.FreeFormat(120, 5, objects.AppendAuthKeyStatus(nil, objects.AuthKeyStatus{Sequence: 1, User: 1,
				WrapAlgorithm: 1, Status: status, Algorithm: 4, Challenge: []byte{1, 2, 3, 4}, MAC: mac}))
			answer(r, seq, app.FuncAuthResponse, app.AppendObjectHeader(nil, h))
		}
		buf := make([]byte, stack.ReadChunk)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				_ = st.Receive(conn, buf[:n], func(r stack.Received) {
					f, perr := app.ParseFragment(nil, r.Fragment)
					if perr != nil || f.Header.Func == app.FuncConfirm {
						return
					}
					seq := f.Header.Control.Seq
					switch {
					case f.Header.Func == app.FuncAuthRequest && len(f.Objects) == 1 && f.Objects[0].Variation == 4:
						keyStatus(r, seq, 2, nil) // not initialised
					case f.Header.Func == app.FuncAuthRequest && len(f.Objects) == 1 && f.Objects[0].Variation == 6:
						data, _ := app.FirstFreeFormatObject(f.Objects[0])
						change, err := objects.ParseAuthKeyChange(data)
						if err != nil {
							return
						}
						plain, err := security.Unwrap(key, change.Wrapped)
						if err != nil {
							return
						}
						n := int(binary.LittleEndian.Uint16(plain))
						control = append([]byte(nil), plain[2:2+n]...)
						monitor = append([]byte(nil), plain[2+n:2+2*n]...)
						keyStatus(r, seq, 1, security.MAC(monitor, r.Fragment))
					case f.Header.Func == app.FuncDirectOperate:
						// A four-octet challenge of the critical request.
						challenged = append([]byte(nil), r.Fragment...)
						h, _ := app.FreeFormat(120, 1, objects.AppendAuthChallenge(nil, objects.AuthChallenge{Sequence: 1, Algorithm: 4, Reason: 1, Data: []byte{9, 8, 7, 6}}))
						challenge = app.AppendHeader(nil, app.Header{Control: app.Control{Fir: true, Fin: true, Seq: seq}, Func: app.FuncAuthResponse})
						challenge = app.AppendObjectHeader(challenge, h)
						_ = st.SendTo(conn, r.Source, challenge)
					case f.Header.Func == app.FuncAuthRequest && len(f.Objects) == 1 && f.Objects[0].Variation == 2:
						data, _ := app.FirstFreeFormatObject(f.Objects[0])
						reply, err := objects.ParseAuthReply(data)
						ok := err == nil && bytes.Equal(reply.MAC, security.MAC(control, challenge, challenged))
						replied <- ok
						if ok {
							answer(r, seq, app.FuncResponse, challenged[2:]) // the controls echoed, status 0
						}
					default:
						answer(r, seq, app.FuncResponse, nil)
					}
				})
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { cancel(); _ = conn.Close(); _ = mch.Close(); _ = och.Close(); wg.Wait() })
	waitFor(t, 3*time.Second, func() bool { return m.Connected() })

	result, err := m.DirectOperate(ctx, master.LatchOn(0))
	select {
	case ok := <-replied:
		if !ok {
			t.Fatal("the challenge reply does not carry the expected MAC")
		}
	default:
		t.Fatalf("no challenge reply to a four-octet challenge (operate: %v)", err)
	}
	if err != nil || !result.OK() {
		t.Fatal(result, err)
	}
}
