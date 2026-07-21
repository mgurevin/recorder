import { describe, expect, it } from "vitest";
import { parseHar } from "../lib/parse";
import { sampleHar } from "../sampleHar";
import { applyFilters, emptyFilters } from "./Filters";

describe("applyFilters", () => {
  it("matches standard HAR entry comments", () => {
    const { entries } = parseHar(JSON.stringify(sampleHar));
    const matches = applyFilters(entries, { ...emptyFilters, search: "reconciliation workflow" });

    expect(matches).toHaveLength(1);
    expect(matches[0].e.comment).toContain("reconciliation workflow");
  });
});
