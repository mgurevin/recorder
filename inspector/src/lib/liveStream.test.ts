import { describe, expect, it } from "vitest";
import { liveReconnectDelay, parseLiveEntry, validateDebugStreamURL } from "./liveStream";

const entry = {
  startedDateTime: "2026-07-22T10:00:00.000Z",
  time: 12,
  request: { method: "GET", url: "https://example.test/", httpVersion: "HTTP/1.1", cookies: [], headers: [], queryString: [], headersSize: -1, bodySize: 0 },
  response: { status: 200, statusText: "OK", httpVersion: "HTTP/1.1", cookies: [], headers: [], content: { size: 0, mimeType: "" }, redirectURL: "", headersSize: -1, bodySize: 0 },
  cache: {},
  timings: { blocked: -1, dns: -1, connect: -1, send: 0, wait: 10, receive: 2, ssl: -1 },
};

describe("validateDebugStreamURL", () => {
  it("accepts loopback HTTP URLs and strips credentials", () => {
    expect(validateDebugStreamURL("http://user:secret@localhost:7070/entries").toString()).toBe("http://localhost:7070/entries");
    expect(validateDebugStreamURL("https://127.0.0.1:7443/live").hostname).toBe("127.0.0.1");
  });

  it("rejects remote and non-HTTP URLs", () => {
    expect(() => validateDebugStreamURL("https://example.com/entries")).toThrow(/loopback/);
    expect(() => validateDebugStreamURL("file:///tmp/entries")).toThrow(/HTTP/);
  });
});

describe("parseLiveEntry", () => {
  it("normalizes one NDJSON entry and assigns the stream id", () => {
    const parsed = parseLiveEntry(JSON.stringify(entry), 42);
    expect(parsed.id).toBe(42);
    expect(parsed.host).toBe("example.test");
    expect(parsed.status).toBe(200);
  });

  it("rejects non-entry payloads", () => {
    expect(() => parseLiveEntry("{}", 1)).toThrow(/missing.*log/i);
  });
});

describe("liveReconnectDelay", () => {
  it("backs off to a bounded delay and never signals a retry limit", () => {
    expect([0, 1, 2, 3, 4, 5, 20].map(liveReconnectDelay)).toEqual([
      500, 1_000, 2_000, 4_000, 8_000, 10_000, 10_000,
    ]);
  });
});
