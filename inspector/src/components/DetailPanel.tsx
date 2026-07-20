import { useEffect, useMemo, useState } from "react";
import { AlertTriangle, ArrowLeft, Info, Network, Terminal } from "lucide-react";
import type { BodyInfo, CertInfo, NEntry, ProtectionCounts, RedactionScopeInfo } from "../types/har";
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
  decryptProtectedTokens,
  protectedOccurrences,
  verifyProtectedToken,
  withDecryptedValues,
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

const TABS = ["Overview", "Timings", "Request", "Response", "Error", "Network", "TLS", "Trace", "Raw", "Redaction", "Protection", "Replay"] as const;
type Tab = (typeof TABS)[number];

export function DetailPanel({ entry, entries, decryptedValues, onDecrypted, keyInputs, onKeyInput, onBack }: {
  entry: NEntry;
  entries: NEntry[];
  decryptedValues: ReadonlyMap<string, string>;
  onDecrypted: (values: ReadonlyMap<string, string>) => void;
  keyInputs: ReadonlyMap<string, string>;
  onKeyInput: (group: string, value: string) => void;
  onBack: () => void;
}) {
  const [tab, setTab] = useState<Tab>("Overview");
  const decryptedEntry = useMemo(() => withDecryptedValues(entry, decryptedValues), [entry, decryptedValues]);
  const decryptedCount = useMemo(() => protectedOccurrences(entry.e)
    .filter((occurrence) => decryptedValues.has(occurrence.token)).length, [entry.e, decryptedValues]);
  return (
    <div className="detail">
      <div className="detail-head">
        <button type="button" className="icon-btn back-btn" onClick={onBack} data-tooltip="Back to request list" aria-label="Back to request list">
          <ArrowLeft size={15} />
        </button>
        <span className="badge method" data-tooltip={`HTTP method: ${entry.method}`}>{entry.method}</span>
        <span className="detail-url mono" title={decryptedEntry.url}>
          {decryptedEntry.host}
          <span className="muted">{decryptedEntry.path}</span>
        </span>
        <StatusBadge status={entry.status} />
        <StateBadge state={entry.state} />
        {decryptedCount > 0 && <span className="badge" data-tooltip="Protected values are shown decrypted in this in-memory view">decrypted in memory</span>}
      </div>
      <nav className="tabs">
        {TABS.map((t) => (
          <button key={t} type="button" className={t === tab ? "tab active" : "tab"} onClick={() => setTab(t)}>
            {t}
            {t === "Error" && entry.e._error ? <span className="tab-dot" /> : null}
            {t === "Redaction" && entry.e._redaction ? <span className="tab-dot audit" /> : null}
          </button>
        ))}
      </nav>
      <div className="detail-body">
        {tab === "Overview" && <OverviewTab entry={decryptedEntry} />}
        {tab === "Timings" && <TimingsTab entry={decryptedEntry} />}
        {tab === "Request" && <RequestTab entry={decryptedEntry} />}
        {tab === "Redaction" && <RedactionAuditTab entry={entry} />}
        {tab === "Protection" && (
          <ProtectionTab
            entry={entry}
            entries={entries}
            decryptedValues={decryptedValues}
            onDecrypted={onDecrypted}
            keyInputs={keyInputs}
            onKeyInput={onKeyInput}
          />
        )}
        {tab === "Replay" && <ReplayTab entry={entry} decryptedValues={decryptedValues} />}
        {tab === "Response" && <ResponseTab entry={decryptedEntry} />}
        {tab === "Error" && <ErrorTab entry={decryptedEntry} />}
        {tab === "Network" && <NetworkTab entry={decryptedEntry} />}
        {tab === "TLS" && <TlsTab entry={decryptedEntry} />}
        {tab === "Trace" && <TraceTab entry={decryptedEntry} />}
        {tab === "Raw" && <RawTab entry={decryptedEntry} decrypted={decryptedCount > 0} />}
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
    <div className="replay-page">
      <div className="replay-hero">
        <div className="replay-hero-icon"><Terminal size={20} /></div>
        <div>
          <h2>Replay request</h2>
          <p>Generate a reviewable cURL command from the recorded exchange.</p>
        </div>
        <div className="replay-meta">
          <span className="badge method">{entry.method}</span>
          <span className="badge">POSIX shell</span>
        </div>
      </div>

      <section className="replay-card replay-options-card">
        <div className="replay-card-head">
          <div>
            <h3>Replay options</h3>
            <p>Optional values are included only in this generated command.</p>
          </div>
        </div>
        <div className="replay-options">
          <label
          className="replay-option-row"
          data-tooltip={hasLocalAddress
            ? "Add the recorded local IP using curl --interface"
            : "No local address was recorded for this exchange"}
          >
            <span className="replay-option-icon"><Network size={16} /></span>
            <span className="replay-option-copy">
              <strong>Recorded local interface</strong>
              <small>{hasLocalAddress ? entry.e._network?.localAddress : "No local address was captured"}</small>
            </span>
            <input
              className="replay-switch"
              type="checkbox"
              checked={includeLocalInterface}
              disabled={!hasLocalAddress}
              onChange={(event) => setIncludeLocalInterface(event.target.checked)}
            />
          </label>
          <label
          className="replay-option-row"
          data-tooltip={requestDecrypted.size
            ? "Insert decrypted request values into this in-memory cURL command"
            : "Decrypt request values in the Protection tab first"}
          >
            <span className="replay-option-icon"><span className="mono">{requestDecrypted.size}/{requestTokens.length}</span></span>
            <span className="replay-option-copy">
              <strong>Decrypted request values</strong>
              <small>{requestDecrypted.size ? "Insert decrypted values from this browser session" : "Decrypt request values in Protection first"}</small>
            </span>
            <input
              className="replay-switch"
              type="checkbox"
              checked={includeDecryptedValues && requestDecrypted.size > 0}
              disabled={requestDecrypted.size === 0}
              onChange={(event) => setIncludeDecryptedValues(event.target.checked)}
            />
          </label>
        </div>
      </section>

      <section className="replay-card replay-command-card">
        <div className="replay-card-head">
          <div>
            <h3>cURL command</h3>
            <p>Inspect the generated command before running or sharing it.</p>
          </div>
          <span className="replay-ready"><span /> ready</span>
        </div>
        {replay.command ? (
          <CodeBlock text={replay.command} note="POSIX shell" language="shell" />
        ) : (
          <EmptyState text="no request recorded" />
        )}
      </section>

      <section className="replay-card replay-notes-card">
        <div className="replay-card-head">
          <div>
            <h3>Review before running</h3>
            <p>Replay commands may contain sensitive or incomplete recorded data.</p>
          </div>
        </div>
        <div className="replay-warnings">
          {replay.warnings.map((warning) => {
            const caution = /REDACTED|Encrypted|partial|truncated|incomplete/i.test(warning);
            return (
              <div className={`replay-warning ${caution ? "caution" : "info"}`} key={warning}>
                {caution ? <AlertTriangle size={15} /> : <Info size={15} />}
                <span>{warning}</span>
              </div>
            );
          })}
        </div>
      </section>
    </div>
  );
}

