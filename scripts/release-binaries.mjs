#!/usr/bin/env node

import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import process from "node:process";
import { spawnSync } from "node:child_process";

import { normalizeVersion } from "./release.mjs";

export const binaries = Object.freeze([
  Object.freeze({
    name: "recorder",
    package: "./cmd/recorder",
    versionVariable:
      "github.com/mgurevin/recorder/internal/buildinfo.injectedVersion",
  }),
]);

export const targets = Object.freeze([
  Object.freeze({ os: "linux", arch: "amd64", archive: "tar.gz" }),
  Object.freeze({ os: "linux", arch: "arm64", archive: "tar.gz" }),
  Object.freeze({ os: "darwin", arch: "amd64", archive: "tar.gz" }),
  Object.freeze({ os: "darwin", arch: "arm64", archive: "tar.gz" }),
  Object.freeze({ os: "windows", arch: "amd64", archive: "zip" }),
  Object.freeze({ os: "windows", arch: "arm64", archive: "zip" }),
]);

export function archiveName(binary, version, target) {
  return `${binary.name}-${version}-${target.os}-${target.arch}.${target.archive}`;
}

export function checksumName(version) {
  return `recorder-${version}-checksums.txt`;
}

function run(command, args, options = {}) {
  const result = spawnSync(command, args, {
    cwd: options.cwd,
    env: options.env,
    encoding: "utf8",
    stdio: options.capture ? "pipe" : "inherit",
  });
  if (result.error) {
    throw result.error;
  }
  if (result.status !== 0) {
    const detail = options.capture ? `: ${(result.stderr || result.stdout).trim()}` : "";
    throw new Error(`${command} exited with status ${result.status}${detail}`);
  }

  return result.stdout;
}

function sha256(file) {
  const hash = crypto.createHash("sha256");
  hash.update(fs.readFileSync(file));
  return hash.digest("hex");
}

function executableName(binary, target) {
  return target.os === "windows" ? `${binary.name}.exe` : binary.name;
}

function packageArchive(root, stage, output, binary, target) {
  const executable = executableName(binary, target);
  fs.copyFileSync(path.join(root, "LICENSE"), path.join(stage, "LICENSE"));

  if (target.archive === "zip") {
    run("zip", ["-q", "-j", output, path.join(stage, executable), path.join(stage, "LICENSE")]);
    return;
  }

  run("tar", ["-czf", output, "-C", stage, executable, "LICENSE"]);
}

export function buildReleaseBinaries(root, rawVersion) {
  const version = normalizeVersion(rawVersion);
  const releaseDir = path.join(root, "build", "release", "binaries");
  const stagingRoot = path.join(root, "build", "release", ".binary-staging");

  fs.rmSync(releaseDir, { force: true, recursive: true });
  fs.rmSync(stagingRoot, { force: true, recursive: true });
  fs.mkdirSync(releaseDir, { recursive: true });
  fs.mkdirSync(stagingRoot, { recursive: true });

  const archives = [];
  try {
    for (const binary of binaries) {
      for (const target of targets) {
        const stage = path.join(stagingRoot, binary.name, `${target.os}-${target.arch}`);
        fs.mkdirSync(stage, { recursive: true });

        const executable = executableName(binary, target);
        run(
          process.env.GO || "go",
          [
            "build",
            "-trimpath",
            "-buildvcs=false",
            "-ldflags",
            `-s -w -X ${binary.versionVariable}=v${version}`,
            "-o",
            path.join(stage, executable),
            binary.package,
          ],
          {
            cwd: root,
            env: {
              ...process.env,
              CGO_ENABLED: "0",
              GOCACHE:
                process.env.RELEASE_GOCACHE ||
                path.join(root, "build", "cache", "release-binaries"),
              GOOS: target.os,
              GOARCH: target.arch,
            },
          },
        );

        const archive = path.join(releaseDir, archiveName(binary, version, target));
        packageArchive(root, stage, archive, binary, target);
        archives.push(archive);
      }
    }
  } finally {
    fs.rmSync(stagingRoot, { force: true, recursive: true });
  }

  const checksums = archives
    .sort((left, right) => path.basename(left).localeCompare(path.basename(right)))
    .map((file) => `${sha256(file)}  ${path.basename(file)}`)
    .join("\n");
  fs.writeFileSync(path.join(releaseDir, checksumName(version)), `${checksums}\n`);

  return { archives, releaseDir };
}

function main() {
  const [rawVersion] = process.argv.slice(2);
  if (!rawVersion) {
    console.error("usage: node scripts/release-binaries.mjs VERSION");
    process.exitCode = 2;
    return;
  }

  const { archives, releaseDir } = buildReleaseBinaries(process.cwd(), rawVersion);
  console.log(`wrote ${archives.length} binary archives and checksums to ${releaseDir}`);
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === path.resolve(new URL(import.meta.url).pathname)
) {
  try {
    main();
  } catch (error) {
    console.error(`release binaries: ${error.message}`);
    process.exitCode = 1;
  }
}
