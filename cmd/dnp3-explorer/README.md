# dnp3-explorer

A full-screen terminal browser for one DNP3 outstation, driven by the keyboard
or the mouse.

It answers the question you actually have when pointing at an unfamiliar device:
*what is this thing reporting, and does it respond?*

```console
$ dnp3-explorer -demo
dnp3-explorer  demo (in-process outstation)  ● connected      up 1:36  13.4/s  12:04:29
 1 Overview  2 Points  3 Events  4 Log  5 Files  6 Help                   20 points
────────────────────────────────────────────────────────────────────────────────────
 POINT ▲          VALUE TREND        QUALITY                AGE TIMESTAMP
 BI 0                ON ▁▁▁▁████████ ONLINE                 0s  12:04:29.375
 BI 5               OFF ▄▄▄▄▄▄▄▄▄▄▄▄ COMM_LOST              0s  12:04:29.375
 CT 0             10422 ▁▂▃▄▄▅▆▆▇███ ONLINE                 0s  12:04:29.375
 AI 0         11019.967 ▁▁▂▂▃▄▄▅▆▆▇█ ONLINE                 0s  12:04:29.375
 AI 4             0.420 ▄▄▄▄▄▄▄▄▄▄▄▄ ONLINE|LOCAL_FORCED    0s  12:04:29.375
 BO 0                ON ▁▁▁▁████████ ONLINE                 0s  12:04:29.375
 OS 0  GO-DNP3 DEMO RTU              ONLINE                 3s  —
────────────────────────────────────────────────────────────────────────────────────
 [i Integrity] [p Poll] [/ Filter] [d Inspect] [c Close] [o Open] [S SBO] [e Export]
 ↑↓ move · enter act · d inspect · / filter · < > r sort · b deadband · ? help
```

```console
$ go run ./cmd/dnp3-explorer -demo    # from the repository
$ go install github.com/dscsystems/go-dnp3/cmd/dnp3-explorer@latest
```

**`-demo` needs no hardware.** It runs a full outstation inside the same
process, connected over `channel.Pipe` — the real link, transport, application
and object layers, with no socket and no device. It is the fastest way to see
what the tool does.

## Usage

```
dnp3-explorer -host HOST:PORT [flags]
dnp3-explorer -demo
```

**Connection** — all of it editable while running, with `C`

| Flag | Default | Meaning |
| --- | --- | --- |
| `-host ADDR` | — | outstation address (`host:port`) |
| `-serial PORT` | — | use a serial port instead of TCP |
| `-baud RATE` | 9600 | serial line rate |
| `-local N` | 1 | master link address |
| `-remote N` | 10 | outstation link address |
| `-poll DUR` | 5s | event class poll interval; `0` disables |
| `-timeout DUR` | 5s | response timeout |
| `-demo` | off | run a simulated outstation in-process |

**Interface**

| Flag | Default | Meaning |
| --- | --- | --- |
| `-mouse` | true | enable the mouse |
| `-inline` | off | draw inline instead of taking the whole terminal |
| `-stale DUR` | 30s | fade points not updated for this long; `0` disables |
| `-file-root PATH` | `/` | directory the Files screen opens on |

**Controls**

| Flag | Default | Meaning |
| --- | --- | --- |
| `-direct` | off | direct operate instead of select-before-operate |
| `-no-confirm` | off | issue controls without asking first |
| `-pulse MS` | 1000 | pulse duration for trip and close |

## The six screens

**1 Overview** — the session and the device's health: connection state, uptime,
message rate, the internal indications the outstation is asserting, and the task
counters.

**2 Points** — the point table. Every point the device has reported, with its
value, a sparkline trend, the named quality flags, how long since it last
updated, and the outstation's own timestamp.

**3 Events** — the sequence of events, newest last. What changed and when, as
distinct from what the value is now.

**4 Log** — the activity log: what was sent, what came back, what failed.

**5 Files** — the device's filesystem, over group 70 file transfer. `enter`
opens a directory or reads a file into the pane; text is shown as text and
anything else as a hex dump, so a firmware image is readable as octets rather
than as noise. `w` saves the selected file to local disk, `W` sends a local file
up, and `D` deletes one.

Sending and deleting always ask first, whatever `-no-confirm` says. A control is
one message and another control reverses it; a file transfer takes minutes,
holds the session while it runs, and replaces something the device may be
running on.

```
 /logs                                                              2 entries
 NAME                        SIZE      MODE      MODIFIED
 events.log                 450 B      rw-r-----  2026-02-11 09:14
 startup.log                1.2 KiB    rw-r-----  2026-02-10 22:03
```

**Devices do not agree on what their root is called.** Some answer to `/`,
others only to `C:/`, `C:\` or `.`; a Windows-hosted outstation may want
backslashes throughout. So `:` types a path, `-file-root` sets the one the
screen opens on, and a listing that fails says which to try:

```
 /: dnp3: file transfer failed: file not found  —  press : to try another path…
