# Live local debugging with `DebugStreamRecorder`

This example records finalized HTTP exchanges to `debug-entries.ndjson` while
also streaming them into one Inspector window during local development.
`NewMultiRecorder` fans each immutable entry out to the file and ephemeral
sinks. The live side is not an evidence store: its bounded queue retains recent
entries until the Inspector connects or reconnects, but an extended absence or
slow browser can still evict entries without affecting the NDJSON sink.

Run the Inspector and the example in separate terminals:

```sh
cd inspector
npm run dev
```

```sh
go run ./docs/examples/debug-stream
```

Open `http://localhost:5173`, choose **live**, keep the default
`http://127.0.0.1:7070` URL, and connect. Only one Inspector may subscribe at a
time. Connecting starts a new Inspector document; temporary disconnects keep
already received entries on screen and retry indefinitely. The example request
runs immediately, so connecting afterward demonstrates replay from the bounded
recorder queue.

The example puts `AsyncRecorder` in front of `MultiRecorder` so file writes,
JSON encoding, and a temporarily slow browser do not run on HTTP response-
finalization goroutines. MultiRecorder preserves batch delivery: the NDJSON
sink writes each batch efficiently while the ordinary live sink receives its
entries in order.
This creates two bounded queues with different purposes: AsyncRecorder applies
its configured evidence/backpressure policy before delivery, while
DebugStreamRecorder always drops the oldest pending UI update and emits a
`gap` event. The Inspector shows the cumulative missed-entry count.
The Inspector also caps its live view at the latest 2,000 entries and reports
how many older rows it removed, so a long-running debug session stays bounded.

`DebugStreamRecorder` is an `http.Handler`; it does not open sockets or own
server shutdown. The application binds explicitly to `127.0.0.1`, installs the
handler, and shuts the server down. The handler additionally rejects
non-loopback peers, non-loopback browser origins, non-GET methods, and a second
subscriber.

The default configuration accepts browser origins on loopback only. To use a
trusted hosted Inspector, explicitly add its exact origin before constructing
the recorder:

```go
config := recorder.DefaultDebugStreamRecorderConfig()
config.AllowedOrigins = []string{"https://mgurevin.github.io"}

stream, err := recorder.NewDebugStreamRecorder(config)
```

This does not make the HTTP listener remotely reachable: peer connections must
still originate from loopback. It does allow all JavaScript executing under
the configured origin to read streamed captures, so prefer the local Inspector
and opt in only when that hosted origin and its assets are trusted.

Do not expose this endpoint through a reverse proxy, container port mapping,
or tunnel. Entries can contain all sensitive material present in a HAR. The
NDJSON side demonstrates independent file persistence, not crash durability.
For production, select and manage an evidence sink with the required durability
independently of this local debugging stream.
