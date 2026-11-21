// The event catalogue. Each kind is listed once, with its defaults, in kindTable.

package secevent

// Kinds. The string values are public API: they are never renamed, only
// added to and deprecated.
const (
	KindConnAccepted                Kind = "conn.accepted"
	KindConnConnected               Kind = "conn.connected"
	KindConnUnexpected              Kind = "conn.unexpected"
	KindConnUnexpectedMAC           Kind = "conn.unexpected_mac"
	KindConnRejectedBusy            Kind = "conn.rejected_busy"
	KindConnRate                    Kind = "conn.rate"
	KindConnNoData                  Kind = "conn.no_data"
	KindConnIdleTimeout             Kind = "conn.idle_timeout"
	KindConnClosed                  Kind = "conn.closed"
	KindConnKeepaliveFailed         Kind = "conn.keepalive_failed"
	KindConnDialFailed              Kind = "conn.dial_failed"
	KindConnAcceptError             Kind = "conn.accept_error"
	KindConnUDPForeignSource        Kind = "conn.udp_foreign_source"
	KindTLSCertRejected             Kind = "tls.cert_rejected"
	KindTLSNoClientCert             Kind = "tls.no_client_cert"
	KindTLSLocalCertRejected        Kind = "tls.local_cert_rejected"
	KindTLSPlaintextOnTLSPort       Kind = "tls.plaintext_on_tls_port"
	KindTLSNonTLSProbe              Kind = "tls.non_tls_probe"
	KindTLSHandshakeFailed          Kind = "tls.handshake_failed"
	KindTLSHandshakeTimeout         Kind = "tls.handshake_timeout"
	KindTLSWeakParams               Kind = "tls.weak_params"
	KindTLSCertExpiring             Kind = "tls.cert_expiring"
	KindTLSCertChanged              Kind = "tls.cert_changed"
	KindPeerMACChanged              Kind = "peer.mac_changed"
	KindPeerMACChangedMidsession    Kind = "peer.mac_changed_midsession"
	KindPeerMACPinningViolation     Kind = "peer.mac_pinning_violation"
	KindPeerIPChanged               Kind = "peer.ip_changed"
	KindPeerMACShared               Kind = "peer.mac_shared"
	KindPeerMACUnavailable          Kind = "peer.mac_unavailable"
	KindPeerLinkAddrChanged         Kind = "peer.link_addr_changed"
	KindLinkCRCError                Kind = "link.crc_error"
	KindLinkBadLength               Kind = "link.bad_length"
	KindLinkGarbage                 Kind = "link.garbage"
	KindLinkForeignDest             Kind = "link.foreign_dest"
	KindLinkWrongDir                Kind = "link.wrong_dir"
	KindLinkInvalidSource           Kind = "link.invalid_source"
	KindLinkUnexpectedMaster        Kind = "link.unexpected_master"
	KindLinkUnexpectedSource        Kind = "link.unexpected_source"
	KindLinkInvalidControl          Kind = "link.invalid_control"
	KindLinkNotReset                Kind = "link.not_reset"
	KindLinkUnsupportedFunction     Kind = "link.unsupported_function"
	KindLinkBroadcast               Kind = "link.broadcast"
	KindLinkSelfAddress             Kind = "link.self_address"
	KindTransportDiscard            Kind = "transport.discard"
	KindProtoAppMalformed           Kind = "proto.app_malformed"
	KindProtoIncompleteRequest      Kind = "proto.incomplete_request"
	KindProtoConfirmWithObjects     Kind = "proto.confirm_with_objects"
	KindProtoRequestCONUNS          Kind = "proto.request_con_uns"
	KindProtoInconsistentQualifier  Kind = "proto.inconsistent_qualifier"
	KindProtoNoObjects              Kind = "proto.no_objects"
	KindProtoWrongDirection         Kind = "proto.wrong_direction"
	KindProtoInvalidResponse        Kind = "proto.invalid_response"
	KindProtoParameterError         Kind = "proto.parameter_error"
	KindProtoFieldInvalid           Kind = "proto.field_invalid"
	KindProtoSecureAuthUnsupported  Kind = "proto.secure_auth_unsupported"
	KindAccessUnsupportedFunction   Kind = "access.unsupported_function"
	KindAccessUnknownObject         Kind = "access.unknown_object"
	KindAccessUnknownPoint          Kind = "access.unknown_point"
	KindAccessScan                  Kind = "access.scan"
	KindAccessFunctionBlocked       Kind = "access.function_blocked"
	KindAccessBroadcastMisuse       Kind = "access.broadcast_misuse"
	KindAccessOversizedRequest      Kind = "access.oversized_request"
	KindAuthChallenge               Kind = "auth.challenge"
	KindAuthSuccess                 Kind = "auth.success"
	KindAuthFailure                 Kind = "auth.failure"
	KindAuthAuthorizationDenied     Kind = "auth.authorization_denied"
	KindAuthReplyTimeout            Kind = "auth.reply_timeout"
	KindAuthUnknownUser             Kind = "auth.unknown_user"
	KindAuthKeyChange               Kind = "auth.key_change"
	KindAuthKeyChangeFailed         Kind = "auth.key_change_failed"
	KindAuthKeyExpired              Kind = "auth.key_expired"
	KindAuthErrorReceived           Kind = "auth.error_received"
	KindAuthBroadcastCritical       Kind = "auth.broadcast_critical"
	KindAuthObjectSmuggled          Kind = "auth.object_smuggled"
	KindAuthBruteForce              Kind = "auth.brute_force"
	KindAuthMasterChallengeFailed   Kind = "auth.master_challenge_failed"
	KindAuthMasterErrorReceived     Kind = "auth.master_error_received"
	KindAuthMasterKeyChange         Kind = "auth.master_key_change"
	KindAuthFileFailed              Kind = "auth.file_failed"
	KindSeqFCBRepeat                Kind = "seq.fcb_repeat"
	KindSeqTestLinkNack             Kind = "seq.test_link_nack"
	KindSeqRepeatRequest            Kind = "seq.repeat_request"
	KindSeqReusedSeq                Kind = "seq.reused_seq"
	KindSeqResponseStale            Kind = "seq.response_stale"
	KindSeqResponseRepeat           Kind = "seq.response_repeat"
	KindSeqUnsolDuplicate           Kind = "seq.unsol_duplicate"
	KindCmdDuplicate                Kind = "cmd.duplicate"
	KindCmdReceived                 Kind = "cmd.received"
	KindCmdRejected                 Kind = "cmd.rejected"
	KindCmdBruteForce               Kind = "cmd.brute_force"
	KindCmdOperateWithoutSelect     Kind = "cmd.operate_without_select"
	KindCmdSelectTimeout            Kind = "cmd.select_timeout"
	KindCmdOperateSeqGap            Kind = "cmd.operate_seq_gap"
	KindCmdOperateMismatch          Kind = "cmd.operate_mismatch"
	KindCmdSelectAbandoned          Kind = "cmd.select_abandoned"
	KindCmdNoAckOperate             Kind = "cmd.no_ack_operate"
	KindCmdLocalMode                Kind = "cmd.local_mode"
	KindCtrlColdRestart             Kind = "ctrl.cold_restart"
	KindCtrlWarmRestart             Kind = "ctrl.warm_restart"
	KindCtrlAlreadyExecuting        Kind = "ctrl.already_executing"
	KindCtrlStopApplication         Kind = "ctrl.stop_application"
	KindCtrlStartApplication        Kind = "ctrl.start_application"
	KindCtrlInitializeApplication   Kind = "ctrl.initialize_application"
	KindCtrlInitializeData          Kind = "ctrl.initialize_data"
	KindCtrlSaveConfig              Kind = "ctrl.save_config"
	KindCtrlActivateConfig          Kind = "ctrl.activate_config"
	KindCtrlTimeWrite               Kind = "ctrl.time_write"
	KindCtrlClockStep               Kind = "ctrl.clock_step"
	KindCtrlRecordCurrentTime       Kind = "ctrl.record_current_time"
	KindCtrlDelayMeasure            Kind = "ctrl.delay_measure"
	KindCtrlUnsolDisable            Kind = "ctrl.unsol_disable"
	KindCtrlUnsolEnable             Kind = "ctrl.unsol_enable"
	KindCtrlAssignClass             Kind = "ctrl.assign_class"
	KindCtrlDeadbandWrite           Kind = "ctrl.deadband_write"
	KindCtrlFreeze                  Kind = "ctrl.freeze"
	KindCtrlFreezeClear             Kind = "ctrl.freeze_clear"
	KindCtrlFreezeAtTime            Kind = "ctrl.freeze_at_time"
	KindCtrlRestartIINCleared       Kind = "ctrl.restart_iin_cleared"
	KindCtrlAttributeWrite          Kind = "ctrl.attribute_write"
	KindCtrlTerminalWrite           Kind = "ctrl.terminal_write"
	KindCtrlDatasetWrite            Kind = "ctrl.dataset_write"
	KindCtrlTimeIntervalWrite       Kind = "ctrl.time_interval_write"
	KindCtrlIndicationChange        Kind = "ctrl.indication_change"
	KindRemoteRestart               Kind = "remote.restart"
	KindRemoteConfigCorrupt         Kind = "remote.config_corrupt"
	KindRemoteDeviceTrouble         Kind = "remote.device_trouble"
	KindRemoteLocalControl          Kind = "remote.local_control"
	KindRemoteEventOverflow         Kind = "remote.event_overflow"
	KindRemoteRejected              Kind = "remote.rejected"
	KindUnsolUnexpectedConfirm      Kind = "unsol.unexpected_confirm"
	KindUnsolResponseWithoutRequest Kind = "unsol.response_without_request"
	KindUnsolWhileDisabled          Kind = "unsol.while_disabled"
	KindFileDirectoryList           Kind = "file.directory_list"
	KindFileReadOpened              Kind = "file.read_opened"
	KindFileWriteOpened             Kind = "file.write_opened"
	KindFileDelete                  Kind = "file.delete"
	KindFileRejected                Kind = "file.rejected"
	KindFileSuspiciousName          Kind = "file.suspicious_name"
	KindFileInvalidHandle           Kind = "file.invalid_handle"
	KindFileOversized               Kind = "file.oversized"
	KindFileTimeout                 Kind = "file.timeout"
	KindFileAborted                 Kind = "file.aborted"
	KindFloodCommands               Kind = "flood.commands"
	KindFloodIntegrity              Kind = "flood.integrity"
	KindFloodEventPolls             Kind = "flood.event_polls"
	KindFloodRestart                Kind = "flood.restart"
	KindFloodTimeWrite              Kind = "flood.time_write"
	KindFloodAppControl             Kind = "flood.app_control"
	KindFloodLinkStatus             Kind = "flood.link_status"
	KindFloodLinkReset              Kind = "flood.link_reset"
	KindFloodAuthChallenges         Kind = "flood.auth_challenges"
	KindFloodFileOps                Kind = "flood.file_ops"
	KindFloodFrames                 Kind = "flood.frames"
	KindFloodUnsolicited            Kind = "flood.unsolicited"
	KindDoSRxBacklog                Kind = "dos.rx_backlog"
	KindDoSWriteStall               Kind = "dos.write_stall"
	KindDoSSlowFrame                Kind = "dos.slow_frame"
	KindDoSConfirmWithheld          Kind = "dos.confirm_withheld"
	KindDoSEventOverflow            Kind = "dos.event_overflow"
	KindDoSLinkRetriesExhausted     Kind = "dos.link_retries_exhausted"
	KindDoSQueueDrop                Kind = "dos.queue_drop"
	KindMetaEventsDropped           Kind = "meta.events_dropped"
	KindMetaSinkPanic               Kind = "meta.sink_panic"
)

