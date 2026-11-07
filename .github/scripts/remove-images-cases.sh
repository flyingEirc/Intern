#!/usr/bin/env bash
# Accept and reject cases for remove-images.sh. A fake docker keeps one table of
# images (id, parent, number of digest references, comma separated tags) and
# answers only the exact commands and templates the script is allowed to run,
# failing on any other command, flag or template. The cases therefore prove what
# the script removes (the image and the ancestors only it needs) and, as
# important, what it never touches: an ancestor another image still builds on,
# a tagged or digest-referenced base image, an image a container uses, and an
# image another job already removed. They also prove that it never asks the
# daemon to prune.
#
# The last section generates mutants of the script and requires the cases to
# fail on each: a mutant that survives means the cases no longer protect that
# decision.
#
# REMOVE_SCRIPT points the cases at another script than the one next to this file.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script=${REMOVE_SCRIPT:-$here/remove-images.sh}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin" "$work/state"
export FAKE_DOCKER_STATE=$work/state
: > "$FAKE_DOCKER_STATE/images"

cat > "$work/bin/docker" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
state=$FAKE_DOCKER_STATE
fail() {
	echo "fake docker: $*" >&2
	exit 2
}
id_format='{{.Id}}'
graph_format='{{.Id}} {{with index . "Parent"}}{{.}}{{else}}-{{end}} {{with index . "RepoTags"}}{{len .}}{{else}}0{{end}} {{with index . "RepoDigests"}}{{len .}}{{else}}0{{end}}'
if [[ ${FAKE_DOCKER_DOWN:-} == all ]]; then
	echo 'Cannot connect to the Docker daemon' >&2
	exit 1
fi
[[ ${1:-} == image ]] || fail "unsupported command: $*"
sub=${2:-}
shift 2

# resolve TARGET prints the id of the image TARGET names (an id or a tag).
resolve() {
	awk -v target="$1" '
		$1 == target { print $1; found = 1; exit }
		{ n = split($4, refs, ","); for (i = 1; i <= n; i++) if (refs[i] == target) { print $1; found = 1; exit } }
		END { exit !found }' "$state/images"
}
missing() { echo "Error response from daemon: No such image: $1" >&2; }

case $sub in
ls)
	[[ $* == '--all --quiet --no-trunc' ]] || fail "unsupported list arguments: $*"
	awk '{ n = ($4 == "-") ? 1 : split($4, refs, ","); for (i = 0; i < n; i++) print $1 }' "$state/images"
	;;
inspect)
	[[ ${1:-} == --format ]] || fail "missing --format: $*"
	format=$2
	shift 2
	status=0
	for target in "$@"; do
		if [[ $target == "${FAKE_DOCKER_VANISH:-}" ]] || ! id=$(resolve "$target"); then
			missing "$target"
			status=1
			continue
		fi
		case $format in
		"$id_format") echo "$id" ;;
		"$graph_format")
			[[ ${FAKE_DOCKER_DOWN:-} != graph ]] || { echo 'daemon error: graph unavailable' >&2; exit 1; }
			awk -v id="$id" '$1 == id { n = ($4 == "-") ? 0 : split($4, refs, ","); print $1, $2, n, $3 }' "$state/images"
			;;
		*) fail "unsupported template: $format" ;;
		esac
	done
	exit "$status"
	;;
