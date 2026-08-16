# Commands

Four programs ship with the library. They are usable tools, and they are the
worked examples: between them they exercise every part of the public API.

| Command | What it is | Read it for |
| --- | --- | --- |
| [`dnp3-explorer`](dnp3-explorer/README.md) | A terminal DNP3 browser for one outstation | Driving a session from a UI without ever blocking it |
| [`dnp3-master`](dnp3-master/README.md) | A SCADA client, CSV recorder and control tool | Polling many sites, consuming updates, issuing controls |
| [`dnp3-outstation`](dnp3-outstation/README.md) | A simulated RTU with plant behind it and fault injection | Implementing an outstation: database, commands, events |
| [`dnp3-decode`](dnp3-decode/README.md) | An offline frame decoder | Rendering protocol traces |

Try them without any hardware:

```console
$ go run ./cmd/dnp3-explorer -demo                 # full outstation in-process
$ go run ./cmd/dnp3-outstation                     # a substation on :20000
$ go run ./cmd/dnp3-master -host 127.0.0.1:20000   # ...and poll it
$ go run ./cmd/dnp3-decode -f decoder/testdata/sample.hex
```

These carry dependencies the library itself does not reach: Bubble Tea in the
explorer, `yaml.v3` for the configuration files in `dnp3-master` and
`dnp3-outstation`. Nothing in `cmd/` is imported by any importable package, so
none of it lands in your build.

See the [user guide](../docs/user-guide.md) for building your own, and the
[API reference](../docs/api.md) for the details.
