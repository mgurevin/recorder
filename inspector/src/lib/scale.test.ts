import { describe, expect, it } from "vitest";
import { applyFilters, emptyFilters, sortEntries } from "../components/Filters";
import { groupByTrace, parseHar } from "./parse";

const entryCount = 10_000;

function largeCapture() {
  return JSON.stringify({
    log: {
      version: "1.2",
      creator: { name: "scale-test", version: "1" },
      entries: Array.from({ length: entryCount }, (_, index) => ({
        startedDateTime: new Date(Date.UTC(2026, 0, 1, 0, 0, 0, index)).toISOString(),
        time: index % 1_000,
        request: {
          method: index % 7 === 0 ? "POST" : "GET",
          url: `https://api.example.com/resources/${index}?page=${index % 100}`,
          httpVersion: "HTTP/2.0",
          headers: [],
          cookies: [],
          queryString: [],
          headersSize: 120,
          bodySize: 0,
        },
        response: {
          status: index % 20 === 0 ? 503 : 200,
          statusText: index % 20 === 0 ? "Service Unavailable" : "OK",
          httpVersion: "HTTP/2.0",
          headers: [],
          cookies: [],
          content: { size: index, mimeType: "application/json" },
          redirectURL: "",
          headersSize: 96,
          bodySize: index,
        },
        cache: {},
        timings: { blocked: 0, dns: -1, connect: -1, ssl: -1, send: 0.1, wait: 1, receive: 0.2 },
        comment: index % 997 === 0 ? "priority investigation" : undefined,
        _recorder: {
          schemaVersion: "1",
          traceId: `trace-${Math.floor(index / 5)}`,
          redirectIndex: index % 5,
          state: "completed",
        },
      })),
    },
  });
}

describe("large capture data paths", () => {
  it("parses, groups, filters, and sorts ten thousand exchanges", () => {
    const { entries } = parseHar(largeCapture());
    expect(entries).toHaveLength(entryCount);

    const groups = groupByTrace(entries);
    expect(groups).toHaveLength(entryCount / 5);
    expect(groups.every((group) => group.hops === 5)).toBe(true);

    const failures = applyFilters(entries, { ...emptyFilters, method: "POST", statusClass: "5xx" });
    expect(failures.length).toBeGreaterThan(0);
    expect(failures.every((entry) => entry.method === "POST" && entry.status === 503)).toBe(true);

    const comments = applyFilters(entries, { ...emptyFilters, search: "priority investigation" });
    expect(comments).toHaveLength(Math.ceil(entryCount / 997));

    const sorted = sortEntries(entries, "size", true);
    expect(sorted[0].respSize).toBe(entryCount - 1);
    expect(sorted.at(-1)?.respSize).toBe(0);
  });
});
