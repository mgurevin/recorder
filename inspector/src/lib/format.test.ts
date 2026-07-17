import { describe, expect, it } from "vitest";
import {
  base64Size,
  formatBytes,
  formatDuration,
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
  it("falls back to text for invalid json", () => {
    expect(prettyBody("application/json", "{oops", undefined).kind).toBe("text");
  });
  it("marks base64 content as binary without decoding", () => {
    const out = prettyBody("application/octet-stream", "AAAA", "base64");
    expect(out.kind).toBe("binary");
    expect(out.text).toBeUndefined();
  });
  it("handles empty bodies", () => {
    expect(prettyBody("text/plain", undefined, undefined).kind).toBe("empty");
  });
  it("indents xml", () => {
    const out = prettyBody("text/xml", "<a><b>x</b></a>", undefined);
    expect(out.kind).toBe("xml");
    expect(out.text).toContain("\n  <b>x</b>");
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
