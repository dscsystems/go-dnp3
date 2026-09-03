---
name: go-dnp3
description: Build DNP3 (IEEE 1815-2012) masters, outstations and protocol tooling in Go with github.com/dscsystems/go-dnp3. Use when writing SCADA pollers, RTU/outstation simulators, substation gateways, DNP3 decoders, or any Go code that reads or writes DNP3 — including when the task mentions DNP3, outstations, RTUs, points, CROB, integrity polls, unsolicited responses, or IEEE 1815.
---

# go-dnp3

A native Go DNP3 stack with both roles: `master` (polls, receives events, issues
controls) and `outstation` (holds measurements, answers polls, executes
controls).

Reference docs in this repo: [`docs/api.md`](docs/api.md) (every exported
symbol), [`docs/user-guide.md`](docs/user-guide.md) (task-oriented),
[`docs/device-profile.md`](docs/device-profile.md) (what is and is not
supported).

## Before writing code

Run `go doc` against the real package rather than trusting memory of this file.
The API is pre-1.0 and moves.

```
go doc github.com/dscsystems/go-dnp3/master Session
go doc github.com/dscsystems/go-dnp3/outstation Database
go doc -all github.com/dscsystems/go-dnp3/channel
```

Then verify what you wrote compiles and runs. `channel.Pipe` makes an end-to-end
test cheap — see [Verify your work](#verify-your-work).

## Packages

| Import | Use for |
| --- | --- |
| `github.com/dscsystems/go-dnp3` | value types: measurements, `Flags`, `Timestamp`, `Class`, commands, errors |
| `…/master` | the polling side |
| `…/outstation` | the device side |
| `…/channel` | TCP, TLS, UDP, serial, in-process pipe |
| `…/multidrop` | one channel shared by several sessions: multi-drop serial, serial gateways |
| `…/decoder` | structured traces for logs and tooling |
| `…/objects` | group/variation codecs, object descriptor table |

`internal/*` is not importable. Module requires Go 1.26. **Licence is GPLv3+** —
anything linking it must be GPL too; say so if the user's context implies
proprietary distribution.

---

## Master template

```go
type handler struct{ master.NopHandler } // embed, implement only what you need

func (h *handler) HandleAnalog(info master.HeaderInfo, vs []dnp3.Indexed[dnp3.Analog]) {
	for _, v := range vs {
		// info.IsEvent() distinguishes an event from a static read
		// v.Value.Flags.IsGood() before you trust v.Value.Value
	}
}

m := master.New(master.Config{
	LocalAddr:  1,  // ours
	RemoteAddr: 10, // the outstation's
	DisableUnsolOnStartup: true,
	IntegrityOnStartup:    true,        // also re-baselines on every reported restart
	UnsolClassMask:        dnp3.Class123,
	ResponseTimeout:       5 * time.Second,
	KeepAlive:             30 * time.Second,
}, &handler{})

go func() { _ = m.Run(ctx, channel.TCPClient("10.0.0.5:20000", channel.DefaultRetry)) }()

_ = m.AddPeriodicScan(ctx, 5*time.Second, dnp3.Class123) // events
_ = m.AddPeriodicScan(ctx, 5*time.Minute, dnp3.ClassAll) // integrity
```

`Run` blocks and reconnects on its own; cancel `ctx` to stop it. There is **no
`Close`/`Stop` method** — context cancellation is the shutdown path.

Request methods (`IntegrityPoll`, `ScanClasses`, `ScanRange`, `DirectOperate`,
`SelectAndOperate`, `SyncTime`, `WriteDeadband`, `EnableUnsolicited`,
`DisableUnsolicited`, `Restart`, `WriteTime`) are **safe from any goroutine** and
block until answered or `ctx` is done.

## Outstation template

```go
type commands struct{ sess *outstation.Session }

func (c *commands) SelectCROB(i uint16, crob dnp3.ControlRelayOutputBlock) dnp3.CommandStatus {
	// MUST NOT operate anything — answer whether we *would* accept
	return dnp3.CommandSuccess
}
func (c *commands) OperateCROB(i uint16, crob dnp3.ControlRelayOutputBlock, op outstation.OperateType) dnp3.CommandStatus {
	on := crob.Code.IsClose() || crob.Code.OpType() == dnp3.ControlLatchOn
	c.sess.Update(func(db *outstation.Database) {
		db.UpdateBinaryOutputStatus(i, dnp3.BinaryOutputStatus{Value: on, Flags: dnp3.Online, Time: dnp3.Now(time.Now())})
	})
	return dnp3.CommandSuccess
}
func (c *commands) SelectAnalog(i uint16, v outstation.AnalogOutput) dnp3.CommandStatus { … }
func (c *commands) OperateAnalog(i uint16, v outstation.AnalogOutput, op outstation.OperateType) dnp3.CommandStatus { … }

type app struct{ outstation.NopApplication }
func (app) Now() time.Time                     { return time.Now() }
func (app) SupportsWriteTime() bool            { return true }
func (app) WriteAbsoluteTime(t time.Time) bool { return true }

cmds := &commands{}
out := outstation.New(outstation.Config{
	LocalAddr: 10, RemoteAddr: 1,
	Database: outstation.DatabaseConfig{
		Binary: 8, Analog: 4, Counter: 2, BinaryOutputStatus: 4,
		DefaultClass: dnp3.Class1,
	},
	Events: outstation.EventBufferConfig{MaxEvents: 5000},
	Unsolicited: outstation.UnsolicitedConfig{Enabled: true, HoldTime: 200 * time.Millisecond, MaxEvents: 20},
}, app{}, cmds)
cmds.sess = out

go func() { _ = out.Run(ctx, channel.TCPServer(":20000")) }()

out.Update(func(db *outstation.Database) {
	db.UpdateAnalog(0, dnp3.Analog{Value: 11.2, Flags: dnp3.Online, Time: dnp3.Now(time.Now())})
})
```

---

## Hard rules

These are the mistakes that compile and then misbehave.

1. **Update the outstation database only through `Session.Update`** once `Run`
   has started. `Database()` is not safe for concurrent use. Configuring points
   via `Database()` *before* `Run` is fine and is where point configuration
   belongs.

2. **Batch related changes into one `Update` call.** A breaker opening and its
   alarm asserting must be one call, or the master sees a torn read.

3. **Always set `Flags` and `Time` on measurements.** `dnp3.Online` at minimum;
   `dnp3.Now(t)` for a synced clock, `dnp3.Unsynchronized(t)` when it is not.
   Never claim synchronisation you do not have. When a source goes away, set
   `dnp3.CommLost` — do not leave a stale value or write zero.

4. **Master `Handler` methods run on the session goroutine.** Slow work there
   delays polling. Use `master.NewChannelHandler(n)` and consume
   `h.Updates()` if the consumer can be slow — but note updates are **dropped**
   (and counted in `h.Dropped()`) when the consumer falls behind.

5. **`Configure` replaces the entire `PointConfig`.** Zero `StaticVariation` /
   `EventVariation` fall back to the existing value, but zero `Class` is
   `ClassNone` and silently kills the point's events. Always read-modify-write:

   ```go
   if _, cfg, ok := db.Analog(0); ok {
       cfg.StaticVariation = 5 // g30v5, single precision
       db.Configure(dnp3.TypeAnalog, 0, cfg)
   }
   ```

6. **Analog defaults are 32-bit integer variations** (static g30v1, event
   g32v3). `123.5` arrives as `123` unless you configure a float variation
   (static 5, event 7). This is the single most common "the library is broken"
   report and it is not a bug.

7. **`SelectCROB`/`SelectAnalog` must not move anything.** Operate is the call
   that acts. Command handlers run on the session goroutine; returning success
   before the plant has actually moved is a claim that cannot be retracted — the
   usual honest pattern is to accept the command and report real movement
   through the status point.

8. **`outstation.New(cfg, nil, nil)` refuses every control** with
   `NOT_SUPPORTED` (`RejectingCommandHandler`). That is intended. Pass a real
   handler when controls should work.

9. **Analog output commands do not update the analog output status point by
   themselves.** Your `OperateAnalog` must write it back through `Update`.

10. **Use `SelectAndOperate` for operator-initiated controls**, `DirectOperate`
    for automated ones. A multi-command result can partially succeed: check
    `res.OK()` / iterate `res.Statuses`, never just `err == nil`.

11. **One `decoder.Decoder` per direction per connection.** Sharing one across
    both directions interleaves two transport sequences and produces nonsense.
    `Trace.App` is nil for frames that did not complete a fragment — check it.

12. **Link addresses are not IP addresses.** The master's `RemoteAddr` must equal
    the outstation's `LocalAddr` and vice versa. Wrong addresses produce total
    silence, not an error.

---

## Recipes

**Poll one range of one group**

```go
_ = m.ScanRange(ctx, 30, 0, 0, 15) // analog inputs 0..15; variation 0 = outstation's choice
```

**Issue controls**

```go
res, err := m.SelectAndOperate(ctx, master.Trip(3, 1000))     // breaker trip, 1s pulse
res, err = m.DirectOperate(ctx, master.Close(3, 1000))
res, err = m.DirectOperate(ctx, master.LatchOn(1), master.LatchOff(2))
res, err = m.DirectOperate(ctx, master.AnalogOutputFloat32(7, 13.75))
res, err = m.DirectOperate(ctx, master.CROB(4, dnp3.ControlRelayOutputBlock{
	Code: dnp3.ControlPulseOn | dnp3.ControlTrip, Count: 1, OnTime: 500,
}))
```

Never hand-build the command object bytes; the constructors zero the status
octet so the outstation's echo fills it in.

**Time sync** — `m.SyncTime(ctx)` on Ethernet; `m.SyncTimeWithDelay(ctx)` on
serial or any slow link (it measures the turnaround and corrects for one-way
transit).

**Deadbands** — `m.WriteDeadband(ctx, map[uint16]float32{0: 0.5})`, max 255
entries. Outstation side: `PointConfig.Deadband`. The comparison is against the
last *reported* value, not the last stored one.

**Classify errors**

```go
errors.Is(err, dnp3.ErrTimeout)    // no answer within ResponseTimeout
errors.Is(err, dnp3.ErrTaskFailed) // retries exhausted
errors.Is(err, dnp3.ErrBadConfig)  // bad arguments (empty class mask, start>stop, no commands)
// also: dnp3.ErrMalformed, ErrClosed, ErrNotSupported, ErrNoConnection
```

**Health**

```go
m.Connected(); m.Stats() // TasksFailed, ResponseTimeout, RestartsSeen, Connections…
out.Stats()              // ConfirmTimeouts, MalformedRequests, CommandsRejected…
out.Events().Overflowed() // the master's sequence-of-events record has a hole
```

**Read the IIN** — `info.IIN` in `BeginFragment`, or `m.LastIIN()`:

```go
if iin := m.LastIIN(); iin.HasError() { log.Println(iin) } // HasEvents(), Octets(), String()
```

**Transports**

```go
channel.TCPClient(addr, channel.DefaultRetry)
channel.TCPServer(":20000")                       // one master at a time
channel.UDPChannel(channel.UDPConfig{RemoteAddr: addr})
channel.SerialChannel(channel.SerialConfig{Device: "/dev/ttyUSB0", Baud: 9600}, channel.DefaultRetry)
channel.TLSClient(addr, channel.TLSConfig{CertFile: …, KeyFile: …, CAFile: …}, channel.DefaultRetry) // (Channel, error)
channel.Pipe()                                    // (a, b Channel) in memory
```

TLS is **mutual-auth only**, TLS 1.2 floor — by design, not an oversight. Over
serial also set `UseLinkConfirms: true`, `LinkRetries`, `LinkTimeout`.

**Several sessions on one line** — a serial port cannot be opened twice, so a
multi-drop line (or several RTUs behind one terminal server) needs a bus between
the sessions and the port:

```go
bus := multidrop.New(port, multidrop.Config{})   // takes ownership of the channel
defer bus.Close()
ch, err := bus.Add(multidrop.Station{LocalAddr: 1, RemoteAddr: 10, Master: true})
go m.Run(ctx, ch)                                // Master: false for an outstation session
```

The addresses must match the session's own config. The bus routes inbound frames
by address, serialises transmission, and holds the half-duplex line for one
master's exchange at a time. It does not pace the polls — that is still yours.

If two independent parts of a program might reach for the same device without
either knowing it, use `multidrop.Registry` instead of building the `Bus`
directly — `reg.Open(ch, cfg)` returns the existing bus for an equivalent
channel (same device, same baud/host/port) rather than opening it twice, and
`reg.Release(bus)` closes it only once every caller that opened it has let go.

**Transfer files** (group 70)

```go
data, err := m.ReadFileBytes(ctx, "/config.xml")   // or ReadFile(ctx, name, w)
err = m.WriteFileBytes(ctx, "/config.xml", data)   // WriteFile needs the size up front
entries, err := m.ReadDirectory(ctx, "/")          // []dnp3.FileInfo
err = m.DeleteFile(ctx, "/old.log")

// Outstation: off unless a handler is set. OpenDir is rooted at os.Root, so
// "../.." cannot escape it.
files, err := outstation.OpenDir("/var/lib/rtu/public")
files.ReadOnly = true
cfg.Files = outstation.FileConfig{Handler: files}
```

A refusal wraps `dnp3.ErrFileTransfer` and names the status; a device without
file transfer gives `dnp3.ErrNotSupported`. **A transfer holds the session for
its whole duration** — polls wait behind it.

**Read what a device is** (group 0)

```go
attrs, err := m.ReadAttributes(ctx)          // the standard set
a, err := m.ReadAttribute(ctx, 0, 250)       // one, by number
for _, a := range attrs {
	fmt.Println(a.Name(), a.Value())          // "product name and model RTU-9000"
}

// Outstation: configure the nameplate; the point counts and fragment sizes
// are derived from the database and need not be listed.
cfg.Attributes = []dnp3.Attribute{
	objects.StringAttribute(252, "Vendor"),
	objects.StringAttribute(250, "Model"),
}
```

`dnp3.ErrNotSupported` means the device has no attributes. The variation *is*
the attribute — 250 is the product name, not "the product name encoded one way"
— so there is no codec table and a device's own attributes decode fine.

**Decode octets**

```go
d := decoder.New(decoder.DirRx, nil)
d.Feed(buf, func(tr decoder.Trace) {
	var sb strings.Builder
	tr.Render(&sb, false)
	log.Print(sb.String())
})
decoder.DecodeFrame(nil, oneFrame) // one-shot, no session state
```

---

## Does not exist — do not invent it

Checked against the current tree. If a task needs one of these, say it is
unimplemented rather than writing a call that will not compile.

- **No `Session.Close()`, `Stop()` or `Shutdown()`** on either role. Cancel the
  context passed to `Run`.
- **No master-side freeze, `ASSIGN_CLASS` or `RECORD_CURRENT_TIME`.** The
  outstation answers those function codes; the master API does not send them.
  (`Database.FreezeCounters()` and `Database.AssignClass()` are outstation-local
  calls, not protocol requests.)
- **No datasets** (groups 85–87), **no `FREEZE_AT_TIME`**, **no Secure
  Authentication v5** (out of scope — use TLS), **no self-address** (0xFFFC).
- **Device attributes are implemented** (group 0) for reading. No writing, and
  no variation 255 "list of attributes" request.
- **File transfer is implemented** (group 70) — read, write, list, delete — but
  there is **no `AUTHENTICATE_FILE` handshake**, and an outstation serves **one
  transfer at a time**.
- **No multi-master TCP server.** `TCPServer`/`TLSServer` serve one connection at
  a time; concurrent sessions would need a session per connection, which is not
  implemented.
- **No `db.Set*` / `db.Get*`** — the methods are `UpdateBinary`, `UpdateAnalog`,
  … and the typed accessors `Binary(i)`, `Analog(i)`, … returning
  `(value, PointConfig, bool)`.
- **No typed values from `decoder`** — `decoder.Value.Value` is a formatted
  `string`. Use `objects` codecs or a real master session for typed data.
- **You cannot name `app.IIN`** from outside the module (it lives in
  `internal/app`). Values work fine via inference and methods; `var x app.IIN`
  will not compile.

---

## Verify your work

An end-to-end test needs no hardware and no socket — `channel.Pipe` runs a real
master against a real outstation through the real layers:

```go
mch, och := channel.Pipe()
out := outstation.New(outstation.Config{LocalAddr: 10, RemoteAddr: 1,
	Database: outstation.DatabaseConfig{Binary: 4, Analog: 2, DefaultClass: dnp3.Class1}}, nil, nil)
m := master.New(master.Config{LocalAddr: 1, RemoteAddr: 10, ResponseTimeout: 2 * time.Second}, h)

go func() { _ = out.Run(ctx, och) }()
go func() { _ = m.Run(ctx, mch) }()

out.Update(func(db *outstation.Database) {
	db.UpdateBinary(0, dnp3.Binary{Value: true, Flags: dnp3.Online})
})
// wait for m.Connected(), then:
if err := m.IntegrityPoll(ctx); err != nil { t.Fatal(err) }
```

Poll `m.Connected()` before asserting on anything timing-sensitive.

Against a more demanding target:

```
go run ./cmd/dnp3-outstation -inject event-storm=500
go run ./cmd/dnp3-outstation -inject restart-after=5m -inject offline-every=30s
go run ./cmd/dnp3-explorer -demo          # full outstation in-process, no hardware
go run ./cmd/dnp3-decode -f capture.hex   # reads Wireshark hex exports directly
```

Each tool has a README with its full flag reference, config-file schema and
output format — [`cmd/README.md`](cmd/README.md) indexes them. Read the relevant
one before generating config files or parsing these tools' output.

When modifying the stack itself:

```
make check       # gofmt, vet, race-enabled tests — the pre-commit gate
make fuzz-short  # 20s per fuzz target; parsers face hostile bytes, so this is part of the gate
make generate    # regenerate object codecs after editing objects/spec/dnp3_objects.yaml
```

Object codecs are generated from `objects/spec/dnp3_objects.yaml`. **Edit the
YAML, not `zz_generated_*.go`** — CI fails if the committed output drifts from
the spec.

## Domain notes worth stating to users

- **Static vs event**: a static read gives the present value; events record
  changes and are what a sequence-of-events record is made of. A master that
  only reads statics misses everything between polls.
- **Class 0 is not an event class** — it means all static data. Integrity poll =
  `dnp3.ClassAll` (0+1+2+3); routine event poll = `dnp3.Class123`.
- **`DEVICE_RESTART`** means the event history is gone; only an integrity poll
  makes the picture correct again (`IntegrityOnStartup: true` does this
  automatically).
- **Keep polling even with unsolicited enabled** — after `MaxRetries`
  unconfirmed re-sends, the outstation gives up and waits to be polled.
- **Nothing here is a certified conformance claim.** Point users at
  [`docs/device-profile.md`](docs/device-profile.md), including its known-gaps
  section, before they rely on a feature in the field.
