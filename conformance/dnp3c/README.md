# dnp3c conformance

[dnp3c](https://github.com/stepfunc/dnp3c) is Step Function I/O's
implementation of the DNP Users Group's *DNP3 IED Certification Procedure*,
revisions 3.1 rev 1 (subset levels 1 to 3) and 2.7 (subset level 2). This
directory runs it against go-dnp3's outstation.

The Go tests in `conformance/` are written by the same hands as the outstation
they test, so they share its reading of the standard. dnp3c is an independent
reading of the certification procedures, and it is the reason this directory
exists. A pass here is still not certification. A failure is worth reading.

```
make dnp3c        # 3.1r1 at subset level 3, every procedure
make dnp3c-27     # 2.7 at subset level 2

conformance/dnp3c/run.sh 3.1r1 8.11      # one section or procedure, stopping at the first failure
KEEP_GOING=1 conformance/dnp3c/run.sh    # past failures, with a summary at the end
```

The report is `results/<revision>/L<level>/report.html`. Each procedure has its
own log, with every frame sent and received, decoded layer by layer. With
`KEEP_GOING`, `summary.txt` next to the report lists each outcome on one line.
The DUT's own log is `results/dut-<revision>.log`.

It needs Go, and Java 17 with Maven or Docker. Without `java` and `mvn` on the
PATH, the plugin is built and run in the `maven:3.9-eclipse-temurin-17` image,
on the host's network. `DNP3C_M2` names the Maven repository to mount, and
defaults to `~/.m2`.

A full run waits out every timeout the procedures test, and takes about an
hour per revision. Two revisions can run at once on different addresses:
`DNP3C_HOST=127.0.0.2 DNP3C_CONTROL_PORT=20081` moves the second one off
`127.0.0.1:20000` and control port 20080.

## How it fits together

```
 dnp3c (JVM)                             dut (Go)
 ┌───────────────────────┐   DNP3/TCP   ┌──────────────────────────┐
 │ certification         │◀────────────▶│ go-dnp3 outstation       │
 │ procedures            │  :20000      │ 127.0.0.1:20000          │
 │          │            │              │                          │
 │          ▼            │  JSON/HTTP   │ control: restart, events,│
 │ plugin/ (shim)  ──────┼─────────────▶│ settings, checks         │
 └───────────────────────┘  :20080      └──────────────────────────┘
```

dnp3c drives the outstation over DNP3. Some steps can't be done over DNP3:
cycling power, making an input change, checking that an output really
operated, or checking that the events in a response are the ones that were
generated. dnp3c hands those to an *integration plugin*, which has to live in
its JVM.

- **`plugin/`** is that plugin, in Scala. It holds no test logic. Each
  `IntegrationPlugin` call becomes one JSON request to the DUT. The DUT's log
  lines go into the procedure's log, and its error, if any, fails the step.
- **`dut/`** is the device under test: a go-dnp3 outstation, plus a control
  endpoint that carries out each plugin call. It generates events and
  remembers them in batches, one per call, which is how the event checks know
  what to expect. It also compares what dnp3c parsed with the outstation's
  database. Its unit tests, in `go test ./...`, cover the checks against the
  reports they must reject. A check that passed wrongly would turn a failing
  outstation into a passing report.
- **`device.ini`** describes the DUT to dnp3c: addresses, point counts,
  classes, timeouts, output roles, attributes. It and `dut/device.go` describe
  the same device and have to agree.

Each procedure starts from a freshly started outstation with its default
settings. As on a real device, a setting the procedures change (link confirms,
self-address, unsolicited responses and their timeout, retries and master)
takes effect when power is next cycled, which is how the procedures use them.
Uninstalling points and changing the fragment size restart the outstation at
once.

## The device

| | |
| --- | --- |
| Addresses | outstation 1024, master 1 |
| Points | 20 of each: binary and double-bit inputs, counters, frozen counters, analog inputs, binary and analog outputs |
| Classes | binary inputs 0–15 and double-bit inputs in class 1 (binary 16–17 in class 2 and 18–19 in class 3, for Assign Class), counters and frozen counters in class 2, analog inputs in class 3 |
| Event variations | g2v2, g4v2, g22v1, g23v1, g32v1 |
| Event buffer | 1000 events, shared by every type |
| Outputs | every binary output takes every control code; every analog output every g41 variation; at most 4 controls in a request |
| Clock | NEED_TIME 10 s after each clock write; unsynchronized 2 s after that |
| Timeouts | select 2 s, application confirm 2 s, unsolicited confirm 2 s, link 1 s with 3 retries |
| Unsolicited | off until a procedure enables it; retries unlimited unless set |

## Results

With dnp3c 0.2.0, on 2026-10-18:

| Revision | Passed | Failed | Excluded | Skipped |
| --- | --- | --- | --- | --- |
| 3.1r1, subset level 3 | 183 | 0 | 0 | 45 |
| 2.7, subset level 2 | 150 | 0 | 0 | 9 |

A procedure is skipped when it's for a feature the device lacks, by its
`supports.*` option in `device.ini`: output events, for example, and a
configurable fragment size.

## What it found

The first runs found seventeen places where the outstation answered other than
the procedures expect. All are fixed, each with a test in
`conformance/certification_findings_test.go` that names the procedure that
found it.

| | Was | Now | Found by |
| --- | --- | --- | --- |
| F1 | A command to an output the device lacks answered NOT_SUPPORTED alone | Also PARAMETER_ERROR, without reaching the handler; `OutputPoints` lets a handler say which outputs exist | 8.2.1.2.3, 8.2.1.2.10 |
| F2 | An OPERATE after the select timeout answered NO_SELECT | TIMEOUT | 8.2.1.2.4 |
| F3 | No limit on the controls in one request | `Config.MaxControlsPerRequest`; past it, TOO_MANY_OPS | 8.2.4 |
| F4 | IIN1.1–1.3 counted events already sent | Only events not yet sent | 8.5.2.2.1, 8.26.2 |
| F5 | Static data came before events | Events first | 8.5.5.2.2 |
| F6 | A broadcast's IIN1.0 never asked for confirmation | Asks for a broadcast to 0xFFFE, and stays set until confirmed. The link layer's names for 0xFFFD and 0xFFFF were swapped too | 8.6.5b |
| F7 | A read replacing an unfinished multi-fragment response handed its own events back: re-sent, and EVENT_BUFFER_OVERFLOW never cleared | A new request abandons the old response before it is acted on | 8.6.6.2.1 |
| F8 | A read arriving while a response with events awaited confirmation was never answered | Answered | 8.17.2c |
| F9 | Cold restart support could not be turned off | An Application implementing `RestartSupport` can refuse either restart | 8.8.2 |
| F10 | Unsolicited retries repeated the first attempt's IIN | The same objects and sequence number, with the current IIN | 8.11.2a |
| F11 | Reads were answered while an unsolicited response awaited confirmation | Deferred until the confirmation or its timeout; no unsolicited response while a solicited one is in progress | 8.11.2.5 |
| F12 | ENABLE_UNSOLICITED was accepted with unsolicited responses off | NO_FUNC_CODE_SUPPORT | 8.11.2e |
| F13 | Reads of points or types the device lacks answered silently | PARAMETER_ERROR past the last point, OBJECT_UNKNOWN for a type with none | 8.13.2d, 8.22.1, 8.23.2 |
| F14 | A clock set once was trusted forever | `Config.Clock`: NEED_TIME again after `SyncInterval`, unsynchronized after `Holdover` | 8.14.2m, 8.15.3 |
| F15 | A freeze raised events only for changed values | Every frozen point raises one | 8.16.2.2.3 |
| F16 | Reads of g1v1, g3v1 and g10v1 returned nothing | Reported packed | 8.24.2a |
| F17 | Variations without flags hid abnormal flags | Such points are reported with flags | 8.24.2d |

Three of these change what an existing application sees. A command for an
output past the database's output status points is now refused before the
handler is asked (F1): a handler that serves outputs with no status point
should implement `OutputPoints`. A freeze now always raises frozen-value
events (F15). And a point whose flags are not just ONLINE comes back with
flags from a read of a flagless variation (F17).

## Licensing

dnp3c is GPL-3.0-only, and the plugin links against it. The plugin's own code is
GPL-3.0-or-later, like the rest of this repository. The combination is
distributed under GPL-3.0. Nothing from Step Function I/O's own dnp3c plugin,
for their proprietary-licensed `dnp3` library, was used. The plugin is written
against dnp3c's `IntegrationPlugin` trait and its documentation.
