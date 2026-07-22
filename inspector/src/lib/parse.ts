import type { Har, HarEntry, NEntry, TraceGroup } from "../types/har";
import { parseIsoMs, urlParts } from "./format";

/** HarParseError carries a user-facing explanation of why loading failed. */
export class HarParseError extends Error {
  detail?: string;
  constructor(message: string, detail?: string) {
    super(message);
    this.name = "HarParseError";
    this.detail = detail;
  }
}

export interface LoadedHar {
  har: Har;
  entries: NEntry[];
  format: "har" | "ndjson" | "live";
}

/** parseHar validates and normalizes a HAR document or JSONStreamRecorder NDJSON. */
export function parseHar(text: string): LoadedHar {
  let root: unknown;
  try {
    root = JSON.parse(text);
  } catch (err) {
    return parseNdjson(text, err);
  }
  if (typeof root !== "object" || root === null || Array.isArray(root)) {
    throw new HarParseError("The file is JSON but not a HAR document (top level must be an object).");
  }
  const log = (root as Record<string, unknown>)["log"];
  if (typeof log !== "object" || log === null) {
    if (looksLikeEntry(root)) return loadedNdjson([root as HarEntry]);
    throw new HarParseError('Not a HAR document: missing the top-level "log" object.');
  }
  const entriesRaw = (log as Record<string, unknown>)["entries"];
  if (!Array.isArray(entriesRaw)) {
    throw new HarParseError('Not a HAR document: "log.entries" is missing or not an array.');
  }
  const entries = normalizeEntries(entriesRaw, (i) => `Entry #${i}`);
  return { har: root as Har, entries, format: "har" };
}

function parseNdjson(text: string, originalError: unknown): LoadedHar {
  const rawEntries: HarEntry[] = [];
  const lineNumbers: number[] = [];
  const lines = text.split(/\r?\n/);

  for (let i = 0; i < lines.length; i += 1) {
    const line = lines[i].trim();
    if (line === "") continue;

    let raw: unknown;
    try {
      raw = JSON.parse(line);
    } catch (err) {
      throw new HarParseError(
        `Invalid NDJSON at line ${i + 1}.`,
        err instanceof Error ? err.message : String(err),
      );
    }

    if (!looksLikeEntry(raw)) {
      throw new HarParseError(`NDJSON line ${i + 1} is not a HAR entry.`);
    }
    rawEntries.push(raw as HarEntry);
    lineNumbers.push(i + 1);
  }

  if (rawEntries.length === 0) {
    throw new HarParseError(
      "The file is not valid HAR JSON or recorder NDJSON.",
      originalError instanceof Error ? originalError.message : String(originalError),
    );
  }

  return loadedNdjson(rawEntries, lineNumbers);
}

function loadedNdjson(rawEntries: HarEntry[], lineNumbers = rawEntries.map((_, i) => i + 1)): LoadedHar {
  const entries = normalizeEntries(rawEntries, (i) => `NDJSON line ${lineNumbers[i]}`);
  const har: Har = {
    log: {
      version: "1.2",
      creator: { name: "github.com/mgurevin/recorder/inspector", version: "ndjson-import" },
      entries: rawEntries,
      comment: "Synthetic HAR wrapper created in memory from recorder NDJSON.",
    },
  };
  return { har, entries, format: "ndjson" };
}

function looksLikeEntry(value: unknown): boolean {
  if (typeof value !== "object" || value === null || Array.isArray(value)) return false;
  const candidate = value as Record<string, unknown>;
  const request = candidate["request"];
  const response = candidate["response"];
  return typeof request === "object" && request !== null && !Array.isArray(request) &&
    typeof response === "object" && response !== null && !Array.isArray(response);
}

