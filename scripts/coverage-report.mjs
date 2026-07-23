import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";

const root = process.cwd();
const outputDir = path.join(root, "build", "coverage");

const components = [
  {
    name: "Recorder libraries",
    measure: "Go statements",
    report: "go-recorder.html",
    file: path.join(outputDir, "go-recorder.out"),
    minimum: 85,
    format: "go",
  },
  {
    name: "Root-module examples",
    measure: "Go statements",
    report: "go-examples.html",
    file: path.join(outputDir, "go-examples.out"),
    minimum: 60,
    format: "go",
  },
  {
    name: "OpenTelemetry recorder",
    measure: "Go statements",
    report: "go-otelrecorder.html",
    file: path.join(outputDir, "go-otelrecorder.out"),
    minimum: 80,
    format: "go",
  },
  {
    name: "Content decoder example",
    measure: "Go statements",
    report: "go-content-decoders.html",
    file: path.join(outputDir, "go-content-decoders.out"),
    minimum: 80,
    format: "go",
  },
  {
    name: "Inspector",
    measure: "TypeScript lines",
    report: "inspector/index.html",
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

function escapeHtml(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#039;");
}

function workflowRunUrl() {
  const server = process.env.GITHUB_SERVER_URL?.replace(/\/$/, "");
  const repository = process.env.GITHUB_REPOSITORY;
  const runID = process.env.GITHUB_RUN_ID;

  if (!server || !repository || !runID) return undefined;

  return `${server}/${repository}/actions/runs/${runID}`;
}

function dashboardHtml(report) {
  const componentRows = report.components
    .map((component) => {
      const status = component.percentage >= component.minimum ? "Pass" : "Fail";

      return `<tr>
        <th scope="row"><a href="${escapeHtml(component.report)}">${escapeHtml(component.name)}</a><span>${escapeHtml(component.measure)}</span></th>
        <td>${component.covered.toLocaleString("en-US")} / ${component.total.toLocaleString("en-US")}</td>
        <td><strong>${component.percentage.toFixed(2)}%</strong></td>
        <td>${component.minimum.toFixed(0)}%</td>
        <td><span class="status ${status.toLowerCase()}">${status}</span></td>
      </tr>`;
    })
    .join("\n");
  const generatedAt = new Intl.DateTimeFormat("en", {
    dateStyle: "medium",
    timeStyle: "long",
    timeZone: "UTC",
  }).format(new Date(report.generatedAt));
  const workflowLink = report.workflowRun
    ? `<a href="${escapeHtml(report.workflowRun)}">View generating workflow run</a>`
    : "";

  return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="dark light">
  <title>recorder coverage</title>
  <style>
    :root { color-scheme: dark; --bg: #0b1118; --surface: #111a25; --line: #273548; --text: #e7edf5; --muted: #93a4b8; --accent: #65b7f3; --pass: #45cf78; --fail: #ff7070; }
    @media (prefers-color-scheme: light) { :root { color-scheme: light; --bg: #f5f7fa; --surface: #fff; --line: #d7e0ea; --text: #17202b; --muted: #5f6f81; --accent: #0969da; --pass: #16813a; --fail: #cf222e; } }
    * { box-sizing: border-box; }
    body { margin: 0; background: var(--bg); color: var(--text); font: 15px/1.5 ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    main { width: min(1080px, calc(100% - 32px)); margin: 0 auto; padding: 64px 0; }
    header { display: flex; align-items: flex-end; justify-content: space-between; gap: 32px; margin-bottom: 28px; }
    .eyebrow { margin: 0 0 8px; color: var(--accent); font-size: 12px; font-weight: 700; letter-spacing: .12em; text-transform: uppercase; }
    h1 { margin: 0; font-size: clamp(32px, 6vw, 56px); line-height: 1; letter-spacing: -.04em; }
    .summary { text-align: right; }
    .summary strong { display: block; font-size: 32px; line-height: 1.1; }
    .summary span, .description, footer { color: var(--muted); }
    .description { max-width: 720px; margin: 18px 0 32px; }
    .panel { overflow: hidden; border: 1px solid var(--line); border-radius: 12px; background: var(--surface); }
    table { width: 100%; border-collapse: collapse; }
    th, td { padding: 16px 18px; border-bottom: 1px solid var(--line); text-align: right; white-space: nowrap; }
    thead th { color: var(--muted); font-size: 12px; letter-spacing: .06em; text-transform: uppercase; }
    th:first-child { text-align: left; white-space: normal; }
    tbody tr:last-child th, tbody tr:last-child td { border-bottom: 0; }
    tbody th span { display: block; color: var(--muted); font-size: 12px; font-weight: 400; }
    a { color: var(--accent); text-decoration: none; }
    a:hover { text-decoration: underline; }
    .status { display: inline-block; min-width: 54px; padding: 3px 9px; border: 1px solid currentColor; border-radius: 999px; text-align: center; font-size: 12px; font-weight: 700; }
    .pass { color: var(--pass); }
    .fail { color: var(--fail); }
    footer { display: flex; flex-wrap: wrap; justify-content: space-between; gap: 16px; margin-top: 22px; font-size: 13px; }
    .provenance { display: grid; gap: 3px; }
    footer nav { display: flex; gap: 16px; }
    @media (max-width: 720px) { main { padding: 36px 0; } header { align-items: flex-start; flex-direction: column; } .summary { text-align: left; } .panel { overflow-x: auto; } th, td { padding: 13px; } }
  </style>
</head>
<body>
  <main>
    <header>
      <div><p class="eyebrow">recorder / quality evidence</p><h1>Test coverage</h1></div>
      <div class="summary"><strong>${report.project.percentage.toFixed(2)}%</strong><span>${report.project.passing ? "All thresholds passed" : "A threshold failed"}</span></div>
    </header>
    <p class="description">Coverage is calculated locally in GitHub Actions and published with the Inspector. The project value is weighted across Go statements and Inspector TypeScript lines; no source or report is uploaded to an external coverage service.</p>
    <section class="panel" aria-label="Coverage by component">
      <table>
        <thead><tr><th scope="col">Component</th><th scope="col">Covered</th><th scope="col">Coverage</th><th scope="col">Minimum</th><th scope="col">Result</th></tr></thead>
        <tbody>${componentRows}</tbody>
      </table>
    </section>
    <footer>
      <div class="provenance"><span>Generated ${escapeHtml(generatedAt)}</span>${workflowLink}</div>
      <nav aria-label="Coverage downloads"><a href="coverage.json">JSON summary</a><a href="summary.md">Markdown summary</a><a href="../">Open Inspector</a></nav>
    </footer>
  </main>
</body>
</html>
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
  workflowRun: workflowRunUrl(),
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
  writeFile(path.join(outputDir, "index.html"), dashboardHtml(report)),
]);

process.stdout.write(markdown);
if (!passing) {
  const failures = results
    .filter((result) => result.percentage < result.minimum)
    .map((result) => `${result.name} ${result.percentage.toFixed(2)}% < ${result.minimum.toFixed(0)}%`)
    .join(", ");
  throw new Error(`coverage threshold failed: ${failures}`);
}
