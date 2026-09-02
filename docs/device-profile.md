# go-dnp3 device profile

What this implementation supports, in the shape of the DNP3 device profile a
vendor ships with a device.

**This is not a conformance claim.** Nothing here has been through the DNP
Users Group's certification. The repository contains a test suite modelled on
the Level 2 procedures (`conformance/`), which is a useful check and not the
same thing. Where this document says "supported", it means "implemented and
covered by tests in this repository".

Last updated for the state of the tree as of the transports-and-examples work.

---

## Identification

| | |
| --- | --- |
| Implementation | go-dnp3 |
| Roles | Master and outstation |
| Target conformance | Level 2 complete, most of Level 3, parts of Level 4 |
| DNP3 revision | IEEE Std 1815-2012 |

---

## Physical and link layer

| Item | Support |
| --- | --- |
| TCP client | Yes |
| TCP server | Yes, one session per channel |
| TLS | Yes, mutual authentication required, TLS 1.2 floor |
| UDP | Yes, one link frame per datagram |
| Serial | Yes, via `go.bug.st/serial` |
| Link addresses | Full 16-bit range |
| Broadcast addresses | Received and executed; never answered |
| Self-address (0xFFFC) | **Not supported** |
| Link confirmation | Yes, configurable, with retransmission |
| Link status / keep-alive | Yes |
| Frame size | 292 octets maximum, per the standard |

The **TCP server accepts one master at a time.** Serving several concurrently
needs a session per connection, which belongs above the channel layer and is
not implemented.

---

## Transport function

Fully implemented: segmentation, reassembly, the six-bit sequence, and the
discard rules. Every discard reason is counted separately, because "the link is
unreliable" is not a diagnosis.

Default maximum receive fragment: 2048 octets, configurable.

---

## Application layer

### Function codes

| Code | Name | Master | Outstation |
| --- | --- | --- | --- |
| 0 | CONFIRM | Sends | Receives |
| 1 | READ | Yes | Yes |
| 2 | WRITE | Yes | Yes (g50v1 time, g80v1 indications, g34 deadbands) |
| 3 | SELECT | Yes | Yes |
| 4 | OPERATE | Yes | Yes |
| 5 | DIRECT_OPERATE | Yes | Yes |
| 6 | DIRECT_OPERATE_NR | Yes | Yes |
| 7/8 | IMMED_FREEZE(_NR) | — | Yes |
| 9–12 | FREEZE_CLEAR, FREEZE_AT_TIME | — | **No** |
| 13 | COLD_RESTART | Yes | Yes |
| 14 | WARM_RESTART | Yes | Yes |
| 20/21 | ENABLE/DISABLE_UNSOLICITED | Yes | Yes |
| 22 | ASSIGN_CLASS | — | Yes |
| 23 | DELAY_MEASURE | Yes | Yes |
| 24 | RECORD_CURRENT_TIME | — | Yes |
| 25 | OPEN_FILE | Yes | Yes |
| 26 | CLOSE_FILE | Yes | Yes |
| 27 | DELETE_FILE | Yes | Yes |
| 28 | GET_FILE_INFO | Yes | Yes |
| 29 | AUTHENTICATE_FILE | **No** | **No** |
| 30 | ABORT_FILE | — | Yes |
| 31 | ACTIVATE_CONFIG | **No** | **No** |
| 32/33 | Authentication | **No** | **No** |
| 129 | RESPONSE | Receives | Sends |
| 130 | UNSOLICITED_RESPONSE | Receives | Sends |

An unknown function code is answered with `IIN2.NO_FUNC_CODE_SUPPORT`.

### Internal indications

All fourteen defined bits are produced and interpreted. Of note:

- `DEVICE_RESTART` is asserted on start and after a restart, and cleared only
  by a master writing g80v1 index 7.
- `EVENT_BUFFER_OVERFLOW` is latched when events are dropped, which is the only
  way a master learns its sequence-of-events record has a hole in it.
- `NEED_TIME` is asserted until a master writes the clock, and it downgrades
  the quality of every timestamp the outstation reports.

