#!/usr/bin/env bash
# Accept and reject cases for d1-compose-clean.sh. A fake docker keeps three
# tables (containers, networks, volumes; one "id project" row each) and answers
# only the exact commands the script is allowed to run, failing on any other
# command, flag or template. The cases therefore prove what the script removes
# and, as important, what it never touches: another runner's D1 projects, a
# project of any other name, and resources that belong to no project.
#
# CLEAN_SCRIPT points the cases at another script than the one next to this file.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script=${CLEAN_SCRIPT:-$here/d1-compose-clean.sh}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin"
export FAKE_DOCKER_STATE=$work/state
mkdir "$FAKE_DOCKER_STATE"

cat > "$work/bin/docker" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
state=$FAKE_DOCKER_STATE
label='{{.Label "com.docker.compose.project"}}'
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
if [[ $action == list ]]; then
	case "$*" in
		"--format $label") awk '{ print $2 }' "$state/$table" ;;
		'--quiet --filter label=com.docker.compose.project='*)
			want=${3#label=com.docker.compose.project=}
			awk -v want="$want" '$2 == want { print $1 }' "$state/$table" ;;
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

# seed ID:PROJECT... resets the tables. The first letter of an id names its
# table: c container, n network, v volume. An empty project is a resource that
# belongs to no Compose project.
seed() {
	local entry
	: > "$FAKE_DOCKER_STATE/containers"
	: > "$FAKE_DOCKER_STATE/networks"
	: > "$FAKE_DOCKER_STATE/volumes"
	for entry in "$@"; do
		case ${entry:0:1} in
			c) printf '%s %s\n' "${entry%%:*}" "${entry#*:}" >> "$FAKE_DOCKER_STATE/containers" ;;
			n) printf '%s %s\n' "${entry%%:*}" "${entry#*:}" >> "$FAKE_DOCKER_STATE/networks" ;;
			v) printf '%s %s\n' "${entry%%:*}" "${entry#*:}" >> "$FAKE_DOCKER_STATE/volumes" ;;
		esac
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

runs=$(printf 'd1%s18a1b2c3d4e5f607' "$mine")
stale=$(printf 'd1%s0123abcdef' "$mine")
foreign=$(printf 'd1%s18a1b2c3d4e5f607' "$other")

# 1. Everything of this runner's D1 projects goes, whatever kind of resource it
# is; nothing else does.
seed "c1:$runs" "c2:$runs" "c3:$stale" "n1:$runs" "n2:$stale" "v1:$runs" "v2:$runs" "v3:$stale" \
	"cx1:$foreign" "cx2:intern" "cx3:" "cx4:d1${mine}NOTHEX" "cx5:d118a1b2c3d4e5f607" "cx6:xd1${mine}abc" "cx7:d1${mine}" \
	"nx1:$foreign" "nx2:intern" "vx1:$foreign" "vx2:intern" "vx3:"
clean JackVance
expect '1 this runner d1 projects removed, all others kept' 0 'cx1 cx2 cx3 cx4 cx5 cx6 cx7 nx1 nx2 vx1 vx2 vx3' 'removing Compose project d1'"$mine"

# 2. A project that lost its containers still owns a network and volumes.
seed "n1:$runs" "v1:$runs" "vx1:intern"
clean JackVance
expect '2 network and volumes of a project without containers' 0 'vx1'

# 3. The same host looks different to another runner: it removes its own.
seed "c1:$runs" "c2:$foreign" "n1:$foreign"
clean OtherRunner
expect '3 another runner removes only its own projects' 0 'c1'

# 4. Nothing to remove is a success with a log line, and touches nothing.
seed "cx1:intern" "vx1:$foreign"
clean JackVance
expect '4 no D1 project of this runner' 0 'cx1 vx1' 'no D1 Compose project of runner JackVance'

# 5. A container that refuses removal does not keep the rest on the host, and
# the script reports the failure.
seed "c1:$runs" "c2:$runs" "n1:$runs" "v1:$runs" "cx1:intern"
FAKE_DOCKER_STUCK=c1 clean JackVance
expect '5 a stuck container is reported, the rest still goes' 1 'c1 cx1'

# 6. An unreachable engine must not read as an empty host, whichever listing
# notices it first.
for down in all containers networks volumes; do
	seed "c1:$runs" "n1:$runs" "v1:$runs"
	FAKE_DOCKER_DOWN=$down clean JackVance
	expect "6 docker unreachable ($down)" 1 'c1 n1 v1' 'Cannot connect to the Docker daemon'
done

# 7. Outside a job there is no runner, so there is no scope: refuse.
seed "c1:$runs"
clean ''
expect '7 RUNNER_NAME unset' 1 'c1' 'RUNNER_NAME'

# 8. The scope key is the sha256 of the exact name bytes.
seed "c1:d1$(tag 'runner one')1" "c2:d1$(tag 'Runner one')1"
clean 'runner one'
expect '8 runner names differing in case are different runners' 0 'c2'

printf '\n%s cases, %s failed\n' "$cases" "$failed"
[[ $failed -eq 0 ]]