function ProtectionTab({
  entry,
  entries,
  decryptedValues,
  onDecrypted,
  keyInputs,
  onKeyInput,
}: {
  entry: NEntry;
  entries: NEntry[];
  decryptedValues: ReadonlyMap<string, string>;
  onDecrypted: (values: ReadonlyMap<string, string>) => void;
  keyInputs: ReadonlyMap<string, string>;
  onKeyInput: (keyId: string, value: string) => void;
}) {
  const occurrences = useMemo(() => entries.flatMap((item) =>
    protectedOccurrences(item.e).map((occurrence) => ({ ...occurrence, entryId: item.id }))), [entries]);
  const groups = useMemo(() => {
    const result = new Map<string, Array<ProtectedOccurrence & { entryId: number }>>();
    for (const occurrence of occurrences) {
      const key = `${occurrence.mode}:${occurrence.keyId}`;
      const group = result.get(key) ?? [];
      group.push(occurrence);
      result.set(key, group);
    }
    return [...result.entries()];
  }, [occurrences]);
  if (groups.length === 0) return <EmptyState text="no encrypted or tokenized values detected in this HAR" />;
  return (
    <>
      <Section title="Protected values">
        <p className="muted protection-intro">
          Enter each key once, then process this exchange or the entire HAR. Work runs in bounded batches and keys and
          plaintext stay in memory only until another HAR is loaded. Replay remains a separate opt-in.
        </p>
        <div className="protection-list">
          {groups.map(([groupKey, group]) => (
            <ProtectionKeyGroup
              key={groupKey}
              groupKey={groupKey}
              occurrences={group}
              selectedEntryId={entry.id}
              decryptedValues={decryptedValues}
              keyInput={keyInputs.get(groupKey) ?? ""}
              onKeyInput={(value) => onKeyInput(groupKey, value)}
              onDecrypted={onDecrypted}
            />
          ))}
        </div>
      </Section>
    </>
  );
}

