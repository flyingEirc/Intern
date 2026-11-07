#!/usr/bin/env bash
# Accept and reject cases for the Web container lanes in quality.yml.
#
# All eleven container jobs used to depend only on scope, so a full run could
# start several jobs building images from the same Dockerfile at once on the
# shared Docker engine (S1/W3/R1 share ftp.Dockerfile, P4/W5/S2 share
# restore.Dockerfile). Three `needs` lanes serialise those builds. A lane must
# stay inside the web tier: container-mysql belongs to the cli tier and never
# joins a lane, otherwise a cli-only run would have a skipped upstream and
# quality-gate would reject it. All four container entry points also wait for
# quality-fast, so golangci-lint never competes with containers for this host's
# 10 GiB (run 42355033628: lint took 25.2 min and timed out with 0 issues while
# the first CLI profile missed its MySQL readiness window). Every web job also
# keeps its own `needs.scope.outputs.web` gate, and quality-gate must still
# depend on every job it validates.
#
# This script is the single source of that contract. Reject cases mutate a copy
# of the real workflow and assert each mutation changed its input.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
workflow=${QUALITY_WORKFLOW:-$root/.github/workflows/quality.yml}

scope='    needs: scope'
fast_barrier='    needs: [scope, quality-fast]'
web_if="    if: needs.scope.outputs.web == 'true'"
cli_if="    if: needs.scope.outputs.cli == 'true'"

order=(quality-fast container-mysql container-physical container-ftp container-web container-replication container-restore container-restore-api container-ftps container-deploy container-dispatch)
declare -A want=(
	[quality-fast]="$scope"
	[container-mysql]="$fast_barrier"
	[container-physical]="$fast_barrier"
	[container-ftp]="$fast_barrier"
	[container-web]='    needs: [scope, container-ftp]'
	[container-replication]='    needs: [scope, container-web]'
	[container-restore]="$fast_barrier"
	[container-restore-api]='    needs: [scope, container-restore]'
	[container-ftps]='    needs: [scope, container-restore-api]'
	[container-deploy]='    needs: [scope, container-physical]'
	[container-dispatch]='    needs: [scope, container-deploy]'
)
web_jobs=(container-physical container-ftp container-web container-replication container-restore container-restore-api container-ftps container-deploy container-dispatch)
gate_jobs=(scope quality-fast container-mysql container-physical container-ftp container-replication container-web container-dispatch container-restore container-restore-api container-deploy container-ftps)

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

# field_of JOB KEY FILE prints the first job-level KEY line of JOB, or nothing.
field_of() {
	local job=$1 key=$2 file=$3
	awk -v want="$job" -v key="    $key:" '
		/^  [a-z][a-z0-9-]+:$/ {
			cur = $1; sub(/:$/, "", cur)
			if (seen && cur != want) exit
			if (cur == want) seen = 1
			next
		}
		seen && index($0, key) == 1 { print; exit }
	' "$file"
}

# max_parallel_of FILE prints container-mysql's strategy.max-parallel line.
max_parallel_of() {
	awk -v want=container-mysql '
		/^  [a-z][a-z0-9-]+:$/ {
			cur = $1; sub(/:$/, "", cur)
			if (seen && cur != want) exit
			if (cur == want) seen = 1
			next
		}
		seen && /^      max-parallel:/ { print; exit }
	' "$1"
}

verify() {
	local file=$1 job got gate
	[[ -f $file ]] || { printf 'no workflow at %s\n' "$file"; return 1; }
	for job in "${order[@]}"; do
		got=$(field_of "$job" needs "$file")
		if [[ $got != "${want[$job]}" ]]; then
			printf '%s: needs is "%s", want "%s"\n' "$job" "$got" "${want[$job]}"
			return 1
		fi
	done
	for job in "${web_jobs[@]}"; do
		got=$(field_of "$job" if "$file")
		if [[ $got != "$web_if" ]]; then
			printf '%s: if is "%s", want "%s"\n' "$job" "$got" "$web_if"
			return 1
		fi
	done
	got=$(field_of container-mysql if "$file")
	if [[ $got != "$cli_if" ]]; then
		printf 'container-mysql: if is "%s", want "%s"\n' "$got" "$cli_if"
		return 1
	fi
	got=$(max_parallel_of "$file")
	if [[ $got != '      max-parallel: 1' ]]; then
		printf 'container-mysql: max-parallel is "%s", want "      max-parallel: 1"\n' "$got"
		return 1
	fi
	gate=$(field_of quality-gate needs "$file")
	for job in "${gate_jobs[@]}"; do
		if [[ $gate != *"$job"* ]]; then
			printf 'quality-gate does not depend on %s\n' "$job"
			return 1
		fi
	done
	return 0
}

expect_ok() {
	local label=$1 file=$2 problem=''
	problem=$(verify "$file") || true
	report "$label" "$problem"
}

expect_bad() {
	local label=$1 file=$2
	if [[ -z $file || ! -f $file ]]; then
		report "$label" 'the mutation produced no workflow (a no-op mutate must not pass)'
	elif verify "$file" >/dev/null 2>&1; then
		report "$label" 'the mutated workflow was accepted'
	else
		report "$label"
	fi
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# mutate NAME JOB FROM TO prints the path of a copy with one job-level line
# replaced, and fails when the mutation changed nothing.
mutate() {
	local name=$1 job=$2 from=$3 to=$4 out="$work/$1.yml"
	awk -v job="$job" -v from="$from" -v to="$to" '
		/^  [a-z][a-z0-9-]+:$/ { cur = $1; sub(/:$/, "", cur); in_job = (cur == job); print; next }
		in_job && $0 == from { print to; in_job = 0; next }
		{ print }
	' "$workflow" >"$out" || return 1
	if cmp -s "$workflow" "$out"; then
		printf 'runner-lanes-cases: mutation %s changed nothing\n' "$name" >&2
		exit 1
	fi
	printf '%s\n' "$out"
}

expect_ok 'the committed workflow' "$workflow"

expect_bad 'a lane is broken (container-web back to needs: scope)' \
	"$(mutate break-lane container-web '    needs: [scope, container-ftp]' "$scope")"

expect_bad 'a lane loses its scope dependency' \
	"$(mutate drop-scope container-replication '    needs: [scope, container-web]' '    needs: container-web')"

expect_bad 'a lane head bypasses the fast-tier barrier' \
	"$(mutate skip-fast container-ftp "$fast_barrier" "$scope")"

expect_bad 'the cli tier is pulled into a web lane' \
	"$(mutate cross-tier container-mysql "$fast_barrier" '    needs: [scope, container-ftp]')"

expect_bad 'a web job gates on its upstream instead of scope' \
	"$(mutate wrong-if container-web "$web_if" "    if: needs.container-ftp.result == 'success'")"

expect_bad 'the cli tier loses its own scope gate' \
	"$(mutate cli-if container-mysql "$cli_if" "$web_if")"

expect_bad 'the cli matrix runs both profiles in parallel' \
	"$(mutate matrix-parallel container-mysql '      max-parallel: 1' '      max-parallel: 2')"

gate_line=$(field_of quality-gate needs "$workflow")
expect_bad 'quality-gate drops one job from its needs list' \
	"$(mutate gate-missing quality-gate "$gate_line" "${gate_line/container-dispatch, /}")"

if verify "$work/absent.yml" >/dev/null 2>&1; then
	report 'the workflow is missing' 'a missing workflow was accepted'
else
	report 'the workflow is missing'
fi

printf '\n%s cases, %s failed\n' "$cases" "$failed"
[[ $failed -eq 0 ]]
