import { describe, expect, it } from "vitest";
import { decodeKey, decryptProtectedToken, parseProtectedToken, protectedOccurrences, verifyProtectedToken } from "./protection";
import type { HarEntry } from "../types/har";

describe("protected token parsing", () => {
  it("parses versioned encrypted and tokenized values", () => {
    expect(parseProtectedToken("REC-ENC-v1.a2lk.AAECAw")).toMatchObject({ mode: "encrypt", keyId: "kid" });
    expect(parseProtectedToken("REC-TOK-v1.a2lk.AAECAw")).toMatchObject({ mode: "tokenize", keyId: "kid" });
  });

  it("rejects malformed tokens and decodes supported key encodings", () => {
    expect(() => parseProtectedToken("REC-ENC-v2.a2lk.AA")).toThrow();
    expect([...decodeKey("0011ff")]).toEqual([0, 17, 255]);
    expect([...decodeKey("ABH_" )]).toEqual([0, 17, 255]);
  });

  it("finds request and response occurrences without scanning extensions", () => {
    const entry = {
      request: { url: "https://e.test/?x=REC-ENC-v1.a2lk.AA", headers: [], queryString: [], cookies: [] },
      response: { headers: [{ name: "x", value: "REC-TOK-v1.a2lk.AQ" }], cookies: [] },
    } as unknown as HarEntry;
    const found = protectedOccurrences(entry);
    expect(found.map((item) => [item.mode, item.request])).toEqual([["encrypt", true], ["tokenize", false]]);
  });

  it("decrypts the Go AES-GCM test vector", async () => {
    const token = parseProtectedToken("REC-ENC-v1.ZW5jLXRlc3Q.AAAAAAAAAAAAAAAAt699FYTbc90VTqYwASFV4Vz4ucN_7A");
    await expect(decryptProtectedToken(token, "11".repeat(32))).resolves.toBe("secret");
    await expect(decryptProtectedToken(token, "22".repeat(32))).rejects.toBeTruthy();
  });

  it("verifies the Go HMAC test vector", async () => {
    const token = parseProtectedToken("REC-TOK-v1.dG9rLXRlc3Q.NSnKjUFGTW2fAPl9GG2ZSFBsVbl0_MKjPMl6jfHuL8I");
    await expect(verifyProtectedToken(token, "secret", "22".repeat(32))).resolves.toBe(true);
    await expect(verifyProtectedToken(token, "wrong", "22".repeat(32))).resolves.toBe(false);
  });
});
