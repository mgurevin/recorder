import type { HarEntry, NameValue } from "../types/har";

export interface CurlReplay {
  command: string;
  warnings: string[];
}

export interface CurlReplayOptions {
  includeLocalInterface?: boolean;
}

const GENERATED_HEADERS = new Set(["content-length", "transfer-encoding", "connection", "proxy-connection"]);

/** shellQuote produces one POSIX-shell-safe argument, including newlines. */
export function shellQuote(value: string): string {
  return `'${value.replaceAll("'", `'\\''`)}'`;
}

export function curlReplay(entry: HarEntry, options: CurlReplayOptions = {}): CurlReplay {
  const request = entry.request;
  if (!request) return { command: "", warnings: ["No request was recorded for this entry."] };

  const warnings: string[] = [];
  const args = ["curl", `  --request ${shellQuote(request.method || "GET")}`, `  --url ${shellQuote(request.url)}`];
  const proxy = entry._network?.proxy;
  if (proxy) args.splice(2, 0, `  --proxy ${shellQuote(proxy)}`);
  if (options.includeLocalInterface) {
    const localInterface = interfaceAddress(entry._network?.localAddress);
    if (localInterface) args.splice(proxy ? 3 : 2, 0, `  --interface ${shellQuote(localInterface)}`);
    else warnings.push("The local interface was requested but no usable local address was recorded.");
  }
  const headers = (request.headers ?? []).filter(replayableHeader);
  for (const header of headers) args.push(`  --header ${shellQuote(`${header.name}: ${header.value}`)}`);

  const postData = request.postData;
  if (postData?.text != null) {
    if (postData._encoding === "base64") {
      warnings.push("The request body is binary/base64 and was omitted from the command; save and attach it manually.");
    } else {
      args.push(`  --data-binary ${shellQuote(postData.text)}`);
    }
  } else if ((entry._requestBody?.totalBytes ?? request.bodySize ?? 0) > 0) {
    warnings.push("The request body was not embedded in the HAR and cannot be included in the command.");
  }

  if (entry._requestBody?.truncated) warnings.push("The recorded request body is truncated.");
  if (entry._requestBody && !entry._requestBody.complete) warnings.push("The recorded request body is incomplete.");
  if (
    containsRedaction(request.url)
    || containsRedaction(proxy)
    || headers.some((h) => containsRedaction(h.value))
    || containsRedaction(postData?.text)
  ) {
    warnings.push("The command contains [REDACTED] placeholders; replace them with authorized values before use.");
  }
  warnings.push("Review the command before sharing it: URLs, headers, cookies, and bodies may contain sensitive data.");
  warnings.push("This command is reconstructed from recorded data and may not exactly reproduce transport behavior.");
  return { command: args.join(" \\\n"), warnings };
}

/** Strip the ephemeral port from Go net.Addr strings, including bracketed IPv6. */
export function interfaceAddress(address: string | undefined): string | undefined {
  if (!address) return undefined;
  if (address.startsWith("[")) {
    const end = address.indexOf("]");
    return end > 1 ? address.slice(1, end) : undefined;
  }
  if (address.indexOf(":") !== address.lastIndexOf(":")) return address;
  const colon = address.lastIndexOf(":");
  if (colon > 0 && /^\d+$/.test(address.slice(colon + 1))) return address.slice(0, colon);
  return address;
}

function replayableHeader(header: NameValue): boolean {
  const name = header.name.toLowerCase();
  return !name.startsWith(":") && !GENERATED_HEADERS.has(name);
}

function containsRedaction(value: string | undefined): boolean {
  return value?.includes("[REDACTED]") ?? false;
}
