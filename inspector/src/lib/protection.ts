import type { HarEntry } from "../types/har";

export type ProtectedTokenMode = "encrypt" | "tokenize";

export interface ProtectedToken {
  token: string;
  mode: ProtectedTokenMode;
  keyId: string;
  payload: Uint8Array;
}

export interface ProtectedOccurrence extends ProtectedToken {
  path: string;
  request: boolean;
}

export interface BatchDecryptProgress {
  completed: number;
  total: number;
  failures: number;
}

export interface BatchDecryptResult {
  values: Map<string, string>;
  failures: number;
}

const TOKEN_RE = /REC-(ENC|TOK)-v1\.([A-Za-z0-9_-]+)\.([A-Za-z0-9_-]+)/g;
const MAX_TOKEN_CHARS = 24 << 20;

export function parseProtectedToken(token: string): ProtectedToken {
  if (token.length > MAX_TOKEN_CHARS) throw new Error("Protected token exceeds the Inspector safety limit.");
  const match = /^(REC-(ENC|TOK)-v1)\.([A-Za-z0-9_-]+)\.([A-Za-z0-9_-]+)$/.exec(token);
  if (!match) throw new Error("Unsupported or malformed protected token.");
  const keyId = new TextDecoder("utf-8", { fatal: true }).decode(base64UrlBytes(match[3]));
  if (!keyId) throw new Error("Protected token has an empty key ID.");
  return {
    token,
    mode: match[2] === "ENC" ? "encrypt" : "tokenize",
    keyId,
    payload: base64UrlBytes(match[4]),
  };
}

export function protectedOccurrences(entry: HarEntry): ProtectedOccurrence[] {
  const occurrences: ProtectedOccurrence[] = [];
  const seen = new Set<string>();
  walk(entry.request, "request", true, occurrences, seen);
  walk(entry.response, "response", false, occurrences, seen);
  return occurrences;
}

/** Return an in-memory view with decrypted tokens substituted in every string.
 * The parsed HAR and its nested objects are never mutated. */
export function withDecryptedValues<T>(value: T, decryptedValues: ReadonlyMap<string, string>): T {
  if (decryptedValues.size === 0) return value;
  return replaceDecrypted(value, decryptedValues) as T;
}

function replaceDecrypted(value: unknown, decryptedValues: ReadonlyMap<string, string>): unknown {
  if (typeof value === "string") {
    return value.replace(TOKEN_RE, (token) => decryptedValues.get(token) ?? token);
  }
  if (Array.isArray(value)) return value.map((item) => replaceDecrypted(item, decryptedValues));
  if (value && typeof value === "object") {
    return Object.fromEntries(Object.entries(value).map(([key, child]) => [key, replaceDecrypted(child, decryptedValues)]));
  }
  return value;
}

function walk(
  value: unknown,
  path: string,
  request: boolean,
  out: ProtectedOccurrence[],
  seen: Set<string>,
): void {
  if (typeof value === "string") {
    for (const match of value.matchAll(TOKEN_RE)) {
      const token = match[0];
      const identity = `${path}\0${token}`;
      if (seen.has(identity)) continue;
      seen.add(identity);
      try {
        out.push({ ...parseProtectedToken(token), path, request });
      } catch {
        // Rendering untrusted HAR input must remain best-effort.
      }
    }
    return;
  }
  if (Array.isArray(value)) {
    value.forEach((item, index) => walk(item, `${path}[${index}]`, request, out, seen));
    return;
  }
  if (value && typeof value === "object") {
    for (const [key, child] of Object.entries(value as Record<string, unknown>)) {
      walk(child, `${path}.${key}`, request, out, seen);
    }
  }
}

