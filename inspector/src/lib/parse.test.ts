import { describe, expect, it } from "vitest";
import { captureMetadata, HarParseError, extensionFields, groupByTrace, parseHar } from "./parse";
import { sampleHar } from "../sampleHar";
import { maximalEvidenceHar } from "../test/maximalEvidenceHar";

const sampleText = JSON.stringify(sampleHar);

describe("parseHar", () => {
  it("loads the sample document", () => {
    const { har, entries, format } = parseHar(sampleText);
    expect(har.log.version).toBe("1.2");
    expect(format).toBe("har");
    expect(entries).toHaveLength(6);
    const first = entries[0];
    expect(first.method).toBe("GET");
    expect(first.host).toBe("api.example.com");
    expect(first.path).toContain("/v1/orders");
    expect(first.status).toBe(200);
    expect(first.state).toBe("completed");
    expect(first.timeMs).toBeCloseTo(184.6);
  });

  it("rejects invalid JSON and identifies the NDJSON line", () => {
    expect(() => parseHar("{oops")).toThrowError(HarParseError);
    try {
      parseHar("{oops");
    } catch (err) {
      expect((err as HarParseError).message).toContain("line 1");
      expect((err as HarParseError).detail).toBeTruthy();
    }
  });

  it("loads JSONStreamRecorder NDJSON and ignores blank lines", () => {
    const text = `${JSON.stringify(sampleHar.log.entries[0])}\n\n${JSON.stringify(sampleHar.log.entries[1])}\n`;
    const { har, entries, format } = parseHar(text);

    expect(format).toBe("ndjson");
    expect(entries).toHaveLength(2);
    expect(entries[0].host).toBe("api.example.com");
    expect(har.log.entries).toHaveLength(2);
    expect(har.log.comment).toContain("Synthetic HAR wrapper");
  });

  it("loads a single NDJSON entry", () => {
    const { entries, format } = parseHar(JSON.stringify(sampleHar.log.entries[0]));
    expect(format).toBe("ndjson");
    expect(entries).toHaveLength(1);
  });

  it("reports a malformed NDJSON line without partially loading", () => {
    const text = `${JSON.stringify(sampleHar.log.entries[0])}\n{oops\n${JSON.stringify(sampleHar.log.entries[1])}`;
    expect(() => parseHar(text)).toThrowError(/line 2/);
  });

  it("rejects NDJSON values that are not HAR entries", () => {
    const text = `${JSON.stringify(sampleHar.log.entries[0])}\n{"hello":"world"}`;
    expect(() => parseHar(text)).toThrowError(/line 2 is not a HAR entry/);
  });

  it("rejects JSON that is not a HAR document", () => {
    expect(() => parseHar("[]")).toThrowError(/top level/);
    expect(() => parseHar("{}")).toThrowError(/"log"/);
    expect(() => parseHar('{"log":{}}')).toThrowError(/entries/);
  });

  it("survives missing optional fields and bad dates", () => {
    const minimal = JSON.stringify({
      log: {
        version: "1.2",
        creator: { name: "x", version: "1" },
        entries: [
          {
            startedDateTime: "not-a-date",
            time: Number.NaN,
            request: { method: "GET", url: "::::", headers: [], cookies: [], queryString: [] },
            response: { status: "weird" },
          },
        ],
      },
    });
    const { entries } = parseHar(minimal);
    expect(entries).toHaveLength(1);
    expect(entries[0].startMs).toBeNull();
    expect(entries[0].timeMs).toBe(0);
    expect(entries[0].status).toBe(0);
  });

  it("marks failure/truncation/closed-early flags", () => {
    const { entries } = parseHar(sampleText);
    const dnsFail = entries.find((e) => e.errorPhase === "dns");
    expect(dnsFail?.failed).toBe(true);
    const truncated = entries.find((e) => e.truncated);
    expect(truncated?.host).toBe("cdn.example.com");
    const closed = entries.find((e) => e.closedEarly);
    expect(closed?.status).toBe(301);
  });

  it("preserves unknown _ extension fields", () => {
    const doc = JSON.parse(sampleText);
    doc.log.entries[0]._futureExtension = { hello: "world" };
    const { entries } = parseHar(JSON.stringify(doc));
    const ext = extensionFields(entries[0].e);
    expect(ext["_futureExtension"]).toEqual({ hello: "world" });
    expect((ext["_recorder"] as { traceId?: string }).traceId).toBe("6f1c9b2a77aa41d0");
  });

  it("preserves all capture-level evidence outside the separately inspected entries", () => {
    const metadata = captureMetadata(maximalEvidenceHar);
    const text = JSON.stringify(metadata);
    expect(text).toContain("creator-comment-evidence");
    expect(text).toContain("browser-comment-evidence");
    expect(text).toContain("page-timing-comment-evidence");
    expect(text).toContain("top-level-extension-evidence");
    expect(text).toContain("log-extension-evidence");
    expect(text).not.toContain("entry-comment-evidence");
  });

  it("rejects unsupported recorder extension versions", () => {
    const doc = JSON.parse(sampleText);
    doc.log.entries[0]._recorder.schemaVersion = "2";
    expect(() => parseHar(JSON.stringify(doc))).toThrow(/unsupported recorder extension schema 2/);
  });
});

describe("groupByTrace", () => {
  it("groups redirect chains ordered by redirectIndex", () => {
    const { entries } = parseHar(sampleText);
    const groups = groupByTrace(entries);
    const chain = groups.find((g) => g.traceId === "chain-42");
    expect(chain).toBeDefined();
    expect(chain?.hops).toBe(2);
    expect(chain?.entries.map((e) => e.status)).toEqual([301, 200]);
    expect(chain?.finalStatus).toBe(200);
    expect(chain?.totalMs).toBeCloseTo(12.1 + 48.9);
    expect(chain?.hasFailed).toBe(false);
  });

  it("marks groups containing failures", () => {
    const { entries } = parseHar(sampleText);
    const groups = groupByTrace(entries);
    const failed = groups.find((g) => g.traceId === "f00dfeedcafe0001");
    expect(failed?.hasFailed).toBe(true);
    expect(failed?.finalStatus).toBe(0);
  });

  it("keeps entries without traceId as singleton groups", () => {
    const { entries } = parseHar(
      JSON.stringify({
        log: {
          version: "1.2",
          creator: { name: "x", version: "1" },
          entries: [
            { startedDateTime: "2026-01-01T00:00:00Z", time: 1, request: { method: "GET", url: "http://a/" }, response: { status: 200 } },
          ],
        },
      }),
    );
    const groups = groupByTrace(entries);
    expect(groups).toHaveLength(1);
    expect(groups[0].traceId).toBeNull();
    expect(groups[0].hops).toBe(1);
  });
});
