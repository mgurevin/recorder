import { useEffect, useMemo, useState } from "react";
import { ArrowLeft } from "lucide-react";
import type { BodyInfo, CertInfo, NEntry, RedactionScopeInfo } from "../types/har";
import {
  formatBytes,
  formatDuration,
  decodeBase64,
  parseIsoMs,
  prettyContent,
  prettyPostData,
  relMs,
} from "../lib/format";
import { extensionFields } from "../lib/parse";
import { curlReplay } from "../lib/curl";
import {
  decryptProtectedToken,
  protectedOccurrences,
  verifyProtectedToken,
  type ProtectedOccurrence,
} from "../lib/protection";
import {
  BoolMark,
  CodeBlock,
  CookiesTable,
  CopyButton,
  EmptyState,
  JsonTree,
  KV,
  PairsTable,
  Section,
  StateBadge,
  StatusBadge,
} from "./Shared";
import { Waterfall } from "./Waterfall";

const TABS = ["Overview", "Timings", "Request", "Response", "Error", "Network", "TLS", "Trace", "Raw", "Protection", "Replay"] as const;
type Tab = (typeof TABS)[number];

export function DetailPanel({ entry, onBack }: { entry: NEntry; onBack: () => void }) {
  const [tab, setTab] = useState<Tab>("Overview");
  const [sessionEntry, setSessionEntry] = useState(entry.e);
  const [decryptedValues, setDecryptedValues] = useState<ReadonlyMap<string, string>>(new Map());
  const [keyInputs, setKeyInputs] = useState<ReadonlyMap<string, string>>(new Map());
  const activeDecryptedValues = sessionEntry === entry.e ? decryptedValues : new Map<string, string>();
  const activeKeyInputs = sessionEntry === entry.e ? keyInputs : new Map<string, string>();
  useEffect(() => {
    setSessionEntry(entry.e);
    setDecryptedValues(new Map());
    setKeyInputs(new Map());
  }, [entry.e]);
  return (
    <div className="detail">
      <div className="detail-head">
        <button type="button" className="icon-btn back-btn" onClick={onBack} data-tooltip="Back to request list" aria-label="Back to request list">
          <ArrowLeft size={15} />
        </button>
        <span className="badge method" data-tooltip={`HTTP method: ${entry.method}`}>{entry.method}</span>
        <span className="detail-url mono" title={entry.url}>
          {entry.host}
          <span className="muted">{entry.path}</span>
        </span>
        <StatusBadge status={entry.status} />
        <StateBadge state={entry.state} />
      </div>
      <nav className="tabs">
        {TABS.map((t) => (
          <button key={t} type="button" className={t === tab ? "tab active" : "tab"} onClick={() => setTab(t)}>
            {t}
            {t === "Error" && entry.e._error ? <span className="tab-dot" /> : null}
          </button>
        ))}
      </nav>
      <div className="detail-body">
        {tab === "Overview" && <OverviewTab entry={entry} />}
        {tab === "Timings" && <TimingsTab entry={entry} />}
        {tab === "Request" && <RequestTab entry={entry} />}
        {tab === "Protection" && (
          <ProtectionTab
            entry={entry}
            decryptedValues={activeDecryptedValues}
            onDecrypted={(token, value) => {
              setSessionEntry(entry.e);
              setDecryptedValues((current) => new Map(sessionEntry === entry.e ? current : []).set(token, value));
            }}
            keyInputs={activeKeyInputs}
            onKeyInput={(keyId, value) => {
              setSessionEntry(entry.e);
              setKeyInputs((current) => new Map(sessionEntry === entry.e ? current : []).set(keyId, value));
            }}
          />
        )}
        {tab === "Replay" && <ReplayTab entry={entry} decryptedValues={activeDecryptedValues} />}
        {tab === "Response" && <ResponseTab entry={entry} />}
        {tab === "Error" && <ErrorTab entry={entry} />}
        {tab === "Network" && <NetworkTab entry={entry} />}
        {tab === "TLS" && <TlsTab entry={entry} />}
        {tab === "Trace" && <TraceTab entry={entry} />}
        {tab === "Raw" && <RawTab entry={entry} />}
      </div>
    </div>
  );
}

