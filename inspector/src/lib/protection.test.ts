import { describe, expect, it } from "vitest";
import { decodeKey, decryptLiveEntry, decryptProtectedToken, decryptProtectedTokens, parseProtectedToken, protectedOccurrences, verifyProtectedToken, verifyProtectedTokens, withResolvedValues } from "./protection";
import type { HarEntry } from "../types/har";

describe("protected token parsing", () => {
  it("parses versioned encrypted and tokenized values", () => {
    expect(parseProtectedToken("REC-ENC-v1.a2lk.AAECAw")).toMatchObject({ mode: "encrypt", keyId: "kid" });
    expect(parseProtectedToken("REC-TOK-v1.a2lk.AAECAw")).toMatchObject({ mode: "tokenize", keyId: "kid" });
  });

  it("rejects malformed tokens and decodes supported key encodings", () => {
    expect(() => parseProtectedToken("REC-ENC-v2.a2lk.AA")).toThrow(/Unsupported or malformed protected token/);
    expect([...decodeKey("0011ff")]).toEqual([0, 17, 255]);
    expect([...decodeKey("ABH_" )]).toEqual([0, 17, 255]);
  });

  it("finds request, response, and recorder-extension occurrences", () => {
    const entry = {
      request: { url: "https://e.test/?x=REC-ENC-v1.a2lk.AA", headers: [], queryString: [], cookies: [] },
      response: { headers: [{ name: "x", value: "REC-TOK-v1.a2lk.AQ" }], cookies: [] },
      _recorder: {
        schemaVersion: "1",
        network: { proxy: "http://user:REC-ENC-v1.a2lk.Ag@proxy.test:8080" },
        trace: { events: [{ detail: "REC-ENC-v1.a2lk.Aw" }] },
      },
    } as unknown as HarEntry;
    const found = protectedOccurrences(entry);
    expect(found.map((item) => [item.path, item.mode, item.request])).toEqual([
      ["request.url", "encrypt", true],
      ["response.headers[0].value", "tokenize", false],
      ["_recorder.network.proxy", "encrypt", true],
      ["_recorder.trace.events[0].detail", "encrypt", false],
    ]);
  });

  it("decrypts the Go AES-GCM test vector", async () => {
    const token = parseProtectedToken("REC-ENC-v1.ZW5jLXRlc3Q.AAAAAAAAAAAAAAAAt699FYTbc90VTqYwASFV4Vz4ucN_7A");
    await expect(decryptProtectedToken(token, "11".repeat(32))).resolves.toBe("secret");
    await expect(decryptProtectedToken(token, "22".repeat(32))).rejects.toBeTruthy();
  });

  it("decrypts token groups once per unique token and rejects a wrong key early", async () => {
    const token = parseProtectedToken("REC-ENC-v1.ZW5jLXRlc3Q.AAAAAAAAAAAAAAAAt699FYTbc90VTqYwASFV4Vz4ucN_7A");
    const progress: number[] = [];
    const result = await decryptProtectedTokens([token, token], "11".repeat(32), (state) => progress.push(state.completed));
    expect(result.values.get(token.token)).toBe("secret");
    expect(result.values.size).toBe(1);
    expect(result.failures).toBe(0);
    expect(progress).toEqual([1]);
    await expect(decryptProtectedTokens([token, token], "22".repeat(32))).rejects.toThrow("first value");
  });

  it("automatically decrypts new live values only with activated keys", async () => {
    const token = "REC-ENC-v1.ZW5jLXRlc3Q.AAAAAAAAAAAAAAAAt699FYTbc90VTqYwASFV4Vz4ucN_7A";
    const damaged = "REC-ENC-v1.ZW5jLXRlc3Q.AA";
    const entry = {
      request: { url: "https://example.test/", headers: [], queryString: [], cookies: [] },
      response: { headers: [{ name: "X-Secret", value: token }, { name: "X-Damaged", value: damaged }], cookies: [] },
    } as unknown as HarEntry;

    await expect(decryptLiveEntry(entry, new Map())).resolves.toEqual({ values: new Map(), failures: 0 });

    const result = await decryptLiveEntry(entry, new Map([["encrypt:enc-test", "11".repeat(32)]]));
    expect(result.values).toEqual(new Map([[token, "secret"]]));
    expect(result.failures).toBe(1);
  });

  it("creates a resolved display view without mutating encrypted or tokenized HAR values", () => {
    const token = "REC-ENC-v1.ZW5jLXRlc3Q.AAAAAAAAAAAAAAAAt699FYTbc90VTqYwASFV4Vz4ucN_7A";
    const tokenized = "REC-TOK-v1.dG9rLXRlc3Q.NSnKjUFGTW2fAPl9GG2ZSFBsVbl0_MKjPMl6jfHuL8I";
    const source = {
      request: {
        url: `https://example.test/?secret=${token}`,
        headers: [{ name: "Authorization", value: token }, { name: "X-Account", value: tokenized }],
      },
      response: { content: { text: `before:${token}:after` } },
      _recorder: { schemaVersion: "1", network: { proxy: `http://user:${token}@proxy.test:8080` } },
    };
    const view = withResolvedValues(source, new Map([[token, "secret"], [tokenized, "account-42"]]));
    expect(view.request.url).toBe("https://example.test/?secret=secret");
    expect(view.request.headers[0].value).toBe("secret");
    expect(view.request.headers[1].value).toBe("account-42");
    expect(view.response.content.text).toBe("before:secret:after");
    expect(view._recorder.network.proxy).toBe("http://user:secret@proxy.test:8080");
    expect(source.request.headers[0].value).toBe(token);
    expect(source.request.headers[1].value).toBe(tokenized);
    expect(view).not.toBe(source);
  });

  it("preserves JSON value types in resolved entry bodies", () => {
    const stringToken = "REC-ENC-v1.c3RyaW5n.AA";
    const numberToken = "REC-ENC-v1.bnVtYmVy.AA";
    const source = {
      request: {
        postData: {
          mimeType: "application/json",
          text: `{"secret":"${stringToken}","pin":"${numberToken}"}`,
        },
      },
      response: {
        content: {
          mimeType: "application/json",
          text: `{"secret":"${stringToken}"}`,
        },
      },
    };

    const view = withResolvedValues(source, new Map([
      [stringToken, '"plain-secret"'],
      [numberToken, "1234"],
    ]));

    expect(JSON.parse(view.request.postData.text)).toEqual({ secret: "plain-secret", pin: 1234 });
    expect(JSON.parse(view.response.content.text)).toEqual({ secret: "plain-secret" });
    expect(source.request.postData.text).toContain(stringToken);
  });

  it("does not resolve malformed or base64 JSON bodies", () => {
    const token = "REC-ENC-v1.c3RyaW5n.AA";
    const source = {
      request: {
        postData: { mimeType: "application/json", text: `{"secret":"${token}"` },
      },
      response: {
        content: { mimeType: "application/json", encoding: "base64", text: token },
      },
      _recorder: { requestBodyEncoding: "base64" },
    };

    const view = withResolvedValues(source, new Map([[token, '"plain-secret"']]));

    expect(view.request.postData.text).toBe(source.request.postData.text);
    expect(view.response.content.text).toBe(token);
  });

  it("verifies the Go HMAC test vector", async () => {
    const token = parseProtectedToken("REC-TOK-v1.dG9rLXRlc3Q.NSnKjUFGTW2fAPl9GG2ZSFBsVbl0_MKjPMl6jfHuL8I");
    await expect(verifyProtectedToken(token, "secret", "22".repeat(32))).resolves.toBe(true);
    await expect(verifyProtectedToken(token, "wrong", "22".repeat(32))).resolves.toBe(false);
  });

  it("retains verified candidates once per matching token", async () => {
    const token = parseProtectedToken("REC-TOK-v1.dG9rLXRlc3Q.NSnKjUFGTW2fAPl9GG2ZSFBsVbl0_MKjPMl6jfHuL8I");
    const progress: number[] = [];
    const matched = await verifyProtectedTokens([token, token], "secret", "22".repeat(32), (state) => progress.push(state.matches));
    expect(matched).toEqual(new Map([[token.token, "secret"]]));
    expect(progress).toEqual([1]);
    await expect(verifyProtectedTokens([token], "wrong", "22".repeat(32))).resolves.toEqual(new Map());
  });
});
