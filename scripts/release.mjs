#!/usr/bin/env node

import fs from "node:fs";
import path from "node:path";
import process from "node:process";

const rootModule = "github.com/mgurevin/recorder";

export function normalizeVersion(value) {
  const version = String(value ?? "").replace(/^v/, "");
  if (!/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(version)) {
    throw new Error(`expected VERSION=X.Y.Z, got ${JSON.stringify(value ?? "")}`);
  }

  return version;
}

function read(root, name) {
  return fs.readFileSync(path.join(root, name), "utf8");
}

function write(root, name, value) {
  fs.writeFileSync(path.join(root, name), value);
}

function updateExactly(value, pattern, replacement, description) {
  const flags = pattern.flags.includes("g") ? pattern.flags : `${pattern.flags}g`;
  const matches = [...value.matchAll(new RegExp(pattern.source, flags))];
  if (matches.length !== 1) {
    throw new Error(`expected exactly one ${description}; found ${matches.length}`);
  }

  return value.replace(pattern, replacement);
}

function releaseDate(value) {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(value)) {
    throw new Error(`expected RELEASE_DATE=YYYY-MM-DD, got ${JSON.stringify(value)}`);
  }

  return value;
}

function changelogSection(changelog, heading) {
  const start = changelog
    .split("\n")
    .reduce((offset, line) => {
      if (offset.found) {
        return offset;
      }
      if (line === heading || line.startsWith(`${heading} - `)) {
        return { found: true, value: offset.value };
      }
      return { found: false, value: offset.value + line.length + 1 };
    }, { found: false, value: 0 });
  if (!start.found) {
    throw new Error(`missing changelog heading ${heading}`);
  }

  const contentStart = changelog.indexOf("\n", start.value) + 1;
  const next = changelog.indexOf("\n## [", contentStart);
  return changelog.slice(contentStart, next < 0 ? changelog.length : next).trim();
}

export function prepareRelease(root, rawVersion, rawDate) {
  const version = normalizeVersion(rawVersion);
  const date = releaseDate(rawDate);
  let changelog = read(root, "CHANGELOG.md");
  const unreleased = changelogSection(changelog, "## [Unreleased]");

  if (!unreleased) {
    throw new Error("CHANGELOG.md Unreleased section is empty");
  }
  if (changelog.includes(`## [${version}]`)) {
    throw new Error(`CHANGELOG.md already contains ${version}`);
  }

  changelog = updateExactly(
    changelog,
    /^## \[Unreleased\]\n/m,
    `## [Unreleased]\n\n## [${version}] - ${date}\n`,
    "Unreleased heading",
  );

  const previousLink = changelog.match(
    /^\[Unreleased\]: https:\/\/github\.com\/mgurevin\/recorder\/compare\/v([^.\s]+\.[^.\s]+\.[^.\s]+)\.\.\.HEAD$/m,
  );
  if (!previousLink) {
    throw new Error("missing or malformed Unreleased comparison link");
  }

  changelog = updateExactly(
    changelog,
    /^\[Unreleased\]: .*$/m,
    `[Unreleased]: https://github.com/mgurevin/recorder/compare/v${version}...HEAD\n` +
      `[${version}]: https://github.com/mgurevin/recorder/compare/v${previousLink[1]}...v${version}`,
    "Unreleased comparison link",
  );
  write(root, "CHANGELOG.md", changelog);

  let har = read(root, "har.go");
  har = updateExactly(
    har,
    /^\s*creatorVersion = "[^"]+"$/m,
    `\tcreatorVersion = "${version}"`,
    "creatorVersion constant",
  );
  write(root, "har.go", har);

  let integration = read(root, "integration_test.go");
  integration = updateExactly(
    integration,
    /(creator\["version"\].*got != )"[^"]+"/,
    `$1"${version}"`,
    "creator version comparison",
  );
  integration = updateExactly(
    integration,
    /(creator\["version"\], )"[^"]+"/,
    `$1"${version}"`,
    "creator version failure message",
  );
  write(root, "integration_test.go", integration);

  for (const name of ["inspector/package.json", "inspector/package-lock.json"]) {
    const document = JSON.parse(read(root, name));
    document.version = version;
    if (name.endsWith("package-lock.json")) {
      if (!document.packages?.[""]) {
        throw new Error("inspector/package-lock.json is missing its root package");
      }
      document.packages[""].version = version;
    }
    write(root, name, `${JSON.stringify(document, null, 2)}\n`);
  }

  verifyRelease(root, version);
}

export function verifyRelease(root, rawVersion) {
  const version = normalizeVersion(rawVersion);
  const checks = [
    ["CHANGELOG release", read(root, "CHANGELOG.md").includes(`## [${version}] - `)],
    [
      "CHANGELOG comparison link",
      read(root, "CHANGELOG.md").includes(
        `[Unreleased]: https://github.com/mgurevin/recorder/compare/v${version}...HEAD`,
      ),
    ],
    ["HAR creator version", read(root, "har.go").includes(`creatorVersion = "${version}"`)],
    [
      "HAR integration assertion",
      read(root, "integration_test.go").includes(`got != "${version}"`),
    ],
    ["Inspector version", JSON.parse(read(root, "inspector/package.json")).version === version],
    [
      "Inspector lock version",
      JSON.parse(read(root, "inspector/package-lock.json")).packages?.[""]?.version === version,
    ],
  ];
  const failed = checks.filter(([, ok]) => !ok).map(([name]) => name);
  if (failed.length > 0) {
    throw new Error(`release version is inconsistent: ${failed.join(", ")}`);
  }
}

export function retireExceptions(root, moduleName) {
  const file = "api/compatibility-exceptions.txt";
  const lines = read(root, file).split("\n");
  const output = [];
  let dropping = false;

  for (const line of lines) {
    const section = line.match(/^\[([^\]]+)\]$/);
    if (section) {
      dropping = section[1] === moduleName;
    }
    if (!dropping) {
      output.push(line);
    }
  }

  const normalized = output.join("\n").replace(/\n{3,}/g, "\n\n").trimEnd();
  write(
    root,
    file,
    `${normalized || "# Temporary pre-v1 exceptions belong here only after explicit review."}\n`,
  );
}

export function releaseNotes(root, rawVersion) {
  const version = normalizeVersion(rawVersion);
  return changelogSection(read(root, "CHANGELOG.md"), `## [${version}]`);
}

function usage() {
  console.error(
    "usage: node scripts/release.mjs prepare|verify|notes|retire-root|retire-otel VERSION",
  );
}

function main() {
  const [command, rawVersion] = process.argv.slice(2);
  const root = process.cwd();

  switch (command) {
    case "prepare":
      prepareRelease(root, rawVersion, process.env.RELEASE_DATE ?? new Date().toISOString().slice(0, 10));
      break;
    case "verify":
      verifyRelease(root, rawVersion);
      break;
    case "notes":
      process.stdout.write(`${releaseNotes(root, rawVersion)}\n`);
      break;
    case "retire-root":
      retireExceptions(root, rootModule);
      break;
    case "retire-otel":
      retireExceptions(root, `${rootModule}/otelrecorder`);
      break;
    default:
      usage();
      process.exitCode = 2;
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === path.resolve(new URL(import.meta.url).pathname)) {
  try {
    main();
  } catch (error) {
    console.error(`release: ${error.message}`);
    process.exitCode = 1;
  }
}