function ReplayTab({ entry, decryptedValues }: { entry: NEntry; decryptedValues: ReadonlyMap<string, string> }) {
  const [includeLocalInterface, setIncludeLocalInterface] = useState(false);
  const [includeDecryptedValues, setIncludeDecryptedValues] = useState(false);
  const hasLocalAddress = Boolean(entry.e._network?.localAddress);
  const requestTokens = useMemo(() => protectedOccurrences(entry.e).filter((item) => item.request && item.mode === "encrypt"), [entry.e]);
  const requestDecrypted = useMemo(() => new Map(
    [...decryptedValues].filter(([token]) => requestTokens.some((item) => item.token === token)),
  ), [decryptedValues, requestTokens]);
  useEffect(() => setIncludeDecryptedValues(false), [entry.e]);
  const replay = useMemo(
    () => curlReplay(entry.e, {
      includeLocalInterface,
      decryptedValues: includeDecryptedValues && requestDecrypted.size > 0 ? requestDecrypted : undefined,
    }),
    [entry.e, includeLocalInterface, includeDecryptedValues, requestDecrypted],
  );
  return (
    <>
      <Section title="cURL command">
        <label
          className="replay-option"
          data-tooltip={hasLocalAddress
            ? "Add the recorded local IP using curl --interface"
            : "No local address was recorded for this exchange"}
        >
          <input
            type="checkbox"
            checked={includeLocalInterface}
            disabled={!hasLocalAddress}
            onChange={(event) => setIncludeLocalInterface(event.target.checked)}
          />
          use recorded local interface
        </label>
        <label
          className="replay-option"
          data-tooltip={requestDecrypted.size
            ? "Insert decrypted request values into this in-memory cURL command"
            : "Decrypt request values in the Protection tab first"}
        >
          <input
            type="checkbox"
            checked={includeDecryptedValues && requestDecrypted.size > 0}
            disabled={requestDecrypted.size === 0}
            onChange={(event) => setIncludeDecryptedValues(event.target.checked)}
          />
          use decrypted values ({requestDecrypted.size}/{requestTokens.length})
        </label>
        {replay.command ? (
          <CodeBlock text={replay.command} note="POSIX shell" language="shell" />
        ) : (
          <EmptyState text="no request recorded" />
        )}
      </Section>
      <Section title="Replay notes">
        <ul className="replay-warnings">
          {replay.warnings.map((warning) => (
            <li key={warning}>{warning}</li>
          ))}
        </ul>
      </Section>
    </>
  );
}

function ProtectionTab({
  entry,
  decryptedValues,
  onDecrypted,
  keyInputs,
  onKeyInput,
}: {
  entry: NEntry;
  decryptedValues: ReadonlyMap<string, string>;
  onDecrypted: (token: string, value: string) => void;
  keyInputs: ReadonlyMap<string, string>;
  onKeyInput: (keyId: string, value: string) => void;
}) {
  const occurrences = useMemo(() => protectedOccurrences(entry.e), [entry.e]);
  if (occurrences.length === 0) return <EmptyState text="no encrypted or tokenized values detected" />;
  return (
    <>
      <Section title="Protected values">
        <p className="muted protection-intro">
          Keys and plaintext stay in this page's memory only and are cleared when another HAR entry is loaded.
          Decrypted values are never added to Replay unless you explicitly enable them there.
        </p>
        <div className="protection-list">
          {occurrences.map((occurrence, index) => (
            <ProtectedValueCard
              key={`${occurrence.path}-${occurrence.token}-${index}`}
              occurrence={occurrence}
              decrypted={decryptedValues.get(occurrence.token)}
              keyInput={keyInputs.get(occurrence.keyId) ?? ""}
              onKeyInput={(value) => onKeyInput(occurrence.keyId, value)}
              onDecrypted={(value) => onDecrypted(occurrence.token, value)}
            />
          ))}
        </div>
      </Section>
    </>
  );
}

