# Live local debugging with `DebugStreamRecorder`

This example records finalized HTTP exchanges to `debug-entries.ndjson` while
also streaming them into one Inspector window during local development.
`NewMultiRecorder` fans each immutable entry out to the file and ephemeral
sinks. The live side is not an evidence store: no subscriber means no live
retention, disconnecting forgets queued live entries, and a slow browser can
miss live entries without affecting the NDJSON sink.

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
time. Connecting clears the Inspector's current document; disconnecting keeps
the already received entries on screen but the Go recorder retains no replay
history. The sample waits for that first subscriber before making its example
request, so the exchange is visible after connection.

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

Do not expose this endpoint through a reverse proxy, container port mapping,
or tunnel. Entries can contain all sensitive material present in a HAR. The
NDJSON side demonstrates independent file persistence, not crash durability.
For production, select and manage an evidence sink with the required durability
independently of this local debugging stream.
