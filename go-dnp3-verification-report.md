> **STALE — superseded (re-checked 2026-10-17 at `f8152c7`).** This report
> reviews `51337b3`, and many of its findings no longer describe the code.
> Re-verified as resolved: F-1 (unsolicited events are released on close),
> F-3 (a file short of its size is FileFatal), F-10 (`make fuzz-short` passes,
> all 14 targets run), F-11 (`go vet -tags interop` is clean), F-13 (one-way
> delay is `max((rtt-processing)/2, 0)`), F-24 (CI pins golangci-lint v2),
> F-59 (`generate-check` runs in CI). The headline "multiple transport
> segments per link frame" finding does not apply: opendnp3 also processes
> one segment per link frame. Do not act on anything here without checking
> it against the current tree.

# go-dnp3 — Implementation Verification Report

**Repository:** `github.com/dscsystems/go-dnp3` (`/home/ricardolo/Projects/go-dnp3`)
**Commit reviewed:** `51337b3` — "Fix control codes and other fixes."
**Branch:** `master`, clean working tree
**Date of review:** 2026-09-29
**Scope:** whole library — `internal/link`, `internal/transport`, `internal/stack`, `internal/app`, `objects`, `master`, `outstation`, `channel`, `multidrop`, `decoder`, `cmd/*`, `conformance`, `interop`, docs, CI gates

---

## ⚠️ Read this before using this report

**1. This report is INCOMPLETE — two of nine audits never finished.**

