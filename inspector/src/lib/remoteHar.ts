const MAX_REMOTE_HAR_BYTES = 100 * 1024 * 1024;

export interface RemoteHar {
  name: string;
  text: string;
  url: string;
}

/** Resolve a direct HAR URL, including normal gist.github.com share links. */
export function remoteHarURL(value: string): URL {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    throw new Error("The har parameter is not a valid URL.");
  }
  if (url.protocol !== "https:") throw new Error("Remote HAR links must use HTTPS.");
  if (url.username || url.password) throw new Error("Remote HAR links must not contain URL credentials.");

  if (url.hostname === "gist.github.com") {
    const parts = url.pathname.split("/").filter(Boolean);
    if (parts.length < 2) throw new Error("The GitHub Gist URL must include an owner and gist ID.");
    url = new URL(`https://gist.githubusercontent.com/${parts[0]}/${parts[1]}/raw`);
  }
  url.hash = "";
  return url;
}

export async function fetchRemoteHar(value: string, signal?: AbortSignal): Promise<RemoteHar> {
  const url = remoteHarURL(value);
  let response: Response;
  try {
    response = await fetch(url, { signal, credentials: "omit", referrerPolicy: "no-referrer" });
  } catch (error) {
    if (error instanceof DOMException && error.name === "AbortError") throw error;
    throw new Error("The remote HAR could not be downloaded. The host may not allow browser CORS requests.", { cause: error });
  }
  if (!response.ok) throw new Error(`The remote HAR returned HTTP ${response.status}.`);

  const declaredSize = Number(response.headers.get("content-length"));
  if (Number.isFinite(declaredSize) && declaredSize > MAX_REMOTE_HAR_BYTES) {
    throw new Error("The remote HAR is larger than the 100 MiB limit.");
  }
  const text = await readLimitedText(response);

  const pathName = new URL(response.url || url).pathname.split("/").filter(Boolean).at(-1);
  return { name: pathName && pathName !== "raw" ? pathName : `remote HAR (${url.hostname})`, text, url: url.href };
}

async function readLimitedText(response: Response): Promise<string> {
  if (!response.body) return "";
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let bytes = 0;
  let text = "";
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    bytes += value.byteLength;
    if (bytes > MAX_REMOTE_HAR_BYTES) {
      await reader.cancel();
      throw new Error("The remote HAR is larger than the 100 MiB limit.");
    }
    text += decoder.decode(value, { stream: true });
  }
  return text + decoder.decode();
}
