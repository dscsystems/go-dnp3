package conformance

import (
	"bytes"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
	"github.com/dscsystems/go-dnp3/outstation"
)

type managingApp struct {
	calls []outstation.ManagementOperation
}

func (a *managingApp) Manage(op outstation.ManagementOperation, _ []byte) bool {
	a.calls = append(a.calls, op)
	return true
}

func TestApplicationManagementAndActivation(t *testing.T) {
	mgr := &managingApp{}
	h := newHarness(t, outstation.Config{Management: mgr, ActivateConfig: func(files []string) objects.ActivationResult {
		if len(files) != 1 || files[0] != "device.cfg" {
			return objects.ActivationResult{Statuses: []objects.ActivationStatus{{Code: 1}}}
		}
		return objects.ActivationResult{Delay: 2 * time.Second, Statuses: []objects.ActivationStatus{{Code: 0, Text: files[0]}}}
	}}, nil)
	for _, fc := range []app.FuncCode{15, 16, 17, 18, 19} {
		var headers []app.ObjectHeader
		if fc >= 16 && fc <= 18 {
			hdr, _ := app.FreeFormat(90, 1, []byte("app"))
			headers = append(headers, hdr)
		}
		r := h.request(fc, headers...)
		if r.Header.IIN.HasAny(app.RequestErrorMask) {
			t.Fatal(r)
		}
	}
	if len(mgr.calls) != 5 {
		t.Fatal(mgr.calls)
	}
	hdr, _ := app.FreeFormat(70, 8, []byte("device.cfg"))
	r := h.request(app.FuncActivateConfig, hdr)
	if len(r.Objects) != 1 || r.Objects[0].Group != 91 {
		t.Fatal(r)
	}
	v, err := objects.ParseActivationResult(r.Objects[0].Data)
	if err != nil || v.Delay != 2*time.Second || v.Statuses[0].Text != "device.cfg" {
		t.Fatalf("%+v: %v", v, err)
	}
}

func TestDatasetStorageReadWriteAndSnapshot(t *testing.T) {
	descriptor, err := objects.AppendDatasetDescriptor(nil, []objects.DatasetElement{{Code: 0, Ancillary: []byte{3}}})
	if err != nil {
		t.Fatal(err)
	}
	// Present values remain prototype-dependent encoded values. This backend
	// validates and applies its own value encoding.
	present := []byte{1, 3, 6, 0, 0, 0, 0, 0, 0, 1, 42}
	var written []byte
	h := newHarness(t, outstation.Config{Datasets: []outstation.DatasetObject{
		{Group: 85, Variation: 1, Index: 3, Data: descriptor},
		{Group: 86, Variation: 1, Index: 3, Data: descriptor},
		{Group: 86, Variation: 2, Index: 3, Data: []byte{1}},
		{Group: 87, Variation: 1, Index: 3, Data: present},
	}, DatasetWrite: func(g, v uint8, data []byte) bool {
		if g != 87 || v != 1 {
			return false
		}
		written = data
		return true
	}}, nil)
	r := h.request(app.FuncRead, app.ReadRange(85, 1, 3, 3), app.ReadRange(86, 2, 3, 3), app.ReadRange(87, 1, 3, 3))
	if len(r.Objects) != 3 || r.Header.IIN.HasAny(app.RequestErrorMask) {
		t.Fatal(r)
	}
	value, err := app.FirstFreeFormatObject(r.Objects[2])
	if err != nil || !bytes.Equal(value, present) {
		t.Fatalf("%x: %v", value, err)
	}
	hdr, _ := app.FreeFormat(87, 1, present)
	r = h.request(app.FuncWrite, hdr)
	if r.Header.IIN.HasAny(app.RequestErrorMask) || !bytes.Equal(written, present) {
		t.Fatal(r)
	}
	h.out.Update(func(db *outstation.Database) {
		if err := db.UpdateDataset(outstation.DatasetObject{Group: 87, Variation: 1, Index: 3, Data: present}, dnp3.Class2); err != nil {
			panic(err)
		}
	})
	waitFor(t, func() bool { return h.out.Events().Total() == 1 })
	r = h.request(app.FuncRead, app.ReadAllObjects(88, 0))
	if len(r.Objects) != 1 || r.Objects[0].Group != 88 || !r.Header.Control.Con {
		t.Fatal(r)
	}
	value, err = app.FirstFreeFormatObject(r.Objects[0])
	if err != nil || !bytes.Equal(value, present) {
		t.Fatalf("%x: %v", value, err)
	}
	h.sendConfirm(r.Header.Control.Seq)
	waitFor(t, func() bool { return h.out.Events().Total() == 0 })
}
