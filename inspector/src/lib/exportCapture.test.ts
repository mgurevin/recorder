import { describe, expect, it } from "vitest";
import { parseHar } from "./parse";
import { exportCapture, exportFilename, exportReadiness, resolvedExportSummary } from "./exportCapture";
import { sampleHar } from "../sampleHar";

const encryptedToken = "REC-ENC-v1.a2V5.cGF5bG9hZA";
const tokenizedToken = "REC-TOK-v1.dG9rZW4.aGFzaA";
const numberToken = "REC-ENC-v1.bnVtYmVy.cGF5bG9hZA";
const objectToken = "REC-ENC-v1.b2JqZWN0.cGF5bG9hZA";

describe("capture export", () => {
  it("preserves capture metadata and exports only selected original entries as HAR", () => {
    const loaded = parseHar(JSON.stringify(sampleHar));
    const selected = [loaded.entries[1]];
    const output = JSON.parse(exportCapture(loaded.har, selected, "har"));

    expect(output.log.creator).toEqual(sampleHar.log.creator);
    expect(output.log.entries).toEqual([sampleHar.log.entries[1]]);
  });

  it("emits parseable one-entry-per-line NDJSON", () => {
    const loaded = parseHar(JSON.stringify(sampleHar));
    const output = exportCapture(loaded.har, loaded.entries.slice(0, 2), "ndjson");
    const lines = output.trimEnd().split("\n");

    expect(lines).toHaveLength(2);
    expect(lines.map((line) => JSON.parse(line))).toEqual(sampleHar.log.entries.slice(0, 2));
  });

  it("never substitutes in-memory resolved plaintext", () => {
    const loaded = parseHar(JSON.stringify(sampleHar));
    const original = loaded.entries[0].e.request.url;

    expect(exportCapture(loaded.har, [loaded.entries[0]], "ndjson")).toContain(original);
  });

  it("exports resolved values across the complete entry without mutating source evidence", () => {
    const capture = structuredClone(sampleHar);
    const entry = capture.log.entries[0];
    entry.request.url = `https://api.example.com/orders?token=${encryptedToken}`;
    entry.request.headers = [{ name: "Authorization", value: `Bearer ${encryptedToken}` }];
    entry.request.postData = {
      mimeType: "application/json",
      text: `{"secret":"${tokenizedToken}","pin":"${numberToken}","metadata":"${objectToken}"}`,
    };
    entry.response.cookies = [{ name: "session", value: encryptedToken }];
    entry.response.content.mimeType = "application/json";
    entry.response.content.text = `{"secret":"${encryptedToken}","pin":"${numberToken}"}`;
    entry._recorder = {
      ...entry._recorder!,
      requestTrailers: [{ name: "Digest", value: tokenizedToken }],
      network: {
        ...entry._recorder!.network!,
        proxy: `http://user:${encryptedToken}@proxy.example`,
      },
    };
    const loaded = parseHar(JSON.stringify(capture));
    const original = structuredClone(loaded.entries[0].e);
    const resolved = new Map([
      [encryptedToken, '"plain-secret"'],
      [tokenizedToken, '"verified-secret"'],
      [numberToken, "1234"],
      [objectToken, '{"source":"fixture"}'],
    ]);

    const output = exportCapture(loaded.har, [loaded.entries[0]], "har", { resolvedValues: resolved });
    const exported = JSON.parse(output).log.entries[0];

    expect(JSON.stringify(exported)).not.toContain("REC-");
    expect(JSON.stringify(exported)).toContain("plain-secret");
    expect(JSON.stringify(exported)).toContain("verified-secret");
    expect(JSON.parse(exported.request.postData.text)).toEqual({
      secret: "verified-secret",
      pin: 1234,
      metadata: { source: "fixture" },
    });
    expect(JSON.parse(exported.response.content.text)).toEqual({ secret: "plain-secret", pin: 1234 });
    expect(loaded.entries[0].e).toEqual(original);
  });

  it.each(["har", "ndjson"] as const)("exports parseable %s with resolved JSON value types", (format) => {
    const capture = structuredClone(sampleHar);
    const entry = capture.log.entries[0];
    entry.request.postData = {
      mimeType: "application/problem+json",
      text: `{"secret":"${encryptedToken}","pin":"${numberToken}"}`,
    };
    const loaded = parseHar(JSON.stringify(capture));
    const resolved = new Map([
      [encryptedToken, '"plain-secret"'],
      [numberToken, "1234"],
    ]);

    const output = exportCapture(loaded.har, [loaded.entries[0]], format, { resolvedValues: resolved });
    const exported = format === "har"
      ? JSON.parse(output).log.entries[0]
      : JSON.parse(output.trim());

    expect(JSON.parse(exported.request.postData.text)).toEqual({ secret: "plain-secret", pin: 1234 });
  });

  it("summarizes resolved export risk without exposing plaintext", () => {
    const capture = structuredClone(sampleHar);
    const entry = capture.log.entries[0];
    entry.request.url = `https://api.example.com/?token=${encryptedToken}`;
    entry.response.content.text = tokenizedToken;
    const loaded = parseHar(JSON.stringify(capture));
    const resolved = new Map([
      [encryptedToken, "url-plaintext"],
      [tokenizedToken, "body-plaintext"],
    ]);

    const summary = resolvedExportSummary([loaded.entries[0]], resolved);
    const serialized = JSON.stringify(summary);

    expect(summary).toMatchObject({
      resolvedLocations: 2,
      uniqueResolvedValues: 2,
      encryptedLocations: 1,
      tokenizedLocations: 1,
      unresolvedLocations: 0,
      keyIds: ["key", "token"],
    });
    expect(summary.areas).toEqual([
      { name: "request URL", count: 1 },
      { name: "response body", count: 1 },
    ]);
    expect(serialized).not.toContain("url-plaintext");
    expect(serialized).not.toContain("body-plaintext");
  });

  it("summarizes fixture readiness without exposing values", () => {
    const loaded = parseHar(JSON.stringify(sampleHar));
    loaded.entries[0].e._recorder = {
      ...loaded.entries[0].e._recorder!,
      requestBody: {
        present: true,
        complete: false,
        truncated: true,
        capturedBytes: 4,
        totalBytes: 8,
        store: "filebody:v1:opaque",
      },
    };

    expect(exportReadiness([loaded.entries[0]])).toMatchObject({
      entries: 1,
      externalBodies: 1,
      incompleteBodies: 1,
    });
  });

  it("creates filesystem-safe names", () => {
    expect(exportFilename("/tmp/order 42.har", "ndjson", "selected entries")).toBe(
      "order 42-selected-entries.ndjson",
    );
    expect(exportFilename("/tmp/order 42.har", "har", "selected entries", "resolved")).toBe(
      "order 42-selected-entries.resolved.har",
    );
  });
});
