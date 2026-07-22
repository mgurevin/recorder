import type { NEntry } from "../types/har";
import { HarParseError, parseHar } from "./parse";

/** validateDebugStreamURL restricts live inspection to the recorder's local-development threat model. */
export function validateDebugStreamURL(value: string): URL {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    throw new Error("Enter a valid absolute URL.");
  }

  if (url.protocol !== "http:" && url.protocol !== "https:") {
    throw new Error("The live stream URL must use HTTP or HTTPS.");
  }

  if (url.hostname !== "localhost" && url.hostname !== "127.0.0.1" && url.hostname !== "[::1]" && url.hostname !== "::1") {
    throw new Error("Live inspection accepts loopback URLs only.");
  }

  url.username = "";
  url.password = "";

  return url;
}

/** parseLiveEntry validates one entry event using the same rules as file imports. */
export function parseLiveEntry(data: string, id: number): NEntry {
  const loaded = parseHar(data);
  if (loaded.entries.length !== 1) {
    throw new HarParseError("A live entry event must contain exactly one HAR entry.");
  }

  return { ...loaded.entries[0], id };
}
