# dnp3-master

Polls one outstation or many, records what they report, and issues controls.

```console
$ dnp3-master -host 127.0.0.1:20000 -record ./data -listen :8080
Polling 1 site(s) as master 1:
  127.0.0.1:20000  outstation 10    TCP 127.0.0.1:20000  poll 5s  integrity 5m0s
Recording to ./data/values.csv and ./data/events.csv
Press Ctrl-C to stop.
```

It is a usable polling client and a worked example of the `master` package: a
session per site, a `ChannelHandler` draining into a recorder, and a scheduler
that owns the poll intervals. If you are writing a SCADA client with this
library, `run.go` is the file to read.

```console
$ go run ./cmd/dnp3-master -h        # from the repository
$ go install github.com/dscsystems/go-dnp3/cmd/dnp3-master@latest
```

## Usage

```
dnp3-master [flags]
dnp3-master [flags] operate <control> <index> [value]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-config FILE` | — | read sites from YAML; **wins over `-host`/`-serial`** |
| `-host ADDR` | — | poll a single outstation over TCP (`host:port`) |
| `-serial PORT` | — | poll a single outstation over serial |
| `-baud RATE` | 9600 | serial line rate |
| `-local N` | 1 | master link address |
| `-remote N` | 10 | outstation link address |
| `-poll DUR` | 5s | event class 1+2+3 poll interval; `0` disables |
| `-integrity DUR` | 5m | integrity (class 0+1+2+3) poll interval; `0` disables |
| `-timeout DUR` | 5s | response timeout |
| `-unsolicited` | off | enable unsolicited reporting for classes 1, 2 and 3 |
| `-record DIR` | — | write `values.csv` and `events.csv` into `DIR` |
| `-listen ADDR` | — | serve the live values over HTTP |
| `-v` / `-q` | — | debug logging / errors only |

One of `-config`, `-host` or `-serial` is required.

## What it does on connect

Each site runs the standard startup sequence — disable unsolicited, integrity
poll, then enable the classes asked for — because `IntegrityOnStartup` and
`DisableUnsolOnStartup` are set. That also means **every reported device restart
re-baselines automatically**, which is the behaviour you want: a restarted
outstation has lost its event buffer, and only a full poll makes the master's
picture correct again.

Two seconds after startup, the scheduler syncs the outstation's clock (when
`sync_clock` is on, the default) and registers the periodic scans. The delay is
there so the scans do not race the startup sequence into the queue.

Serial sites get link-layer confirmation turned on automatically. A serial link
has no transport-level delivery guarantee, and link confirmation is what makes
it reliable — defaulting it on there beats making every serial configuration
remember it.

## Configuration file

`-config` takes YAML. Unknown fields are an error, not a silent no-op: a typo in
a poll interval should fail at startup rather than at three in the morning.

```yaml
# The master's own link address, shared by every site unless one overrides it.
local: 1

# Inherited by any site that does not say otherwise.
defaults:
  poll: 5s           # event class 1+2+3
  integrity: 5m      # class 0+1+2+3
  timeout: 5s        # response timeout
  keep_alive: 30s    # probe an idle link this often
  unsolicited: false
  link_confirms: false
  sync_clock: true

sites:
  - name: substation-a          # identifies the site in logs, CSV rows and the API
    host: 10.0.0.5:20000
    address: 10                 # the outstation's link address (required)
    poll: 2s

  - name: substation-b
    host: 10.0.0.6:20000
    address: 11
    local: 2                    # this site only: talk as master 2
    unsolicited: true
    integrity: 15m

  - name: pole-top-rtu
    serial: /dev/ttyUSB0
    baud: 9600
    address: 20
    poll: 30s                   # slow link, slow poll
    # link_confirms defaults to true for serial sites

  - name: secure-site
    host: 10.0.0.7:20000
    address: 12
    tls:
      cert: master.crt
      key: master.key
      ca: ca.crt                # the authority that signed the outstation's cert
      server_name: outstation.example   # optional; defaults to the dialled host
```

Validation runs before any connection is attempted and rejects: no sites, a site
with no name, duplicate names (they identify rows in the recording), a site with
both `host` and `serial` or neither, a missing `address`, TLS without all three
of cert/key/CA, TLS on a serial port, and a site whose master and outstation
addresses are equal.

**TLS is mutually authenticated and cannot be otherwise.** A channel that does
not verify its peer lets anyone who can reach the port operate plant.

## Recording

`-record DIR` writes two CSV files. They are **appended**, never truncated — a
restarted recorder must not erase the history it was started to keep — and
**flushed per row**, because a recorder that loses the last minute of an outage
to a buffer is worse than one that writes a little more slowly.

