#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
apidiff="${APIDIFF:-"$root_dir/build/tools/apidiff"}"
exceptions_file="${API_DIFF_EXCEPTIONS:-"$root_dir/api/compatibility-exceptions.txt"}"
output_dir="${API_DIFF_OUTPUT_DIR:-"$root_dir/build/api-diff"}"
tmp_dir="$(mktemp -d)"

cleanup() {
	rm -rf "$tmp_dir"
}

trap cleanup EXIT

latest_tag() {
	local pattern="$1"

	git -C "$root_dir" tag --list "$pattern" --sort=-v:refname | head -n 1
}

module_dir() {
	local module="$1"
	local metadata
	local directory

	if ! metadata="$(go mod download -json "$module")"; then
		echo "failed to download API baseline module $module:" >&2
		printf '%s\n' "$metadata" >&2

		return 1
	fi

	directory="$(
		printf '%s\n' "$metadata" |
			sed -n 's/^[[:space:]]*"Dir": "\(.*\)",$/\1/p'
	)"
	if [[ -z "$directory" ]]; then
		echo "API baseline module $module did not report a source directory:" >&2
		printf '%s\n' "$metadata" >&2

		return 1
	fi

	printf '%s\n' "$directory"
}

has_package() {
	local directory="$1"

	if [[ ! -d "$directory" ]]; then
		return 1
	fi

	find "$directory" -maxdepth 1 -type f -name '*.go' ! -name '*_test.go' -print -quit |
		grep -q .
}

append_report() {
	local package="$1"
	local old_export="$2"
	local new_export="$3"
	local report
	local incompatible

	report="$("$apidiff" "$old_export" "$new_export")"
	incompatible="$("$apidiff" -incompatible "$old_export" "$new_export")"

	printf '## %s\n\n' "$package" >>"$tmp_dir/report.md"
	if [[ -n "$report" ]]; then
		printf '```text\n%s\n```\n\n' "$report" >>"$tmp_dir/report.md"
	else
		printf 'No API changes.\n\n' >>"$tmp_dir/report.md"
	fi

	if [[ -n "$incompatible" ]]; then
		printf '[%s]\n%s\n' "$package" "$incompatible" >>"$tmp_dir/incompatible.txt"
	fi
}