function ProtectedValueCard({
  occurrence,
  decrypted,
  keyInput,
  onKeyInput,
  onDecrypted,
}: {
  occurrence: ProtectedOccurrence;
  decrypted: string | undefined;
  keyInput: string;
  onKeyInput: (value: string) => void;
  onDecrypted: (value: string) => void;
}) {
  const [candidate, setCandidate] = useState("");
  const [result, setResult] = useState("");
  const act = async () => {
    setResult("");
    try {
      if (occurrence.mode === "encrypt") {
        onDecrypted(await decryptProtectedToken(occurrence, keyInput));
        setResult("decrypted in memory");
      } else {
        const verified = await verifyProtectedToken(occurrence, candidate, keyInput);
        setResult(verified ? "candidate matches" : "candidate does not match");
      }
    } catch (error) {
      setResult(error instanceof Error ? error.message : "Protection operation failed.");
    }
  };
  return (
    <div className="protection-card">
      <div className="protection-card-head">
        <span className="badge">{occurrence.mode === "encrypt" ? "encrypted" : "tokenized"}</span>
        <span className="mono wrap">{occurrence.path}</span>
        <span className="muted">key: {occurrence.keyId}</span>
      </div>
      <div className="mono protection-token" title={occurrence.token}>{occurrence.token}</div>
      <label className="protection-field">
        key (hex, base64, or base64url)
        <input
          type="password"
          autoComplete="off"
          spellCheck={false}
          value={keyInput}
          onChange={(event) => onKeyInput(event.target.value)}
        />
      </label>
      {occurrence.mode === "tokenize" && (
        <label className="protection-field">
          candidate value
          <textarea value={candidate} onChange={(event) => setCandidate(event.target.value)} />
        </label>
      )}
      <button type="button" className="btn" disabled={!keyInput || (occurrence.mode === "tokenize" && !candidate)} onClick={act}>
        {occurrence.mode === "encrypt" ? "decrypt" : "verify candidate"}
      </button>
      {result && <span className="protection-result" role="status">{result}</span>}
      {decrypted !== undefined && (
        <div className="protection-plain">
          <CodeBlock text={decrypted} note="decrypted in memory" />
        </div>
      )}
    </div>
  );
}

function bodySummary(info: BodyInfo | undefined): string | null {
  if (!info) return null;
  return [
    `${formatBytes(info.totalBytes)} total`,
    `${formatBytes(info.capturedBytes)} captured`,
    info.complete ? "complete" : "incomplete",
    info.truncated ? "truncated" : null,
    info.closedEarly ? "closed early" : null,
    info.readError ? `read error: ${info.readError}` : null,
  ]
    .filter(Boolean)
    .join(" · ");
}

function missingEmbeddedBodyText(kind: "request" | "response", info: BodyInfo | undefined): string {
  if ((info?.capturedBytes ?? 0) > 0) {
    return info?.store
      ? `${kind} body captured in an external store, but not embedded in this HAR`
      : `${kind} body captured, but not embedded in this HAR`;
  }
  if ((info?.totalBytes ?? 0) > 0) {
    return `${kind} body observed, but its content was not captured`;
  }
  return `no ${kind} body content recorded`;
}

function redactionSummary(scope: RedactionScopeInfo | undefined): string {
  if (!scope) return "none reported";
  const body = scope.body;
  return [
    scope.url ? `${scope.url} URL value${scope.url === 1 ? "" : "s"}` : null,
    scope.headers ? `${scope.headers} header value${scope.headers === 1 ? "" : "s"}` : null,
    scope.queryParameters ? `${scope.queryParameters} query value${scope.queryParameters === 1 ? "" : "s"}` : null,
    scope.cookies ? `${scope.cookies} cookie value${scope.cookies === 1 ? "" : "s"}` : null,
    body
      ? `${body.kind} body: ${body.outcome}${body.replacements != null ? ` (${body.replacements} replacements)` : ""}${protectionSummary(body.protection)}`
      : null,
    scope.protection ? `values:${protectionSummary(scope.protection)}` : null,
  ]
    .filter(Boolean)
    .join(" · ") || "none reported";
}

function protectionSummary(protection: import("../types/har").ProtectionCounts | undefined): string {
  if (!protection) return "";
  const modes = [
    protection.redacted ? `${protection.redacted} redacted` : null,
    protection.encrypted ? `${protection.encrypted} encrypted` : null,
    protection.tokenized ? `${protection.tokenized} tokenized` : null,
  ].filter(Boolean).join(", ");
  const fallbacks = Object.entries(protection.fallbacks ?? {})
    .filter(([, count]) => count > 0)
    .map(([reason, count]) => `${count} ${reason}`)
    .join(", ");
  return modes || fallbacks ? ` [${[modes, fallbacks && `fallback: ${fallbacks}`].filter(Boolean).join("; ")}]` : "";
}

