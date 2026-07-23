import assert from "node:assert/strict";
import test from "node:test";

import {
  badgeStatus,
  camoURLFromREADME,
  purgeCamo,
  renderedREADME,
  statusesDiffer,
} from "./api-badge-cache.mjs";

const compatible =
  '<svg role="img" aria-label="API compatibility: v1 compatible"></svg>';
const tracked =
  '<svg role="img" aria-label="API compatibility: v1 tracked"></svg>';

test("badgeStatus reads the semantic status", () => {
  assert.equal(badgeStatus(compatible), "v1 compatible");
  assert.equal(statusesDiffer(compatible, tracked), true);
  assert.equal(statusesDiffer(tracked, tracked), false);
  assert.throws(() => badgeStatus("<svg></svg>"), /status is missing/);
});

test("camoURLFromREADME selects only the canonical badge image", () => {
  const html = [
    '<img src="https://camo.githubusercontent.com/other" data-canonical-src="https://example.test/other.svg">',
    '<img alt="API compatibility" data-canonical-src="https://mgurevin.github.io/recorder/api-compatibility.svg"',
    ' src="https://camo.githubusercontent.com/digest?x=1&amp;y=2">',
  ].join("");

  assert.equal(
    camoURLFromREADME(html),
    "https://camo.githubusercontent.com/digest?x=1&y=2",
  );
  assert.throws(
    () =>
      camoURLFromREADME(
        '<img data-canonical-src="https://mgurevin.github.io/recorder/api-compatibility.svg" src="https://example.test/badge.svg">',
      ),
    /refusing to purge/,
  );
});

test("renderedREADME authenticates and accepts GitHub JSON HTML", async () => {
  let request;
  const html = "<p>README</p>";
  const result = await renderedREADME("owner/repo", "secret", async (...args) => {
    request = args;
    return new Response(JSON.stringify(html), {
      headers: { "content-type": "application/json; charset=utf-8" },
    });
  });

  assert.equal(result, html);
  assert.equal(request[0], "https://api.github.com/repos/owner/repo/readme");
  assert.equal(request[1].headers.Authorization, "Bearer secret");
});

test("purgeCamo uses the PURGE method and rejects failures", async () => {
  let method;
  await purgeCamo("https://camo.githubusercontent.com/digest", async (_, options) => {
    method = options.method;
    return new Response("ok");
  });
  assert.equal(method, "PURGE");

  await assert.rejects(
    purgeCamo(
      "https://camo.githubusercontent.com/digest",
      async () => new Response("no", { status: 503 }),
    ),
    /HTTP 503/,
  );
});
