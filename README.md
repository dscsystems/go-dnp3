# go-dnp3

A native Go implementation of DNP3 (IEEE Std 1815-2012), providing both master
and outstation roles.

> **Status: usable, API not yet stable.** A master and an outstation talk to
> each other end to end over TCP, TLS, UDP, serial or an in-process pipe, with
> or without link-layer confirmation. Three applications ship with it. What is
> and is not supported is set out honestly in the
> [device profile](docs/device-profile.md), including the gaps.

```go
m := master.New(master.Config{
    LocalAddr: 1, RemoteAddr: 10,
    IntegrityOnStartup: true, DisableUnsolOnStartup: true,
}, handler)

go m.Run(ctx, channel.TCPClient("10.0.0.5:20000", channel.DefaultRetry))

m.AddPeriodicScan(ctx, 5*time.Second, dnp3.Class123)
m.ScanRange(ctx, 30, 1, 0, 15)
m.SyncTime(ctx)

// Controls. SelectAndOperate gives the outstation a chance to refuse
// before anything in the substation moves.
res, err := m.SelectAndOperate(ctx, master.Trip(3, 1000))
res, err = m.DirectOperate(ctx, master.AnalogOutputFloat32(7, 13.75))
```

## Why

There is no comprehensive native Go DNP3 stack. [opendnp3][], the de-facto
reference implementation, is C++11 and has been archived since September 2022.
What exists in Go today is limited to banner-grab scanners and partial dissectors —
no session layer, no outstation, no event model.

[opendnp3]: https://github.com/dnp3/opendnp3

## Design

DNP3 is a four-layer stack, and each layer here is a package whose core takes
bytes and returns bytes: it performs no I/O, starts no goroutines, and reads no
clock. Sessions and channels supply those from the outside.

That constraint is the whole design. A layer that cannot touch a socket can be
fuzzed with a byte slice, unit-tested against golden hex, and driven by a
virtual clock — which is what a protocol facing untrusted field devices needs.

```
internal/link       framing, CRC-16/DNP, primary and secondary state machines
internal/transport  FIR/FIN/SEQ segmentation and reassembly
internal/app        application headers, function codes, IIN, qualifiers
internal/stack      link + transport plumbing shared by both roles
objects             group/variation codecs, generated from a spec table
master, outstation  session state machines built on the layers above
channel             TCP, TLS, UDP, serial and an in-process pipe
decoder             structured protocol traces for logging and tooling
```

Each session owns its protocol state in a single goroutine. The socket is read
by a separate goroutine that hands completed fragments across a channel, so no
protocol state is ever touched from two places. The request API is safe to call
from anywhere: it submits a task and waits, rather than reaching into the
session.

Every integration test runs a full master against a full outstation through
`channel.Pipe` — the real link, transport, application and object layers, with
no socket and no hardware. That is where the two halves are proven to agree.

The library depends on the standard library plus one package: `go.bug.st/serial`,
reached only from `channel/serial.go`. `gopkg.in/yaml.v3` is in `go.mod` for the
code generator in `internal/gen` and no importable package reaches it, which
`go list -deps` will confirm. The example applications and the terminal explorer
carry their own dependencies and are not imported by the library.

### Conformance

The [device profile](docs/device-profile.md) states what is supported, in the
shape a vendor ships with a device — including a **known gaps** section rather
than leaving them to be discovered. Nothing here is a certified conformance
claim.

### Interoperability

Verified in both directions against opendnp3, built
from source in container:

```
make interop-build   # clone and compile opendnp3 3.1.2
make interop         # our master against opendnp3 outstation
make interop-reverse # opendnp3 master against our outstation
```

`conformance/` drives an outstation with hand-built request fragments and checks
what comes back, modelled on the DNP Users Group's Level 2 procedures: unknown
function codes, unknown groups, broadcast handling, the restart handshake,
event confirmation, select-before-operate. It is not a substitute for certified
conformance testing, and nothing in this repository is a conformance claim.

### Object codecs

The ~113 group/variation encodings are generated from a single declarative
table, [`objects/spec/dnp3_objects.yaml`](objects/spec/dnp3_objects.yaml).
Adding a variation is one line; the generator emits its parser, its writer, its
registry entry, and the size the framing layer needs to walk a fragment. The
output is committed, so consumers never run the generator, and CI fails if it
drifts from the spec.

Two things stay hand-written because the table cannot express them: bit-packed
objects, whose unit of encoding is the range rather than the object, and
commands, whose fields map onto purpose-built structs rather than a measurement
type.

## Scope

Targeting IEEE 1815 conformance **Level 2, then Level 3**, with Level 4 features
staged after that. Transports: TCP, TLS, serial and UDP. Secure Authentication
v5 is out of scope; use TLS.

## Commands

| Command | What it is | Status |
| --- | --- | --- |
| `dnp3-decode` | An offline frame decoder | **Working** |
| `dnp3-outstation` | A simulated substation RTU with plant behind it and fault injection | **Working** |
| `dnp3-explorer` | A terminal DNP3 browser built on Bubble Tea v2 | **Working** |
| `dnp3-master` | A SCADA client, CSV recorder and control tool | **Working** |

### `dnp3-explorer`

Point it at a device, or run `-demo` for a full outstation in-process over
`channel.Pipe` — the real link, transport, application and object layers with
no socket and no hardware:

