import type { HarEntry, NameValue } from "../types/har";
import { prettyBody } from "./format";

export interface CurlReplay {
  command: string;
  warnings: string[];
}

export interface CurlReplayOptions {
  includeLocalInterface?: boolean;
  decryptedValues?: ReadonlyMap<string, string>;
  bodyFormat?: "raw" | "formatted";
}

const GENERATED_HEADERS = new Set(["content-length", "transfer-encoding", "connection", "proxy-connection"]);
const REDACTED_PLACEHOLDER = "[REDACTED]";

/** shellQuote produces one POSIX-shell-safe argument, including newlines. */
export function shellQuote(value: string): string {
  return `'${value.replaceAll("'", `'\\''`)}'`;
}

export function curlReplay(entry: HarEntry, options: CurlReplayOptions = {}): CurlReplay {
  const request = entry.request;
  if (!request) return { command: "", warnings: ["No request was recorded for this entry."] };

  const warnings: string[] = [];
  const overrides = options.decryptedValues;
  const replayURL = replayURLWithOverrides(request.url, overrides);
  const args = ["curl", `  --request ${shellQuote(request.method || "GET")}`, `  --url ${shellQuote(replayURL)}`];
  const proxySource = entry._recorder?.network?.proxy;
  const proxy = proxySource ? replayURLWithOverrides(proxySource, overrides) : undefined;
  if (proxy) args.splice(2, 0, `  --proxy ${shellQuote(proxy)}`);
  if (options.includeLocalInterface) {
    const localInterface = interfaceAddress(entry._recorder?.network?.localAddress);
    if (localInterface) args.splice(proxy ? 3 : 2, 0, `  --interface ${shellQuote(localInterface)}`);
    else warnings.push("The local interface was requested but no usable local address was recorded.");
  }
  const headers = (request.headers ?? []).filter(replayableHeader);
  for (const header of headers) {
    args.push(`  --header ${shellQuote(`${header.name}: ${replaceProtectedTokens(header.value, overrides)}`)}`);
  }

  const postData = request.postData;
  if (postData?.text != null) {
    if (entry._recorder?.requestBodyEncoding === "base64") {
      warnings.push("The request body is binary/base64 and was omitted from the command; save and attach it manually.");
    } else {
      const replayBody = replayBodyWithOverrides(postData.text, postData.mimeType, overrides);
      const body = options.bodyFormat === "formatted" ? formatReplayBody(replayBody, postData.mimeType) : replayBody;
      args.push(`  --data-binary ${shellQuote(body)}`);
      if (options.bodyFormat === "formatted" && body !== replayBody) {
        warnings.push("The request body was formatted for replay; its insignificant whitespace differs from the recorded body.");
      }
    }
  } else if ((entry._recorder?.requestBody?.totalBytes ?? request.bodySize ?? 0) > 0) {
    warnings.push("The request body was not embedded in the HAR and cannot be included in the command.");
  }

  if (entry._recorder?.requestBody?.truncated) warnings.push("The recorded request body is truncated.");
  if (entry._recorder?.requestBody && !entry._recorder?.requestBody.complete) warnings.push("The recorded request body is incomplete.");
  if (
    containsRedaction(request.url)
    || containsRedaction(proxy)
    || headers.some((h) => containsRedaction(h.value))
    || containsRedaction(postData?.text)
  ) {
    warnings.push(`The command contains ${REDACTED_PLACEHOLDER} placeholders; replace them with authorized values before use.`);
  }
  const replaySource = { request, proxy: proxySource };
  const encryptedCount = protectedTokenCount(replaySource, "REC-ENC-v1.");
  const tokenizedCount = protectedTokenCount(replaySource, "REC-TOK-v1.");
  const appliedCount = protectedOverrideCount(replaySource, overrides);
  if (encryptedCount > 0 && appliedCount === 0) {
    warnings.push("Encrypted request values remain protected; decrypt and explicitly enable them before replay.");
  } else if (encryptedCount > appliedCount) {
    warnings.push(`Only ${appliedCount} of ${encryptedCount} encrypted request values are available to replay; the command is partial.`);
  } else if (appliedCount > 0) {
    warnings.push(`${appliedCount} decrypted request value${appliedCount === 1 ? " was" : "s were"} inserted into this command in memory.`);
  }
  if (tokenizedCount > 0) warnings.push("Tokenized request values are irreversible and remain tokenized in the command.");
  warnings.push("Review the command before sharing it: URLs, headers, cookies, and bodies may contain sensitive data.");
  warnings.push("This command is reconstructed from recorded data and may not exactly reproduce transport behavior.");
  return { command: args.join(" \\\n"), warnings };
}

export function supportsReplayBodyFormatting(mimeType: string | undefined): boolean {
  const base = mimeType?.split(";", 1)[0].trim().toLowerCase() ?? "";
  return base === "application/json"
    || base.endsWith("+json")
    || base === "application/xml"
    || base === "text/xml"
    || base.endsWith("+xml");
}

function formatReplayBody(text: string, mimeType: string | undefined): string {
  const body = prettyBody(mimeType, text, undefined, true);
  return body.copyText ?? body.text ?? text;
}

function protectedOverrideCount(value: unknown, overrides: ReadonlyMap<string, string> | undefined): number {
  if (!overrides?.size) return 0;
  let count = 0;
  if (typeof value === "string") {
    for (const token of overrides.keys()) count += value.split(token).length - 1;
    return count;
  }
  if (Array.isArray(value)) {
    for (const item of value) count += protectedOverrideCount(item, overrides);
  } else if (value && typeof value === "object") {
    for (const child of Object.values(value as Record<string, unknown>)) count += protectedOverrideCount(child, overrides);
  }
  return count;
}

function replayURLWithOverrides(value: string, overrides: ReadonlyMap<string, string> | undefined): string {
  if (!overrides?.size) return value;
  try {
    const url = new URL(value);
    const params = new URLSearchParams();
    for (const [name, current] of url.searchParams.entries()) {
      params.append(name, replaceProtectedTokens(current, overrides));
    }
    url.search = params.toString();
    url.username = replaceProtectedTokens(decodeURIComponent(url.username), overrides);
    url.password = replaceProtectedTokens(decodeURIComponent(url.password), overrides);
    return url.toString();
  } catch {
    return replaceProtectedTokens(value, overrides);
  }
}

function replayBodyWithOverrides(
  text: string,
  mimeType: string | undefined,
  overrides: ReadonlyMap<string, string> | undefined,
): string {
  if (!overrides?.size) return text;
  const base = mimeType?.split(";", 1)[0].trim().toLowerCase() ?? "";
  if (base === "application/json" || base.endsWith("+json") || base === "application/x-ndjson") {
    let out = text;
    for (const [token, plain] of overrides) out = out.replaceAll(JSON.stringify(token), plain);
    return out;
  }
  return replaceProtectedTokens(text, overrides);
}

function replaceProtectedTokens(value: string, overrides: ReadonlyMap<string, string> | undefined): string {
  if (!overrides?.size) return value;
  let out = value;
  for (const [token, plain] of overrides) out = out.replaceAll(token, plain);
  return out;
}

function protectedTokenCount(value: unknown, prefix: string): number {
  let count = 0;
  if (typeof value === "string") return value.split(prefix).length - 1;
  if (Array.isArray(value)) {
    for (const item of value) count += protectedTokenCount(item, prefix);
  } else if (value && typeof value === "object") {
    for (const child of Object.values(value as Record<string, unknown>)) count += protectedTokenCount(child, prefix);
  }
  return count;
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
  return value?.includes(REDACTED_PLACEHOLDER) ?? false;
}
