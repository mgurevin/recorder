import { ArrowLeft, GitBranch } from "lucide-react";
import type { NEntry, Timings, TraceGroup } from "../types/har";
import { formatBytes, formatDuration } from "../lib/format";
import { KV, MethodBadge, Section, StatusBadge } from "./Shared";

const PHASES: Array<{ key: keyof Timings; label: string; cls: string }> = [
  { key: "blocked", label: "blocked", cls: "wf-blocked" },
  { key: "dns", label: "dns", cls: "wf-dns" },
  { key: "connect", label: "connect", cls: "wf-connect" },
  { key: "ssl", label: "ssl", cls: "wf-ssl" },
  { key: "send", label: "send", cls: "wf-send" },
  { key: "wait", label: "wait", cls: "wf-wait" },
  { key: "receive", label: "receive", cls: "wf-receive" },
];

export function TraceGroupPanel({ group, onBack, onSelectEntry }: {
  group: TraceGroup;
  onBack: () => void;
  onSelectEntry: (id: number) => void;
}) {
  const starts = group.entries.map((entry) => entry.startMs).filter((value): value is number => value != null);
  const start = starts.length ? Math.min(...starts) : null;
  const end = start == null
    ? null
    : Math.max(...group.entries.map((entry) => (entry.startMs ?? start) + entry.timeMs));
  const elapsed = start != null && end != null ? end - start : group.totalMs;
  const capturedBytes = group.entries.reduce((sum, entry) => sum + entry.respSize, 0);

  return (
    <div className="detail">
      <div className="detail-head chain-detail-head">
        <button type="button" className="icon-btn back-btn" onClick={onBack} data-tooltip="Back to request list" aria-label="Back to request list">
          <ArrowLeft size={15} />
        </button>
        <GitBranch size={15} />
        <strong>Redirect chain</strong>
        <span className="detail-url mono" data-tooltip={`Trace ID: ${group.traceId ?? "not recorded"}`}>
          {group.traceId ?? "trace ID not recorded"}
        </span>
        <StatusBadge status={group.finalStatus} />
      </div>
      <div className="detail-body">
        <Section title="Chain overview">
          <KV rows={[
            ["trace ID", <span className="mono wrap">{group.traceId}</span>],
            ["redirect hops", String(group.hops)],
            ["started", start == null ? "—" : new Date(start).toISOString()],
            ["elapsed time", formatDuration(elapsed)],
            ["sum of exchange durations", formatDuration(group.totalMs)],
            ["final status", String(group.finalStatus || "ERR")],
            ["failed exchanges", String(group.entries.filter((entry) => entry.failed).length)],
            ["captured response bytes", formatBytes(capturedBytes)],
          ]} />
        </Section>
        <Section title="Chain timing waterfall">
          <ChainWaterfall entries={group.entries} start={start} elapsed={elapsed} onSelectEntry={onSelectEntry} />
        </Section>
        <Section title="Exchanges">
          <div className="chain-exchanges">
            {group.entries.map((entry) => (
              <button type="button" className="chain-exchange" key={entry.id} onClick={() => onSelectEntry(entry.id)}>
                <span className="badge hop" data-tooltip={`Redirect hop index ${entry.redirectIndex ?? 0} (zero-based)`}>
                  #{entry.redirectIndex ?? 0}
                </span>
                <MethodBadge method={entry.method} />
                <span className="mono chain-url">{entry.url}</span>
                <span className="muted">{formatDuration(entry.timeMs)}</span>
                <StatusBadge status={entry.status} />
              </button>
            ))}
          </div>
        </Section>
      </div>
    </div>
  );
}

function ChainWaterfall({ entries, start, elapsed, onSelectEntry }: {
  entries: NEntry[];
  start: number | null;
  elapsed: number;
  onSelectEntry: (id: number) => void;
}) {
  const scale = Math.max(elapsed, 0.001);
  return (
    <div className="chain-waterfall">
      <div className="chain-wf-legend">
        {PHASES.map((phase) => <span key={phase.key}><i className={phase.cls} />{phase.label}</span>)}
      </div>
      {entries.map((entry) => {
        const offset = start != null && entry.startMs != null ? Math.max(0, entry.startMs - start) : 0;
        const timings = entry.e.timings;
        const measured = PHASES.map((phase) => {
          const value = timings?.[phase.key];
          return typeof value === "number" && Number.isFinite(value) && value >= 0 ? value : 0;
        });
        const measuredTotal = measured.reduce((sum, value) => sum + value, 0);
        const duration = Math.max(entry.timeMs, measuredTotal);
        let phaseOffset = offset;
        return (
          <button type="button" className="chain-wf-row" key={entry.id} onClick={() => onSelectEntry(entry.id)}>
            <span className="chain-wf-label">#{entry.redirectIndex ?? 0}</span>
            <span className="chain-wf-url" title={entry.url}>{entry.host}{entry.path}</span>
            <span className="chain-wf-track">
              {PHASES.map((phase, index) => {
                const value = measured[index];
                const left = (phaseOffset / scale) * 100;
                const width = value > 0 ? Math.max((value / scale) * 100, 0.4) : 0;
                phaseOffset += value;
                return value > 0 ? (
                  <i
                    key={phase.key}
                    className={`chain-wf-bar ${phase.cls}`}
                    style={{ left: `${left}%`, width: `${width}%` }}
                    data-tooltip={`${phase.label}: ${formatDuration(value)}`}
                  />
                ) : null;
              })}
              {measuredTotal === 0 ? (
                <i
                  className="chain-wf-bar wf-unmeasured"
                  style={{ left: `${(offset / scale) * 100}%`, width: `${Math.max((duration / scale) * 100, 0.4)}%` }}
                  data-tooltip="No phase timings recorded"
                />
              ) : null}
            </span>
            <span className="chain-wf-time">{formatDuration(entry.timeMs)}</span>
            <StatusBadge status={entry.status} />
          </button>
        );
      })}
      <div className="chain-wf-axis"><span>0</span><span>{formatDuration(elapsed)}</span></div>
    </div>
  );
}
