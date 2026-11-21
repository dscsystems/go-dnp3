# Plan — cyber-security awareness for go-dnp3

- **Date:** 2026-11-21
- **Base:** `1fcec1d` (main)
- **Derived from:** `PLAN-cybersecurity-events.md` (the go-iecp5 / IEC 60870-5-104 plan).
  The structure, event model, delivery rules and the answers to its open
  questions carry over. The catalogue, findings and code locations are new and
  are written against this repository.
- **Scope:** `channel`, `internal/link`, `internal/transport`, `internal/stack`,
  `outstation`, `master`, `multidrop`, `decoder`, `cmd/*`, `conformance`, `docs`
- **Status:** plan only. No code written yet.

## 0. Goals and non-goals

### Goals

1. **Peer identity in the API.** Every network channel (TCP, TLS, UDP) can
   report its peer's MAC address together with IP, port, TLS identity and
   connect time. Serial and multidrop channels report the port. The library
   notices when the MAC behind a known IP changes.
2. **A cyber-event channel.** One structured callback (a "sink") receives
   typed security events from every layer: channel (TCP/TLS/UDP/serial), link,
   transport, application, Secure Authentication, commands and file transfer.
   It works the same way in `master`, `outstation` and `multidrop`.
3. **Detection of the requested event classes** (catalogue in §4), mapped from
   the IEC 104 list onto DNP3:
   unexpected connections · rejected certificates · invalid link addresses,
   points and objects · oversized requests · unsupported function codes ·
   wrong or unexpected master/outstation addresses · rejected commands ·
   invalid frames, segments, fragments, qualifiers and functions · FCB,
   transport and application sequence errors and replay · restart, stop
   application, configuration, clock, deadband, class-assignment, file and
   other unusual functions · MAC change for an IP · unsolicited responses and
   confirms · command flooding · link-status / keep-alive storms · DoS and
   connection squatting · **SAv5 authentication failures** (new: this library
   implements Secure Authentication, the 104 library did not).
4. **Optional hardening hooks.** Allowlists (IP and link address), limits and
   function policies the application can turn on. Every enforcement is opt-in.
   The default stays "observe and report".
5. **Zero cost and zero behaviour change when unused.** With no sink and no
   policy, wire behaviour and performance stay as they are today. The only
   exceptions are the robustness fixes in §7, which are called out explicitly
   and are each opt-in or default-off where they would change wire behaviour.

### Non-goals

- Extending SAv5 (remote update-key management, aggressive mode, asymmetric
  keys). We *report* what the existing SAv5 code does (§4.7) and nothing more.
- Kernel-level DoS mitigation (SYN floods, half-open TCP). User space cannot
  see these. Document the limitation and recommend SYN cookies and a firewall.
- Being an IDS. The library reports what it observes as a protocol endpoint.
  Correlation across devices belongs to the consumer (SIEM, IEC 62351-7 NSM
  agent).
- Replacing `slog`. Sessions keep logging as today; the event channel is a
  separate, typed stream (§3.4 provides an `slog` sink).

### Differences from the IEC 104 plan that shape everything below

| Topic | IEC 104 (go-iecp5) | DNP3 (this repo) | Consequence |
|-------|-------------------|------------------|-------------|
| Who owns the socket | `cs104.Server` accepts and owns sessions | A `channel.Channel` produces an `io.ReadWriteCloser`; `outstation.Session.Run` / `master.Session.Run` consume it (`outstation/session.go:585`, `master/session.go:203`) | Connection-level detection (accept, TLS, allowlist, MAC) lives in `channel`; sessions attach their emitter to the channel (§3.8). |
| Concurrency | Many sessions per server | `channel.TCPServer`/`TLSServer` serve **one connection at a time** (`channel/channel.go:196-204`) | Session limits do not apply; **squatting the single slot** is the main DoS (F4, F5). |
| Peer addressing | CA/IOA known only to handlers | The outstation owns its `Database`, so the library knows exactly which points exist | No `AddressSpace` interface needed; unknown-point detection is exact (§4.5). |
| Replay defences | APCI N(S)/N(R), time-tagged commands | Link FCB, transport sequence, application SEQ, SBO sequence check, **SAv5 challenge/HMAC** | `seq.*` and `auth.*` replace the 104 APCI kinds; there is no time-tag age check on CROB/AO (they carry no time) — duplicate-content detection plus SAv5 are the replay story (§4.8). |
| Handler context | `asdu.Connect` passed to every handler | `CommandHandler` / `Application` get no connection argument (`outstation/command.go:72-76`) | Handlers report through the `Session` they already hold (the documented template stores `c.sess`), §3.5. |
| Logging | printf `clog`, off by default | `*slog.Logger` per session (`outstation/session.go:111`, `master/session.go:74`) | `SlogSink` replaces `ClogSink`. |

---

## 1. Findings from the current code that shape the design

