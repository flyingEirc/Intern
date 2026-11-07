#!/usr/bin/env bash
# Accept and reject cases for e2e-stack-clean.sh. A fake docker keeps three
# tables (containers, networks, volumes; one "id label project" row each, the
# label being compose, e2e or -) and answers only the exact commands the script
# is allowed to run, failing on any other command, flag or template. The cases
# therefore prove what the script removes and, as important, what it never
# touches: another runner's stacks, a local run's stack, D1 projects, names of
# any other shape, and resources without either label.
#
# CLEAN_SCRIPT points the cases at another script than the one next to this file.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script=${CLEAN_SCRIPT:-$here/e2e-stack-clean.sh}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin"
export FAKE_DOCKER_STATE=$work/state
mkdir "$FAKE_DOCKER_STATE"

cat > "$work/bin/docker" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
state=$FAKE_DOCKER_STATE
fail() {
	echo "fake docker: $*" >&2
	exit 2
}
case "$1 ${2:-}" in
	'ps --all') table=containers action=list ;;
	'network ls') table=networks action=list ;;
	'volume ls') table=volumes action=list ;;
	'rm --force') table=containers action=remove ;;
	'network rm') table=networks action=remove ;;
	'volume rm') table=volumes action=remove ;;
	*) fail "unsupported command: $*" ;;
esac
shift 2
# FAKE_DOCKER_DOWN is all, or the one table whose listing fails.
if [[ ${FAKE_DOCKER_DOWN:-} == all || ($action == list && ${FAKE_DOCKER_DOWN:-} == "$table") ]]; then
	echo 'Cannot connect to the Docker daemon' >&2
	exit 1
