import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";

const root = process.cwd();
const outputDir = path.join(root, "build", "coverage");

const components = [
  {
    name: "Recorder and CSV example",
    measure: "Go statements",
    file: path.join(outputDir, "go-root.out"),
    minimum: 85,
    format: "go",
  },
  {
    name: "OpenTelemetry recorder",
    measure: "Go statements",
    file: path.join(outputDir, "go-otelrecorder.out"),
    minimum: 80,
    format: "go",
  },
  {
    name: "Content decoder example",
    measure: "Go statements",
    file: path.join(outputDir, "go-content-decoders.out"),
    minimum: 80,
    format: "go",
  },
  {
    name: "Inspector",
    measure: "TypeScript lines",
    file: path.join(root, "inspector", "coverage", "coverage-summary.json"),
    minimum: 90,
    format: "istanbul",
  },
];

function percentage(covered, total) {
  return total === 0 ? 100 : (covered / total) * 100;
}

function parseGoProfile(text) {
  let covered = 0;
  let total = 0;

  for (const line of text.split("\n")) {
    if (!line || line.startsWith("mode:")) continue;

    const match = /\s(\d+)\s+(\d+)$/.exec(line);
    if (!match) throw new Error(`invalid Go coverage line: ${line}`);

    const statements = Number.parseInt(match[1], 10);
    const count = Number.parseInt(match[2], 10);
    total += statements;
    if (count > 0) covered += statements;
  }

  return { covered, total };
}

function parseIstanbulSummary(text) {
  const summary = JSON.parse(text);
  const lines = summary?.total?.lines;
  if (!Number.isFinite(lines?.covered) || !Number.isFinite(lines?.total)) {
    throw new Error("invalid Inspector coverage summary");
  }

  return { covered: lines.covered, total: lines.total };
}

function badgeSvg(value, passing) {
  const label = "coverage";
  const message = `${value.toFixed(1)}%`;
  const color = passing ? "#2da44e" : "#cf222e";
  const labelWidth = 72;
  const valueWidth = 58;

  return `<svg xmlns="http://www.w3.org/2000/svg" width="${labelWidth + valueWidth}" height="20" role="img" aria-label="${label}: ${message}">
  <title>${label}: ${message}</title>
  <linearGradient id="s" x2="0" y2="100%"><stop offset="0" stop-color="#bbb" stop-opacity=".1"/><stop offset="1" stop-opacity=".1"/></linearGradient>
  <clipPath id="r"><rect width="${labelWidth + valueWidth}" height="20" rx="3"/></clipPath>
  <g clip-path="url(#r)"><rect width="${labelWidth}" height="20" fill="#555"/><rect x="${labelWidth}" width="${valueWidth}" height="20" fill="${color}"/><rect width="${labelWidth + valueWidth}" height="20" fill="url(#s)"/></g>
  <g fill="#fff" text-anchor="middle" font-family="Verdana,DejaVu Sans,sans-serif" font-size="11"><text x="${labelWidth / 2}" y="15" fill="#010101" fill-opacity=".3">${label}</text><text x="${labelWidth / 2}" y="14">${label}</text><text x="${labelWidth + valueWidth / 2}" y="15" fill="#010101" fill-opacity=".3">${message}</text><text x="${labelWidth + valueWidth / 2}" y="14">${message}</text></g>
</svg>
`;
}

await mkdir(outputDir, { recursive: true });

const results = [];
for (const component of components) {
  const text = await readFile(component.file, "utf8");
  const counts = component.format === "go" ? parseGoProfile(text) : parseIstanbulSummary(text);
  results.push({ ...component, ...counts, percentage: percentage(counts.covered, counts.total) });
}

const covered = results.reduce((sum, result) => sum + result.covered, 0);
const total = results.reduce((sum, result) => sum + result.total, 0);
const projectCoverage = percentage(covered, total);
const passing = results.every((result) => result.percentage >= result.minimum);

const rows = results.map((result) =>
  `| ${result.name} | ${result.measure} | ${result.covered} / ${result.total} | ${result.percentage.toFixed(2)}% | ${result.minimum.toFixed(0)}% | ${result.percentage >= result.minimum ? "Pass" : "Fail"} |`,
);
const markdown = [
  "## Coverage",
  "",
  "Coverage is calculated locally in GitHub Actions; no source or report is uploaded to a coverage service.",
  "",
  "| Component | Measure | Covered | Coverage | Minimum | Result |",
  "| --- | --- | ---: | ---: | ---: | --- |",
  ...rows,
  "",
  `**Project coverage:** ${projectCoverage.toFixed(2)}% (${covered} / ${total}) — weighted across Go statements and Inspector TypeScript lines.`,
  "",
].join("\n");

const report = {
  generatedAt: new Date().toISOString(),
  project: { covered, total, percentage: Number(projectCoverage.toFixed(2)), passing },
  components: results.map(({ file: _file, format: _format, ...result }) => ({
    ...result,
    percentage: Number(result.percentage.toFixed(2)),
  })),
};

await Promise.all([
  writeFile(path.join(outputDir, "summary.md"), markdown),
  writeFile(path.join(outputDir, "coverage.json"), `${JSON.stringify(report, null, 2)}\n`),
  writeFile(path.join(outputDir, "coverage.svg"), badgeSvg(projectCoverage, passing)),
]);

process.stdout.write(markdown);
if (!passing) {
  const failures = results
    .filter((result) => result.percentage < result.minimum)
    .map((result) => `${result.name} ${result.percentage.toFixed(2)}% < ${result.minimum.toFixed(0)}%`)
    .join(", ");
  throw new Error(`coverage threshold failed: ${failures}`);
}