rm)
	[[ ${1:-} == --no-prune ]] || fail "docker image rm without --no-prune would prune shared ancestors: $*"
	shift
	[[ $# -eq 1 && $1 != -* ]] || fail "unsupported remove arguments: $*"
	target=$1
	if ! id=$(resolve "$target"); then
		missing "$target"
		exit 1
	fi
	if [[ $target == "${FAKE_DOCKER_STUCK:-}" || $id == "${FAKE_DOCKER_STUCK:-}" ]]; then
		echo "Error response from daemon: removal of $target failed" >&2
		exit 1
	fi
	for used in ${FAKE_DOCKER_USED:-}; do
		if [[ $used == "$id" ]]; then
			echo "Error response from daemon: conflict: unable to delete $id (cannot be forced) - image is being used by running container 0123456789ab" >&2
			exit 1
		fi
	done
	# Another tag of the same image: only this tag goes.
	awk -v id="$id" -v target="$target" '
		$1 != id { print; next }
		{
			n = split($4, refs, ","); kept = ""
			for (i = 1; i <= n; i++) if (refs[i] != target && refs[i] != "-") kept = kept (kept == "" ? "" : ",") refs[i]
			if (target != id && kept != "") print $1, $2, $3, kept
		}' "$state/images" > "$state/images.new"
	mv "$state/images.new" "$state/images"
	;;
*) fail "unsupported image command: $sub" ;;
esac
FAKE
chmod +x "$work/bin/docker"

checks=0
failed=0
status=0
log=''

# seed ROW... resets the image table. A row is ID:PARENT:DIGESTS:TAGS; PARENT is
# - for none, TAGS is a comma separated list or - for an untagged image.
seed() {
	local row id parent digests tags
	: > "$FAKE_DOCKER_STATE/images"
	for row in "$@"; do
		# The last field keeps the colons of repo:tag.
		IFS=: read -r id parent digests tags <<< "$row"
		printf '%s %s %s %s\n' "$id" "$parent" "$digests" "$tags" >> "$FAKE_DOCKER_STATE/images"
	done
}

# remove ARGS... runs the script with the fake docker first on PATH and sets
# status and log.
remove() {
	if log=$(PATH="$work/bin:$PATH" "$script" "$@" 2>&1); then
		status=0
	else
		status=$?
	fi
}

left() {
	awk '{ print $1 }' "$FAKE_DOCKER_STATE/images" | LC_ALL=C sort | tr '\n' ' '
}
tags_of() {
	awk -v id="$1" '$1 == id { print $4 }' "$FAKE_DOCKER_STATE/images"
}

# expect NAME STATUS LEFT [LOG-PATTERN] compares the last remove call: the exit
# status, the ids that must remain (sorted, space separated) and optionally a
# pattern the log must match.
expect() {
	local name=$1 want_status=$2 want_left=$3 pattern=${4:-} got problem=''
	checks=$((checks + 1))
	got=$(left)
	if [[ $status -ne $want_status ]]; then
		problem="exit status $status, want $want_status"
	elif [[ $got != "$want_left " && ! ( -z $want_left && -z $got ) ]]; then
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

# base is a pulled base image; a1..a3 are build steps on top of it, nearest the
# base first; f is the final image carrying the tag.
chain=(
	"base:-:0:alpine:3.24"
	"a1:base:0:-"
	"a2:a1:0:-"
	"a3:a2:0:-"
	"f:a3:0:repo:fixture-1"
)

# 1. The image and every ancestor only it needs go; the tagged base stays.
seed "${chain[@]}"
remove repo:fixture-1
expect '1 image and its private ancestors removed, base kept' 0 'base' 'removed ancestor a3 .*removed ancestor a2 .*removed ancestor a1 '

# 2. An ancestor another image builds on stays, with everything below it, and
# the other image keeps its whole chain.
seed "base:-:0:alpine:3.24" "p1:base:0:-" "p2:p1:0:-" "x1:p2:0:-" "x:x1:0:repo:x" "y1:p2:0:-" "y:y1:0:repo:y"
remove repo:x
expect '2 shared ancestor and the other chain kept' 0 'base p1 p2 y y1' 'removed ancestor x1 '

# 3. Once the other chain is gone too, the shared ancestors are unshared.
remove repo:y
expect '3 shared ancestors go with the last image that needs them' 0 'base' 'removed ancestor p2 .*removed ancestor p1 '

# 4. A second tag on the same image keeps it: only the tag goes, nothing above.
seed "base:-:0:alpine:3.24" "a1:base:0:-" "f:a1:0:repo:one,repo:two"
remove repo:one
expect '4 untagging one of two tags keeps the image and its ancestors' 0 'a1 base f' 'untagged repo:one'
[[ $(tags_of f) == repo:two ]] || { printf 'FAIL 4 the other tag must remain on f, got [%s]\n' "$(tags_of f)"; failed=$((failed + 1)); }

# 5. A container uses the image: it stays, quietly, and nothing above it goes.
seed "${chain[@]}"
FAKE_DOCKER_USED=f remove repo:fixture-1
expect '5 an image a container uses is kept without an error' 0 'a1 a2 a3 base f' 'kept repo:fixture-1: a container uses it'

# 6. A container uses an ancestor: the walk stops there, nothing above it goes.
seed "${chain[@]}"
FAKE_DOCKER_USED=a2 remove repo:fixture-1
expect '6 an ancestor in use stops the walk' 0 'a1 a2 base' 'kept ancestor a2: a container uses it'

# 7. An image that is already gone is not an error: other jobs run the same cleanup.
seed "base:-:0:alpine:3.24"
remove repo:fixture-1
expect '7 an image that is already gone' 0 'base' 'repo:fixture-1 is already gone'

# 8. A base image that only has a digest reference is not dangling and stays.
seed "base:-:1:-" "a1:base:0:-" "f:a1:0:repo:fixture-1"
remove repo:fixture-1
expect '8 a digest-referenced base stays' 0 'base' 'removed ancestor a1 '

# 9. An unreachable engine fails and removes nothing.
seed "${chain[@]}"
FAKE_DOCKER_DOWN=all remove repo:fixture-1
expect '9 docker unreachable' 1 'a1 a2 a3 base f' 'Cannot connect to the Docker daemon'

# 10. No reference is a usage error.
seed "${chain[@]}"
remove
expect '10 no reference' 2 'a1 a2 a3 base f' 'usage'

# 11. An image another job deletes between the listing and the inspection is
# missing from the snapshot, which is harmless.
seed "${chain[@]}" "z:-:0:other:z"
FAKE_DOCKER_VANISH=z remove repo:fixture-1
expect '11 an image vanishing from the snapshot is tolerated' 0 'base z' 'removed ancestor a1 '

# 12. Any other error while reading the images stops that removal.
seed "${chain[@]}"
FAKE_DOCKER_DOWN=graph remove repo:fixture-1
expect '12 an unreadable image graph removes nothing' 1 'a1 a2 a3 base f' 'graph unavailable'

# 13. A failing removal is reported, and the remaining references are still tried.
seed "${chain[@]}" "g:-:0:repo:fixture-2"
FAKE_DOCKER_STUCK=f remove repo:fixture-1 repo:fixture-2
expect '13 a stuck removal fails the run, the next reference still goes' 1 'a1 a2 a3 base f' 'removal of repo:fixture-1 failed'

# 14. Nothing outside the removed chain is touched.
seed "${chain[@]}" "other:-:0:other:keep" "o1:other:0:-" "o2:o1:0:other:keep2"
remove repo:fixture-1
expect '14 unrelated images stay' 0 'base o1 o2 other' 'removed repo:fixture-1'

# 15. Mutants: each one must fail the case that protects its decision, with the
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
	if REMOVE_SCRIPT=$work/mutant.sh REMOVE_CASES_MUTANTS=0 "$here/$(basename "$0")" > "$work/mutant.log" 2>&1; then
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

if [[ ${REMOVE_CASES_MUTANTS:-1} == 1 ]]; then
	mutant 'asks the daemon to prune' 'docker image rm --no-prune' 'docker image rm' \
		'^FAIL 1 .*exit status 1, want 0'
	mutant 'ancestor with other children no longer kept' 'children[current] == 0' 'children[current] >= 0' \
		'^FAIL 2 shared ancestor and the other chain kept'
	mutant 'tags and digests no longer protect an ancestor' 'refs[current] == 0' 'refs[current] >= 0' \
		'^FAIL (1|8) '
	mutant 'a container using the image becomes an error' 'return 3' 'return 1' \
		'^FAIL 5 an image a container uses'
	mutant 'an image that is already gone becomes an error' "'No such image'" "'No such imageX'" \
		'^FAIL (7|11) '
	mutant 'ancestors of an image that stays are removed' 'if docker image inspect --format "$id_format" "$id" >/dev/null 2>&1; then' 'if false; then' \
		'^FAIL 4 untagging'
	mutant 'failures not reported in the exit status' 'status=1' 'status=0' \
		'^FAIL (9|12|13) '
fi

printf '\n%s checks, %s failed\n' "$checks" "$failed"
[[ $failed -eq 0 ]]