function OverviewTab({ entry }: { entry: NEntry }) {
  const e = entry.e;
  return (
    <>
      <Section title="Exchange">
        <KV
          rows={[
            ["started", e.startedDateTime],
            ["total time", formatDuration(entry.timeMs)],
            ["method", entry.method],
            ["url", <span className="mono wrap">{entry.url}</span>],
            ["status", e.response ? `${e.response.status} ${e.response.statusText}`.trim() : "—"],
            ["state", <StateBadge state={entry.state} />],
            ["error", e._error ? `${e._error.phase}: ${e._error.message}` : ""],
            ["redirect", e.response?.redirectURL ? `→ ${e.response.redirectURL}` : ""],
            [
              "trace",
              entry.traceId ? (
                <span className="mono">
                  {entry.traceId}
                  {entry.redirectIndex != null ? ` (hop #${entry.redirectIndex})` : ""}
                </span>
              ) : (
                ""
              ),
            ],
            ["exchange id", e._exchangeId ? <span className="mono">{e._exchangeId}</span> : ""],
            ["server ip", e.serverIPAddress],
            ["connection", e.connection],
          ]}
        />
      </Section>
      <Section title="Timing waterfall">
        <Waterfall timings={e.timings} totalMs={entry.timeMs} compact />
      </Section>
      <Section title="Bodies">
        <KV
          rows={[
            ["request", bodySummary(e._requestBody) ?? (e.request?.postData ? "present" : "none")],
            ["response", bodySummary(e._responseBody) ?? "none"],
          ]}
        />
      </Section>
      {e._redaction ? (
        <Section title="Redaction audit">
          <KV
            rows={[
              ["request", redactionSummary(e._redaction.request)],
              ["response", redactionSummary(e._redaction.response)],
              ["errors", e._redaction.errors ? `${e._redaction.errors} changed` : ""],
              ["raw trace", e._redaction.rawTrace ? `${e._redaction.rawTrace} changed` : ""],
            ]}
          />
          <p className="muted note">Counts describe recorded values changed or body redactors executed; rule names and original values are never included.</p>
        </Section>
      ) : null}
    </>
  );
}

function TimingsTab({ entry }: { entry: NEntry }) {
  return (
    <Section title="HAR timings">
      <Waterfall timings={entry.e.timings} totalMs={entry.timeMs} />
      <p className="muted note">
        Values of -1 in the HAR mean the phase was not observed or does not apply (reused connections report -1 for
        dns/connect/ssl by design).
      </p>
    </Section>
  );
}

function RequestTab({ entry }: { entry: NEntry }) {
  const req = entry.e.request;
  const body = prettyPostData(req?.postData);
  return (
    <>
      <Section title="Request line">
        <KV
          rows={[
            ["method", req?.method],
            ["url", <span className="mono wrap">{req?.url}</span>],
            ["http version", req?.httpVersion || "unknown"],
            ["body size", formatBytes(req?.bodySize)],
            [
              "transfer encoding",
              entry.e._requestTransferEncoding?.join(", ") ?? "",
            ],
          ]}
        />
      </Section>
      <Section title="Headers">
        <PairsTable pairs={req?.headers} />
      </Section>
      <Section title="Query string">
        <PairsTable pairs={req?.queryString} />
      </Section>
      <Section title="Cookies">
        <CookiesTable cookies={req?.cookies} />
      </Section>
      <Section title="Body">
        {body.kind === "empty" ? (
          <EmptyState text={missingEmbeddedBodyText("request", entry.e._requestBody)} />
        ) : body.kind === "binary" ? (
          <BinaryBody body={body} />
        ) : (
          <CodeBlock
            text={body.text ?? ""}
            copyText={body.copyText}
            note={body.note ?? req?.postData?.mimeType}
            language={body.kind === "json" || body.kind === "xml" ? body.kind : undefined}
          />
        )}
      </Section>
      <BodyInfoSection title="_requestBody" info={entry.e._requestBody} />
      <Section title="Trailers">
        <PairsTable pairs={entry.e._requestTrailers} />
      </Section>
    </>
  );
}

