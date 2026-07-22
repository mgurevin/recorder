import { describe, expect, it } from "vitest";
import { fileBodyAssetPath } from "./bodyStore";

describe("fileBodyAssetPath", () => {
  it("maps a FileBodyStore reference to its root-relative asset path", () => {
    expect(fileBodyAssetPath("filebody:v1:0b9bb1c29eddedef33a41b4ce8d68228"))
      .toBe("assets/0b9bb1c29eddedef33a41b4ce8d68228.body");
  });

  it.each([
    undefined,
    "custom-store:asset",
    "filebody:v2:0b9bb1c29eddedef33a41b4ce8d68228",
    "filebody:v1:../secret",
    "filebody:v1:short",
  ])("does not invent a path for an unrecognized reference: %s", (reference) => {
    expect(fileBodyAssetPath(reference)).toBeUndefined();
  });
});
