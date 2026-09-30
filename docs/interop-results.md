# Interoperability results

What was run against opendnp3, when, and what that does and does not show. The
suite in `interop/` is the thing to rerun; this records one run of it.

| | |
| --- | --- |
| Date | 2026-09-30 |
| go-dnp3 | branch `claude/cool-johnson-ece4uk` at the commit that added this file |
| opendnp3 | 3.1.2 (`26b4c01`), the tag `interop/Dockerfile.opendnp3` pins |
| Run | Natively, not in a container: the machine had no Docker daemon |

## How this run differed from `make interop-build`

The Dockerfile builds opendnp3 in a container. This run built the same source
with CMake directly, and two of its dependencies came from somewhere else:

- **asio**: Debian's `libasio-dev` 1.28.1 instead of the 1.16.0 the CMake file
  downloads. The download was refused by the network policy. opendnp3 built and
  ran against it, but it is not the exact dependency the container uses.
- **ser4cpp** (`3c44973`) and **exe4cpp** (`fb878a4`): fetched with `git` at the
  exact commits the CMake files name.

The test suite starts the peer with `docker run`, so a small `docker` shim on
`PATH` ran the demo binary and forwarded the port instead. None of that touches
the protocol, but it means "passed in the container" has not been shown.

## Our master against opendnp3's outstation

`go test -tags interop -v ./interop/`: all five pass.

| Test | Result |
| --- | --- |
| `TestIntegrityPoll` | 10 binaries, 10 analogs, 10 counters; `IIN=NEED_TIME` |
| `TestControls` | select-before-operate on index 3 and a float analog output on index 1: `SUCCESS` |
| `TestClassPolls` | pass |
| `TestClockSync` | pass |
| `TestNoDecodeErrors` | 10 of each including 10 octet strings; no decode errors |

## opendnp3's master against our outstation

Following `interop/reverse.sh` by hand: integrity scan, a CROB, an event scan.
This direction is observation, not assertion: the log was read, not scripted
against. opendnp3's master parsed every object our outstation sent, with no
error, warning or malformed-data message:

g1v2, g2v2, g10v2, g11v2, g12v1, g20v1, g21v1, g22v5, g30v5, g32v7, g40v1,
g40v3, g42v3, g42v7, g80v1.

## What this does not show

- **File transfer (g70) and device attributes (g0).** opendnp3 3.1.2 implements
  neither, so no run against it can cover them, and no other implementation has
  been tried.
- **Anything added since.** Command events (g13/g43), `FREEZE_AT_TIME`, event
  reads by group and variation, and relative-time events with a CTO were not
  exercised against opendnp3.
- **Link-layer confirmations, serial, UDP and TLS.** The run was plain TCP with
  unconfirmed link frames.
- **Any other vendor's device.**