### Fragment sizes

Transmit and receive fragment limits are configurable; both default to 2048
octets. Responses spanning several fragments are supported in both directions.

---

## Object groups

Sizes and field layouts for all of these are generated from
[`objects/spec/dnp3_objects.yaml`](../objects/spec/dnp3_objects.yaml).

| Group | Variations | Parse | Write | Notes |
| --- | --- | --- | --- | --- |
| 1 | 1, 2 | Yes | Yes | Binary input, packed and with flags |
| 2 | 1, 2, 3 | Yes | Yes | Events; v3 relative time resolved against a g51 CTO |
| 3 | 1, 2 | Yes | Yes | Double-bit binary input |
| 4 | 1, 2, 3 | Yes | Yes | Double-bit events |
| 10 | 1, 2 | Yes | Yes | Binary output status |
| 11 | 1, 2 | Yes | Yes | Binary output events |
| 12 | 1, 2, 3 | Yes | Yes | CROB; v2 and v3 decode but the outstation treats them as v1 |
| 13 | 1, 2 | Sizes only | — | Binary output command events |
| 20–23 | see spec | Yes | Yes | Counters and frozen counters |
| 30–33 | see spec | Yes | Yes | Analog inputs, frozen, and their events |
| 34 | 1, 2, 3 | Yes | Yes | Analog deadbands, writable by a master |
| 40–43 | see spec | Yes | Yes | Analog outputs, commands and events |
| 50 | 1, 2, 3, 4 | Yes | Yes | Time and date |
| 51 | 1, 2 | Yes | Yes | Common time of occurrence |
| 52 | 1, 2 | Yes | Yes | Time delay |
| 60 | 1, 2, 3, 4 | Yes | Yes | Class data |
| 80 | 1 | Yes | Yes | Internal indications |
| 110, 111 | any length | Yes | Yes | Octet strings, static and event |
| 112, 113 | — | Sizes only | — | Virtual terminal |
| 0 | any | Yes | Yes | Device attributes |
| 70 | 2–8 | Yes | Yes | File transfer |
| 85–87 | — | **No** | **No** | Datasets |
| 120–121 | — | **No** | **No** | Secure Authentication v5 — use TLS |

"Sizes only" means the framing layer can walk past the objects without
misparsing the rest of the fragment, but no codec turns them into values.

---

## Outstation behaviour

| Item | Support |
| --- | --- |
| Static data reporting | Yes, per-point configurable variation |
| Event generation | On value or quality change; analogs and counters honour a deadband |
| Deadband reference | The last **reported** value, not the last stored one |
| Event classes | 1, 2, 3 assignable per point; class 0 for static data |
| Event buffer | Configurable capacity, oldest discarded on overflow |
| Event confirmation | Events are cleared only by an application confirm |
| Confirm timeout | Selected events return to the queue and are re-sent |
| Unsolicited reporting | Yes, with a null response first, hold time and retries |
| Select-before-operate | Yes, with a configurable timeout |
| Select matching | Raw object octets must match exactly |
| Multi-fragment responses | Yes |
| Broadcast requests | Executed, not answered; `IIN1.BROADCAST` on the next response |
| Clock | Set by a master; the outstation reports `NEED_TIME` until then |
| Clock procedures accepted | Direct write (g50v1) and the recorded-time procedure (RECORD_CURRENT_TIME then a g50v3 write) |
| Cold and warm restart | Yes, answered with a g52v2 time delay |

### Default variations

| Type | Static | Event |
| --- | --- | --- |
| Binary input | g1v2 | g2v2 |
| Double-bit | g3v2 | g4v2 |
| Binary output status | g10v2 | g11v2 |
| Counter | g20v1 | g22v5 |
| Frozen counter | g21v1 | g23v5 |
| Analog input | g30v1 | g32v3 |
| Analog output status | g40v1 | g42v3 |

