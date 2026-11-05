#!/usr/bin/env bash
# Accept and reject cases for cli-quality-clean.sh. A fake docker keeps four
# tables (containers, networks, volumes, images; one "id runner" row each, one
# image reference per row) and answers only the exact commands the script is
# allowed to run, failing on any other command, flag or template. The cases
# therefore prove what the script removes and, as important, what it never
# touches: another runner's resources, unlabelled resources (including the D1
# Compose resources this script must not know about), candidate images of
# another profile or architecture, and the current commit's candidate image.
# They also prove that removal failures propagate, that a missing RUNNER_NAME
# refuses to run, and that --inventory both passes on a clean host and fails on
# leftovers without removing anything.
#
# The last section generates mutants of the script and requires the cases to
# fail on each: a mutant that survives means the cases no longer protect that
# decision.
#
# CLEAN_SCRIPT points the cases at another script than the one next to this file.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script=${CLEAN_SCRIPT:-$here/cli-quality-clean.sh}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin" "$work/state"
export FAKE_DOCKER_STATE=$work/state
: > "$FAKE_DOCKER_STATE/containers"
: > "$FAKE_DOCKER_STATE/networks"
: > "$FAKE_DOCKER_STATE/volumes"
: > "$FAKE_DOCKER_STATE/images"

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
	'image ls') table=images action=list ;;
	'rm --force') table=containers action=remove ;;
	'network rm') table=networks action=remove ;;
	'volume rm') table=volumes action=remove ;;
	'image rm') table=images action=remove ;;
	*) fail "unsupported command: $*" ;;
esac
shift 2
# FAKE_DOCKER_DOWN is all, or the one table whose listing fails.
if [[ ${FAKE_DOCKER_DOWN:-} == all || ($action == list && ${FAKE_DOCKER_DOWN:-} == "$table") ]]; then
	echo 'Cannot connect to the Docker daemon' >&2
	exit 1
fi
if [[ $action == list ]]; then
	if [[ $table == images ]]; then
		[[ $* == '--format {{.CreatedAt}} {{.Repository}}:{{.Tag}}' ]] || fail "unsupported list arguments: $*"
		cat "$state/images"
		exit 0
	fi
	[[ $* == --quiet\ --filter\ label=intern.quality.runner=* ]] || fail "unsupported list arguments: $*"
	want=${3#label=intern.quality.runner=}
	awk -v want="$want" '$2 == want { print $1 }' "$state/$table"
	exit 0
fi
if [[ $table == containers ]]; then
	[[ ${1:-} == --volumes ]] || fail "missing --volumes: $*"
	shift
fi
status=0
for id in "$@"; do
	[[ $id != -* ]] || fail "unsupported flag: $id"
	if [[ $id == "${FAKE_DOCKER_STUCK:-}" ]]; then
		echo "Error: removal of $id failed" >&2
		status=1
		continue
	fi
	if [[ $table == images ]]; then
		awk -v ref="$id" '$NF != ref' "$state/images" > "$state/images.new"
	else
		awk -v id="$id" '$1 != id' "$state/$table" > "$state/$table.new"
	fi
	mv "$state/$table.new" "$state/$table"
done
exit "$status"
FAKE
chmod +x "$work/bin/docker"

checks=0
failed=0
status=0
log=''

tag() { printf '%s' "$1" | sha256sum | cut -c1-8; }
mine=$(tag JackVance)
other=$(tag OtherRunner)
profile=mysql-9.7.1
arch=amd64
current=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
old=0123456789ab

# seed ID:LABEL... resets the tables. The first letter of an id names its
# table: c container, n network, v volume, i image. An image entry is
# iN:CREATED REF; CREATED may contain spaces, REF is the last field. An empty
# label is a resource no CLI quality label matches (an unlabelled resource, or a
# D1 Compose resource, which this script must never touch).
seed() {
	local entry
	: > "$FAKE_DOCKER_STATE/containers"
	: > "$FAKE_DOCKER_STATE/networks"
	: > "$FAKE_DOCKER_STATE/volumes"
	: > "$FAKE_DOCKER_STATE/images"
	for entry in "$@"; do
		case ${entry:0:1} in
		c) printf '%s %s\n' "${entry%%:*}" "${entry#*:}" >> "$FAKE_DOCKER_STATE/containers" ;;
		n) printf '%s %s\n' "${entry%%:*}" "${entry#*:}" >> "$FAKE_DOCKER_STATE/networks" ;;
		v) printf '%s %s\n' "${entry%%:*}" "${entry#*:}" >> "$FAKE_DOCKER_STATE/volumes" ;;
		i) printf '%s\n' "${entry#*:}" >> "$FAKE_DOCKER_STATE/images" ;;
		esac
	done
}

