import { describe, expect, it } from "vitest";
import { clampSidebarWidth, sidebarDefaultWidth, sidebarMaxWidth, sidebarMinWidth } from "./layout";

describe("clampSidebarWidth", () => {
  it("keeps valid widths and rounds subpixels", () => {
    expect(clampSidebarWidth(481.6)).toBe(482);
  });

  it("bounds invalid and extreme widths", () => {
    expect(clampSidebarWidth(Number.NaN)).toBe(sidebarDefaultWidth);
    expect(clampSidebarWidth(120)).toBe(sidebarMinWidth);
    expect(clampSidebarWidth(900)).toBe(sidebarMaxWidth);
  });
});
