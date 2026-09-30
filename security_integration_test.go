package dnp3_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
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
