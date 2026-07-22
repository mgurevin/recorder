import { describe, expect, it } from "vitest";
import { cookieAttributeTexts } from "./cookies";

describe("cookieAttributeTexts", () => {
  it("preserves every observed Set-Cookie attribute", () => {
    const attributes = cookieAttributeTexts(
      [{ name: "session", value: "secret", path: "/", httpOnly: true }],
      [{ name: "Set-Cookie", value: "session=secret; Path=/; HttpOnly; SameSite=Lax; Priority=High; Partitioned" }],
    );
    expect(attributes).toEqual(["Path=/; HttpOnly; SameSite=Lax; Priority=High; Partitioned"]);
  });

  it("matches repeated names in header order", () => {
    const attributes = cookieAttributeTexts(
      [
        { name: "id", value: "one" },
        { name: "id", value: "two" },
      ],
      [
        { name: "set-cookie", value: "id=one; Path=/one; SameSite=Strict" },
        { name: "Set-Cookie", value: "id=two; Path=/two; Secure" },
      ],
    );
    expect(attributes).toEqual(["Path=/one; SameSite=Strict", "Path=/two; Secure"]);
  });

  it("falls back to HAR 1.2 structured attributes", () => {
    const attributes = cookieAttributeTexts(
      [{ name: "session", value: "secret", path: "/", domain: "example.test", httpOnly: true, secure: true }],
      undefined,
    );
    expect(attributes).toEqual(["path=/; domain=example.test; httpOnly; secure"]);
  });
});