export async function decryptProtectedToken(token: ProtectedToken, keyText: string): Promise<string> {
  if (token.mode !== "encrypt") throw new Error("This token is not encrypted.");
  const keyBytes = decodeKey(keyText);
  if (keyBytes.length !== 32) throw new Error("AES-256-GCM keys must contain exactly 32 bytes.");
  if (token.payload.length < 12 + 16) throw new Error("Encrypted token payload is too short.");
  const key = await crypto.subtle.importKey("raw", arrayBuffer(keyBytes), { name: "AES-GCM" }, false, ["decrypt"]);
  const plain = await crypto.subtle.decrypt({
    name: "AES-GCM",
    iv: arrayBuffer(token.payload.slice(0, 12)),
    additionalData: arrayBuffer(new TextEncoder().encode(token.keyId)),
    tagLength: 128,
  }, key, arrayBuffer(token.payload.slice(12)));
  return new TextDecoder("utf-8", { fatal: true }).decode(plain);
}

/** Decrypt unique tokens in bounded batches. The first value validates the
 * key, avoiding thousands of identical failures when the key is wrong. */
export async function decryptProtectedTokens(
  tokens: readonly ProtectedToken[],
  keyText: string,
  onProgress?: (progress: BatchDecryptProgress) => void,
): Promise<BatchDecryptResult> {
  const unique = [...new Map(tokens.filter((token) => token.mode === "encrypt").map((token) => [token.token, token])).values()];
  const values = new Map<string, string>();
  if (unique.length === 0) return { values, failures: 0 };

  let first: string;
  try {
    first = await decryptProtectedToken(unique[0], keyText);
  } catch {
    throw new Error("The first value could not be decrypted; the key may be wrong or the token may be damaged.");
  }
  values.set(unique[0].token, first);
  let completed = 1;
  let failures = 0;
  onProgress?.({ completed, total: unique.length, failures });

  const batchSize = 64;
  for (let offset = 1; offset < unique.length; offset += batchSize) {
    const batch = unique.slice(offset, offset + batchSize);
    const results = await Promise.allSettled(batch.map((token) => decryptProtectedToken(token, keyText)));
    results.forEach((result, index) => {
      if (result.status === "fulfilled") values.set(batch[index].token, result.value);
      else failures += 1;
    });
    completed += batch.length;
    onProgress?.({ completed, total: unique.length, failures });
    await new Promise<void>((resolve) => setTimeout(resolve, 0));
  }
  return { values, failures };
}

export async function verifyProtectedToken(
  token: ProtectedToken,
  candidate: string,
  keyText: string,
): Promise<boolean> {
  if (token.mode !== "tokenize") throw new Error("This token is not tokenized.");
  const keyBytes = decodeKey(keyText);
  if (keyBytes.length < 32) throw new Error("HMAC keys must contain at least 32 bytes.");
  const key = await crypto.subtle.importKey("raw", arrayBuffer(keyBytes), { name: "HMAC", hash: "SHA-256" }, false, ["verify"]);
  return crypto.subtle.verify("HMAC", key, arrayBuffer(token.payload), arrayBuffer(new TextEncoder().encode(candidate)));
}

/** Accept hex, standard base64, or unpadded base64url without persisting it. */
export function decodeKey(text: string): Uint8Array {
  const compact = text.trim();
  if (/^(?:[0-9a-fA-F]{2})+$/.test(compact)) {
    return Uint8Array.from(compact.match(/.{2}/g) ?? [], (part) => Number.parseInt(part, 16));
  }
  return base64UrlBytes(compact.replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, ""));
}

function base64UrlBytes(value: string): Uint8Array {
  if (!/^[A-Za-z0-9_-]*$/.test(value)) throw new Error("Invalid base64url data.");
  const padded = value.replaceAll("-", "+").replaceAll("_", "/") + "=".repeat((4 - value.length % 4) % 4);
  let binary: string;
  try {
    binary = atob(padded);
  } catch {
    throw new Error("Invalid base64url data.");
  }
  return Uint8Array.from(binary, (char) => char.charCodeAt(0));
}

function arrayBuffer(value: Uint8Array): ArrayBuffer {
  return value.slice().buffer as ArrayBuffer;
}