**The analog defaults are 32-bit integers.** That is the widest lossless
*integer* encoding, but it truncates fractions: a point reporting 123.5 arrives
as 123. A point that carries fractions must be configured for g30v5 or g30v6.
This is the encoding behaving as specified, not a defect, and it is the first
thing to check when values look wrong.

---

## Master behaviour

| Item | Support |
| --- | --- |
| Startup sequence | Clear restart → disable unsolicited → integrity poll → enable unsolicited |
| Sequence integrity | The steps are chained, so nothing can be interleaved |
| Integrity poll | Classes 0, 1, 2 and 3 |
| Class polls | One-shot and periodic |
| Range reads | Yes, any group and variation |
| Unsolicited handling | Yes, separate sequence space, duplicate detection |
| Commands | Direct operate, direct operate no-reply, select-before-operate |
| Command results | Per-point status; a partial success is reported as a failure |
| Clock synchronisation | LAN (write time) and serial (delay-measure first) |
| Restart detection | Re-runs the startup sequence, guarded against re-entry |
| Deadband writes | Yes, g34v3 |
| Keep-alive | Link status request on an idle link |
| Reconnect | Exponential backoff with jitter |

---

## Interoperability

Verified against opendnp3, in both directions, with
container built from source (`make interop-build && make interop`):

| Peer | Version | Our master → their outstation | Their master → our outstation |
| --- | --- | --- | --- |
| [opendnp3](https://github.com/dnp3/opendnp3) | 3.1.2 (final release) | Integrity and class polls, controls, both clock procedures | All object headers parsed, no errors |

## Known gaps

Listed rather than left to be discovered:

- **Self-address** (0xFFFC) is not implemented, so a master cannot address an
  outstation whose configured address it does not know.
- **Device attributes** (group 0) are implemented for reading, in any set: a
  master reads one attribute or all of them, and an outstation answers from
  what the application configured plus the point counts and fragment sizes it
  derives from its own database. What is not implemented: writing an attribute,
  and the "list of attribute variations" request (variation 255), which is a
  distinct encoding this implementation does not have and answers with
  OBJECT_UNKNOWN rather than a guess.

  The **names** this library prints for standard-set variations are transcribed
  from the standard's table and have not been checked against another vendor's
  device. They are display only — nothing routes on a name, the wire carries
  numbers, and an entry that is wrong mislabels a row without affecting an
  octet. The numbers an outstation **answers with** are a different matter and
  are listed in `outstation/attribute.go`.
- **File transfer** (group 70) is implemented for reading, writing, listing and
  deleting: `OPEN_FILE`, `CLOSE_FILE`, `DELETE_FILE`, `ABORT_FILE`,
  `GET_FILE_INFO`, and the `READ`/`WRITE` of group 70 variation 5 blocks.
  Variations 2 through 8 all decode. What is not implemented: the
  `AUTHENTICATE_FILE` exchange (variation 2 is encoded and decoded, but a
  session never performs the handshake, so an outstation demanding one cannot
  be talked to), and an outstation serves one transfer at a time. Blocks must
  arrive in order — the outstation cannot rewind a stream, so a master that
  re-requests an earlier block is answered with `BLOCK_SEQUENCE`.
  `GET_FILE_INFO` follows this implementation's reading of the standard, with
  the file named in a variation 7 descriptor; it has not been exercised against
  another vendor's device.
- **Datasets** (groups 85–87) are not implemented.
- **Secure Authentication v5** is out of scope by design; use TLS.
- **`FREEZE_AT_TIME`** is not implemented, and the framing layer's rule for
  whether a function code carries object data is wrong for it — its leading
  group 50 object carries data while the counter headers after it do not.
  Resolving that needs per-object semantics rather than a per-fragment rule.
- The **TCP server** serves one master at a time.
- **Analog output status points are not driven by analog output commands** in
  the library: a command reaches the `CommandHandler`, and it is the
  application's job to reflect it back into the database. Whether a device
  assumes a setpoint, holds it, or drives plant that only approaches it is a
  property of the device, not of the protocol, so the library does not decide.
  `cmd/dnp3-outstation` shows the usual answer — it holds what it was written
  and reports it back on the status point.
