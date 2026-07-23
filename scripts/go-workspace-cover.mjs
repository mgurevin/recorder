import { execFile } from "node:child_process";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";

const run = promisify(execFile);
const [goCommand, profile, output, ...modules] = process.argv.slice(2);

if (!goCommand || !profile || !output || modules.length === 0) {
  throw new Error("usage: go-workspace-cover.mjs GO PROFILE OUTPUT MODULE...");
}

const workspace = await mkdtemp(path.join(tmpdir(), "recorder-cover-"));
const workspaceFile = path.join(workspace, "go.work");

try {
  await run(goCommand, ["work", "init", ...modules], { cwd: workspace });
  await run(goCommand, ["tool", "cover", `-html=${profile}`, `-o=${output}`], {
    env: { ...process.env, GOWORK: workspaceFile },
  });
} finally {
  await rm(workspace, { recursive: true, force: true });
}
