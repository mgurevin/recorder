# recorder HAR and NDJSON inspector

A React + TypeScript viewer for the HAR 1.2 and `JSONStreamRecorder` NDJSON
files produced by `github.com/mgurevin/recorder`, including every `_`-prefixed extension.
Built for dense, prod-debugging-style inspection: filterable exchange list,
trace-chain grouping, timing waterfalls, and task-oriented detail workspaces
down to raw httptrace events.

![HAR Inspector screenshot](../docs/assets/inspector.png)

[Open the live Inspector](https://mgurevin.github.io/recorder/?sample) or run
it locally as described below.

## Supported fields

- All plain HAR 1.2 entry fields (request, response, cookies, headers,
  query string, postData, content, timings, cache, serverIPAddress,
  connection).
- Parsed response-cookie rows recover the complete attribute text from the
  authoritative `Set-Cookie` header, including attributes that HAR 1.2 cannot
  model such as `SameSite`, `Max-Age`, `Priority`, and `Partitioned`.
- The versioned `_recorder` extension has dedicated views for `error` (incl. unwrap chain),
  `_recorder.network` (DNS, reuse, redacted proxy URL, putIdle), `_recorder.tls` (certificate chain, rawDER
  collapsed by default), `_recorder.trace` (relative-time filterable timeline),
  `_recorder.requestBody` / `_recorder.responseBody` (hashes, truncation, store refs),
  `_recorder.redaction` (a dedicated Privacy/Audit view with request/response categories, body
  outcomes, protection-mode counts, fail-closed reasons, and error/trace totals),
  `_recorder.expect100`, `_recorder.informational`, `_recorder.traceId`,
  `_recorder.exchangeId`, `_recorder.redirectIndex`, `_recorder.state`,
  trailers and transfer encodings.
- Unknown future `_` extensions are preserved and shown in the Raw workspace's
  tree viewer. Raw also exposes capture-level creator, browser, page, comment,
  and extension metadata separately from each complete entry representation.
- The Replay workspace reconstructs a copyable, shell-highlighted cURL command,
  including the recorded proxy through `--proxy` when available.
- The Privacy/Protected values view detects `REC-ENC-v1` and `REC-TOK-v1` values. It can
  decrypt AES-256-GCM values or verify HMAC candidates with Web Crypto using
  session-memory-only keys. Enter a key once per mode/key ID, then process the
  selected exchange or the entire HAR in bounded batches. Replay inserts
  decrypted request values only when its separate checkbox is explicitly
  enabled. Verified token candidates and decrypted values are retained only
  in session memory and shown as a resolved view across other detail tabs,
  including recorder extensions such as the Network proxy URL. Replay can
  substitute decrypted encrypted values, but never verified tokenized values;
  the loaded HAR remains unchanged. The **clear resolved data** control forgets
  all plaintext, verified candidates, and entered keys immediately and restores
  the original HAR view. Redacted values remain irreversible.
  In live mode, successfully using an encryption key activates it for that
  browser-memory session: later entries carrying the same key ID are decrypted
  automatically without mutating their raw representation. Changing the key,
  clearing resolved data, loading a file, or starting another connection
  deactivates it. Previously verified token candidates resolve only when the
  exact same token reappears; unseen tokenized values still require a candidate.
  Resolved values are forgotten when their last live entry leaves the bounded
  2,000-entry view.
- Deep link: `/?sample` opens the app with the built-in sample loaded.
- Remote deep link: `/?har=https%3A%2F%2Fexample.com%2Fcapture.har` loads an HTTPS HAR or NDJSON URL automatically.
  Normal `https://gist.github.com/<owner>/<id>` links are converted to their raw Gist endpoint. The remote host must
  allow browser CORS requests; downloads omit credentials and referrer information and are limited to 100 MiB.
- Live local stream: **live** connects to a loopback `DebugStreamRecorder` SSE
  endpoint, clears the current document, and appends finalized exchanges as
  they arrive. Only one Inspector can subscribe. A visible missed-entry count
  reports bounded queue overflow. The recorder retains a bounded backlog before
  subscription and between connections; the Inspector retries indefinitely
  with bounded backoff without clearing received rows. The browser retains only
  the latest 2,000 live entries and reports older removals separately. **clear
  entries** empties the current exchange and trace-chain list without stopping
  the connection or forgetting validated protection keys, so later entries
  continue to arrive and resolve normally. See the
  [complete Go example](../docs/examples/debug-stream/).
  A Pages-hosted Inspector can connect only when its exact origin is present in
  `DebugStreamRecorderConfig.AllowedOrigins`; the Go server still accepts only
  loopback peers. This grants scripts served by that origin access to sensitive
  local capture data, so the local Inspector remains the safer default.

  For this repository's Pages URL, configure the host origin—not the page path:

  ```go
  config := recorder.DefaultDebugStreamRecorderConfig()
  config.AllowedOrigins = append(
	  config.AllowedOrigins,
	  "https://mgurevin.github.io",
  )
  ```

  `https://mgurevin.github.io/recorder/` is not a valid `AllowedOrigins` value
  because `/recorder/` is a path. Local origins such as
  `http://localhost:5173` continue to work without being listed, so this one
  configuration supports both local and Pages-hosted Inspector sessions.

## Fixture exports

Export defaults to immutable protected evidence. HAR and NDJSON downloads retain
every `REC-ENC-v1` and `REC-TOK-v1` token even when the current browser session
has resolved values for display.

The separate **resolved plaintext** option is intended only for controlled test
fixture creation. It substitutes values already resolved in memory across the
complete selected entries, names the result `.resolved.har` or
`.resolved.ndjson`, displays a plaintext-free summary of affected areas, modes,
key IDs, and unresolved locations, and requires two explicit acknowledgements
before enabling download. It never asks for keys or resolves new values during
export and never mutates the loaded capture.

Resolved downloads can contain credentials, cookies, personal or financial
data, proxy secrets, diagnostics, and bodies in plaintext. They are derived
fixtures, not protected evidence. Store them in an access-controlled location,
do not commit them, and use **clear resolved data** after completing the task.

## Local development

```bash
cd inspector
npm install
npm run dev        # http://localhost:5173
```

Other commands:

```bash
npm run build      # typecheck + production build into dist/
npm run build:pages # production build with the GitHub Pages base path
npm run preview    # serve the production build
npm run test       # unit and jsdom evidence-workspace tests (vitest)
npm run test:coverage # tests plus enforced src/lib coverage thresholds and LCOV
```

Coverage gates the Inspector's framework-independent `src/lib` logic at 85%
statements, 75% branches, 90% functions, and 90% lines. A maximal evidence
fixture additionally renders the real detail workspaces in jsdom and verifies
structured field visibility, raw/formatted copy behavior, complete cookie
attributes, future extensions, and capture-level metadata. Component tests are
kept separate from the library percentage so importing React code cannot
silently inflate the coverage gate.

The exchange list is windowed, so DOM size remains bounded while navigating
large captures. CI also exercises parsing, trace grouping, filtering, and
sorting with a synthetic 10,000-exchange HAR. The full capture and normalized
entry model still reside in browser memory; the test is a regression guard,
not a promise that every 100 MiB capture will fit every device's memory budget.

## Appearance and themes

The Inspector provides system, dark, and light color modes. The preference is
kept locally in the browser. The visual system is defined by semantic CSS
custom properties in `src/styles/tokens.css`; components do not need to know
concrete colors.

A deployment can apply its own palette after the bundled stylesheet by
overriding semantic tokens such as:

```css
:root {
  --color-accent: #3b82f6;
  --color-focus: #2563eb;
  --color-surface-selected: #172d4d;
  --color-status-network: #c06cac;
}
```

Keep status and timing tokens semantically distinct and verify text/background
combinations at WCAG AA contrast. Do not override transitional aliases such as
`--panel` or `--muted`; they exist only while older components are migrated to
the semantic token names.

## Loading capture files

- Drag & drop a `.har` or `.ndjson` file anywhere onto the window, or use
  **open file**. NDJSON is validated line by line and wrapped as an in-memory
  HAR document; blank lines are ignored and any malformed line rejects the
  entire file with its physical line number.
- **sample** loads a built-in document covering every recorder feature
  (success, trace chain, DNS/TLS failures, truncated body, gzip-decoded
  JSON, raw trace events).
- Parse and validation problems are shown inline with the underlying JSON
  error; a broken file never crashes the UI.

Timings shown as `not observed` correspond to `-1` in the HAR — the recorder
reports unmeasured phases honestly instead of writing zeros (reused
connections have no dns/connect/ssl by design).

## Security note

HAR and NDJSON files routinely contain sensitive data (URLs, tokens, cookies, bodies) —
even with the recorder's redaction enabled, treat them as confidential. This
inspector runs **entirely in your browser**: files are parsed locally and
nothing is uploaded anywhere. Prefer keeping it that way when deploying; a
static file host serving `dist/` is all it needs.

Protection keys and plaintext are never persisted by the app and are cleared
when another HAR is loaded or **clear resolved data** is used. They can still be exposed through the
screen, clipboard, browser memory/debugging tools, or a copied Replay command;
use the Privacy/Protected values view only on a trusted workstation and trusted static host.

Live mode deliberately accepts only `localhost`, `127.0.0.1`, or `::1` URLs,
and the Go handler rejects non-loopback peers and browser origins. Run the
Inspector locally for the most predictable browser mixed-content behavior.
Never proxy, tunnel, or publish the debug endpoint: SSE entries contain the
same potentially sensitive material as a capture file.
