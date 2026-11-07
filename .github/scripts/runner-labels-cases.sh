#!/usr/bin/env bash
# Accept and reject cases for the single runner label set of quality.yml.
#
# Every job of the only CI workflow must select the same WSL2 self-hosted
# instances. The three default labels (self-hosted, Linux, X64) are not specific
# enough on this host: the Windows runner registered here also carries
# self-hosted and X64, and a plain Linux runner with no native Docker engine
# would match all three. `docker` and `wsl` are the custom labels of the WSL2
# instances configured for this harness, so only those can take a job. One job
# on the wrong runner fails the real-component gates or hangs until its timeout,
# and a half-updated label set is easy to miss in a 700-line workflow.
#
# This script is the single source of that contract. It reads the committed
# workflow (or QUALITY_WORKFLOW) and requires every runs-on line outside a
# comment to be byte-identical to the expected job-level line, with exactly as
# many lines as the workflow has jobs. The reject cases mutate a copy of the
# real workflow and assert that each mutation changed its input, so a change in
# the workflow's shape cannot make this check pass vacuously.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
workflow=${QUALITY_WORKFLOW:-$root/.github/workflows/quality.yml}

expected_line='    runs-on: [self-hosted, Linux, X64, docker, wsl]'
expected_count=14

cases=0 failed=0
report() {
	local label=$1 problem=${2:-}
	cases=$((cases + 1))
	if [[ -n $problem ]]; then
		failed=$((failed + 1))
		printf 'not ok %s: %s\n' "$label" "$problem"
	else
		printf 'ok   %s\n' "$label"
	fi
}

# verify prints the first problem and returns 1, or prints nothing and returns 0.
# A commented-out line is not a job's runs-on; every other occurrence, at any
# indentation, must be exactly the expected job-level line.
verify() {
	local file=$1 entry line count=0
	local lines=()
	if [[ ! -f $file ]]; then
		printf 'no workflow at %s\n' "$file"
		return 1
	fi
	mapfile -t lines < <(grep -n 'runs-on:' "$file" || true)
	for entry in "${lines[@]}"; do
		line=${entry#*:}
		[[ $line =~ ^[[:space:]]*# ]] && continue
		count=$((count + 1))
		if [[ $line != "$expected_line" ]]; then
			printf 'line %s is "%s", want "%s"\n' "${entry%%:*}" "$line" "$expected_line"
			return 1
		fi
	done
	if ((count != expected_count)); then
		printf 'found %s runs-on lines, want exactly %s (the number of jobs)\n' "$count" "$expected_count"
		return 1
	fi
	return 0
}

expect_ok() {
	local label=$1 file=$2 problem=''
	problem=$(verify "$file") || true
	report "$label" "$problem"
}

expect_bad() {
	local label=$1 file=$2
	if verify "$file" >/dev/null 2>&1; then
		report "$label" 'the mutated workflow was accepted'
	else
		report "$label"
	fi
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# mutant NAME AWK-PROGRAM prints the path of a copy of the real workflow with
# one mutation applied, and fails if the mutation did not change anything.
mutant() {
	local name=$1 program=$2 out="$work/$1.yml"
	awk "$program" "$workflow" >"$out" || return 1
	if cmp -s "$workflow" "$out"; then
		printf 'runner-labels-cases: mutation %s changed nothing\n' "$name" >&2
		exit 1
	fi
	printf '%s\n' "$out"
}

first_only='BEGIN { done = 0 } /^    runs-on:/ && !done { %s done = 1; next } { print }'

expect_ok 'the committed workflow' "$workflow"

expect_bad 'a job drops the docker label' \
	"$(mutant missing-docker "$(printf "$first_only" 'print "    runs-on: [self-hosted, Linux, X64, docker]";')")"

expect_bad 'a job targets the Windows runner' \
	"$(mutant windows "$(printf "$first_only" 'print "    runs-on: [self-hosted, Windows, X64]";')")"

expect_bad 'a job loses its runs-on' \
	"$(mutant removed "$(printf "$first_only" '')")"

expect_bad 'a runs-on is duplicated' \
	"$(mutant duplicated 'BEGIN { done = 0 } /^    runs-on:/ && !done { print; print; done = 1; next } { print }')"

expect_bad 'a job uses an expression instead of the literal label set' \
	"$(mutant expression "$(printf "$first_only" 'print "    runs-on: ${{ matrix.runner }}";')")"

expect_bad 'a runs-on sits at step indentation' \
	"$(mutant indented "$(printf "$first_only" 'print "      runs-on: [self-hosted, Linux, X64, docker, wsl]";')")"

expect_bad 'a runs-on is commented out' \
	"$(mutant commented "$(printf "$first_only" 'print "    # runs-on: [self-hosted, Linux, X64, docker, wsl]";')")"

expect_bad 'the workflow is missing' "$work/absent.yml"

printf '\n%s cases, %s failed\n' "$cases" "$failed"
[[ $failed -eq 0 ]]
