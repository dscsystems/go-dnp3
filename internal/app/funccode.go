// Package app implements the DNP3 application layer framing: the fragment
// header, function codes, internal indications, and the object headers with
// their qualifier, range and prefix encodings.
//
// It does not decode object data. Walking a fragment requires knowing how
// large each object is, which is a property of its group and variation; that
// knowledge is supplied through the [ObjectSizer] interface so this package
// stays independent of the object codecs.
//
// Nothing here performs I/O or reads a clock.
package app

import "fmt"

// FuncCode is an application layer function code.
type FuncCode uint8

// Request function codes, sent by a master to an outstation.
const (
	FuncConfirm            FuncCode = 0
	FuncRead               FuncCode = 1
	FuncWrite              FuncCode = 2
	FuncSelect             FuncCode = 3
	FuncOperate            FuncCode = 4
	FuncDirectOperate      FuncCode = 5
	FuncDirectOperateNR    FuncCode = 6
	FuncImmedFreeze        FuncCode = 7
	FuncImmedFreezeNR      FuncCode = 8
	FuncFreezeClear        FuncCode = 9
	FuncFreezeClearNR      FuncCode = 10
	FuncFreezeAtTime       FuncCode = 11
	FuncFreezeAtTimeNR     FuncCode = 12
	FuncColdRestart        FuncCode = 13
	FuncWarmRestart        FuncCode = 14
	FuncInitializeData     FuncCode = 15
	FuncInitializeAppl     FuncCode = 16
	FuncStartAppl          FuncCode = 17
	FuncStopAppl           FuncCode = 18
	FuncSaveConfig         FuncCode = 19
	FuncEnableUnsolicited  FuncCode = 20
	FuncDisableUnsolicited FuncCode = 21
	FuncAssignClass        FuncCode = 22
	FuncDelayMeasure       FuncCode = 23
	FuncRecordCurrentTime  FuncCode = 24
	FuncOpenFile           FuncCode = 25
	FuncCloseFile          FuncCode = 26
	FuncDeleteFile         FuncCode = 27
	FuncGetFileInfo        FuncCode = 28
	FuncAuthenticateFile   FuncCode = 29
	FuncAbortFile          FuncCode = 30
	FuncActivateConfig     FuncCode = 31
	FuncAuthRequest        FuncCode = 32
	FuncAuthRequestNoAck   FuncCode = 33
)

// Response function codes, sent by an outstation to a master.
const (
	FuncResponse            FuncCode = 129
	FuncUnsolicitedResponse FuncCode = 130
	FuncAuthResponse        FuncCode = 131
)

var funcNames = map[FuncCode]string{
	FuncConfirm:             "CONFIRM",
	FuncRead:                "READ",
	FuncWrite:               "WRITE",
	FuncSelect:              "SELECT",
	FuncOperate:             "OPERATE",
	FuncDirectOperate:       "DIRECT_OPERATE",
	FuncDirectOperateNR:     "DIRECT_OPERATE_NR",
	FuncImmedFreeze:         "IMMED_FREEZE",
	FuncImmedFreezeNR:       "IMMED_FREEZE_NR",
	FuncFreezeClear:         "FREEZE_CLEAR",
	FuncFreezeClearNR:       "FREEZE_CLEAR_NR",
	FuncFreezeAtTime:        "FREEZE_AT_TIME",
	FuncFreezeAtTimeNR:      "FREEZE_AT_TIME_NR",
	FuncColdRestart:         "COLD_RESTART",
	FuncWarmRestart:         "WARM_RESTART",
	FuncInitializeData:      "INITIALIZE_DATA",
	FuncInitializeAppl:      "INITIALIZE_APPL",
	FuncStartAppl:           "START_APPL",
	FuncStopAppl:            "STOP_APPL",
	FuncSaveConfig:          "SAVE_CONFIG",
	FuncEnableUnsolicited:   "ENABLE_UNSOLICITED",
	FuncDisableUnsolicited:  "DISABLE_UNSOLICITED",
	FuncAssignClass:         "ASSIGN_CLASS",
	FuncDelayMeasure:        "DELAY_MEASURE",
	FuncRecordCurrentTime:   "RECORD_CURRENT_TIME",
	FuncOpenFile:            "OPEN_FILE",
	FuncCloseFile:           "CLOSE_FILE",
	FuncDeleteFile:          "DELETE_FILE",
	FuncGetFileInfo:         "GET_FILE_INFO",
	FuncAuthenticateFile:    "AUTHENTICATE_FILE",
	FuncAbortFile:           "ABORT_FILE",
	FuncActivateConfig:      "ACTIVATE_CONFIG",
	FuncAuthRequest:         "AUTHENTICATE_REQ",
	FuncAuthRequestNoAck:    "AUTHENTICATE_REQ_NO_ACK",
	FuncResponse:            "RESPONSE",
	FuncUnsolicitedResponse: "UNSOLICITED_RESPONSE",
	FuncAuthResponse:        "AUTHENTICATE_RESP",
}