// kindTable fixes the default category, severity and ATT&CK hint of each
// kind, so call sites pass only what varies.
var kindTable = map[Kind]kindInfo{
	KindConnAccepted:                {CatConnection, Info, "", false},
	KindConnConnected:               {CatConnection, Info, "", false},
	KindConnUnexpected:              {CatConnection, Warning, "T0849", false},
	KindConnUnexpectedMAC:           {CatConnection, Alert, "T0849", true},
	KindConnRejectedBusy:            {CatConnection, Notice, "", false},
	KindConnRate:                    {CatConnection, Alert, "T0814", false},
	KindConnNoData:                  {CatConnection, Notice, "T0846", false},
	KindConnIdleTimeout:             {CatConnection, Warning, "T0814", false},
	KindConnClosed:                  {CatConnection, Info, "", false},
	KindConnKeepaliveFailed:         {CatConnection, Notice, "", false},
	KindConnDialFailed:              {CatConnection, Notice, "", false},
	KindConnAcceptError:             {CatConnection, Critical, "T0814", false},
	KindConnUDPForeignSource:        {CatConnection, Warning, "T0849", false},
	KindTLSCertRejected:             {CatTLS, Alert, "T0849", true},
	KindTLSNoClientCert:             {CatTLS, Alert, "T0849", false},
	KindTLSLocalCertRejected:        {CatTLS, Warning, "T0830", false},
	KindTLSPlaintextOnTLSPort:       {CatTLS, Warning, "", false},
	KindTLSNonTLSProbe:              {CatTLS, Notice, "T0846", false},
	KindTLSHandshakeFailed:          {CatTLS, Warning, "", false},
	KindTLSHandshakeTimeout:         {CatTLS, Warning, "T0814", false},
	KindTLSWeakParams:               {CatTLS, Notice, "", false},
	KindTLSCertExpiring:             {CatTLS, Notice, "", false},
	KindTLSCertChanged:              {CatTLS, Notice, "T0830", false},
	KindPeerMACChanged:              {CatPeer, Alert, "T0830", true},
	KindPeerMACChangedMidsession:    {CatPeer, Critical, "T0830", true},
	KindPeerMACPinningViolation:     {CatPeer, Critical, "T0849", true},
	KindPeerIPChanged:               {CatPeer, Info, "", false},
	KindPeerMACShared:               {CatPeer, Notice, "T0830", false},
	KindPeerMACUnavailable:          {CatPeer, Info, "", false},
	KindPeerLinkAddrChanged:         {CatPeer, Warning, "T0849", false},
	KindLinkCRCError:                {CatLink, Notice, "", false},
	KindLinkBadLength:               {CatLink, Notice, "", false},
	KindLinkGarbage:                 {CatLink, Notice, "", false},
	KindLinkForeignDest:             {CatLink, Notice, "", false},
	KindLinkWrongDir:                {CatLink, Warning, "T0849", false},
	KindLinkInvalidSource:           {CatLink, Warning, "", false},
	KindLinkUnexpectedMaster:        {CatLink, Warning, "T0849", false},
	KindLinkUnexpectedSource:        {CatLink, Warning, "T0830", false},
	KindLinkInvalidControl:          {CatLink, Warning, "", false},
	KindLinkNotReset:                {CatLink, Notice, "", false},
	KindLinkUnsupportedFunction:     {CatLink, Warning, "", false},
	KindLinkBroadcast:               {CatLink, Notice, "", false},
	KindLinkSelfAddress:             {CatLink, Notice, "T0846", false},
	KindTransportDiscard:            {CatTransport, Notice, "", false},
	KindProtoAppMalformed:           {CatProtocol, Warning, "", false},
	KindProtoIncompleteRequest:      {CatProtocol, Warning, "", false},
	KindProtoConfirmWithObjects:     {CatProtocol, Warning, "", false},
	KindProtoRequestCONUNS:          {CatProtocol, Warning, "", false},
	KindProtoInconsistentQualifier:  {CatProtocol, Warning, "", false},
	KindProtoNoObjects:              {CatProtocol, Warning, "", false},
	KindProtoWrongDirection:         {CatProtocol, Warning, "", false},
	KindProtoInvalidResponse:        {CatProtocol, Warning, "T0856", false},
	KindProtoParameterError:         {CatProtocol, Notice, "", false},
	KindProtoFieldInvalid:           {CatProtocol, Warning, "", false},
	KindProtoSecureAuthUnsupported:  {CatProtocol, Notice, "T0846", false},
	KindAccessUnsupportedFunction:   {CatAccess, Warning, "T0888", false},
	KindAccessUnknownObject:         {CatAccess, Warning, "T0861", false},
	KindAccessUnknownPoint:          {CatAccess, Warning, "T0861", false},
	KindAccessScan:                  {CatAccess, Alert, "T0861", false},
	KindAccessFunctionBlocked:       {CatAccess, Warning, "T0855", false},
	KindAccessBroadcastMisuse:       {CatAccess, Alert, "T0855", false},
	KindAccessOversizedRequest:      {CatAccess, Warning, "T0814", false},
	KindAuthChallenge:               {CatAuth, Info, "", false},
	KindAuthSuccess:                 {CatAuth, Info, "", false},
	KindAuthFailure:                 {CatAuth, Alert, "T0855", true},
	KindAuthAuthorizationDenied:     {CatAuth, Alert, "T0855", false},
	KindAuthReplyTimeout:            {CatAuth, Warning, "", false},
	KindAuthUnknownUser:             {CatAuth, Warning, "T0806", false},
	KindAuthKeyChange:               {CatAuth, Notice, "", false},
	KindAuthKeyChangeFailed:         {CatAuth, Alert, "T0830", false},
	KindAuthKeyExpired:              {CatAuth, Info, "", false},
	KindAuthErrorReceived:           {CatAuth, Notice, "", false},
	KindAuthBroadcastCritical:       {CatAuth, Warning, "", false},
	KindAuthObjectSmuggled:          {CatAuth, Alert, "T0866", false},
	KindAuthBruteForce:              {CatAuth, Alert, "T0806", false},
	KindAuthMasterChallengeFailed:   {CatAuth, Alert, "", false},
	KindAuthMasterErrorReceived:     {CatAuth, Warning, "", false},
	KindAuthMasterKeyChange:         {CatAuth, Notice, "", false},
	KindAuthFileFailed:              {CatAuth, Warning, "T0806", false},
	KindSeqFCBRepeat:                {CatSequence, Notice, "", false},
	KindSeqTestLinkNack:             {CatSequence, Notice, "", false},
	KindSeqRepeatRequest:            {CatSequence, Info, "", false},
	KindSeqReusedSeq:                {CatSequence, Notice, "T0830", false},
	KindSeqResponseStale:            {CatSequence, Notice, "", false},
	KindSeqResponseRepeat:           {CatSequence, Info, "", false},
	KindSeqUnsolDuplicate:           {CatSequence, Info, "", false},
	KindCmdDuplicate:                {CatCommand, Warning, "T0855", false},
	KindCmdReceived:                 {CatCommand, Notice, "", false},
	KindCmdRejected:                 {CatCommand, Notice, "", false},
	KindCmdBruteForce:               {CatCommand, Alert, "T0806", false},
	KindCmdOperateWithoutSelect:     {CatCommand, Warning, "", false},
	KindCmdSelectTimeout:            {CatCommand, Notice, "", false},
	KindCmdOperateSeqGap:            {CatCommand, Warning, "T0830", false},
	KindCmdOperateMismatch:          {CatCommand, Alert, "T0830", false},
	KindCmdSelectAbandoned:          {CatCommand, Info, "", false},
	KindCmdNoAckOperate:             {CatCommand, Notice, "", false},
	KindCmdLocalMode:                {CatCommand, Info, "", false},
	KindCtrlColdRestart:             {CatControl, Alert, "T0816", true},
	KindCtrlWarmRestart:             {CatControl, Alert, "T0816", true},
	KindCtrlAlreadyExecuting:        {CatControl, Notice, "", false},
	KindCtrlStopApplication:         {CatControl, Alert, "T0858", true},
	KindCtrlStartApplication:        {CatControl, Warning, "T0858", false},
	KindCtrlInitializeApplication:   {CatControl, Warning, "T0858", false},
	KindCtrlInitializeData:          {CatControl, Warning, "", false},
	KindCtrlSaveConfig:              {CatControl, Alert, "T0843", false},
	KindCtrlActivateConfig:          {CatControl, Alert, "T0843", false},
	KindCtrlTimeWrite:               {CatControl, Notice, "", false},
	KindCtrlClockStep:               {CatControl, Alert, "", false},
	KindCtrlRecordCurrentTime:       {CatControl, Info, "", false},
	KindCtrlDelayMeasure:            {CatControl, Info, "", false},
	KindCtrlUnsolDisable:            {CatControl, Warning, "T0804", false},
	KindCtrlUnsolEnable:             {CatControl, Info, "", false},
	KindCtrlAssignClass:             {CatControl, Warning, "T0878", false},
	KindCtrlDeadbandWrite:           {CatControl, Warning, "T0836", false},
	KindCtrlFreeze:                  {CatControl, Info, "", false},
	KindCtrlFreezeClear:             {CatControl, Warning, "", false},
	KindCtrlFreezeAtTime:            {CatControl, Info, "", false},
	KindCtrlRestartIINCleared:       {CatControl, Info, "", false},
	KindCtrlAttributeWrite:          {CatControl, Warning, "T0836", false},
	KindCtrlTerminalWrite:           {CatControl, Alert, "T0807", false},
	KindCtrlDatasetWrite:            {CatControl, Notice, "", false},
	KindCtrlTimeIntervalWrite:       {CatControl, Notice, "", false},
	KindCtrlIndicationChange:        {CatControl, Notice, "", false},
	KindRemoteRestart:               {CatRemote, Notice, "T0816", false},
	KindRemoteConfigCorrupt:         {CatRemote, Warning, "", false},
	KindRemoteDeviceTrouble:         {CatRemote, Notice, "", false},
	KindRemoteLocalControl:          {CatRemote, Info, "", false},
	KindRemoteEventOverflow:         {CatRemote, Warning, "T0815", false},
	KindRemoteRejected:              {CatRemote, Notice, "", false},
	KindUnsolUnexpectedConfirm:      {CatUnsolicited, Notice, "", false},
	KindUnsolResponseWithoutRequest: {CatUnsolicited, Warning, "T0856", false},
	KindUnsolWhileDisabled:          {CatUnsolicited, Warning, "T0856", false},
	KindFileDirectoryList:           {CatFileTransfer, Info, "T0846", false},
	KindFileReadOpened:              {CatFileTransfer, Notice, "T0845", false},
	KindFileWriteOpened:             {CatFileTransfer, Alert, "T0843", true},
	KindFileDelete:                  {CatFileTransfer, Alert, "T0809", true},
	KindFileRejected:                {CatFileTransfer, Warning, "", false},
	KindFileSuspiciousName:          {CatFileTransfer, Alert, "T0866", false},
	KindFileInvalidHandle:           {CatFileTransfer, Warning, "", false},
	KindFileOversized:               {CatFileTransfer, Warning, "T0814", false},
	KindFileTimeout:                 {CatFileTransfer, Notice, "", false},
	KindFileAborted:                 {CatFileTransfer, Notice, "", false},
	KindFloodCommands:               {CatFlood, Alert, "T0806", false},
	KindFloodIntegrity:              {CatFlood, Warning, "T0814", false},
	KindFloodEventPolls:             {CatFlood, Warning, "T0814", false},
	KindFloodRestart:                {CatFlood, Alert, "T0816", false},
	KindFloodTimeWrite:              {CatFlood, Warning, "", false},
	KindFloodAppControl:             {CatFlood, Alert, "T0858", false},
	KindFloodLinkStatus:             {CatFlood, Warning, "T0814", false},
	KindFloodLinkReset:              {CatFlood, Warning, "T0814", false},
	KindFloodAuthChallenges:         {CatFlood, Warning, "T0814", false},
	KindFloodFileOps:                {CatFlood, Warning, "T0814", false},
	KindFloodFrames:                 {CatFlood, Warning, "T0814", false},
	KindFloodUnsolicited:            {CatFlood, Warning, "T0814", false},
	KindDoSRxBacklog:                {CatResource, Warning, "T0814", false},
	KindDoSWriteStall:               {CatResource, Warning, "T0814", false},
	KindDoSSlowFrame:                {CatResource, Warning, "T0814", false},
	KindDoSConfirmWithheld:          {CatResource, Warning, "T0804", false},
	KindDoSEventOverflow:            {CatResource, Warning, "T0815", false},
	KindDoSLinkRetriesExhausted:     {CatResource, Notice, "", false},
	KindDoSQueueDrop:                {CatResource, Warning, "", false},
	KindMetaEventsDropped:           {CatMeta, Warning, "", true},
	KindMetaSinkPanic:               {CatMeta, Critical, "", true},
}

// Kind is a stable dotted event code, such as "link.unexpected_master".
type Kind string

type kindInfo struct {
	cat       Category
	sev       Severity
	technique string
	// always marks a kind that bypasses aggregation: every occurrence is
	// delivered on its own.
	always bool
}

// Category returns the kind's category, or zero for an unknown kind.
func (k Kind) Category() Category { return kindTable[k].cat }

// DefaultSeverity returns the severity an event of this kind gets when the
// emitter does not set one.
func (k Kind) DefaultSeverity() Severity { return kindTable[k].sev }

// Technique returns the kind's MITRE ATT&CK for ICS hint, or "".
func (k Kind) Technique() string { return kindTable[k].technique }

// AlwaysDeliver reports whether every occurrence of the kind is delivered on
// its own rather than aggregated.
func (k Kind) AlwaysDeliver() bool { return kindTable[k].always }

// Known reports whether the kind is in the catalogue.
func (k Kind) Known() bool { _, ok := kindTable[k]; return ok }

// Kinds returns every kind in the catalogue, in no particular order.
func Kinds() []Kind {
	out := make([]Kind, 0, len(kindTable))
	for k := range kindTable {
		out = append(out, k)
	}
	return out
}
