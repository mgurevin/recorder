import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { AlertTriangle, ArrowLeft, Braces, Clock3, Database, Info, Network, Route, ShieldCheck, ShieldX, Terminal } from "lucide-react";
import type { BodyInfo, CertInfo, NEntry, PostParam, ProtectionCounts, RedactionScopeInfo } from "../types/har";
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
import { curlReplay, supportsReplayBodyFormatting } from "../lib/curl";
import {
  decryptProtectedTokens,
  protectedOccurrences,
  verifyProtectedTokens,
  withResolvedValues,
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
  SegmentedControl,
  Section,
  StateBadge,
  StatusBadge,
  moveTabFocus,
} from "./Shared";
import { Waterfall } from "./Waterfall";

const TABS = ["Summary", "Request", "Response", "Connection", "Diagnostics", "Privacy", "Replay", "Raw"] as const;
type Tab = (typeof TABS)[number];

export function DetailPanel({ entry, entries, resolvedValues, onResolved, onClearResolved, protectionClearEpoch, keyInputs, onKeyInput, onProtectionKeyActivated, onBack }: {
  entry: NEntry;
  entries: NEntry[];
  resolvedValues: ReadonlyMap<string, string>;
  onResolved: (values: ReadonlyMap<string, string>) => void;
  onClearResolved: () => void;
  protectionClearEpoch: number;
  keyInputs: ReadonlyMap<string, string>;
  onKeyInput: (group: string, value: string) => void;
  onProtectionKeyActivated: (group: string, value: string) => void;
  onBack: () => void;
}) {
  const [tab, setTab] = useState<Tab>("Summary");
  const resolvedEntry = useMemo(() => withResolvedValues(entry, resolvedValues), [entry, resolvedValues]);
  const resolvedOccurrences = useMemo(() => protectedOccurrences(entry.e)
    .filter((occurrence) => resolvedValues.has(occurrence.token)), [entry.e, resolvedValues]);
  return (
    <div className="detail">
      <div className="detail-head">
        <button type="button" className="icon-btn back-btn" onClick={onBack} data-tooltip="Back to request list" aria-label="Back to request list">
          <ArrowLeft size={15} />
        </button>
        <span className="badge method" data-tooltip={`HTTP method: ${entry.method}`}>{entry.method}</span>
        <span className="detail-url mono" title={resolvedEntry.url}>
          {resolvedEntry.host}
          <span className="muted">{resolvedEntry.path}</span>
        </span>
        <StatusBadge status={entry.status} />
        <StateBadge state={entry.state} />
        {resolvedOccurrences.length > 0 && (
          <button
            type="button"
            className="badge resolved-memory-btn"
            onClick={onClearResolved}
            data-tooltip="Forget all plaintext, candidate values, and protection keys; return to the original HAR view"
          >
            <ShieldX size={12} /> clear resolved data
          </button>
        )}
      </div>
      <nav className="tabs" role="tablist" aria-label="Exchange details">
        {TABS.map((t) => (
          <button
            key={t}
            type="button"
            role="tab"
            aria-selected={t === tab}
            tabIndex={t === tab ? 0 : -1}
            className={t === tab ? "tab active" : "tab"}
            onClick={() => setTab(t)}
            onKeyDown={(event) => moveTabFocus(event, TABS, t, setTab)}
          >
            {t}
            {t === "Diagnostics" && entry.e._recorder?.error ? <span className="tab-dot" /> : null}
            {t === "Privacy" && entry.e._recorder?.redaction ? <span className="tab-dot audit" /> : null}
          </button>
        ))}
      </nav>
      <div className="detail-body" role="tabpanel" aria-label={tab}>
        {tab === "Summary" && <OverviewTab entry={resolvedEntry} />}
        {tab === "Request" && <RequestTab entry={resolvedEntry} />}
        {tab === "Response" && <ResponseTab entry={resolvedEntry} />}
        {tab === "Connection" && <ConnectionWorkspace entry={resolvedEntry} />}
        {tab === "Diagnostics" && <DiagnosticsWorkspace entry={resolvedEntry} />}
        {tab === "Privacy" && (
          <PrivacyWorkspace
            entry={entry}
            entries={entries}
            resolvedValues={resolvedValues}
            onResolved={onResolved}
            clearEpoch={protectionClearEpoch}
            keyInputs={keyInputs}
            onKeyInput={onKeyInput}
            onProtectionKeyActivated={onProtectionKeyActivated}
          />
        )}
        {tab === "Replay" && <ReplayTab entry={entry} resolvedValues={resolvedValues} />}
        {tab === "Raw" && <RawTab entry={resolvedEntry} resolved={resolvedOccurrences.length > 0} />}
      </div>
    </div>
  );
}

