import { readFile, writeFile } from "node:fs/promises";

const [output, ...inputs] = process.argv.slice(2);
if (!output || inputs.length < 2) {
  throw new Error("usage: merge-go-coverage.mjs OUTPUT INPUT...");
}

let mode;
const blocks = [];

for (const input of inputs) {
  const text = await readFile(input, "utf8");
  const [header, ...lines] = text.trimEnd().split("\n");
  if (!header?.startsWith("mode: ")) {
    throw new Error(`invalid Go coverage profile: ${input}`);
  }

  if (mode && header !== mode) {
    throw new Error(`coverage mode mismatch: ${input} uses ${header}, expected ${mode}`);
  }

  mode = header;
  blocks.push(...lines.filter(Boolean));
}

await writeFile(output, `${mode}\n${blocks.join("\n")}\n`);
