#!/usr/bin/env bash
# Accept and reject cases for web-image-clean.sh. A fake docker keeps one table
# of images (creation time and reference per row) and answers only the exact
# listing the script is allowed to run, failing on any other command, flag or
# template; a stand-in for remove-images.sh records what the script asks to be
# removed (remove-images-cases.sh proves how that script removes), and a direct
# `docker image rm` is an unsupported command. The cases therefore prove which
# fixture images are kept (the newest three of each fixture and architecture)
# and which go, and, as important, what the script never touches: another
# repository, a tag that is not a fixture image tag, another fixture or another
# architecture. They also prove that a removal failure is reported without
# keeping the other images and that an unreachable engine is not an empty host.
#
# The last section generates mutants of the script and requires the cases to
# fail on each: a mutant that survives means the cases no longer protect that
# decision.
#
# CLEAN_SCRIPT points the cases at another script than the one next to this file.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script=${CLEAN_SCRIPT:-$here/web-image-clean.sh}

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
if [[ ${FAKE_DOCKER_DOWN:-} == all ]]; then
	echo 'Cannot connect to the Docker daemon' >&2
	exit 1
fi
[[ ${1:-} == image && ${2:-} == ls ]] || fail "unsupported command: $*"
shift 2
# The daemon applies the reference filter; the script must not rely on it alone.
[[ ${1:-} == --filter && ${2:-} == 'reference=intern-quality-web:*' && ${3:-} == --format && ${4:-} == '{{.CreatedAt}} {{.Repository}}:{{.Tag}}' && $# -eq 4 ]] ||
	fail "unsupported list arguments: $*"
cat "$state/images"
FAKE
chmod +x "$work/bin/docker"

cat > "$work/remove-images.sh" <<'STAND_IN'
#!/usr/bin/env bash
# Stand-in for remove-images.sh: removes each reference from the fake image
# table, and fails for FAKE_IMAGES_STUCK.
set -euo pipefail
state=$FAKE_DOCKER_STATE
status=0
for ref in "$@"; do
	[[ $ref != -* ]] || { echo "stand-in: unsupported argument $ref" >&2; exit 2; }
	if [[ $ref == "${FAKE_IMAGES_STUCK:-}" ]]; then
		echo "Error: removal of $ref failed" >&2
		status=1
		continue
	fi
	awk -v ref="$ref" '$NF != ref' "$state/images" > "$state/images.new"
	mv "$state/images.new" "$state/images"
done
exit "$status"
STAND_IN
chmod +x "$work/remove-images.sh"

checks=0
failed=0
status=0
log=''

repo=intern-quality-web
# image DAY NAME builds one table row for a fixture image tag NAME.
image() { printf '2024-01-%02d 00:00:00 +0000 UTC %s:%s' "$1" "$repo" "$2"; }
hash() { printf '%012x' "$1"; }
seed() { printf '%s\n' "$@" > "$FAKE_DOCKER_STATE/images"; }

clean() {
	if log=$(REMOVE_IMAGES=$work/remove-images.sh PATH="$work/bin:$PATH" "$script" "$@" 2>&1); then
		status=0
	else
		status=$?
	fi
}

left() {
	awk '{ print $NF }' "$FAKE_DOCKER_STATE/images" | LC_ALL=C sort | tr '\n' ' '
}

# expect NAME STATUS LEFT [LOG-PATTERN] compares the last clean call: the exit
# status, the references that must remain (space separated, any order) and
# optionally a pattern the log must match.
expect() {
	local name=$1 want_status=$2 want_left=$3 pattern=${4:-} got want_sorted problem=''
	checks=$((checks + 1))
	got=$(left)
	want_sorted=''
	if [[ -n $want_left ]]; then
		want_sorted=$(printf '%s\n' $want_left | LC_ALL=C sort | tr '\n' ' ')
	fi
	if [[ $status -ne $want_status ]]; then
		problem="exit status $status, want $want_status"
	elif [[ $got != "$want_sorted" ]]; then
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

ref() { printf '%s:%s' "$repo" "$1"; }

# 1. The newest three of each fixture and architecture stay; older ones go, and
# the fixtures and architectures are counted separately.
ftp1=ftp-$(hash 1)-amd64 ftp2=ftp-$(hash 2)-amd64 ftp3=ftp-$(hash 3)-amd64 ftp4=ftp-$(hash 4)-amd64 ftp5=ftp-$(hash 5)-amd64
arm1=ftp-$(hash 6)-arm64 arm2=ftp-$(hash 7)-arm64
tools1=client-tools-$(hash 8)-amd64 tools2=client-tools-$(hash 9)-amd64 tools3=client-tools-$(hash 10)-amd64 tools4=client-tools-$(hash 11)-amd64
seed "$(image 1 "$ftp1")" "$(image 5 "$ftp5")" "$(image 3 "$ftp3")" "$(image 2 "$ftp2")" "$(image 4 "$ftp4")" \
	"$(image 1 "$arm1")" "$(image 2 "$arm2")" \
	"$(image 4 "$tools4")" "$(image 1 "$tools1")" "$(image 3 "$tools3")" "$(image 2 "$tools2")"
clean
expect '1 newest three of each fixture and architecture kept, older removed' 0 \
	"$(ref "$ftp3") $(ref "$ftp4") $(ref "$ftp5") $(ref "$arm1") $(ref "$arm2") $(ref "$tools2") $(ref "$tools3") $(ref "$tools4")" \
	"removing superseded fixture image $(ref "$ftp1").*removing superseded fixture image $(ref "$ftp2")|removing superseded fixture image $(ref "$ftp2").*removing superseded fixture image $(ref "$ftp1")"

# 1b. Inside GitHub Actions a tag ends in the runner's digest prefix, and every
# runner keeps its own newest three: one runner's images never count against
# another's, and the unscoped tags of a local run are a group of their own.
run_a=aaaaaaaa
run_b=bbbbbbbb
seed "$(image 1 "ftp-$(hash 1)-amd64-$run_a")" "$(image 2 "ftp-$(hash 2)-amd64-$run_a")" "$(image 3 "ftp-$(hash 3)-amd64-$run_a")" "$(image 4 "ftp-$(hash 4)-amd64-$run_a")" \
	"$(image 1 "ftp-$(hash 5)-amd64-$run_b")" "$(image 2 "ftp-$(hash 6)-amd64-$run_b")" \
	"$(image 1 "ftp-$(hash 7)-amd64")" "$(image 2 "ftp-$(hash 8)-amd64")"
clean
expect '1b every runner keeps its own newest three' 0 \
	"$(ref "ftp-$(hash 2)-amd64-$run_a") $(ref "ftp-$(hash 3)-amd64-$run_a") $(ref "ftp-$(hash 4)-amd64-$run_a") $(ref "ftp-$(hash 5)-amd64-$run_b") $(ref "ftp-$(hash 6)-amd64-$run_b") $(ref "ftp-$(hash 7)-amd64") $(ref "ftp-$(hash 8)-amd64")" \
	"removing superseded fixture image $(ref "ftp-$(hash 1)-amd64-$run_a")"

# 2. Three or fewer images of a fixture are all kept.
seed "$(image 1 "$ftp1")" "$(image 2 "$ftp2")" "$(image 3 "$ftp3")"
clean
expect '2 at most three images are all kept' 0 "$(ref "$ftp1") $(ref "$ftp2") $(ref "$ftp3")" 'keeping fixture image'

# 3. Another repository and tags that are not fixture image tags are never
# touched, however many there are and however old: five of each wrong shape (an
# upper case hash, an eleven digit hash, an unsupported architecture, a suffix)
# would be superseded if they were counted as fixture images.
other=other/repository
junk=()
junk_old='2023-12-31 00:00:00 +0000 UTC'
for i in 1 2 3 4 5; do
	junk+=("$junk_old $other:ftp-$(hash "$i")-amd64"
		"$junk_old $repo:ftp-$(printf '%011xA' "$i")-amd64"
		"$junk_old $repo:ftp-$(printf '%011x' "$i")-amd64"
		"$junk_old $repo:ftp-$(hash "$i")-s390x"
		"$junk_old $repo:ftp-$(hash "$i")-amd64-extra"
		"$junk_old $repo:-$(hash "$i")-amd64")
done
junk+=("$junk_old $repo:latest")
seed "$(image 1 "$ftp1")" "$(image 2 "$ftp2")" "$(image 3 "$ftp3")" "$(image 4 "$ftp4")" "${junk[@]}"
clean
expected="$(ref "$ftp2") $(ref "$ftp3") $(ref "$ftp4") $repo:latest"
for i in 1 2 3 4 5; do
	expected="$expected $other:ftp-$(hash "$i")-amd64 $repo:ftp-$(printf '%011xA' "$i")-amd64 $repo:ftp-$(printf '%011x' "$i")-amd64 $repo:ftp-$(hash "$i")-s390x $repo:ftp-$(hash "$i")-amd64-extra $repo:-$(hash "$i")-amd64"
done
expect '3 only fixture image tags of this repository are considered' 0 "$expected" \
	"removing superseded fixture image $(ref "$ftp1")"

# 4. No fixture image is a success with a log line.
seed "2024-01-01 00:00:00 +0000 UTC $other:something"
clean
expect '4 no Web fixture images' 0 "$other:something" 'no Web fixture images on this host'

# 5. An unreachable engine must not read as an empty host.
seed "$(image 1 "$ftp1")" "$(image 2 "$ftp2")" "$(image 3 "$ftp3")" "$(image 4 "$ftp4")"
FAKE_DOCKER_DOWN=all clean
expect '5 docker unreachable' 1 "$(ref "$ftp1") $(ref "$ftp2") $(ref "$ftp3") $(ref "$ftp4")" 'Cannot connect to the Docker daemon'

# 6. An image that cannot be removed is reported, and the others still go.
seed "$(image 1 "$ftp1")" "$(image 2 "$ftp2")" "$(image 3 "$ftp3")" "$(image 4 "$ftp4")" "$(image 5 "$ftp5")"
FAKE_IMAGES_STUCK=$(ref "$ftp2") clean
expect '6 a stuck removal is reported, the other old image still goes' 1 \
	"$(ref "$ftp2") $(ref "$ftp3") $(ref "$ftp4") $(ref "$ftp5")" "removal of $(ref "$ftp2") failed"

# 7. An argument is a usage error and removes nothing.
seed "$(image 1 "$ftp1")" "$(image 2 "$ftp2")" "$(image 3 "$ftp3")" "$(image 4 "$ftp4")"
clean --all
expect '7 arguments are refused' 2 "$(ref "$ftp1") $(ref "$ftp2") $(ref "$ftp3") $(ref "$ftp4")" 'usage'

# 8. Mutants: each one must fail the case that protects its decision, with the
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
	mutant 'newest K ignored' 'keep_images=3' 'keep_images=0' \
		'^FAIL 1 .*: left \['
	mutant 'oldest kept instead of newest' '-k2,2r' '-k2,2' \
		'^FAIL 1 .*: left \['
	mutant 'fixtures counted together' '${BASH_REMATCH[1]}/${BASH_REMATCH[4]}/${BASH_REMATCH[6]}' '${BASH_REMATCH[4]}/${BASH_REMATCH[6]}' \
		'^FAIL 1 .*: left \['
	mutant 'architectures counted together' '${BASH_REMATCH[1]}/${BASH_REMATCH[4]}/${BASH_REMATCH[6]}' '${BASH_REMATCH[1]}/${BASH_REMATCH[6]}' \
		'^FAIL 1 .*: left \['
	mutant 'runners counted together' '${BASH_REMATCH[1]}/${BASH_REMATCH[4]}/${BASH_REMATCH[6]}' '${BASH_REMATCH[1]}/${BASH_REMATCH[4]}' \
		'^FAIL 1b .*: left \['
	mutant 'unsupported architectures accepted' '(amd64|arm64)' '([a-z0-9]+)' \
		'^FAIL 3 .*: left \['
	mutant 'hash shape loosened' '[0-9a-f]{12}' '[0-9a-fA-F]{11,12}' \
		'^FAIL 3 .*: left \['
	mutant 'removal failures ignored' '|| status=1' '|| status=0' \
		'^FAIL 6 .*: exit status 0, want 1'
	mutant 'candidates removed with docker image rm' '"$remove_images" "${superseded[@]}"' 'docker image rm "${superseded[@]}"' \
		'^FAIL 1 .*: exit status 1, want 0'
fi

printf '\n%s checks, %s failed\n' "$checks" "$failed"
[[ $failed -eq 0 ]]
