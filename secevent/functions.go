package secevent

import (
	"slices"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
)

// FunctionPolicy refuses application functions an outstation should never
// carry out for this deployment, whoever asks. A refused function is
// answered NO_FUNC_CODE_SUPPORT — a refused control gets DenyStatus — the
// handler is never called, and access.function_blocked is reported.
//
// It complements SAv5's Authorize callback: Authorize decides per
// authenticated user, a FunctionPolicy for every peer alike.
type FunctionPolicy struct {
	// Deny lists application function codes to refuse.
	Deny []uint8
	// ReadOnly refuses everything that changes the outstation: controls,
	// freezes, class assignment, restarts, application and configuration
	// functions, file writes and deletes, and writes other than clearing
	// the restart indication — and time, when AllowTimeWrite.
	ReadOnly bool
	// AllowTimeWrite lets a read-only outstation have its clock set.
	AllowTimeWrite bool

	DenyRestart            bool // COLD_RESTART, WARM_RESTART
	DenyApplicationControl bool // INITIALIZE_DATA, INITIALIZE/START/STOP_APPL, SAVE/ACTIVATE_CONFIG
	DenyFileDelete         bool // DELETE_FILE
	DenyAssignClass        bool // ASSIGN_CLASS
	DenyDeadbandWrite      bool // WRITE of group 34
	DenyTerminal           bool // WRITE of group 112
	DenyUnsolDisable       bool // DISABLE_UNSOLICITED

	// DenyStatus is the status a refused control is answered with. Zero
	// uses NOT_AUTHORIZED.
	DenyStatus dnp3.CommandStatus
}

// Blocks reports whether the policy refuses function fc outright. WRITE is
// judged per object by [FunctionPolicy.BlocksWrite].
func (fp *FunctionPolicy) Blocks(fc uint8) bool {
	if fp == nil {
		return false
	}
	if slices.Contains(fp.Deny, fc) {
		return true
	}
	switch f := app.FuncCode(fc); f {
	case app.FuncColdRestart, app.FuncWarmRestart:
		return fp.DenyRestart || fp.ReadOnly
	case app.FuncInitializeData, app.FuncInitializeAppl, app.FuncStartAppl, app.FuncStopAppl,
		app.FuncSaveConfig, app.FuncActivateConfig:
		return fp.DenyApplicationControl || fp.ReadOnly
	case app.FuncDeleteFile:
		return fp.DenyFileDelete || fp.ReadOnly
	case app.FuncAssignClass:
		return fp.DenyAssignClass || fp.ReadOnly
	case app.FuncDisableUnsolicited:
		return fp.DenyUnsolDisable
	case app.FuncSelect, app.FuncOperate, app.FuncDirectOperate, app.FuncDirectOperateNR,
		app.FuncImmedFreeze, app.FuncImmedFreezeNR, app.FuncFreezeClear, app.FuncFreezeClearNR,
		app.FuncFreezeAtTime, app.FuncFreezeAtTimeNR:
		return fp.ReadOnly
	}
	return false
}

// BlocksWrite reports whether the policy refuses a WRITE of group g.
func (fp *FunctionPolicy) BlocksWrite(g uint8) bool {
	if fp == nil {
		return false
	}
	switch g {
	case 34:
		return fp.DenyDeadbandWrite || fp.ReadOnly
	case 112:
		return fp.DenyTerminal || fp.ReadOnly
	case 80:
		return false // clearing the restart indication changes nothing
	case 50:
		return fp.ReadOnly && !fp.AllowTimeWrite
	}
	return fp.ReadOnly
}

// CommandDenyStatus returns the status a refused control is answered with.
func (fp *FunctionPolicy) CommandDenyStatus() dnp3.CommandStatus {
	if fp == nil || fp.DenyStatus == 0 {
		return dnp3.CommandNotAuthorized
	}
	return fp.DenyStatus
}

// DefaultBroadcastAllowed is the functions a broadcast may carry without
// being reported: setting the time and freezing counters, which every
// outstation on a line is meant to do at once.
var DefaultBroadcastAllowed = []uint8{
	uint8(app.FuncWrite), // of time and the restart indication only
	uint8(app.FuncRecordCurrentTime),
	uint8(app.FuncImmedFreeze), uint8(app.FuncImmedFreezeNR),
	uint8(app.FuncFreezeClear), uint8(app.FuncFreezeClearNR),
	uint8(app.FuncFreezeAtTime), uint8(app.FuncFreezeAtTimeNR),
	uint8(app.FuncDelayMeasure),
}

// BroadcastAllows reports whether a broadcast of function fc, writing the
// given groups when fc is WRITE, is expected under the policy.
func (p *Policy) BroadcastAllows(fc uint8, groups []uint8) bool {
	allowed := DefaultBroadcastAllowed
	if p != nil && p.BroadcastAllowed != nil {
		allowed = p.BroadcastAllowed
	}
	switch app.FuncCode(fc) {
	case app.FuncRead, app.FuncConfirm:
		return true // changes nothing
	case app.FuncWrite:
		if !slices.Contains(allowed, fc) {
			return false
		}
		for _, g := range groups {
			if g != 50 && g != 80 {
				return false
			}
		}
		return true
	}
	return slices.Contains(allowed, fc)
}

// Scan detection defaults.
const (
	DefaultScanThreshold = 20
	DefaultScanWindow    = time.Minute
)

// Scan returns the effective scan threshold and window; a zero threshold
// means detection is off.
func (p *Policy) Scan() (int, time.Duration) {
	n, w := DefaultScanThreshold, DefaultScanWindow
	if p != nil {
		if p.ScanThreshold < 0 {
			return 0, 0
		}
		if p.ScanThreshold > 0 {
			n = p.ScanThreshold
		}
		if p.ScanWindow > 0 {
			w = p.ScanWindow
		}
	}
	return n, w
}
