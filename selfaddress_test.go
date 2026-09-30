package dnp3_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/internal/link"
	"github.com/dscsystems/go-dnp3/master"
	"github.com/dscsystems/go-dnp3/outstation"
)

// A master that does not know its outstation's address sends to the self
// address and is answered from the real one. That has to work with link
// confirmations too, where the acknowledgement of each frame also comes from
// an address the master was not sent to.
func TestSelfAddressDiscoveryWorksWithAndWithoutLinkConfirms(t *testing.T) {
	for _, confirms := range []bool{false, true} {
		name := "unconfirmed"
		if confirms {
			name = "link confirms"
		}
		t.Run(name, func(t *testing.T) {
			mch, och := channel.Pipe()
			out := outstation.New(outstation.Config{
				LocalAddr: 10, RemoteAddr: 1, SelfAddress: true, UseLinkConfirms: confirms,
				LinkTimeout: 200 * time.Millisecond, LinkRetries: 1,
				Database: outstation.DatabaseConfig{Binary: 2, DefaultClass: dnp3.Class1},
			}, nil, nil)
			m := master.New(master.Config{
				LocalAddr: 1, RemoteAddr: link.SelfAddress, UseLinkConfirms: confirms,
				LinkTimeout: 200 * time.Millisecond, LinkRetries: 1, ResponseTimeout: time.Second,
			}, master.NopHandler{})

			ctx, cancel := context.WithCancel(t.Context())
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); _ = out.Run(ctx, och) }()
			go func() { defer wg.Done(); _ = m.Run(ctx, mch) }()
			t.Cleanup(func() { cancel(); _ = mch.Close(); _ = och.Close(); wg.Wait() })
			waitFor(t, 3*time.Second, func() bool { return m.Connected() })

			pollCtx, pollCancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer pollCancel()
			for range 3 { // more than one exchange, so the state after discovery is used too
				if err := m.ScanClasses(pollCtx, dnp3.Class0); err != nil {
					t.Fatalf("poll of an outstation found by self address: %v", err)
				}
			}
		})
	}
}