function normalizeEntries(rawEntries: unknown[], label: (index: number) => string): NEntry[] {
  return rawEntries.map((raw, i) => {
    if (typeof raw !== "object" || raw === null || Array.isArray(raw)) {
      throw new HarParseError(`${label(i)} is not an object.`);
    }
    const entry = raw as HarEntry;
    if (entry._recorder && entry._recorder.schemaVersion !== "1") {
      throw new HarParseError(`${label(i)} uses unsupported recorder extension schema ${String(entry._recorder.schemaVersion)}.`);
    }
    return normalizeEntry(entry, i);
  });
}

function num(v: unknown, fallback: number): number {
  return typeof v === "number" && Number.isFinite(v) ? v : fallback;
}

function normalizeEntry(e: HarEntry, id: number): NEntry {
  const req = e.request ?? ({ method: "", url: "", httpVersion: "", cookies: [], headers: [], queryString: [], headersSize: -1, bodySize: -1 } as HarEntry["request"]);
  const resp = e.response;
  const { host, path } = urlParts(req.url ?? "");
  const status = num(resp?.status, 0);
  const state = typeof e._recorder?.state === "string" ? e._recorder?.state : "";
  const respSize =
    e._recorder?.responseBody && Number.isFinite(e._recorder?.responseBody.totalBytes)
      ? e._recorder?.responseBody.totalBytes
      : num(resp?.content?.size, -1);
  return {
    id,
    e,
    startMs: parseIsoMs(e.startedDateTime),
    timeMs: Math.max(0, num(e.time, 0)),
    method: req.method || "?",
    url: req.url ?? "",
    host,
    path,
    status,
    state,
    errorPhase: e._recorder?.error?.phase ?? null,
    respSize,
    traceId: typeof e._recorder?.traceId === "string" && e._recorder?.traceId !== "" ? e._recorder?.traceId : null,
    redirectIndex: typeof e._recorder?.redirectIndex === "number" ? e._recorder?.redirectIndex : null,
    failed: e._recorder?.error != null || state === "failed",
    truncated: Boolean(e._recorder?.requestBody?.truncated) || Boolean(e._recorder?.responseBody?.truncated),
    closedEarly: state === "closed_early" || Boolean(e._recorder?.responseBody?.closedEarly),
  };
}

/** knownExtensionKeys are the "_" fields this UI renders in dedicated tabs. */
export const knownExtensionKeys = new Set(["_recorder"]);

/** extensionFields returns every "_" field of an entry (known and unknown). */
export function extensionFields(e: HarEntry): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const key of Object.keys(e)) {
    if (key.startsWith("_")) out[key] = (e as unknown as Record<string, unknown>)[key];
  }
  return out;
}

/** groupByTrace clusters entries by _recorder.traceId, ordered by redirectIndex. */
export function groupByTrace(entries: NEntry[]): TraceGroup[] {
  const byTrace = new Map<string, NEntry[]>();
  const groups: TraceGroup[] = [];
  for (const en of entries) {
    if (en.traceId == null) {
      groups.push(makeGroup(null, [en]));
      continue;
    }
    const list = byTrace.get(en.traceId);
    if (list) {
      list.push(en);
    } else {
      const fresh = [en];
      byTrace.set(en.traceId, fresh);
      groups.push(makeGroup(en.traceId, fresh)); // placeholder, finalized below
    }
  }
  // Finalize multi-entry groups (sorted by redirectIndex, then start).
  return groups.map((g) => {
    if (g.traceId == null) return g;
    const list = byTrace.get(g.traceId) ?? g.entries;
    const sorted = [...list].sort((a, b) => {
      const ra = a.redirectIndex ?? 0;
      const rb = b.redirectIndex ?? 0;
      if (ra !== rb) return ra - rb;
      return (a.startMs ?? 0) - (b.startMs ?? 0);
    });
    return makeGroup(g.traceId, sorted);
  });
}

function makeGroup(traceId: string | null, entries: NEntry[]): TraceGroup {
  const last = entries[entries.length - 1];
  return {
    traceId,
    entries,
    hops: entries.length,
    finalStatus: last?.status ?? 0,
    totalMs: entries.reduce((sum, e) => sum + e.timeMs, 0),
    hasFailed: entries.some((e) => e.failed),
  };
}