```

Entry names are joined onto the current directory using whatever separator that
directory already uses, so a device reached at `C:\LOGS` gets
`C:\LOGS\events.log` rather than a path it has never heard of.

In `-demo` the simulated device carries files of its own, so the screen has
something to browse without hardware.

**6 Help** — the full key and mouse reference.

Points can be filtered on anything in the row and sorted by any column —
including by quality, **worst first**, which is how you find the broken points
in a device with a thousand good ones. `d` opens the inspector for one point,
with the flags named, the trend drawn, and the group and variation each value
arrived under.

## Keys

| Key | Does |
| --- | --- |
| `1`–`6`, `tab` | switch screens |
| `↑` `↓`, `j` `k` | move the cursor; `pgup`/`pgdn` by a page |
| `enter` | act on the selected row — control, setpoint, or inspector |
| `i` / `p` | integrity poll / poll event classes 1, 2 and 3 |
| `s` | range scan a group |
| `t` / `T` | set the outstation clock (`T` measures the link delay first) |
| `u` / `U` | enable / disable unsolicited reporting |
| `R` | restart the outstation |
| `c` / `o` | close / open the selected binary output |
| `b` | write a deadband to the selected analog input |
| `S` | switch between select-before-operate and direct operate |
| `C` | edit the connection and reconnect in place |
| `/`, `esc` | filter the list; clear the filter |
| `<` `>`, `r` | change and reverse the sort column |
| `d` | the point inspector |
| `f` | follow the newest row |
| `:` | Files: type a directory to list |
| `l` | Files: list the directory again |
| `-`, `backspace` | Files: go up a directory |
| `w` / `W` | Files: save to local disk / send a local file |
| `D` | Files: delete the file on the device |
| `x` | clear the current list |
| `e` | export the current list as CSV |
| `?` | the full reference |
| `q` | quit |

Use `T` rather than `t` on a serial link: it measures the turnaround with
DELAY_MEASURE and writes a time already corrected for one-way transit. Over a
slow link, `t` leaves the outstation's clock late by tens of milliseconds and
puts every event it stamps into the past.

## Mouse

Click a tab, a row, a column heading or a footer button; click a selected row
again to act on it, right-click one for the inspector, scroll with the wheel,
and drag the scrollbar. `-mouse=false` turns it off.

Every click resolves to the key the keyboard would have pressed, so the two can
never drift apart.

## Controls are deliberate

Controls are the reason to be careful, so the tool is.

`enter` on an output opens a dialog naming exactly what will be sent,
**select-before-operate by default**, with a confirmation before anything moves.
The select is the outstation's chance to refuse before plant moves, and a failed
select is never followed by an operate.

`-direct` and `-no-confirm` turn that off for the devices and situations that
need it. While `-no-confirm` is in effect **the toolbar says so** — it is the one
mode where no dialog appears to say it for itself.

`S` toggles between select-before-operate and direct operate at runtime.

## Editing the connection while it runs

`C` opens an editor for the address, the two link addresses, the response
timeout and the poll interval. Applying it tears the session down and brings a
new one up in place.

That exists because a link address read off a drawing is a guess until something
answers, and restarting the tool to try 11 instead of 10 is how ten minutes of
commissioning becomes an afternoon.

Pointing somewhere new drops the point table with it. Those measurements came
from a different device.

## Export

`e` writes what is on screen — **after the filter and the sort**, not before —
to a timestamped CSV in the working directory. The operator exports the view
they were looking at rather than something they have to reconstruct.

| Screen | File | Columns |
| --- | --- | --- |
| Points, Overview | `dnp3-points-<stamp>.csv` | `type,index,value,quality,timestamp,timestamp_quality,received,updates,events,source` |
| Events | `dnp3-events-<stamp>.csv` | `received,type,index,value,quality,timestamp,class,source` |
| Log | `dnp3-log-<stamp>.csv` | `time,level,message` |

`source` is the group and variation the value arrived under, e.g. `g30v5`.

## Reading it as example code

This is the largest of the four programs and the one most worth reading if you
are building a UI on the library.

| File | Shows |
| --- | --- |
| `conn.go` | the session lifecycle, running the DNP3 session in its own goroutine, demo mode over `channel.Pipe` |
| `model.go` | the Bubble Tea model: point state, event ring, sorting and filtering |
| `view.go`, `layout.go`, `theme.go` | rendering, and a layout computed from the terminal size |
| `link.go`, `mouse.go` | resolving a pointer position to the key the keyboard would have pressed |
| `export.go` | CSV export of the current view |

The architecture worth copying: **the DNP3 session runs in its own goroutine and
never touches the model.** Every action is a `tea.Cmd` that returns a result
message, so a poll that takes five seconds costs five seconds of that one
command and nothing else.

Key handling is a plain `HandleKey(string)`, and the pointer resolves against a
layout computed from the terminal size. That is what lets the whole interface —
clicks included — be driven from tests without a terminal, which is how
`explorer_test.go` works.

## Troubleshooting

**Connects but the point table stays empty.** The link addresses. `-remote` must
match the outstation's own address. Press `C` and try the neighbouring values —
that is what the connection editor is for.

**Points fade after 30 seconds.** That is `-stale` marking them as not recently
updated. Raise it, or set `-stale 0`, for a device polled slowly.

**A control says the point does not support it.** The outstation refused —
`NOT_SUPPORTED` usually means the index has no control point, or an interlock
rejected it. The log screen has the exact status.

**Analog values look truncated.** The outstation is reporting them in an integer
variation. Nothing the explorer can fix; it shows what arrived.

**The terminal is left in a strange state after a crash.** Run `reset`. Use
`-inline` to avoid the alternate screen entirely.

## See also

- [`dnp3-outstation`](../dnp3-outstation/README.md) — a device to point it at
- [`dnp3-master`](../dnp3-master/README.md) — the headless equivalent
- [User guide](../../docs/user-guide.md) · [API reference](../../docs/api.md)
