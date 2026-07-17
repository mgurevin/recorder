import type { Timings } from "../types/har";
import { formatDuration } from "../lib/format";

const PHASES: Array<{ key: keyof Timings; label: string; cls: string }> = [
  { key: "blocked", label: "blocked", cls: "wf-blocked" },
  { key: "dns", label: "dns", cls: "wf-dns" },
  { key: "connect", label: "connect", cls: "wf-connect" },
  { key: "ssl", label: "ssl", cls: "wf-ssl" },
  { key: "send", label: "send", cls: "wf-send" },
  { key: "wait", label: "wait", cls: "wf-wait" },
  { key: "receive", label: "receive", cls: "wf-receive" },
];

/** Waterfall draws per-phase bars against the total duration. -1 phases are
 * listed as "not observed / not applicable" instead of zero-width lies. */
export function Waterfall({ timings, totalMs, compact }: { timings: Timings | undefined; totalMs: number; compact?: boolean }) {
  if (!timings) return <div className="empty-state">no timings recorded</div>;
  const measured = PHASES.map(({ key }) => {
    const v = timings[key];
    return typeof v === "number" && Number.isFinite(v) && v >= 0 ? v : -1;
  });
  const sum = measured.reduce((acc, v) => acc + (v > 0 ? v : 0), 0);
  const scale = Math.max(totalMs, sum, 0.001);

  let offset = 0;
  return (
    <div className={`waterfall ${compact ? "compact" : ""}`}>
      {PHASES.map(({ key, label, cls }, i) => {
        const v = measured[i];
        const left = (offset / scale) * 100;
        const width = v > 0 ? Math.max((v / scale) * 100, 0.5) : 0;
        if (v > 0) offset += v;
        return (
          <div className="wf-row" key={key}>
            <span className="wf-label">{label}</span>
            <div className="wf-track">
              {v >= 0 ? (
                <div className={`wf-bar ${cls}`} style={{ left: `${left}%`, width: `${width}%` }} />
              ) : null}
            </div>
            <span className={`wf-value ${v < 0 ? "muted" : ""}`}>
              {v >= 0 ? formatDuration(v) : "not observed"}
            </span>
          </div>
        );
      })}
      {!compact && (
        <div className="wf-row wf-total">
          <span className="wf-label">total</span>
          <div className="wf-track" />
          <span className="wf-value">{formatDuration(totalMs)}</span>
        </div>
      )}
    </div>
  );
}
