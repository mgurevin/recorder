import type { Har, HarEntry, NEntry } from "../types/har";
import { protectedOccurrences } from "./protection";

export type ExportFormat = "har" | "ndjson";

export interface ExportReadiness {
  entries: number;
  protectedValues: number;
  externalBodies: number;
  incompleteBodies: number;
}

export function exportCapture(har: Har, entries: readonly NEntry[], format: ExportFormat): string {
  const rawEntries = entries.map((entry) => entry.e);
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

export function exportFilename(sourceName: string, format: ExportFormat, scope: string): string {
  const leaf = sourceName.split(/[\\/]/).pop() ?? "capture";
  const base = leaf.replace(/\.(?:har|ndjson)$/i, "") || "capture";
  const safeScope = scope.replace(/[^a-z0-9_-]+/gi, "-").replace(/^-+|-+$/g, "").toLowerCase();
  return `${base}${safeScope ? `-${safeScope}` : ""}.${format}`;
}

function bodyInfos(entry: HarEntry) {
  const extension = entry._recorder;
  return [extension?.requestBody, extension?.responseBody].filter((body) => body != null);
}