function ProtectionKeyGroup({
  groupKey,
  occurrences,
  selectedEntryId,
  decryptedValues,
  keyInput,
  onKeyInput,
  onDecrypted,
}: {
  groupKey: string;
  occurrences: Array<ProtectedOccurrence & { entryId: number }>;
  selectedEntryId: number;
  decryptedValues: ReadonlyMap<string, string>;
  keyInput: string;
  onKeyInput: (value: string) => void;
  onDecrypted: (values: ReadonlyMap<string, string>) => void;
}) {
  const mode = occurrences[0].mode;
  const keyId = occurrences[0].keyId;
  const selected = occurrences.filter((item) => item.entryId === selectedEntryId);
  const uniqueAll = [...new Map(occurrences.map((item) => [item.token, item])).values()];
  const uniqueSelected = [...new Map(selected.map((item) => [item.token, item])).values()];
  const decryptedCount = uniqueAll.filter((item) => decryptedValues.has(item.token)).length;
  const [candidate, setCandidate] = useState("");
  const [status, setStatus] = useState("");
  const [busy, setBusy] = useState(false);

  const decrypt = async (scope: "exchange" | "har") => {
    const targets = scope === "exchange" ? uniqueSelected : uniqueAll;
    setStatus("");
    setBusy(true);
    try {
      const result = await decryptProtectedTokens(targets, keyInput, (progress) => {
        setStatus(`decrypting ${progress.completed.toLocaleString()} / ${progress.total.toLocaleString()} · ${progress.failures.toLocaleString()} failed`);
      });
      onDecrypted(result.values);
      setStatus(`${result.values.size.toLocaleString()} decrypted · ${result.failures.toLocaleString()} failed`);
    } catch (error) {
      setStatus(error instanceof Error ? error.message : "Protection operation failed.");
    } finally {
      setBusy(false);
    }
  };

  const verify = async (scope: "exchange" | "har") => {
    const targets = scope === "exchange" ? uniqueSelected : uniqueAll;
    setStatus("");
    setBusy(true);
    try {
      let matches = 0;
      let completed = 0;
      for (let offset = 0; offset < targets.length; offset += 64) {
        const batch = targets.slice(offset, offset + 64);
        const results = await Promise.all(batch.map((item) => verifyProtectedToken(item, candidate, keyInput)));
        matches += results.filter(Boolean).length;
        completed += batch.length;
        setStatus(`verifying ${completed.toLocaleString()} / ${targets.length.toLocaleString()}`);
        await new Promise<void>((resolve) => setTimeout(resolve, 0));
      }
      setStatus(`${matches.toLocaleString()} of ${targets.length.toLocaleString()} tokenized values match the candidate`);
    } catch (error) {
      setStatus(error instanceof Error ? error.message : "Protection operation failed.");
    } finally {
      setBusy(false);
    }
  };

  const visibleSelected = selected.slice(0, 100);
  return (
    <div className="protection-card">
      <div className="protection-card-head">
        <span className="badge">{mode === "encrypt" ? "encrypted" : "tokenized"}</span>
        <strong className="mono">key: {keyId}</strong>
        <span className="muted protection-group-id">{groupKey}</span>
      </div>
      <div className="protection-group-stats">
        <span><strong>{occurrences.length.toLocaleString()}</strong> occurrences</span>
        <span><strong>{uniqueAll.length.toLocaleString()}</strong> unique in HAR</span>
        <span><strong>{uniqueSelected.length.toLocaleString()}</strong> in this exchange</span>
        {mode === "encrypt" && <span><strong>{decryptedCount.toLocaleString()}</strong> decrypted</span>}
      </div>
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
      {mode === "tokenize" && (
        <label className="protection-field">
          candidate value
          <textarea value={candidate} onChange={(event) => setCandidate(event.target.value)} />
        </label>
      )}
      <div className="protection-actions">
        <button type="button" className="btn" disabled={busy || !keyInput || uniqueSelected.length === 0 || (mode === "tokenize" && !candidate)} onClick={() => mode === "encrypt" ? decrypt("exchange") : verify("exchange")}>
          {mode === "encrypt" ? "decrypt this exchange" : "verify this exchange"}
        </button>
        <button type="button" className="btn primary" disabled={busy || !keyInput || (mode === "tokenize" && !candidate)} onClick={() => mode === "encrypt" ? decrypt("har") : verify("har")}>
          {mode === "encrypt" ? "decrypt all in HAR" : "verify all in HAR"}
        </button>
      </div>
      {status && <div className="protection-result" role="status">{status}</div>}
      {selected.length > 0 && (
        <details className="protection-values">
          <summary>values in this exchange ({selected.length.toLocaleString()})</summary>
          <div className="protection-value-list">
            {visibleSelected.map((occurrence, index) => {
              const plain = decryptedValues.get(occurrence.token);
              return (
                <div className="protection-value-row" key={`${occurrence.path}-${occurrence.token}-${index}`}>
                  <span className="mono wrap">{occurrence.path}</span>
                  <span className={plain == null ? "muted" : "bool-yes"}>{plain == null ? "protected" : "decrypted"}</span>
                  {plain != null && <CopyButton text={plain} label="copy plaintext" />}
                  <code title={plain ?? occurrence.token}>{truncateProtectionValue(plain ?? occurrence.token)}</code>
                </div>
              );
            })}
          </div>
          {selected.length > visibleSelected.length && <p className="muted note">Showing the first 100 occurrences.</p>}
        </details>
      )}
    </div>
  );
}

