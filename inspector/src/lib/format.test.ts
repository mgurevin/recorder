import { describe, expect, it } from "vitest";
import {
  base64Size,
  formatBytes,
  formatDuration,
  formatTimelineTooltip,
  formatTimelineCursor,
  parseIsoMs,
  prettyBody,
  prettyXml,
  relMs,
  statusClass,
  statusTone,
  urlParts,
} from "./format";

describe("formatDuration", () => {
  it("marks unmeasured values", () => {
    expect(formatDuration(-1)).toBe("—");
    expect(formatDuration(null)).toBe("—");
    expect(formatDuration(Number.NaN)).toBe("—");
  });
  it("scales units", () => {
    expect(formatDuration(0.42)).toBe("0.42 ms");
    expect(formatDuration(12.34)).toBe("12.3 ms");
    expect(formatDuration(1234)).toBe("1.23 s");
    expect(formatDuration(65_000)).toBe("1m 5.0s");
  });
});

describe("formatTimelineTooltip", () => {
  it("includes precise relative and absolute bounds", () => {
    expect(formatTimelineTooltip("dns", 0.4, 8.2, Date.parse("2026-07-17T09:15:02.120Z"))).toBe([
      "dns",
      "offset: +0.40 ms → +8.60 ms",
      "duration: 8.20 ms",
      "start: 2026-07-17T09:15:02.120Z",
      "end: 2026-07-17T09:15:02.128Z",
    ].join("\n"));
  });
});

describe("formatTimelineCursor", () => {
  it("renders the cursor offset in seconds with millisecond precision", () => {
    expect(formatTimelineCursor(0.4)).toBe("+0.0004 s");
    expect(formatTimelineCursor(184.6)).toBe("+0.185 s");
    expect(formatTimelineCursor(-1)).toBe("+0.0000 s");
  });
});

describe("formatBytes", () => {
  it("marks unknown sizes", () => {
    expect(formatBytes(-1)).toBe("—");
    expect(formatBytes(undefined)).toBe("—");
  });
  it("scales units", () => {
    expect(formatBytes(512)).toBe("512 B");
    expect(formatBytes(2048)).toBe("2.0 KiB");
    expect(formatBytes(9834127)).toBe("9.4 MiB");
  });
});

describe("statusClass / statusTone", () => {
  it("classifies", () => {
    expect(statusClass(200)).toBe("2xx");
    expect(statusClass(301)).toBe("3xx");
    expect(statusClass(404)).toBe("4xx");
    expect(statusClass(503)).toBe("5xx");
    expect(statusClass(0)).toBe("0");
    expect(statusClass(Number.NaN)).toBe("0");
  });
  it("maps tones", () => {
    expect(statusTone(204)).toBe("ok");
    expect(statusTone(0)).toBe("net");
  });
});

describe("urlParts", () => {
  it("splits host and path+query", () => {
    expect(urlParts("https://api.example.com/v1/x?q=1")).toEqual({
      host: "api.example.com",
      path: "/v1/x?q=1",
    });
  });
  it("keeps ports", () => {
    expect(urlParts("http://127.0.0.1:8080/health").host).toBe("127.0.0.1:8080");
  });
  it("tolerates garbage without throwing", () => {
    expect(urlParts("not a url").host).toBe("not a url");
    expect(urlParts("").host).toBe("");
  });
});

