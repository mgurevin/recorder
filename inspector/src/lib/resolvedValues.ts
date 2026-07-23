export function replaceProtectedTokens(
  value: string,
  resolvedValues: ReadonlyMap<string, string> | undefined,
): string {
  if (!resolvedValues?.size) return value;

  let resolved = value;
  for (const [token, plaintext] of resolvedValues) {
    resolved = resolved.replaceAll(token, plaintext);
  }

  return resolved;
}

export function replaceProtectedBody(
  body: string,
  mimeType: string | undefined,
  resolvedValues: ReadonlyMap<string, string> | undefined,
): string {
  if (!resolvedValues?.size) return body;
  if (!isJSONMimeType(mimeType)) return replaceProtectedTokens(body, resolvedValues);
  if (!isValidJSONBody(body, mimeType)) return body;

  let resolved = body;
  for (const [token, plaintext] of resolvedValues) {
    if (!isJSONValue(plaintext)) continue;
    resolved = resolved.replaceAll(JSON.stringify(token), plaintext);
  }

  return resolved;
}

function isJSONMimeType(mimeType: string | undefined): boolean {
  const base = baseMimeType(mimeType);
  return base === "application/json"
    || base.endsWith("+json")
    || base === "application/x-ndjson"
    || base === "application/ndjson";
}

function isValidJSONBody(body: string, mimeType: string | undefined): boolean {
  const base = baseMimeType(mimeType);
  if (base === "application/x-ndjson" || base === "application/ndjson") {
    const documents = body.split(/\r?\n/).filter((line) => line.trim() !== "");
    return documents.length > 0 && documents.every(isJSONValue);
  }

  return isJSONValue(body);
}

function isJSONValue(value: string): boolean {
  try {
    JSON.parse(value);
    return true;
  } catch {
    return false;
  }
}

function baseMimeType(mimeType: string | undefined): string {
  return mimeType?.split(";", 1)[0].trim().toLowerCase() ?? "";
}
