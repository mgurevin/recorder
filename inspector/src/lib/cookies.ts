import type { HarCookie, NameValue } from "../types/har";

/**
 * Returns the complete Set-Cookie attribute text when the response header is
 * available. HAR 1.2's structured cookie object cannot represent modern
 * attributes such as SameSite, Max-Age, Priority, or Partitioned.
 */
export function cookieAttributeTexts(cookies: HarCookie[], headers: NameValue[] | undefined): string[] {
  const setCookies = (headers ?? [])
    .filter((header) => header.name.toLowerCase() === "set-cookie")
    .map((header) => parseSetCookie(header.value));
  const used = new Set<number>();

  return cookies.map((cookie) => {
    const match = setCookies.findIndex((candidate, index) => !used.has(index) && candidate?.name === cookie.name);
    if (match >= 0) {
      used.add(match);
      const attributes = setCookies[match]?.attributes;
      if (attributes) return attributes;
    }
    return structuredAttributes(cookie);
  });
}

function parseSetCookie(value: string): { name: string; attributes: string } | undefined {
  const semicolon = value.indexOf(";");
  const pair = (semicolon >= 0 ? value.slice(0, semicolon) : value).trim();
  const equals = pair.indexOf("=");
  if (equals <= 0) return undefined;
  return {
    name: pair.slice(0, equals).trim(),
    attributes: semicolon >= 0 ? value.slice(semicolon + 1).trim() : "",
  };
}

function structuredAttributes(cookie: HarCookie): string {
  return [
    cookie.path && `path=${cookie.path}`,
    cookie.domain && `domain=${cookie.domain}`,
    cookie.expires && `expires=${cookie.expires}`,
    cookie.httpOnly && "httpOnly",
    cookie.secure && "secure",
  ].filter(Boolean).join("; ");
}
