# dnp3-decode

Renders DNP3 octets as a decoded protocol tree. Offline, no connection, no
device.

Paste hex from a capture, a vendor log or a device console and see what the
frame actually says — which layer a problem lives in, what the outstation
reported, what a master asked for.

```console
$ dnp3-decode 05 64 05 C0 0A 00 01 00 B1 AC
--  link  MSTR→OUTS RESET_LINK_STATES  1→10  len=5  frame=10B

1 frame(s), 0 error(s), 10 octets
```

```console
$ go run ./cmd/dnp3-decode -h     # from the repository
$ go install github.com/dscsystems/go-dnp3/cmd/dnp3-decode@latest
```

## Usage

```
dnp3-decode [flags] <hex octets...>
dnp3-decode [flags] -
dnp3-decode [flags] -f FILE
```

| Flag | Meaning |
| --- | --- |
| `-f FILE` | read hex from a file instead of the command line |
| `-x` | include a hex dump of each frame |
| `-s` | treat the input as one continuous stream and reassemble multi-frame fragments |
| `-q` | suppress the summary line |

Input comes from the arguments, from `-f FILE`, or from standard input when the
only argument is `-`.

Exit status is `0` when everything decoded, `2` when any frame failed, and `1`
for a usage error.

## Reading input

Input is read leniently but not blindly:

- `#` starts a comment
- a hex-dump offset column and an ASCII gutter are recognised and dropped
- only whole hex tokens become octets

So output pasted from Wireshark, a vendor log or a device console usually works
unedited:

```console
$ dnp3-decode -f capture.hex
$ cat capture.hex | dnp3-decode -
$ tcpdump -i lo -s0 -x port 20000 | dnp3-decode -
```

A Wireshark "Copy → …as a Hex Dump" export needs no editing at all.

## Output

The layout is a layer tree, indented, so a problem's layer is visible at a
glance:

```console
$ dnp3-decode -f decoder/testdata/sample.hex
--  link  MSTR→OUTS RESET_LINK_STATES  1→10  len=5  frame=10B

--  link  OUTS→MSTR ACK  10→1  len=5  frame=10B

--  link  MSTR→OUTS UNCONFIRMED_USER_DATA  1→10  len=20  frame=27B
      transport  seq=00 FIR|FIN
      application  READ seq=00 FIR FIN
        g60v2  0x06(none,all-objects) all            0 object(s)
        g60v3  0x06(none,all-objects) all            0 object(s)
        g60v4  0x06(none,all-objects) all            0 object(s)
        g60v1  0x06(none,all-objects) all            0 object(s)

--  link  OUTS→MSTR UNCONFIRMED_USER_DATA  10→1  len=30  frame=39B
      transport  seq=00 FIR|FIN
      application  RESPONSE seq=00 FIR FIN iin=CLASS_1_EVENTS|DEVICE_RESTART
        g1v2  0x00(none,start-stop8) [0..3]         4 object(s)  4 octets
          [0] ON  ONLINE
          [1] OFF  ONLINE
          [2] ON  ONLINE
          [3] OFF  ONLINE
        g30v2  0x00(none,start-stop8) [0..1]         2 object(s)  6 octets
          [0] 300  ONLINE
          [1] 400  ONLINE

--  link  MSTR→OUTS UNCONFIRMED_USER_DATA  1→10  len=24  frame=33B
      transport  seq=00 FIR|FIN
      application  DIRECT_OPERATE seq=01 FIR FIN
        g12v1  0x17(index8,count8)    count=1        1 object(s)  12 octets
          [3] PULSE_ON|TRIP count=1 on=1000ms off=0ms → SUCCESS

5 frame(s), 0 error(s), 119 octets
```

Reading a line:

- **link** — direction, function code, source→destination link addresses, the
  length octet, and the total frame size on the wire
- **transport** — the sequence number and the FIR/FIN segmentation bits
- **application** — function code, application sequence, FIR/FIN, and the
  internal indications on a response
- **objects** — group and variation, the qualifier byte with its prefix and
  range codes named, the index range or count, object count and payload size,
  then the decoded values with their quality flags

`-x` adds a hex dump under each frame:

```console
$ dnp3-decode -x 05 64 05 C0 0A 00 01 00 B1 AC
--  link  MSTR→OUTS RESET_LINK_STATES  1→10  len=5  frame=10B
      0000  05 64 05 c0 0a 00 01 00  b1 ac                    |.d........|

1 frame(s), 0 error(s), 10 octets
```

## Frame-at-a-time versus streaming

By default each frame is decoded **independently**, which is what you want when
pasting frames captured out of context: a frame that happens to be the middle
segment of a fragment still shows its link and transport layers.

`-s` treats the input as **one continuous stream** and keeps transport state
across frames, so a fragment split over several frames is reassembled and its
application layer decoded on the frame that completes it. Use it for a whole
capture of one direction of one connection.

```console
$ cat capture.hex | dnp3-decode -s
```

In streaming mode, octets discarded at the link or transport layer are reported
and counted as errors — a capture that quietly drops segments is a capture
telling you something.

**Feed one direction at a time.** The two directions of a connection carry
independent transport sequences; interleaving them into one stream produces
nonsense. This is the same constraint the `decoder` package documents for
`decoder.Decoder`.

## Errors

A frame that does not decode is reported in place, and the rest of the input is
still processed:

```console
$ dnp3-decode 05 64 FF
-- 3 trailing octet(s) do not form a complete frame

0 frame(s), 1 error(s), 3 octets
```

When a fragment's header parses but its object headers do not, the headers
decoded before the failure are still shown. Showing an operator what was
understood before the corruption beats showing nothing.

## Reading it as example code

`main.go` is the whole program, and it is a small one. What it demonstrates:

- `decoder.DecodeFrame(nil, data)` for one-shot decoding with no session state
- `decoder.New(decoder.DirUnknown, nil)` plus `Feed` for stateful reassembly
- `Trace.Render` for the human-readable tree, and `Decoder.Stats()` for the
  discard counters

Every piece of parsing lives in the `decoder` package, not here. There is
exactly one place in the repository that knows how to read a DNP3 frame, and
this tool is a renderer over it — which is why the terminal explorer, the
session logs and this program can never disagree about what a frame says.

## See also

- [`dnp3-explorer`](../dnp3-explorer/README.md) — the live equivalent
- [User guide](../../docs/user-guide.md#decoding-traffic) · [API reference](../../docs/api.md#package-decoder)
