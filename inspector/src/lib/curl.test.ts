import { describe, expect, it } from "vitest";
import type { HarEntry } from "../types/har";
import { curlReplay, interfaceAddress, shellQuote } from "./curl";

function entry(overrides: Partial<HarEntry> = {}): HarEntry {
  return {
    startedDateTime: "2026-01-01T00:00:00Z",
    time: 1,
    request: {
      method: "POST",
      url: "https://example.com/api?q=it's",
      httpVersion: "HTTP/2.0",
      cookies: [],
      headers: [
        { name: ":authority", value: "example.com" },
        { name: "Content-Type", value: "application/json" },
        { name: "Content-Length", value: "20" },
        { name: "Authorization", value: "[REDACTED]" },
      ],
      queryString: [],
      postData: { mimeType: "application/json", text: `{"name":"O'Reilly"}` },
      headersSize: -1,
      bodySize: 19,
    },
    response: {
      status: 200,
      statusText: "OK",
      httpVersion: "HTTP/2.0",
      cookies: [],
      headers: [],
      content: { size: 0, mimeType: "text/plain" },
      redirectURL: "",
      headersSize: -1,
      bodySize: 0,
    },
    cache: {},
    timings: { blocked: -1, dns: -1, connect: -1, ssl: -1, send: -1, wait: -1, receive: -1 },
    ...overrides,
  };
}

describe("curlReplay", () => {
  it("quotes values and excludes transport-generated headers", () => {
    const out = curlReplay(entry());
    expect(out.command).toContain(`--request 'POST'`);
    expect(out.command).toContain(`q=it'\\''s`);
    expect(out.command).toContain(`O'\\''Reilly`);
    expect(out.command).not.toContain("Content-Length");
    expect(out.command).not.toContain(":authority");
    expect(out.warnings.join(" ")).toContain("[REDACTED]");
  });

  it("uses explicitly supplied decrypted values with JSON structure preserved", () => {
    const token = "REC-ENC-v1.a2lk.AA";
    const e = entry({
      request: {
        ...entry().request,
        url: `https://example.test/?token=${token}`,
        headers: [{ name: "Authorization", value: `Bearer ${token}` }],
        postData: { mimeType: "application/json", text: `{"password":"${token}","keep":1}` },
      },
    });
    const out = curlReplay(e, { decryptedValues: new Map([[token, `{"raw":true}`]]) });
    expect(out.command).toContain("token=%7B%22raw%22%3Atrue%7D");
    expect(out.command).toContain(`Bearer {"raw":true}`);
    expect(out.command).toContain(`{"password":{"raw":true},"keep":1}`);
    expect(out.warnings.join(" ")).toContain("inserted");
  });

  it("does not use decrypted values unless they are explicitly supplied", () => {
    const token = "REC-ENC-v1.a2lk.AA";
    const e = entry({ request: { ...entry().request, url: `https://example.test/?token=${token}` } });
    const out = curlReplay(e);
    expect(out.command).toContain(token);
    expect(out.warnings.join(" ")).toContain("remain protected");
  });

  it("omits binary request bodies with a warning", () => {
    const e = entry();
    e.request!.postData!._encoding = "base64";
    e.request!.postData!.text = "AAAA";
    const out = curlReplay(e);
    expect(out.command).not.toContain("--data-binary");
    expect(out.warnings.join(" ")).toContain("binary/base64");
  });

  it("includes the recorded proxy", () => {
    const out = curlReplay(entry({
      _network: {
        proxy: "http://proxy.example:8080",
        connectionReused: false,
        wasIdle: false,
        http2: false,
      },
    }));
    expect(out.command).toContain("--proxy 'http://proxy.example:8080'");
  });

  it("optionally includes the recorded local IP without its port", () => {
    const e = entry({
      _network: {
        localAddress: "192.0.2.10:54321",
        connectionReused: false,
        wasIdle: false,
        http2: false,
      },
    });
    expect(curlReplay(e).command).not.toContain("--interface");
    expect(curlReplay(e, { includeLocalInterface: true }).command).toContain("--interface '192.0.2.10'");
  });
});

describe("shellQuote", () => {
  it("quotes an empty argument", () => expect(shellQuote("")).toBe("''"));
});

describe("interfaceAddress", () => {
  it("handles IPv4 and bracketed IPv6 addresses", () => {
    expect(interfaceAddress("127.0.0.1:1234")).toBe("127.0.0.1");
    expect(interfaceAddress("[2001:db8::1]:443")).toBe("2001:db8::1");
  });
});
