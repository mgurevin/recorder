import { describe, expect, it } from "vitest";
import { parseHar } from "./parse";
import { exportCapture, exportFilename, exportReadiness } from "./exportCapture";
import { sampleHar } from "../sampleHar";

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
  });
});
