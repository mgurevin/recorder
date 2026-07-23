import { describe, expect, it, vi } from "vitest";
import { fetchRemoteHar, remoteHarURL } from "./remoteHar";

describe("remoteHarURL", () => {
  it("converts a normal Gist share URL to its raw endpoint", () => {
    expect(remoteHarURL("https://gist.github.com/alice/abc123#file-demo-har").href)
      .toBe("https://gist.githubusercontent.com/alice/abc123/raw");
  });

  it("keeps direct HTTPS URLs and removes fragments", () => {
    expect(remoteHarURL("https://example.com/test.har#x").href).toBe("https://example.com/test.har");
  });

  it("rejects insecure and credential-bearing URLs", () => {
    expect(() => remoteHarURL("http://example.com/test.har")).toThrow(/HTTPS/);
    expect(() => remoteHarURL("https://user:pass@example.com/test.har")).toThrow(/credentials/);
  });

  it("allows HTTP only for loopback CLI links", () => {
    expect(remoteHarURL("http://127.0.0.1:43001/capture").href)
      .toBe("http://127.0.0.1:43001/capture");
    expect(remoteHarURL("http://localhost:43001/capture").href)
      .toBe("http://localhost:43001/capture");
    expect(remoteHarURL("http://[::1]:43001/capture").href)
      .toBe("http://[::1]:43001/capture");
    expect(() => remoteHarURL("http://127.0.0.2:43001/capture")).toThrow(/HTTPS/);
  });
});

describe("fetchRemoteHar", () => {
  it("downloads without credentials or referrer", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("{\"log\":{}}", {
      status: 200,
      headers: { "content-length": "10" },
    }));
    const result = await fetchRemoteHar("https://example.com/demo.har");
    expect(result.text).toBe('{"log":{}}');
    expect(fetchMock).toHaveBeenCalledWith(expect.any(URL), expect.objectContaining({
      credentials: "omit",
      referrerPolicy: "no-referrer",
    }));
    fetchMock.mockRestore();
  });
});