`values.csv` — the current picture. Every measurement, static and event:

```
received,site,type,index,value,quality,timestamp,source
```

`events.csv` — the sequence of record. Event-sourced measurements only, same
columns minus `source`:

```
received,site,type,index,value,quality,timestamp
```

The two are kept separate deliberately: a sequence-of-events file with static
poll data mixed into it is no longer a sequence of events.

- `received` — when this process decoded it (RFC 3339, nanoseconds)
- `timestamp` — the outstation's own timestamp, empty when the variation carried
  none
- `value` — `ON`/`OFF` for binaries, the double-bit state name, a number
  otherwise
- `quality` — flag names for the point's type; the state bit is dropped from
  binaries because the value column already says `ON` or `OFF`
- `source` — `static` or `event`

There is no SQLite output. That would mean a large dependency in what is meant
to be an example; load these files into whatever database you like.

## HTTP API

`-listen ADDR` serves three endpoints.

**`GET /status`** — one object per site, refreshed every second:

```console
$ curl -s localhost:8080/status
[{"name":"127.0.0.1:20000","target":"TCP 127.0.0.1:20000","connected":true,
  "indications":"CLASS_2_EVENTS|CLASS_3_EVENTS",
  "tasks_run":9,"tasks_succeeded":9,"tasks_failed":0,"response_timeouts":0,
  "unsolicited":0,"restarts_seen":1,"connections":1}]
```

**`GET /points`** — the latest value of every point. `?site=NAME` filters.
Output is sorted by site, type and index, so a diff of two responses shows what
changed rather than map iteration order:

```console
$ curl -s 'localhost:8080/points?site=substation-a'
[{"site":"substation-a","type":"Analog","index":0,"value":"11.03",
  "quality":"ONLINE","good":true,"timestamp":"2026-08-15T12:04:29.375Z",
  "received":"2026-08-15T12:04:29.376Z","source":"event"}]
```

**`GET /healthz`** — liveness.

## Issuing a control

`operate` is a separate one-shot mode: it connects, issues one control, prints
the outcome and exits. No integrity poll is run first — this connects to issue a
control and leave, and polling the device first would be noise on the wire and
in its log.

```console
$ dnp3-master -host 127.0.0.1:20000 operate trip 0
[0] CROB{PULSE_ON|TRIP count=1 on=1000ms off=0ms status=SUCCESS}: [0]=SUCCESS
```

| Control | Sends |
| --- | --- |
| `latch-on N` | CROB, latch on |
| `latch-off N` | CROB, latch off |
| `trip N` | CROB, pulse-on trip coil, 1000 ms |
| `close N` | CROB, pulse-on close coil, 1000 ms |
| `analog N VALUE` | group 41 variation 3 (single-precision) setpoint |

**Every control goes select-before-operate.** This is a person issuing a
control, and the select is the outstation's chance to refuse before anything in
the substation moves. It waits up to 15 seconds for the link and 30 seconds for
the exchange, and exits non-zero if the control failed.

## Reading it as example code

| File | Shows |
| --- | --- |
| `run.go` | building a session per site, one `ChannelHandler` consumer per session, scheduling periodic scans, the status poller, the HTTP API |
| `record.go` | draining `master.Update` into CSV and a live snapshot, formatting values and quality per point type |
| `config.go` | a YAML site model with inherited defaults and validation that fails early |
| `main.go` | flag handling, the one-shot `operate` path, logger setup |

The pattern worth copying is in `consume`: one goroutine per session drains
`h.Updates()`, so a slow recorder delays only its own site. It also watches
`h.Dropped()` and logs when the count moves — the handler drops updates rather
than stalling the protocol, which is the right trade, but an operator reading a
recording with holes in it deserves to know they are there.

## Troubleshooting

**Connects, then every poll times out.** Almost always the link addresses.
`-remote` must match the outstation's own address, and `-local` must match what
it expects to be polled from. Wrong addresses produce silence, not an error.

**`configuration: site "x" has neither a host nor a serial port`.** Validation
is refusing to start a session that could never connect. Check the site block.

**"updates dropped; the recorder is not keeping up" in the log.** The site is
producing measurements faster than the recorder writes them. The buffer is 4096
per session; a sustained event storm will outrun a slow disk.

**Nothing in `events.csv`.** Only event-sourced measurements go there. If the
outstation's points are not assigned to an event class, everything arrives as
static data and `values.csv` is the only file that fills.

## See also

- [`dnp3-outstation`](../dnp3-outstation/README.md) — something to point this at
- [`dnp3-explorer`](../dnp3-explorer/README.md) — the interactive equivalent
- [User guide](../../docs/user-guide.md) · [API reference](../../docs/api.md)