Coverage is missing for the object-group layer (every group/variation layout
against IEEE 1815-2012 §11) and for the outstation role end to end. Both areas
are among the least-tested in the repository (`outstation` 27.4% coverage, and
the `objects` package's golden-hex anchors cover 6 of 113 variations).

**Treat those two areas as UNASSESSED, not as clean.** No finding below implies
they are sound, and the absence of a finding for, say, a group 30 variation is an
absence of evidence. See §4.4 for what is known incidentally and what is not
covered at all. Before acting on this report, re-run those two audits.

**2. IEEE 1815-2012 was never directly readable — it is paywalled.**

No auditor in this review had the normative text. Every standards claim here
rests on cross-reference to opendnp3 3.1.2, the Wireshark dissector, Chipkin's
published tables, or an independent reimplementation. **No clause numbers were
invented**, and findings resting on reference-implementation behaviour rather than
on the standard are marked LIKELY or UNCERTAIN in the source audits.

Practical consequence: "the code does not match opendnp3" is *not* the same as
"the code violates the standard." In at least three places the two deliberately
differ and **go-dnp3 is the stricter/correct one** — broadcast handling, FCB on
sec-to-pri frames, and `TEST_LINK_STATES` handling. Do not "fix" those toward
opendnp3.

**3. This is a review of a moving target.** The repository is under active
defect-hunt development, with a named regression test accompanying nearly every
recent fix. Findings may already be fixed at a newer commit. The commit under
review is `51337b3`.

**4. Nothing here was executed against a real device or a live DNP3 peer.**

The interop suite was **compiled** (it does not compile) but never run — no Docker
in the review environment. Fuzzing and lint findings were reproduced on pristine
copies of `HEAD` under `/tmp`. Serial links were never exercised against hardware.
Pass/fail against a real DNP3 device is unmeasured.

**5. The defects below were found by auditing, not by the project's own test
suite.** §3.1 explains why. The practical implication is that **fixing these
will not raise the project's confidence in its tests** — the oracles need
independent work too (§6 steps 10–16).

---

## 1. Method

- Seven independent audits, each reading its layer in full and cross-checking
  against **opendnp3 3.1.2 sources** (the version `interop/Dockerfile.opendnp3`
  pins), the **Wireshark `packet-dnp3.c` dissector**, the **Chipkin DNP3 Quick
  Reference / AN2013-004b** validity tables, the **published DNP3 group-0
  attribute table**, and an independent Python CRC-16/DNP implementation.
- Every reported defect was reproduced with a concrete byte sequence or a
  throwaway program. Findings requiring it carry a reproduction.
- Findings are marked **CONFIRMED** (reproduced), **LIKELY** (code path verified,
  normative text unavailable), or **UNCERTAIN** (genuinely unresolved).
- **IEEE 1815-2012 is paywalled.** No auditor had normative clause text. Where a
  claim rests on the standard rather than on a reference implementation it is
  marked UNCERTAIN. No clause numbers are invented.
- Baseline tool runs (§2) were executed directly.

## 2. Baseline results

| Check | Result |
|---|---|
| `go build ./...` | pass |
| `go vet ./...` | pass, clean |
| `gofmt -l .` / `gofmt -s -l .` | clean |
| `go test ./...` | **pass** — 16/16 packages |
| `go test -race ./...` | **pass** — 16/16 packages, no races |
| `go test -tags interop -c ./interop/` | **FAIL** — 4 compile errors (F-2) |
| `make fuzz-short` | **FAIL** at target 4 of 14, in 0.07 s (F-1) |
| `golangci-lint run` (v2.13.2) | **FAIL** — 6 issues; CI pins v1.65.0 against a v2 config (F-9) |
| `make generate` (into a /tmp copy) | clean — output byte-identical to committed |
| Coverage (`-coverpkg=./...`) | **76.5%** total. `objects` 96.2%, `internal/transport` 91.5%, `multidrop` 92.6%, `internal/link` 85.1%, `internal/app` 71.6%, `internal/stack` 69.9%, `channel` 68.7%, root `dnp3` 53.6%, `outstation` **27.4%**, `master` **6.7%** |
| Dependency hygiene | 4 direct / 15 indirect, all permissive, no `replace`, no vendor tree |
| Licence (GPLv3) claim | consistent across `LICENSE`, `NOTICE`, `dnp3.go`, `cmd/*/main.go` |

The library is well built. Its git history is a deliberate defect-hunt campaign
(`Fix: Unsolicited retries can lose newly queued events`, `Fix: Application
fragment state is not validated`, `Fix: Invalid broadcast link frames are
accepted and answered`, `Fix: Reserved source addresses are not rejected`, …),
each fix accompanied by a named regression test. Zero `TODO`/`FIXME`/`HACK`
comments and zero commented-out assertions in the tree.

## 3. Bottom line

**The protocol core is in better shape than its own hedging implies, and worse
than its documentation claims about verification.**

The link layer is genuinely correct — CRC verified three independent ways, the
control-code validity matrix exact, FCB retransmission semantics right, broadcast
and reserved-source handling stricter than opendnp3. `flags.go` (the highest-risk
constant table, since a wrong bit silently corrupts every measurement) is exactly
right against both Wireshark and opendnp3, including the LSB-first octet
convention. TLS is sound: mandatory CA, `RequireAndVerifyClientCert`, TLS 1.2
floor, no `InsecureSkipVerify`, real-certificate tests. No goroutine or fd leaks.
The generated codecs are in sync with the spec.

What it lacks is **breadth of evidence, and several real defects that reproduce**:

- **11 HIGH defects**, of which the most serious cause **silent data loss**
  (unsolicited events destroyed on both ends), **silent data corruption** (a
  truncated file reported as written successfully), and **controls applied to
  the wrong physical point** (index truncation).
- **A framing-layer defect beneath everything else**: multiple transport
  segments in one link frame are not parsed, so a conforming peer that packs
  segments has requests silently swallowed and phantom objects fabricated.
- **Every quality gate that looks like verification is broken or unrun**: fuzz
  red, lint job pinned to a config it cannot parse, interop suite does not
  compile and is in no CI job, `generate-check` unwired.

### 3.1 The cross-cutting theme: the oracles are the weakest layer

This is the single most important finding, and it explains most of §4.

The repo's discipline is *fix a defect, add a regression test*. That works well
for the implementation and poorly for the tests themselves:

- **`make fuzz-short` is red.** `internal/link/fuzz_test.go:208-211` asserts "a
  primary frame always gets a reply", but `secondary.go:82-84` **deliberately**
  discards frames whose FCV/function combination is invalid, without a reply —
  behaviour introduced by commit `4f5321f` and asserted by `internal/link/fsm_test.go`.
  The fix and the fuzz oracle contradict each other and the oracle was not
  updated. Targets 5–14 never run. **CONFIRMED** (both the master and tooling
  audits hit it independently).
- **The conformance suite has no independent oracle.** It decodes the outstation's
  replies with the same `app.ParseResponse` and the same `objects` tables the
  outstation encoded them with. Of 66 tests, **5 are anchored to a literal byte
  string**; the only real external anchors in the tree (6 golden-hex cases) live
  in `objects/`. A shared misreading of the standard is invisible to the rest.
- **`interop/` does not compile** and runs in no CI job. Its reverse direction is
  a `grep` pipeline with no assertion constructs that cannot fail.

Put plainly: **the defects below were found by auditing, not by the suite.** The
suite is a bug-regression suite wearing a conformance label.

## 4. Findings

Severity: **CRITICAL** (silent data loss/corruption, wrong physical point acted
on) · **HIGH** (unrecoverable state, protocol violation) · **MEDIUM** ·
**LOW** · **NIT**.

> **Scope of §4: seven completed audits only.** Object-group layouts (§11) and
> the outstation role are **not covered** — see the notice at the top of this
> report and §4.4. A finding below is evidence of a defect; the *absence* of one
> in an unaudited area is not evidence of correctness.

### 4.1 CRITICAL / HIGH

---

**F-1 · HIGH — Unsolicited events are silently destroyed; the bug exists on both ends.**
*CONFIRMED, two independent audits, opposite ends of the same path.*

`outstation/unsolicited.go:152` selects up to 64 events → `:216` `body := bodies[0]`
(extra fragments discarded; the comment at `:213-215` claims "the rest stay
queued") → `:273` `s.db.events.Confirm()` confirms **every selected event**.

Reproduction: 60 class-1 points, `MaxTxFragment: 40` → 4 objects sent, 56 events
gone, no indication.

The master side is independently broken (`master/session.go:669-687`): no FIR/FIN
series state on unsolicited fragments, and dedup by sequence number only, so a
multi-fragment unsolicited response is either split into several bogus
transactions or silently truncated mid-series.

Neither bug is visible from either side alone.

**Fix.** Bound the selection to what the builder can carry, or transmit the full
`bodies` slice as a real series, or `Unselect()` everything not in `bodies[0]`
before sending. Master: track FIR/FIN series state and detect duplicates by "FIR
on a series already in progress", not by sequence equality.

---

**F-2 · HIGH — Multiple transport segments in one link frame are never parsed.**
*CONFIRMED.* A framing-layer defect, so it sits beneath every other finding.

`internal/stack/stack.go:340` hands the **entire** link user-data payload to
`Reassembler.Accept`, which by contract takes **one** segment (`transport.go:308`).
Nothing splits the payload. The same single-segment assumption is baked into
`decoder/decoder.go:204` and `multidrop/conn.go:279`.

Reproduction — two complete requests in one frame
(`C1 C1 01 3C 01 06 60 01 00 C2 C2 01 3C 01 06 60 01 00`): one request answered,
the second swallowed, and a phantom `g96v1 [194..194]` object fabricated from the
second request's header octets.

**Fix.** Loop the payload, splitting segments by header, and feed each to `Accept`.
Add a shared `Segments(payload, fn)` helper to the transport package so all three
call sites use one rule. opendnp3 never packs segments when *sending*, so the
interop suite cannot catch this.

---

**F-3 · HIGH — A file write is committed and reported successful regardless of what arrived.**
*CONFIRMED.*

`t.size` is set from the master's declared size (`outstation/file.go:483`), echoed
back at `:436`, and **never read at close**. `onCloseFile` (`:565-575`) closes the
writer and returns `FileSuccess` unconditionally. Meanwhile
`master/fileapi.go:189` sends CLOSE_FILE on *any* failure — and close is the
commit point. There is no `AbortFile` on the master at all (grep: zero
references).

Reproduction: `WriteFile(declared 1000, sent 100)` returns `nil`, leaves a
100-octet file. `O_TRUNC` in `OpenWrite` (`:247`) means an OPEN+CLOSE with no
blocks also truncates an existing file and is told it succeeded.

**Fix.** Track octets accepted; on close of a write, if `written != t.size` or the
last-block flag was never seen, answer non-success (`WRITE_BLOCK_SIZE`/`FATAL`).
Add `Session.AbortFile` and send ABORT_FILE — not CLOSE_FILE — on write failure.

---

**F-4 · HIGH — Command and deadband indexes above 255 are truncated to one octet.**
*CONFIRMED on the wire.* A control is applied to **the wrong physical point**,
silently.

`master/command.go:189` does `data = append(data, byte(c.Index))` with
`PrefixIndex1`; `master/task.go:273` does the same for `WriteDeadband`.

Reproduction: 400 binary-output-status points —
`m.DirectOperate(ctx, master.LatchOn(300))` operates point **44**.
`m.WriteDeadband(ctx, map[uint16]float32{300: 1.5})` sets point 44's deadband.

The asymmetry is stark: `outstation/response.go:498-501` picks the index-prefix
width dynamically (1/2/4 octets) from the max index, while the master hardcodes
one octet. The outstation can address 32-bit indexes; the master cannot address
anything above point 255 on any write path.

**Fix.** Choose the prefix width from the maximum index in the run — the same
choice `app.ReadRange` already makes — and add an `ErrBadConfig` check on index
width in `WriteDeadband`.

---

**F-5 · HIGH — `WRITE g50v1`/`g50v3` with a legal index-prefixed qualifier sets a garbage clock.**
*CONFIRMED on the wire.* `outstation/session.go:765-798` decodes `h.Data`
directly, but `ObjectHeader.Data` **includes** per-object index and size prefixes
(`objheader.go:41-44`). The command path (`command.go:248-262`) and the deadband
path (`session.go:830-844`) handle this correctly — **only the time paths do not.**

Qualifiers `0x17` and `0x28` are both standard-defined for a write, and
`objectDataLen` sizes them correctly, so the frame parses and the handler gets the
wrong octets:

```
WRITE, 0x17 → clock set to 7428-05-30 18:28:09.344 UTC (wanted 2026-03-01)
        IIN = DEVICE_RESTART — no PARAMETER_ERROR
WRITE, 0x00 → correct
```

A corrupted clock then poisons every timestamp the outstation ever reports, and
the master is told the write succeeded.

**Fix.** Skip `h.Qualifier.IndexPrefix().Octets()` per object before `ParseTime48`,
mirroring `command.go:248`, or reject a non-zero prefix on a fixed-size write with
IIN2.2.

---

**F-6 · HIGH — A request whose object section fails to parse is answered with nothing, forever.**
*CONFIRMED.* `outstation/session.go:508-517`: on `ParseFragment` error the session
sets `IINParameterError` and `return nil` — no response, ever. The indication is
deferred to an unrelated later response.

```
WRITE g99v7 → fragments=0, malformed=1, unknownFunc=0
  the *next* read answers with the error bit
```

A master that writes an object this outstation does not implement times out and
retries forever. Same for a reserved qualifier, and for a **perfectly conformant
`FREEZE_AT_TIME`**, which trips the documented `CarriesObjectData` limitation —
answering nothing instead of the `NO_FUNC_CODE_SUPPORT` the docs promise.

opendnp3 sends a null response carrying the mapped indication whenever the APDU
header parsed (`OutstationContext.cpp:694-703`).

**Fix.** When `ParseHeader` succeeded, answer with a null response carrying
`IINObjectUnknown` for `ErrUnknownObject` and `IINParameterError` otherwise. Test
the function code for support *before* walking objects.

---

**F-7 · HIGH — Two independent paths latch link state and wedge the session permanently.**
*CONFIRMED, two audits, unrelated mechanisms, identical outcome.*

1. `internal/link/primary.go:194` sets `p.dfc = f.Header.Control.Dfc()`
   **before** the state switch, and `stack.drain` (`stack.go:315-319`) calls
   `OnFrame` for any frame from `s.dest` regardless of whether anything is in
   flight. A stray secondary frame with FCV set — which on a sec-to-pri frame
   *is* the DFC bit — latches flow control on an idle link. The only escape is
   `RequestLinkStatus`, which the master gates on `KeepAlive > 0` (never
   defaulted, so off) and the outstation never calls. `Send` then returns
   `ErrDataFlowControl` forever.
2. `master/session.go:390` — a keep-alive probe that goes unanswered leaves
   `stack.awaiting` set (`Stack.OnTimeout`'s `default:` arm returns without
   clearing it), so every later `SendTo` fails with
   `"a fragment is already in flight"`.

Reproduction for (2): an in-memory peer that accepts the connection and never
answers. `Connected()` stays `true`, `Run` never returns, no reconnect ever
happens, `ResponseTimeout` stops moving, and the error matches **none** of the
documented sentinels. This is the serial configuration the docs recommend.

**Fix.** Gate the `p.dfc` assignment on a non-idle state (or only when
`s.awaiting`); have `checkKeepAlive` skip when work is queued and end the
connection on give-up so `Run` reconnects; clear `awaiting` on any non-`ActionTransmit`
result; treat "already in flight" as requeue, not failure.

---

**F-8 · HIGH — `DEVICE_RESTART` silently deletes every periodic scan.**
*CONFIRMED.* `master/session.go:257` calls `s.sched.clear()` inside the startup
sequence, reached from `observeIIN` (`:586`). `scheduler.clear`
(`master/task.go:328-333`) empties the heap unconditionally, and a periodic task is
only re-queued **after it runs** (`:519-526`). Any periodic scan pending at that
moment is gone permanently — no error, no counter.

Reproduction: a 60 ms class poll running steadily (11 requests in 600 ms), then
`out.Restart()`:
```
before restart: 11 requests
just after:     13   (clear-restart + integrity)
+900 ms later:  13   <-- periodic scan is gone
```

**Fix.** `scheduler.clear` should skip or re-arm tasks with `period > 0`, failing
only one-shot (`period == 0`) tasks.

---

**F-9 · HIGH — A transport RESET is accepted as a zero-length fragment; no reset is ever sent.**
*CONFIRMED.* `0xC0` is both the transport-reset marker and a legal header-only
`FIR|FIN` segment. `transport.go:316-338` has no reset check, so it produces an
empty application fragment and hands it upward. Mid-fragment it is misattributed
as `DiscardUnexpectedFIR`.

Symmetrically, **nothing ever generates a transport reset**: on a sequence
violation (`transport.go:351`) or overflow (`:363`) the stack discards locally and
`stack.deliver` **ignores `res.Discarded` entirely** (`stack.go:339-350`). The peer
keeps transmitting into an abandoned sequence space and is never told.
`grep -rn '0xC0'` finds only test fixtures.

**Fix.** Detect a single-octet `0xC0` before parsing as a segment; `Reset()`, count
it, surface it. Have `deliver` inspect `res.Discarded` and emit a `0xC0` user-data
frame on violation.

---

**F-10 · HIGH — `make fuzz-short` is red; 10 of 14 fuzz targets never run.**
*CONFIRMED by two audits, one reproducing on a pristine copy of HEAD.* See §3.1.
CI's `fuzz` job runs `make fuzz-short`, so CI is red on this too.

**Fix.** Amend the invariant to `&& validControl(frame.Header.Control)` (or
`&& !res.Discarded`) at `fuzz_test.go:208-211`, seed the corpus, re-run to
completion.

---

**F-11 · HIGH — The interop suite does not compile and runs in no CI job.**
*CONFIRMED.* `interop/interop_test.go:109,117,125,133` — four
`cannot use v.Index (type uint32) as uint16 value in map index` errors. It was
missed by the `uint16`→`uint32` index migration described in `docs/api.md:138-141`
and is invisible to `go build`/`vet`/`test` because of the `//go:build interop` tag.

```
$ go test -tags interop -c ./interop/
interop/interop_test.go:109:12: cannot use v.Index ... (×4)
$ grep -rn interop .github/   →  no matches
```

This is the **only** evidence for the strongest claim in the docs
(`docs/device-profile.md:210-215`, `README.md:97`), and it has not run since the
migration. `TestMain` also returns without calling `m.Run()` when Docker is
absent, so `make interop` **exits 0 on a machine with no Docker**.

Compounding: opendnp3 3.1.2 has **no group-70 codec and no group-0 implementation
at all** (nor does pymodbus have DNP3). So the interop claim could never have
covered file transfer or attributes — precisely the two areas now known to be
wrong (F-3, F-15).

**Fix.** `uint16` → `uint32` on the four map keys. Add the interop package to
`go vet ./...` (e.g. a `//go:build interop` vet target) so it cannot rot again.
Wire interop into CI or state plainly that it is manual.

---

### 4.2 MEDIUM

| ID | Finding | Location | Note |
|---|---|---|---|
| **F-12** | Data race on `ChannelHandler.Dropped()` — unguarded `h.dropped++` on the session goroutine vs an unlocked read. SKILL.md rule 4 and `user-guide.md:224-226` both tell users to call it from a consumer. | `master/handler.go:134,143` | `go test -race` never exercises both concurrently, so `make check` stays green. Make it `atomic.Uint64`. |
| **F-13** | `SyncTimeWithDelay` applies the one-way transit correction with the **wrong sign** and without the standard's negative-delay mechanism, writing plain g50v1. Net effect: the outstation's clock lands *later* than uncorrected by 2× the transit — the opposite of what the comment, `docs/api.md` and SKILL.md promise. | `master/api.go:136-142` vs its own comment at `:121-124` | g50v3 is not implemented in this repo; `internal/app/zz_generated_sizes.go:104` sizes it as 48 bits and misnames it. Interim fix `Add(-oneWay)`; real fix is g50v3 with the negative-delay flag. |
| **F-14** | A malformed response is reported to the caller as `ErrTimeout` and increments `ResponseTimeout` — the counter whose whole purpose is "the outstation did not answer". `dnp3.ErrMalformed` is never produced by the master. | `master/session.go:544-548` | Complete the task with a wrapped `ErrMalformed`; bump `FragmentsDiscarded`. |
| **F-15** | g70v2 (file authentication) is encoded and decoded wrongly: a spurious 12-octet constant and a 16-bit field, and the parser reads the username *length* as an offset. A standard-layout object is rejected outright. | `objects/file.go:32,64-70,83-89` | Round-trips cleanly against itself, which is why nothing caught it. The object is deprecated — either fix the layout or delete the codec and refuse g70v2 explicitly. |
| **F-16** | No block-level retry, and the outstation cannot resend — `onFileRead` refuses any block but the next with `BLOCK_SEQUENCE` and does not retain the previous block. No retry strategy can recover a transfer. | `outstation/file.go:722-727,758-781` | Related to the dead `TaskRetryPeriod` (F-22). Buffer the last block served and accept `t.block-1` idempotently. |
| **F-17** | A READ of an unsupported **variation** of a supported group returns an empty success with no indication — a master cannot distinguish "no such variation" from "empty database". | `outstation/session.go:710-717`, `response.go:203-205` | opendnp3 sets OBJECT_UNKNOWN here. |
| **F-18** | Group 0 objects with an index prefix are mis-walked — `walkAttributes` ignores the prefix entirely, so a fragment is mis-sized and rejected wholesale. `decoder/attribute.go:16-40` makes the same assumption, so fixing one leaves the other wrong. | `internal/app/objheader.go:145-146,181-195` | The one place in the file where the prefix is not accounted for. |
| **F-19** | The free-format exception is far wider than the case it exists for: **any** group with a size prefix, **any** function code, takes the self-describing path. It re-breaks the time writes (F-5) by a second route and hides unknown objects from `ErrUnknownObject`. | `internal/app/objheader.go:108-110` | Gate on the sizer reporting unknown, or an explicit allow-list (groups 0 and 70). |
| **F-20** | An unexpected FIR that abandons a zero-octet partial fragment is counted nowhere — `if r.assembly && len(r.buf) > 0` wrongly excludes the legitimate empty-partial case. Caller is told `DiscardNone` while a fragment was dropped. | `internal/transport/transport.go:325` | Test `r.assembly` alone. `transport_test.go:291` misses it because its first segment carries a payload octet. |
| **F-21** | No LEN ↔ function-code consistency check. `RESET_LINK_STATES` with 200 octets of user data is ACKed; `CONFIRMED_USER_DATA` with no payload is ACKed and the data **never reaches the transport layer** — the master is told it arrived. | `internal/link/frame.go:307-327`, `secondary.go:91-134` | opendnp3 checks this in `ValidateHeaderParameters`. |
| **F-22** | `Config.TaskRetryPeriod` is dead configuration — defaulted, documented as "how long to wait before retrying a failed task", read **nowhere**. There is no application-layer retry at all. | `master/session.go:29-30,76-78` | Also makes SKILL.md's "`ErrTaskFailed` // retries exhausted" false. Implement or delete. |
| **F-23** | `multidrop.Registry` over-release silently closes a shared bus other callers still hold — `Release` never detects a count going negative. The exact failure `Registry` exists to prevent. A foreign bus *is* correctly rejected, so the guard is in the wrong place. | `multidrop/registry.go:118-138` | Refuse `refs < 0`; consider keying on a per-`Open` token. |
| **F-24** | CI pins `golangci-lint v1.65.0` against a `version: "2"` config. v1 binaries have no v2 parser, so the lint job **cannot succeed**. The correct v2 binary reports 6 real issues (2 unchecked `f.Close`, 2 `SA4006`, `QF1012`, 1 unused). | `.github/workflows/ci.yml:41` vs `.golangci.yml:1` | `make lint` fails for the same reason; `make check` omits lint, so it is invisible locally. |
| **F-25** | `docs/device-profile.md:95` — "All fourteen defined bits are produced and interpreted." **False.** `IINLocalControl`, `IINDeviceTrouble`, `IINAlreadyExecuting`, `IINConfigCorrupt` are defined at `internal/app/iin.go:25,29,49,50` and set by **no code path in the tree**. Ten of fourteen are produced. | `docs/device-profile.md:95` | The device profile is what a certification reviewer reads first. State which ten are produced. |
| **F-26** | `docs/device-profile.md:22` — "Target conformance: Level 2 complete, most of Level 3, parts of Level 4" is not supportable. The certification procedure requires a subset device either to omit out-of-subset features or be **configurable to disable** them; level-3/4 objects are emitted by default with no switch to suppress them. Also internally contradicted by the doc's own known-gaps list. | `docs/device-profile.md:22`, `README.md:130-132` | Reword to object coverage + explicit statement that the subset-configuration requirement is not met. |
| **F-27** | SKILL.md Hard Rule 1 and its user-guide restatement assert "`Database()` is not safe for concurrent use … a race afterwards." **Empirically false** — two `-race` tests with four goroutines against a live outstation passed clean; every accessor takes `db.mu`. | `SKILL.md:134-137`, `user-guide.md:423-427` | An agent following this rule will add needless synchronisation and may report a nonexistent race. Keep the advice, fix the reason: batching is for **event consistency**, not memory safety. |
| **F-28** | The interop claim reads as a test result but is not: forward is build-tagged and skipped without Docker, reverse is a `grep` pipeline with no assertion constructs, no output is committed, and no CI job runs it. | `device-profile.md:210-215`, `README.md:97-104`, `interop/reverse.sh:33-38` | Commit captured logs with dates and the opendnp3 commit, or state plainly it is a manual grep-based check outside `make check`. |

### 4.3 LOW / NIT

| ID | Finding | Location |
|---|---|---|
| F-29 | A multi-command control request exceeding one fragment fails outright, executing nothing (~168 CROBs at the 2048 default). The standard requires the master to fragment. | `master/command.go:193-201`, `master/session.go:462-465` |
| F-30 | A cancelled `DirectOperate` can still operate the point — the task is submitted, the caller gets `ctx.Err()`, the session sends the command anyway. | `master/api.go:26-37` |
| F-31 | `LinkRetries` has no default, and 0 means "give up immediately". A user setting `UseLinkConfirms: true` and forgetting it gets zero retransmissions. | `master/session.go:54-55,198`; cf. `outstation/unsolicited.go:85-86` which defaults to 3 |
| F-32 | The restart response's g52v2 time delay is discarded, so `Restart` cannot tell the caller how long the device expects to be down. | `master/task.go:198-209` |
| F-33 | The master's response-series state records only "started", so a repeated **continuation** fragment is delivered twice. | `master/session.go:613-632` |
| F-34 | A CONFIRM carrying object headers is accepted and acted on (a CONFIRM is a 2-octet header with nothing after it). | `outstation/session.go:576-585` |
| F-35 | A request with CON set is processed; opendnp3 discards it outright. | `outstation/session.go:519-547` |
| F-36 | Qualifier legality is not enforced per object — `qualifier.go` composes prefix × range freely, so `0x1B`, `0x30`, `0x4B` are all "valid". A g12v1 control under `0x37` is **executed**. | `internal/app/qualifier.go`, `outstation/command.go:249` |
| F-37 | An object larger than the configured maximum is emitted anyway (g52v2, g50v3 never consult the limit). | `outstation/response.go:60-65`, `session.go:878-884,702-707` |
| F-38 | An unsolicited retransmission takes a **new** transport sequence number — the commit `6a9eefe` fixed the application sequence, not the transport one. | `outstation/unsolicited.go:187-202`, `transport.go:117-121` |
| F-39 | A link-layer `RESET_LINK_STATES` does not clear the transport reassembler (self-correcting, but books a spurious discard). | `internal/stack/stack.go:295-307` |
| F-40 | An over-long segment is accepted silently and uncounted; the segmenter has no fragment-size limit of its own. | `transport.go:309-317,142-162` |
| F-41 | Transport discard counters are **unreachable** in a live session — `Stack.Stats()` has no callers outside tests. The carefully separated per-reason counters are visible only in the offline decoder. | `internal/stack/stack.go:113-116` |
| F-42 | A secondary frame with FCB set is accepted by the primary; a `PRM=1, DIR=0` frame is executed. Both violate the sec-to-pri valid-control-code table; opendnp3 rejects them. Combined with F-7, an unauthenticated peer can drive link state. | `internal/link/primary.go:191-196`, `secondary.go:73` |
| F-43 | `REQUEST_LINK_STATUS` answered with `ACK` is recorded as a **successful** keep-alive; opendnp3 treats it as a failure. A live-but-wrong link looks healthy. | `internal/link/primary.go:242-252` |
| F-44 | `SendLinkStatusRequest` addresses `cfg.RemoteAddr` while its reply is filtered on `s.dest`. Not currently triggerable — one call site away. | `internal/stack/stack.go:374-387` |
| F-45 | The **link layer never asserts DFC**, contradicting the comment that justifies it: the transport above drops segments on overflow with no back-pressure signal. The master side implements DFC correctly, so the mechanism is one-sided. | `internal/link/secondary.go:168-170` vs `transport.go:361-365` |
| F-46 | UDP silently truncates an oversized datagram and the excess is unreported. Legal frames are safe (264 ≤ 292), so this affects only non-conforming peers. | `channel/udp.go:123-144` |
| F-47 | `Retry.Max` is not a bound (jitter applies after the cap → effective ceiling `Max×(1+Jitter)`), and `Min > Max` is never clamped. | `channel/channel.go:63-80` |
| F-48 | `dnp3-decode` caps per-line input but not total stdin. | `cmd/dnp3-decode/main.go:162-174` |
| F-49 | `--inject device-trouble` is advertised and banner-printed but asserts nothing — `DeviceTrouble` is read by no consumer. | `cmd/dnp3-outstation/main.go:344`, `sim.go:132` |
| F-50 | `--inject offline-every=DUR` fires at t=0 (zero `lastOffline`), so the device starts permanently comm-lost. | `cmd/dnp3-outstation/sim.go:161,210-217` |
| F-51 | Duration injections accept `0` and negatives silently, while `event-storm` correctly rejects them. A typo'd flag is a silent no-op behind a clean banner. | `cmd/dnp3-outstation/main.go:337-359` |
| F-52 | The function-code table omits codes **15–19** (INITIALIZE_DATA … SAVE_CONFIG), which are defined and answered with IIN2.0. | `docs/device-profile.md:63-89` |
| F-53 | `docs/api.md` claims completeness but omits **47** exported symbols (17 in `objects`, 30 in root including the whole `FileStatus` value set and the nine `Perm*` bits). Every signature it *does* document is correct. | `docs/api.md:3` |
| F-54 | `docs/api.md` states `dnp3.Version` is `0.1.0-dev`; `dnp3.go:32` says `0.5.3`. | `docs/api.md:14,46` |
| F-55 | `README.md:171` says "Five screens" and omits Files; there are six. | `README.md:171` |
| F-56 | `readTheFieldSomehow()` is an undeclared identifier in an otherwise-complete program, with no indication it is a placeholder. (Every other block in the file compiles.) | `user-guide.md:412` |
| F-57 | `TestDelayMeasure` asserts only that g52v2 is present and 2 octets; the value is hardcoded `[]byte{0,0}`. `TestUnsolicitedControlAccepted` asserts only the **absence** of two IIN bits, so a no-op handler passes both. | `conformance/conformance_test.go:323,564`; `outstation/session.go:882` |
| F-58 | `master/` has **no** in-package test for `session.go` (722 lines), `dispatch.go` (214) or `file.go` (406) — only `task_test.go`. Five conformance tests encode assumptions about master behaviour that is never directly asserted. | `master/` (own coverage 6.7%) |
| F-59 | `generate-check` — the target enforcing "CI fails if the committed output drifts" — is in neither `check` nor any visible CI step. | `Makefile:31-34,93-96`; `.github/` |
| F-60 | NITs: `parser.resync` has an unguarded `i == 0` case (unreachable, but an invariant held only by callers, on the function whose job is arbitrary input); `Encode` ignores `Header.Length`; `lastSent` aliases a caller-owned buffer; `Header.Byte()` silently truncates `Seq > 63`. | `link/parser.go:151-165`; `link/frame.go:286`; `link/primary.go:151-153`; `transport.go:68-77` |

### 4.4 NOT ASSESSED — two audits never completed

> **This subsection describes gaps in the review, not defects in the library.**
> Nothing here should be read as a finding against go-dnp3 except where a
> specific, verified defect is named as such.

**Coverage gap A — object groups and variations (IEEE 1815-2012 §11).** No
per-group field-layout audit was performed. The 113 variations declared in
`objects/spec/dnp3_objects.yaml` are generated in sync with the spec (verified
independently by regenerating into a `/tmp` copy and diffing), and the package
holds the strongest external anchors in the tree: 6 golden-hex cases in
`objects/objects_test.go:230-282`. **Beyond those 6, no group layout has been
checked against the standard.** Unexamined specifically: the g12 packed control
byte, g2v3 relative time resolved against a g51 CTO, the g50v4 UTC flag and its
reserved bits, the g110/111 three-octet count overflow scheme, the g112/113
virtual-terminal header and terminator rules, and the g70 2–8 layouts.

*One concrete suspicion*, surfaced incidentally by the app-layer audit and
**not yet re-verified against the standard**: the g50v3 size descriptor appears
wrong — `internal/app/zz_generated_sizes.go:104` sizes it 48 bits and names it
`TimeAndDateLastRecorded`, whereas g50v3 is the 8-octet time-and-net-delay
object. If so this also blocks the proper fix for F-13.

**Coverage gap B — the outstation role.** Outstation package coverage is 27.4%,
the second-lowest of any package carrying protocol logic. No full audit of IIN
error mapping, event generation, or select/operate semantics was performed.
Some outstation behaviour *was* covered incidentally by the application-core
audit and is reported above (F-1, F-5, F-6, F-17, F-18, F-34, F-35, F-37, F-3,
F-15, F-16, and the F-45/transport findings), but that is not a substitute.

*Two concrete suspicions* from the app-layer audit, both **unverified**:
- The outstation never generates a g11/g13/g42/g43 **command event** at all —
  `outstation/database.go:424` `raise` is called only from the point-update and
  freeze paths, never from a command. Certification procedure §10 "Output
  Control Events" tests exactly this.
- **Classes 2 and 3 are never exercised in `conformance/`** — class 1 only.

**What closing these gaps requires:** re-run both audits before treating §6 as a
complete remediation plan. F-1, F-3, F-5 and F-6 are fixable now; the list of
things wrong with this library is not final until A and B are done.

## 5. Conformance claim assessment

`docs/device-profile.md:6-10` says the repo contains "a test suite modelled on the
Level 2 procedures". Compared against the **DNP3 IED Certification Procedure v3.1**
procedure names (the full text is member-only; only the TOC is public), over the
named Level-relevant areas:

- **~14 genuinely covered, ~14 superficially covered, ~20 absent.**
- Entirely concentrated in the application layer and the outstation role.
  **All** of link §6 and **all** of transport §7 are absent from `conformance/`.
- Absent includes: double-bit inputs (§8.21/8.22), common-time-of-occurrence
  (§8.15), specific event variations (§8.25), multiple read requests (§8.20),
  multiple-object control requests (§8.2.4), collision avoidance (§8.12),
  §8.6.4 "Local" indications (which F-25 shows the library cannot produce at
  all), §8.2.6 "no control when status non-zero", and §10 output control events
  (which need command events, also not produced).
- The suite is organised **by defect report, not by certification procedure**.

**Verdict: this library would not pass a real DNP3 Level 2 conformance test — but
because of missing tests and missing features, not because the protocol core is
wrong.** The application layer shows genuine care: correct FIR/FIN gating,
duplicate-request replay suppression, per-point static variation, correct
unsolicited retry semantics, correct event select/confirm/unselect lifecycle,
correct 16-bit index refusal, correct trip/close bit decoding, correct
empty-request rejection, correct broadcast non-answering.

**The doc's known-gaps list is exemplary** — all eleven entries were verified real,
none overstated, none already fixed. The overstatement is precisely in the claims
*of verification* (F-25, F-26, F-28), not in the admissions of limitation.
Documentation is otherwise unusually honest: every caveat is in the right place,
all 12 SKILL.md hard rules are true (one for the wrong reason, F-27), all 10
"Does not exist" items are true (the `Close`/`Stop`/`Shutdown` claim was confirmed
by compilation failure), and both attribute-numbering pointers are exactly right —
including the "earlier releases used 208–228" claim, which checks out against git
history.

## 6. Suggested fix order

**Do first — silent data loss and wrong-point actuation (hours, high value):**

1. **F-1** unsolicited event destruction, both ends. *Fix `unsolicited.go:216`
   and the master's `session.go:669` together — they are one bug seen from two
   sides.*
2. **F-4** index truncation. *Highest severity per line changed: a control
   operating the wrong physical point.*
3. **F-3** file write committed and reported successful.
4. **F-2** multi-segment frame parsing.
5. **F-5** garbage clock via prefix-qualifier write.

**Then — unrecoverable state (each needs a watchdog, not just a fix):**

6. **F-7** both DFC/`awaiting` latching paths. *Consider a session-level
   liveness watchdog rather than two point fixes.*
7. **F-6** no response to unparseable object sections.
8. **F-8** periodic scans destroyed on restart.
9. **F-9** transport reset in both directions.

**Then — restore the gates, so the next defect is caught by CI:**

10. **F-10** fuzz oracle, so 10 targets run again.
11. **F-11** interop compile + CI wiring; **F-24** lint version; **F-59** wire
    `generate-check`.
12. **F-22**/**F-31** dead and defaultless config fields — or delete them.
13. **F-12** data race; **F-13** time-sync sign.

**Then — evidence, since no test currently catches any of the above:**

14. **F-58** in-package master session tests; add confirmed-link interop
    coverage (the link layer has *only* ever been validated against itself).
15. Golden byte strings for groups 0 and 70 in `conformance/`, as
    `objects/objects_test.go:230` already does for measurements.
16. Make **F-57**'s two tests assert something.
17. Add **F-25**/**F-26**/**F-28**/**F-52**–**F-56** doc corrections — cheap, and
    they are the document a reviewer reads first.

**Before starting: run the two missing audits (§4.4).** The steps above are
ordered by severity, not by completeness, and are safe to act on — but the list
of things wrong with this library is **not final** until object-group layouts and
the outstation role are covered.

## 7. Caveats on this report

The five caveats in the notice at the top of this report are the operative ones
and are stated there in full. This section records the details behind them.

- **IEEE 1815-2012 was not directly readable** — the standard is paywalled, and
  no auditor had the normative text. No finding above rests on an invented
  clause. Claims resting on opendnp3's behaviour rather than on the standard are
  marked LIKELY or UNCERTAIN in the source audits.

  **Do not resolve divergences toward opendnp3 by default.** In at least three
  places the two implementations deliberately differ and go-dnp3 is the stricter
  or the better-documented one: broadcast `REQUEST_LINK_STATUS` (go-dnp3 discards,
  opendnp3 answers), FCB set on a sec-to-pri frame (go-dnp3 rejects, opendnp3
  processes), and `TEST_LINK_STATES` with an unexpected FCB (opendnp3's own source
  carries a "PITA implement / TODO" comment and admits uncertainty). Two further
  divergences are judgement calls recorded as UNCERTAIN: discarding the whole
  partial fragment on a mid-fragment sequence mismatch (F-list item in §4.3), and
  not checking the sequence of a new message's first segment.

- **Two audits never completed** (§4.4). Object-group layouts and the outstation
  role are **unassessed, not clean**. The two least-tested packages carrying
  protocol logic in the repository sit almost entirely inside this gap.

- **Nothing was run against real hardware or a live peer.** The interop suite was
  compiled, not executed (no Docker available) — and it does not compile (F-11).
  Serial links were never exercised against a device. Fuzzing and lint results
  were reproduced on pristine copies of `HEAD` under `/tmp`. Behaviour against a
  real DNP3 outstation is unmeasured; §5's conformance verdict rests on procedure
  *names* from the public TOC of Certification Procedure v3.1, not on the
  member-only step text.

- **Severity is a judgement, not a measurement.** CRITICAL/HIGH was reserved for
  defects with a demonstrated consequence — silent data loss, silent corruption,
  or actuation of the wrong physical point. Where a finding's impact depends on
  deployment (F-45 link-side DFC, F-46 UDP truncation, F-16 file retry) the
  report says so rather than picking a number.

- **This review cannot be used as a conformance certificate**, and the library
  does not claim to be one. §5 assesses coverage; it does not certify.

- **Repository integrity:** all audits ran in `/tmp` copies or via
  `go test -overlay`. The go-dnp3 working tree was left clean, confirmed by
  `git status --porcelain` returning empty.
