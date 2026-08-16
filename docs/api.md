# go-dnp3 API reference

Complete reference for the exported API of every importable package.

For a task-oriented introduction — how to build a master, how to build an
outstation, how to test either without hardware — read the
[user guide](user-guide.md) first. This document is the reference you come back
to.

```
import "github.com/dscsystems/go-dnp3"
```

**Status: the API is not yet stable.** `dnp3.Version` is `0.1.0-dev`. Names and
signatures may change before 1.0.

## Packages

| Package | Import path | What it is |
| --- | --- | --- |
| `dnp3` | `github.com/dscsystems/go-dnp3` | Value types shared by the whole stack: measurements, quality flags, timestamps, commands, errors |
| `master` | `…/master` | The master role: polls outstations, receives events, issues controls |
| `outstation` | `…/outstation` | The outstation role: holds measurements, answers polls, executes controls |
| `channel` | `…/channel` | Transports: TCP, TLS, UDP, serial, in-process pipe |
| `decoder` | `…/decoder` | Structured protocol traces for logging and tooling |
| `objects` | `…/objects` | Group/variation codecs and the object descriptor table |

`internal/link`, `internal/transport`, `internal/app` and `internal/stack` are
not importable. See [Internal types in the public API](#internal-types-in-the-public-api)
for the one place that leaks through.

---

# Package dnp3

The root package holds the value types every other package speaks in. It has no
sessions and no I/O.

```go
const Version = "0.1.0-dev"
```

## Measurements

Every measurement carries a value, a quality octet and a timestamp. Static
variations simply leave the timestamp invalid.

```go
type Binary struct {
    Value bool
    Flags Flags
    Time  Timestamp
}

type DoubleBitBinary struct {
    Value DoubleBit
    Flags Flags
    Time  Timestamp
}

type Counter struct {
    Value uint32   // wraps rather than saturating
    Flags Flags
    Time  Timestamp
}

type FrozenCounter struct {
    Value uint32   // a counter captured at a freeze command
    Flags Flags
    Time  Timestamp
}

type Analog struct {
    Value float64  // every analog variation is carried as float64
    Flags Flags
    Time  Timestamp
}

type BinaryOutputStatus struct {
    Value bool
    Flags Flags
    Time  Timestamp
}

type AnalogOutputStatus struct {
    Value float64
    Flags Flags
    Time  Timestamp
}

type OctetString []byte

const MaxOctetStringLen = 255
```

The stack carries every analog variation — 16-bit, 32-bit, single and double
precision — as a `float64` and narrows only at the encoding boundary. A point
configured for a 32-bit integer variation will report `123.5` as `123`; that is
the encoding doing what it says, not a bug. See
[Variations and precision](user-guide.md#variations-and-precision).

```go
type Indexed[T any] struct {
    Index uint16
    Value T
}
```

`Indexed` pairs a measurement with the point index it was reported at. Handler
methods receive `[]Indexed[T]`.

### DoubleBit

```go
type DoubleBit uint8

const (
    DoubleBitIntermediate  DoubleBit = 0 // both contacts open: the device is moving
    DoubleBitDeterminedOff DoubleBit = 1 // open
    DoubleBitDeterminedOn  DoubleBit = 2 // closed
    DoubleBitIndeterminate DoubleBit = 3 // both contacts closed, which is impossible
)

func (d DoubleBit) String() string
```

The numeric values are the on-the-wire encoding. A double-bit input reports a
two-contact device — typically a breaker with separate open and closed auxiliary
contacts — and can therefore distinguish a device in transit or miswired from one
that is definitely open or definitely closed.

### PointType

```go
type PointType uint8

const (
    TypeUnknown PointType = iota
    TypeBinary
    TypeDoubleBitBinary
    TypeCounter
    TypeFrozenCounter
    TypeAnalog
    TypeBinaryOutputStatus
    TypeAnalogOutputStatus
    TypeOctetString
)

func (p PointType) String() string
```

## Flags

```go
type Flags uint8
```

The quality octet accompanying most measurements. The low five bits mean the
same thing for every measurement type; the upper three are type-specific.

```go
// Common to every measurement type.
const (
    Online       Flags = 0x01 // the point is being read from the field
    Restart      Flags = 0x02 // not updated since the device restarted
    CommLost     Flags = 0x04 // communication with the point's source has failed
    RemoteForced Flags = 0x08 // forced by a downstream device
    LocalForced  Flags = 0x10 // forced by the outstation itself
)

// Type-specific. Each is valid only for the types named.
const (
    ChatterFilter Flags = 0x20 // binary, double-bit: toggling faster than the filter allows
    Rollover      Flags = 0x20 // counters (deprecated by the standard, still emitted)
    Discontinuity Flags = 0x40 // counters: not comparable against the previous reading
    OverRange     Flags = 0x20 // analogs: the value exceeds the point's range
    ReferenceErr  Flags = 0x40 // analogs: the digitising reference is inaccurate
    StateBit      Flags = 0x80 // binaries: carries the value itself, as in g1v2
)
```

Note that `0x20` and `0x40` are reused with different meanings per type. This is
why `Flags` stores the raw octet and leaves naming to the accessors.

```go
func (f Flags) Has(mask Flags) bool     // every bit in mask is set
func (f Flags) HasAny(mask Flags) bool  // any bit in mask is set
func (f Flags) Set(mask Flags) Flags
func (f Flags) Clear(mask Flags) Flags
func (f Flags) IsGood() bool
func (f Flags) String() string
func (f Flags) StringFor(t PointType) string
```

`IsGood` is online, not restarting, not comm-lost, and not forced from either
end. A cleared `Online` bit is the single most important quality signal in DNP3:
the value present alongside it is not trustworthy.

`String` renders the upper bits by position, since their meaning depends on the
point type. Use `StringFor` when the type is known:

```go
f := dnp3.Online | dnp3.OverRange
f.String()                        // "ONLINE|BIT5"
f.StringFor(dnp3.TypeAnalog)      // "ONLINE|OVER_RANGE"
```

An unset `Flags` renders as an em dash, not `0x00`.

## Timestamps

```go
type TimestampQuality uint8

const (
    TimestampInvalid        TimestampQuality = iota // carried no time at all
    TimestampUnsynchronized                         // the source clock was not synced
    TimestampSynchronized                           // the source clock was synced
)

type Timestamp struct {
    Time    time.Time
    Quality TimestampQuality
}

func Now(t time.Time) Timestamp            // synchronized
func Unsynchronized(t time.Time) Timestamp
func NoTime() Timestamp                    // the zero value

func (t Timestamp) IsValid() bool          // Quality != TimestampInvalid
func (t Timestamp) String() string
```

The time and its trustworthiness are kept together deliberately. A DNP3
measurement can carry a perfectly well-formed timestamp from an outstation whose
clock has never been set, and a consumer that ignores the quality will file that
event under 1970 or under whatever the drifted clock says.

```go
const MaxDNP3Time = int64(1)<<48 - 1     // year 10889

func TimeToDNP3(t time.Time) uint64      // ms since epoch, clamped to 48 bits
func DNP3ToTime(ms uint64) time.Time     // UTC; bits above the low 48 ignored
```

Times before the epoch clamp to zero.

## Classes

```go
type Class uint8

const (
    Class0 Class = 1 << iota // static data
    Class1                   // event class 1, conventionally the most urgent
    Class2
    Class3

    ClassNone Class = 0                        // assign a point to no class: events suppressed
    Class123        = Class1 | Class2 | Class3 // every event class
    ClassAll        = Class0 | Class123        // an integrity poll
)

func (c Class) Has(mask Class) bool
func (c Class) String() string   // "0+1+2+3", or "none"
```

Masks are how polls are expressed. An integrity poll is `ClassAll`; a routine
event poll is `Class123`.

## Controls

```go
type ControlCode uint8

// Operation types, in the low nibble.
const (
    ControlNUL      ControlCode = 0x00
    ControlPulseOn  ControlCode = 0x01
    ControlPulseOff ControlCode = 0x02
    ControlLatchOn  ControlCode = 0x03
    ControlLatchOff ControlCode = 0x04
)

// Trip/close modifiers, which pair with an operation type to drive the two
// coils of a breaker.
const (
    ControlClose ControlCode = 0x80
    ControlTrip  ControlCode = 0x40
)

func (c ControlCode) OpType() ControlCode
func (c ControlCode) IsTrip() bool
func (c ControlCode) IsClose() bool
func (c ControlCode) Clear() bool
func (c ControlCode) String() string   // e.g. "PULSE_ON|TRIP"
```

```go
type ControlRelayOutputBlock struct {
    Code    ControlCode
    Count   uint8   // how many times to execute; zero is legal and means "do nothing"
    OnTime  uint32  // milliseconds, used by the pulse operations
    OffTime uint32
    Status  CommandStatus // meaningful only on a response echo
}

func (c ControlRelayOutputBlock) String() string
```

```go
type AnalogOutputInt16   struct { Value int16;   Status CommandStatus } // g41v2
type AnalogOutputInt32   struct { Value int32;   Status CommandStatus } // g41v1
type AnalogOutputFloat32 struct { Value float32; Status CommandStatus } // g41v3
type AnalogOutputFloat64 struct { Value float64; Status CommandStatus } // g41v4

type AnalogOutput interface {
    AnalogOutputInt16 | AnalogOutputInt32 | AnalogOutputFloat32 | AnalogOutputFloat64
}
```

Do not build these by hand to send. Use the [`master` command
constructors](#commands), which zero the status octet so the outstation's echo
is what fills it in.

## CommandStatus

```go
type CommandStatus uint8

const (
    CommandSuccess           CommandStatus = 0
    CommandTimeout           CommandStatus = 1
    CommandNoSelect          CommandStatus = 2
    CommandFormatError       CommandStatus = 3
    CommandNotSupported      CommandStatus = 4
    CommandAlreadyActive     CommandStatus = 5
    CommandHardwareError     CommandStatus = 6
    CommandLocal             CommandStatus = 7
    CommandTooManyOps        CommandStatus = 8
    CommandNotAuthorized     CommandStatus = 9
    CommandAutomationInhibit CommandStatus = 10
    CommandProcessingLimited CommandStatus = 11
    CommandOutOfRange        CommandStatus = 12
    CommandDownstreamLocal   CommandStatus = 13
    CommandAlreadyComplete   CommandStatus = 14
    CommandBlocked           CommandStatus = 15
    CommandCanceled          CommandStatus = 16
    CommandBlockedOtherMstr  CommandStatus = 17
    CommandDownstreamFail    CommandStatus = 18
    CommandNonParticipating  CommandStatus = 126
    CommandUndefined         CommandStatus = 127
)

func (s CommandStatus) OK() bool     // s == CommandSuccess
func (s CommandStatus) String() string
```

## Restart

```go
type RestartMode uint8

const (
    ColdRestart RestartMode = iota // reinitialise completely, as though power cycled
    WarmRestart                    // reinitialise only the communications process
)

func (m RestartMode) String() string
```

## Helpers

```go
func AnalogFitsIn16(v float64) bool // representable as int16 without loss
func AnalogFitsIn32(v float64) bool
```

An outstation needs these when a master requests a narrow variation.

## Errors

Layer packages define their own detailed errors and wrap them in these, so a
caller can classify a failure with `errors.Is` without importing internal
packages.

```go
var (
    ErrMalformed    = errors.New("dnp3: malformed data")
    ErrTimeout      = errors.New("dnp3: timeout")
    ErrClosed       = errors.New("dnp3: closed")
    ErrNotSupported = errors.New("dnp3: not supported by peer")
    ErrBadConfig    = errors.New("dnp3: invalid configuration")
    ErrTaskFailed   = errors.New("dnp3: task failed")
    ErrNoConnection = errors.New("dnp3: no connection")
)
```

```go
if err := m.IntegrityPoll(ctx); err != nil {
    switch {
    case errors.Is(err, dnp3.ErrTimeout):     // the outstation did not answer
    case errors.Is(err, dnp3.ErrTaskFailed):  // retries exhausted
    case errors.Is(err, context.Canceled):    // we gave up, not the device
    }
}
```

---

# Package channel

```go
import "github.com/dscsystems/go-dnp3/channel"
```

The physical layer beneath a session: the thing that produces a byte stream and
reproduces it after a failure.

```go
type Channel interface {
    Connect(ctx context.Context) (io.ReadWriteCloser, error)
    Close() error
    String() string
}
```

`Connect` blocks until a connection is available or the context is done. A
session calls it again after every disconnection, so implementations own their
own reconnect timing. Every transport DNP3 runs over reduces to this contract,
which is what lets the session layer be written once.

```go
var ErrClosed = errors.New("channel: closed")
```

Returned once the channel is shut down and will produce no more connections.

## Constructors

```go
func TCPClient(addr string, retry Retry) Channel
func TCPServer(addr string) Channel

func TLSClient(addr string, tlsCfg TLSConfig, retry Retry) (Channel, error)
func TLSServer(addr string, tlsCfg TLSConfig) (Channel, error)

func UDPChannel(cfg UDPConfig) Channel
func SerialChannel(cfg SerialConfig, retry Retry) Channel

func Pipe() (a, b Channel)
```

**`TCPServer` and `TLSServer` accept one master at a time.** That is the common
field configuration; serving several concurrently needs a session per
connection, which belongs above this layer and is not implemented.

`Pipe` returns two channels connected to each other in memory. This is what
every integration test runs over and what the explorer's demo mode uses: a full
master and outstation talking through the real link, transport and application
layers, with no socket and no hardware.

```go
func ServerAddr(c Channel) net.Addr      // bound address of a listening channel, or nil
func TCPServerAddr(c Channel) net.Addr   // same, for TCPServer specifically
func ListSerialPorts() ([]string, error) // ports the system reports
```

`ServerAddr` is how a test binds `:0` and finds out which port it got.

## Retry

```go
type Retry struct {
    Min    time.Duration
    Max    time.Duration
    Factor float64
    Jitter float64 // fraction of the delay to randomise, 0 to 1
}

var DefaultRetry = Retry{Min: 500 * time.Millisecond, Max: 60 * time.Second, Factor: 2, Jitter: 0.2}
var NoRetry      = Retry{Min: 0, Max: 0, Factor: 1}
```

`NoRetry` connects once and gives up, which is what tests and one-shot tools
want.

The jitter matters more than it looks. A substation that loses a switch brings
every master's connection down at the same instant; without jitter they all
retry in lockstep and keep colliding, turning one outage into a self-sustaining
thundering herd.

## TLSConfig

```go
type TLSConfig struct {
    CertFile   string // this end's certificate
    KeyFile    string // this end's private key
    CAFile     string // the authority that signs the peer's certificate
    ServerName string // name to verify against the peer's cert; for a client, defaults to the dialled host
    MinVersion uint16 // zero uses TLS 1.2, the floor IEC 62351 sets
}
```

**Mutual authentication is not optional.** DNP3 carries controls that operate
plant, and a channel that authenticates only the server lets anyone who can
reach the port issue them. IEC 62351-3 requires both sides to present
certificates, and `TLSClient`/`TLSServer` refuse to build a configuration that
does not.

## SerialConfig

```go
type SerialConfig struct {
    Device      string        // /dev/ttyUSB0, COM3, …
    Baud        int           // zero uses 9600, the DNP3 convention
    DataBits    int           // zero uses 8
    Parity      Parity        // defaults to none
    StopBits    StopBits      // defaults to one
    ReadTimeout time.Duration // zero uses one second
}

type Parity string
const (
    ParityNone  Parity = "none"
    ParityOdd   Parity = "odd"
    ParityEven  Parity = "even"
    ParityMark  Parity = "mark"
    ParitySpace Parity = "space"
)

type StopBits string
const (
    StopBits1       StopBits = "1"
    StopBits1Point5 StopBits = "1.5"
    StopBits2       StopBits = "2"
)
```

`ReadTimeout` bounds a blocking read so a session's context can be noticed. It
is not a protocol timeout: an idle line legitimately produces nothing for
minutes at a time, and a read returning empty is not an error.

## UDPConfig

```go
type UDPConfig struct {
    LocalAddr  string // empty binds an ephemeral port on all interfaces — what a master wants
    RemoteAddr string // empty replies to whoever writes first — what an outstation wants
}
```

---

# Package master

```go
import "github.com/dscsystems/go-dnp3/master"
```

The station that polls outstations, receives their events, and issues commands.

## Session

```go
func New(cfg Config, h Handler) *Session   // a nil Handler becomes NopHandler
func (s *Session) Run(ctx context.Context, ch channel.Channel) error
func (s *Session) Connected() bool
func (s *Session) Stats() Stats
func (s *Session) LastIIN() app.IIN        // see "Internal types in the public API"
```

`Run` connects and polls until the context is cancelled. It is the call that
starts the session goroutine; run it in its own goroutine and cancel the context
to stop it.

All protocol state lives in that goroutine. **Every request method below is safe
to call from any goroutine**: each hands a task to the session goroutine and
waits for it to finish, so no caller ever touches protocol state directly. They
all block until the outstation answers or `ctx` is done.

## Config

```go
type Config struct {
    LocalAddr  uint16 // this master's link address
    RemoteAddr uint16 // the outstation's

    ResponseTimeout time.Duration // default 5s
    TaskRetryPeriod time.Duration // default 5s

    IntegrityOnStartup    bool      // class 0+1+2+3 poll at startup and on every reported restart
    DisableUnsolOnStartup bool      // send disable-unsolicited first, the standard's startup sequence
    UnsolClassMask        dnp3.Class // classes to enable after the integrity poll; zero enables none

    MaxTxFragment int // default 2048
    MaxRxFragment int // default 2048

    UseLinkConfirms bool          // link-layer confirmation; normally off over TCP
    LinkRetries     int           // retransmissions of a confirmed frame
    LinkTimeout     time.Duration // default 1s; matters only with UseLinkConfirms

    KeepAlive time.Duration // probe an idle link this often; zero disables

    Log *slog.Logger // nil discards
}
```

`KeepAlive` exists because an idle TCP connection is indistinguishable from a
peer that has gone away: both are silent. Without a probe a master notices only
when its next poll times out, which on a slow schedule can be minutes.

## Reads

```go
func (s *Session) IntegrityPoll(ctx context.Context) error
func (s *Session) ScanClasses(ctx context.Context, mask dnp3.Class) error
func (s *Session) ScanRange(ctx context.Context, group, variation uint8, start, stop uint16) error
func (s *Session) AddPeriodicScan(ctx context.Context, period time.Duration, mask dnp3.Class) error
```

`IntegrityPoll` reads every class, re-baselining the master's picture.
`ScanClasses` reads the given classes once; an empty mask is `ErrBadConfig`.

`ScanRange` reads a contiguous index range of one group and variation. **Pass
variation zero to let the outstation choose its default**, which is what a
master normally wants: the outstation knows which encoding carries its points
without loss. `start > stop` is `ErrBadConfig`.

`AddPeriodicScan` returns as soon as the scan is queued, not when it first runs;
the poll then runs for the life of the session. Failures do not stop it — a poll
that fails because the link dropped must keep trying once the link returns.

## Commands

```go
func (s *Session) DirectOperate(ctx context.Context, cmds ...Command) (CommandResult, error)
func (s *Session) SelectAndOperate(ctx context.Context, cmds ...Command) (CommandResult, error)
func (s *Session) DirectOperateNoReply(ctx context.Context, cmds ...Command) error
```

`DirectOperate` executes immediately and is the right choice for an automated
action. `SelectAndOperate` runs the two-pass sequence, and an operator-initiated
control on plant that matters should use it: the select is the outstation's
opportunity to say "not that point, not right now" before anything in the
substation moves, and a failed select is never followed by an operate.

The two requests of a select-and-operate are chained internally so nothing can
be scheduled between them. The standard requires the OPERATE to carry the
sequence number one above the SELECT, so a periodic poll landing in the middle
would make the outstation reject the operate with `NO_SELECT`.

`DirectOperateNoReply` returns as soon as the request is on the wire. Nothing
comes back, so nothing can be checked. Use it only where the outcome genuinely
does not need confirming.

All three return `ErrBadConfig` when given no commands. `DirectOperate` and
`SelectAndOperate` return a non-nil error when any individual command failed,
*and* the result, so a caller can inspect the per-point statuses either way.

### Building commands

```go
type Command struct {
    Index uint16
    // unexported: group/variation, encoded object, description
}

func (c Command) String() string

func CROB(index uint16, c dnp3.ControlRelayOutputBlock) Command
func Trip(index uint16, pulseMillis uint32) Command   // pulse the trip coil
func Close(index uint16, pulseMillis uint32) Command  // pulse the close coil
func LatchOn(index uint16) Command
func LatchOff(index uint16) Command

func AnalogOutputInt16(index uint16, v int16) Command     // g41v2
func AnalogOutputInt32(index uint16, v int32) Command     // g41v1
func AnalogOutputFloat32(index uint16, v float32) Command // g41v3
func AnalogOutputFloat64(index uint16, v float64) Command // g41v4
```

Build commands only through these constructors. The encoding differs per
variation, and the status octet must be zero on the way out so that the
outstation's echo is what fills it in.

Commands sharing a group and variation are packed into one object header with
per-object index prefixes, so a multi-point control is one request.

### CommandResult

```go
type CommandResult struct {
    Statuses []dnp3.CommandStatus // one per command, in the order sent
    Commands []Command            // echo of what was sent
}

func (r CommandResult) OK() bool     // every command succeeded; false when Statuses is empty
func (r CommandResult) Err() error   // describes the failures, or nil
func (r CommandResult) String() string
```

A multi-command request can partially succeed. `OK` is false unless every status
is `CommandSuccess`, because treating a partial success as success would tell an
operator a breaker operated when it did not.

## Time and configuration

```go
func (s *Session) SyncTime(ctx context.Context) error
func (s *Session) SyncTimeWithDelay(ctx context.Context) error
func (s *Session) WriteTime(ctx context.Context, t time.Time) error
func (s *Session) WriteDeadband(ctx context.Context, deadbands map[uint16]float32) error
func (s *Session) EnableUnsolicited(ctx context.Context, mask dnp3.Class) error
func (s *Session) DisableUnsolicited(ctx context.Context, mask dnp3.Class) error
func (s *Session) Restart(ctx context.Context, mode dnp3.RestartMode) error
```

`SyncTime` uses the LAN procedure, writing the time directly. It assumes the
transit delay is negligible against the outstation's timestamp resolution — true
over Ethernet, not over a slow serial link.

`SyncTimeWithDelay` is the serial procedure: measure the turnaround with
DELAY_MEASURE, then write a time already corrected by the one-way transit (the
round trip less the outstation's reported processing delay, halved). Without the
correction the outstation's clock lands late by that amount, which over a 1200
baud link is easily tens of milliseconds and puts every event it stamps into the
past.

`WriteDeadband` takes at most 255 entries — the limit of the one-octet count —
and rejects an empty map. A deadband is how a master tells an outstation how much
a point must move before it is worth an event.

`Restart` returns when the request was *accepted*, not when the device is back:
the outstation answers with how long it expects to be unavailable and then
restarts.

## Handler

```go
type Handler interface {
    BeginFragment(info ResponseInfo)
    EndFragment(info ResponseInfo)

    HandleBinary(info HeaderInfo, values []dnp3.Indexed[dnp3.Binary])
    HandleDoubleBit(info HeaderInfo, values []dnp3.Indexed[dnp3.DoubleBitBinary])
    HandleCounter(info HeaderInfo, values []dnp3.Indexed[dnp3.Counter])
    HandleFrozenCounter(info HeaderInfo, values []dnp3.Indexed[dnp3.FrozenCounter])
    HandleAnalog(info HeaderInfo, values []dnp3.Indexed[dnp3.Analog])
    HandleBinaryOutputStatus(info HeaderInfo, values []dnp3.Indexed[dnp3.BinaryOutputStatus])
    HandleAnalogOutputStatus(info HeaderInfo, values []dnp3.Indexed[dnp3.AnalogOutputStatus])
    HandleOctetString(info HeaderInfo, values []dnp3.Indexed[dnp3.OctetString])
}

type NopHandler struct{} // implements Handler, discards everything
```

`BeginFragment` and `EndFragment` bracket every fragment, so a consumer that
needs a consistent set — a database transaction, a UI repaint — has somewhere to
open and close it.

**Handler methods are called from the session goroutine.** A slow handler delays
the session's polling, so anything expensive belongs behind a queue;
[`ChannelHandler`](#channelhandler) is that queue. Embed `NopHandler` and
implement only the methods you care about.

`HandleOctetString` receives groups 110 and 111: the point names, firmware
versions and serial numbers a device reports as text rather than as
measurements.

```go
type ResponseInfo struct {
    IIN         app.IIN   // internal indications the outstation reported
    Unsolicited bool      // arrived unprompted rather than in answer to a poll
    Sequence    uint8     // application sequence number
    Received    time.Time // when the fragment was decoded
}

type HeaderInfo struct {
    GV    objects.GroupVar // the group and variation it arrived under
    Kind  objects.Kind     // static or event
    Class dnp3.Class       // the event class, when it came from a class poll
}

func (h HeaderInfo) IsEvent() bool
```

Consumers need `HeaderInfo` more often than it looks: the same analog point read
as a static value and received as an event mean different things to a historian,
and only the group tells them apart.

## ChannelHandler

```go
type ChannelHandler struct{ NopHandler }

func NewChannelHandler(buffer int) *ChannelHandler // buffer <= 0 uses 256
func (h *ChannelHandler) Updates() <-chan Update
func (h *ChannelHandler) Dropped() uint64

type Update struct {
    Info     HeaderInfo
    Fragment ResponseInfo

    Type  dnp3.PointType // selects which measurement field below is meaningful
    Index uint16

    Binary        dnp3.Binary
    DoubleBit     dnp3.DoubleBitBinary
    Counter       dnp3.Counter
    FrozenCounter dnp3.FrozenCounter
    Analog        dnp3.Analog
    BinaryOutput  dnp3.BinaryOutputStatus
    AnalogOutput  dnp3.AnalogOutputStatus
    OctetString   dnp3.OctetString
}
```

This is what a terminal UI or a recorder consumes: the session goroutine stays
responsive because it only ever does a non-blocking send, and the consumer reads
at its own pace.

**Updates are dropped rather than blocking the session when the consumer falls
behind**, and the drop is counted — a stalled UI must not stall the protocol.
Check `Dropped()` if you care whether you have a complete picture.

## Stats

```go
type Stats struct {
    TasksRun        uint64
    TasksSucceeded  uint64
    TasksFailed     uint64
    ResponseTimeout uint64
    FragmentsRx     uint64
    Unsolicited     uint64
    Connections     uint64
    RestartsSeen    uint64
}
```

---

# Package outstation

```go
import "github.com/dscsystems/go-dnp3/outstation"
```

The device that holds measurements, answers a master's polls, and executes its
commands.

## Session

```go
func New(cfg Config, appl Application, cmds CommandHandler) *Session
func (s *Session) Run(ctx context.Context, ch channel.Channel) error
func (s *Session) Update(fn func(*Database))
func (s *Session) Database() *Database
func (s *Session) Events() *EventBuffer
func (s *Session) Restart()
func (s *Session) Stats() Stats
```

A nil `Application` uses `NopApplication`. **A nil `CommandHandler` uses
`RejectingCommandHandler`, which refuses every control** — an outstation whose
controls are not wired up must say so rather than silently report success.

`Update` applies `fn` from the session goroutine and is safe to call from
anywhere. Batching related changes in one call is what makes a breaker opening
and its alarm asserting produce one consistent set of events rather than a torn
read.

```go
out.Update(func(db *outstation.Database) {
    db.UpdateBinary(0, dnp3.Binary{Value: true, Flags: dnp3.Online, Time: dnp3.Now(time.Now())})
    db.UpdateAnalog(3, dnp3.Analog{Value: 11.2, Flags: dnp3.Online, Time: dnp3.Now(time.Now())})
})
```

`Database()` returns the database directly. It is **not safe for concurrent
use** — prefer `Update` for modifications once the session is running. Reading or
configuring it before `Run` starts is fine.

`Restart()` makes the outstation report a restart to its master. It is what a
device calls when it has genuinely restarted and what a simulator calls to
produce the condition on demand. The restart indication is the only signal that
tells a master its whole picture is stale: the event history is gone, so no
incremental poll can recover it and only a full re-baseline will do.

## Config

```go
type Config struct {
    LocalAddr  uint16 // this outstation's link address
    RemoteAddr uint16 // the master's

    Database DatabaseConfig
    Events   EventBufferConfig

    MaxTxFragment int // default 2048
    MaxRxFragment int // default 2048

    ConfirmTimeout time.Duration // default 5s; wait for an application confirm before requeueing events
    SelectTimeout  time.Duration // default 5s; how long a select reservation stays valid

    Unsolicited UnsolicitedConfig

    UseLinkConfirms bool
    LinkRetries     int
    LinkTimeout     time.Duration // default 1s

    Log *slog.Logger // nil discards
}
```

```go
type UnsolicitedConfig struct {
    Enabled        bool          // the device-level switch: the outstation is capable of it at all
    HoldTime       time.Duration // wait this long after an event so a burst becomes one response
    MaxEvents      int           // transmit at this many queued events regardless of hold time; zero means no threshold
    ConfirmTimeout time.Duration // default 5s
    MaxRetries     int           // default 3
}
```

`Enabled` alone does not start unsolicited reporting: the master still has to
enable the individual classes with ENABLE_UNSOLICITED. After `MaxRetries`
unconfirmed re-sends the outstation gives up and waits for the master to poll
instead.

## Database

```go
func NewDatabase(cfg DatabaseConfig, events *EventBuffer) *Database

type DatabaseConfig struct {
    Binary             int
    DoubleBitBinary    int
    Counter            int
    FrozenCounter      int
    Analog             int
    BinaryOutputStatus int
    AnalogOutputStatus int
    OctetString        int

    DefaultClass dnp3.Class // applied to every point at construction
}
```

Each count is the number of points of that type, indexed `0..n-1`.

```go
func (db *Database) UpdateBinary(index uint16, v dnp3.Binary)
func (db *Database) UpdateDoubleBit(index uint16, v dnp3.DoubleBitBinary)
func (db *Database) UpdateCounter(index uint16, v dnp3.Counter)
func (db *Database) UpdateFrozenCounter(index uint16, v dnp3.FrozenCounter)
func (db *Database) UpdateAnalog(index uint16, v dnp3.Analog)
func (db *Database) UpdateBinaryOutputStatus(index uint16, v dnp3.BinaryOutputStatus)
func (db *Database) UpdateAnalogOutputStatus(index uint16, v dnp3.AnalogOutputStatus)
func (db *Database) UpdateOctetString(index uint16, v dnp3.OctetString)
```

An update generates an event when the value or its quality changed and the point
is assigned to an event class. For analogs and counters the deadband applies —
**and the comparison is against the value last *reported*, not the value last
stored.** Comparing against the stored value lets a point drift indefinitely in
deadband-sized steps without ever reporting, which is the classic implementation
bug and one that hides a slow ramp toward a limit.

Octet strings are unusual: the variation number *is* the length, so changing the
string's length changes the variation the outstation reports it in. That is
legal, and masters must cope with it.

```go
func (db *Database) Binary(index uint16) (dnp3.Binary, PointConfig, bool)
func (db *Database) DoubleBit(index uint16) (dnp3.DoubleBitBinary, PointConfig, bool)
func (db *Database) Counter(index uint16) (dnp3.Counter, PointConfig, bool)
func (db *Database) FrozenCounter(index uint16) (dnp3.FrozenCounter, PointConfig, bool)
func (db *Database) Analog(index uint16) (dnp3.Analog, PointConfig, bool)
func (db *Database) BinaryOutputStatus(index uint16) (dnp3.BinaryOutputStatus, PointConfig, bool)
func (db *Database) AnalogOutputStatus(index uint16) (dnp3.AnalogOutputStatus, PointConfig, bool)
func (db *Database) OctetString(index uint16) (dnp3.OctetString, PointConfig, bool)
```

The bool reports whether the index exists.

```go
func (db *Database) Configure(pt dnp3.PointType, index uint16, cfg PointConfig) bool
func (db *Database) AssignClass(pt dnp3.PointType, class dnp3.Class)
func (db *Database) FreezeCounters()
func (db *Database) Counts() DatabaseConfig
```

`Configure` is a no-op returning false for an index the database does not have,
so a configuration file listing a removed point does not panic the outstation at
startup. `AssignClass` sets the class of every point of a type, which is what
the ASSIGN_CLASS function code does. `FreezeCounters` copies every counter into
its frozen counterpart.

### PointConfig

```go
type PointConfig struct {
    Class           dnp3.Class // ClassNone suppresses the point's events entirely
    StaticVariation uint8      // used when reported in a class 0 or range read
    EventVariation  uint8      // used when the point's events are reported
    Deadband        float64    // ignored for binaries, which event on any change
}
```

**`Configure` replaces the whole `PointConfig`.** A zero `StaticVariation` or
`EventVariation` falls back to the point's existing value, but a zero `Class` is
`ClassNone` and a zero `Deadband` is zero — so setting one field by passing a
fresh struct silently switches the point's events off. Read the current config
back first:

```go
_, cfg, ok := db.Analog(0)
if ok {
    cfg.StaticVariation = 5   // g30v5, single precision with flags
    cfg.Deadband = 0.5
    db.Configure(dnp3.TypeAnalog, 0, cfg)
}
```

Defaults, chosen as the widest lossless encoding for each type:

| Type | Static | Event |
| --- | --- | --- |
| Binary | g1v2 (with flags) | g2v2 (absolute time) |
| DoubleBitBinary | g3v2 | g4v2 |
| Counter | g20v1 (32-bit, flags) | g22v5 (with time) |
| FrozenCounter | g21v1 | g23v5 |
| Analog | g30v1 (32-bit, flags) | g32v3 (32-bit, time) |
| BinaryOutputStatus | g10v2 | g11v2 |
| AnalogOutputStatus | g40v1 | g42v3 |

The analog defaults are 32-bit **integer** variations. A point that needs
fractions must be configured for a float variation — see
[Variations and precision](user-guide.md#variations-and-precision).

## Application

```go
type Application interface {
    Now() time.Time                     // the outstation's idea of now; tests inject a virtual clock here
    WriteAbsoluteTime(t time.Time) bool // a master set the clock; false rejects
    ColdRestart() time.Duration         // how long the device expects to be unavailable
    WarmRestart() time.Duration
    SupportsWriteTime() bool            // whether clock writes are accepted at all
}

type NopApplication struct{} // usable defaults for every method
```

The returned restart duration is reported back to the master in a group 52 time
delay.

## CommandHandler

```go
type CommandHandler interface {
    SelectCROB(index uint16, c dnp3.ControlRelayOutputBlock) dnp3.CommandStatus
    OperateCROB(index uint16, c dnp3.ControlRelayOutputBlock, op OperateType) dnp3.CommandStatus

    SelectAnalog(index uint16, v AnalogOutput) dnp3.CommandStatus
    OperateAnalog(index uint16, v AnalogOutput, op OperateType) dnp3.CommandStatus
}

type RejectingCommandHandler struct{} // refuses everything with NOT_SUPPORTED
```

**Select must not operate anything.** It reports whether the outstation *would*
accept the command. Operate is called for OPERATE, DIRECT_OPERATE and
DIRECT_OPERATE_NR, and is the call that actually moves the plant.

Both are called from the session goroutine, so a slow handler stalls the
protocol. Anything slow belongs behind a queue — but note that returning success
before the operation completes is a claim the outstation cannot take back.

```go
type OperateType uint8

const (
    OperateDirect      OperateType = iota // DIRECT_OPERATE: no prior select, answer with the outcome
    OperateDirectNoAck                    // DIRECT_OPERATE_NR: no response at all
    OperateSelected                       // OPERATE following a successful SELECT
)

func (o OperateType) String() string

type AnalogOutput struct {
    Value     float64
    Variation uint8 // 1: int32, 2: int16, 3: float32, 4: float64
}
```

The distinction matters to an implementation that logs or authorises controls: a
direct operate arrived with nothing reserved, and a no-reply operate will get no
acknowledgement whatever the outcome. A handler that does not care can read
`Value` and ignore `Variation`.

## EventBuffer

```go
const DefaultMaxEvents = 1000

type EventBufferConfig struct {
    MaxEvents int // total capacity across all classes; zero uses DefaultMaxEvents
}

func NewEventBuffer(cfg EventBufferConfig) *EventBuffer

func (b *EventBuffer) Add(e Event)
func (b *EventBuffer) Select(mask dnp3.Class, limit int) []Event
func (b *EventBuffer) Confirm() int
func (b *EventBuffer) Unselect() int
func (b *EventBuffer) Count(mask dnp3.Class) int
func (b *EventBuffer) Total() int
func (b *EventBuffer) SelectedCount() int
func (b *EventBuffer) Classes() dnp3.Class
func (b *EventBuffer) Overflowed() bool
func (b *EventBuffer) ClearOverflow()
func (b *EventBuffer) Reset()
```

The lifecycle is the part that matters and the part implementations get wrong:
an event is queued, then **selected** when it goes into a response, and only
**removed** when the master confirms that response. An outstation that drops
events at transmission loses exactly the data a sequence-of-events record exists
to preserve — and loses it silently, because the master has no way to know an
event it never saw was sent. `Unselect` is what happens when a confirmation does
not arrive: the events go back on the queue and are re-sent.

On overflow the **oldest** event is discarded, not the newest: after an overflow
the master's picture is already incomplete, and the recent past is what an
operator needs. The overflow is latched so it can be reported in the internal
indications, which is the only way the master learns there is a hole in its
record.

The session drives all of this. You normally only read the counters.

```go
type Event struct {
    Type      dnp3.PointType // selects which measurement field is meaningful
    Index     uint16
    Class     dnp3.Class
    Variation uint8
    Time      dnp3.Timestamp

    Binary        dnp3.Binary
    DoubleBit     dnp3.DoubleBitBinary
    Counter       dnp3.Counter
    FrozenCounter dnp3.FrozenCounter
    Analog        dnp3.Analog
    BinaryOutput  dnp3.BinaryOutputStatus
    AnalogOutput  dnp3.AnalogOutputStatus
    OctetString   dnp3.OctetString
}
```

A tagged union rather than an interface keeps events allocation-free in the
buffer, which matters when a storm queues thousands per second.

## Stats

```go
type Stats struct {
    RequestsReceived  uint64
    ResponsesSent     uint64
    FragmentsSent     uint64
    ConfirmsReceived  uint64
    ConfirmTimeouts   uint64
    UnknownFunction   uint64
    MalformedRequests uint64
    Connections       uint64

    CommandsExecuted    uint64
    CommandsRejected    uint64
    UnsolicitedSent     uint64
    UnsolicitedTimeouts uint64
}
```

---

# Package decoder

```go
import "github.com/dscsystems/go-dnp3/decoder"
```

Turns DNP3 octets into a structured trace. It produces a tree, not log strings:
one consumer renders it to a log, one to a terminal UI, one to text for the
command-line decoder, and none of them re-implement any parsing.

```go
func New(dir Direction, sizer app.ObjectSizer) *Decoder // pass nil for the default sizer

func (d *Decoder) Feed(data []byte, fn func(Trace))
func (d *Decoder) Reset()
func (d *Decoder) SetSynchronized(v bool)
func (d *Decoder) Stats() (link.Stats, transport.Stats)
```

`Feed` invokes `fn` for each frame found; octets that do not yet form a complete
frame are buffered until they do.

**One `Decoder` belongs to one direction of one connection.** It holds link and
transport state, and feeding both directions into a single decoder would
interleave two independent transport sequences and produce nonsense. `Reset`
clears that state, as when a connection is re-established.

`SetSynchronized` records whether the outstation's clock is set, which decides
the quality stamped on decoded timestamps. Timestamps are treated as
synchronized until told otherwise; call this from a session that has seen
NEED_TIME. An offline tool has no way to know, and marking every timestamp in a
capture as unsynchronized would be a claim the octets do not support either way.

```go
func DecodeFrame(sizer app.ObjectSizer, data []byte) (Trace, int, error)
func HexDump(data []byte) string
```

`DecodeFrame` is the one-shot form for offline tools: a frame pasted from a
capture is assumed to carry a complete fragment, which is true for the
single-frame messages that make up most DNP3 traffic. Multi-frame fragments need
a `Decoder`. Pass `nil` for the sizer to use the default.

```go
type Direction uint8
const (
    DirUnknown Direction = iota
    DirTx
    DirRx
)

type Trace struct {
    Direction Direction
    Link      LinkInfo
    Transport *TransportInfo
    App       *AppInfo

    Raw []byte // the frame's octets as they appeared on the wire
    Err error  // set when the frame itself could not be decoded
}

func (t Trace) Render(b *strings.Builder, showHex bool)
```

A frame always yields link and transport information. It yields application
information only when it completed a fragment, since a fragment can span nine
frames and only the last one finishes it — so **check `t.App != nil`**.

`Render` writes the layer tree, indented, so an operator can see at a glance
which layer a problem lives in.

```go
type LinkInfo struct {
    Control    link.Control
    Dest       uint16
    Src        uint16
    Length     uint8
    PayloadLen int // user data the frame carried
    FrameSize  int // total octets on the wire
}

type AppInfo struct {
    Header  app.Header
    Objects []app.ObjectHeader
    Values  [][]Value // decoded measurements, indexed to match Objects; nil for headers carrying none
    Err     error     // the fragment header parsed but the object headers did not
}
```

When `AppInfo.Err` is set, the headers decoded before the failure are still in
`Objects`: showing an operator what was understood before the corruption beats
showing nothing.

```go
type Value struct {
    Index uint16
    Type  dnp3.PointType
    Value string // formatted, not typed
    Flags dnp3.Flags
    Time  dnp3.Timestamp
}

func DecodeValues(h app.ObjectHeader, ctx objects.Context) ([]Value, bool)
func (v Value) String() string
```

The value is held as a formatted string because every consumer of this package —
a log line, a terminal table, a text dump — wants text. **Callers that need typed
measurements should use the object codecs directly**, or run a real `master`
session.

---

# Package objects

```go
import "github.com/dscsystems/go-dnp3/objects"
```

The group and variation codecs. Most of this package is generated from
`objects/spec/dnp3_objects.yaml`, the single source of truth for every group,
variation, size and field layout. Regenerate with `make generate`; the output is
committed, so consumers never run the generator.

Hand-written code covers what the table cannot express: bit-packed objects,
whose objects share octets, and commands, whose fields map onto purpose-built
structs.

## Identifying an object

```go
type GroupVar struct {
    Group     uint8
    Variation uint8
}

func GV(group, variation uint8) GroupVar
func (gv GroupVar) Key() uint16   // packed form used as a map key
func (gv GroupVar) String() string // "g30v5"
```

```go
type Descriptor struct {
    GV          GroupVar
    Name        string
    Level       int
    Kind        Kind
    Measurement dnp3.PointType

    SizeBits int  // under eight means bit-packed: objects share octets across a range
    Packed   bool

    HasFlags     bool
    HasTime      bool
    RelativeTime bool // timestamp is an offset from a preceding g51 CTO object

    ValueBits  int
    FloatValue bool // IEEE 754 rather than an integer
}

func Lookup(gv GroupVar) (Descriptor, bool)
func All() map[GroupVar]Descriptor // must not be modified
func (d Descriptor) SizeOctets() (int, bool)
func (d Descriptor) String() string
```

`ValueBits` and `FloatValue` are recorded rather than inferred from the variation
number, because the mapping is not consistent across groups: variation 3 is a
32-bit integer in group 30 and a single-precision float in group 40. An
outstation choosing which variation can carry a reading needs the real answer,
not a rule that happens to hold for one group.

`SizeOctets` reports false for packed objects: they are measured per range, not
per object.

```go
type Kind uint8
const (
    KindUnknown Kind = iota
    KindStatic
    KindEvent
    KindCommand
    KindCommandEvent
    KindTime
    KindClass
    KindIndication
    KindDeadband
    KindString
    KindFile
    KindAttribute
)
```

`Kind` is what lets a master decide whether a header names data, a command, or a
class to poll.

## Codecs

```go
type Codec[T any] struct {
    Parse func(buf []byte, ctx Context) T
    Write func(dst []byte, v T, ctx Context) []byte
}

func BinaryCodec(gv GroupVar)       (Codec[dnp3.Binary], bool)
func DoubleBitCodec(gv GroupVar)    (Codec[dnp3.DoubleBitBinary], bool)
func CounterCodec(gv GroupVar)      (Codec[dnp3.Counter], bool)
func AnalogCodec(gv GroupVar)       (Codec[dnp3.Analog], bool)
func BinaryOutputCodec(gv GroupVar) (Codec[dnp3.BinaryOutputStatus], bool)
func AnalogOutputCodec(gv GroupVar) (Codec[dnp3.AnalogOutputStatus], bool)
```

`Parse` assumes `buf` holds at least the object's size. Callers get that
guarantee from the framing layer, which has already validated the header's
length arithmetic against the fragment — if you call a codec on bytes of your
own, you must check the length yourself.

```go
type Context struct {
    Synchronized bool      // the outstation's clock was synchronised
    CTO          time.Time // common time of occurrence from the most recent g51 object
    HasCTO       bool
}

func (c Context) WithCTO(t time.Time) Context
func (c Context) RelativeTime(offsetMillis uint16) dnp3.Timestamp
func (c Context) RelativeOffset(t dnp3.Timestamp) uint16
func (c Context) TimeQuality() dnp3.TimestampQuality
```

`Context` carries what a decoder needs that is not in the object itself. Both
things in it are properties of the session rather than of the octets.

## Commands and times

```go
const CROBSize = 11
const (
    AnalogOutput32Size    = 5 // g41v1
    AnalogOutput16Size    = 3 // g41v2
    AnalogOutputFloatSize = 5 // g41v3
    AnalogOutputDblSize   = 9 // g41v4
)
const Time48Size = 6

func AppendCROB(dst []byte, c dnp3.ControlRelayOutputBlock) []byte
func ParseCROB(buf []byte) dnp3.ControlRelayOutputBlock

func AppendAnalogOutputInt16(dst []byte, v dnp3.AnalogOutputInt16) []byte
func AppendAnalogOutputInt32(dst []byte, v dnp3.AnalogOutputInt32) []byte
func AppendAnalogOutputFloat32(dst []byte, v dnp3.AnalogOutputFloat32) []byte
func AppendAnalogOutputFloat64(dst []byte, v dnp3.AnalogOutputFloat64) []byte
func ParseAnalogOutputInt16(buf []byte) dnp3.AnalogOutputInt16
func ParseAnalogOutputInt32(buf []byte) dnp3.AnalogOutputInt32
func ParseAnalogOutputFloat32(buf []byte) dnp3.AnalogOutputFloat32
func ParseAnalogOutputFloat64(buf []byte) dnp3.AnalogOutputFloat64

func AppendTime48(dst []byte, t dnp3.Timestamp) []byte
func ParseTime48(buf []byte) dnp3.Timestamp
func ParseTimeDelay(variation uint8, buf []byte) uint32 // always milliseconds
```

`ParseTimeDelay` handles group 52: variation 1 counts seconds and variation 2
counts milliseconds, and both are returned in milliseconds so callers need not
care — which is the whole reason the two variations exist separately on the wire.

## Packed objects

```go
func AppendPackedBinary(dst []byte, values []bool) []byte
func AppendPackedDoubleBit(dst []byte, values []dnp3.DoubleBit) []byte
func ParsePackedBinary(buf []byte, count int, out []dnp3.Binary) []dnp3.Binary
func ParsePackedBinaryOutput(buf []byte, count int, out []dnp3.BinaryOutputStatus) []dnp3.BinaryOutputStatus
func ParsePackedDoubleBit(buf []byte, count int, out []dnp3.DoubleBitBinary) []dnp3.DoubleBitBinary
func PackedOctets(count int, bitsPerObject int) int
```

`ParsePackedBinary` serves group 1 variation 1, group 10 variation 1 and group
80 variation 1, which share an encoding. **Packed variations carry no quality
information, so every value comes back online** — the encoding has nowhere to say
otherwise.

---

# Internal types in the public API

Two exported signatures name types from `internal/app`, which external modules
cannot import:

```go
func (s *master.Session) LastIIN() app.IIN
type master.ResponseInfo struct { IIN app.IIN; … }
```

Values of these types are still fully usable from outside — you just cannot write
the type name:

```go
func (h *myHandler) BeginFragment(info master.ResponseInfo) {
    iin := info.IIN            // type inference: fine
    if iin.HasError() {        // methods: fine
        log.Println("outstation reports", iin) // String(): fine
    }
}

// var x app.IIN     ← will not compile: internal package
// func f(i app.IIN) ← same
```

If you need to store or pass one across your own function boundaries, keep it in
an inferred local, convert it with `.String()`, or extract the octets with
`.Octets()`.

The methods available on an IIN value are:

```go
func (i IIN) Octets() (iin1, iin2 byte)
func (i IIN) Has(mask IIN) bool
func (i IIN) HasAny(mask IIN) bool
func (i IIN) Set(mask IIN) IIN
func (i IIN) Clear(mask IIN) IIN
func (i IIN) HasEvents() bool     // any of the three event-class bits
func (i IIN) HasError() bool      // any of the error bits
func (i IIN) EventClasses() IIN
func (i IIN) String() string      // "CLASS_1_EVENTS|DEVICE_RESTART", or "—" when clear
```

The bits themselves are named `IINClass1Events`, `IINNeedTime`,
`IINDeviceRestart`, `IINEventBufferOverflow`, `IINNoFuncCodeSupport`,
`IINObjectUnknown`, `IINParameterError`, `IINDeviceTrouble`, `IINLocalControl`,
`IINBroadcast`, `IINAlreadyExecuting`, `IINConfigCorrupt` — but since you cannot
name the constants either, test them through `HasError`, `HasEvents`, or the
string.

`decoder`'s `New`, `DecodeFrame` and `DecodeValues` also mention
`app.ObjectSizer`, `app.Header` and `app.ObjectHeader`. `nil` satisfies the
sizer parameters, and the header values are reachable through `Trace.App`.

---

## See also

- [User guide](user-guide.md) — how to build things with this
- [Device profile](device-profile.md) — what is and is not supported, including the gaps
- [`SKILL.md`](../SKILL.md) — condensed reference for AI coding agents
