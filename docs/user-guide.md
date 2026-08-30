# go-dnp3 user guide

How to build a DNP3 master, an outstation, or a tool that reads DNP3 traffic,
using this library.

For the exhaustive list of types and signatures, see the
[API reference](api.md). For what the implementation does and does not support,
see the [device profile](device-profile.md).

**Contents**

- [Install](#install)
- [DNP3 in five minutes](#dnp3-in-five-minutes)
- [Building a master](#building-a-master)
- [Building an outstation](#building-an-outstation)
- [Transports](#transports)
- [Variations and precision](#variations-and-precision)
- [Events, classes and deadbands](#events-classes-and-deadbands)
- [Controls](#controls)
- [Time synchronisation](#time-synchronisation)
- [Testing without hardware](#testing-without-hardware)
- [Decoding traffic](#decoding-traffic)
- [The command-line tools](#the-command-line-tools)
- [Logging and observability](#logging-and-observability)
- [Troubleshooting](#troubleshooting)
- [Before you put it in a substation](#before-you-put-it-in-a-substation)

---

## Install

```console
$ go get github.com/dscsystems/go-dnp3
```

Go 1.26 or newer. The library depends on the standard library plus
`go.bug.st/serial`, which is reached only from `channel/serial.go`.

**Licence: GPLv3 or later.** This is strong copyleft, and it is a library: a
program that links it must be released under the GPL too. That is the intended
effect. If you need to link from something proprietary, this is not the right
licence for you and no exception is offered.

---

## DNP3 in five minutes

If you have used Modbus, the things that will surprise you are here.

**Two roles.** A *master* polls; an *outstation* answers. An outstation is the
device in the field — an RTU, a relay, a meter. The master is the SCADA system.
This library implements both.

**Link addresses, not IP.** Every station has a 16-bit link address, and it is
independent of the IP address or the serial port. A master at address 1 talks to
an outstation at address 10 over a socket that knows nothing about either. Both
ends must agree, and getting them wrong produces silence rather than an error —
frames addressed to nobody are simply dropped. This is the single most common
commissioning problem.

**Points are typed and indexed.** Binary inputs, analog inputs, counters, binary
outputs, analog outputs, each independently indexed from zero. Binary input 3
and analog input 3 are different points. There is no shared register space.

**Static values and events are different things.** A *static* read returns the
point's present value. An *event* is a record that the value changed, queued by
the outstation when it changed and delivered later. A master that only reads
static values sees the current state but misses everything that happened between
polls — which is the entire point of a sequence-of-events record. Events are how
you learn that a breaker opened and reclosed while you were not looking.

**Classes are how you ask for events.** Every point is assigned to event class 1,
2 or 3 (or to none, suppressing its events). Class 0 is not an event class at
all: it means "all static data". So:

- an **integrity poll** is class 0+1+2+3 — everything, static and queued events,
  which re-baselines the master's whole picture (`dnp3.ClassAll`);
- a **routine event poll** is class 1+2+3 (`dnp3.Class123`);
- conventionally class 1 is the urgent data and class 3 the least urgent, but
  nothing in the protocol enforces that. It is a configuration convention.

**Unsolicited responses** are the outstation pushing events without being asked.
They have to be enabled at both ends: the outstation must be built with
unsolicited capability, and the master must send ENABLE_UNSOLICITED for the
classes it wants.

**Quality flags travel with every value.** The `Online` bit is the one that
matters most: cleared, the value beside it is not trustworthy. A value of zero
with `Online` clear does not mean the measurement is zero.

**Internal indications (IIN)** are two octets on every response — the
outstation's running health report. `DEVICE_RESTART` means it has restarted and
its event history is gone, so nothing short of an integrity poll will make the
master's picture correct again. `EVENT_BUFFER_OVERFLOW` means events were lost.
`NEED_TIME` means the clock wants setting.

**Confirmation is a real protocol step.** When an outstation sends events it
asks the master to confirm; the events are held until it does, and re-sent if it
does not. This library gets that right on both ends, and it is why events
survive a dropped connection.

---

## Building a master

### The smallest useful master

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/master"
)

// handler receives everything the master decodes. Embedding NopHandler means
// we implement only the methods we care about.
type handler struct {
	master.NopHandler
}

func (h *handler) HandleBinary(info master.HeaderInfo, vs []dnp3.Indexed[dnp3.Binary]) {
	for _, v := range vs {
		kind := "static"
		if info.IsEvent() {
			kind = "event"
		}
		fmt.Printf("BI %d = %v  %s  %s  %s\n",
			v.Index, v.Value.Value, v.Value.Flags.StringFor(dnp3.TypeBinary), kind, v.Value.Time)
	}
}

func (h *handler) HandleAnalog(info master.HeaderInfo, vs []dnp3.Indexed[dnp3.Analog]) {
	for _, v := range vs {
		fmt.Printf("AI %d = %g  %s\n", v.Index, v.Value.Value, v.Value.Flags.StringFor(dnp3.TypeAnalog))
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	m := master.New(master.Config{
		LocalAddr:  1,  // our link address
		RemoteAddr: 10, // the outstation's

		// The standard startup sequence: turn unsolicited reporting off,
		// re-baseline with an integrity poll, then turn on the classes we want.
		DisableUnsolOnStartup: true,
		IntegrityOnStartup:    true,
		UnsolClassMask:        dnp3.Class123,

		ResponseTimeout: 5 * time.Second,
		KeepAlive:       30 * time.Second,
	}, &handler{})

	ch := channel.TCPClient("127.0.0.1:20000", channel.DefaultRetry)

	// Run owns the session goroutine. It returns when ctx is cancelled.
	go func() {
		if err := m.Run(ctx, ch); err != nil && ctx.Err() == nil {
			log.Println("session ended:", err)
		}
	}()

	// Poll events every five seconds and re-baseline every five minutes.
	// These return as soon as the scan is queued.
	if err := m.AddPeriodicScan(ctx, 5*time.Second, dnp3.Class123); err != nil {
		log.Fatal(err)
	}
	if err := m.AddPeriodicScan(ctx, 5*time.Minute, dnp3.ClassAll); err != nil {
		log.Fatal(err)
	}

	<-ctx.Done()
}
```

Three things to notice.

`Run` blocks, so it goes in its own goroutine, and cancelling the context is how
you stop it. It reconnects on its own: a dropped socket is not an error you need
to handle, it is the `Retry` policy doing its job.

**Every request method is safe to call from any goroutine.** `IntegrityPoll`,
`ScanRange`, `DirectOperate` and the rest submit a task to the session goroutine
and wait, so an HTTP handler can call them directly. They block until the
outstation answers or the context is done.

**Handler methods run on the session goroutine.** A handler that writes to a
slow database delays the next poll. If your consumer can be slow, use
`ChannelHandler` instead.

### Consuming updates from a channel

```go
h := master.NewChannelHandler(1024)
m := master.New(cfg, h)
go func() { _ = m.Run(ctx, ch) }()

for u := range h.Updates() {
	switch u.Type {
	case dnp3.TypeBinary:
		save(u.Index, u.Binary.Value, u.Binary.Time)
	case dnp3.TypeAnalog:
		save(u.Index, u.Analog.Value, u.Analog.Time)
	case dnp3.TypeCounter:
		save(u.Index, u.Counter.Value, u.Counter.Time)
	}
}
```

The session never blocks on this channel. When the consumer falls behind,
updates are **dropped** and counted — a stalled UI must not stall the protocol.
If a complete record matters to you, size the buffer generously and watch
`h.Dropped()`.

### One-off requests

```go
// Re-baseline now.
if err := m.IntegrityPoll(ctx); err != nil {
	log.Println("integrity poll failed:", err)
}

// Read one group over one index range. Variation zero lets the outstation
// choose its default encoding, which is usually what you want.
_ = m.ScanRange(ctx, 30, 0, 0, 15) // analog inputs 0..15

// Only class 1 and 2.
_ = m.ScanClasses(ctx, dnp3.Class1|dnp3.Class2)
```

### Classifying failures

```go
err := m.IntegrityPoll(ctx)
switch {
case err == nil:
case errors.Is(err, dnp3.ErrTimeout):
	// the outstation did not answer within ResponseTimeout
case errors.Is(err, dnp3.ErrTaskFailed):
	// retries exhausted
case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
	// we gave up, not the device
}
```

### Watching the session's health

```go
st := m.Stats()
log.Printf("connected=%v tasks=%d failed=%d timeouts=%d restarts=%d",
	m.Connected(), st.TasksRun, st.TasksFailed, st.ResponseTimeout, st.RestartsSeen)

if iin := m.LastIIN(); iin.HasError() {
	log.Println("outstation reports:", iin)
}
```

`RestartsSeen` climbing steadily means a device that keeps rebooting — worth an
alarm, because each restart throws away its event buffer.

---

## Building an outstation

An outstation is a database plus two hooks: an `Application` for the things the
stack cannot decide (the clock, restarts) and a `CommandHandler` for controls.

```go
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/outstation"
)

// commands executes controls. It holds the session so that operating an output
// can also update the database, which is what makes the plant appear to react.
type commands struct {
	sess *outstation.Session
}

// SelectCROB must not operate anything. It answers whether we *would* accept.
func (c *commands) SelectCROB(index uint16, crob dnp3.ControlRelayOutputBlock) dnp3.CommandStatus {
	if index >= 4 {
		return dnp3.CommandNotSupported
	}
	return dnp3.CommandSuccess
}

func (c *commands) OperateCROB(index uint16, crob dnp3.ControlRelayOutputBlock, op outstation.OperateType) dnp3.CommandStatus {
	if index >= 4 {
		return dnp3.CommandNotSupported
	}

	on := crob.Code.IsClose() || crob.Code.OpType() == dnp3.ControlLatchOn
	now := dnp3.Now(time.Now())

	// Close the loop: the output moves, and so does the status point the
	// master reads back. Both changes in one Update, so they are reported as
	// one consistent set.
	c.sess.Update(func(db *outstation.Database) {
		db.UpdateBinaryOutputStatus(index, dnp3.BinaryOutputStatus{Value: on, Flags: dnp3.Online, Time: now})
		db.UpdateBinary(index, dnp3.Binary{Value: on, Flags: dnp3.Online, Time: now})
	})
	return dnp3.CommandSuccess
}

func (c *commands) SelectAnalog(index uint16, v outstation.AnalogOutput) dnp3.CommandStatus {
	if index >= 2 {
		return dnp3.CommandNotSupported
	}
	return dnp3.CommandSuccess
}

func (c *commands) OperateAnalog(index uint16, v outstation.AnalogOutput, op outstation.OperateType) dnp3.CommandStatus {
	if index >= 2 {
		return dnp3.CommandNotSupported
	}
	c.sess.Update(func(db *outstation.Database) {
		db.UpdateAnalogOutputStatus(index, dnp3.AnalogOutputStatus{
			Value: v.Value, Flags: dnp3.Online, Time: dnp3.Now(time.Now()),
		})
	})
	return dnp3.CommandSuccess
}

// app supplies the clock and the restart behaviour. Embedding NopApplication
// gives working defaults for anything we leave out.
type app struct {
	outstation.NopApplication
}

func (app) Now() time.Time                     { return time.Now() }
func (app) SupportsWriteTime() bool            { return true }
func (app) WriteAbsoluteTime(t time.Time) bool { return true } // accept the master's clock
func (app) ColdRestart() time.Duration         { return 30 * time.Second }
func (app) WarmRestart() time.Duration         { return 2 * time.Second }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cmds := &commands{}

	out := outstation.New(outstation.Config{
		LocalAddr:  10, // our link address
		RemoteAddr: 1,  // the master's

		Database: outstation.DatabaseConfig{
			Binary:             8,
			Analog:             4,
			Counter:            2,
			BinaryOutputStatus: 4,
			AnalogOutputStatus: 2,
			DefaultClass:       dnp3.Class1, // every point's events go to class 1
		},
		Events: outstation.EventBufferConfig{MaxEvents: 5000},

		// Push events rather than waiting to be polled. The master still has
		// to send ENABLE_UNSOLICITED for the classes it wants.
		Unsolicited: outstation.UnsolicitedConfig{
			Enabled:   true,
			HoldTime:  200 * time.Millisecond, // coalesce a burst into one response
			MaxEvents: 20,
		},
	}, app{}, cmds)
	cmds.sess = out // the handler needs the session to write back

	// Point configuration, before Run: analog 0 carries fractions, so give it
	// a float variation, and a deadband so it does not chatter.
	if _, cfg, ok := out.Database().Analog(0); ok {
		cfg.StaticVariation = 5 // g30v5, single precision with flags
		cfg.EventVariation = 7  // g32v7, single precision with time
		cfg.Deadband = 0.1
		out.Database().Configure(dnp3.TypeAnalog, 0, cfg)
	}

	go func() {
		if err := out.Run(ctx, channel.TCPServer(":20000")); err != nil && ctx.Err() == nil {
			log.Println("outstation ended:", err)
		}
	}()

	// Feed the database from wherever the real measurements come from.
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			v := readTheFieldSomehow()
			out.Update(func(db *outstation.Database) {
				db.UpdateAnalog(0, dnp3.Analog{Value: v, Flags: dnp3.Online, Time: dnp3.Now(time.Now())})
			})
		}
	}
}
```

### The rules that matter

**Update from `Session.Update`, not from `Database()` directly.** The database is
not safe for concurrent use; `Update` runs your function on the session
goroutine, which serialises it against the protocol. Touching `Database()`
directly is fine *before* `Run` starts — that is where point configuration
belongs — and a race afterwards.

**Batch related changes into one `Update`.** A breaker opening and its alarm
asserting should be one call, so they become one consistent set of events rather
than a torn read.

**Set `Time` on your updates.** An event with no timestamp is an event a
sequence-of-events record cannot order. Use `dnp3.Now(t)` when your clock is
synchronised and `dnp3.Unsynchronized(t)` when it is not — do not claim
synchronisation you do not have.

**Set `Flags`.** A value with no `Online` bit reads as untrustworthy to every
master. When a source goes away, say so: `dnp3.CommLost` rather than a stale
value left in place, and rather than zero.

**A nil `CommandHandler` refuses everything.** `outstation.New(cfg, nil, nil)`
gives you `RejectingCommandHandler`, which answers `NOT_SUPPORTED` to every
control. That is deliberate: an outstation whose controls are not wired up must
say so, not silently report that a breaker operated.

**`Select` must not operate anything.** It answers whether the outstation would
accept the command. The stack holds the reservation for `SelectTimeout` (five
seconds by default) and calls `Operate` when the master follows through.

**Command handlers run on the session goroutine.** Slow work there stalls the
protocol — but returning success before the operation completes is a claim you
cannot take back. If the plant takes ten seconds to move, the honest answer is
usually to return success on *accepting* the command and report the actual
movement through the status point, which is what the example above does.

---

## Transports

Every transport is a `channel.Channel`, and sessions do not care which one they
were given.

```go
// TCP client — a master dialling out.
ch := channel.TCPClient("10.0.0.5:20000", channel.DefaultRetry)

// TCP server — an outstation listening. One master at a time.
ch := channel.TCPServer(":20000")

// Serial. USB adapters disappear and come back; the retry policy handles it.
ch := channel.SerialChannel(channel.SerialConfig{
	Device:   "/dev/ttyUSB0",
	Baud:     9600,
	DataBits: 8,
	Parity:   channel.ParityNone,
	StopBits: channel.StopBits1,
}, channel.DefaultRetry)

// UDP. Empty RemoteAddr means "reply to whoever writes first", which is what
// an outstation wants; a master sets it.
ch := channel.UDPChannel(channel.UDPConfig{RemoteAddr: "10.0.0.5:20000"})

// In-process, for tests and demos.
masterSide, outstationSide := channel.Pipe()
```

`DefaultRetry` backs off from half a second to a minute with 20% jitter. The
jitter is not decoration: a substation that loses a switch brings every master's
connection down at the same instant, and without jitter they all retry in
lockstep and keep colliding. Use `channel.NoRetry` in tests and one-shot tools,
where a single failed attempt should be an error rather than a loop.

### Serial specifics

Over serial you almost certainly want link-layer confirmation, which is normally
off over TCP:

```go
master.Config{
	UseLinkConfirms: true,
	LinkRetries:     3,
	LinkTimeout:     2 * time.Second, // scale with the baud rate
}
```

And use `SyncTimeWithDelay` rather than `SyncTime` — see
[Time synchronisation](#time-synchronisation).

### Several stations on one line

A session owns its channel, and a serial port cannot be opened twice. So a
multi-drop line — one RS-485 pair with several outstations on it — needs
something between the sessions and the port. That is `multidrop.Bus`: it opens
the port once and hands each session a channel of its own.

```go
port := channel.SerialChannel(channel.SerialConfig{Device: "/dev/ttyUSB0", Baud: 9600},
	channel.DefaultRetry)

bus := multidrop.New(port, multidrop.Config{})
defer bus.Close()

for _, addr := range []uint16{10, 11, 12} {
	ch, err := bus.Add(multidrop.Station{LocalAddr: 1, RemoteAddr: addr, Master: true})
	if err != nil {
		log.Fatal(err) // two sessions the bus cannot tell apart
	}
	m := master.New(master.Config{
		LocalAddr:       1,
		RemoteAddr:      addr,
		UseLinkConfirms: true,
	}, handler)
	go func() { _ = m.Run(ctx, ch) }()
}
```

Nothing above the bus changes: each session still connects, reconnects and owns
its own stack, and it cannot tell the difference between its channel and a
socket. Set `Master: false` for an outstation session — the bus routes a master
by source address and an outstation by destination, and only masters wait their
turn to transmit.

The line is half duplex, so the bus lets one master's exchange finish before the
next one starts; `Config.Turnaround` bounds how long a silent outstation holds
everyone else up. What the bus does not do is pace the polls: three masters
asking for a class 0 poll every second on a 9600 baud line will queue behind
each other no matter what sits underneath them.

The same applies to a terminal server — several RTUs behind one TCP connection
to a serial gateway is the same line with a longer wire. Give the bus a
`channel.TCPClient` instead of a serial port.

### TLS

```go
tlsCfg := channel.TLSConfig{
	CertFile: "master.crt",
	KeyFile:  "master.key",
	CAFile:   "ca.crt", // the authority that signed the outstation's certificate
}
ch, err := channel.TLSClient("10.0.0.5:20000", tlsCfg, channel.DefaultRetry)
if err != nil {
	log.Fatal(err) // bad certificate paths fail here, not at connect time
}
```

**Mutual authentication is mandatory and cannot be turned off.** DNP3 carries
controls that operate plant; a channel that authenticates only the server lets
anyone who can reach the port issue them. IEC 62351-3 requires both ends to
present certificates, and these constructors refuse to build a configuration
that does not. `MinVersion` defaults to TLS 1.2, the floor IEC 62351 sets.

Secure Authentication v5 is out of scope for this library. Use TLS.

---

## Variations and precision

A *variation* is the encoding a point is reported in. It decides how many bits
the value gets, whether flags come with it, and whether a timestamp does.

The defaults are the widest **lossless** encoding for each type — but "lossless"
is about the wire format, not about your data. **The analog defaults are 32-bit
integers**, so an outstation configured by default reports `123.5` as `123`:

| Type | Static default | Event default |
| --- | --- | --- |
| Binary | g1v2 (with flags) | g2v2 (absolute time) |
| DoubleBitBinary | g3v2 | g4v2 |
| Counter | g20v1 (32-bit, flags) | g22v5 (with time) |
| FrozenCounter | g21v1 | g23v5 |
| Analog | **g30v1 (32-bit integer, flags)** | **g32v3 (32-bit integer, time)** |
| BinaryOutputStatus | g10v2 | g11v2 |
| AnalogOutputStatus | g40v1 | g42v3 |

If a point carries fractions, configure it:

```go
_, cfg, ok := db.Analog(0)
if ok {
	cfg.StaticVariation = 5 // g30v5, single precision with flags
	cfg.EventVariation = 7  // g32v7, single precision with time
	db.Configure(dnp3.TypeAnalog, 0, cfg)
}
```

**Read the current config back before changing one field.** `Configure` replaces
the whole `PointConfig`. A zero `StaticVariation` or `EventVariation` falls back
to what was there, but a zero `Class` means `ClassNone` — so passing a fresh
struct with only a variation set silently switches that point's events off.

On the master side, `ScanRange` takes a variation too. Pass zero and let the
outstation choose: it knows which encoding carries its points without loss.

`dnp3.AnalogFitsIn16` and `AnalogFitsIn32` are there for an outstation deciding
whether a requested narrow variation can carry a reading.

---

## Events, classes and deadbands

An event is generated when a point's value or quality changes **and** the point
is assigned to an event class. `dnp3.ClassNone` suppresses a point's events
entirely, which is what you want for a noisy point nobody watches.

For analogs and counters, a *deadband* says how far the value must move before
the change is worth an event:

```go
_, cfg, _ := db.Analog(0)
cfg.Deadband = 0.5
db.Configure(dnp3.TypeAnalog, 0, cfg)
```

The comparison is against the value last **reported**, not the value last
stored. That distinction is the difference between a working deadband and the
classic bug: comparing against the stored value lets a point drift indefinitely
in deadband-sized steps without ever reporting, hiding a slow ramp toward a
limit.

A master can set deadbands remotely, which is the usual answer to an analog that
chatters:

```go
_ = m.WriteDeadband(ctx, map[uint16]float32{0: 0.5, 4: 10})
```

At most 255 points per request — the limit of the one-octet count.

### The event buffer

Events are queued, **selected** when they go into a response, and only
**removed** when the master confirms that response. If the confirmation never
arrives they go back on the queue and are re-sent. An outstation that dropped
events at transmission would lose exactly the data a sequence-of-events record
exists to preserve, and lose it silently.

Size the buffer for the worst burst you expect, not the average:

```go
Events: outstation.EventBufferConfig{MaxEvents: 5000}, // default is 1000
```

On overflow the **oldest** events are discarded and the overflow is latched into
the internal indications, which is the only way the master learns there is a
hole in its record. Watch for it:

```go
if out.Events().Overflowed() {
	log.Println("event buffer overflowed: the master's record has a gap")
}
```

### Unsolicited reporting

Two switches, one at each end:

```go
// Outstation: the device-level capability.
Unsolicited: outstation.UnsolicitedConfig{
	Enabled:        true,
	HoldTime:       200 * time.Millisecond, // a burst becomes one response
	MaxEvents:      20,                     // ...or send at 20 queued, whichever first
	ConfirmTimeout: 5 * time.Second,
	MaxRetries:     3,
},

// Master: the classes it actually wants.
UnsolClassMask: dnp3.Class123,   // enabled automatically after the integrity poll
// or, at any time:
_ = m.EnableUnsolicited(ctx, dnp3.Class123)
_ = m.DisableUnsolicited(ctx, dnp3.Class123)
```

Setting `HoldTime` to zero sends as soon as an event appears, which turns a
100-point plant trip into 100 responses. After `MaxRetries` unconfirmed re-sends
the outstation gives up and waits to be polled — so **keep polling even when you
use unsolicited reporting**. Unsolicited is an optimisation, not a substitute
for a poll schedule.

---

## Controls

```go
// Select-before-operate: the outstation gets a chance to refuse before
// anything in the substation moves.
res, err := m.SelectAndOperate(ctx, master.Trip(3, 1000))
if err != nil {
	log.Println("trip failed:", err) // includes the per-point status
}

// Direct operate: one pass, no reservation.
res, err = m.DirectOperate(ctx, master.AnalogOutputFloat32(7, 13.75))

// Several points in one request.
res, err = m.DirectOperate(ctx,
	master.LatchOn(1),
	master.LatchOff(2),
	master.Close(3, 1000),
)
```

The constructors:

| Constructor | What it sends |
| --- | --- |
| `master.Trip(index, pulseMillis)` | CROB, pulse-on with the trip coil |
| `master.Close(index, pulseMillis)` | CROB, pulse-on with the close coil |
| `master.LatchOn(index)` / `LatchOff(index)` | CROB, latch |
| `master.CROB(index, crob)` | any control relay output block you build |
| `master.AnalogOutputInt16/Int32/Float32/Float64(index, v)` | g41 setpoints |

**Use `SelectAndOperate` for operator-initiated controls on plant that matters.**
The select is not a formality: it is the outstation's opportunity to say "not
that point, not right now" before anything moves, and a failed select is never
followed by an operate. `DirectOperate` is the right choice for automated
action, where there is no operator to protect.

The two requests of a select-and-operate are chained internally so nothing can
be scheduled between them — the standard requires the OPERATE to carry the
sequence number one above the SELECT, and a periodic poll landing in the middle
would make the outstation reject the operate with `NO_SELECT`.

### Checking the outcome

A multi-command request can partially succeed, so check per point:

```go
res, err := m.DirectOperate(ctx, master.LatchOn(1), master.LatchOn(2))
if err != nil {
	for i, st := range res.Statuses {
		if !st.OK() {
			log.Printf("point %d: %s", res.Commands[i].Index, st)
		}
	}
}
```

`err` is non-nil when any command failed, and `res` is populated either way.
`res.OK()` is false unless every status is `SUCCESS`, because treating a partial
success as success would tell an operator a breaker operated when it did not.

`DirectOperateNoReply` exists for the cases where no answer is wanted. Nothing
comes back, so nothing can be checked — it returns as soon as the request is on
the wire.

---

## Time synchronisation

An outstation asserts `NEED_TIME` when its clock wants setting. Which procedure
you use depends on the link:

```go
// Ethernet: write the time directly. Transit delay is negligible against the
// outstation's timestamp resolution.
_ = m.SyncTime(ctx)

// Serial, or any slow link: measure the turnaround first, then write a time
// already corrected for the one-way transit.
_ = m.SyncTimeWithDelay(ctx)

// Or set an explicit time.
_ = m.WriteTime(ctx, someTime)
```

Over a 1200 baud link the one-way transit is easily tens of milliseconds.
Without the correction the outstation's clock lands late by that amount, and
every event it stamps goes into the past.

On the outstation side, `Application.SupportsWriteTime` decides whether clock
writes are accepted at all, and `WriteAbsoluteTime` can reject an individual
one. A device with a GPS clock should refuse.

---

## Testing without hardware

`channel.Pipe` connects a full master to a full outstation in memory: the real
link, transport, application and object layers, with no socket and no hardware.
Every integration test in this repository runs over it, and it is the fastest
way to develop against a device you do not have.

```go
func TestMyMaster(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	masterSide, outstationSide := channel.Pipe()

	out := outstation.New(outstation.Config{
		LocalAddr: 10, RemoteAddr: 1,
		Database: outstation.DatabaseConfig{Binary: 4, Analog: 2, DefaultClass: dnp3.Class1},
	}, nil, nil)

	h := &myHandler{}
	m := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10,
		ResponseTimeout: 2 * time.Second,
	}, h)

	go func() { _ = out.Run(ctx, outstationSide) }()
	go func() { _ = m.Run(ctx, masterSide) }()

	out.Update(func(db *outstation.Database) {
		db.UpdateBinary(0, dnp3.Binary{Value: true, Flags: dnp3.Online})
		db.UpdateAnalog(0, dnp3.Analog{Value: 42, Flags: dnp3.Online})
	})

	if err := m.IntegrityPoll(ctx); err != nil {
		t.Fatalf("integrity poll: %v", err)
	}
	// ...assert on what the handler received
}
```

Wait for `m.Connected()` before asserting on anything timing-sensitive.

For a more demanding target, `dnp3-outstation` simulates plant that behaves like
plant and injects the faults that break masters:

```console
$ dnp3-outstation -inject event-storm=500
$ dnp3-outstation -inject restart-after=5m
$ dnp3-outstation -inject offline-every=30s
```

Those are the conditions a master's error handling is usually wrong about, and
the hardest to arrange with real equipment.

---

## Decoding traffic

The `decoder` package turns octets into a structured tree — which is what the
terminal explorer, the logs and `dnp3-decode` all render, none of them
re-implementing any parsing.

```go
d := decoder.New(decoder.DirRx, nil)

d.Feed(bytesFromTheSocket, func(tr decoder.Trace) {
	var sb strings.Builder
	tr.Render(&sb, false) // true adds a hex dump
	log.Print(sb.String())

	// Or walk it structurally.
	if tr.App != nil {
		for i, h := range tr.App.Objects {
			for _, v := range tr.App.Values[i] {
				log.Printf("g%dv%d [%d] %s %s", h.Group, h.Variation, v.Index, v.Value, v.Flags)
			}
		}
	}
})
```

**One `Decoder` per direction per connection.** It holds link and transport
state; feeding both directions into one would interleave two independent
transport sequences and produce nonsense. Call `Reset` when the connection
comes back.

`tr.App` is nil for frames that did not complete a fragment — a fragment can
span nine frames, and only the last one finishes it. Always check.

For a single self-contained frame with no session state, use
`decoder.DecodeFrame(nil, data)`.

Decoded values are formatted strings, because every consumer of the decoder
wants text. If you need typed measurements, use the `objects` codecs or run a
real master session.

---

## The command-line tools

Four programs ship with the library. They are usable tools and also worked
examples — `cmd/dnp3-master` in particular is a reasonable model for a real
polling application.

Each has its own README with the full flag reference, configuration file format
and output shapes: [`dnp3-explorer`](../cmd/dnp3-explorer/README.md),
[`dnp3-master`](../cmd/dnp3-master/README.md),
[`dnp3-outstation`](../cmd/dnp3-outstation/README.md),
[`dnp3-decode`](../cmd/dnp3-decode/README.md). What follows is the short version.

### `dnp3-explorer` — a terminal browser for one outstation

```console
$ dnp3-explorer -demo                        # a full outstation in-process, no hardware
$ dnp3-explorer -host 10.0.0.5:20000 -remote 10 -poll 2s
```

Five screens: session overview, the point table, the sequence of events, the
activity log, and a key reference. Points can be filtered on anything in the
row and sorted by value or by quality — worst first, which is how you find the
broken points in a device with a thousand good ones. `e` exports what is on
screen, after the filter and the sort, as CSV.

Controls are deliberate: `enter` on an output opens a dialog naming exactly what
will be sent, select-before-operate by default, with a confirmation before
anything moves. `-direct` and `-no-confirm` turn that off for the situations
that need it, and while `-no-confirm` is in effect the toolbar says so.

`C` edits the connection while it runs — address, both link addresses, timeouts,
poll interval — and applies it by tearing the session down and bringing a new one
up in place. A link address read off a drawing is a guess until something
answers, and restarting the tool to try 11 instead of 10 is how ten minutes of
commissioning becomes an afternoon.

### `dnp3-master` — poller, recorder and control tool

```console
$ dnp3-master -host 127.0.0.1:20000 -record ./data -listen :8080
$ dnp3-master -config sites.yaml -v
$ dnp3-master -host 127.0.0.1:20000 operate trip 0
```

Recording is CSV: `values.csv` for the current picture, `events.csv` for the
sequence of record, kept separate because a sequence-of-events file with poll
data mixed into it is no longer a sequence of events. Files are appended and
flushed per row — a recorder that loses the last minute of an outage to a buffer
is worse than one that writes a little more slowly.

`-listen` serves `/status`, `/points` and `/healthz`.

### `dnp3-outstation` — a simulated RTU

```console
$ dnp3-outstation                             # a default substation on :20000
$ dnp3-outstation -points                     # print the point list and exit
$ dnp3-outstation --config substation.yaml
$ dnp3-outstation --inject event-storm=500 --inject offline-every=30s
```

The points behave like plant: a breaker stays open once tripped and takes time
to travel, an analog ramps rather than jumping, and a control closes the loop.

### `dnp3-decode` — offline frame decoder

```console
$ dnp3-decode 05 64 05 C0 0A 00 01 00 B1 AC
$ dnp3-decode -x -f capture.hex
$ cat capture.hex | dnp3-decode -s
```

It reads Wireshark hex-dump exports directly — the offset column and ASCII
gutter are recognised and dropped. `-s` reassembles fragments spanning several
frames; `-x` adds a hex dump.

---

## Logging and observability

Both sessions take an `*slog.Logger`. Nil discards everything.

```go
log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

m := master.New(master.Config{ /* … */ Log: log }, handler)
```

The master tags its logger with `role=master` and the outstation link address,
so several sessions in one process stay distinguishable. Debug level includes
per-frame protocol activity, which is verbose enough to be worth a level switch
in production.

For metrics, poll `Stats()` on either session. The fields worth alarming on:

| Field | Meaning |
| --- | --- |
| `master.Stats.ResponseTimeout` | the outstation is not answering |
| `master.Stats.TasksFailed` | polls or commands giving up after retries |
| `master.Stats.RestartsSeen` | a device that keeps rebooting; each restart loses its event buffer |
| `master.Stats.Connections` | climbing means a flapping link |
| `outstation.Stats.ConfirmTimeouts` | the master is not confirming; events are being re-sent |
| `outstation.Stats.MalformedRequests` | something on the wire is wrong |
| `outstation.Stats.CommandsRejected` | controls are being refused |

---

## Troubleshooting

**Nothing happens at all — no error, no data.**
Almost always the link addresses. Frames addressed to a station that is not
listening are dropped silently by design. Check `LocalAddr` and `RemoteAddr` on
both ends and that they are mirror images: the master's `RemoteAddr` is the
outstation's `LocalAddr`. `dnp3-explorer` lets you edit them live with `C`,
which is the fastest way to find the right pair.

**The connection establishes, but every poll times out.**
The socket is right and the addresses are not, or the outstation is answering a
different master. Run `dnp3-decode` over a capture and look at the link header:
the source and destination addresses are in plain sight.

**Analog values arrive truncated.**
The default analog variations are 32-bit integers. Configure the point for a
float variation — see [Variations and precision](#variations-and-precision).

**A point's events stopped arriving after I configured it.**
`Configure` replaces the whole `PointConfig`, and a zero `Class` is `ClassNone`.
Read the config back, change the field, write it back.

**Events arrive late or in bursts.**
That is `HoldTime` doing its job. Lower it, or lower `MaxEvents`, if latency
matters more than the frame count.

**The master's picture is missing changes.**
Either the points are not assigned to an event class, or their deadband is too
wide, or the event buffer overflowed. Check `out.Events().Overflowed()` and the
`EVENT_BUFFER_OVERFLOW` indication.

**`DEVICE_RESTART` keeps appearing.**
The device is rebooting. Its event history is gone each time, so only an
integrity poll makes the picture correct again — which `IntegrityOnStartup: true`
does automatically on every reported restart.

**Commands return `NO_SELECT`.**
The select expired, or something was scheduled between the select and the
operate. This library chains them so nothing can interleave, so on a real device
suspect a `SelectTimeout` shorter than the round trip.

**Commands return `NOT_SUPPORTED` from my own outstation.**
You passed a nil `CommandHandler`. That gives you `RejectingCommandHandler`.

**Updates from `ChannelHandler` are missing.**
The consumer is not keeping up and updates were dropped. Check `Dropped()`, and
either enlarge the buffer or make the consumer faster.

**A `-race` failure in outstation code.**
Something is touching `Database()` from outside the session goroutine after
`Run` started. Move it into `Session.Update`.

---

## Before you put it in a substation

- The API is not stable yet. Pin a commit.
- Nothing here is a certified conformance claim. Read the
  [device profile](device-profile.md), including its **known gaps** section, and
  check that what you rely on is actually implemented.
- A `TCPServer` outstation serves **one master at a time**. If two SCADA systems
  poll the device, that is a session per connection and it is not implemented.
- Self-address (0xFFFC) is not supported. Broadcast is received and executed but
  never answered, as the standard requires.
- Use TLS with mutual authentication for anything that leaves a locked cabinet.
  Secure Authentication v5 is out of scope.
- Size the event buffer for the worst burst, and alarm on the overflow
  indication.
- Keep polling even with unsolicited reporting enabled.
- Set `IntegrityOnStartup` so a device restart re-baselines automatically.
- Prefer `SelectAndOperate` for anything an operator initiates.
- Run `make check` and `make fuzz-short` if you fork or modify the stack. The
  parsers face bytes from devices you do not control over links that corrupt
  them, which is why fuzzing is part of the normal gate here rather than an
  occasional extra.

---

## See also

- [API reference](api.md) — every exported type and function
- [Device profile](device-profile.md) — supported features and known gaps
- [`SKILL.md`](../SKILL.md) — condensed reference for AI coding agents
