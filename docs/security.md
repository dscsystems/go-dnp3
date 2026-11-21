# Security events

go-dnp3 can report what it sees as a protocol endpoint that matters for
security. This covers connections from where they should not come, TLS
refusals, a MAC address that changed, malformed or out-of-sequence frames,
controls and restarts, authentication failures and floods. Each report is a
typed event sent to a sink you supply, so it can be forwarded to a SIEM
without parsing log lines.

This guide covers turning reporting on, the event model, the policy settings
(some of which refuse traffic as well as report it), how to read MAC
addresses, and deployment advice. Section 10 is the full catalogue of event
kinds.

The library is not an intrusion detection system. It reports what one master
or one outstation observes. Correlating events across devices, baselining,
and deciding what is an incident are the job of whatever consumes the stream.

## 1. Turning it on

Reporting is off unless a session's `Config.Security` is set. Leaving it nil
costs one nil check per would-be event, and nothing else changes.

```go
import "github.com/dscsystems/go-dnp3/secevent"

sink := secevent.JSONLinesSink(os.Stdout)

sess, err := outstation.New(ch, outstation.Config{
    // ...
    Security: secevent.DefaultPolicy(sink),
}, db, handler)
```

The same field exists on `master.Config` and on `multidrop.Config`. The
session attaches the policy to its channel by itself. That lets a server
channel check new connections before any TLS handshake, and lets a client
channel report failed dials.

Each session runs its own dispatcher and closes it when `Run` returns. If a
process has many sessions and you want one stream from all of them, give them
a shared dispatcher and close it yourself:

```go
d := secevent.NewDispatcher(sink, secevent.DispatcherOptions{})
defer d.Close()

p := &secevent.Policy{Dispatcher: d}
// use p (or copies of it) in every session's Config.Security
```

`Policy.Valid` checks a policy for contradictions, for example
`RejectUnexpectedMaster` without `AllowedMasters`. The library refuses an invalid
policy: a session's `Run` returns an error wrapping `dnp3.ErrBadConfig`,
and a multidrop bus logs the problem and runs without reporting.

A session also offers:

- `ReportCyberEvent(e)` lets the application add its own events, such as a
  command handler refusing a control for a reason only it knows. The session
  fills in the connection and peer, and the call is safe from any goroutine.
- `PeerInfo()` describes the far end of the current connection.
- `SecurityStats()` returns counts by kind and category, plus delivered,
  aggregated and dropped totals.

## 2. Events

An `Event` has these parts:

- A stable `Kind`, such as `link.unexpected_master`. Kind strings are public
  API: they are never renamed, only added or deprecated.
- A `Category`, a `Severity` (Info, Notice, Warning, Alert, Critical) and a
  MITRE ATT&CK for ICS `Technique`. All three default from the catalogue, and
  a call site raises the severity where the context warrants it.
- An `Action`, saying what the library did:
  - `Observed`: only reported.
  - `Ignored`
  - `Rejected`: refused or closed.
  - `Throttled`: refused under a flood limit.
  - `Disconnected`
  - `Blocked`: dropped before it reached anything.
- Who and where: `Role` (`master`, `outstation` or `bus`), `Transport`, a
  `ConnID` that ties together the events of one connection, the `Peer`
  (address, MAC and its source, interface, TLS identity, DNP3 link address)
  and `Local`.
- Protocol context, where relevant: link addresses and function, transport
  sequence, application function and sequence, IIN, object group/variation,
  qualifier and index, command status and control code, SAv5 user, file name
  and status. `Fields` says which of these are set, since zero is a valid
  value for most of them.
- `Value` holds a short machine-readable detail, such as a sub-reason, a
  clock delta or a command description. `Detail` is a one-line English
  explanation, `Err` is the underlying error, and `Raw` holds up to
  `MaxRaw` octets (default 64) of the offending frame.
- `Count` and `FirstSeen`, for repeats that were aggregated.

The JSON form, from `JSONLinesSink` or `json.Marshal`, has stable snake_case
field names and leaves out context fields that are not set:

```json
{"time":"2026-11-21T10:02:11.402Z","kind":"cmd.operate_mismatch",
 "category":"command","severity":"alert","action":"rejected",
 "protocol":"dnp3","role":"outstation","transport":"tcp","conn_id":7,
 "peer":{"network":"tcp","remote_addr":"10.0.4.20:50122",
         "mac":"00:1b:21:0a:0b:0c","mac_source":"neighbor","on_link":true,
         "interface":"eth1","link_addr":1},
 "link_src":1,"link_dest":10,"app_func":"OPERATE","app_seq":5,
 "count":1,"detail":"OPERATE objects differ from the SELECT's",
 "technique":"T0830"}
```

MAC addresses are written as text. Secrets never are: no key material, no
passwords, and no full certificates (only the subject, issuer, serial, expiry
and fingerprint).

### Delivery

Delivery never blocks a protocol goroutine. Events go into a bounded queue
(`QueueSize`, default 1024) and a dispatcher goroutine hands them to the
sink:

- **Aggregation.** Within `AggregateWindow`, repeats of the same kind from
  the same connection with the same context are folded into one event with a
  `Count`. That bounds a CRC storm to one event per window.
- **Always delivered.** Kinds marked ★ in the catalogue are never
  aggregated: every occurrence arrives on its own. These include a restart,
  a MAC change, an authentication failure, a file write and a delete.
- **Overflow.** When the queue is full, events are dropped and counted. The
  next event delivered is preceded by `meta.events_dropped` with the count,
  so a drop is never silent.
- **Sink panics.** A sink that panics is recovered and reported as
  `meta.sink_panic`.
