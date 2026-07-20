import type { HarContent, PostData } from "../types/har";

/** formatDuration renders milliseconds; negative/non-finite means unmeasured. */
export function formatDuration(ms: number | null | undefined): string {
  if (ms == null || !Number.isFinite(ms) || ms < 0) return "—";
  if (ms < 1) return `${ms.toFixed(2)} ms`;
  if (ms < 1000) return `${ms.toFixed(1)} ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(2)} s`;
  return `${Math.floor(ms / 60_000)}m ${((ms % 60_000) / 1000).toFixed(1)}s`;
}

/** formatTimelineTooltip describes an exact range within a waterfall and,
 * when available, its absolute wall-clock bounds. */
export function formatTimelineTooltip(
  label: string,
  offsetMs: number,
  durationMs: number,
  absoluteStartMs?: number | null,
): string {
  const endOffsetMs = offsetMs + durationMs;
  const lines = [
    label,
    `offset: +${offsetMs.toFixed(2)} ms → +${endOffsetMs.toFixed(2)} ms`,
    `duration: ${durationMs.toFixed(2)} ms`,
  ];
  if (absoluteStartMs != null && Number.isFinite(absoluteStartMs)) {
    lines.push(`start: ${new Date(absoluteStartMs).toISOString()}`);
    lines.push(`end: ${new Date(absoluteStartMs + durationMs).toISOString()}`);
  }
  return lines.join("\n");
}

/** formatTimelineCursor renders the waterfall crosshair position in seconds. */
export function formatTimelineCursor(offsetMs: number): string {
  const decimals = offsetMs < 10 ? 4 : 3;
  return `+${(Math.max(offsetMs, 0) / 1000).toFixed(decimals)} s`;
}

/** formatBytes renders byte counts; negative means unknown per HAR. */
export function formatBytes(n: number | null | undefined): string {
  if (n == null || !Number.isFinite(n) || n < 0) return "—";
  if (n < 1024) return `${n} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let v = n;
  let i = -1;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(1)} ${units[i]}`;
}

/** statusClass folds a status code into "0" / "1xx".."5xx". */
export function statusClass(status: number): string {
  if (!Number.isFinite(status) || status <= 0) return "0";
  return `${Math.floor(status / 100)}xx`;
}

/** statusTone maps a status to a CSS tone suffix. */
export function statusTone(status: number): string {
  switch (statusClass(status)) {
    case "2xx":
      return "ok";
    case "3xx":
      return "redirect";
    case "4xx":
      return "client";
    case "5xx":
      return "server";
    default:
      return "net";
  }
}

