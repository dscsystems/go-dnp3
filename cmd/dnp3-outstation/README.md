# dnp3-outstation

A simulated substation RTU with plant behind it, and fault injection.

It exists to be something to develop a master against. The points move the way
plant moves — a breaker stays open once tripped and takes time to travel, an
analog ramps rather than jumping, a counter only counts up — and controls close
the loop: tripping breaker 0 opens breaker 0, which raises a binary input event,
which the master then receives.

```console
$ dnp3-outstation
$ dnp3-outstation -config substation.yaml -unsolicited -v
$ dnp3-outstation -inject event-storm=500 -inject offline-every=30s
```

```console
$ go run ./cmd/dnp3-outstation -h     # from the repository
$ go install github.com/dscsystems/go-dnp3/cmd/dnp3-outstation@latest
```

It runs with no configuration at all, because a master under development needs
something to poll before anyone writes a YAML file.

## Usage

```
dnp3-outstation [flags]
```

**Transport** (TCP by default)

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen ADDR` | `:20000` | listen address |
| `-udp` | off | UDP instead of TCP |
| `-serial PORT` | — | use a serial port |
| `-baud RATE` | 9600 | serial line rate |
| `-tls-cert FILE` | — | certificate; with `-tls-key` and `-tls-ca`, enables TLS |
| `-tls-key FILE` | — | private key |
| `-tls-ca FILE` | — | authority used to verify the master |

**Device**

| Flag | Default | Meaning |
| --- | --- | --- |
| `-config FILE` | — | read the simulated plant from YAML |
| `-address N` | 10 | override the outstation link address |
| `-master N` | 1 | override the master link address |
| `-unsolicited` | off | push events without being polled |
| `-points` | — | print the point list and exit |

**Fault injection** (repeatable) and **logging** are covered below.

The TCP listener serves **one master at a time**. That is the common field
configuration; two concurrent masters would need a session per connection, which
the library does not implement.

## The default device

Two feeders with breakers, the analogs a feeder actually reports, and energy
counters. `-points` prints it and exits:

```console
$ dnp3-outstation -points
Simulated plant

  Breakers (binary input / binary output)
    BI   0 / BO   0  Feeder 1 breaker (closed)
    BI   1 / BO   1  Feeder 2 breaker (closed)
    BI   2 / BO   2  Bus tie (open)
    BI   3 / BO   3  Earth switch (racked out) (open)  [interlocked: commands refused]

  Analogs
    AI   0  Bus voltage  sine 10.8..11.2 kV
    AI   1  Feeder 1 current  walk 0..400 A
    AI   2  Feeder 2 current  walk 0..400 A
    AI   3  Transformer temperature  ramp 35..85 degC
    AI   4  Tap position  step 7..9

  Counters
    CT   0  Feeder 1 energy  12/s
    CT   1  Feeder 2 energy  8/s
```

Breakers are on class 1, currents and voltage on class 2, temperature and
counters on class 3 — a realistic split, and enough to tell whether a master's
class handling actually works.

Analog points are configured for **single-precision variations** (static g30v5,
event g32v7). The library's default is a 32-bit integer, which would truncate
every reading; a simulator that reported bus voltage as `11` instead of `11.03`
would hide exactly the bugs it exists to expose.

## Configuration file

`-config` takes YAML. Unknown fields are an error rather than a silent no-op.
Listing any `breakers`, `analogs` or `counters` replaces the defaults entirely
rather than adding to them — a file that lists two analogs gets two analogs.

```yaml
address: 10          # this outstation's link address
master: 1            # the address it expects to be polled from

# Optional. Point counts are derived from the plant below when omitted, so a
# simple file need not repeat itself.
points:
  binary: 16
  analog: 8
  counter: 4
  frozen_counter: 4
  binary_output: 8
  analog_output: 8

max_events: 1000     # event buffer capacity

unsolicited:
  enabled: false     # -unsolicited also turns this on
  hold_time: 500ms   # coalesce a burst of events into one response
  max_events: 20     # ...or send at this many queued, whichever comes first

breakers:
  - name: Feeder 1 breaker
    status_index: 0        # binary input index
    control_index: 0       # binary output index
    closed: true           # initial position
    travel_time: 200ms     # stays at its old position until it arrives
    class: 1               # 0 suppresses events, 1-3 select the event class

  - name: Earth switch
    status_index: 1
    control_index: 1
    interlocked: true      # refuses every command with NOT_SUPPORTED
    class: 1

analogs:
  - name: Bus voltage
    index: 0
    units: kV
    signal: sine           # sine | ramp | walk | step | fixed
    min: 10.8
    max: 11.2
    period: 45s            # sine, ramp and step
    noise: 0.01            # fraction of the span added as jitter
    deadband: 0.05         # how far it must move to be worth an event
    class: 2

  - name: Feeder 1 current
    index: 1
    units: A
    signal: walk           # random walk within min..max
    min: 0
    max: 400
    noise: 0.005
    deadband: 5
    class: 2

