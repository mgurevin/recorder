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
  onSelect,
}: {
  entries: NEntry[];
  groups: TraceGroup[] | null;
  selectedId: number | null;
  onSelect: (id: number) => void;
}) {
  const rows: Row[] = [];
  if (groups) {
    for (const g of groups) {
      if (g.entries.length > 1) {
        rows.push({ kind: "group", group: g });
        for (const en of g.entries) rows.push({ kind: "entry", entry: en, inGroup: true });
      } else if (g.entries.length === 1) {
        rows.push({ kind: "entry", entry: g.entries[0], inGroup: false });
      }
    }
  } else {
    for (const en of entries) rows.push({ kind: "entry", entry: en, inGroup: false });
  }

  if (rows.length === 0) {
    return <div className="empty-state">no entries match the current filters</div>;
  }

  return (
    <VirtualList
      rows={rows}
      rowHeight={ROW_HEIGHT}
      render={(row) =>
        row.kind === "group" ? (
          <GroupRow group={row.group} />
        ) : (
          <EntryRow entry={row.entry} inGroup={row.inGroup} selected={row.entry.id === selectedId} onSelect={onSelect} />
        )
      }
    />
  );
}

function GroupRow({ group }: { group: TraceGroup }) {
  return (
    <div className="row group-row" title={group.traceId ?? ""}>
      <GitBranch size={13} />
      <span className="mono trace-chip">{shortId(group.traceId, 12)}</span>
      <span className="muted">
        {group.hops} hops · {formatDuration(group.totalMs)}
      </span>
      <span className={`badge status-${statusTone(group.finalStatus)}`}>
        {group.finalStatus > 0 ? group.finalStatus : "ERR"}
      </span>
      {group.hasFailed ? <AlertTriangle size={13} className="warn" /> : null}
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
      {entry.errorPhase ? <span className="badge phase">{entry.errorPhase}</span> : null}
      {entry.truncated ? <Scissors size={12} className="warn" aria-label="truncated body" /> : null}
      {entry.closedEarly ? <XCircle size={12} className="warn" aria-label="closed early" /> : null}
      {entry.redirectIndex != null ? <span className="badge hop">#{entry.redirectIndex}</span> : null}
      <span className="row-size muted">{formatBytes(entry.respSize)}</span>
      <span className="row-time muted">{formatDuration(entry.timeMs)}</span>
      <StatusBadge status={entry.status} />
    </div>
  );
}