function ConnectionWorkspace({ entry }: { entry: NEntry }) {
  const options = ["Timing", "Network", "TLS"] as const;
  const [view, setView] = useState<(typeof options)[number]>("Timing");
  const network = entry.e._recorder?.network;
  const tls = entry.e._recorder?.tls;
  const observedPhases = Object.values(entry.e.timings ?? {}).filter((duration) => typeof duration === "number" && duration >= 0).length;

  return (
    <div className="workspace-page">
      <WorkspaceHeader
        title="Connection"
        description="Review request phases, socket reuse, proxy routing, TLS negotiation, and peer identity."
      />
      <div className="workspace-snapshot" aria-label="Connection summary">
        <WorkspaceFact icon={<Clock3 size={15} />} label="Observed phases" value={`${observedPhases} of 7`} />
        <WorkspaceFact
          icon={<Network size={15} />}
          label="Connection"
          value={network ? (network.connectionReused ? "Reused" : "New") : "Not recorded"}
        />
        <WorkspaceFact icon={<Route size={15} />} label="Route" value={network?.proxy ? "Proxy" : network ? "Direct" : "Not recorded"} />
        <WorkspaceFact icon={<ShieldCheck size={15} />} label="TLS" value={tls?.version ?? (entry.url.startsWith("https:") ? "Not captured" : "Plain HTTP")} />
      </div>
      <SegmentedControl label="Connection detail" value={view} options={options} onChange={setView} />
      <div className="workspace-content">
        {view === "Timing" && <TimingsTab entry={entry} />}
        {view === "Network" && <NetworkTab entry={entry} />}
        {view === "TLS" && <TlsTab entry={entry} />}
      </div>
    </div>
  );
}

function DiagnosticsWorkspace({ entry }: { entry: NEntry }) {
  const options = ["Error", "Trace"] as const;
  const [view, setView] = useState<(typeof options)[number]>(entry.e._recorder?.error ? "Error" : "Trace");
  const error = entry.e._recorder?.error;
  const trace = entry.e._recorder?.trace ?? [];
  const traceStart = trace.length > 0 ? parseIsoMs(trace[0].time) : null;
  const traceEnd = trace.length > 0 ? parseIsoMs(trace[trace.length - 1].time) : null;
  const traceSpan = traceStart != null && traceEnd != null ? Math.max(0, traceEnd - traceStart) : null;

  return (
    <div className="workspace-page">
      <WorkspaceHeader
        title="Diagnostics"
        description="Inspect transport failures and the ordered httptrace evidence recorded for this exchange."
      />
      <div className={`diagnostic-banner ${error ? "failure" : "success"}`} role="status">
        {error ? <AlertTriangle size={17} /> : <ShieldCheck size={17} />}
        <div>
          <strong>{error ? `${error.phase || "transport"} failure recorded` : "No transport failure recorded"}</strong>
          <span>{error?.message ?? "The exchange completed without recorder-observed transport errors."}</span>
        </div>
      </div>
      <div className="workspace-snapshot" aria-label="Diagnostic summary">
        <WorkspaceFact icon={<AlertTriangle size={15} />} label="Outcome" value={error ? "Failed" : "Completed"} tone={error ? "danger" : "success"} />
        <WorkspaceFact icon={<Terminal size={15} />} label="Error type" value={error?.type ?? "None"} />
        <WorkspaceFact icon={<Database size={15} />} label="Trace events" value={trace.length.toLocaleString()} />
        <WorkspaceFact icon={<Clock3 size={15} />} label="Trace span" value={traceSpan == null ? "Not observed" : formatDuration(traceSpan)} />
      </div>
      <SegmentedControl label="Diagnostic detail" value={view} options={options} onChange={setView} />
      <div className="workspace-content">
        {view === "Error" && <ErrorTab entry={entry} />}
        {view === "Trace" && <TraceTab entry={entry} />}
      </div>
    </div>
  );
}