func (f FuncCode) String() string {
	if n, ok := funcNames[f]; ok {
		return n
	}
	return fmt.Sprintf("FUNC_%d", uint8(f))
}

// IsResponse reports whether the code identifies a fragment carrying an IIN
// field, which changes how the header is parsed.
func (f FuncCode) IsResponse() bool {
	return f == FuncResponse || f == FuncUnsolicitedResponse || f == FuncAuthResponse
}

// IsKnown reports whether the code is defined by the standard. An outstation
// answers an unknown code with IIN2.NO_FUNC_CODE_SUPPORT.
func (f FuncCode) IsKnown() bool {
	_, ok := funcNames[f]
	return ok
}

// NoReply reports whether the code is one of the "no response" variants, which
// an outstation must execute without answering.
func (f FuncCode) NoReply() bool {
	switch f {
	case FuncDirectOperateNR, FuncImmedFreezeNR, FuncFreezeClearNR,
		FuncFreezeAtTimeNR, FuncAuthRequestNoAck:
		return true
	default:
		return false
	}
}

// CarriesObjectData reports whether object headers in a fragment with this
// function code are followed by object data.
//
// This is not a property of the object header alone. In a READ request an
// object header is a *specification* — "give me group 30 variation 2, indexes
// 0 through 15" — and no data follows it, even though that same header in a
// response would introduce sixteen analog values. A parser that sized the
// header from the object table in both cases would run off the end of every
// read request it saw.
//
// The same holds for the freeze, unsolicited-enable and assign-class requests,
// whose headers name points rather than carrying them.
//
// Known limitation: FREEZE_AT_TIME is genuinely mixed — its leading group 50
// variation 2 object carries a time and interval, while the counter headers
// after it are specifications. It is treated as carrying data here, which is
// right for the first header and wrong for the rest. Resolving that needs
// per-object semantics rather than a per-fragment rule.
func (f FuncCode) CarriesObjectData() bool {
	switch f {
	case FuncRead,
		FuncImmedFreeze, FuncImmedFreezeNR,
		FuncFreezeClear, FuncFreezeClearNR,
		FuncEnableUnsolicited, FuncDisableUnsolicited,
		FuncAssignClass,
		FuncConfirm,
		FuncColdRestart, FuncWarmRestart,
		FuncDelayMeasure, FuncRecordCurrentTime,
		FuncInitializeData, FuncSaveConfig,
		FuncGetFileInfo, FuncDeleteFile:
		return false
	default:
		return true
	}
}

// RequiresObjects reports whether a request with this function code is
// meaningless without at least one object header — a read that names nothing
// to read, a control that names nothing to operate. An outstation answers one
// carrying no objects with IIN2.PARAMETER_ERROR and a null response, rather
// than with the empty success it would otherwise look like.
//
// Codes that legitimately carry nothing are absent: CONFIRM, the restarts,
// DELAY_MEASURE and RECORD_CURRENT_TIME take no objects at all, and an empty
// freeze means every counter rather than none.
//
// The file and authentication codes are absent too, for a different reason.
// They do require their objects, but their handlers parse those objects
// themselves and report a more precise failure than this blanket check could
// — a file request on an outstation with no file handler should be told the
// function is unsupported, not that its parameters were wrong.
func (f FuncCode) RequiresObjects() bool {
	switch f {
	case FuncRead, FuncWrite,
		FuncSelect, FuncOperate, FuncDirectOperate, FuncDirectOperateNR,
		FuncFreezeAtTime, FuncFreezeAtTimeNR,
		FuncEnableUnsolicited, FuncDisableUnsolicited,
		FuncAssignClass:
		return true
	default:
		return false
	}
}

// IsControl reports whether the code operates output points, which is the set
// an outstation may want to gate behind authorisation.
func (f FuncCode) IsControl() bool {
	switch f {
	case FuncSelect, FuncOperate, FuncDirectOperate, FuncDirectOperateNR:
		return true
	default:
		return false
	}
}