describe("prettyBody", () => {
  it("pretty-prints json", () => {
    const out = prettyBody("application/json", '{"a":1}', undefined);
    expect(out.kind).toBe("json");
    expect(out.text).toContain('"a": 1');
  });
  it("keeps structured bodies byte-for-byte in raw mode", () => {
    const json = '{ "a" : 1, "nested": {"b":2} }\n';
    const xml = '<root><item value="1"> x </item></root>\n';
    expect(prettyBody("application/json", json, undefined, false).text).toBe(json);
    expect(prettyBody("application/xml", xml, undefined, false).text).toBe(xml);
  });
  it("copies the complete representation of the active mode", () => {
    const json = `{"key":"${"x".repeat(512 * 1024)}"}`;
    const raw = prettyBody("application/json", json, undefined, false);
    const formatted = prettyBody("application/json", json, undefined, true);
    expect(raw.copyText).toBe(json);
    expect(formatted.copyText).toBe(JSON.stringify(JSON.parse(json), null, 2));
    expect(formatted.copyText).not.toBe(raw.copyText);
  });
  it("falls back to text for invalid json", () => {
    expect(prettyBody("application/json", "{oops", undefined).kind).toBe("text");
  });
  it("keeps binary base64 visible and copyable", () => {
    const out = prettyBody("application/octet-stream", "AAAA", "base64");
    expect(out.kind).toBe("binary");
    expect(out.text).toBe("AAAA");
    expect(out.note).toContain("3 B");
  });
  it("decodes base64 textual content using its declared charset", () => {
    const latin1 = btoa(String.fromCharCode(0x63, 0x61, 0x66, 0xe9));
    const out = prettyBody("text/html; charset=ISO-8859-1", latin1, "base64");
    expect(out.kind).toBe("text");
    expect(out.text).toBe("café");
    expect(out.note).toContain("iso-8859-1");
  });
  it("offers image preview only when a safe raster signature matches", () => {
    const jpeg = btoa(String.fromCharCode(0xff, 0xd8, 0xff, 0x00));
    expect(prettyBody("image/jpeg", jpeg, "base64").previewImageMime).toBe("image/jpeg");
    expect(prettyBody("image/png", jpeg, "base64").previewImageMime).toBeUndefined();
    expect(prettyBody("image/svg+xml", btoa("<svg/>"), "base64").previewImageMime).toBeUndefined();
  });
  it("offers video playback only when a supported container signature matches", () => {
    const mp4 = btoa(String.fromCharCode(0, 0, 0, 24) + "ftypisom" + String.fromCharCode(0, 0, 0, 0));
    const webm = btoa(String.fromCharCode(0x1a, 0x45, 0xdf, 0xa3, 0x01));
    expect(prettyBody("video/mp4", mp4, "base64").previewVideoMime).toBe("video/mp4");
    expect(prettyBody("video/webm", webm, "base64").previewVideoMime).toBe("video/webm");
    expect(prettyBody("video/mp4", webm, "base64").previewVideoMime).toBeUndefined();
  });
  it("handles empty bodies", () => {
    expect(prettyBody("text/plain", undefined, undefined).kind).toBe("empty");
  });
  it("indents xml", () => {
    const out = prettyBody("text/xml", "<a><b>x</b></a>", undefined);
    expect(out.kind).toBe("xml");
    expect(out.text).toContain("\n  <b>x</b>");
  });
  it("keeps syntax kind when a large structured body is display-truncated", () => {
    const json = `{"key":"${"x".repeat(512 * 1024)}"}`;
    const xml = `<root>${"x".repeat(512 * 1024)}</root>`;
    const jsonOut = prettyBody("application/json", json, undefined);
    const xmlOut = prettyBody("application/xml", xml, undefined);
    expect(jsonOut.kind).toBe("json");
    expect(xmlOut.kind).toBe("xml");
    expect(jsonOut.text?.length).toBe(512 * 1024);
    expect(jsonOut.copyText).toBe(JSON.stringify(JSON.parse(json), null, 2));
    expect(xmlOut.copyText).toBe(prettyXml(xml));
  });
});

describe("prettyXml", () => {
  it("never throws on malformed xml", () => {
    expect(() => prettyXml("<a><b></a>")).not.toThrow();
    expect(() => prettyXml("plain text")).not.toThrow();
  });
});

describe("base64Size", () => {
  it("accounts for padding", () => {
    expect(base64Size("AAAA")).toBe(3);
    expect(base64Size("AAA=")).toBe(2);
    expect(base64Size("AA==")).toBe(1);
  });
});

describe("time helpers", () => {
  it("parses iso timestamps", () => {
    expect(parseIsoMs("2026-07-17T09:15:02.120Z")).toBe(Date.parse("2026-07-17T09:15:02.120Z"));
    expect(parseIsoMs("not a date")).toBeNull();
    expect(parseIsoMs(undefined)).toBeNull();
  });
  it("formats relative offsets", () => {
    const base = Date.parse("2026-01-01T00:00:00Z");
    expect(relMs(base, base + 12)).toBe("+12.0 ms");
    expect(relMs(null, base)).toBe("—");
  });
});