fi
key_of() {
	case $1 in
		com.docker.compose.project) echo compose ;;
		io.intern.e2e.project) echo e2e ;;
		*) fail "unsupported label: $1" ;;
	esac
}
if [[ $action == list ]]; then
	case "$*" in
		'--format {{.Label "'*'"}}')
			name=${2#\{\{.Label \"}
			key=$(key_of "${name%\"\}\}}")
			awk -v key="$key" '{ print ($2 == key ? $3 : "") }' "$state/$table" ;;
		'--quiet --filter label='*)
			[[ $# -eq 3 ]] || fail "unsupported list arguments: $*"
			filter=${3#label=}
			key=$(key_of "${filter%%=*}")
			want=${filter#*=}
			awk -v key="$key" -v want="$want" '$2 == key && $3 == want { print $1 }' "$state/$table" ;;
		*) fail "unsupported list arguments: $*" ;;
	esac
	exit 0
fi
[[ $table != containers || ${1:-} != --volumes ]] || shift
[[ $table != volumes || ${1:-} != --force ]] || shift
status=0
for id in "$@"; do
	[[ $id != -* ]] || fail "unsupported flag: $id"
	if [[ $id == "${FAKE_DOCKER_STUCK:-}" ]]; then
		echo "Error: removal of $id failed" >&2
		status=1
		continue
	fi
	{ grep -v "^$id " "$state/$table" || true; } > "$state/$table.new"
	mv "$state/$table.new" "$state/$table"
done
exit "$status"
FAKE
chmod +x "$work/bin/docker"

cases=0
failed=0
status=0
log=''

tag() { printf '%s' "$1" | sha256sum | cut -c1-8; }
mine=$(tag JackVance)
other=$(tag OtherRunner)

# seed ID:LABEL:PROJECT... resets the tables. The first letter of an id names its
# table: c container, n network, v volume. LABEL is compose
# (com.docker.compose.project), e2e (io.intern.e2e.project) or empty for a
# resource without either label.
seed() {
	local entry id rest label project table
	: > "$FAKE_DOCKER_STATE/containers"
	: > "$FAKE_DOCKER_STATE/networks"
	: > "$FAKE_DOCKER_STATE/volumes"
	for entry in "$@"; do
		id=${entry%%:*}
		rest=${entry#*:}
		label=${rest%%:*}
		project=${rest#*:}
		case ${id:0:1} in
			c) table=containers ;;
			n) table=networks ;;
			v) table=volumes ;;
		esac
		printf '%s %s %s\n' "$id" "${label:--}" "${project:--}" >> "$FAKE_DOCKER_STATE/$table"
	done
}

# clean RUNNER runs the script as the job does, with the fake docker first on
# PATH, and sets status and log. An empty RUNNER leaves RUNNER_NAME unset.
clean() {
	local runner=$1
	if log=$(
		if [[ -n $runner ]]; then export RUNNER_NAME=$runner; else unset RUNNER_NAME; fi
		PATH="$work/bin:$PATH" "$script" 2>&1
	); then
		status=0
	else
		status=$?
	fi
}

# left prints every id still in the tables, sorted on one line.
left() {
	cat "$FAKE_DOCKER_STATE/containers" "$FAKE_DOCKER_STATE/networks" "$FAKE_DOCKER_STATE/volumes" | awk '{ print $1 }' | LC_ALL=C sort | tr '\n' ' '
}

# expect NAME STATUS LEFT [LOG-PATTERN] compares the last clean call: the exit
# status, the ids that must remain, and optionally a pattern the log must match.
expect() {
	local name=$1 want_status=$2 want_left=$3 pattern=${4:-} got problem=''
	cases=$((cases + 1))
	got=$(left)
	if [[ $status -ne $want_status ]]; then
		problem="exit status $status, want $want_status"
	elif [[ $got != "$want_left " ]]; then
		problem="left [${got% }], want [$want_left]"
	elif [[ -n $pattern && ! $log =~ $pattern ]]; then
		problem="the log does not match: $pattern"
	fi
	if [[ -z $problem ]]; then
		printf 'ok   %s\n' "$name"
	else
		printf 'FAIL %s: %s\n%s\n' "$name" "$problem" "$log"
		failed=$((failed + 1))
	fi
}

runs=intern-e2e-$mine-1a2b3c4d
stale=intern-e2e-$mine-0badcafe
foreign=intern-e2e-$other-1a2b3c4d
local_run=intern-e2e-1a2b3c4d

# 1. Everything of this runner's stacks goes, whatever kind of resource it is and
# whichever label names it; nothing else does.
seed "c1:compose:$runs" "c2:compose:$runs" "c3:e2e:$runs" "c4:e2e:$stale" "n1:compose:$runs" "n2:compose:$stale" \
	"v1:compose:$runs" "v2:e2e:$runs" "v3:e2e:$stale" \
	"cx1:compose:$foreign" "cx2:e2e:$foreign" "cx3::" "cx4:compose:$local_run" "cx5:e2e:$local_run" \
	"cx6:compose:intern-e2e-$mine-NOTHEX00" "cx7:e2e:intern-e2e-$mine-1a2b3c" "cx8:e2e:intern-e2e-$mine-1a2b3c4d5e" \
	"cx9:compose:xintern-e2e-$mine-1a2b3c4d" "cx10:compose:d1${mine}18a1b2c3d4e5f607" "cx11:compose:intern-e2e-$mine" \
	"nx1:compose:$foreign" "nx2:compose:intern" "vx1:e2e:$foreign" "vx2:compose:$local_run" "vx3::"
clean JackVance
expect '1 this runner stacks removed under both labels, all others kept' 0 \
	'cx1 cx10 cx11 cx2 cx3 cx4 cx5 cx6 cx7 cx8 cx9 nx1 nx2 vx1 vx2 vx3' "removing e2e stack $runs"

# 2. A stack whose Compose resources are gone still owns its labelled volumes.
seed "v1:e2e:$runs" "v2:e2e:$runs" "vx1::"
clean JackVance
expect '2 labelled volumes of a stack without Compose resources' 0 'vx1'

# 3. The same host looks different to another runner: it removes its own.
seed "c1:compose:$runs" "c2:compose:$foreign" "v1:e2e:$foreign" "v2:e2e:$runs"
clean OtherRunner
expect '3 another runner removes only its own stacks' 0 'c1 v2'

# 4. Nothing to remove is a success with a log line, and touches nothing.
seed "cx1:compose:$local_run" "vx1:e2e:$foreign"
clean JackVance
expect '4 no e2e stack of this runner' 0 'cx1 vx1' 'no e2e stack of runner JackVance'

# 5. A container that refuses removal does not keep the rest on the host, and
# the script reports the failure.
seed "c1:compose:$runs" "c2:e2e:$runs" "n1:compose:$runs" "v1:e2e:$runs" "cx1:compose:$local_run"
FAKE_DOCKER_STUCK=c1 clean JackVance
expect '5 a stuck container is reported, the rest still goes' 1 'c1 cx1'

# 6. An unreachable engine must not read as an empty host, whichever listing
# notices it first.
for down in all containers networks volumes; do
	seed "c1:compose:$runs" "n1:compose:$runs" "v1:e2e:$runs"
	FAKE_DOCKER_DOWN=$down clean JackVance
	expect "6 docker unreachable ($down)" 1 'c1 n1 v1'
done

# 7. Outside a job there is no runner, so there is no scope: refuse.
seed "c1:compose:$runs"
clean ''
expect '7 RUNNER_NAME unset' 1 'c1' 'RUNNER_NAME'

# 8. The scope key is the sha256 of the exact name bytes.
seed "c1:compose:intern-e2e-$(tag 'runner one')-00000001" "c2:compose:intern-e2e-$(tag 'Runner one')-00000001"
clean 'runner one'
expect '8 runner names differing in case are different runners' 0 'c2'

printf '\n%s cases, %s failed\n' "$cases" "$failed"
[[ $failed -eq 0 ]]