# clean RUNNER [ARGS...] runs the script as the job does, with the fake docker
# first on PATH, and sets status and log. An empty RUNNER leaves RUNNER_NAME
# unset. The scope variables come from the case globals so a case can remove
# one of them.
clean() {
	local runner=$1
	shift
	if log=$(
		if [[ -n $runner ]]; then export RUNNER_NAME=$runner; else unset RUNNER_NAME; fi
		if [[ -n $sha ]]; then export GITHUB_SHA=$sha; else unset GITHUB_SHA; fi
		if [[ -n $prof ]]; then export INTERN_TEST_PROFILE=$prof; else unset INTERN_TEST_PROFILE; fi
		if [[ -n $plat ]]; then export INTERN_TEST_PLATFORM=$plat; else unset INTERN_TEST_PLATFORM; fi
		PATH="$work/bin:$PATH" "$script" "$@" 2>&1
	); then
		status=0
	else
		status=$?
	fi
}

sha=$current
prof=$profile
plat=linux/$arch

# left prints every remaining resource id, sorted on one line. Images are not
# resources of the inventory, so they have their own view.
left() {
	cat "$FAKE_DOCKER_STATE/containers" "$FAKE_DOCKER_STATE/networks" "$FAKE_DOCKER_STATE/volumes" |
		awk '{ print $1 }' | LC_ALL=C sort | tr '\n' ' '
}
images_left() {
	awk '{ print $NF }' "$FAKE_DOCKER_STATE/images" | LC_ALL=C sort | tr '\n' ' '
}