write_report_assets() {
	local badge_status
	local badge_color
	local report_html
	local status_heading
	local status_detail

	mkdir -p "$output_dir"
	cp "$tmp_dir/report.md" "$output_dir/report.md"
	cp "$tmp_dir/incompatible.txt" "$output_dir/incompatible.txt"

	if [[ -s "$tmp_dir/incompatible.txt" ]]; then
		badge_status="v1 tracked"
		badge_color="#2563eb"
		status_heading="Reviewed pre-v1 changes"
		status_detail="The current tree contains intentional API changes recorded in the reviewed exception set. New or altered incompatibilities fail CI."
	else
		badge_status="v1 compatible"
		badge_color="#15803d"
		status_heading="Compatible"
		status_detail="No incompatible public API changes were found relative to the latest release baselines."
	fi

	cat >"$output_dir/api-compatibility.svg" <<EOF
<svg xmlns="http://www.w3.org/2000/svg" width="214" height="20" role="img" aria-label="API compatibility: $badge_status">
  <title>API compatibility: $badge_status</title>
  <linearGradient id="s" x2="0" y2="100%">
    <stop offset="0" stop-color="#fff" stop-opacity=".1"/>
    <stop offset="1" stop-opacity=".1"/>
  </linearGradient>
  <clipPath id="r"><rect width="214" height="20" rx="3" fill="#fff"/></clipPath>
  <g clip-path="url(#r)">
    <rect width="112" height="20" fill="#334155"/>
    <rect x="112" width="102" height="20" fill="$badge_color"/>
    <rect width="214" height="20" fill="url(#s)"/>
  </g>
  <g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">
    <text x="56" y="15" fill="#010101" fill-opacity=".3">API compatibility</text>
    <text x="56" y="14">API compatibility</text>
    <text x="163" y="15" fill="#010101" fill-opacity=".3">$badge_status</text>
    <text x="163" y="14">$badge_status</text>
  </g>
</svg>
EOF

	report_html="$(
		sed \
			-e 's/&/\&amp;/g' \
			-e 's/</\&lt;/g' \
			-e 's/>/\&gt;/g' \
			"$tmp_dir/report.md"
	)"

	cat >"$output_dir/index.html" <<EOF
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>recorder public API compatibility</title>
  <style>
    :root { color-scheme: dark; font-family: Inter, ui-sans-serif, system-ui, sans-serif; background: #081018; color: #e5edf6; }
    * { box-sizing: border-box; }
    body { margin: 0; padding: 64px 24px; }
    main { width: min(1080px, 100%); margin: 0 auto; }
    .eyebrow { color: #60a5fa; font-size: 13px; font-weight: 700; letter-spacing: .14em; text-transform: uppercase; }
    h1 { margin: 12px 0 16px; font-size: clamp(40px, 7vw, 72px); letter-spacing: -.04em; }
    .lead { max-width: 780px; color: #9fb0c3; font-size: 18px; line-height: 1.6; }
    .status, .report { margin-top: 32px; border: 1px solid #29394b; border-radius: 14px; background: #101b27; }
    .status { padding: 24px; }
    .status h2 { margin: 0 0 8px; font-size: 21px; }
    .status p { margin: 0; color: #aebdcd; line-height: 1.55; }
    .report { overflow: hidden; }
    .report header { padding: 18px 22px; border-bottom: 1px solid #29394b; color: #aebdcd; font-size: 13px; letter-spacing: .1em; text-transform: uppercase; }
    pre { margin: 0; padding: 24px; overflow: auto; color: #d8e4f0; font: 14px/1.65 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
    footer { display: flex; gap: 20px; margin-top: 24px; color: #8fa2b7; }
    a { color: #60a5fa; }
  </style>
</head>
<body>
  <main>
    <p class="eyebrow">recorder / release evidence</p>
    <h1>Public API compatibility</h1>
    <p class="lead">Generated from the pinned <code>golang.org/x/exp/apidiff</code> tool against the latest root and OpenTelemetry release tags. CI rejects every incompatible change not present in the explicitly reviewed pre-v1 exception set.</p>
    <section class="status">
      <h2>$status_heading</h2>
      <p>$status_detail</p>
    </section>
    <section class="report">
      <header>apidiff report</header>
      <pre>$report_html</pre>
    </section>
    <footer><a href="report.md">Markdown report</a><a href="../">Open Inspector</a></footer>
  </main>
</body>
</html>
EOF
}

compare_package() {
	local package="$1"
	local old_root="$2"
	local old_relative="$3"
	local current_root="$4"

	if ! has_package "$old_root/$old_relative"; then
		printf '## %s\n\nCompatible change: package added after the baseline release.\n\n' \
			"$package" >>"$tmp_dir/report.md"

		return
	fi

	(
		cd "$old_root"
		"$apidiff" -w "$tmp_dir/old.api" "$package"
	)
	(
		cd "$current_root"
		"$apidiff" -w "$tmp_dir/new.api" "$package"
	)

	append_report "$package" "$tmp_dir/old.api" "$tmp_dir/new.api"
}

if [[ ! -x "$apidiff" ]]; then
	echo "apidiff binary not found: $apidiff" >&2
	exit 1
fi

root_baseline="${API_BASELINE:-$(latest_tag 'v[0-9]*')}"
otel_baseline="${OTEL_API_BASELINE:-$(latest_tag 'otelrecorder/v[0-9]*')}"

if [[ -z "$root_baseline" || -z "$otel_baseline" ]]; then
	echo "root and otelrecorder release tags are required for API comparison" >&2
	exit 1
fi

root_old="$(module_dir "github.com/mgurevin/recorder@$root_baseline")"
otel_version="${otel_baseline#otelrecorder/}"
otel_old="$(module_dir "github.com/mgurevin/recorder/otelrecorder@$otel_version")"

cat >"$tmp_dir/report.md" <<EOF
# Public API compatibility

Root baseline: \`$root_baseline\`

OpenTelemetry baseline: \`$otel_baseline\`

EOF

: >"$tmp_dir/incompatible.txt"

compare_package "github.com/mgurevin/recorder" "$root_old" "." "$root_dir"
compare_package "github.com/mgurevin/recorder/hario" "$root_old" "hario" "$root_dir"
compare_package "github.com/mgurevin/recorder/hartest" "$root_old" "hartest" "$root_dir"
compare_package \
	"github.com/mgurevin/recorder/otelrecorder" \
	"$otel_old" \
	"." \
	"$root_dir/otelrecorder"

sed '/^[[:space:]]*#/d; /^[[:space:]]*$/d' "$exceptions_file" \
	>"$tmp_dir/expected-incompatible.txt"

cat "$tmp_dir/report.md"

if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
	cat "$tmp_dir/report.md" >>"$GITHUB_STEP_SUMMARY"
fi

write_report_assets

if ! diff -u "$tmp_dir/expected-incompatible.txt" "$tmp_dir/incompatible.txt"; then
	echo >&2
	echo "unreviewed public API incompatibility detected" >&2
	echo "review the report; pre-v1 exceptions must be explicit and temporary" >&2
	exit 1
fi

echo "Public API incompatibilities match the reviewed exception set."