function truncateProtectionValue(value: string): string {
  return value.length > 160 ? `${value.slice(0, 160)}…` : value;
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
    </>
  );
}

function RedactionAuditTab({ entry }: { entry: NEntry }) {
  const audit = entry.e._redaction;
  if (!audit) {
    return (
      <div className="audit-empty">
        <EmptyState text="no redaction audit metadata recorded for this exchange" />
        <p className="muted note">
          This means no audit event was reported. It does not prove that sensitive data is absent or that an older HAR
          was recorded without redaction.
        </p>
      </div>
    );
  }
  const requestEvents = auditScopeEvents(audit.request);
  const responseEvents = auditScopeEvents(audit.response);
  const globalEvents = (audit.errors ?? 0) + (audit.rawTrace ?? 0);
  return (
    <>
      <Section title="Redaction audit overview">
        <div className="audit-hero">
          <AuditMetric label="request events" value={requestEvents} tone="request" />
          <AuditMetric label="response events" value={responseEvents} tone="response" />
          <AuditMetric label="error / trace" value={globalEvents} tone="global" />
          <AuditMetric label="total reported" value={requestEvents + responseEvents + globalEvents} tone="total" />
        </div>
        <p className="muted audit-note">
          Counts describe changes to the recorded copy only. The audit deliberately excludes rule names, original
          values, protected tokens, key IDs, and internal error details.
        </p>
      </Section>
      <div className="audit-scope-grid">
        <AuditScopeCard title="Request" scope={audit.request} />
        <AuditScopeCard title="Response" scope={audit.response} />
      </div>
      <Section title="Other sanitized data">
        <div className="audit-global-grid">
          <AuditMetric label="error messages changed" value={audit.errors ?? 0} tone="global" />
          <AuditMetric label="raw trace details changed" value={audit.rawTrace ?? 0} tone="global" />
        </div>
        <p className="muted audit-note">
          These counters cover application-configured sanitization of exported error text and raw httptrace details.
        </p>
      </Section>
    </>
  );
}