/** urlParts splits a URL into host and path+query, tolerating garbage. */
export function urlParts(raw: string): { host: string; path: string } {
  try {
    const u = new URL(raw);
    return { host: u.host, path: `${u.pathname}${u.search}` || "/" };
  } catch {
    const m = /^[a-z+.-]+:\/\/([^/?#]*)([^#]*)/i.exec(raw);
    if (m) return { host: m[1], path: m[2] || "/" };
    return { host: raw, path: "" };
  }
}

export type ContentKind = "json" | "xml" | "text" | "binary" | "empty";

export interface PrettyContent {
  kind: ContentKind;
  /** Display/copy text; binary content uses its original base64 representation. */
  text?: string;
  /** Full text used by copy when the display text is truncated. */
  copyText?: string;
  note?: string;
  /** Validated raster MIME eligible for an explicit, local image preview. */
  previewImageMime?: string;
  /** Validated video MIME eligible for explicit local playback. */
  previewVideoMime?: string;
}

const MAX_PRETTY_BYTES = 512 * 1024;
const MAX_IMAGE_PREVIEW_BYTES = 5 * 1024 * 1024;
const MAX_VIDEO_PREVIEW_BYTES = 20 * 1024 * 1024;

/** prettyBody renders response content / postData text for display. */
export function prettyBody(
  mimeType: string | undefined,
  text: string | undefined,
  encoding: string | undefined,
): PrettyContent {
  if (!text) return { kind: "empty" };
  if (encoding === "base64") {
    const size = base64Size(text);
    if (isTextualMime(mimeType)) {
      const charset = mimeCharset(mimeType) ?? "utf-8";
      try {
        const bytes = decodeBase64(text);
        const decoded = new TextDecoder(charset).decode(bytes);
        const pretty = prettyBody(mimeType, decoded, undefined);
        return {
          ...pretty,
          note: `${pretty.note ? `${pretty.note} · ` : ""}${formatBytes(size)} decoded from HAR base64 as ${charset}`,
        };
      } catch {
        // Unsupported charset or malformed third-party HAR: retain the raw
        // base64 below so it remains visible and copyable.
      }
    }
    return {
      kind: "binary",
      text,
      note: `binary content, ${formatBytes(size)} (base64-encoded in the HAR)`,
      previewImageMime: safeImageMime(mimeType, text, size),
      previewVideoMime: safeVideoMime(mimeType, text, size),
    };
  }
  const mt = (mimeType ?? "").toLowerCase();
  if (text.length > MAX_PRETTY_BYTES) {
    const kind: ContentKind = mt.includes("json")
      ? "json"
      : mt.includes("xml") || text.trimStart().startsWith("<?xml")
        ? "xml"
        : "text";
    return {
      kind,
      text: text.slice(0, MAX_PRETTY_BYTES),
      copyText: text,
      note: `showing first ${formatBytes(MAX_PRETTY_BYTES)} of ${formatBytes(text.length)}`,
    };
  }
  if (mt.includes("json")) {
    try {
      return { kind: "json", text: JSON.stringify(JSON.parse(text), null, 2) };
    } catch {
      // fall through to plain text
    }
  }
  if (mt.includes("xml") || text.trimStart().startsWith("<?xml")) {
    return { kind: "xml", text: prettyXml(text) };
  }
  return { kind: "text", text };
}

function isTextualMime(mimeType: string | undefined): boolean {
  const mt = (mimeType ?? "").split(";", 1)[0].trim().toLowerCase();
  return (
    mt.startsWith("text/") ||
    mt.endsWith("+json") ||
    mt.endsWith("+xml") ||
    [
      "application/json",
      "application/xml",
      "application/javascript",
      "application/ecmascript",
      "application/x-www-form-urlencoded",
      "application/x-ndjson",
      "application/xhtml+xml",
      "image/svg+xml",
    ].includes(mt)
  );
}

function mimeCharset(mimeType: string | undefined): string | undefined {
  const match = /(?:^|;)\s*charset\s*=\s*(?:"([^"]+)"|([^;\s]+))/i.exec(mimeType ?? "");
  return (match?.[1] ?? match?.[2])?.trim().toLowerCase();
}

export function decodeBase64(text: string): Uint8Array {
  const raw = atob(text.replace(/\s/g, ""));
  const bytes = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) bytes[i] = raw.charCodeAt(i);
  return bytes;
}

/**
 * Returns a raster MIME only when the declared type and file signature agree.
 * SVG is deliberately excluded: previews must never introduce active content.
 */
function safeImageMime(mimeType: string | undefined, base64: string, size: number): string | undefined {
  if (size <= 0 || size > MAX_IMAGE_PREVIEW_BYTES) return undefined;
  const mt = (mimeType ?? "").split(";", 1)[0].trim().toLowerCase();
  let bytes: Uint8Array;
  try {
    bytes = decodeBase64(base64);
  } catch {
    return undefined;
  }
  const ascii = (start: number, value: string) =>
    bytes.length >= start + value.length && [...value].every((c, i) => bytes[start + i] === c.charCodeAt(0));
  const signatures: Record<string, boolean> = {
    "image/jpeg": bytes.length >= 3 && bytes[0] === 0xff && bytes[1] === 0xd8 && bytes[2] === 0xff,
    "image/png":
      bytes.length >= 8 &&
      bytes[0] === 0x89 &&
      ascii(1, "PNG") &&
      bytes[4] === 0x0d &&
      bytes[5] === 0x0a &&
      bytes[6] === 0x1a &&
      bytes[7] === 0x0a,
    "image/gif": ascii(0, "GIF87a") || ascii(0, "GIF89a"),
    "image/webp": ascii(0, "RIFF") && ascii(8, "WEBP"),
  };
  return signatures[mt] ? mt : undefined;
}

