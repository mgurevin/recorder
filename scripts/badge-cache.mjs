import fs from "node:fs";
import { pathToFileURL } from "node:url";

function decodeHTML(value) {
  return value
    .replaceAll("&amp;", "&")
    .replaceAll("&quot;", '"')
    .replaceAll("&#39;", "'")
    .replaceAll("&lt;", "<")
    .replaceAll("&gt;", ">");
}

export function badgeValue(svg, label) {
  const escapedLabel = label.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const match = svg.match(
    new RegExp(`aria-label=["']${escapedLabel}:\\s*([^"']+)["']`, "i"),
  );
  if (!match) {
    throw new Error(`${label} badge value is missing`);
  }

  return decodeHTML(match[1].trim());
}

export function badgeValueChanged(
  previousSVG,
  nextSVG,
  label,
  minimumChange = 0,
) {
  const previous = badgeValue(previousSVG, label);
  const next = badgeValue(nextSVG, label);
  if (minimumChange === 0) {
    return previous !== next;
  }

  const previousNumber = Number.parseFloat(previous);
  const nextNumber = Number.parseFloat(next);
  if (!Number.isFinite(previousNumber) || !Number.isFinite(nextNumber)) {
    throw new Error(`${label} badge values must be numeric when a threshold is used`);
  }

  // GitHub asks Camo users to purge sparingly. Numeric badges therefore purge
  // only after a material change instead of on every small measurement drift.
  return Math.abs(nextNumber - previousNumber) + 1e-9 >= minimumChange;
}

export function camoURLFromREADME(html, sourceURL) {
  for (const tag of html.matchAll(/<img\b[^>]*>/gi)) {
    const attributes = new Map();
    for (const attribute of tag[0].matchAll(/([\w:-]+)=["']([^"']*)["']/g)) {
      attributes.set(attribute[1].toLowerCase(), decodeHTML(attribute[2]));
    }

    if (attributes.get("data-canonical-src") !== sourceURL) {
      continue;
    }

    const candidate = new URL(attributes.get("src"));
    if (
      candidate.protocol !== "https:" ||
      (candidate.hostname !== "camo.githubusercontent.com" &&
        !candidate.hostname.endsWith(".githubusercontent.com"))
    ) {
      throw new Error(`refusing to purge unexpected image proxy: ${candidate}`);
    }

    return candidate.href;
  }

  throw new Error(`README Camo URL was not found for ${sourceURL}`);
}

export async function renderedREADME(repository, token, fetchImpl = fetch) {
  const response = await fetchImpl(
    `https://api.github.com/repos/${repository}/readme`,
    {
      headers: {
        Accept: "application/vnd.github.html+json",
        Authorization: `Bearer ${token}`,
        "X-GitHub-Api-Version": "2022-11-28",
      },
    },
  );
  if (!response.ok) {
    throw new Error(`GitHub README request failed: HTTP ${response.status}`);
  }

  const body = await response.text();
  if ((response.headers.get("content-type") ?? "").includes("application/json")) {
    return JSON.parse(body);
  }

  return body;
}

export async function purgeCamo(camoURL, fetchImpl = fetch) {
  const response = await fetchImpl(camoURL, { method: "PURGE" });
  if (!response.ok) {
    throw new Error(`Camo purge failed: HTTP ${response.status}`);
  }
}

function workflowOutput(name, value) {
  const output = process.env.GITHUB_OUTPUT;
  if (output) {
    fs.appendFileSync(output, `${name}=${value}\n`);
  } else {
    console.log(`${name}=${value}`);
  }
}

async function compare(localFile, remoteURL, label, minimumChange = 0) {
  const nextSVG = fs.readFileSync(localFile, "utf8");
  const nextValue = badgeValue(nextSVG, label);
  let previousValue = "";
  let changed = false;

  try {
    const remote = new URL(remoteURL);
    remote.searchParams.set("status-check", process.env.GITHUB_SHA ?? Date.now().toString());
    const response = await fetch(remote, { cache: "no-store" });
    if (response.ok) {
      const previousSVG = await response.text();
      previousValue = badgeValue(previousSVG, label);
      changed = badgeValueChanged(
        previousSVG,
        nextSVG,
        label,
        minimumChange,
      );
    } else {
      console.log(
        `::warning::Previous ${label} badge could not be read (HTTP ${response.status}); Camo purge skipped.`,
      );
    }
  } catch (error) {
    console.log(
      `::warning::Previous ${label} badge could not be compared; Camo purge skipped: ${error.message}`,
    );
  }

  workflowOutput("value_changed", changed);
  workflowOutput("previous_value", previousValue);
  workflowOutput("next_value", nextValue);
}

async function purge(repository, sourceURL) {
  try {
    const token = process.env.GITHUB_TOKEN;
    if (!token) {
      throw new Error("GITHUB_TOKEN is required");
    }

    const html = await renderedREADME(repository, token);
    const camoURL = camoURLFromREADME(html, sourceURL);
    await purgeCamo(camoURL);
    console.log(`Purged the stale README badge ${sourceURL} from Camo.`);
  } catch (error) {
    console.log(`::warning::README badge Camo purge failed for ${sourceURL}: ${error.message}`);
  }
}

async function main() {
  const [command, ...args] = process.argv.slice(2);
  if (command === "compare" && (args.length === 3 || args.length === 4)) {
    const minimumChange = args[3] === undefined ? 0 : Number(args[3]);
    if (!Number.isFinite(minimumChange) || minimumChange < 0) {
      throw new Error("minimum change must be a non-negative number");
    }

    await compare(args[0], args[1], args[2], minimumChange);
    return;
  }
  if (command === "purge" && args.length === 2) {
    await purge(args[0], args[1]);
    return;
  }

  throw new Error(
    "usage: badge-cache.mjs compare <local-svg> <published-url> <label> [minimum-change] | purge <owner/repo> <source-url>",
  );
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