counters:
  - name: Feeder 1 energy
    index: 0
    per_second: 12
    class: 3
```

**Signals**

| `signal` | Behaviour |
| --- | --- |
| `sine` | oscillates between `min` and `max` over `period` |
| `ramp` | climbs from `min` to `max` over `period`, then repeats |
| `walk` | random walk bounded by `min` and `max` |
| `step` | steps through discrete values between `min` and `max` every `period` |
| `fixed` | holds its value; what a point becomes after an analog setpoint |

Phases are staggered at startup so a rack of points does not move in lockstep,
which makes an event stream look artificial and hides ordering bugs.

Validation rejects a duplicate index within a point type, an analog whose `max`
is below its `min`, and an outstation whose address equals the master's.

## Fault injection

The injections matter as much as the simulation. These are the conditions a
master's error handling is usually wrong about, and the hardest to arrange with
real equipment.

| Injection | Effect |
| --- | --- |
| `-inject event-storm=N` | generates N binary events per second, cycling through the breakers |
| `-inject restart-after=DUR` | reports a restart every `DUR` — the outstation asserts `DEVICE_RESTART` and its event history is gone |
| `-inject offline-every=DUR` | flips every point between `ONLINE` and `COMM_LOST` on that period |
| `-inject device-trouble` | asserts the `DEVICE_TROUBLE` internal indication |

Repeatable, and they compose:

```console
$ dnp3-outstation -inject event-storm=500 -inject restart-after=5m
```

What each is for:

- **`event-storm`** outruns the event buffer, so the outstation latches
  `EVENT_BUFFER_OVERFLOW`. A master that does not notice that indication is
  silently missing data, and this is the only way to find out cheaply.
- **`restart-after`** checks that a master re-baselines with an integrity poll
  rather than carrying on with a stale picture. `IntegrityOnStartup` in the
  library does this automatically; a hand-rolled master often does not.
- **`offline-every`** exercises bad-quality handling. A master that renders a
  `COMM_LOST` value as though it were live is the classic operator-facing bug.
- **`device-trouble`** checks that the indication reaches an operator at all.

## Controls

Controls act on the simulated plant, which is what makes the tool worth having:

- A **trip** or **close** starts the breaker moving. It keeps its old position
  until `travel_time` elapses, then arrives — and the arrival raises a binary
  input event the master receives.
- A breaker already in motion answers `ALREADY_ACTIVE`.
- An **interlocked** breaker refuses with `NOT_SUPPORTED`. A real interlock
  refuses, and reporting success would tell an operator the breaker moved when
  it did not.
- **Select** never moves anything — it answers whether the operate would be
  accepted, which is the whole point of the two-pass sequence.
- An **analog setpoint** within the point's `min`..`max` switches that point to
  the `fixed` signal at the requested value; outside it, `OUT_OF_RANGE`.

## Logging

`-v` logs protocol activity at debug level; `-q` logs nothing but errors.
Without either, it logs session-level events at info.

## Reading it as example code

| File | Shows |
| --- | --- |
| `main.go` | transport selection (TCP/UDP/serial/TLS), flag handling, the simulation tick |
| `sim.go` | a `CommandHandler` that models real plant, including select-vs-operate and travel time |
| `config.go` | a YAML plant model, deriving database sizes from the plant, per-point class and variation configuration |

`config.go`'s `applyPointConfig` is the piece most worth copying: it is where
per-point classes, deadbands and float variations get set, and it runs against
`Database()` before `Run` starts, which is the only time touching the database
directly is safe.

## Troubleshooting

**A master connects but reads nothing.** Check the link addresses — `-address`
here must equal the master's remote address, and `-master` its local one.

**Analog values arrive truncated at a master.** Not this tool: it configures
float variations. Check the master is not requesting a specific integer
variation with a range scan.

**No events, only static data.** The point's `class` is `0`, which suppresses
its events. Or the master is only polling class 0.

**Events stop arriving after a while.** The buffer filled (`max_events`) and the
oldest events were dropped, with `EVENT_BUFFER_OVERFLOW` latched into the
indications. That is the intended behaviour under `event-storm`.

**Unsolicited responses never arrive.** Two switches are needed: `unsolicited:
enabled` (or `-unsolicited`) here, *and* the master sending ENABLE_UNSOLICITED
for the classes it wants.

## See also

- [`dnp3-master`](../dnp3-master/README.md) · [`dnp3-explorer`](../dnp3-explorer/README.md) — things to point at it
- [User guide](../../docs/user-guide.md) · [API reference](../../docs/api.md)
