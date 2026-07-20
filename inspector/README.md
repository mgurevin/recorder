# recorder HAR inspector

A React + TypeScript viewer for the HAR 1.2 files produced by
`github.com/mgurevin/recorder`, including every `_`-prefixed extension.
Built for dense, prod-debugging-style inspection: filterable exchange list,
trace-chain grouping, timing waterfalls, and tabbed detail views down to
raw httptrace events.

![HAR Inspector screenshot](../docs/assets/inspector.png)

## Supported fields

- All plain HAR 1.2 entry fields (request, response, cookies, headers,
  query string, postData, content, timings, cache, serverIPAddress,
  connection).
- Recorder extensions with dedicated views: `_error` (incl. unwrap chain),
  `_network` (DNS, reuse, redacted proxy URL, putIdle), `_tls` (certificate chain, rawDER
  collapsed by default), `_trace` (relative-time filterable timeline),
  `_requestBody` / `_responseBody` (hashes, truncation, store refs),
  `_redaction` (a dedicated audit tab with request/response categories, body
  outcomes, protection-mode counts, fail-closed reasons, and error/trace totals),
  `_expect100`, `_informational`, `_traceId` / `_exchangeId` /
  `_redirectIndex`, `_state`, trailers and transfer encodings.
- Unknown future `_` extensions are preserved and shown in the Raw tab's
  tree viewer.
- The Replay tab reconstructs a copyable, shell-highlighted cURL command,
  including the recorded proxy through `--proxy` when available.
- The Protection tab detects `REC-ENC-v1` and `REC-TOK-v1` values. It can
  decrypt AES-256-GCM values or verify HMAC candidates with Web Crypto using
  session-memory-only keys. Enter a key once per mode/key ID, then process the
  selected exchange or the entire HAR in bounded batches. Replay inserts
  decrypted request values only when its separate checkbox is explicitly
  enabled. Other detail tabs show the decrypted in-memory view after a
  successful operation; the loaded HAR remains unchanged. Redacted and
  tokenized values remain irreversible.
- Deep link: `/?sample` opens the app with the built-in sample loaded.
- Remote deep link: `/?har=https%3A%2F%2Fexample.com%2Fcapture.har` loads an HTTPS HAR URL automatically.
  Normal `https://gist.github.com/<owner>/<id>` links are converted to their raw Gist endpoint. The remote host must
  allow browser CORS requests; downloads omit credentials and referrer information and are limited to 100 MiB.

## Local development

```bash
cd inspector
npm install
npm run dev        # http://localhost:5173
```

Other commands:

```bash
npm run build      # typecheck + production build into dist/
npm run preview    # serve the production build
npm run test       # parser/formatter unit tests (vitest)
```

## Loading HAR files

- Drag & drop a `.har` file anywhere onto the window, or use **open HAR**.
- **sample** loads a built-in document covering every recorder feature
  (success, trace chain, DNS/TLS failures, truncated body, gzip-decoded
  JSON, raw trace events).
- Parse and validation problems are shown inline with the underlying JSON
  error; a broken file never crashes the UI.

Timings shown as `not observed` correspond to `-1` in the HAR — the recorder
reports unmeasured phases honestly instead of writing zeros (reused
connections have no dns/connect/ssl by design).

## Security note

HAR files routinely contain sensitive data (URLs, tokens, cookies, bodies) —
even with the recorder's redaction enabled, treat them as confidential. This
inspector runs **entirely in your browser**: files are parsed locally and
nothing is uploaded anywhere. Prefer keeping it that way when deploying; a
static file host serving `dist/` is all it needs.

Protection keys and plaintext are never persisted by the app and are cleared
when another HAR is loaded. They can still be exposed through the
screen, clipboard, browser memory/debugging tools, or a copied Replay command;
use the Protection tab only on a trusted workstation and trusted static host.
