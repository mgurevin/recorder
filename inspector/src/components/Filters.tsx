import { ArrowDownUp, Layers } from "lucide-react";
import type { NEntry } from "../types/har";

export interface FilterState {
  search: string;
  method: string;
  statusClass: string;
  state: string;
  errorPhase: string;
  traceId: string;
  onlyFailed: boolean;
  onlyTruncated: boolean;
  onlyClosedEarly: boolean;
}

export const emptyFilters: FilterState = {
  search: "",
  method: "",
  statusClass: "",
  state: "",
  errorPhase: "",
  traceId: "",
  onlyFailed: false,
  onlyTruncated: false,
  onlyClosedEarly: false,
};

export type SortKey = "start" | "duration" | "status" | "size";

export function applyFilters(entries: NEntry[], f: FilterState): NEntry[] {
  const search = f.search.trim().toLowerCase();
  const traceId = f.traceId.trim().toLowerCase();
  return entries.filter((en) => {
    if (f.method && en.method !== f.method) return false;
    if (f.statusClass) {
      const cls = en.status <= 0 ? "0" : `${Math.floor(en.status / 100)}xx`;
      if (cls !== f.statusClass) return false;
    }
    if (f.state && en.state !== f.state) return false;
    if (f.errorPhase && en.errorPhase !== f.errorPhase) return false;
    if (traceId && !(en.traceId ?? "").toLowerCase().includes(traceId)) return false;
    if (f.onlyFailed && !en.failed) return false;
    if (f.onlyTruncated && !en.truncated) return false;
    if (f.onlyClosedEarly && !en.closedEarly) return false;
    if (search) {
      const hay = `${en.host}${en.path}`.toLowerCase();
      if (!hay.includes(search)) return false;
    }
    return true;
  });
}

export function sortEntries(entries: NEntry[], key: SortKey, desc: boolean): NEntry[] {
  const sorted = [...entries].sort((a, b) => {
    switch (key) {
      case "duration":
        return a.timeMs - b.timeMs;
      case "status":
        return a.status - b.status;
      case "size":
        return a.respSize - b.respSize;
      default:
        return (a.startMs ?? 0) - (b.startMs ?? 0);
    }
  });
  return desc ? sorted.reverse() : sorted;
}

function uniq(values: Array<string | null>): string[] {
  return [...new Set(values.filter((v): v is string => Boolean(v)))].sort();
}

export function Filters({
  entries,
  filters,
  onChange,
  sortKey,
  sortDesc,
  onSort,
  grouped,
  onGrouped,
  shown,
  total,
}: {
  entries: NEntry[];
  filters: FilterState;
  onChange: (f: FilterState) => void;
  sortKey: SortKey;
  sortDesc: boolean;
  onSort: (key: SortKey, desc: boolean) => void;
  grouped: boolean;
  onGrouped: (v: boolean) => void;
  shown: number;
  total: number;
}) {
  const set = (patch: Partial<FilterState>) => onChange({ ...filters, ...patch });
  const methods = uniq(entries.map((e) => e.method));
  const states = uniq(entries.map((e) => e.state || null));
  const phases = uniq(entries.map((e) => e.errorPhase));

  return (
    <div className="filters">
      <input
        className="filter-search"
        type="search"
        placeholder="filter host/path…"
        value={filters.search}
        onChange={(e) => set({ search: e.target.value })}
      />
      <div className="filter-row">
        <select value={filters.method} onChange={(e) => set({ method: e.target.value })}>
          <option value="">method</option>
          {methods.map((m) => (
            <option key={m}>{m}</option>
          ))}
        </select>
        <select value={filters.statusClass} onChange={(e) => set({ statusClass: e.target.value })}>
          <option value="">status</option>
          {["2xx", "3xx", "4xx", "5xx", "0"].map((c) => (
            <option key={c} value={c}>
              {c === "0" ? "no response" : c}
            </option>
          ))}
        </select>
        <select value={filters.state} onChange={(e) => set({ state: e.target.value })}>
          <option value="">state</option>
          {states.map((s) => (
            <option key={s}>{s}</option>
          ))}
        </select>
        <select value={filters.errorPhase} onChange={(e) => set({ errorPhase: e.target.value })}>
          <option value="">error phase</option>
          {phases.map((p) => (
            <option key={p}>{p}</option>
          ))}
        </select>
      </div>
      <div className="filter-row">
        <input
          className="filter-trace mono"
          type="search"
          placeholder="traceId…"
          value={filters.traceId}
          onChange={(e) => set({ traceId: e.target.value })}
        />
        <select
          value={sortKey}
          onChange={(e) => onSort(e.target.value as SortKey, sortDesc)}
          title="sort key"
        >
          <option value="start">start time</option>
          <option value="duration">duration</option>
          <option value="status">status</option>
          <option value="size">response size</option>
        </select>
        <button
          type="button"
          className={`icon-btn ${sortDesc ? "active" : ""}`}
          title={sortDesc ? "descending" : "ascending"}
          onClick={() => onSort(sortKey, !sortDesc)}
        >
          <ArrowDownUp size={14} />
        </button>
        <button
          type="button"
          className={`icon-btn ${grouped ? "active" : ""}`}
          title="group redirect chains by traceId"
          onClick={() => onGrouped(!grouped)}
        >
          <Layers size={14} />
        </button>
      </div>
      <div className="filter-row checks">
        <label>
          <input type="checkbox" checked={filters.onlyFailed} onChange={(e) => set({ onlyFailed: e.target.checked })} />
          failed
        </label>
        <label>
          <input
            type="checkbox"
            checked={filters.onlyTruncated}
            onChange={(e) => set({ onlyTruncated: e.target.checked })}
          />
          truncated
        </label>
        <label>
          <input
            type="checkbox"
            checked={filters.onlyClosedEarly}
            onChange={(e) => set({ onlyClosedEarly: e.target.checked })}
          />
          closed early
        </label>
        <span className="muted count">
          {shown}/{total}
        </span>
      </div>
    </div>
  );
}
