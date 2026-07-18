import { useEffect, useMemo } from "react";
import { AlertTriangle, GitBranch, Scissors, XCircle } from "lucide-react";
import type { NEntry, TraceGroup } from "../types/har";
import { formatBytes, formatDuration, shortId, statusTone } from "../lib/format";
import { VirtualList } from "./VirtualList";
import { MethodBadge, StatusBadge } from "./Shared";

const ROW_HEIGHT = 46;

type Row =
  | { kind: "entry"; entry: NEntry; inGroup: boolean }
  | { kind: "group"; group: TraceGroup };

export function EntryList({
  entries,
  groups,
  selectedId,
  selectedTraceId,
  onSelect,
  onSelectGroup,
}: {
  entries: NEntry[];
  groups: TraceGroup[] | null;
  selectedId: number | null;
  selectedTraceId: string | null;
  onSelect: (id: number) => void;
  onSelectGroup: (traceId: string) => void;
}) {
  const rows = useMemo(() => {
    const result: Row[] = [];
    if (groups) {
      for (const g of groups) {
        if (g.entries.length > 1) {
          result.push({ kind: "group", group: g });
          for (const en of g.entries) result.push({ kind: "entry", entry: en, inGroup: true });
        } else if (g.entries.length === 1) {
          result.push({ kind: "entry", entry: g.entries[0], inGroup: false });
        }
      }
    } else {
      for (const en of entries) result.push({ kind: "entry", entry: en, inGroup: false });
    }
    return result;
  }, [entries, groups]);

  const selectedIndex = rows.findIndex((row) =>
    row.kind === "group" ? row.group.traceId === selectedTraceId : row.entry.id === selectedId,
  );

  useEffect(() => {
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "ArrowUp" && event.key !== "ArrowDown") return;
      const target = event.target;
      if (target instanceof Element && target.closest("input, select, textarea, button, a, [contenteditable='true']")) return;
      if (rows.length === 0) return;

      event.preventDefault();
      const direction = event.key === "ArrowDown" ? 1 : -1;
      const nextIndex = selectedIndex < 0
        ? (direction > 0 ? 0 : rows.length - 1)
        : Math.max(0, Math.min(rows.length - 1, selectedIndex + direction));
      const row = rows[nextIndex];
      if (row.kind === "group") {
        if (row.group.traceId) onSelectGroup(row.group.traceId);
        else if (row.group.entries[0]) onSelect(row.group.entries[0].id);
      } else {
        onSelect(row.entry.id);
      }
    };
    window.addEventListener("keydown", handleKeyDown);
    return () => window.removeEventListener("keydown", handleKeyDown);
  }, [onSelect, onSelectGroup, rows, selectedIndex]);

  if (rows.length === 0) {
    return <div className="empty-state">no entries match the current filters</div>;
  }

  return (
    <VirtualList
      rows={rows}
      rowHeight={ROW_HEIGHT}
      activeIndex={selectedIndex}
      render={(row) =>
        row.kind === "group" ? (
          <GroupRow
            group={row.group}
            selected={row.group.traceId === selectedTraceId}
            onSelect={onSelectGroup}
          />
        ) : (
          <EntryRow entry={row.entry} inGroup={row.inGroup} selected={row.entry.id === selectedId} onSelect={onSelect} />
        )
      }
    />
  );
}

function GroupRow({ group, selected, onSelect }: { group: TraceGroup; selected: boolean; onSelect: (traceId: string) => void }) {
  return (
    <div
      className={`row group-row ${selected ? "selected" : ""}`}
      data-tooltip="Open trace chain overview"
      onClick={() => group.traceId && onSelect(group.traceId)}
    >
      <GitBranch size={13} aria-label="Trace chain" />
      <span className="mono trace-chip" data-tooltip={`Trace ID: ${group.traceId ?? "not recorded"}`}>
        {shortId(group.traceId, 12)}
      </span>
      <span className="muted">
        {group.hops} hops · {formatDuration(group.totalMs)}
      </span>
      <span className={`badge status-${statusTone(group.finalStatus)}`} data-tooltip="Final response status">
        {group.finalStatus > 0 ? group.finalStatus : "ERR"}
      </span>
      {group.hasFailed ? (
        <span data-tooltip="Trace chain contains a failed request" aria-label="Trace chain contains a failed request">
          <AlertTriangle size={13} className="warn" />
        </span>
      ) : null}
    </div>
  );
}

function EntryRow({
  entry,
  inGroup,
  selected,
  onSelect,
}: {
  entry: NEntry;
  inGroup: boolean;
  selected: boolean;
  onSelect: (id: number) => void;
}) {
  return (
    <div
      className={`row entry-row ${selected ? "selected" : ""} ${inGroup ? "in-group" : ""}`}
      onClick={() => onSelect(entry.id)}
    >
      <MethodBadge method={entry.method} />
      <div className="row-url" title={entry.url}>
        <span className="row-host">{entry.host}</span>
        <span className="row-path muted">{entry.path}</span>
      </div>
      {entry.errorPhase ? <span className="badge phase" data-tooltip="Phase in which the request failed">{entry.errorPhase}</span> : null}
      {entry.truncated ? (
        <span data-tooltip="Captured body was truncated" aria-label="Captured body was truncated">
          <Scissors size={12} className="warn" />
        </span>
      ) : null}
      {entry.closedEarly ? (
        <span data-tooltip="Response body was closed before EOF" aria-label="Response body was closed before EOF">
          <XCircle size={12} className="warn" />
        </span>
      ) : null}
      {entry.redirectIndex != null ? (
        <span className="badge hop" data-tooltip={`Redirect hop index ${entry.redirectIndex} (zero-based)`}>
          #{entry.redirectIndex}
        </span>
      ) : null}
      <span className="row-size muted" data-tooltip="Captured response body size">{formatBytes(entry.respSize)}</span>
      <span className="row-time muted" data-tooltip="Total request duration">{formatDuration(entry.timeMs)}</span>
      <StatusBadge status={entry.status} />
    </div>
  );
}