function PrivacyWorkspace({ entry, entries, resolvedValues, onResolved, clearEpoch, keyInputs, onKeyInput, onProtectionKeyActivated }: {
  entry: NEntry;
  entries: NEntry[];
  resolvedValues: ReadonlyMap<string, string>;
  onResolved: (values: ReadonlyMap<string, string>) => void;
  clearEpoch: number;
  keyInputs: ReadonlyMap<string, string>;
  onKeyInput: (group: string, value: string) => void;
  onProtectionKeyActivated: (group: string, value: string) => void;
}) {
  const options = ["Audit", "Protected values"] as const;
  const [view, setView] = useState<(typeof options)[number]>("Audit");

  return (
    <div className="workspace-page">
      <WorkspaceHeader
        title="Privacy"
        description="Review sanitization outcomes and resolve protected values locally without modifying the capture."
      />
      <SegmentedControl label="Privacy detail" value={view} options={options} onChange={setView} />
      <div className="workspace-content">
        {view === "Audit" && <RedactionAuditTab entry={entry} />}
        {view === "Protected values" && (
          <ProtectionTab
            entry={entry}
            entries={entries}
            resolvedValues={resolvedValues}
            onResolved={onResolved}
            clearEpoch={clearEpoch}
            keyInputs={keyInputs}
            onKeyInput={onKeyInput}
            onProtectionKeyActivated={onProtectionKeyActivated}
          />
        )}
      </div>
    </div>
  );
}

function WorkspaceHeader({ title, description }: { title: string; description: string }) {
  return (
    <header className="workspace-header">
      <h2>{title}</h2>
      <p>{description}</p>
    </header>
  );
}

function WorkspaceFact({ icon, label, value, tone = "default" }: {
  icon: ReactNode;
  label: string;
  value: string;
  tone?: "default" | "success" | "danger";
}) {
  return (
    <div className={`workspace-fact ${tone}`}>
      <span className="workspace-fact-icon">{icon}</span>
      <span><small>{label}</small><strong title={value}>{value}</strong></span>
    </div>
  );
}

