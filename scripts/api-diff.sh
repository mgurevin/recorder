#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
apidiff="${APIDIFF:-"$root_dir/build/tools/apidiff"}"
exceptions_file="${API_DIFF_EXCEPTIONS:-"$root_dir/api/compatibility-exceptions.txt"}"
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

	go mod download -json "$module" |
		sed -n 's/^[[:space:]]*"Dir": "\(.*\)",$/\1/p'
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

if ! diff -u "$tmp_dir/expected-incompatible.txt" "$tmp_dir/incompatible.txt"; then
	echo >&2
	echo "unreviewed public API incompatibility detected" >&2
	echo "review the report; pre-v1 exceptions must be explicit and temporary" >&2
	exit 1
fi

echo "Public API incompatibilities match the reviewed exception set."
