package conformance

import (
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/outstation"
)

func iinOfRead(h *harness) app.IIN {
	return h.request(app.FuncRead, app.ReadAllObjects(60, 1)).Header.IIN
}

// The three device-controlled indications are asserted while set and cleared
// when cleared, and the application cannot use SetIndication to assert the
// ones that describe the protocol.
func TestDeviceControlledIndications(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database:    smallDB(),
		Indications: outstation.IndicationConfigCorrupt,
	}, nil)

	if iin := iinOfRead(h); !iin.Has(app.IINConfigCorrupt) {
		t.Errorf("IIN = %v, want CONFIG_CORRUPT from the configuration", iin)
	}

	h.out.SetIndication(outstation.IndicationLocalControl|outstation.IndicationDeviceTrouble, true)
	iin := iinOfRead(h)
	if !iin.Has(app.IINLocalControl) || !iin.Has(app.IINDeviceTrouble) || !iin.Has(app.IINConfigCorrupt) {
		t.Errorf("IIN = %v, want LOCAL_CONTROL, DEVICE_TROUBLE and CONFIG_CORRUPT", iin)
	}
	// They persist: unlike a request error, one response does not clear them.
	if iin := iinOfRead(h); !iin.Has(app.IINLocalControl) {
		t.Errorf("IIN = %v, LOCAL_CONTROL did not persist", iin)
	}

	h.out.SetIndication(outstation.IndicationDeviceTrouble|outstation.IndicationConfigCorrupt, false)
	iin = iinOfRead(h)
	if iin.Has(app.IINDeviceTrouble) || iin.Has(app.IINConfigCorrupt) || !iin.Has(app.IINLocalControl) {
		t.Errorf("IIN = %v, want only LOCAL_CONTROL", iin)
	}

	// NEED_TIME belongs to the library.
	h.out.SetIndication(outstation.Indication(app.IINNeedTime|app.IINDeviceRestart), false)
	if iin := iinOfRead(h); !iin.Has(app.IINNeedTime) {
		t.Errorf("IIN = %v: SetIndication cleared a bit that is not the application's", iin)
	}
}

type restartApp struct {
	outstation.NopApplication
	mu    sync.Mutex
	colds int
}

func (a *restartApp) ColdRestart() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.colds++
	return 2 * time.Second
}

// A second restart request while the first is still under way is understood
// but not carried out again.
func TestRepeatedRestartIsAlreadyExecuting(t *testing.T) {
	a := &restartApp{}
	h := newHarnessWithApp(t, outstation.Config{Database: smallDB()}, a)

	first := h.request(app.FuncColdRestart)
	if first.Header.IIN.Has(app.IINAlreadyExecuting) {
		t.Fatal("the first restart was reported as already executing")
	}

	second := h.request(app.FuncColdRestart)
	if !second.Header.IIN.Has(app.IINAlreadyExecuting) {
		t.Errorf("IIN = %v, want ALREADY_EXECUTING", second.Header.IIN)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.colds != 1 {
		t.Errorf("the application was asked to restart %d times, want 1", a.colds)
	}
}

// The same freeze asked for twice is one freeze.
func TestDuplicateFreezeIsAlreadyExecuting(t *testing.T) {
	h := freezeHarness(t)
	at := time.Now().Add(time.Hour)

	first := h.request(app.FuncFreezeAtTime, freezeAt(at, 0), app.ReadAllObjects(20, 0))
	if first.Header.IIN.Has(app.IINAlreadyExecuting) {
		t.Fatal("the first request was reported as already executing")
	}
	second := h.request(app.FuncFreezeAtTime, freezeAt(at, 0), app.ReadAllObjects(20, 0))
	if !second.Header.IIN.Has(app.IINAlreadyExecuting) {
		t.Errorf("IIN = %v, want ALREADY_EXECUTING", second.Header.IIN)
	}
}