function ReplayTab({ entry, resolvedValues }: { entry: NEntry; resolvedValues: ReadonlyMap<string, string> }) {
  const [includeLocalInterface, setIncludeLocalInterface] = useState(false);
  const [includeDecryptedValues, setIncludeDecryptedValues] = useState(false);
  const bodyFormats = ["Raw", "Formatted"] as const;
  const [bodyFormat, setBodyFormat] = useState<(typeof bodyFormats)[number]>("Raw");
  const hasLocalAddress = Boolean(entry.e._recorder?.network?.localAddress);
  const canFormatBody = entry.e._recorder?.requestBodyEncoding !== "base64"
    && entry.e.request?.postData?.text != null
    && supportsReplayBodyFormatting(entry.e.request.postData.mimeType);
  const requestTokens = useMemo(() => protectedOccurrences(entry.e).filter((item) => item.request && item.mode === "encrypt"), [entry.e]);
  const requestDecrypted = useMemo(() => new Map(
    [...resolvedValues].filter(([token]) => requestTokens.some((item) => item.token === token)),
  ), [resolvedValues, requestTokens]);
  useEffect(() => {
    setIncludeDecryptedValues(false);
    setBodyFormat("Raw");
  }, [entry.e]);
  const replay = useMemo(
    () => curlReplay(entry.e, {
      includeLocalInterface,
      decryptedValues: includeDecryptedValues && requestDecrypted.size > 0 ? requestDecrypted : undefined,
      bodyFormat: bodyFormat === "Formatted" ? "formatted" : "raw",
    }),
    [bodyFormat, entry.e, includeLocalInterface, includeDecryptedValues, requestDecrypted],
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
              <small>{hasLocalAddress ? entry.e._recorder?.network?.localAddress : "No local address was captured"}</small>
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
          <div
            className={`replay-option-row ${canFormatBody ? "" : "disabled"}`}
            data-tooltip={canFormatBody
              ? "Choose whether cURL receives the recorded body or a formatted representation"
              : "Only embedded JSON and XML request bodies can be formatted"}
          >
            <span className="replay-option-icon"><Braces size={16} /></span>
            <span className="replay-option-copy">
              <strong>Request body representation</strong>
              <small>{canFormatBody ? "Raw preserves the recorded body exactly" : "No formattable request body was recorded"}</small>
            </span>
            {canFormatBody ? (
              <SegmentedControl
                label="Replay request body representation"
                value={bodyFormat}
                options={bodyFormats}
                onChange={setBodyFormat}
              />
            ) : <span className="badge mono">raw</span>}
          </div>
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
  resolvedValues,
  onResolved,
  clearEpoch,
  keyInputs,
  onKeyInput,
  onProtectionKeyActivated,
}: {
  entry: NEntry;
  entries: NEntry[];
  resolvedValues: ReadonlyMap<string, string>;
  onResolved: (values: ReadonlyMap<string, string>) => void;
  clearEpoch: number;
  keyInputs: ReadonlyMap<string, string>;
  onKeyInput: (keyId: string, value: string) => void;
  onProtectionKeyActivated: (group: string, value: string) => void;
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
          resolved plaintext stays in memory only until another HAR is loaded. Verified candidates are shown across
          detail tabs. A successfully used encryption key automatically resolves later entries in the same live session,
          while Replay remains encrypted-value-only and a separate opt-in.
        </p>
        <div className="protection-list">
          {groups.map(([groupKey, group]) => (
            <ProtectionKeyGroup
              key={groupKey}
              groupKey={groupKey}
              occurrences={group}
              selectedEntryId={entry.id}
              resolvedValues={resolvedValues}
              keyInput={keyInputs.get(groupKey) ?? ""}
              onKeyInput={(value) => onKeyInput(groupKey, value)}
              onResolved={onResolved}
              onKeyActivated={(value) => onProtectionKeyActivated(groupKey, value)}
              clearEpoch={clearEpoch}
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
  resolvedValues,
  keyInput,
  onKeyInput,
  onResolved,
  onKeyActivated,
  clearEpoch,
}: {
  groupKey: string;
  occurrences: Array<ProtectedOccurrence & { entryId: number }>;
  selectedEntryId: number;
  resolvedValues: ReadonlyMap<string, string>;
  keyInput: string;
  onKeyInput: (value: string) => void;
  onResolved: (values: ReadonlyMap<string, string>) => void;
  onKeyActivated: (value: string) => void;
  clearEpoch: number;
}) {
  const mode = occurrences[0].mode;
  const keyId = occurrences[0].keyId;
  const selected = useMemo(() => occurrences.filter((item) => item.entryId === selectedEntryId), [occurrences, selectedEntryId]);
  const uniqueAll = useMemo(() => [...new Map(occurrences.map((item) => [item.token, item])).values()], [occurrences]);
  const uniqueSelected = useMemo(() => [...new Map(selected.map((item) => [item.token, item])).values()], [selected]);
  const resolvedCount = uniqueAll.filter((item) => resolvedValues.has(item.token)).length;
  const verifiedCandidates = useMemo(() => {
    const grouped = new Map<string, { tokens: number; occurrences: number }>();
    for (const item of uniqueAll) {
      const value = resolvedValues.get(item.token);
      if (value == null || item.mode !== "tokenize") continue;
      const current = grouped.get(value) ?? { tokens: 0, occurrences: 0 };
      current.tokens += 1;
      current.occurrences += occurrences.filter((occurrence) => occurrence.token === item.token).length;
      grouped.set(value, current);
    }
    return [...grouped.entries()];
  }, [occurrences, resolvedValues, uniqueAll]);
  const [candidate, setCandidate] = useState("");
  const [status, setStatus] = useState("");
  const [busy, setBusy] = useState(false);
  const clearEpochRef = useRef(clearEpoch);

  useEffect(() => {
    clearEpochRef.current = clearEpoch;
    setCandidate("");
    setStatus("");
    setBusy(false);
  }, [clearEpoch]);

  const decrypt = async (scope: "exchange" | "har") => {
    const operationEpoch = clearEpoch;
    const targets = scope === "exchange" ? uniqueSelected : uniqueAll;
    setStatus("");
    setBusy(true);
    try {
      const result = await decryptProtectedTokens(targets, keyInput, (progress) => {
        if (clearEpochRef.current !== operationEpoch) return;
        setStatus(`decrypting ${progress.completed.toLocaleString()} / ${progress.total.toLocaleString()} · ${progress.failures.toLocaleString()} failed`);
      });
      if (clearEpochRef.current !== operationEpoch) return;
      onResolved(result.values);
      onKeyActivated(keyInput);
      setStatus(`${result.values.size.toLocaleString()} decrypted · ${result.failures.toLocaleString()} failed · future live values resolve automatically`);
    } catch (error) {
      if (clearEpochRef.current === operationEpoch) {
        setStatus(error instanceof Error ? error.message : "Protection operation failed.");
      }
    } finally {
      if (clearEpochRef.current === operationEpoch) setBusy(false);
    }
  };

  const verify = async (scope: "exchange" | "har") => {
    const operationEpoch = clearEpoch;
    const targets = scope === "exchange" ? uniqueSelected : uniqueAll;
    setStatus("");
    setBusy(true);
    try {
      const matches = await verifyProtectedTokens(targets, candidate, keyInput, (progress) => {
        if (clearEpochRef.current !== operationEpoch) return;
        setStatus(`verifying ${progress.completed.toLocaleString()} / ${progress.total.toLocaleString()} · ${progress.matches.toLocaleString()} matched`);
      });
      if (clearEpochRef.current !== operationEpoch) return;
      if (matches.size > 0) onResolved(matches);
      setStatus(`${matches.size.toLocaleString()} of ${targets.length.toLocaleString()} tokenized values match and are resolved in memory`);
    } catch (error) {
      if (clearEpochRef.current === operationEpoch) {
        setStatus(error instanceof Error ? error.message : "Protection operation failed.");
      }
    } finally {
      if (clearEpochRef.current === operationEpoch) setBusy(false);
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
        <span>
          <strong>{resolvedCount.toLocaleString()}</strong> {mode === "encrypt" ? "decrypted" : "verified"}
        </span>
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
      {mode === "tokenize" && verifiedCandidates.length > 0 && (
        <div className="verified-candidates">
          <div className="verified-candidates-head">
            <strong>Verified candidates in memory</strong>
            <span className="badge">{verifiedCandidates.length.toLocaleString()}</span>
          </div>
          {verifiedCandidates.map(([value, counts]) => (
            <div className="verified-candidate" key={value}>
              <div className="verified-candidate-meta">
                <span>{counts.tokens.toLocaleString()} unique token{counts.tokens === 1 ? "" : "s"}</span>
                <span>{counts.occurrences.toLocaleString()} occurrence{counts.occurrences === 1 ? "" : "s"}</span>
                <CopyButton text={value} label="copy candidate" />
              </div>
              <pre className="mono">{value}</pre>
            </div>
          ))}
        </div>
      )}
      {selected.length > 0 && (
        <details className="protection-values">
          <summary>values in this exchange ({selected.length.toLocaleString()})</summary>
          <div className="protection-value-list">
            {visibleSelected.map((occurrence, index) => {
              const plain = resolvedValues.get(occurrence.token);
              const resolvedLabel = occurrence.mode === "tokenize" ? "verified" : "decrypted";
              return (
                <div className="protection-value-row" key={`${occurrence.path}-${occurrence.token}-${index}`}>
                  <span className="mono wrap">{occurrence.path}</span>
                  <span className={plain == null ? "muted" : "bool-yes"}>{plain == null ? "protected" : resolvedLabel}</span>
                  {plain != null && <CopyButton text={plain} label={occurrence.mode === "tokenize" ? "copy candidate" : "copy plaintext"} />}
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
  const responseBody = e._recorder?.responseBody;
  const protocol = e.response?.httpVersion || e.request?.httpVersion || "unknown";
  const attention = [
    e._recorder?.error ? `${e._recorder.error.phase} transport failure` : null,
    responseBody?.truncated ? "response body truncated" : null,
    responseBody?.closedEarly ? "response body closed before EOF" : null,
    responseBody?.readError ? "response body read error" : null,
  ].filter((value): value is string => Boolean(value));

  return (
    <div className="summary-page">
      <div className="summary-hero">
        <div className="summary-title">
          <span className="summary-eyebrow">Exchange outcome</span>
          <div>
            <StatusBadge status={entry.status} />
            <StateBadge state={entry.state} />
          </div>
        </div>
        <SummaryMetric icon={<Clock3 size={15} />} label="Total duration" value={formatDuration(entry.timeMs)} />
        <SummaryMetric icon={<Network size={15} />} label="Protocol" value={protocol} />
        <SummaryMetric
          icon={<Database size={15} />}
          label="Response captured"
          value={responseBody ? formatBytes(responseBody.capturedBytes) : formatBytes(e.response?.content?.size)}
        />
      </div>
      {attention.length > 0 ? (
        <div className="summary-attention" role="status">
          <AlertTriangle size={16} />
          <div><strong>Review required</strong><span>{attention.join(" · ")}</span></div>
        </div>
      ) : null}
      <Section title="Exchange details">
        <KV
          rows={[
            ["started", e.startedDateTime],
            ["total time", formatDuration(entry.timeMs)],
            ["method", entry.method],
            ["url", <span className="mono wrap">{entry.url}</span>],
            ["status", e.response ? `${e.response.status} ${e.response.statusText}`.trim() : "—"],
            ["state", <StateBadge state={entry.state} />],
            ["error", e._recorder?.error ? `${e._recorder?.error.phase}: ${e._recorder?.error.message}` : ""],
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
            ["exchange id", e._recorder?.exchangeId ? <span className="mono">{e._recorder?.exchangeId}</span> : ""],
            ["server ip", e.serverIPAddress],
            ["connection", e.connection],
            ["page reference", e.pageref],
          ]}
        />
      </Section>
      {e.comment ? (
        <Section title="Comment" actions={<CopyButton text={e.comment} label="copy comment" />}>
          <div className="entry-comment">{e.comment}</div>
        </Section>
      ) : null}
      <Section title="Timing waterfall">
        <Waterfall timings={e.timings} totalMs={entry.timeMs} startMs={entry.startMs} compact />
      </Section>
      <Section title="Bodies">
        <KV
          rows={[
            ["request", bodySummary(e._recorder?.requestBody) ?? (e.request?.postData ? "present" : "none")],
            ["response", bodySummary(e._recorder?.responseBody) ?? "none"],
          ]}
        />
      </Section>
      {Object.keys(e.cache ?? {}).length > 0 ? (
        <Section title="HAR cache state">
          <JsonTree value={e.cache} />
        </Section>
      ) : null}
    </div>
  );
}

function SummaryMetric({ icon, label, value }: { icon: ReactNode; label: string; value: string }) {
  return (
    <div className="summary-metric">
      <span className="summary-metric-icon">{icon}</span>
      <span><small>{label}</small><strong>{value}</strong></span>
    </div>
  );
}

function RedactionAuditTab({ entry }: { entry: NEntry }) {
  const audit = entry.e._recorder?.redaction;
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
      <Waterfall timings={entry.e.timings} totalMs={entry.timeMs} startMs={entry.startMs} />
      <p className="muted note">
        Values of -1 in the HAR mean the phase was not observed or does not apply (reused connections report -1 for
        dns/connect/ssl by design).
      </p>
    </Section>
  );
}

function RequestTab({ entry }: { entry: NEntry }) {
  const req = entry.e.request;
  const options = ["Overview", "Headers", "Parameters", "Body"] as const;
  const [view, setView] = useState<(typeof options)[number]>("Overview");
  const bodyModes = ["Raw", "Formatted"] as const;
  const [bodyMode, setBodyMode] = useState<(typeof bodyModes)[number]>("Raw");
  const rawBody = prettyPostData(req?.postData, entry.e._recorder?.requestBodyEncoding, false);
  const body = bodyMode === "Formatted"
    ? prettyPostData(req?.postData, entry.e._recorder?.requestBodyEncoding, true)
    : rawBody;

  return (
    <div className="workspace-page">
      <WorkspaceHeader title="Request" description="Inspect the outbound request exactly as it was recorded." />
      <SegmentedControl label="Request detail" value={view} options={options} onChange={setView} />
      <div className="workspace-content">
        {view === "Overview" && (
          <Section title="Request line">
            <KV rows={[
              ["method", req?.method],
              ["url", <span className="mono wrap">{req?.url}</span>],
              ["http version", req?.httpVersion || "unknown"],
              ["body size", formatBytes(req?.bodySize)],
              ["headers size", formatBytes(req?.headersSize)],
              ["transfer encoding", entry.e._recorder?.requestTransferEncoding?.join(", ") ?? ""],
              ["comment", req?.comment],
            ]} />
          </Section>
        )}
        {view === "Headers" && (
          <>
            <Section title="Headers"><PairsTable pairs={req?.headers} /></Section>
            <Section title="Trailers"><PairsTable pairs={entry.e._recorder?.requestTrailers} /></Section>
          </>
        )}
        {view === "Parameters" && (
          <>
            <Section title="Query string"><PairsTable pairs={req?.queryString} /></Section>
            <Section title="Cookies"><CookiesTable cookies={req?.cookies} /></Section>
          </>
        )}
        {view === "Body" && (
          <>
            <Section
              title="Body"
              actions={rawBody.kind === "json" || rawBody.kind === "xml" ? (
                <SegmentedControl label="Request body display" value={bodyMode} options={bodyModes} onChange={setBodyMode} />
              ) : undefined}
            >
              {body.kind === "empty" ? (
                <EmptyState text={missingEmbeddedBodyText("request", entry.e._recorder?.requestBody)} />
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
            {req?.postData?.params?.length ? (
              <Section title={`Form parameters (${req.postData.params.length})`}>
                <PostParamsTable params={req.postData.params} />
              </Section>
            ) : null}
            {req?.postData?.comment ? (
              <Section title="Body comment"><div className="entry-comment">{req.postData.comment}</div></Section>
            ) : null}
            <BodyInfoSection title="Request body metadata" info={entry.e._recorder?.requestBody} />
          </>
        )}
      </div>
    </div>
  );
}

function ResponseTab({ entry }: { entry: NEntry }) {
  const resp = entry.e.response;
  const options = ["Overview", "Headers", "Body"] as const;
  const [view, setView] = useState<(typeof options)[number]>("Overview");
  const bodyModes = ["Raw", "Formatted"] as const;
  const [bodyMode, setBodyMode] = useState<(typeof bodyModes)[number]>("Raw");
  const rawBody = prettyContent(resp?.content, false);
  const body = bodyMode === "Formatted" ? prettyContent(resp?.content, true) : rawBody;

  return (
    <div className="workspace-page">
      <WorkspaceHeader title="Response" description="Review the response outcome, metadata, and captured representation." />
      <SegmentedControl label="Response detail" value={view} options={options} onChange={setView} />
      <div className="workspace-content">
        {view === "Overview" && (
          <Section title="Status">
            <KV rows={[
              ["status", resp ? `${resp.status} ${resp.statusText}`.trim() : "—"],
              ["http version", resp?.httpVersion || "unknown"],
              ["mime type", resp?.content?.mimeType],
              ["content size", formatBytes(resp?.content?.size)],
              ["body size (wire)", formatBytes(resp?.bodySize)],
              ["headers size", formatBytes(resp?.headersSize)],
              ["content compression", resp?.content?.compression != null ? formatBytes(resp.content.compression) : ""],
              ["decoded by recorder", entry.e._recorder?.responseBodyDecoded ? <BoolMark v /> : ""],
              ["redirect url", resp?.redirectURL],
              ["transfer encoding", entry.e._recorder?.responseTransferEncoding?.join(", ") ?? ""],
              ["comment", resp?.comment],
            ]} />
          </Section>
        )}
        {view === "Headers" && (
          <>
            <Section title="Headers"><PairsTable pairs={resp?.headers} /></Section>
            <Section title="Cookies"><CookiesTable cookies={resp?.cookies} /></Section>
            <Section title="Trailers"><PairsTable pairs={entry.e._recorder?.responseTrailers} /></Section>
          </>
        )}
        {view === "Body" && (
          <>
            <Section
              title="Body"
              actions={rawBody.kind === "json" || rawBody.kind === "xml" ? (
                <SegmentedControl label="Response body display" value={bodyMode} options={bodyModes} onChange={setBodyMode} />
              ) : undefined}
            >
              {body.kind === "empty" ? (
                <EmptyState text={missingEmbeddedBodyText("response", entry.e._recorder?.responseBody)} />
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
            <BodyInfoSection title="Response body metadata" info={entry.e._recorder?.responseBody} />
            {resp?.content?.comment ? (
              <Section title="Content comment"><div className="entry-comment">{resp.content.comment}</div></Section>
            ) : null}
          </>
        )}
      </div>
    </div>
  );
}

function PostParamsTable({ params }: { params: PostParam[] }) {
  return (
    <table className="pairs-table">
      <thead><tr><th>name</th><th>value</th><th>file</th><th>content type</th><th>comment</th></tr></thead>
      <tbody>
        {params.map((param, index) => (
          <tr key={`${param.name}-${index}`}>
            <td className="pair-name">{param.name}</td>
            <td className="pair-value mono">{param.value ?? ""}</td>
            <td className="mono">{param.fileName ?? ""}</td>
            <td className="mono">{param.contentType ?? ""}</td>
            <td className="pair-comment">{param.comment ?? ""}</td>
          </tr>
        ))}
      </tbody>
    </table>
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
  const err = entry.e._recorder?.error;
  if (!err) return <EmptyState text="this exchange completed without a transport error" />;
  return (
    <>
      <Section title="error">
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
  const n = entry.e._recorder?.network;
  if (!n) return <EmptyState text="no _recorder.network data on this entry" />;
  return (
    <>
      <Section title="network">
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
      {entry.e._recorder?.expect100 ? (
        <Section title="expect 100-continue">
          <KV
            rows={[
              ["waited", <BoolMark v={entry.e._recorder?.expect100.waited} />],
              ["100 received", <BoolMark v={entry.e._recorder?.expect100.continueReceived} />],
              ["wait", entry.e._recorder?.expect100.waitMs != null ? formatDuration(entry.e._recorder?.expect100.waitMs) : ""],
            ]}
          />
        </Section>
      ) : null}
      {entry.e._recorder?.informational?.length ? (
        <Section title="informational responses (1xx)">
          {entry.e._recorder?.informational.map((ir, i) => (
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
  const t = entry.e._recorder?.tls;
  if (!t) return <EmptyState text="no _recorder.tls data (plain HTTP, or TLS not captured)" />;
  return (
    <>
      <Section title="tls">
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
        {entry.e._recorder?.network?.connectionReused ? (
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
  const events = useMemo(() => entry.e._recorder?.trace ?? [], [entry.e._recorder?.trace]);
  const baseMs = events.length ? parseIsoMs(events[0].time) : null;
  const shown = useMemo(() => {
    const q = filter.trim().toLowerCase();
    if (!q) return events;
    return events.filter(
      (ev) => ev.name.toLowerCase().includes(q) || (ev.detail ?? "").toLowerCase().includes(q),
    );
  }, [events, filter]);

  if (events.length === 0) {
    return <EmptyState text="no trace events (enable Config.CaptureRawTrace to record raw httptrace events)" />;
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

function RawTab({ entry, resolved = false }: { entry: NEntry; resolved?: boolean }) {
  const ext = extensionFields(entry.e);
  const json = JSON.stringify(entry.e, null, 2);
  const truncated = json.length > 400_000;
  const visibleJson = truncated ? `${json.slice(0, 400_000)}\n… (truncated view)` : json;
  const options = ["Entry JSON", "Extensions"] as const;
  const [view, setView] = useState<(typeof options)[number]>("Entry JSON");

  return (
    <div className="workspace-page">
      <WorkspaceHeader
        title="Raw evidence"
        description="Inspect the complete entry representation and every recorder or future extension field."
      />
      <SegmentedControl label="Raw evidence detail" value={view} options={options} onChange={setView} />
      <div className="workspace-content">
        {view === "Entry JSON" && (
          <Section title={resolved ? "Entry JSON (resolved in-memory view)" : "Entry JSON"}>
            <CodeBlock text={visibleJson} copyText={json} note={truncated ? "view limited to 400,000 characters" : "complete entry"} language="json" />
          </Section>
        )}
        {view === "Extensions" && (
          <Section title={`Extension fields (${Object.keys(ext).length})`}>
            {Object.keys(ext).length ? <JsonTree value={ext} /> : <EmptyState text="no extension fields" />}
          </Section>
        )}
      </div>
    </div>
  );
}