/** Returns a video MIME only when its declared type and container signature agree. */
function safeVideoMime(mimeType: string | undefined, base64: string, size: number): string | undefined {
  if (size <= 0 || size > MAX_VIDEO_PREVIEW_BYTES) return undefined;
  const mt = (mimeType ?? "").split(";", 1)[0].trim().toLowerCase();
  let bytes: Uint8Array;
  try {
    bytes = decodeBase64(base64);
  } catch {
    return undefined;
  }
  const ascii = (start: number, value: string) =>
    bytes.length >= start + value.length && [...value].every((c, i) => bytes[start + i] === c.charCodeAt(0));
  const signatures: Record<string, boolean> = {
    // ISO Base Media File Format: the first box is normally ftyp.
    "video/mp4": bytes.length >= 12 && ascii(4, "ftyp"),
    // EBML header used by WebM.
    "video/webm":
      bytes.length >= 4 && bytes[0] === 0x1a && bytes[1] === 0x45 && bytes[2] === 0xdf && bytes[3] === 0xa3,
  };
  return signatures[mt] ? mt : undefined;
}

export function prettyPostData(pd: PostData | undefined): PrettyContent {
  if (!pd) return { kind: "empty" };
  return prettyBody(pd.mimeType, pd.text, pd._encoding);
}

export function prettyContent(c: HarContent | undefined): PrettyContent {
  if (!c) return { kind: "empty" };
  return prettyBody(c.mimeType, c.text, c.encoding);
}

/** base64Size estimates the decoded byte size of a base64 string. */
export function base64Size(b64: string): number {
  const padding = b64.endsWith("==") ? 2 : b64.endsWith("=") ? 1 : 0;
  return Math.max(0, Math.floor((b64.length * 3) / 4) - padding);
}

/**
 * prettyXml is a lightweight indenter: it never parses, only re-flows tag
 * boundaries, so malformed XML degrades gracefully instead of throwing.
 */
export function prettyXml(xml: string): string {
  const compact = xml.replace(/>\s+</g, "><").trim();
  const parts = compact.replace(/></g, ">\u0000<").split("\u0000");
  let depth = 0;
  const out: string[] = [];
  for (const part of parts) {
    const isClosing = /^<\//.test(part);
    const isSelfContained =
      /\/>$/.test(part) ||
      /^<[?!]/.test(part) ||
      /^<([^\s>/]+)[^>]*>[^<]*<\/\1>$/.test(part);
    if (isClosing) depth = Math.max(0, depth - 1);
    out.push("  ".repeat(depth) + part);
    if (!isClosing && !isSelfContained && /^<[^\s>/]/.test(part)) depth++;
  }
  return out.join("\n");
}

/** parseIsoMs parses an ISO timestamp, null on failure. */
export function parseIsoMs(s: string | undefined): number | null {
  if (!s) return null;
  const ms = Date.parse(s);
  return Number.isFinite(ms) ? ms : null;
}

/** relMs formats an offset relative to a base timestamp. */
export function relMs(baseMs: number | null, ms: number | null): string {
  if (baseMs == null || ms == null) return "—";
  const d = ms - baseMs;
  return `+${formatDuration(Math.max(0, d))}`;
}

/** shortId trims correlation ids for list display. */
export function shortId(id: string | null | undefined, n = 8): string {
  if (!id) return "";
  return id.length > n ? id.slice(0, n) : id;
}