function ResponseTab({ entry }: { entry: NEntry }) {
  const resp = entry.e.response;
  const body = prettyContent(resp?.content);
  return (
    <>
      <Section title="Status">
        <KV
          rows={[
            ["status", resp ? `${resp.status} ${resp.statusText}`.trim() : "—"],
            ["http version", resp?.httpVersion || "unknown"],
            ["mime type", resp?.content?.mimeType],
            ["content size", formatBytes(resp?.content?.size)],
            ["body size (wire)", formatBytes(resp?.bodySize)],
            ["decoded by recorder", resp?.content?._decoded ? <BoolMark v /> : ""],
            ["redirect url", resp?.redirectURL],
            ["transfer encoding", entry.e._responseTransferEncoding?.join(", ") ?? ""],
          ]}
        />
      </Section>
      <Section title="Headers">
        <PairsTable pairs={resp?.headers} />
      </Section>
      <Section title="Cookies">
        <CookiesTable cookies={resp?.cookies} />
      </Section>
      <Section title="Body">
        {body.kind === "empty" ? (
          <EmptyState text={missingEmbeddedBodyText("response", entry.e._responseBody)} />
        ) : body.kind === "binary" ? (
          <BinaryBody body={body} />
        ) : (
          <CodeBlock
            text={body.text ?? ""}
            copyText={body.copyText}
            note={body.note ?? `${body.kind} · ${resp?.content?.mimeType ?? ""}`}
            language={body.kind === "json" || body.kind === "xml" ? body.kind : undefined}
          />
        )}
      </Section>
      <BodyInfoSection title="_responseBody" info={entry.e._responseBody} />
      <Section title="Trailers">
        <PairsTable pairs={entry.e._responseTrailers} />
      </Section>
    </>
  );
}

function BinaryBody({ body }: { body: ReturnType<typeof prettyContent> }) {
  const [showImage, setShowImage] = useState(false);
  const [showVideo, setShowVideo] = useState(false);
  const [videoURL, setVideoURL] = useState<string>();
  const [videoError, setVideoError] = useState<string>();
  const base64 = body.text ?? "";
  const normalizedBase64 = base64.replace(/\s/g, "");
  useEffect(() => {
    if (!showVideo || !body.previewVideoMime) {
      setVideoURL(undefined);
      setVideoError(undefined);
      return;
    }
    try {
      const bytes = decodeBase64(normalizedBase64);
      const buffer = new ArrayBuffer(bytes.byteLength);
      new Uint8Array(buffer).set(bytes);
      const url = URL.createObjectURL(new Blob([buffer], { type: body.previewVideoMime }));
      setVideoURL(url);
      setVideoError(undefined);
      return () => URL.revokeObjectURL(url);
    } catch {
      setVideoError("invalid base64 video data");
    }
  }, [body.previewVideoMime, normalizedBase64, showVideo]);
  return (
    <>
      {body.previewImageMime ? (
        <div className="image-preview">
          <button type="button" className="copy-btn" onClick={() => setShowImage((v) => !v)}>
            {showImage ? "hide image" : "show image"}
          </button>
          {showImage ? (
            <img
              src={`data:${body.previewImageMime};base64,${normalizedBase64}`}
              alt="Captured response preview"
              loading="lazy"
              decoding="async"
            />
          ) : null}
        </div>
      ) : null}
      {body.previewVideoMime ? (
        <div className="media-preview">
          <button type="button" className="copy-btn" onClick={() => setShowVideo((v) => !v)}>
            {showVideo ? "hide video" : "show video"}
          </button>
          {showVideo && videoURL ? (
            <video
              controls
              preload="metadata"
              src={videoURL}
              onLoadedMetadata={() => setVideoError(undefined)}
              onError={(event) => {
                const code = event.currentTarget.error?.code;
                setVideoError(
                  code === MediaError.MEDIA_ERR_DECODE
                    ? "the browser could not decode this video's codec"
                    : "the video cannot be played; its codec may be unsupported or the capture may be incomplete",
                );
              }}
            >
              Captured video cannot be played by this browser.
            </video>
          ) : null}
          {showVideo && videoError ? <div className="media-error">{videoError}</div> : null}
        </div>
      ) : null}
      <CodeBlock text={base64} note={body.note ?? "binary body (base64 in HAR)"} />
    </>
  );
}