| # | Location | Observation | Consequence for the plan |
|---|----------|-------------|--------------------------|
| F1 | `channel/channel.go:260-279` | Accept loop already retries with backoff on non-close errors (unlike 104's F1). The errors are swallowed: an `EMFILE` storm is invisible. | No fix needed. Emit `conn.accept_error` (Critical) from the retry branch (§4.1). |
| F2 | `channel/tls.go:656-684` | TLS handshake already explicit with a 10 s deadline. A failed handshake is closed and dropped with **no record**: wrong CA, expired cert, no client cert, plaintext probe all look the same (nothing). The handshake also runs inline in `Connect`, so each slow handshaker holds the only accept slot for `tlsHandshakeTimeout` (10 s, a constant). | Classify the error and emit `tls.*` (§4.2). Make the handshake timeout configurable (§6). |
| F3 | `channel/channel.go:196-204`, `tls.go:601-603` | One connection at a time. A second master's connection sits unanswered in the kernel backlog until the first drops (same as 104 F16). | `conn.rejected_busy` with accept-and-close, opt-in (§7.5). No "replace the current connection" option (Q12). |
| F4 | `outstation/session.go:625-737` | The outstation has **no idle timeout and no keep-alive** (only the master has `KeepAlive`, `master/session.go:66-72`). A TCP connect that sends nothing — or a partial link frame — holds the single slot **forever**. One `nc host 20000` locks every real master out. | Real DoS hole. Opt-in `IdleTimeout` / outstation keep-alive with `conn.idle_timeout` / `dos.slow_frame` (§7.3). |
| F5 | `internal/stack/stack.go:404-412`, `outstation/session.go:700-735` | `conn.Write` runs synchronously on the session goroutine with **no write deadline**. A peer advertising a zero TCP window blocks the session: no ticker, no confirm timeouts, no unsolicited, no `Update` draining. | Write deadline plus `dos.write_stall` (§7.2). |
| F6 | `channel/udp.go:158-169` | UDP learns the peer from the first datagram, then **accepts datagrams from any source** for ever after; replies keep going to the first peer. Any host that can reach the port can inject requests. | `conn.udp_foreign_source` event; opt-in `PinPeer` drops them (§7.6). |
| F7 | `internal/link/parser.go:9-19,105-167` | Parser already counts CRC errors, bad lengths, resyncs and discarded octets (`link.Stats`). Nobody turns them into signals. | `link.crc_error`, `link.bad_length`, `link.garbage`, aggregated, driven from the existing counters (§4.4). |
| F8 | `internal/stack/stack.go:305-363` | Four silent `continue`s: frame for another destination (305), DIR bit equal to our own role (314), reserved/invalid source (327), secondary frame from a station other than the one we are talking to (351). | `link.foreign_dest`, `link.wrong_dir`, `link.invalid_source`, `link.unexpected_source` (§4.4). Needs a stack callback (§3.6). |
| F9 | `internal/link/secondary.go:97-149` | Invalid FCV/length combinations, confirmed data before reset, FCB repeats and unsupported link functions are handled correctly but `SecResult.Discarded` is ignored by the stack (`stack.go:332`). | `link.invalid_control`, `link.not_reset`, `seq.fcb_repeat`, `link.unsupported_function` (§4.4, §4.8). |
| F10 | `internal/transport/transport.go:182-240` | Reassembler counts discards by reason (`DiscardNoFIR`, `UnexpectedFIR`, `BadSequence`, `Overflow`). Not surfaced. | `transport.discard` with sub-reason; `Overflow` → `access.oversized_request` (§4.5). |
| F11 | `outstation/session.go` (whole) | **The outstation never compares the request's link source with `Config.RemoteAddr`.** It answers whichever master addressed it (`stack.go:140-146` says so on purpose). Any station on the wire can act as master. | Opt-in `Policy.AllowedMasters`; `link.unexpected_master` only when it is set, plus `RejectUnexpectedMaster` (§4.4, §6.3). |
| F12 | `outstation/session.go:812-905` | The request validation ladder is good — malformed fragment (815), not FIR+FIN (827), CONFIRM with objects (843), CON/UNS on a request (848), inconsistent qualifier (859), no objects (901) — and each is logged and counted in `Stats`. No structured signal. | One `proto.*` kind per rung, with the sentinel error as sub-reason (§4.6). |
| F13 | `outstation/session.go:985-992`, `1020-1048` | Unknown function codes set `NO_FUNC_CODE_SUPPORT`; unknown objects `OBJECT_UNKNOWN`; everything else `PARAMETER_ERROR`. These IIN2 bits are the DNP3 analogue of 104's COT 44–47. | **Observe outbound IIN2** in `sendFragments` (`session.go:1709`): every request error the outstation reports becomes an event with no per-site code (§3.7). |
| F14 | `outstation/command.go:142-163, 305-317, 431-442` | SBO checks (no select, timeout, wrong SEQ, different object bytes), unknown point (`NOT_SUPPORTED`) and `TOO_MANY_OPS` are exact. `logCommands` already logs every outcome. | `cmd.*` from the outcome list, with distinct kinds for each SBO failure (§4.9). |
| F15 | `outstation/session.go:636-640` | `lastReqValid` is cleared on every new connection, so a request captured on the wire and **replayed over a fresh TCP connection executes again** (by design: the repeat cache protects against lost responses, not attackers). Without SAv5 nothing else stops it. | `cmd.duplicate`: content hash of state-changing requests across connections within a window (§4.8). Document that SAv5 is the real defence. |
| F16 | `outstation/security.go` (whole) | SAv5 failures already increment g121 statistics via `countSecurity(i)` at ~15 sites, and the IIN/statistics events go to the master. The *local* operator sees nothing. | `auth.*` events emitted next to each `countSecurity` call (§4.7). |
| F17 | `outstation/security.go:137,156-163,224-228` | Broadcast critical requests, SA requests when SA is not configured, and g120 objects under READ/WRITE are refused correctly but silently. | `auth.broadcast_critical`, `proto.secure_auth_unsupported` (capability probe), `auth.object_smuggled` (§4.7). |
| F18 | `master/session.go:613-637, 677-720, 778-795` | Master discards non-responses, wrong FUNC/UNS combinations, responses with nothing in flight, stale SEQ, repeated fragments and duplicate unsolicited — all at `Debug`. | `unsol.*` and `seq.*` master-side kinds (§4.8, §4.10). |
| F19 | `master/session.go` | The master tracks whether it enabled unsolicited reporting only implicitly through its startup tasks. An unsolicited response carrying **events for a class the master disabled**, or before enabling, is delivered without comment. | `unsol.while_disabled` (Warning, T0856 Spoof Reporting) using a small enabled-mask tracker (§5.4). |
| F20 | `outstation/file.go:168-176`, `414`, `613` | Filenames are cleaned against a root (traversal is neutralised), file auth keys are checked. Neither a traversal attempt nor a bad key leaves a trace. | `file.suspicious_name`, `file.auth_failed` (§4.11). |
| F21 | `multidrop/multidrop.go:460-480, 505-520` | The bus drops frames for no station (`FramesUnrouted`) and drops octets when the parser is full — both at `Debug`/`Warn` only. | `link.foreign_dest` on the bus, `dos.queue_drop` (§4.4, §4.12). |
| F22 | `outstation/eventbuffer.go:99-110` | Event buffer overflow drops the oldest event and sets IIN2.3. A master that never confirms (or an attacker forcing confirm timeouts) can drive this. | `dos.event_overflow` (Warning, T0815 Denial of View) and `dos.confirm_withheld` (§4.12). |

---

## 2. Architecture overview

```
           ┌────────────────────────────────────────────────────────────┐
           │                       application                          │
           │   sink := secevent.SinkFunc(func(e secevent.Event){…})     │
           └──────────────▲──────────────────────────────▲──────────────┘
                          │ async, bounded, never blocks  │ PeerInfo(), SecurityStats()
                 ┌────────┴──────────┐          ┌─────────┴────────┐
                 │ secevent.Dispatcher│         │ netpeer (MAC)    │
                 │ (queue, aggregate, │         │ resolver + cache │
                 │  drop accounting)  │         └─────────▲────────┘
                 └────────▲──────────┘                    │
        ┌─────────────────┼─────────────────────┬─────────┴────────┐
   internal/secmon detectors (token buckets, scan window, request/unsol trackers, dup window)
        │                 │                     │                  │
   channel (TCP/TLS/UDP)  internal/stack hooks  outstation.Session master.Session  multidrop.Bus
   accept, TLS, MAC       link + transport       app, SAv5, cmds,   app, SAv5,      routing,
                                                 files              unsol           queues
```

### New packages

| Package | Visibility | Contents |
|---------|-----------|----------|
| `secevent` | public | `Event`, `Kind`, `Category`, `Severity`, `Action`, `PeerInfo`, `Sink`/`SinkFunc`, `Dispatcher`, ready-made sinks, `Reporter` and `Report`, `Policy`/`Limits`, `Stats`. Imports only the root `dnp3` package (for `CommandStatus`, `FileStatus`, `ControlCode`) and the standard library, so `channel`, `master`, `outstation` and `multidrop` can all import it without cycles. |
| `netpeer` | public | Same as the 104 plan: `LookupMAC(ip)`, `Resolver`, OS backends (Linux netlink + `/proc/net/arp`, Windows `GetIpNetTable2`, Darwin/BSD via `golang.org/x/net/route`), `Cache`, `OnLink(ip)`. Usable on its own with any `net.Conn`. **Code can be shared with go-iecp5** — see Q9. |
| `internal/secmon` | internal | Token bucket, sliding-window distinct counter, request tracker, unsolicited-mask tracker, duplicate-content window, aggregator. |

Package names follow the 104 decision (Q1 there: split packages).

---

## 3. Event model and delivery

### 3.1 `secevent.Event`

```
Event {
    Time        time.Time
    Kind        Kind        // stable dotted code, e.g. "link.unexpected_master"
    Category    Category    // Connection | Peer | TLS | Link | Transport | Protocol | Sequence |
                            //   Access | Auth | Control | Command | Unsolicited | Flood |
                            //   Resource | FileTransfer | Meta
    Severity    Severity    // Info | Notice | Warning | Alert | Critical (RFC 5424 aligned)
    Action      Action      // Observed | Ignored | Rejected | Throttled | Disconnected | Blocked
    Protocol    string      // "dnp3"
    Role        string      // "master" | "outstation" | "bus"
    Transport   string      // "tcp" | "tls" | "udp" | "serial" | "pipe" | "multidrop"
    ConnID      uint64      // monotonically increasing per process; correlates events of one connection
    Peer        PeerInfo    // §3.2
    Local       string      // local address or serial port

    // Protocol context — zero when not applicable; Fields says which are set
    Fields      FieldMask
    LinkSrc, LinkDest uint16
    LinkFunc    uint8       // link function code
    FCB         bool
    TransportSeq uint8
    AppFunc     uint8       // application function code (name via secevent.FuncName)
    AppSeq      uint8
    ExpectedSeq uint8
    IIN         uint16      // IIN1|IIN2 of the response involved
    Group, Variation, Qualifier uint8
    Index       uint32      // point index (wide: as the request gave it)
    CommandStatus dnp3.CommandStatus
    ControlCode dnp3.ControlCode // CROB code byte
    User        uint16      // SAv5 user number
    FileStatus  dnp3.FileStatus
    FileName    string      // bounded length, already cleaned
    Value       string      // human-readable command value (bounded)

    Count       int         // >1 when aggregated (§3.3)
    FirstSeen   time.Time
    Detail      string      // one-line English explanation, no secrets
    Err         error       // underlying sentinel where one exists (errors.Is-able)
    Raw         []byte      // copy of the offending link frame or fragment, truncated to MaxRaw (default 64 B)
    Technique   string      // optional MITRE ATT&CK for ICS hint (§3.9)
}
```

Rules (unchanged from the 104 plan): `Kind` values are public API and never
renamed; each Kind has a fixed default Category/Severity/Technique in
`kinds.go`; `Raw` is always a copy (the stack's buffers alias reassembly
memory, `stack.go:254-266`); `Detail` never contains key material — **in
particular never SAv5 session/update keys, challenge data or MACs, only their
lengths and sequence numbers**; `String()` and a stable `MarshalJSON`.

`Event.Decode()` (lazy, optional) renders `Raw` through the existing `decoder`
package for human-facing sinks. It is never called on the hot path.

### 3.2 `secevent.PeerInfo`

```
PeerInfo {
    Network     string           // "tcp", "udp", "serial", "pipe"
    RemoteAddr  netip.AddrPort   // zero for serial/pipe
    LocalAddr   netip.AddrPort
    MAC         net.HardwareAddr // nil when unknown
    MACSource   MACSource        // None | Neighbor | Gateway | Static | Custom
    OnLink      bool
    Interface   string
    TLS         *TLSPeer         // nil when plaintext
    ConnectedAt time.Time
    SerialPort  string
    LinkAddr    uint16           // last DNP3 source address seen from this peer (0 until known)
}
TLSPeer { Version, CipherSuite uint16; ServerName, Subject, Issuer, SerialHex string;
          NotAfter time.Time; FingerprintSHA256 string; Verified bool }
```

Accessors:

- `(*outstation.Session).PeerInfo() (secevent.PeerInfo, bool)`,
  `(*master.Session).PeerInfo() (secevent.PeerInfo, bool)` — false while not
  connected. A session serves one connection at a time, so "the" peer is
  well-defined, unlike a 104 server.
- `secevent.PeerOf(conn io.ReadWriteCloser) (PeerInfo, bool)`: type-asserts an
  optional `interface{ PeerInfo() secevent.PeerInfo }` implemented by the
  connections `channel` returns (wrapping `net.Conn`/`*tls.Conn`/`udpConn`/
  serial). Falls back to `net.Conn.RemoteAddr()`. **`channel.Channel` itself is
  not changed**, so external Channel implementations keep working (they simply
  get a best-effort PeerInfo).
- `multidrop` station channels report the bus's underlying peer and port.
- `PeerInfo` is computed once at connect time in `channel`, cached on the
  connection, updated under a mutex by MAC re-checks (§5.2). Accessors return a
  copy.

### 3.3 Delivery: `secevent.Dispatcher`

Identical to the 104 plan §3.3: atomic-pointer nil check as the zero-cost
path; bounded queue (1024) with one delivery goroutine; drops counted and
announced by `meta.events_dropped`; aggregation per `(ConnID, Kind, key)` with
key = link source / function / group-variation / index where relevant, window
1 s, LRU 4096; *always-deliver* kinds bypass aggregation (`peer.mac_changed`,
`tls.cert_rejected`, `ctrl.cold_restart`, `ctrl.warm_restart`,
`ctrl.stop_application`, `auth.failure`, `file.write_opened`, `file.delete`);
`Close()` flushes; panicking sinks recovered and reported as `meta.sink_panic`;
`SyncSink` for tests only.

Sessions are single-goroutine (`stack.go:8-14`), so the per-session emitter and
its detectors need no locks. Only the dispatcher queue is shared.

### 3.4 Ready-made sinks

| Sink | Purpose |
|------|---------|
| `SinkFunc(func(Event))` | adapter |
| `ChannelSink(chan<- Event)` | non-blocking send, counts drops |
| `SlogSink(*slog.Logger)` | Severity → slog level (Critical/Alert → Error, Warning → Warn, Notice/Info → Info), attrs = event fields, message prefix `cyber` |
| `JSONLinesSink(io.Writer)` | one JSON object per line (Q5 of the 104 plan: JSON is the only built-in format) |
| `MultiSink(...Sink)` | fan-out |
| `FilterSink(min Severity, cats []Category, s Sink)` | filtering |

### 3.5 Reporting from outside the library: `secevent.Reporter`

```
type Reporter interface { ReportCyberEvent(Event) }
```

`outstation.Session` and `master.Session` implement it, filling Peer, ConnID,
Role, Transport and Time. Handlers already hold their session (the SKILL.md
template's `commands{sess}`), so a `CommandHandler` that refuses a command for
a reason only it knows (interlock, local mode) can do
`c.sess.ReportCyberEvent(secevent.CommandRejected(index, status, "interlock"))`.
`ReportCyberEvent` is safe from any goroutine (it only enqueues).

Because the outstation knows its database, the 104 plan's `AddressSpace`
interface is **not needed**.

### 3.6 Stack hooks (`internal/stack`)

The link and transport layers are pure state machines with no clock and no
I/O (`stack.go:45-51`), which should stay true. Add one optional field:

```
stack.Config.Observe func(stack.Observation)   // nil = no cost
stack.Observation { Kind uint8; Frame link.Header; Reason uint8; Raw []byte }
```

called at the silent `continue`s (F8), on `SecResult.Discarded` (F9), on
reassembly discards (F10) and after `parser.Next` when parser counters moved
(F7: compare `Stats` before/after, no change to the parser). The session
translates observations into `secevent` kinds; `internal/stack` never imports
`secevent`. `multidrop` uses the same translation for its own parser and router.

### 3.7 Automatic detection from outbound responses

In `outstation.(*Session).sendFragments` (`outstation/session.go:1709`), when a
dispatcher is attached, inspect the request-error IIN bits
(`app.RequestErrorMask`, `internal/app/iin.go:65`) the response is about to
carry. They are cleared after every response, so they describe *this* request:

| Outbound IIN | Event |
|--------------|-------|
| IIN2.0 `NO_FUNC_CODE_SUPPORT` | `access.unsupported_function` |
| IIN2.1 `OBJECT_UNKNOWN` | `access.unknown_object` |
| IIN2.2 `PARAMETER_ERROR` | `proto.parameter_error` (sub-reason from the site that set it when known) |
| IIN2.4 `ALREADY_EXECUTING` | `ctrl.already_executing` (e.g. second restart, `session.go:1503`) |
| IIN2.3 `EVENT_BUFFER_OVERFLOW` (rising edge) | `dos.event_overflow` |

Command outcomes come from `executeCommands` directly (§4.9) and file statuses
from the file handlers (§4.11) rather than by re-parsing the response body.

Requests that take no reply (`*_NR`, broadcast) never reach `sendFragments`;
for those the same mapping runs at the point the IIN bit is set, guarded by
`frag.Header.Func.NoReply() || r.Broadcast`.

Master side, mirrored: a response whose IIN2 carries request-error bits for a
request we sent, or a command echo with a non-success status, becomes
`remote.rejected` (Notice). Unexpected IIN1 changes become `remote.*` (§4.10).

### 3.8 Wiring channels to sessions

Connection-level events originate inside `Channel.Connect`, before any session
code runs. Rather than give every channel constructor a new argument:

```
// in secevent
type Attachable interface { AttachSecurity(*Emitter, *Policy) }
```

`channel` server/client types, `multidrop.Bus` and the UDP channel implement
it. `outstation.Session.Run` / `master.Session.Run` call
`if a, ok := ch.(secevent.Attachable); ok { a.AttachSecurity(...) }` once,
so configuring the session is enough. A channel used by several sessions
(only `multidrop`) gets the policy of the bus (`multidrop.Config.Security`),
not of the sessions.

### 3.9 MITRE ATT&CK for ICS hints

Same caveat as the 104 plan: a routing hint, not attribution, and **the IDs
must be checked against the current matrix when implementing**. Starting map:
T0814 Denial of Service · T0855 Unauthorized Command Message · T0846 Remote
System Discovery · T0888 Remote System Information Discovery · T0861 Point &
Tag Identification · T0830 Adversary-in-the-Middle · T0836 Modify Parameter ·
T0816 Device Restart/Shutdown · T0843 Program Download · T0845 Program Upload ·
T0806 Brute Force I/O · T0849 Masquerading · T0856 Spoof Reporting Message ·
T0858 Change Operating Mode · T0815 Denial of View · T0878 Alarm Suppression ·
T0804 Block Reporting Message · T0809 Data Destruction · T0807 Command-Line
Interface · T0866 Exploitation of Remote Services.

---

## 4. Event catalogue

Columns: **Kind** · default **Sev** · trigger · where · roles (O = outstation,
M = master, B = multidrop bus; transports in brackets when restricted).

### 4.1 Connection (`conn.*`)

| Kind | Sev | Trigger | Where | Roles |
|------|-----|---------|-------|-------|
| `conn.accepted` | Info | Every accepted connection with full PeerInfo incl. MAC (104 Q3: Info) | `channel/channel.go:244`, `tls.go:662` | O [tcp, tls] |
| `conn.unexpected` | Warning | Peer IP not in `Policy.AllowedPeers`. `Rejected` (closed before the TLS handshake) when `RejectUnlisted`, else `Observed`. | accept path, before handshake | O |
| `conn.unexpected_mac` | Alert | IP allowed but MAC not the pinned `Policy.PeerBindings[ip]` | accept path after MAC lookup | O, M |
| `conn.rejected_busy` | Notice | A connection arrived while the single slot is in use (F3). Accepted and closed at once when `BusyPolicy=Reject` (§7.5). With the default `Backlog` (today's behaviour) it is not observable without accepting, so it is not reported. | `channel` server types | O |
| `conn.rate` | Alert | New connections from one IP exceed `AcceptRate` (default 10 / 10 s), or distinct-IP bursts exceed `GlobalAcceptRate`. Connection storm or scan. | accept path | O |
| `conn.no_data` | Notice | Connection closed without one valid link frame. TCP-connect scan or banner grab. | session end (link `FramesDecoded == 0`) | O |
| `conn.idle_timeout` | Warning | No valid request within `IdleTimeout` after connect, or no valid frame for `IdleTimeout` (F4). Slot squatting. Action `Disconnected`. | outstation ticker | O |
| `conn.closed` | Info | End of connection with reason, duration, frames, octets in/out, requests | `Run` after `serve` returns | O, M |
| `conn.keepalive_failed` | Notice | Link status request unanswered (master `checkKeepAlive`, `master/session.go:420`; outstation keep-alive from §7.3) | ticker | O, M |
| `conn.dial_failed` | Notice | Client dial failure; Warning after N consecutive failures | `channel.go:142`, `tls.go:574` | M (and O over client channels) |
| `conn.accept_error` | Critical | Accept error other than close (EMFILE, ENFILE, ENOBUFS…) — F1 | `channel.go:270` | O |
| `conn.udp_foreign_source` | Warning | UDP datagram from an address other than the configured/learned peer (F6). `Blocked` when `PinPeer`. | `channel/udp.go:158` | O, M [udp] |

### 4.2 TLS (`tls.*`)

Classification of the error from `HandshakeContext` at `channel/tls.go:674`
(server) and `tls.Dialer.DialContext` at `tls.go:574` (client) with
`errors.As`. Same kinds as the 104 plan:

| Kind | Sev | Trigger |
|------|-----|---------|
| `tls.cert_rejected` | Alert | `*tls.CertificateVerificationError` → reason from `x509.UnknownAuthorityError`, `CertificateInvalidError{Expired,…}`, `HostnameError`. Carries the unverified leaf's subject, issuer, serial, NotAfter, SHA-256. |
| `tls.no_client_cert` | Alert | Server requires client certs (always here: `tls.go:526`) and none was sent |
| `tls.local_cert_rejected` | Warning | Peer sent `bad_certificate`/`unknown_ca`/`certificate_expired` alert |
| `tls.plaintext_on_tls_port` | Warning | `tls.RecordHeaderError` whose first octets are `0x05 0x64` — a DNP3 master speaking plaintext to the TLS port |
| `tls.non_tls_probe` | Notice | `RecordHeaderError` with anything else (`GET `, `SSH-`, null probe) |
| `tls.handshake_failed` | Warning | Other handshake failures |
| `tls.handshake_timeout` | Warning | Deadline exceeded (slow-handshake DoS — and with one slot, a lock-out) |
| `tls.weak_params` | Notice | Negotiated version < 1.2 is already impossible (floor in `tls.go:512-515`); this fires for a user-lowered `MinVersion` or an insecure suite |
| `tls.cert_expiring` | Notice | Peer leaf `NotAfter` within `CertExpiryWarning` (30 days). Also checked for **our own** cert at construction. |
| `tls.cert_changed` | Notice | Peer IP presents a different leaf than last time (TOFU history in the MAC cache) |

`channel.TLSConfig` gains an optional `VerifyConnection` passthrough so users
who need extra checks keep them; the library's classification wraps it.

### 4.3 Peer identity (`peer.*`)

Unchanged from the 104 plan §4.3: `peer.mac_changed` (Alert, always
delivered), `peer.mac_changed_midsession` (Critical, opt-in re-check driven by
the session's existing 50 ms ticker), `peer.mac_pinning_violation`
(Critical), `peer.ip_changed` (Info), `peer.mac_shared` (Notice),
`peer.mac_unavailable` (Info). Added:

| Kind | Sev | Trigger |
|------|-----|---------|
| `peer.link_addr_changed` | Warning | Same IP (and MAC) now uses a different DNP3 source address than earlier connections. Masquerading (T0849) or reconfiguration. History kept in the same cache. |

### 4.4 Link layer and addressing (`link.*`)

| Kind | Sev | Trigger | Where |
|------|-----|---------|-------|
| `link.crc_error` | Notice serial / Warning tcp, tls, udp | Header or body CRC mismatch (parser counters). On TCP the transport already has a checksum, so a DNP3 CRC error means a broken or malicious sender, not noise. Aggregated. | `parser.go:130-133` via §3.6 |
| `link.bad_length` | Notice / Warning | Length octet out of range | `parser.go:134` |
| `link.garbage` | Notice / Warning | Octets discarded while resyncing to `0x0564` (count) | `parser.go:140-165` |
| `link.foreign_dest` | Notice | Frame for another destination on a point-to-point transport (TCP/TLS/UDP). Silent on serial unless `LinkTopology=PointToPoint`. On a multidrop bus: frame for no station (`multidrop.go:513`). | `stack.go:305`, `multidrop.go:505` |
| `link.wrong_dir` | Warning | DIR bit equals our own role: another master's primary frame at a master, or an outstation-to-outstation frame (reflection / spoofing) | `stack.go:314` |
| `link.invalid_source` | Warning | Source address is reserved or a broadcast address | `stack.go:327` |
| `link.unexpected_master` | Warning | Only when `Policy.AllowedMasters` is set (Q10): outstation receives a primary frame from a source not in it (F11). `Rejected` when `RejectUnexpectedMaster` (dropped before the secondary FSM, so it cannot reset FCB state either). With `AllowedMasters` nil nothing is checked or reported, as today. | `stack.go:331` via a `stack.Config.AcceptSource` predicate |
| `link.unexpected_source` | Warning | Secondary (reply) frame from a station other than the one in flight | `stack.go:351` |
| `link.invalid_control` | Warning | FCV contradicts the function, or user data length wrong | `secondary.go:97` |
| `link.not_reset` | Notice | Confirmed user data before RESET_LINK_STATES | `secondary.go:126` |
| `link.unsupported_function` | Warning | Unknown primary link function → NOT_SUPPORTED reply | `secondary.go:147` |
| `link.broadcast` | Notice on tcp/tls/udp, Info on serial | Frame to 0xFFFD/0xFFFE/0xFFFF. Broadcasts over a point-to-point TCP link are unusual. | `stack.go:394` |
| `link.self_address` | Notice | Frame to 0xFFFC (discovery, T0846). Accepted only with `SelfAddress`; otherwise also `link.foreign_dest`. | `stack.go:400` |
| `link.reset_storm` | see §4.12 | | |

### 4.5 Transport function (`transport.*`)

| Kind | Sev | Trigger |
|------|-----|---------|
| `transport.discard` | Notice | Reassembler discard with sub-reason `empty`, `no_fir`, `unexpected_fir`, `bad_sequence` (F10). `bad_sequence` on TCP is Warning (injection or a broken peer; TCP does not reorder). |
| `access.oversized_request` | Warning | `DiscardOverflow`: fragment beyond `MaxRxFragment`. Also: request carrying more command objects than `MaxControlsPerRequest` (`command.go:315`), file block size above negotiated, `READ` of a range wider than the database. |

### 4.6 Application validity (`proto.*`, `access.*`)

| Kind | Sev | Trigger | Where |
|------|-----|---------|-------|
| `proto.app_malformed` | Warning | `ParseFragment` failure. Sub-reason from sentinels: `ErrShortFragment`, `ErrTruncated`, `ErrBadQualifier`, `ErrBadRange`, `ErrUnknownObject`, `ErrFragmentTooLarge` (`internal/app/header.go:126-139`) | O `session.go:815`, M `master/session.go:597` |
| `proto.incomplete_request` | Warning | Request without FIR+FIN (multi-fragment or continuation) | `session.go:827` |
| `proto.confirm_with_objects` | Warning | CONFIRM carrying objects | `session.go:843` |
| `proto.request_con_uns` | Warning | Request with CON or UNS set | `session.go:848` |
| `proto.inconsistent_qualifier` | Warning | Prefix and range that do not go together | `session.go:859` |
| `proto.no_objects` | Warning | Function that requires objects sent with none — could otherwise read as a successful control | `session.go:901` |
| `proto.wrong_direction` | Warning | Outstation receives a response function (129/130/131) — `rejectMalformed` `hdr.IsResponse()` (`session.go:1022`); master receives a request function (`master/session.go:614`) |
| `proto.invalid_response` | Warning | Master: FUNC/UNS disagree (solicited claiming unsolicited or vice versa) | `master/session.go:631` |
| `proto.parameter_error` | Notice | Outbound IIN2.2 (§3.7) with sub-reason: index beyond database, bad time write, missing RECORD_CURRENT_TIME before g50v3, bad deadband, bad freeze | many `IINParameterError` sites |
| `proto.field_invalid` | Warning | Semantically invalid field in a well-formed object: CROB reserved control code / count 0 / pulse with on-time 0, analog output NaN/±Inf (§8), g50 time write with absurd value (year < 1990 or > now+1y) | command and write paths, before the handler |
| `proto.secure_auth_unsupported` | Notice | AUTH_REQUEST(_NO_ACK) or g120 received while SAv5 is not configured — capability probe or misconfigured master | `security.go:156` |
| `access.unsupported_function` | Warning | Outbound IIN2.0 or the `default` branch (`session.go:987`): non-existent function code | |
| `access.unknown_object` | Warning | Outbound IIN2.1: group/variation this device does not support | |
| `access.unknown_point` | Warning | READ, WRITE, command, freeze or class-assignment naming an index beyond the database (`controlExists`, `command.go:308`; read range checks `session.go:1180-1260`) | |
| `access.scan` | Alert | One connection hits ≥ `ScanThreshold` distinct unknown (group, variation, index) or unsupported functions within `ScanWindow` (default 20 in 60 s). Point/tag and capability discovery (T0861, T0846). Also fires on many distinct link destinations from one peer on a point-to-point link. | `internal/secmon` |
| `access.function_blocked` | Warning | Function denied by `Policy.Functions` (§6.3). Library answers `NO_FUNC_CODE_SUPPORT` (or the command status from `DenyStatus`) and the handler is never called. | dispatch, before `switch` at `session.go:910` |
| `access.broadcast_misuse` | Alert | State-changing function to a broadcast address outside `Policy.BroadcastAllowed` (default allowed: time write g50v1/v3, RECORD_CURRENT_TIME, IMMED_FREEZE(_NR), FREEZE_CLEAR(_NR), FREEZE_AT_TIME(_NR)). SELECT/OPERATE/DIRECT_OPERATE(_NR), restarts, application control, file and class-assignment functions to broadcast are reported. `Rejected` when `RejectBroadcastMisuse`. | `session.go:888` |

### 4.7 Secure Authentication (`auth.*`) — new versus the 104 plan

One event next to each `countSecurity` call and at the silent refusals. The
mapping of statistic index to meaning **must be checked against IEEE
1815-2012 §7 (security statistics table) during implementation**; the kinds
below are defined by the condition, not the index.

| Kind | Sev | Trigger | Where (outstation unless noted) |
|------|-----|---------|-------|
| `auth.challenge` | Info (aggregated) | Critical request challenged | `security.go:184` |
| `auth.success` | Info | Reply verified, request executed; carries user and function | `security.go:276` |
| `auth.failure` | Alert | Wrong MAC, wrong challenge sequence, reply from another source, late reply (T0855/T0830). Always delivered. | `security.go:262-264` |
| `auth.authorization_denied` | Alert | `Authorize` callback refused user/function | `security.go:270` |
| `auth.reply_timeout` | Warning | Challenge expired unanswered | `expireSecurity`, `security.go:360` |
| `auth.unknown_user` | Warning | Key status / key change for a user that does not exist (user enumeration) | `security.go:205, 221` |
| `auth.key_change` | Notice | Session keys changed | `security.go:252` |
| `auth.key_change_failed` | Alert | Unwrap or status-MAC failure on key change — wrong update key or tampering | `security.go:231, 239` |
| `auth.key_expired` | Info | Session keys expired by time or message budget | `security.go:363-366` |
| `auth.error_received` | Notice | AUTH_REQUEST_NO_ACK (error message) received | `security.go:180` |
| `auth.broadcast_critical` | Warning | Critical request to broadcast — cannot be challenged, so dropped | `security.go:137` |
| `auth.object_smuggled` | Alert | g120 object under a normal function (READ/WRITE) | `hasAuthenticationObject`, `session.go:868` |
| `auth.brute_force` | Alert | `auth.failure` + `auth.key_change_failed` from one connection exceed `AuthFailureRate` (default 5 / min). T0806. | `internal/secmon` |
| `auth.master_challenge_failed` | Alert | Master: outstation's challenge cannot be answered (no valid session keys) or a key change is refused | `master/security.go` |
| `auth.master_error_received` | Warning | Master: AUTH_RESPONSE carrying an error object (g120v7) — the outstation refused our authentication | `master/session.go:620` |
| `auth.master_key_change` | Notice | Master: session keys changed successfully | `master/security.go` |
| `auth.file_failed` | Warning | File-transfer credential check failed | `file_auth.go:60`, `file.go:414,613` |

### 4.8 Sequence and replay (`seq.*`, `cmd.duplicate`)

| Kind | Sev | Trigger |
|------|-----|---------|
| `seq.fcb_repeat` | Notice→Alert | Link FCB mismatch on confirmed user data (`secondary.go:131`). Notice per event on serial; Warning per event on TCP (TCP does not duplicate); Alert above `FCBRepeatRate`. |
| `seq.test_link_nack` | Notice | TEST_LINK_STATES with wrong FCB or before reset (`secondary.go:116-121`) |
| `seq.repeat_request` | Info→Warning | Identical request, same SEQ and source, answered from the cache (`session.go:877`). Normal after a lost response; Warning above `RepeatRate`. |
| `seq.reused_seq` | Notice | Same source and SEQ as the last request but different content (`isRepeatRequest` false only on content). Masters advance SEQ; reuse with new content suggests injection by a second sender. |
| `seq.response_stale` | Notice | Master: response SEQ does not match the request in flight (`master/session.go:711`) |
| `seq.response_repeat` | Info | Master: repeated response fragment (`master/session.go:717`) — normal when a confirm is lost; Warning above a rate |
| `seq.unsol_duplicate` | Info→Warning | Master: same unsolicited SEQ again (`master/session.go:786`) |
| `cmd.duplicate` | Warning | Byte-identical state-changing request (commands, restarts, writes, application control) seen again **on a different connection** within `DuplicateWindow` (F15). Replay of a captured request. The repeat cache cannot catch this because it resets per connection. Not raised when SAv5 is on — challenges make replay detectable as `auth.failure` instead. |

There is no 104-style `cmd.stale_timetag`: CROB (g12) and analog output
(g41) objects carry no time. Documentation must say plainly that without
SAv5 (or TLS) DNP3 commands have no cryptographic replay protection.

### 4.9 Commands (`cmd.*`) — audit trail and rejections

| Kind | Sev | Trigger |
|------|-----|---------|
| `cmd.received` | Notice | Every CROB / analog output object in SELECT, OPERATE, DIRECT_OPERATE, DIRECT_OPERATE_NR: index, control code (op type, trip/close, clear, queue), count, on/off time or value, operate type, status. Generated from `executeCommands` outcomes where `logCommands` runs (`command.go:206`, `239`). |
| `cmd.rejected` | Notice | Non-success status, with `CommandStatus` as sub-reason. Escalates to `cmd.brute_force` (Alert, T0806) above `RejectRate`. |
| `cmd.operate_without_select` | Warning | OPERATE with no live selection (`NO_SELECT`, `command.go:150`) |
| `cmd.select_timeout` | Notice | OPERATE after the select expired (`TIMEOUT`, `command.go:152`) |
| `cmd.operate_seq_gap` | Warning | OPERATE SEQ not SELECT+1 (`command.go:154`): a request in between — possibly another sender |
| `cmd.operate_mismatch` | Alert | OPERATE objects differ from the SELECT's (`command.go:159`): what was confirmed is not what is being operated (T0830) |
| `cmd.select_abandoned` | Info | Selection expired without an operate. Many of them from one peer → recon. |
| `cmd.no_ack_operate` | Notice | DIRECT_OPERATE_NR: executes with no response, invisible to the master's own confirmation path |
| `cmd.local_mode` | Info | Rejected with `LOCAL`/`NOT_AUTHORIZED` by the handler |
| `cmd.burst` | see §4.12 | |

`OperateCROB`/`OperateAnalog` handlers can add context through `Reporter`
(§3.5). An optional `secevent.CommandGuard` (§6.3) can veto a command before
the handler, e.g. a read-only policy.

### 4.10 Control and unusual functions (`ctrl.*`, `remote.*`)

Audit events (IEC 62351-7 style). All opt-out with `Policy.AuditControl=false`.

| Kind | Sev | Trigger |
|------|-----|---------|
| `ctrl.cold_restart` / `ctrl.warm_restart` | Alert | COLD/WARM_RESTART accepted (`session.go:1483`). T0816. Always delivered. Refused restarts (`RestartSupport` false) → Warning. |
| `ctrl.already_executing` | Notice | Second restart while one is under way (`session.go:1503`) |
| `ctrl.stop_application` | Alert | STOP_APPL (T0858) |
| `ctrl.start_application` / `ctrl.initialize_application` / `ctrl.initialize_data` | Warning | (`services.go:183`) |
| `ctrl.save_config` / `ctrl.activate_config` | Alert | Configuration persisted or activated, with the names for ACTIVATE_CONFIG (T0843) |
| `ctrl.time_write` | Notice | g50v1 or g50v3 write accepted, with old→new delta (`session.go:1316-1361`) |
| `ctrl.clock_step` | Alert | \|delta\| > `ClockStepThreshold` (default 60 s): defeats timestamp-based correlation |
| `ctrl.record_current_time` / `ctrl.delay_measure` | Info | |
| `ctrl.unsol_disable` | Warning | DISABLE_UNSOLICITED for classes that were enabled (T0815/T0804: stops event reporting) |
| `ctrl.unsol_enable` | Info | |
| `ctrl.assign_class` | Warning | ASSIGN_CLASS; moving points to class 0 (no events) is Alert (T0878 Alarm Suppression) |
| `ctrl.deadband_write` | Warning | g34 write (`session.go:1380`); a widened deadband suppresses analog events (T0836/T0878). Carries old→new. |
| `ctrl.freeze` / `ctrl.freeze_clear` / `ctrl.freeze_at_time` | Info / Warning / Info | Freeze-and-clear resets counters (Warning) |
| `ctrl.restart_iin_cleared` | Info | g80v1 write clearing DEVICE_RESTART |
| `ctrl.attribute_write` | Warning | g0 write accepted (device identity change) |
| `ctrl.terminal_write` | Alert | g112 virtual terminal output to the device (T0807) |
| `ctrl.dataset_write` / `ctrl.time_interval_write` | Notice | g85–87, g50v4 |
| `ctrl.indication_change` | Notice | Application changes LOCAL_CONTROL / DEVICE_TROUBLE / CONFIG_CORRUPT (`session.go:258`) |
| `remote.restart` | Notice | Master: outstation reports IIN1.7 DEVICE_RESTART outside the startup sequence (`master/session.go:658`). Repeated restarts within an hour → Warning. |
| `remote.config_corrupt` / `remote.device_trouble` / `remote.local_control` | Warning / Notice / Info | Master: rising edge of IIN2.5 / IIN1.6 / IIN1.5 |
| `remote.event_overflow` | Warning | Master: IIN2.3 — events lost at the outstation |
| `remote.rejected` | Notice | Master: request-error IIN2 bits or non-success command status for our request (§3.7) |

### 4.11 File transfer (`file.*`)

| Kind | Sev | Trigger |
|------|-----|---------|
| `file.directory_list` | Info | OPEN of a directory (`directoryContents`, `file.go:501`) — reconnaissance |
| `file.read_opened` | Notice | File opened for reading, name and size (T0845/T0882 upload/exfiltration) |
| `file.write_opened` | Alert | File opened for writing (T0843 download). Always delivered. |
| `file.delete` | Alert | DELETE_FILE (T0809). Always delivered. |
| `file.rejected` | Warning | Non-success `FileStatus` (not found, permission denied, fatal) |
| `file.suspicious_name` | Alert | `resolve()` changed the name (`file.go:168`): `..`, absolute path, backslashes, NUL — a traversal attempt even though it was neutralised |
| `file.invalid_handle` | Warning | Operation on a handle that was never issued or belongs to a closed transfer (handles are never reused, so this is guessing) |
| `file.oversized` | Warning | Announced write size > `MaxFileSize`, or block size above negotiated |
| `file.timeout` / `file.aborted` | Notice | (`checkFileTimeout`, `onAbortFile`) |

`auth.file_failed` is in §4.7. Master-side `file.*` mirror the
`master/file.go` outcomes as `remote.rejected` with the file status.

### 4.12 Flooding and resource (`flood.*`, `dos.*`)

Token buckets per connection per class, owned by the session goroutine.
Defaults chosen so normal SCADA traffic never trips them (validated in §9):

| Kind | Class / default threshold | Notes |
|------|---------------------------|-------|
| `flood.commands` | SELECT/OPERATE/DIRECT_OPERATE(_NR) objects > 10/s burst 20 | T0806 |
| `flood.integrity` | class 0 reads > 6/min burst 3 | amplification: one request, a full database |
| `flood.event_polls` | class 1/2/3 reads > 20/s | |
| `flood.restart` | restarts > 2 / 10 min | |
| `flood.time_write` | time writes > 6/min | |
| `flood.app_control` | application/config functions > 2/min | |
| `flood.link_status` | REQUEST_LINK_STATUS > 5/s **or** > 3 per configured keep-alive | the keep-alive storm |
| `flood.link_reset` | RESET_LINK_STATES > 5 / 10 s | repeatedly resetting FCB state |
| `flood.auth_challenges` | challenged critical requests > 20/s | each costs the outstation a 32-byte nonce and an HMAC: SAv5 CPU exhaustion |
| `flood.file_ops` | file opens/deletes > 10/s | |
| `flood.frames` | inbound frames > `MaxFrameRate` (2000/s) or octets > `MaxByteRate` | generic |
| `dos.rx_backlog` | `rx` channel (cap 8, `session.go:634`) full for > 1 s: the peer outpaces processing. Hysteresis. | |
| `dos.write_stall` | write deadline hit (F5). Action `Disconnected`. | |
| `dos.slow_frame` | a partial link frame not completed within `FrameAssemblyTimeout` (F4). Action `Disconnected` when the idle timeout is on. | slowloris |
| `dos.confirm_withheld` | ≥ N consecutive application or unsolicited confirm timeouts (`session.go:778-793`, `unsolicited.go:192`): events requeued over and over | T0804 |
| `dos.event_overflow` | event buffer overflow (rising edge, F22) | T0815 |
| `dos.link_retries_exhausted` | link layer gave up (`session.go:773`) | |
| `dos.queue_drop` | multidrop per-station queue overflow or parser-full drop (F21) | B |

Action per class, from `Policy.FloodAction`: `Report` (default),
`Throttle` (requests beyond the bucket are answered without executing —
commands with `CommandStatus` from `ThrottleStatus`, default `TOO_MANY_OPS`;
other functions with IIN2.2 — never silently dropped, so the master is not
left waiting), `Disconnect`. Link-status replies, confirms, link ACKs and
AUTH traffic are never throttled — report only.

---

## 5. Key mechanisms in detail

### 5.1 MAC lookup (`netpeer`)

As in the 104 plan §5.1 (on-link decision first; Linux netlink with
`/proc/net/arp` fallback; Windows `GetIpNetTable2`; Darwin/BSD via
`golang.org/x/net/route` per 104 Q4; `MACLookupTimeout` 200 ms; never blocks
or rejects unless pinning is configured). DNP3 specifics:

- Lookup runs in `channel` right after accept/dial, **before** the TLS
  handshake when pinning is configured (so a pinned-MAC violation costs no
  crypto), otherwise after it.
- UDP: lookup on the first datagram from a new source; re-lookup when the
  source changes (§4.1).
- Serial, pipe and multidrop over serial: `MACSource=None`.

### 5.2 MAC/IP history cache (`netpeer.Cache`)

As in the 104 plan, plus the DNP3 source link address per IP
(`peer.link_addr_changed`) and the TLS leaf fingerprint. Optional
persistence via `Save`/`Load`. Mid-session re-check driven by the existing
50 ms session ticker; no new goroutine.

### 5.3 Connection tracker (channel level)

Runs in `acceptContext`'s caller before handshake and session:

1. Allowlist (CIDR) → `conn.unexpected`.
2. Per-IP and global accept-rate buckets → `conn.rate`.
3. Busy policy (§7.5) → `conn.rejected_busy`.
4. MAC lookup and pinning → `peer.*`, `conn.unexpected_mac`.
5. TLS handshake → `tls.*`.

### 5.4 Master-side trackers

- **Unsolicited mask tracker.** Records the classes the master disabled and
  enabled (`DisableUnsolOnStartup`, `UnsolClassMask`, explicit
  `EnableUnsolicited`/`DisableUnsolicited` calls, including their success).
  An unsolicited response carrying class events for a disabled class →
  `unsol.while_disabled` (Warning, T0856). The null unsolicited after restart
  is exempt.
- **Request tracker.** The master already has exactly one request in flight
  (`s.inflight`), so "response without request" is `inflight == nil`
  (`master/session.go:679`) → `unsol.response_without_request` (Warning).
  No new structure needed.

### 5.5 Outstation-side unsolicited checks (`unsol.*`)

| Kind | Sev | Trigger |
|------|-----|---------|
| `unsol.unexpected_confirm` | Notice | CONFIRM for a response or unsolicited SEQ we are not awaiting (`session.go:1124`, `unsolicited.go:327`). Repeated → Warning: confirm injection can release events the master never got. |
| `unsol.response_without_request` | Warning | Master: §5.4 |
| `unsol.while_disabled` | Warning | Master: §5.4 |
| `unsol.unexpected_response_func` | Warning | = `proto.invalid_response` alias for SIEM rules |

### 5.6 Duplicate-content window

`internal/secmon.DupWindow`: SHA-256 of the fragment's object section plus
function code, for state-changing functions only, bounded LRU (default 1024
entries, `DuplicateWindow` default 10 min). Shared by all connections of one
session (the outstation is one session per channel, so this is per device).
Feeds `cmd.duplicate`.

---

## 6. Policy / configuration surface

### 6.1 Shared struct

```
secevent.Policy {
    // Delivery
    Sink               Sink
    Dispatcher         *Dispatcher      // share one across sessions (104 Q8: per endpoint unless shared)
    AggregateWindow    time.Duration
    MaxRaw             int
    AuditControl       bool             // §4.10, default true when a sink is set

    // Connection (applied by channel via Attachable, §3.8)
    AllowedPeers       []netip.Prefix
    RejectUnlisted     bool
    PeerBindings       map[netip.Addr]net.HardwareAddr
    RejectBindingMismatch bool
    AcceptRate, GlobalAcceptRate Rate
    // The P1 robustness settings are NOT here (implemented 2026-11-21): they
    // change behaviour without any sink, so they live with the thing they
    // configure — channel.ServerOptions.Busy / TLSConfig.Busy,
    // TLSConfig.HandshakeTimeout, UDPConfig.PinPeer,
    // outstation.Config.{IdleTimeout,FrameAssemblyTimeout,WriteTimeout},
    // master.Config.WriteTimeout. All default off (Q11).

    // Identity
    Resolver           netpeer.Resolver
    MACCache           *netpeer.Cache
    MACLookupTimeout, MACRecheckInterval, MACHistoryTTL time.Duration
    CertExpiryWarning  time.Duration

    // Link / application
    LinkTopology       LinkTopology     // PointToPoint (default on tcp/tls/udp) | SharedBus (default on serial)
    AllowedMasters     []uint16         // outstation; nil = no source check, no event (Q10)
    RejectUnexpectedMaster bool
    Functions          FunctionPolicy
    BroadcastAllowed   []BroadcastRule
    RejectBroadcastMisuse bool
    DuplicateWindow    time.Duration
    ClockStepThreshold time.Duration
    MaxFileSize        int64

    // Flooding
    Limits             Limits
    FloodAction        Action
    ThrottleStatus     dnp3.CommandStatus
    ScanThreshold      int; ScanWindow time.Duration
    FCBRepeatRate, RepeatRate, RejectRate, AuthFailureRate Rate
}
```

- `secevent.DefaultPolicy()` is report-only (104 Q2) with the §4.12 thresholds.
- `Policy.Valid()` returns `dnp3.ErrBadConfig`-wrapped errors, matching the
  repo's `ErrBadConfig` convention (`errors.go:25`).

### 6.2 Setters (additive)

- `outstation.Config.Security *secevent.Policy` and
  `master.Config.Security *secevent.Policy` — a pointer so nil means "off"
  and the zero-cost path is a nil check. Both sessions get
  `PeerInfo()`, `ReportCyberEvent(Event)`, `SecurityStats() secevent.Stats`.
- `multidrop.Config.Security *secevent.Policy` for bus-level events.
- `channel`: no new constructors. Channels pick the policy up through
  `Attachable` when a session runs them; a standalone
  `channel.AttachSecurity(ch, emitter, policy) bool` helper serves users who
  drive a channel themselves.
- Config structs are plain values here (not chained setters like go-iecp5),
  so this follows the repo style.

### 6.3 `FunctionPolicy` and `CommandGuard` (hardening)

```
FunctionPolicy {
    Deny            []uint8   // application function codes
    ReadOnly        bool      // deny WRITE (except g80 restart clear and time when AllowTimeWrite),
                              //   all controls, freezes, class assignment, restarts,
                              //   application/config functions and file writes/deletes
    AllowTimeWrite  bool
    DenyRestart, DenyApplicationControl, DenyFileWrite, DenyFileDelete,
    DenyAssignClass, DenyDeadbandWrite, DenyTerminal, DenyUnsolDisable bool
    DenyStatus      dnp3.CommandStatus // for commands; default NOT_AUTHORIZED
}
type CommandGuard func(peer PeerInfo, fc uint8, group uint8, index uint16, code dnp3.ControlCode) dnp3.CommandStatus
```

A denied function gets `NO_FUNC_CODE_SUPPORT` (or the command status for
controls) and `access.function_blocked`; the handler is never called. This
complements, not replaces, SAv5's `Authorize` callback: `Authorize` decides
per authenticated *user*, the guard per *peer*.

### 6.4 Stats

As in the 104 plan: monotonic counters per Kind and Category, dropped and
aggregated counts, rejected connections. The existing `outstation.Stats`,
`master.Stats`, `link.Stats`, `transport.Stats` and `multidrop.Stats` stay
as they are.

---

## 7. Robustness fixes found during analysis (behaviour changes, called out)

1. **Accept loop resilience.** Already done (`channel.go:260-279`). Only add
   the event. *No behaviour change.*
2. **Write deadline (F5).** When `conn` is a `net.Conn` and `WriteTimeout > 0`,
   set `SetWriteDeadline(now + WriteTimeout)` in `stack.write` via a small
   deadline-aware writer the session wraps around `conn`. On timeout:
   `dos.write_stall`, close. Default off; documented as recommended. Test: a
   client that never reads, with a tiny socket buffer.
3. **Outstation idle timeout and keep-alive (F4).** Opt-in `IdleTimeout`: if
   no valid link frame for that long, send a REQUEST_LINK_STATUS (the
   standard's keep-alive; **check the exact keep-alive requirement for
   outstations in IEEE 1815-2012's TCP/IP annex during implementation**);
   no answer within `LinkTimeout` → close with `conn.idle_timeout`.
   Separately, `FrameAssemblyTimeout`: a link frame started (`0x05 0x64`
   seen) but not completed in time → `dos.slow_frame`, close. Both default
   off; both strongly recommended for any outstation reachable over TCP,
   because today one silent connection locks every master out.
4. **TLS handshake timeout configurable (F2).** Replace the constant with
   `Policy.HandshakeTimeout` (default unchanged, 10 s).
5. **Busy-slot handling (F3).** `BusyPolicy`: `Backlog` (today, default) or
   `Reject` (accept and close at once with `conn.rejected_busy` — the second
   master learns immediately instead of hanging). Implementation: a
   background accept goroutine per server channel while a connection is
   active, only when `BusyPolicy=Reject`. A `Replace` mode (new connection
   takes over the current one) was considered and **dropped** (Q12): it is a
   takeover primitive, and the half-open-TCP case it would address is covered
   by the opt-in idle timeout (§7.3).
6. **UDP peer pinning (F6).** `PinUDPPeer`: drop datagrams not from the
   configured `RemoteAddr`, or from the first learned peer when none is
   configured. Default off (today's behaviour), with `conn.udp_foreign_source`
   reported either way when a sink is set.

Each is its own commit with a test, landed before the event work (P1).

---

## 8. Validation helpers (`objects` / `internal/app`)

- `objects.ValidateCROB(c dnp3.ControlRelayOutputBlock) error` — reserved
  op-type/trip-close combinations, count 0, pulse with on-time 0, queue/clear
  bits. Typed errors (`ErrReservedControlCode`, `ErrZeroCount`, …). **Check
  each rule against IEEE 1815-2012 g12v1 semantics before rejecting; at first
  these are report-only (`proto.field_invalid`).**
- `objects.ValidateAnalogOutput(v)` — NaN/±Inf for float variations.
- `secevent.FuncName(uint8)`, `secevent.IsStateChanging(uint8)` (reusing
  `outstation.criticalFunction`'s list — move it to `internal/app` so SAv5 and
  the policy share one table), `IsBroadcastAllowed`.
- All pure, table-driven, and added to the `FUZZERS` list in the `Makefile`.
- Existing sentinel errors stay unwrapped so `errors.Is` keeps working.

---

## 9. Performance and safety requirements

Same as the 104 plan §9, applied here:

- No policy → one nil-pointer check per frame; `stack.Config.Observe` nil →
  no calls. Benchmarks: a new `BenchmarkOutstationThroughput` over
  `channel.Pipe` with nil policy and with a no-op sink. Target < 2 %
  throughput loss with a sink, 0 allocations per frame on the happy path.
- Detectors live on the session goroutine — lock-free. Shared structures
  (MAC cache, dispatcher, duplicate window) use one mutex each and are bounded.
- Hostile input cannot grow any detector without limit (LRU caps everywhere).
- `Detail` strings built lazily.
- Thresholds validated against `cmd/dnp3-outstation/config-10k.yaml` load and
  the interop suites so they produce **zero** events under legitimate load.

---

## 10. Tests

| Layer | Tests |
|-------|-------|
| `secevent` | dispatcher non-blocking under a slow sink; drop accounting; aggregation; always-deliver kinds; sink panic recovery; JSON golden file; Kind table completeness |
| `netpeer` | as in the 104 plan (canned netlink, `/proc/net/arp`, Windows/Darwin buffers; cache TTL/LRU/persistence; fake Resolver) |
| `internal/secmon` | bucket math, distinct counter, dup window, unsol mask tracker |
| `internal/stack` | `Observe` fires once per silent drop with the right reason; nil `Observe` changes nothing (existing tests unchanged) |
| `channel` | allowlist reject before handshake; busy policies; accept-rate; UDP foreign source; TLS: unknown CA, expired, no client cert, plaintext `0x0564` on TLS port, HTTP probe, handshake timeout (certs generated in-test, as `channel/tls_test.go` already does) |
| `outstation` (style of `conformance/*_test.go`, hand-built frames) | one test per wire-triggerable Kind: no data; idle squatter; slow frame; write stall; CRC/length/garbage; foreign dest; wrong DIR; invalid source; unexpected master (observe and reject); invalid control; FCB repeat; transport discards; every `proto.*` rung; unknown function/object/point; scan; broadcast misuse; each SBO failure; command duplicate across two connections; restart/app control/time step/deadband/assign class/terminal audit; every `auth.*` (extend `conformance/security_attacks_test.go` to assert the event); each flood class; event overflow; confirm withheld |
| `master` | response without request, stale SEQ, invalid response, wrong direction, unsolicited while disabled, unsol duplicate, remote restart/config corrupt, SA failures |
| `multidrop` | frame for nobody, queue drop |
| file transfer | suspicious names, invalid handle, write/delete audit, auth failure (extend `conformance/file_test.go`, `file_auth_test.go`) |
| MAC end-to-end | Linux-only, skipped unless `DNP3_NETNS=1` and root: veth pair in a netns, MAC change → `peer.mac_changed` |
| Regression | `make check` (`go test -race ./...`); interop suites (`interop/`, `interop/otfabric/`) green with **policy nil and with `DefaultPolicy()`+sink**: proves no wire change |
| Fuzz | existing `FUZZERS` (`FuzzParser`, `FuzzReassembler`, `FuzzParseFragment`, …) plus a new end-to-end fuzz that feeds random octets into an outstation with a sink attached: never a panic, never a blocked goroutine |

---

## 11. Phased delivery (each phase = one PR, green CI)

| Phase | Content | Depends |
|-------|---------|---------|
| **P0** | Agree on open questions (§12); freeze Kind names (`kinds.go` + doc table) | — |
| **P1** | §7 robustness fixes: write deadline, outstation idle timeout / keep-alive / frame-assembly timeout, configurable TLS handshake timeout, busy policy, UDP pinning. Every one defaults to today's behaviour; called out in README. | — |
| **P2** | `secevent` core: Event, Kinds table, Dispatcher, sinks (`SlogSink`, `JSONLinesSink`, …), Reporter, Stats, Policy skeleton; `Config.Security` on outstation and master; `Attachable`; `conn.accepted/closed/no_data/accept_error/dial_failed/idle_timeout` | P1 |
| **P3** | `netpeer` Linux + cache; `PeerInfo` on connections and sessions; `peer.*`; pinning; mid-session re-check | P2 |
| **P4** | Channel policy: allowlist, rates, busy events, UDP foreign source; TLS classification and `tls.*` | P2 |
| **P5** | `stack.Config.Observe`; `link.*`, `transport.*`, `seq.fcb_repeat`, `link.unexpected_master` + `AllowedMasters` | P2 |
| **P6** | Outstation application layer: `proto.*`, outbound IIN observation, `access.*`, `ctrl.*`, `cmd.*` incl. SBO kinds and duplicate window, `FunctionPolicy`, `CommandGuard`, broadcast policy, validation helpers | P5 |
| **P7** | `auth.*`: outstation in full; master limited to the three `auth.master_*` kinds over today's SAv5 master code (Q13) | P6 |
| **P8** | Flood detectors and actions, `dos.*` | P6 |
| **P9** | Master: `unsol.*`, `seq.response_*`, `remote.*`, unsol mask tracker | P6 |
| **P10** | File transfer events | P6 |
| **P11** | `multidrop` bus events | P5 |
| **P12** | `netpeer` Windows and Darwin/BSD backends | P3 |
| **P13** | Docs: new `docs/security.md` (catalogue = §4 tables, policy guide, MAC caveats, deployment guidance: TLS per IEC 62351-3, SAv5, firewalling, SYN cookies, idle timeout, what the library cannot see); updates to `docs/api.md`, `docs/user-guide.md`, `docs/device-profile.md`, `SKILL.md`, `README.md`. Tools: `dnp3-outstation` and `dnp3-master` get `--events json|<file>` and policy flags; `dnp3-explorer` gets an events pane (optional). | all |

Rough size: P2–P8 are the bulk (~3–4 k lines including tests). Later phases
≤ 1 k each.

---

## 12. Open questions

### Inherited from the 104 plan (answers carried over unless you say otherwise)

1. Package names — **split packages** (`secevent`, `netpeer`, `internal/secmon`).
2. Defaults when a sink is set — **report only**.
3. `conn.accepted` severity — **Info**.
4. Darwin/BSD MAC backend — **new dependency** `golang.org/x/net/route`.
5. Output formats — **JSON** (plus `SlogSink`, since the repo already uses slog).
6. `ServerSpecial` — not applicable; the analogous question is §3.8
   (`Attachable` optional interface rather than changing `channel.Channel`).
   Proposed: optional interface.
7. Profile checks — the 104 "type not in profile" kind has no DNP3 equivalent
   beyond `access.unsupported_function`/`access.unknown_object`; proposed:
   **report-only Notice** for anything outside the device profile.
8. Dispatcher scope — **per session unless shared**.

### New for DNP3 (decided 2026-11-21)

9. **Share `netpeer` with go-iecp5?** Answer: **copy** it into this repo. The
   licences of the two repos must still be checked before copying code in
   either direction (this repo is GPLv3).
10. **`link.unexpected_master` default.** Answer: **report only when
    `AllowedMasters` is set.** With it nil, nothing is checked (§4.4).
11. **P1 defaults.** Answer: **off.** `IdleTimeout`, `FrameAssemblyTimeout`
    and `WriteTimeout` all default to 0; docs recommend values.
12. **`BusyPolicy=Replace`.** Answer: **no.** Only `Backlog` and `Reject`
    (§7.5).
13. **SAv5 events on the master side.** Decided: **in P7, small.** Three
    kinds (`auth.master_challenge_failed`, `auth.master_error_received`,
    `auth.master_key_change`) over the existing master SAv5 code; more only
    if master SAv5 support grows.
14. **Command audit volume.** Answer: **as proposed** — one `cmd.received`
    per object, aggregated into one event per request above 16 objects.

All questions are answered; P0 reduces to freezing the Kind names.

---

## 13. Implementation log

Branch `cyber-events`, one local commit per phase. Deviations from the text
above are recorded here rather than by rewriting it.

### P1 — done (`352dd73`)

- The robustness settings live on the channel and session configs, not on
  `secevent.Policy` (see §6.1 note): `channel.TCPServerWithOptions` +
  `ServerOptions{Busy}`, `TLSConfig.{Busy,HandshakeTimeout}`,
  `UDPConfig.PinPeer`, `outstation.Config.{IdleTimeout,FrameAssemblyTimeout,
  WriteTimeout}`, `master.Config.WriteTimeout`. All default off.
- New `internal/netio.Writer` (per-write deadline, passes UDP message
  bracketing through). Verified the write-timeout test hangs without the fix.

### P2 — done

- `secevent` package: kinds (all ~170 names of §4 frozen in `kinds.go`),
  `Event` + stable JSON, `PeerInfo`/`PeerOf`, `Dispatcher` (queue, drops,
  aggregation, panic recovery, `SyncSink`), `Emitter`, sinks (`SlogSink`,
  `JSONLinesSink`, `ChannelSink`, `MultiSink`, `FilterSink`), `Policy`.
- Differences from §3/§6: `DefaultPolicy(sink)` takes the sink;
  `Policy.DisableAudit` instead of `AuditControl` (zero value = audit on);
  `Attachable.AttachSecurity(*Emitter)` (no policy argument); new kind
  `conn.connected` (master side of `conn.accepted`); shared session plumbing
  in `internal/sechub`.
- Emitting: `conn.accepted/connected/closed/no_data/idle_timeout/
  keepalive_failed/accept_error/rejected_busy/dial_failed/udp_foreign_source`,
  `dos.write_stall`, `dos.slow_frame`. Sessions expose `PeerInfo()`,
  `ReportCyberEvent()`, `SecurityStats()`.

### P3 — done

- `netpeer`: Linux neighbour lookup through the standard library's
  `syscall.NetlinkRIB` (no new dependency), `/proc/net/arp` fallback, IPv4
  default gateway from `/proc/net/route`; on-link decision first; `Cache`
  with MAC / IP / shared-MAC / link-address / certificate change detection,
  TTL, LRU bound, `Save`/`Load`. Other platforms return `ErrUnsupported`
  until P12.
- Added `netpeer.Authoritative`: a resolver that knows MACs directly (switch
  table, inventory, tests) is asked about every address, reported as
  `SourceCustom`.
- Policy gained `DisableMACLookup` (lookup is on by default with a policy),
  `Resolver`, `MACCache`, `MACLookupTimeout`, `MACRecheckInterval`,
  `DisconnectOnMACChange`, `PeerBindings`, `RejectBindingMismatch`.
- MAC lookup and pinning run in the session when a connection opens (after
  any TLS handshake), not in the channel accept path as §5.1 proposed;
  moving pinning before the handshake is left for P4 with the allowlist.
  A refused connection makes the master wait 5 s before redialling.
- Mid-session re-check runs one goroutine per connection, only when
  `MACRecheckInterval` is set (§5.2 hoped to reuse the session ticker; the
  master has no ticker).
- Emitting: `peer.mac_changed`, `peer.mac_changed_midsession`,
  `peer.mac_pinning_violation`, `conn.unexpected_mac`, `peer.ip_changed`,
  `peer.mac_shared`, `peer.mac_unavailable`, `peer.link_addr_changed`,
  `tls.cert_changed`.

### P4 — done

- `internal/secmon`: token `Bucket` and bounded per-key `Keyed` buckets.
- Policy gained `AllowedPeers`, `RejectUnlisted`, `AcceptRate`,
  `GlobalAcceptRate` (defaults 10/10 s per IP, 50/10 s overall; negative N
  turns off), `RejectOverRate`, `CertExpiryWarning`, and type `Rate`.
- Server channels check the allowlist and rates right after accept, before
  any TLS handshake or session (`conn.unexpected`, `conn.rate`). The busy
  rejecter applies the same checks.
- `secevent.ClassifyTLS` maps handshake errors to `tls.cert_rejected`,
  `tls.no_client_cert`, `tls.local_cert_rejected`,
  `tls.plaintext_on_tls_port` (record header `05 64`), `tls.non_tls_probe`,
  `tls.handshake_timeout`, `tls.handshake_failed`. Used by the TLS server
  on handshake failure, by the TLS client on dial failure, and by sessions
  on a read error — TLS 1.3 clients only learn their certificate was refused
  on the first read.
- `tls.weak_params` after a handshake; `tls.cert_expiring` for our own
  certificate when a session attaches and for the peer's when a connection
  opens.
- Not done: `TLSConfig.VerifyConnection` passthrough (§4.2 last paragraph) —
  `TLSConfig` builds its own `tls.Config` and has no user callback to
  preserve; left out until someone asks for custom verification.
- Aggregation keys now include the peer address for events not tied to a
  connection, so refusals from different IPs are not folded together.

### P5 — done

- `link.SecResult` gained a `Note` (invalid control, not reset, FCB repeat,
  test-link NACK, unsupported function, broadcast of a non-data function,
  and the ordinary reset / link-status notes P8 will count).
- `stack.Stack.SetObserver(observe, admit)` instead of a `stack.Config`
  field: the policy is known only after the stack is built, and a nil
  observer keeps the cost at a nil check. Observations: parser deltas per
  `Receive`, foreign dest, wrong DIR, invalid source, unexpected reply
  source, secondary notes, transport discards, completed broadcast and
  self-address fragments.
- `admit(link.Header)` runs before the secondary FSM and reports the refusal
  itself; `sechub.Hub.Observe` installs it only for an outstation whose
  policy sets `AllowedMasters` (Q10).
- Policy gained `LinkTopology` (`TopologyAuto`/`PointToPoint`/`SharedBus`,
  `SharedLink(transport)`), `AllowedMasters`, `RejectUnexpectedMaster`
  (needs `AllowedMasters`; `Valid` says so), `MasterAllowed`. Emitter gained
  `Transport()`.
- Severity: CRC / length / garbage are Notice on a shared link, Warning
  otherwise; `transport.discard` `bad_sequence` is Warning point-to-point;
  `link.broadcast` is Info on a shared link. `link.foreign_dest` is silent
  on a shared link.
- `link.garbage` counts every octet the parser skipped, including those of
  frames also reported as CRC or length errors — the parser does not tell
  them apart.
- A broadcast of a non-data link function is reported as
  `link.invalid_control` (no own kind in the catalogue).

### P6 — split; P6a done

P6 is split in two commits. P6a (this one): `proto.*`, outbound IIN,
`access.*` other than `unknown_point`, `seq.repeat_request` /
`seq.reused_seq`, `FunctionPolicy` for non-command functions, broadcast
policy, scan detection. P6b: `cmd.*` and SBO, `CommandGuard`,
`FunctionPolicy` for controls (per-object status), `access.unknown_point`,
`ctrl.*` audit, `proto.field_invalid`, `cmd.duplicate`.

- The outstation tracks the request being handled (`secRequest`):
  `handle` begins and ends it, so request error IIN bits this request
  earned — whether its response carried them or, for an unanswered request,
  they wait for the next — become `access.unsupported_function`,
  `access.unknown_object`, `proto.parameter_error`,
  `ctrl.already_executing`. Bits pending from an earlier request and cache
  replays are not reported again. `dos.event_overflow` stays with P8.
- `proto.wrong_direction` for a response function at the outstation, both
  when its objects parse and when they do not.
- `secevent.FunctionPolicy` (`Blocks`, `BlocksWrite`, `CommandDenyStatus`)
  and `Policy.Functions`. `DenyFileWrite` is left for P10, which parses the
  open mode.
- Broadcast rules are function codes: `Policy.BroadcastAllowed []uint8`
  (nil → `DefaultBroadcastAllowed`: WRITE of g50/g80 only, RECORD_CURRENT_TIME,
  freezes, DELAY_MEASURE), `RejectBroadcastMisuse`. READ and CONFIRM always
  pass.
- `access.scan`: `Hub.Unknown(key)` counts distinct keys (function,
  function+object) per connection within `ScanWindow`; default 20 in a
  minute, `ScanThreshold < 0` off; at most one event per window; key memory
  bounded at 1024.
- Behaviour fix across P4–P6: a policy that refuses anything
  (`Reject*`, `DisconnectOnMACChange`, `Functions`) but has no sink now
  still opens an emitter, on a discarding sink, so the refusals happen.
  Before, `RejectUnlisted` without a sink silently did nothing.
- `seq.fcb_repeat` is Warning on a point-to-point link.

### P6b — done

- Commands: `commandOutcome` carries the object and who refused it
  (`refusal`: select fault, unknown point, too many, policy).
  `selection.matches` also returns a `selectFault`, giving
  `cmd.operate_without_select`, `cmd.select_timeout`,
  `cmd.operate_seq_gap`, `cmd.operate_mismatch` once per request.
  `cmd.select_abandoned` when a SELECT replaces a live selection.
  `cmd.no_ack_operate` for DIRECT_OPERATE_NR. `cmd.rejected` for a
  handler or library refusal; LOCAL, and NOT_AUTHORIZED from the handler,
  become `cmd.local_mode`. `access.unknown_point` (and a scan key) for an
  output the device does not have.
- `cmd.received` (audit): one per object with control code, status and a
  `Value` describing the command; above 16 objects one per request (Q14).
- `guardCommand` before the handler: `FunctionPolicy` for controls
  (`DenyStatus`, default NOT_AUTHORIZED) and `Policy.CommandGuard`, both
  reporting `access.function_blocked` with the status; and
  `proto.field_invalid` (report only) for a reserved operation type, count
  0, a pulse with on-time 0, or a NaN/Inf analog output.
- `cmd.duplicate`: `secmon.DupWindow` (SHA-256 of function + object
  octets, 1024 entries, `Policy.DuplicateWindow` default 10 min) in the
  hub, so it spans connections. Watches OPERATE / DIRECT_OPERATE(_NR),
  restarts, application and configuration functions, DELETE_FILE, and
  WRITE of anything but time and the restart indication. Deviation: class
  assignment and unsolicited enable/disable are left out — a master's
  startup sequence repeats them octet for octet after every reconnect.
  Skipped under SAv5.
- `ctrl.*`: `auditControl` runs from `endRequest` for a dispatched request,
  using the request error bits to tell accepted from refused. Time writes
  (`ctrl.time_write`, and `ctrl.clock_step` above
  `Policy.ClockStepThreshold`, default 1 min, not audit-gated) and deadband
  writes (old→new) are reported at their sites. Restarts are reported even
  with `DisableAudit` (always-deliver kinds); a refused restart is Warning.
  `SetIndication` reports `ctrl.indication_change`.
- `sechub.Hub` keeps the session emitter in an atomic too, so
  `ReportCyberEvent` / `SetIndication` from other goroutines do not race
  `Run` starting.
- Left for P8: `cmd.brute_force` (`RejectRate`).

### P7 — done

- Outstation: one event beside each refusal and statistic in
  `security.go`, with the user where known: `auth.challenge`,
  `auth.success` (audit), `auth.failure` with `Value` naming the first
  failed check (`bad_mac`, `wrong_challenge_sequence`,
  `reply_from_other_source`, `late_reply`, `no_session_keys`,
  `no_challenge`), `auth.authorization_denied`, `auth.reply_timeout`,
  `auth.unknown_user` (key status and key change), `auth.key_change`,
  `auth.key_change_failed` (`stale_key_status`, `unwrap_failed`,
  `status_mismatch`), `auth.key_expired` (time or message count),
  `auth.error_received`, `auth.broadcast_critical`,
  `auth.object_smuggled` (g120 under a non-critical function; the request
  is still challenged as before).
- `auth.brute_force`: `auth.failure` + `auth.key_change_failed` per
  connection over `Policy.AuthFailureRate` (default 5/min). Built on a new
  generic `Hub.Exceeded(key, Rate)` — per-connection token buckets that
  trip once per flood — which P8 reuses.
- Master: `auth.master_key_change`, `auth.master_challenge_failed`
  (unusable key status, key change not confirmed, challenge with no valid
  keys), `auth.master_error_received` (g120v7, with the error code).
- The statistic index ↔ meaning mapping was not changed; events are named
  by the condition at each site, as §4.7 asked.
- `auth.file_failed` is left for P10.

### P8 — done

- `secevent.Limits` (one `Rate` per flood class, plus `FCBRepeats`,
  `Repeats`, `CommandRejects`), `DefaultLimits`, `Policy.Limits`,
  `FloodAction` (Observed default / Throttled / Disconnected; `Valid`
  refuses others), `ThrottleStatus` (default TOO_MANY_OPS),
  `ConfirmWithheldAfter` (default 3). Integrity default is 6/min with a
  burst of 6 rather than "6/min burst 3", so a 10 s class 0 poll does not
  trip it.
- `sechub`: `limit` / `Exceeded` / `Over` / `Flood` over per-connection
  buckets (`Bucket.AllowN` added for frame counts); a flood is reported
  once until the rate is respected again. `Flood` returns the action;
  Disconnected sets a drop the session collects with `Hub.Drop()` after
  each read. Throttle or disconnect policies count as enforcing, so they
  work without a sink.
- Outstation: request classes (integrity, event polls, restart, time
  write, app control, file ops) are checked just before dispatch — a
  throttled request gets PARAMETER_ERROR and is not executed; controls are
  counted per object in `guardCommand` and refused with `ThrottleStatus`.
  Report-only classes: link status and link reset (from the stack's
  secondary notes), auth challenges, frames (from the serve loop's
  `FramesDecoded` delta; frames may still disconnect). `MaxByteRate` is not
  done.
- `seq.fcb_repeat` → Alert above `FCBRepeats`; `seq.repeat_request` →
  Warning above `Repeats`; `cmd.brute_force` above `CommandRejects`.
- `dos.rx_backlog` (receive queue full ≥ 1 s, re-armed below half),
  `dos.confirm_withheld` (application and unsolicited confirm timeouts in a
  row, reset by a good confirm), `dos.event_overflow` (rising edge),
  `dos.link_retries_exhausted`. `unsol.unexpected_confirm` for both confirm
  kinds came along here (§5.5).
- Dispatcher: the aggregation key now includes command status, control
  code, action and value, so an accepted control and the same control
  throttled, or failures for different reasons, are not folded together.
- Master-side `dos.*` not done; the master has no request flood to count.

### P9 — done

- Master `secMaster` state, reset per connection: unsolicited mask
  tracker, previous IIN, last completed response SEQ.
- `proto.app_malformed` (response), `proto.wrong_direction` (request
  function at the master), `proto.invalid_response` (FUNC/UNS disagree).
- `seq.response_stale`, `seq.response_repeat` (out-of-series repeat, and a
  repeat of the last response after it completed — which would otherwise
  read as unrequested), `seq.unsol_duplicate`.
- `unsol.response_without_request` when nothing is in flight and the SEQ
  is not the last completed one.
- `unsol.while_disabled`: the tracker records successful enable/disable
  tasks (startup and API) by class. Event objects do not carry their class,
  so the event fires only when every class 1–3 is disabled and the
  unsolicited response carries objects; the null unsolicited is exempt.
- `remote.restart` when `observeIIN` re-runs the startup sequence (now
  returns whether it did); Warning for a second within an hour.
  `remote.config_corrupt` / `device_trouble` / `local_control` /
  `event_overflow` on rising edges.
- `remote.rejected` for request-error IIN2 bits on a response to our
  request, and per command echo with a non-success status.

### P10 — done

- Outstation `secfile.go`: `file.read_opened` and `file.directory_list`
  (audit; `transfer.dir` marks a listing), `file.write_opened` (always),
  `file.delete` (always), `file.rejected` (open refused, close of an
  incomplete write, delete refused), `file.invalid_handle` (close, abort,
  write), `file.timeout`, `file.aborted`, `file.oversized` (announced size
  over `Policy.MaxFileSize`; report only).
- `file.suspicious_name`: deviation — a name with `..`, a backslash, NUL or
  control characters. A leading `/` is not flagged: DNP3 masters name files
  by absolute path as a matter of course.
- `auth.file_failed`: AUTHENTICATE_FILE refused (user name kept, password
  never), and opens or deletes without a valid key.
- `FunctionPolicy.DenyFileWrite` (and `ReadOnly`) refuse an open for write
  or append with PERMISSION_DENIED and `access.function_blocked`.
- Master: a file step the outstation refused is reported as
  `remote.rejected` with the file status and name, once per API call.

### P11 — done

- `multidrop.Config.Security *secevent.Policy`; an invalid policy is logged
  and ignored. The bus opens role `bus`, transport `multidrop`, and attaches
  the emitter to its channel; `Close` flushes it after the channel closes.
- Line events: `conn.connected` / `conn.closed` for the line itself;
  `link.crc_error`, `link.bad_length`, `link.garbage` at Notice (a bus is a
  shared medium); `link.foreign_dest` for a frame no station on the bus
  owns; `dos.queue_drop` when the parser is full or a station's queue is.
- Per-station events still come from each outstation's own `Security`.

### P12 — done

- Darwin and the BSDs (`neigh_bsd.go`): ARP/NDP entries from the routing
  socket through `golang.org/x/net/route` (`NET_RT_FLAGS` + `RTF_LLINFO`, as
  arp(8) does); the IPv4 default gateway from the route dump. New direct
  dependency `golang.org/x/net` (Q4); `golang.org/x/sys` became direct and
  moved to v0.50.0, which x/net requires; `go` directive is now `1.26.0`.
- Windows (`neigh_windows.go`): `GetIpNetTable2` through a lazy iphlpapi
  proc (x/sys does not wrap it), states Probe..Permanent counted as usable;
  default gateway from `GetIpForwardTable2`, lowest metric. The table is
  decoded by a portable `parseIPNetTable2` so it is tested on any host.
- Other platforms keep `ErrUnsupported`. Linux-only parser tests moved to
  `netpeer_linux_test.go`, so the package's tests build everywhere.
- Verified by cross-`go vet` and lint for windows/{amd64,386,arm64},
  darwin, freebsd, netbsd, openbsd and dragonfly; not run on those hosts.

### P13 — done

- New `docs/security.md`: turning reporting on, the event model and JSON,
  delivery (aggregation, always-delivered kinds, drops, sinks), a policy
  guide that separates report-only settings from the ones that refuse
  traffic, the flood limits, the P1 robustness settings, MAC caveats and
  per-platform sources, multidrop, deployment guidance, what the library
  cannot see, stability, and the catalogue of all 162 kinds. The severity,
  ATT&CK and always-delivered columns are generated from `kindTable`; the
  trigger text was checked against the emit sites, not copied from §4.
- `docs/api.md`: `secevent` and `netpeer` package sections; the new
  config fields (`Security`, timeouts, `TLSConfig.{HandshakeTimeout,Busy}`,
  `UDPConfig.PinPeer`, `TCPServerWithOptions`/`BusyPolicy`) and session
  methods. Short sections in `user-guide.md`, `device-profile.md`
  ("Security behaviour"), `README.md`, `SKILL.md`, and the tool READMEs.
- Tools: shared `cmd/internal/secflags`. Both tools take `-events
  json|FILE` and `-events-min`; one dispatcher serves every site of
  `dnp3-master`. `dnp3-outstation` also takes `-allow-peer`,
  `-reject-unlisted`, `-allowed-master`, `-reject-unexpected-master`,
  `-read-only`, `-flood-action`, `-idle-timeout`, `-write-timeout` and
  `-busy-reject`; `dnp3-master` takes `-write-timeout`. `-allow-peer` is
  outstation-only: the allowlist is checked by server channels.
- Not done: the optional `dnp3-explorer` events pane.

### P14 — follow-ups left from P4, P8 and P13

- Byte-rate limit (P8's `MaxByteRate`): `Limits.Octets` (default 1 MiB/s),
  counted per read beside frames by `Hub.Traffic(frames, octets)`, which
  replaces `Hub.Frames`. Both report `flood.frames`, with `Value` `frames` or
  `octets`; separate detectors, so one does not mask the other.
- Master-side floods (P8): the master's serve loop counts frames and
  octets, runs the receive-queue detector (`dos.rx_backlog`, moved from the
  outstation into `sechub.Hub.Backlog` and shared), and collects `Drop()`.
  New kind `flood.unsolicited` with `Limits.Unsolicited` (default 20/s). A
  master cannot refuse what it is sent, so Throttled only reports there;
  Disconnected drops the connection.
- TLS passthrough (P4): `channel.TLSConfig.VerifyConnection`, run after the
  standard checks on both ends. A refusal is wrapped in
  `secevent.VerifyError` and classified as `tls.cert_rejected` with `Value`
  `application`, carrying the leaf's identity.
- Explorer events pane (P13): a Security tab (key 6; Help moves to 7) fed
  by a report-only policy whose sink pushes onto the UI's message channel,
  dropping rather than blocking. Severity colouring, filter, clear, CSV
  export, and a count on the Overview. MAC lookup is off in the explorer:
  its reconnects to one device would only feed the process-wide history.

### P15 — review fixes

- Raw octets are never kept for credential-bearing fragments (file
  open/delete/authenticate, SAv5 requests/responses, g120, g70v2–4), even
  when malformed (`secevent/redact.go`).
- UDP peers learned from the first datagram are checked against
  `AllowedPeers`/`RejectUnlisted` and MAC pinning before they are learned
  (`SetPeerCheck`); refused senders are dropped.
- Multidrop: an invalid policy makes `Add` refuse every station (fail closed).
- Dispatcher: sinks are called without the state lock (a sink may call
  Stats); Submit/Close ordered by an RW lock so nothing accepted is lost.
- TLS client refusals keep the peer certificate and dialled address.
- Floods carry the action actually taken (`FloodMode`: Refusable,
  Unrefusable, ReportOnly).
- Explorer: events tagged with their device, stragglers dropped, rows reset
  on device change; tracked binary removed and ignored.