```console
$ dnp3-explorer -demo
dnp3-explorer  demo (in-process outstation)  connected
 1 Overview  2 Points  3 Events  4 Log

  POINT            VALUE  QUALITY                    AGE        TIMESTAMP
  BI 0                ON  ONLINE                     0s         19:43:49.947
  BI 1               OFF  ONLINE                     0s         19:43:49.947
  CT 0                 4  ONLINE                     0s         19:43:50.947
  AI 0         11029.888  ONLINE                     0s         19:43:50.947
  AI 1           175.518  ONLINE                     0s         19:43:50.947
  BO 0                ON  ONLINE                     0s         19:43:49.947

follow ON  1-4 screens · i integrity · p poll · t time · c/o close/open · / filter
```

The DNP3 session runs in its own goroutine and never touches the model; every
action is a `tea.Cmd` that returns a result message, so a poll that takes five
seconds costs five seconds of that one command and nothing else. Key handling
is a plain `HandleKey(string)`, which is what lets the whole interface be
driven from tests without a terminal.

### `dnp3-master`

Polls one outstation or many, records what they report, and issues controls:

```console
$ dnp3-master -host 127.0.0.1:20000 -record ./data -listen :8080
$ curl -s localhost:8080/status | head
[{"name":"127.0.0.1:20000","connected":true,
  "indications":"CLASS_2_EVENTS|CLASS_3_EVENTS",
  "tasks_run":9,"tasks_succeeded":9,"tasks_failed":0,"response_timeouts":0}]

$ dnp3-master -host 127.0.0.1:20000 operate trip 0
[0] CROB{PULSE_ON|TRIP count=1 on=1000ms off=0ms status=SUCCESS}: [0]=SUCCESS
```

Recording is CSV — `values.csv` for the current picture, `events.csv` for the
sequence of record, kept separate because a sequence-of-events file with poll
data mixed into it is no longer a sequence of events. Files are appended and
flushed per row: a recorder that loses the last minute of an outage to a buffer
is worse than one that writes a little more slowly.

The plan called for SQLite as well. That would mean a large dependency in what
is meant to be an example, so it is left out; anyone who wants a database can
load these files into one.

### `dnp3-outstation`

A simulated RTU whose points behave like plant: a breaker stays open once
tripped and takes time to travel, an analog ramps rather than jumping, and a
control closes the loop — tripping breaker 0 opens breaker 0, which raises a
binary input event, which the master receives.

```console
$ dnp3-outstation -points
  Breakers (binary input / binary output)
    BI   0 / BO   0  Feeder 1 breaker (closed)
    BI   2 / BO   2  Bus tie (open)
    BI   3 / BO   3  Earth switch (racked out)  [interlocked: commands refused]
  Analogs
    AI   0  Bus voltage  sine 10.8..11.2 kV
    AI   1  Feeder 1 current  walk 0..400 A
```

The fault injections matter as much as the simulation: `-inject
event-storm=500`, `-inject restart-after=5m`, `-inject offline-every=30s`.
Those are the conditions a master's error handling is usually wrong about and
the hardest to arrange with real equipment.

`dnp3-decode` takes hex from arguments, a file or a pipe and renders the layer
tree. It reads Wireshark hex-dump exports directly — the offset column and
ASCII gutter are recognised and dropped:

```console
$ go run ./cmd/dnp3-decode -f decoder/testdata/sample.hex
--  link  MSTR→OUTS UNCONFIRMED_USER_DATA  1→10  len=20  frame=27B
      transport  seq=00 FIR|FIN
      application  READ seq=00 FIR FIN
        g60v2  0x06(none,all-objects) all            0 object(s)
        g60v3  0x06(none,all-objects) all            0 object(s)
        g60v4  0x06(none,all-objects) all            0 object(s)
        g60v1  0x06(none,all-objects) all            0 object(s)

--  link  OUTS→MSTR UNCONFIRMED_USER_DATA  10→1  len=30  frame=39B
      transport  seq=00 FIR|FIN
      application  RESPONSE seq=00 FIR FIN iin=CLASS_1_EVENTS|DEVICE_RESTART
        g1v2   0x00(none,start-stop8) [0..3]         4 object(s)  4 octets
          [0] ON  ONLINE
          [1] OFF  ONLINE
          [2] ON  ONLINE
          [3] OFF  ONLINE
        g30v2  0x00(none,start-stop8) [0..1]         2 object(s)  6 octets
          [0] 300  ONLINE
          [1] 400  ONLINE

--  link  MSTR→OUTS UNCONFIRMED_USER_DATA  1→10  len=24  frame=33B
      transport  seq=00 FIR|FIN
      application  DIRECT_OPERATE seq=01 FIR FIN
        g12v1  0x17(index8,count8)    count=1        1 object(s)  12 octets
          [3] PULSE_ON|TRIP count=1 on=1000ms off=0ms → SUCCESS
```

Pass `-s` to reassemble fragments that span several frames, and `-x` for a hex
dump of each frame.

## Development

```
make check       # gofmt, vet and race-enabled tests
make fuzz-short  # 30 seconds per fuzz target
make cover       # coverage.html
make help        # everything else
```

Fuzzing is part of the normal gate rather than an occasional extra. Crashers
found by the fuzzer are committed under `testdata/fuzz/` and become regression
tests.

## License

**GNU General Public License, version 3 or later.** See [LICENSE](LICENSE) for
the full text and [NOTICE](NOTICE) for provenance and dependencies.

GPLv3 is a strong copyleft licence, and this is a library: a program that links
it must be released under the GPL too. That is the intended effect. If you need
to link it from something proprietary, it is not the right licence for you and
no exception is offered here.

All dependencies are permissive (MIT, Apache-2.0, BSD-3-Clause) and compatible
as inbound licences.

This is a clean-room implementation; no source from another DNP3 stack was
copied. It is not affiliated with or certified by the DNP Users Group.