function BodyInfoSection({ title, info }: { title: string; info: BodyInfo | undefined }) {
  if (!info) return null;
  return (
    <Section title={title}>
      <KV
        rows={[
          ["complete", <BoolMark v={info.complete} />],
          ["closed early", info.closedEarly ? <BoolMark v /> : ""],
          ["truncated", info.truncated ? <BoolMark v /> : ""],
          ["captured bytes", formatBytes(info.capturedBytes)],
          ["total bytes", formatBytes(info.totalBytes)],
          [
            "hash",
            info.hash ? (
              <span className="mono wrap">
                {info.hashAlgorithm}:{info.hash} <CopyButton text={info.hash} />
              </span>
            ) : (
              ""
            ),
          ],
          ["read error", info.readError],
          ["close error", info.closeError],
          ["store", info.store ? <span className="mono wrap">{info.store}</span> : ""],
        ]}
      />
    </Section>
  );
}

function ErrorTab({ entry }: { entry: NEntry }) {
  const err = entry.e._error;
  if (!err) return <EmptyState text="this exchange completed without a transport error" />;
  return (
    <>
      <Section title="_error">
        <KV
          rows={[
            ["phase", <span className="badge phase">{err.phase}</span>],
            ["type", <span className="mono">{err.type}</span>],
            ["message", <span className="mono wrap">{err.message}</span>],
            ["timeout", <BoolMark v={err.timeout} />],
            ["temporary", <BoolMark v={err.temporary} />],
            ["context canceled", <BoolMark v={err.contextCanceled} />],
            ["context deadline exceeded", <BoolMark v={err.contextDeadlineExceeded} />],
            ["cause", err.cause],
          ]}
        />
      </Section>
      <Section title="Unwrap chain">
        {err.unwrapChain?.length ? (
          <ol className="unwrap-chain mono">
            {err.unwrapChain.map((t, i) => (
              <li key={i}>{t}</li>
            ))}
          </ol>
        ) : (
          <EmptyState text="no chain recorded" />
        )}
      </Section>
    </>
  );
}

function NetworkTab({ entry }: { entry: NEntry }) {
  const n = entry.e._network;
  if (!n) return <EmptyState text="no _network extension on this entry" />;
  return (
    <>
      <Section title="_network">
        <KV
          rows={[
            ["dns addresses", n.dnsAddresses?.length ? <span className="mono wrap">{n.dnsAddresses.join(", ")}</span> : ""],
            ["dns coalesced", n.dnsCoalesced ? <BoolMark v /> : ""],
            ["network", n.network],
            ["local address", n.localAddress ? <span className="mono">{n.localAddress}</span> : ""],
            ["remote address", n.remoteAddress ? <span className="mono">{n.remoteAddress}</span> : ""],
            ["ip version", n.ipVersion],
            ["connection reused", <BoolMark v={n.connectionReused} />],
            ["was idle", <BoolMark v={n.wasIdle} />],
            ["idle time", n.idleTimeMs != null ? formatDuration(n.idleTimeMs) : ""],
            ["proxy", n.proxy ? <span className="mono wrap">{n.proxy}</span> : ""],
            ["http/2", <BoolMark v={n.http2} />],
            [
              "returned to idle pool",
              n.putIdle ? (n.putIdle.returned ? <BoolMark v /> : `no — ${n.putIdle.error ?? "unknown reason"}`) : "",
            ],
          ]}
        />
      </Section>
      {entry.e._expect100 ? (
        <Section title="_expect100">
          <KV
            rows={[
              ["waited", <BoolMark v={entry.e._expect100.waited} />],
              ["100 received", <BoolMark v={entry.e._expect100.continueReceived} />],
              ["wait", entry.e._expect100.waitMs != null ? formatDuration(entry.e._expect100.waitMs) : ""],
            ]}
          />
        </Section>
      ) : null}
      {entry.e._informational?.length ? (
        <Section title="_informational (1xx interim responses)">
          {entry.e._informational.map((ir, i) => (
            <div key={i} className="informational">
              <StatusBadge status={ir.status} />
              <PairsTable pairs={ir.headers} />
            </div>
          ))}
        </Section>
      ) : null}
    </>
  );
}