function AuditScopeCard({ title, scope }: { title: string; scope: RedactionScopeInfo | undefined }) {
  return (
    <section className="audit-scope-card">
      <div className="audit-scope-head">
        <h3>{title}</h3>
        <span className="audit-total">{auditScopeEvents(scope)} reported</span>
      </div>
      {!scope ? (
        <EmptyState text={`no ${title.toLowerCase()}-side audit events`} />
      ) : (
        <>
          <div className="audit-category-grid">
            <AuditMetric label="URL" value={scope.url ?? 0} />
            <AuditMetric label="headers" value={scope.headers ?? 0} />
            <AuditMetric label="query parameters" value={scope.queryParameters ?? 0} />
            <AuditMetric label="cookies" value={scope.cookies ?? 0} />
          </div>
          <div className="audit-subsection">
            <h4>Value protection</h4>
            <ProtectionBreakdown protection={scope.protection} />
          </div>
          <div className="audit-subsection">
            <h4>Body redactor</h4>
            {scope.body ? (
              <>
                <div className="audit-body-grid">
                  <div><span>kind</span><strong className="mono">{scope.body.kind}</strong></div>
                  <div><span>outcome</span><AuditOutcome outcome={scope.body.outcome} /></div>
                  <div><span>replacements</span><strong>{scope.body.replacements ?? "not reported"}</strong></div>
                </div>
                <ProtectionBreakdown protection={scope.body.protection} emptyText="no body protection outcomes reported" />
              </>
            ) : (
              <EmptyState text="no body redactor audit" />
            )}
          </div>
        </>
      )}
    </section>
  );
}

function ProtectionBreakdown({ protection, emptyText = "no value protection outcomes reported" }: {
  protection: ProtectionCounts | undefined;
  emptyText?: string;
}) {
  const fallbacks = Object.entries(protection?.fallbacks ?? {}).filter(([, count]) => count > 0);
  if (!protection || (protectionCount(protection) === 0 && fallbacks.length === 0)) {
    return <EmptyState text={emptyText} />;
  }
  return (
    <div className="audit-protection">
      <div className="audit-mode-grid">
        <AuditMetric label="redacted" value={protection.redacted ?? 0} tone="redacted" />
        <AuditMetric label="encrypted" value={protection.encrypted ?? 0} tone="encrypted" />
        <AuditMetric label="tokenized" value={protection.tokenized ?? 0} tone="tokenized" />
      </div>
      {fallbacks.length > 0 && (
        <div className="audit-fallbacks">
          <div className="audit-fallback-head"><span>Fail-closed fallback reason</span><span>count</span></div>
          {fallbacks.map(([reason, count]) => (
            <div className="audit-fallback-row" key={reason}>
              <code>{reason}</code><strong>{count}</strong>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function AuditMetric({ label, value, tone = "default" }: { label: string; value: number; tone?: string }) {
  return (
    <div className={`audit-metric ${tone}`}>
      <strong>{value.toLocaleString()}</strong>
      <span>{label}</span>
    </div>
  );
}

function AuditOutcome({ outcome }: { outcome: string }) {
  const known = ["processed", "redacted", "unchanged", "failed"].includes(outcome) ? outcome : "unknown";
  return <span className={`audit-outcome ${known}`}>{outcome}</span>;
}

function protectionCount(protection: ProtectionCounts | undefined): number {
  return (protection?.redacted ?? 0) + (protection?.encrypted ?? 0) + (protection?.tokenized ?? 0);
}

function auditScopeEvents(scope: RedactionScopeInfo | undefined): number {
  if (!scope) return 0;
  return (scope.url ?? 0) + (scope.headers ?? 0) + (scope.queryParameters ?? 0) + (scope.cookies ?? 0)
    + (scope.body?.replacements ?? 0);
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

function RawTab({ entry, decrypted = false }: { entry: NEntry; decrypted?: boolean }) {
  const ext = extensionFields(entry.e);
  const json = JSON.stringify(entry.e, null, 2);
  return (
    <>
      <Section title="Recorder extensions (all _ fields)">
        {Object.keys(ext).length ? <JsonTree value={ext} /> : <EmptyState text="no extension fields" />}
      </Section>
      <Section title={decrypted ? "Entry JSON (decrypted in-memory view)" : "Entry JSON"} actions={<CopyButton text={json} label="copy JSON" />}>
        <CodeBlock text={json.length > 400_000 ? `${json.slice(0, 400_000)}\n… (truncated view)` : json} />
      </Section>
    </>
  );
}
