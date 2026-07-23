import { describe, expect, it } from "vitest";
import { replaceProtectedBody, replaceProtectedTokens } from "./resolvedValues";

const token = "REC-ENC-v1.a2lk.AA";

describe("resolved value substitution", () => {
  it("replaces protected tokens in ordinary strings", () => {
    expect(replaceProtectedTokens(`Bearer ${token}`, new Map([[token, "secret"]]))).toBe("Bearer secret");
  });

  it.each([
    ['"hunter2"', '{"value":"hunter2"}'],
    ["1234", '{"value":1234}'],
    ["true", '{"value":true}'],
    ["null", '{"value":null}'],
    ['{"nested":true}', '{"value":{"nested":true}}'],
    ['["a",2]', '{"value":["a",2]}'],
  ])("restores the raw JSON token %s without changing its type", (plaintext, expected) => {
    const body = `{"value":"${token}"}`;
    expect(replaceProtectedBody(body, "application/json; charset=utf-8", new Map([[token, plaintext]]))).toBe(expected);
  });

  it("resolves each document in NDJSON", () => {
    const body = `{"value":"${token}"}\n{"value":"${token}"}\n`;
    expect(replaceProtectedBody(body, "application/x-ndjson", new Map([[token, "42"]]))).toBe(
      '{"value":42}\n{"value":42}\n',
    );
  });

  it("leaves malformed JSON and invalid plaintext protected", () => {
    const malformed = `{"value":"${token}"`;
    expect(replaceProtectedBody(malformed, "application/json", new Map([[token, '"secret"']]))).toBe(malformed);

    const valid = `{"value":"${token}"}`;
    expect(replaceProtectedBody(valid, "application/json", new Map([[token, "not-json"]]))).toBe(valid);
  });
});