function TlsTab({ entry }: { entry: NEntry }) {
  const t = entry.e._tls;
  if (!t) return <EmptyState text="no _tls extension (plain HTTP, or TLS not captured)" />;
  return (
    <>
      <Section title="_tls">
        <KV
          rows={[
            ["version", t.version],
            ["cipher suite", <span className="mono">{t.cipherSuite}</span>],
            ["alpn", t.negotiatedProtocol],
            ["server name (sni)", t.serverName],
            ["handshake complete", <BoolMark v={t.handshakeComplete} />],
            ["tls session resumed", <BoolMark v={t.didResume} />],
            ["ocsp stapled", <BoolMark v={t.ocspStapled} />],
            ["sct count", String(t.sctCount ?? 0)],
            ["verified chains", String(t.verifiedChains ?? 0)],
          ]}
        />
        {entry.e._network?.connectionReused ? (
          <p className="note muted">
            TLS state was inherited from a reused connection; no TLS handshake occurred during this exchange.
          </p>
        ) : null}
      </Section>
      <Section title={`Certificate chain (${t.peerCertificates?.length ?? 0})`}>
        {t.peerCertificates?.length ? (
          t.peerCertificates.map((c, i) => <CertCard key={i} cert={c} index={i} />)
        ) : (
          <EmptyState text="certificates not captured" />
        )}
      </Section>
    </>
  );
}

function CertCard({ cert, index }: { cert: CertInfo; index: number }) {
  const [showRaw, setShowRaw] = useState(false);
  return (
    <div className="cert">
      <div className="cert-title mono">
        #{index} {cert.subject}
      </div>
      <KV
        rows={[
          ["issuer", <span className="mono wrap">{cert.issuer}</span>],
          ["serial", <span className="mono">{cert.serialNumber}</span>],
          ["valid", `${cert.notBefore} → ${cert.notAfter}`],
          ["dns names", cert.dnsNames?.join(", ") ?? ""],
          ["ip addresses", cert.ipAddresses?.join(", ") ?? ""],
          ["public key", cert.publicKeyAlgorithm],
          ["signature", cert.signatureAlgorithm],
          [
            "sha-256",
            <span className="mono wrap">
              {cert.sha256Fingerprint} <CopyButton text={cert.sha256Fingerprint} />
            </span>,
          ],
        ]}
      />
      {cert.rawDER ? (
        <div>
          <button type="button" className="link-btn" onClick={() => setShowRaw(!showRaw)}>
            {showRaw ? "hide raw DER (base64)" : "show raw DER (base64)"}
          </button>
          {showRaw ? <CodeBlock text={cert.rawDER} /> : null}
        </div>
      ) : null}
    </div>
  );
}

function TraceTab({ entry }: { entry: NEntry }) {
  const [filter, setFilter] = useState("");
  const events = entry.e._trace ?? [];
  const baseMs = events.length ? parseIsoMs(events[0].time) : null;
  const shown = useMemo(() => {
    const q = filter.trim().toLowerCase();
    if (!q) return events;
    return events.filter(
      (ev) => ev.name.toLowerCase().includes(q) || (ev.detail ?? "").toLowerCase().includes(q),
    );
  }, [events, filter]);

  if (events.length === 0) {
    return <EmptyState text="no _trace events (enable WithCaptureRawTrace to record raw httptrace events)" />;
  }
  return (
    <Section
      title={`Raw httptrace events (${shown.length}/${events.length})`}
      actions={
        <input
          type="search"
          className="trace-filter"
          placeholder="filter events…"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
        />
      }
    >
      <table className="pairs-table trace-table">
        <thead>
          <tr>
            <th>rel</th>
            <th>event</th>
            <th>detail</th>
          </tr>
        </thead>
        <tbody>
          {shown.map((ev, i) => (
            <tr key={i}>
              <td className="mono muted">{relMs(baseMs, parseIsoMs(ev.time))}</td>
              <td className="pair-name">{ev.name}</td>
              <td className="pair-value mono">{ev.detail ?? ""}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </Section>
  );
}

function RawTab({ entry }: { entry: NEntry }) {
  const ext = extensionFields(entry.e);
  const json = JSON.stringify(entry.e, null, 2);
  return (
    <>
      <Section title="Recorder extensions (all _ fields)">
        {Object.keys(ext).length ? <JsonTree value={ext} /> : <EmptyState text="no extension fields" />}
      </Section>
      <Section title="Entry JSON" actions={<CopyButton text={json} label="copy JSON" />}>
        <CodeBlock text={json.length > 400_000 ? `${json.slice(0, 400_000)}\n… (truncated view)` : json} />
      </Section>
    </>
  );
}