- **Synchronous delivery.** `SyncSink(s)` makes the dispatcher call `s` on
  the emitting goroutine instead. Use it for tests, or for a sink that only
  appends to memory.

### Sinks

| Sink | Use |
|------|-----|
| `JSONLinesSink(w)` | One JSON object per line, for a file, stdout or a socket. `Errors()` counts failed writes. |
| `SlogSink(logger)` | Through `log/slog`. Critical and Alert map to Error, Warning to Warn, and Notice and Info to Info. |
| `NewChannelSink(c)` | Onto a Go channel, never blocking. `Dropped()` counts events that did not fit. |
| `MultiSink(a, b, …)` | Every event to each sink. |
| `FilterSink(min, cats, s)` | Only events at or above a severity and, optionally, in the given categories. |
| `SinkFunc(f)` | Any function. |

A sink is called from one goroutine at a time. A slow sink delays later
events, not the protocol.

## 3. The policy

`secevent.Policy` holds everything. Its zero value reports with built-in
defaults. `DefaultPolicy(sink)` is the zero value with those defaults written
out, and the [API reference](api.md#policy) lists every field.

**By default a policy only reports.** The settings in the table below change
what goes on the wire, and every one of them is off until you set it. A
policy that sets any of them takes effect even without a sink.

| Setting | What it does |
|---------|--------------|
| `RejectUnlisted` (with `AllowedPeers`) | Closes connections from unlisted addresses at once, before TLS. |
| `RejectOverRate` | Closes connections over `AcceptRate` / `GlobalAcceptRate`. |
| `RejectBindingMismatch` (with `PeerBindings`) | Closes a connection whose MAC is not the pinned one. |
| `DisconnectOnMACChange` (with `MACRecheckInterval`) | Closes a connection whose peer's MAC changed while it was open. |
| `RejectUnexpectedMaster` (with `AllowedMasters`) | Drops link frames from other master addresses before the link layer acts on them. |
| `Functions` | Refuses application functions outright. See [Refusing functions](#refusing-functions). |
| `CommandGuard` | Refuses individual controls with a status of your choosing. |
| `RejectBroadcastMisuse` | Drops broadcasts carrying functions outside `BroadcastAllowed`. |
| `FloodAction: Throttled` / `Disconnected` | Refuses traffic beyond the flood limits, or closes the connection. |

### Connections and peers

- `AllowedPeers []netip.Prefix`: connections from elsewhere raise
  `conn.unexpected`.
- `AcceptRate` and `GlobalAcceptRate`: new-connection rates per IP and
  overall. The defaults are 10 per 10 s and 50 per 10 s, and a negative `N`
  turns a check off.
- `PeerBindings map[netip.Addr]net.HardwareAddr`: pins IPs to MACs.
- `DisableMACLookup`, `Resolver`, `MACCache`, `MACLookupTimeout` (default
  200 ms) and `MACRecheckInterval`: see section 4.
- `CertExpiryWarning`: how far ahead a certificate's expiry is reported.
  The default is 30 days.

### Link layer

- `LinkTopology`: whether other stations share the link. By default serial
  and multidrop count as shared, while TCP, TLS and UDP are point to point.
  On a shared link, frames for other stations are normal and line noise is
  Notice. On a point-to-point link both are suspicious.
- `AllowedMasters []uint16`: the master link addresses an outstation serves.
  When it is nil, which is the default, nothing is checked, as before.

### Refusing functions

`Functions *FunctionPolicy` refuses functions for every peer alike. This
complements SAv5's `Authorize` callback, which decides per authenticated
user. A refused function is answered NO_FUNC_CODE_SUPPORT, and a refused
control gets `DenyStatus` (default NOT_AUTHORIZED). The handler never runs,
and `access.function_blocked` is reported.

```go
Security: &secevent.Policy{
    Sink: sink,
    Functions: &secevent.FunctionPolicy{
        ReadOnly:       true, // no controls, freezes, restarts, config, file writes
        AllowTimeWrite: true, // but let the master set the clock
    },
},
```

The policy has one field per class: `DenyRestart`,
`DenyApplicationControl`, `DenyFileWrite`, `DenyFileDelete`,
`DenyAssignClass`, `DenyDeadbandWrite`, `DenyTerminal` and
`DenyUnsolDisable`. `Deny` takes raw function codes.

`CommandGuard` is asked about each control object before the handler sees
it, and gets the peer, function, group, index and control code. Any status
other than SUCCESS refuses that object.

### Broadcasts and scanning

- `BroadcastAllowed`: the function codes a broadcast may carry. The default,
  `DefaultBroadcastAllowed`, is time writes, RECORD_CURRENT_TIME, freezes and
  DELAY_MEASURE. READ and CONFIRM always pass. Anything else raises
  `access.broadcast_misuse`.
- `ScanThreshold` and `ScanWindow`: when this many distinct unknown
  functions, objects or points arrive from one connection within the window,
  `access.scan` is raised. The default is 20 in a minute.

### Commands, control functions and audit

- Audit events (`cmd.received`, `ctrl.*`, `auth.success`, `file.read_opened`
  and others) record state-changing operations that succeeded.
  `DisableAudit` turns them off. Restarts and clock steps are still
  reported.
- `DuplicateWindow`: how long a state-changing request is remembered for
  `cmd.duplicate`. The default is 10 minutes. It is not checked under SAv5.
- `ClockStepThreshold`: the clock change that makes a time write
  `ctrl.clock_step`. The default is 1 minute.

### Secure authentication

- `AuthFailureRate`: failures per connection before `auth.brute_force`.
  The default is 5 a minute.

### Floods

`Limits` sets one `Rate` per traffic class. Any rate left at zero uses
`DefaultLimits`, and a negative `N` turns that class off. The defaults are
set well above ordinary polling:

| Limit | Default | Counts |
|-------|---------|--------|
| `Commands` | 20 / 2 s | control objects |
| `Integrity` | 6 / min | class 0 reads |
| `EventPolls` | 20 / s | class 1–3 reads |
| `Restart` | 2 / 10 min | cold and warm restarts |
| `TimeWrite` | 6 / min | clock writes |
| `AppControl` | 2 / min | application and configuration functions |
| `LinkStatus` | 5 / s | REQUEST_LINK_STATUS (report only) |
| `LinkReset` | 5 / 10 s | RESET_LINK_STATES (report only) |
| `AuthChallenges` | 20 / s | SAv5 challenges (report only) |
| `FileOps` | 10 / s | file opens and deletes |
| `Frames` | 2000 / s | link frames of any kind |
| `FCBRepeats` | 5 / min | raises `seq.fcb_repeat` to Alert |
| `Repeats` | 5 / min | raises `seq.repeat_request` to Warning |
| `CommandRejects` | 10 / min | raises `cmd.brute_force` |

`FloodAction` decides what happens beyond a limit:

- `Observed` (the default) only reports.
- `Throttled` refuses the excess. A throttled control is answered
  `ThrottleStatus` (default TOO_MANY_OPS), and any other request is answered
  PARAMETER_ERROR. Neither is executed.
- `Disconnected` closes the connection.

Link status, link resets and authentication challenges are only ever
reported. Refusing them would break the link or SAv5 itself.

`ConfirmWithheldAfter` is the number of confirmations in a row that may time
out before `dos.confirm_withheld`. The default is 3.

### Files

- `MaxFileSize` reports a file opened for writing with a larger announced
  size. Zero, the default, does not check.

### Robustness settings

Several settings that guard against resource exhaustion live on the channel
and session configs rather than on the policy. They work with or without
reporting, and every one of them is off by default:

| Setting | Effect | Event |
|---------|--------|-------|
| `outstation.Config.IdleTimeout` | Closes a connection that sends no valid frame for this long | `conn.idle_timeout` |
| `outstation.Config.FrameAssemblyTimeout` | Closes a connection that leaves a link frame half sent | `dos.slow_frame` |
| `outstation.Config.WriteTimeout`, `master.Config.WriteTimeout` | Bounds each write, so a peer that stops reading cannot stall the session | `dos.write_stall` |
| `channel.ServerOptions{Busy: BusyReject}`, `TLSConfig.Busy` | Closes a second connection at once instead of leaving it in the backlog | `conn.rejected_busy` |
| `TLSConfig.HandshakeTimeout` | Bounds the TLS handshake | `tls.handshake_timeout` |
| `UDPConfig.PinPeer` | Drops datagrams from anyone but the peer | `conn.udp_foreign_source` |

## 4. MAC addresses

When a connection opens, the session looks the peer's MAC up in the
kernel's neighbour table, unless `DisableMACLookup` is set. The lookup is one
table read per connection, bounded by `MACLookupTimeout`, and it never delays
or refuses a connection unless `PeerBindings` says to. The
[`netpeer`](api.md#package-netpeer) package does the work and can be used on its own
with any `net.Conn`.

| Platform | Source |
|----------|--------|
| Linux | netlink `RTM_GETNEIGH`, falling back to `/proc/net/arp`. Gateway from `/proc/net/route`. |
| Windows | `GetIpNetTable2`. Gateway from `GetIpForwardTable2`. |
| macOS, FreeBSD, NetBSD, OpenBSD, DragonFly | The routing socket's link-layer entries, as `arp -a` and `ndp -a` read them. |
| Others | None: `peer.mac_unavailable` is never raised and pinning cannot be satisfied. |

Read MAC addresses with these limits in mind:

- **Only on-link peers have their own MAC.** For a peer behind a router, the
  table holds the router's MAC. The library decides on-link first and
  reports such a MAC as `mac_source: gateway`, never as the peer's. Replacing
  the router does not look like a spoofed peer.
- **The table can be empty.** A peer that has not been ARPed yet, a
  container with its own network namespace, or a seccomp profile that
  refuses netlink all leave the MAC unknown.
- **A MAC proves nothing on its own.** Anyone on the segment can claim one.
  A change is worth an alert because it is unusual on a static OT network.
  It is not authentication. Use TLS or SAv5 for that.
- **Your own data source.** Through `Resolver`, a switch's forwarding table
  or an asset inventory can supply MACs. A resolver that implements
  `netpeer.Authoritative` is asked about every peer, on-link or not.
- **History.** `MACCache` (default `netpeer.DefaultCache`, shared by the
  whole process) remembers each IP's MAC, DNP3 link address and certificate,
  so changes raise `peer.*` and `tls.cert_changed`. `Save` and `Load` let
  the history survive a restart.

## 5. Multidrop buses

A `multidrop.Bus` with `Config.Security` reports what concerns the line as a
whole, with role `bus` and transport `multidrop`:

- the line opening and closing (`conn.connected`, `conn.closed`)
- line noise (`link.crc_error`, `link.bad_length`, `link.garbage`, all at
  Notice)
- frames addressed to no station on the bus (`link.foreign_dest`)
- frames dropped because the parser or a station's queue was full
  (`dos.queue_drop`)

Each station's own session reports its own events through its own
`Config.Security`.

## 6. Deployment guidance

- **Use TLS** (IEC 62351-3 profile, `channel.TLSServer` /
  `channel.TLSClient`) on any network you do not fully control. The library
  always requires client certificates and refuses TLS below 1.2.
  `tls.cert_rejected`, `tls.no_client_cert` and `tls.plaintext_on_tls_port`
  are then among the most valuable events you will get.
- **Use SAv5** where controls matter and TLS is not available end to end,
  for example over serial or through protocol gateways. It stops replays,
  which `cmd.duplicate` can only report.
- **List your masters.** On an outstation, `AllowedPeers` plus
  `RejectUnlisted` refuses strangers before they cost a TLS handshake, and
  `AllowedMasters` catches a second master on a serial line.
- **Turn on the idle and write timeouts** on outstations reachable over TCP.
  With one connection slot, a silent client otherwise locks out the real
  master indefinitely. Consider `BusyReject` so the real master learns
  straight away that the slot is taken.
- **Firewall first.** Allowlists in the library run after the kernel has
  accepted the TCP connection. Drop unwanted traffic at a host or network
  firewall, and enable SYN cookies (`net.ipv4.tcp_syncookies=1` on Linux)
  against SYN floods, which never reach user space.
- **Make outstations read-only where you can.** A device that is only
  monitored should run with `FunctionPolicy{ReadOnly: true}`.
- **Start in report-only mode.** Run with the defaults for a while, look at
  what your own masters do, and only then turn on throttling or tighten the
  limits.

## 7. What the library cannot see

- Traffic it is not party to: other masters' sessions with other devices,
  and anything on the wire not addressed to this endpoint. Use a network
  sensor for those.
- Anything the kernel handles before the socket: SYN floods, port scans of
  closed ports, ARP spoofing as it happens. Only the resulting MAC change is
  visible, the next time the neighbour table is read.
- Whether a well-formed, authorised command is a good idea. A valid
  authenticated OPERATE that trips a breaker is reported as `cmd.received`,
  and only you can say whether it should have happened.
- Inside TLS, only the application layer. Inside SAv5, only what the
  outstation's checks report.

## 8. Tools

`dnp3-outstation` and `dnp3-master` take `-events` to report security events
as JSON lines. `-events json` writes them to stderr, and `-events FILE`
appends them to a file. `-events-min` filters by severity. The outstation
also takes policy flags that make it a hardened device to test a master
against: `-allow-peer`, `-reject-unlisted`, `-allowed-master`,
`-reject-unexpected-master`, `-read-only` and `-flood-action`, plus the
robustness flags `-idle-timeout`, `-write-timeout` and `-busy-reject`. See
[`cmd/README.md`](../cmd/README.md).

## 9. Stability

Event kinds, their categories and their JSON field names are public API. New
kinds may appear in minor releases. A kind's default severity may be tuned.
Kinds are never renamed or reused.

## 10. Event catalogue

Roles: O outstation, M master, B multidrop bus. ★ marks kinds that are
always delivered on their own, never aggregated. The ATT&CK column gives the
closest MITRE ATT&CK for ICS technique, as a hint for correlation rules.
Severities are defaults: a call site raises one where the context warrants it,
for example on a point-to-point link.

### 10.1 Connection (`conn.*`)

| Kind | Severity | ATT&CK | Roles | Trigger |
|------|----------|--------|-------|---------|
| `conn.accepted` | Info |  | O | A server channel accepted a connection. Carries the peer's address, MAC (when known) and TLS identity. |
| `conn.connected` | Info |  | M, B | A client channel connected to its peer — the master-side counterpart of `conn.accepted`. A multidrop bus reports its line opening the same way. |
| `conn.unexpected` | Warning | T0849 | O | The peer's IP is not in `AllowedPeers`. Rejected before any TLS handshake when `RejectUnlisted`, otherwise Observed. |
| `conn.unexpected_mac` ★ | Alert | T0849 | O, M | The IP is allowed, but its MAC is not the one pinned in `PeerBindings`. Rejected when `RejectBindingMismatch`. |
| `conn.rejected_busy` | Notice |  | O | A connection arrived while the server was serving another one, and `Busy: BusyReject` closed it. With the default `BusyBacklog` it stays in the listen backlog, so it is never seen or reported. |
| `conn.rate` | Alert | T0814 | O | New connections from one IP went over `AcceptRate` (default 10 per 10 s), or new connections from everywhere went over `GlobalAcceptRate` (default 50 per 10 s). A connection storm or a scan. Rejected when `RejectOverRate`. |
| `conn.no_data` | Notice | T0846 | O | A connection closed before one valid link frame arrived. A TCP connect scan or a banner grab. |
| `conn.idle_timeout` | Warning | T0814 | O | No valid frame arrived within the outstation's `IdleTimeout`. Someone is holding the only connection slot. Disconnected. |
| `conn.closed` | Info |  | O, M, B | The connection ended. Carries the reason and the duration. |
| `conn.keepalive_failed` | Notice |  | M | A link status probe got no answer. |
| `conn.dial_failed` | Notice |  | M | A client channel failed to connect. |
| `conn.accept_error` | Critical | T0814 | O | Accept failed for a reason other than shutdown, such as running out of file descriptors. |
| `conn.udp_foreign_source` | Warning | T0849 | O, M [udp] | A UDP datagram came from an address other than the peer's. Blocked when `UDPConfig.PinPeer` is set. |

### 10.2 TLS (`tls.*`)

| Kind | Severity | ATT&CK | Roles | Trigger |
|------|----------|--------|-------|---------|
| `tls.cert_rejected` ★ | Alert | T0849 | O, M | The peer's certificate failed verification: unknown authority, expired, or wrong name. Carries the unverified leaf's subject, issuer, serial, expiry and SHA-256 fingerprint. |
| `tls.no_client_cert` | Alert | T0849 | O, M | A client connected without sending a certificate. The TLS server always requires one. |
| `tls.local_cert_rejected` | Warning | T0830 | O, M | The peer refused our certificate (a `bad_certificate`, `unknown_ca` or `certificate_expired` alert). TLS 1.3 clients learn this only on their first read. |
| `tls.plaintext_on_tls_port` | Warning |  | O, M | The first octets on the TLS port were `05 64`: a DNP3 master talking plaintext to the TLS port. |
| `tls.non_tls_probe` | Notice | T0846 | O, M | Anything else that is not TLS arrived on the TLS port, such as an HTTP request, an SSH banner or a null probe. |
| `tls.handshake_failed` | Warning |  | O, M | The handshake failed for any other reason. |
| `tls.handshake_timeout` | Warning | T0814 | O, M | The handshake did not finish within `TLSConfig.HandshakeTimeout`. A slow handshake holds the only slot. |
| `tls.weak_params` | Notice |  | O, M | The negotiated version or cipher suite is weak. This only happens when the application has lowered the defaults. |
| `tls.cert_expiring` | Notice |  | O, M | A certificate, either the peer's or our own, expires within `CertExpiryWarning` (default 30 days). |
| `tls.cert_changed` | Notice | T0830 | O, M | The peer's IP presented a different certificate than last time. The history is kept in the MAC cache. |

### 10.3 Peer identity (`peer.*`)

| Kind | Severity | ATT&CK | Roles | Trigger |
|------|----------|--------|-------|---------|
| `peer.mac_changed` ★ | Alert | T0830 | O, M | The IP now answers from a different MAC than the cache remembers. This can be ARP spoofing, or a replaced device. |
| `peer.mac_changed_midsession` ★ | Critical | T0830 | O, M | The MAC changed while the connection was open. Only checked with `MACRecheckInterval`. Disconnected when `DisconnectOnMACChange`. |
| `peer.mac_pinning_violation` ★ | Critical | T0849 | O, M | The IP is pinned in `PeerBindings`, and either its MAC differs or no MAC could be found. |
| `peer.ip_changed` | Info |  | O, M | A known MAC appeared with a new IP. |
| `peer.mac_shared` | Notice | T0830 | O, M | One MAC is in use by several IPs: a router, a NAT, or a spoofer. |
| `peer.mac_unavailable` | Info |  | O, M | No MAC could be found for an on-link peer. |
| `peer.link_addr_changed` | Warning | T0849 | O, M | The same IP (and MAC) now uses a different DNP3 source address than before. Either masquerading or a reconfiguration. |

### 10.4 Link layer (`link.*`)

| Kind | Severity | ATT&CK | Roles | Trigger |
|------|----------|--------|-------|---------|
| `link.crc_error` | Notice |  | O, M, B | A frame's header or body CRC did not match. Notice on a shared link. Warning point to point, where TCP already checks the data, so a DNP3 CRC error means a broken or malicious sender. |
| `link.bad_length` | Notice |  | O, M, B | A frame's length octet was out of range. Notice on a shared link, Warning otherwise. |
| `link.garbage` | Notice |  | O, M, B | Octets were skipped while resynchronising to `05 64`. The count covers the octets of frames also reported as CRC or length errors. |
| `link.foreign_dest` | Notice |  | O, M, B | A frame was addressed to another station on a point-to-point link. Silent on a shared link. On a multidrop bus it means a frame for no station on the bus. |
| `link.wrong_dir` | Warning | T0849 | O, M | The DIR bit matches our own role: a second master's frame reached a master, or one outstation's frame reached another. Reflection or spoofing. |
| `link.invalid_source` | Warning |  | O, M | The source address is reserved or a broadcast address. |
| `link.unexpected_master` | Warning | T0849 | O | A primary frame came from a source not in `AllowedMasters`. Only checked when that list is set. Rejected when `RejectUnexpectedMaster`, before the link layer acts on the frame. |
| `link.unexpected_source` | Warning | T0830 | O, M | A reply frame came from a station other than the one we asked. |
| `link.invalid_control` | Warning |  | O, M | The FCV bit contradicts the function, the user data length is wrong, or a non-data link function was broadcast. |
| `link.not_reset` | Notice |  | O, M | Confirmed user data arrived before RESET_LINK_STATES. |
| `link.unsupported_function` | Warning |  | O, M | An unknown primary link function arrived. It is answered NOT_SUPPORTED. |
| `link.broadcast` | Notice |  | O, M | A frame was sent to a broadcast address (0xFFFD to 0xFFFF). Notice point to point, Info on a shared link. |
| `link.self_address` | Notice | T0846 | O, M | A frame was sent to the self address 0xFFFC (discovery). |

### 10.5 Transport function (`transport.*`)

| Kind | Severity | ATT&CK | Roles | Trigger |
|------|----------|--------|-------|---------|
| `transport.discard` | Notice |  | O, M | The transport layer discarded a segment. `Value` says why: `empty`, `no_fir`, `unexpected_fir` or `bad_sequence`. A `bad_sequence` is Warning point to point, because TCP does not reorder. |

### 10.6 Application protocol (`proto.*`)

| Kind | Severity | ATT&CK | Roles | Trigger |
|------|----------|--------|-------|---------|
| `proto.app_malformed` | Warning |  | O, M | An application fragment failed to parse: short, truncated, bad qualifier or range, unknown object, or too large. |
| `proto.incomplete_request` | Warning |  | O | A request arrived without both FIR and FIN. |
| `proto.confirm_with_objects` | Warning |  | O | A CONFIRM carried objects. |
| `proto.request_con_uns` | Warning |  | O | A request had CON or UNS set. |
| `proto.inconsistent_qualifier` | Warning |  | O | A prefix and range that do not go together. |
| `proto.no_objects` | Warning |  | O | A function that needs objects arrived with none. An empty control could otherwise look like a successful one. |
| `proto.wrong_direction` | Warning |  | O | A response function reached the outstation, or a request function reached the master. |
| `proto.invalid_response` | Warning | T0856 | M | The response's function and UNS bit disagree. |
| `proto.parameter_error` | Notice |  | O | The outstation answered IIN2.2 (parameter error): an index beyond the database, a bad time write, or a bad deadband or freeze. |
| `proto.field_invalid` | Warning |  | O | A well-formed object carries an invalid field: a reserved CROB operation type, a count of 0, a pulse with an on-time of 0, or a NaN/Inf analog output. Report only. |
| `proto.secure_auth_unsupported` | Notice | T0846 | O | SAv5 traffic arrived at an outstation without SAv5 configured. Either a capability probe or a misconfigured master. |

### 10.7 Access (`access.*`)

| Kind | Severity | ATT&CK | Roles | Trigger |
|------|----------|--------|-------|---------|
| `access.unsupported_function` | Warning | T0888 | O | A function code the outstation does not support (IIN2.0). |
| `access.unknown_object` | Warning | T0861 | O | A group or variation the outstation does not support (IIN2.1). |
| `access.unknown_point` | Warning | T0861 | O | A control was sent to an index the device does not have. |
| `access.scan` | Alert | T0861 | O | One connection hit `ScanThreshold` distinct unknown functions, objects or points within `ScanWindow` (default 20 in a minute). This is point and capability discovery. Reported at most once per window. |
| `access.function_blocked` | Warning | T0855 | O | The function was refused by `Functions` or `CommandGuard`. It is answered NO_FUNC_CODE_SUPPORT, or for a control with the policy's status, and the handler never runs. |
| `access.broadcast_misuse` | Alert | T0855 | O | A broadcast carried a function outside `BroadcastAllowed`. Rejected when `RejectBroadcastMisuse`. |
| `access.oversized_request` | Warning | T0814 | O | A fragment was larger than the receive buffer allows. |

### 10.8 Secure authentication (`auth.*`)

| Kind | Severity | ATT&CK | Roles | Trigger |
|------|----------|--------|-------|---------|
| `auth.challenge` | Info |  | O | A critical request was challenged. |
| `auth.success` | Info |  | O | A challenge reply verified, and the request ran. Carries the user and the function. Audit. |
| `auth.failure` ★ | Alert | T0855 | O | Authentication failed. `Value` gives the first check that failed: `bad_mac`, `wrong_challenge_sequence`, `reply_from_other_source`, `late_reply`, `no_session_keys` or `no_challenge`. |
| `auth.authorization_denied` | Alert | T0855 | O | The `Authorize` callback refused this user and function. |
| `auth.reply_timeout` | Warning |  | O | A challenge expired without an answer. |
| `auth.unknown_user` | Warning | T0806 | O | A key status or key change named a user that does not exist (user enumeration). |
| `auth.key_change` | Notice |  | O | Session keys were changed. |
| `auth.key_change_failed` | Alert | T0830 | O | A key change failed. `Value` says how: `stale_key_status`, `unwrap_failed` or `status_mismatch`. This means a wrong update key, or tampering. |
| `auth.key_expired` | Info |  | O | Session keys expired, either by time or by message count. |
| `auth.error_received` | Notice |  | O | An authentication error message (AUTH_REQUEST_NO_ACK) arrived. |
| `auth.broadcast_critical` | Warning |  | O | A critical request was broadcast. It cannot be challenged, so it is dropped. |
| `auth.object_smuggled` | Alert | T0866 | O | A g120 object arrived under a function that is not an authentication function. |
| `auth.brute_force` | Alert | T0806 | O | Authentication failures and failed key changes from one connection went over `AuthFailureRate` (default 5 a minute). |
| `auth.master_challenge_failed` | Alert |  | M | The master could not answer the outstation's challenge: it had no valid session keys, or a key change was refused. |
| `auth.master_error_received` | Warning |  | M | The outstation sent an authentication error (g120v7). Carries the error code. |
| `auth.master_key_change` | Notice |  | M | The master changed its session keys. |
| `auth.file_failed` | Warning | T0806 | O | AUTHENTICATE_FILE was refused, or a file was opened or deleted without a valid key. Carries the user name, never the password. |

### 10.9 Sequence numbers (`seq.*`)

| Kind | Severity | ATT&CK | Roles | Trigger |
|------|----------|--------|-------|---------|
| `seq.fcb_repeat` | Notice |  | O | Link FCB repeat on confirmed user data. Notice on a shared link, Warning point to point, Alert above `Limits.FCBRepeats`. |
| `seq.test_link_nack` | Notice |  | O | TEST_LINK_STATES arrived with the wrong FCB, or before a reset. |
| `seq.repeat_request` | Info |  | O | The same request, with the same SEQ, arrived again and was answered from the cache. This is normal after a lost response. Warning above `Limits.Repeats`. |
| `seq.reused_seq` | Notice | T0830 | O | The same source and SEQ as the last request, but different content. A master advances SEQ, so this suggests a second sender. |
| `seq.response_stale` | Notice |  | M | A response SEQ does not match the request in flight. |
| `seq.response_repeat` | Info |  | M | A response fragment was repeated. |
| `seq.unsol_duplicate` | Info |  | M | The same unsolicited SEQ arrived again. |

### 10.10 Commands (`cmd.*`)

| Kind | Severity | ATT&CK | Roles | Trigger |
|------|----------|--------|-------|---------|
| `cmd.duplicate` | Warning | T0855 | O | A state-changing request was seen again, octet for octet, on a different connection within `DuplicateWindow` (default 10 minutes). This means a captured request is being replayed. It is not checked under SAv5, and class assignment and unsolicited enable/disable are exempt. |
| `cmd.received` | Notice |  | O | One event per CROB or analog output object, carrying the index, control code, value and status. A request with more than 16 objects gets one event for the whole request. Audit. |
| `cmd.rejected` | Notice |  | O | A control was refused by the handler or by the library. `CommandStatus` holds the status. |
| `cmd.brute_force` | Alert | T0806 | O | Rejected controls from one connection went over `Limits.CommandRejects` (default 10 a minute). |
| `cmd.operate_without_select` | Warning |  | O | OPERATE arrived with no live selection (NO_SELECT). |
| `cmd.select_timeout` | Notice |  | O | OPERATE arrived after the selection had expired (TIMEOUT). |
| `cmd.operate_seq_gap` | Warning | T0830 | O | The OPERATE's SEQ is not the SELECT's plus one. A request came in between, possibly from another sender. |
| `cmd.operate_mismatch` | Alert | T0830 | O | The OPERATE's objects differ from the SELECT's. What was confirmed is not what is being operated. |
| `cmd.select_abandoned` | Info |  | O | A new SELECT replaced a selection that was never operated. |
| `cmd.no_ack_operate` | Notice |  | O | DIRECT_OPERATE_NR ran. It gets no response, so the master never sees the outcome. |
| `cmd.local_mode` | Info |  | O | The handler refused a control with LOCAL or NOT_AUTHORIZED. |

### 10.11 Control functions (`ctrl.*`)

| Kind | Severity | ATT&CK | Roles | Trigger |
|------|----------|--------|-------|---------|
| `ctrl.cold_restart` ★ | Alert | T0816 | O | COLD_RESTART was accepted. Warning when the device refuses it. Reported even with `DisableAudit`. |
| `ctrl.warm_restart` ★ | Alert | T0816 | O | WARM_RESTART was accepted. Warning when the device refuses it. Reported even with `DisableAudit`. |
| `ctrl.already_executing` | Notice |  | O | A restart arrived while one was already under way. |
| `ctrl.stop_application` ★ | Alert | T0858 | O | STOP_APPL. |
| `ctrl.start_application` | Warning | T0858 | O | START_APPL. |
| `ctrl.initialize_application` | Warning | T0858 | O | INITIALIZE_APPL. |
| `ctrl.initialize_data` | Warning |  | O | INITIALIZE_DATA. |
| `ctrl.save_config` | Alert | T0843 | O | SAVE_CONFIG. |
| `ctrl.activate_config` | Alert | T0843 | O | ACTIVATE_CONFIG, with the names. |
| `ctrl.time_write` | Notice |  | O | A g50v1 or g50v3 time write was accepted. |
| `ctrl.clock_step` | Alert |  | O | A time write moved the clock by more than `ClockStepThreshold` (default 1 minute). Reported even with `DisableAudit`. |
| `ctrl.record_current_time` | Info |  | O | RECORD_CURRENT_TIME. |
| `ctrl.delay_measure` | Info |  | O | DELAY_MEASURE. |
| `ctrl.unsol_disable` | Warning | T0804 | O | Unsolicited reporting was turned off for classes that had it on. This stops event reporting. |
| `ctrl.unsol_enable` | Info |  | O | Unsolicited reporting was turned on for classes that had it off. |
| `ctrl.assign_class` | Warning | T0878 | O | ASSIGN_CLASS. Moving points to class 0, so that they raise no events, is Alert. |
| `ctrl.deadband_write` | Warning | T0836 | O | A g34 deadband write, with the old and new values. A wider deadband suppresses analog events. |
| `ctrl.freeze` | Info |  | O | IMMED_FREEZE or IMMED_FREEZE_NR. |
| `ctrl.freeze_clear` | Warning |  | O | FREEZE_CLEAR or FREEZE_CLEAR_NR: counters frozen and then cleared. |
| `ctrl.freeze_at_time` | Info |  | O | FREEZE_AT_TIME or FREEZE_AT_TIME_NR. |
| `ctrl.restart_iin_cleared` | Info |  | O | A g80v1 write cleared DEVICE_RESTART. |
| `ctrl.attribute_write` | Warning | T0836 | O | A g0 device attribute write. |
| `ctrl.terminal_write` | Alert | T0807 | O | Virtual terminal output (g112) to the device. |
| `ctrl.dataset_write` | Notice |  | O | A data set write (g85–87). |
| `ctrl.time_interval_write` | Notice |  | O | A time-and-interval write (g50v4). |
| `ctrl.indication_change` | Notice |  | O | The application set LOCAL_CONTROL, DEVICE_TROUBLE or CONFIG_CORRUPT through `SetIndication`. |

### 10.12 Remote device state (`remote.*`)

| Kind | Severity | ATT&CK | Roles | Trigger |
|------|----------|--------|-------|---------|
| `remote.restart` | Notice | T0816 | M | The outstation reported DEVICE_RESTART (IIN1.7) after startup. Warning for a second restart within an hour. |
| `remote.config_corrupt` | Warning |  | M | The outstation began reporting CONFIG_CORRUPT. |
| `remote.device_trouble` | Notice |  | M | The outstation began reporting DEVICE_TROUBLE. |
| `remote.local_control` | Info |  | M | The outstation began reporting LOCAL_CONTROL. |
| `remote.event_overflow` | Warning | T0815 | M | The outstation began reporting event buffer overflow (IIN2.3): events have been lost. |
| `remote.rejected` | Notice |  | M | The outstation refused our request. This covers request-error IIN2 bits, a control echoed with a non-success status, or a refused file step. |

### 10.13 Unsolicited (`unsol.*`)

| Kind | Severity | ATT&CK | Roles | Trigger |
|------|----------|--------|-------|---------|
| `unsol.unexpected_confirm` | Notice |  | O | A CONFIRM arrived when none was awaited, or with the wrong SEQ. |
| `unsol.response_without_request` | Warning | T0856 | M | A solicited response arrived while nothing was in flight. |
| `unsol.while_disabled` | Warning | T0856 | M | An unsolicited response with objects arrived after the master had disabled every class. The null unsolicited at startup is exempt. |

### 10.14 File transfer (`file.*`)

| Kind | Severity | ATT&CK | Roles | Trigger |
|------|----------|--------|-------|---------|
| `file.directory_list` | Info | T0846 | O | A directory was opened for listing (reconnaissance). |
| `file.read_opened` | Notice | T0845 | O | A file was opened for reading, with its name and size. Audit. |
| `file.write_opened` ★ | Alert | T0843 | O | A file was opened for writing or appending. |
| `file.delete` ★ | Alert | T0809 | O | DELETE_FILE. |
| `file.rejected` | Warning |  | O | A file operation was refused, or a write was closed before it was complete. |
| `file.suspicious_name` | Alert | T0866 | O | The name contains `..`, a backslash, a NUL or a control character. This is reported even though the name is neutralised. A leading `/` is normal in DNP3, so it is not flagged. |
| `file.invalid_handle` | Warning |  | O | A close, abort or write used a handle that was never issued. Handles are never reused, so this is guessing. |
| `file.oversized` | Warning | T0814 | O | A file opened for writing announced a size over `MaxFileSize`. Report only. |
| `file.timeout` | Notice |  | O | A file transfer timed out. |
| `file.aborted` | Notice |  | O | The master aborted a file transfer. |

### 10.15 Floods (`flood.*`)

| Kind | Severity | ATT&CK | Roles | Trigger |
|------|----------|--------|-------|---------|
| `flood.commands` | Alert | T0806 | O | Control objects (SELECT, OPERATE, DIRECT_OPERATE, DIRECT_OPERATE_NR) went over `Limits.Commands` (default 20 per 2 s). Throttled controls are answered `ThrottleStatus`. |
| `flood.integrity` | Warning | T0814 | O | Class 0 reads went over `Limits.Integrity` (default 6 a minute). One request makes the outstation return its whole database. |
| `flood.event_polls` | Warning | T0814 | O | Class 1–3 reads went over `Limits.EventPolls` (default 20 a second). |
| `flood.restart` | Alert | T0816 | O | Restarts went over `Limits.Restart` (default 2 per 10 minutes). |
| `flood.time_write` | Warning |  | O | Time writes went over `Limits.TimeWrite` (default 6 a minute). |
| `flood.app_control` | Alert | T0858 | O | Application and configuration functions went over `Limits.AppControl` (default 2 a minute). |
| `flood.link_status` | Warning | T0814 | O | REQUEST_LINK_STATUS went over `Limits.LinkStatus` (default 5 a second). Report only. |
| `flood.link_reset` | Warning | T0814 | O | RESET_LINK_STATES went over `Limits.LinkReset` (default 5 per 10 s). Report only. |
| `flood.auth_challenges` | Warning | T0814 | O | Challenged critical requests went over `Limits.AuthChallenges` (default 20 a second). Each challenge costs a nonce and an HMAC. Report only. |
| `flood.file_ops` | Warning | T0814 | O | File opens and deletes went over `Limits.FileOps` (default 10 a second). |
| `flood.frames` | Warning | T0814 | O | Link frames of any kind went over `Limits.Frames` (default 2000 a second). Never throttled, but can be disconnected. |

### 10.16 Resources (`dos.*`)

| Kind | Severity | ATT&CK | Roles | Trigger |
|------|----------|--------|-------|---------|
| `dos.rx_backlog` | Warning | T0814 | O | The receive queue stayed full for a second or more: the peer is sending faster than the outstation can process. Re-arms once the queue is below half. |
| `dos.write_stall` | Warning | T0814 | O, M | A write ran past `WriteTimeout`: the peer stopped reading. Disconnected. |
| `dos.slow_frame` | Warning | T0814 | O | A partial link frame was not finished within `FrameAssemblyTimeout` (slowloris). Disconnected. |
| `dos.confirm_withheld` | Warning | T0804 | O | `ConfirmWithheldAfter` application or unsolicited confirmations in a row timed out (default 3). Events are sent again and again. |
| `dos.event_overflow` | Warning | T0815 | O | The outstation's event buffer overflowed. |
| `dos.link_retries_exhausted` | Notice |  | O | The link layer gave up after its retries. |
| `dos.queue_drop` | Warning |  | B | Frames were dropped on a multidrop bus, either because the parser was full or because a station's queue was. |

### 10.17 Reporting itself (`meta.*`)

| Kind | Severity | ATT&CK | Roles | Trigger |
|------|----------|--------|-------|---------|
| `meta.events_dropped` ★ | Warning |  | all | The dispatcher's queue was full and events were lost. Carries the count. |
| `meta.sink_panic` ★ | Critical |  | all | The sink panicked. The dispatcher recovered and carries on. |
