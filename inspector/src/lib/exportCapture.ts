import type { Har, HarEntry, NEntry } from "../types/har";
import { protectedOccurrences, withResolvedValues } from "./protection";

export type ExportFormat = "har" | "ndjson";
export type ExportProtection = "protected" | "resolved";

export interface ExportOptions {
  resolvedValues?: ReadonlyMap<string, string>;
}

export interface ExportReadiness {
  entries: number;
  protectedValues: number;
  externalBodies: number;
  incompleteBodies: number;
}

export interface ResolvedExportSummary {
  resolvedLocations: number;
  uniqueResolvedValues: number;
  encryptedLocations: number;
  tokenizedLocations: number;
  unresolvedLocations: number;
  keyIds: string[];
  areas: Array<{ name: string; count: number }>;
}

export function exportCapture(
  har: Har,
  entries: readonly NEntry[],
  format: ExportFormat,
  options: ExportOptions = {},
): string {
  const rawEntries = entries.map((entry) => (
    options.resolvedValues == null
      ? entry.e
      : withResolvedValues(entry.e, options.resolvedValues)
  ));
  if (format === "ndjson") {
    return rawEntries.map((entry) => JSON.stringify(entry)).join("\n") + (rawEntries.length > 0 ? "\n" : "");
  }

  return JSON.stringify({
    ...har,
    log: {
      ...har.log,
      entries: rawEntries,
    },
  }, null, 2) + "\n";
}

export function exportReadiness(entries: readonly NEntry[]): ExportReadiness {
  let protectedValues = 0;
  let externalBodies = 0;
  let incompleteBodies = 0;

  for (const entry of entries) {
    protectedValues += protectedOccurrences(entry.e).length;

    for (const body of bodyInfos(entry.e)) {
      if (body.store) externalBodies += 1;
      if (body.truncated || body.closedEarly || !body.complete || body.capturedBytes < body.totalBytes) {
        incompleteBodies += 1;
      }
    }
  }

  return { entries: entries.length, protectedValues, externalBodies, incompleteBodies };
}

export function resolvedExportSummary(
  entries: readonly NEntry[],
  resolvedValues: ReadonlyMap<string, string>,
): ResolvedExportSummary {
  const unique = new Set<string>();
  const keyIds = new Set<string>();
  const areas = new Map<string, number>();
  let resolvedLocations = 0;
  let encryptedLocations = 0;
  let tokenizedLocations = 0;
  let unresolvedLocations = 0;

  for (const entry of entries) {
    for (const occurrence of protectedOccurrences(entry.e)) {
      if (!resolvedValues.has(occurrence.token)) {
        unresolvedLocations += 1;
        continue;
      }

      resolvedLocations += 1;
      unique.add(occurrence.token);
      keyIds.add(occurrence.keyId);
      if (occurrence.mode === "encrypt") encryptedLocations += 1;
      else tokenizedLocations += 1;

      const area = exportArea(occurrence.path);
      areas.set(area, (areas.get(area) ?? 0) + 1);
    }
  }

  return {
    resolvedLocations,
    uniqueResolvedValues: unique.size,
    encryptedLocations,
    tokenizedLocations,
    unresolvedLocations,
    keyIds: [...keyIds].sort(),
    areas: [...areas].map(([name, count]) => ({ name, count })).sort((left, right) => left.name.localeCompare(right.name)),
  };
}

export function exportFilename(
  sourceName: string,
  format: ExportFormat,
  scope: string,
  protection: ExportProtection = "protected",
): string {
  const leaf = sourceName.split(/[\\/]/).pop() ?? "capture";
  const base = leaf.replace(/\.(?:har|ndjson)$/i, "") || "capture";
  const safeScope = scope.replace(/[^a-z0-9_-]+/gi, "-").replace(/^-+|-+$/g, "").toLowerCase();
  const sensitivity = protection === "resolved" ? ".resolved" : "";
  return `${base}${safeScope ? `-${safeScope}` : ""}${sensitivity}.${format}`;
}

function bodyInfos(entry: HarEntry) {
  const extension = entry._recorder;
  return [extension?.requestBody, extension?.responseBody].filter((body) => body != null);
}

function exportArea(path: string): string {
  if (path.startsWith("request.url")) return "request URL";
  if (path.startsWith("request.headers")) return "request headers";
  if (path.startsWith("request.cookies")) return "request cookies";
  if (path.startsWith("request.postData")) return "request body";
  if (path.startsWith("response.headers")) return "response headers";
  if (path.startsWith("response.cookies")) return "response cookies";
  if (path.startsWith("response.content")) return "response body";
  if (path.includes("requestTrailers")) return "request trailers";
  if (path.includes("responseTrailers")) return "response trailers";
  if (path.startsWith("_recorder.network")) return "network and proxy";
  if (path.startsWith("_recorder")) return "recorder diagnostics";
  return "other fields";
}