# expect NAME STATUS LEFT [LOG-PATTERN] compares the last clean call: the exit
# status, the ids that must remain, and optionally a pattern the log must match.
expect() {
	local name=$1 want_status=$2 want_left=$3 pattern=${4:-} got problem=''
	checks=$((checks + 1))
	got=$(left)
	if [[ $status -ne $want_status ]]; then
		problem="exit status $status, want $want_status"
	elif [[ -z $want_left && -n $got ]] || [[ -n $want_left && $got != "$want_left " ]]; then
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

# expect_images NAME IMAGES compares the remaining image references, ignoring
# order.
expect_images() {
	local name=$1 want=$2 got want_sorted problem=''
	checks=$((checks + 1))
	got=$(images_left)
	if [[ -n $want ]]; then
		want_sorted=$(printf '%s\n' $want | LC_ALL=C sort | tr '\n' ' ')
	else
		want_sorted=''
	fi
	if [[ $got != "$want_sorted" ]]; then
		problem="images [${got% }], want [$want]"
	fi
	if [[ -z $problem ]]; then
		printf 'ok   %s\n' "$name"
	else
		printf 'FAIL %s: %s\n%s\n' "$name" "$problem" "$log"
		failed=$((failed + 1))
	fi
}

# 1. Everything of this runner's CLI quality resources goes, whatever kind of
# resource it is; another runner, unlabelled resources and D1 Compose resources
# (which carry no intern.quality label) stay.
seed "c1:$mine" "c2:$mine" "cx1:$other" "cx2:" "cd1:" \
	"n1:$mine" "nx1:$other" "nx2:" "nd1:" \
	"v1:$mine" "v2:$mine" "vx1:$other" "vx2:" "vd1:"
clean JackVance
expect '1 this runner resources removed, everything else kept' 0 'cd1 cx1 cx2 nd1 nx1 nx2 vd1 vx1 vx2' \
	'found container c1'

# 2. Candidate pruning keeps the newest three images of this profile and the
# current commit's image even though it is older; another profile, another
# architecture, malformed tags, longer tags and unrelated repositories stay.
seed \
	"i1:2023-12-31 00:00:00 +0000 UTC intern-quality:$profile-${current:0:12}-$arch" \
	"i2:2024-01-05 00:00:00 +0000 UTC intern-quality:$profile-111111111111-$arch" \
	"i3:2024-01-04 00:00:00 +0000 UTC intern-quality:$profile-222222222222-$arch" \
	"i4:2024-01-03 00:00:00 +0000 UTC intern-quality:$profile-333333333333-$arch" \
	"i5:2024-01-02 00:00:00 +0000 UTC intern-quality:$profile-444444444444-$arch" \
	"i6:2024-01-01 00:00:00 +0000 UTC intern-quality:$profile-555555555555-$arch" \
	"i7:2024-01-05 00:00:00 +0000 UTC intern-quality:mysql-8.4.11-$old-$arch" \
	"i8:2024-01-05 00:00:00 +0000 UTC intern-quality:$profile-$old-arm64" \
	"i9:2024-01-05 00:00:00 +0000 UTC intern-quality:$profile-$old-$arch-extra" \
	"i10:2024-01-05 00:00:00 +0000 UTC intern-quality:$profile-0123456789AB-$arch" \
	"i11:2024-01-05 00:00:00 +0000 UTC intern-quality:$profile-0123456789a-$arch" \
	"i12:2024-01-05 00:00:00 +0000 UTC other/repository:$old"
clean JackVance
expect '2 newest K and current SHA kept, older candidates removed' 0 '' \
	"removing candidate image intern-quality:$profile-444444444444-$arch"
expect_images '2 candidate image scope (current older)' \
	"intern-quality:$profile-${current:0:12}-$arch intern-quality:$profile-111111111111-$arch intern-quality:$profile-222222222222-$arch intern-quality:$profile-333333333333-$arch intern-quality:mysql-8.4.11-$old-$arch intern-quality:$profile-$old-arm64 intern-quality:$profile-$old-$arch-extra intern-quality:$profile-0123456789AB-$arch intern-quality:$profile-0123456789a-$arch other/repository:$old"

# 2b. When the current commit's image is itself one of the newest images it
# counts toward K, so only three candidates survive.
seed \
	"i1:2024-01-06 00:00:00 +0000 UTC intern-quality:$profile-${current:0:12}-$arch" \
	"i2:2024-01-05 00:00:00 +0000 UTC intern-quality:$profile-111111111111-$arch" \
	"i3:2024-01-04 00:00:00 +0000 UTC intern-quality:$profile-222222222222-$arch" \
	"i4:2024-01-03 00:00:00 +0000 UTC intern-quality:$profile-333333333333-$arch" \
	"i5:2024-01-02 00:00:00 +0000 UTC intern-quality:$profile-444444444444-$arch"
clean JackVance
expect '2b current SHA among the newest K, older candidates removed' 0 '' \
	"removing candidate image intern-quality:$profile-333333333333-$arch"
expect_images '2b candidate image scope (current newest)' \
	"intern-quality:$profile-${current:0:12}-$arch intern-quality:$profile-111111111111-$arch intern-quality:$profile-222222222222-$arch"

# 3. Nothing to remove is a success with a log line, and touches nothing.
seed "cx1:" "vx1:$other"
clean JackVance
expect '3 no CLI quality resource of this runner' 0 'cx1 vx1' 'no CLI quality resources of runner JackVance'

# 4. A container that refuses removal does not keep the rest on the host, and
# the script reports the failure.
seed "c1:$mine" "c2:$mine" "n1:$mine" "v1:$mine"
FAKE_DOCKER_STUCK=c1 clean JackVance
expect '4 a stuck container is reported, the rest still goes' 1 'c1' 'removal of c1 failed'

# 5. An unreachable engine must not read as an empty host, whichever listing
# notices it first, and nothing is removed before all listings succeeded.
for down in all containers networks volumes images; do
	seed "c1:$mine" "n1:$mine" "v1:$mine" "i1:2024-01-01 00:00:00 +0000 UTC intern-quality:$profile-$old-$arch"
	FAKE_DOCKER_DOWN=$down clean JackVance
	expect "5 docker unreachable ($down)" 1 'c1 n1 v1' 'Cannot connect to the Docker daemon'
done

# 6. Outside a job there is no runner, so there is no scope: refuse.
seed "c1:$mine"
clean ''
expect '6 RUNNER_NAME unset' 1 'c1' 'RUNNER_NAME'

# 7. The scope key is the sha256 of the exact name bytes.
seed "c1:$(tag 'runner one')" "c2:$(tag 'Runner one')"
clean 'runner one'
expect '7 runner names differing in case are different runners' 0 'c2'

# 8. --inventory passes on a clean host and removes nothing.
seed "cx1:$other" "vx1:$other"
clean JackVance --inventory
expect '8 inventory on a clean host' 0 'cx1 vx1' 'no CLI quality resource of runner JackVance remains'

# 9. --inventory fails on any labelled leftover, names it, and removes nothing.
seed "c1:$mine" "n1:$mine" "v1:$mine" "cx1:$other"
clean JackVance --inventory
expect '9 inventory fails on leftovers without removing them' 1 'c1 cx1 n1 v1' 'leftover volume v1'

# 10. Without a current commit id the candidate image cannot be protected, so
# no image is removed; the labelled resources still are.
seed "c1:$mine" "i1:2024-01-01 00:00:00 +0000 UTC intern-quality:$profile-$old-$arch"
sha=''
clean JackVance
expect '10 no GITHUB_SHA keeps every candidate image' 0 '' 'candidate images kept'
expect_images '10 old image kept without a current SHA' "intern-quality:$profile-$old-$arch"

# 11. Without a profile the image name cannot be scoped, so no image is
# removed; the labelled resources still are.
seed "c1:$mine" "i1:2024-01-01 00:00:00 +0000 UTC intern-quality:$profile-$old-$arch"
sha=$current
prof=''
clean JackVance
expect '11 no profile keeps every candidate image' 0 '' 'candidate images kept'
expect_images '11 old image kept without a profile' "intern-quality:$profile-$old-$arch"
prof=$profile

# 12. Mutants: each one must fail the case that protects its decision, with the
# expected assertion message. A survivor, or a failure for another reason (a
# crash, an unsupported fake command), means the cases stopped proving that
# decision.
mutant() {
	local name=$1 from=$2 to=$3 expected=$4 content mutated
	checks=$((checks + 1))
	content=$(<"$script")
	if [[ $content != *"$from"* ]]; then
		printf 'FAIL mutant %s: pattern not found: %s\n' "$name" "$from"
		failed=$((failed + 1))
		return
	fi
	mutated=${content//"$from"/"$to"}
	printf '%s\n' "$mutated" > "$work/mutant.sh"
	chmod +x "$work/mutant.sh"
	if CLEAN_SCRIPT=$work/mutant.sh CLEAN_CASES_MUTANTS=0 "$here/$(basename "$0")" > "$work/mutant.log" 2>&1; then
		printf 'FAIL mutant survived: %s\n' "$name"
		failed=$((failed + 1))
	elif grep -Eq "$expected" "$work/mutant.log"; then
		printf 'ok   mutant killed: %s\n' "$name"
	else
		printf 'FAIL mutant %s was not caught by the expected case:\n' "$name"
		tail -n 5 "$work/mutant.log"
		failed=$((failed + 1))
	fi
}

if [[ ${CLEAN_CASES_MUTANTS:-1} == 1 ]]; then
	mutant 'runner label filter removed' 'filter=label=$label=$tag' 'filter=label=$label=' \
		'^FAIL 1 .*: left \['
	mutant 'removal failures ignored' '|| status=1' '|| status=0' \
		'^FAIL 4 .*: exit status 0, want 1'
	mutant 'current SHA no longer protected when older' '[[ $sha == "${current:0:12}" ]]' 'false' \
		'^FAIL 2 candidate image scope .*images \['
	mutant 'newest K ignored' 'keep_candidates=3' 'keep_candidates=0' \
		'^FAIL 2b candidate image scope .*images \['
	mutant 'inventory leftovers ignored' 'if ((leftovers)); then' 'if false; then' \
		'^FAIL 9 .*: exit status 0, want 1'
	mutant 'RUNNER_NAME guard removed' 'runner=${RUNNER_NAME:?' 'runner=${RUNNER_NAME:-' \
		'^FAIL 6 .*: exit status 0, want 1'
fi

printf '\n%s checks, %s failed\n' "$checks" "$failed"
[[ $failed -eq 0 ]]
