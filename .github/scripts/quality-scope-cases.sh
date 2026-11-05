#!/usr/bin/env bash
# Accept and reject cases for quality-scope.sh. Every case builds a scratch Git
# repository, provides refs/remotes/origin/main the way actions/checkout with
# fetch-depth 0 does, runs the script with the environment the scope job
# passes, and compares the required output. The cases cover the base selection
# of each ref kind, the unusable bases and the failed diff that must end in
# required=true with exit status 0 and one logged reason, and the
# documentation/CI-only follow-up push that the push delta used to pass for a
# head whose code never had a completed matrix.
#
# SCOPE_SCRIPT and SCOPE_CONTRACT point the cases at another script or
# contract.json than the ones next to this file.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
scope_script=${SCOPE_SCRIPT:-$here/quality-scope.sh}
contract=${SCOPE_CONTRACT:-$here/../../app/cli/internal/qualitygate/contract.json}
zero=0000000000000000000000000000000000000000
missing=1111111111111111111111111111111111111111

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
out=$work/github_output
cases=0
failed=0

# fresh_repo NAME creates a repository whose main holds the base commit C0 (code,
# documentation and the contract) with refs/remotes/origin/main at C0, and
# leaves the working directory inside it.
fresh_repo() {
	local dir=$work/$1
	mkdir -p "$dir/app/cli/internal/qualitygate" "$dir/internal" "$dir/docs"
	cd "$dir"
	git init -q -b main
	git config user.email scope-cases@example.invalid
	git config user.name scope-cases
	cp "$contract" app/cli/internal/qualitygate/contract.json
	printf 'package a\n' > internal/a.go
	printf '# a\n' > docs/a.md
	printf '# readme\n' > README.md
	git add -A
	git commit -q -m C0
	C0=$(git rev-parse HEAD)
	git update-ref refs/remotes/origin/main "$C0"
}

# commit MESSAGE PATH... appends a line to each path, commits and prints the sha.
commit() {
	local message=$1 path
	shift
	for path in "$@"; do
		mkdir -p "$(dirname "$path")"
		printf '%s\n' "$message" >> "$path"
	done
	git add -A
	git commit -q -m "$message"
	git rev-parse HEAD
}

# scope EVENT REF BEFORE HEAD runs the script as the scope job does and sets
# required, status and log.
scope() {
	: > "$out"
	if log=$(EVENT_NAME=$1 REF=$2 BEFORE_SHA=$3 HEAD_SHA=$4 GITHUB_OUTPUT=$out "$scope_script" 2>&1); then
		status=0
	else
		status=$?
	fi
	required=$(sed -n 's/^required=//p' "$out")
}

# expect NAME REQUIRED [REASON] checks the last scope call: exit status 0, the
# wanted required output, exactly one quality-scope line and, when REASON is
# given, that the line matches it as a regular expression, so a case proves
# which branch produced the verdict and not only the verdict.
expect() {
	local name=$1 want=$2 pattern=${3:-} reasons problem=''
	cases=$((cases + 1))
	reasons=$(printf '%s\n' "$log" | grep -c '^quality-scope: ' || true)
	if [[ $status -ne 0 ]]; then
		problem="exit status $status, want 0"
	elif [[ $required != "$want" ]]; then
		problem="required=${required:-<none>}, want $want"
	elif [[ $reasons -ne 1 ]]; then
		problem="$reasons quality-scope lines, want 1"
	elif [[ -n $pattern && ! $(printf '%s\n' "$log" | grep '^quality-scope: ') =~ $pattern ]]; then
		problem="the quality-scope line does not match: $pattern"
	fi
	if [[ -z $problem ]]; then
		printf 'ok   %s: required=%s\n       %s\n' "$name" "$required" "$(printf '%s\n' "$log" | grep '^quality-scope: ')"
	else
		printf 'FAIL %s: %s\n%s\n' "$name" "$problem" "$log"
		failed=$((failed + 1))
	fi
}

# 1. The follow-up push the push delta passed: a code push, then docs only.
fresh_repo f1-followup
git checkout -q -b feat
C1=$(commit code internal/a.go)
D1=$(commit docs docs/a.md)
scope push refs/heads/feat "$C0" "$C1"
expect '1a feature branch, code push' true
scope push refs/heads/feat "$C1" "$D1"
expect '1b feature branch, docs-only push after the code push' true

# 2. A branch whose whole diff against main is documentation stays skippable.
fresh_repo docs-branch
git checkout -q -b docs
D1=$(commit docs docs/a.md README.md)
scope push refs/heads/docs "$zero" "$D1"
expect '2a docs-only branch, first push (zero before)' false
D2=$(commit docs docs/b.md)
scope push refs/heads/docs "$D1" "$D2"
expect '2b docs-only branch, follow-up push' false

# 3-5. main judges the delta of each push.
fresh_repo main-delta
M1=$(commit code internal/a.go)
M2=$(commit docs docs/a.md)
scope push refs/heads/main "$M1" "$M2"
expect '3 main push, docs-only delta' false 'no business change \(documentation or CI files only\)'
C1=$(commit ci .github/scripts/x.sh)
scope push refs/heads/main "$M2" "$C1"
expect '3b main push, .github-only delta' false 'no business change \(documentation or CI files only\)'
M3=$(commit code internal/a.go)
scope push refs/heads/main "$C1" "$M3"
expect '4 main push, code delta' true 'internal/a\.go needs CI'
scope push refs/heads/main "$zero" "$M3"
expect '5 main push, zero before' true

# 6. A force-push can leave before outside the clone: full run, no failure.
fresh_repo main-force-push
git checkout -q -b orphan
X=$(commit orphan internal/x.go)
git checkout -q main
D1=$(commit docs docs/a.md)
git branch -q -D orphan
git reflog expire --expire=now --all
git gc -q --prune=now
if git cat-file -e "${X}^{commit}" 2>/dev/null; then
	echo 'precondition failed: the orphaned commit is still in the clone' >&2
	exit 1
fi
scope push refs/heads/main "$X" "$D1"
expect '6a main force-push, before pruned from the clone' true
scope push refs/heads/main "$missing" "$D1"
expect '6b main push, before is not an object' true

# 6c. A usable before and a head the diff cannot read: the base is accepted, then
# git diff itself fails. That is the last unusable-base branch, and it must end
# in a full run with exit status 0 like the others, never in required=false.
scope push refs/heads/main "$C0" "$missing"
expect '6c main push, valid before, git diff fails (head is not an object)' true 'no usable base, git diff .* failed'

# 7. A branch push without origin/main cannot be judged: full run, no failure.
fresh_repo no-origin-main
git checkout -q -b feat
D1=$(commit docs docs/a.md)
git update-ref -d refs/remotes/origin/main
scope push refs/heads/feat "$C0" "$D1"
expect '7 feature branch, origin/main missing' true

# 8. Tag pushes always run everything, even for a documentation-only head.
fresh_repo tag-push
git checkout -q -b docs
D1=$(commit docs docs/a.md)
git tag v1.0.0
scope push refs/tags/v1.0.0 "$zero" "$D1"
expect '8 tag push at a docs-only head' true
scope push '' "$C0" "$D1"
expect '8b push without a ref' true

# 9. Schedule, workflow_dispatch and any other event always run everything.
scope schedule refs/heads/main '' "$D1"
expect '9a schedule' true
scope workflow_dispatch refs/heads/docs '' "$D1"
expect '9b workflow_dispatch' true
scope pull_request refs/pull/1/merge "$C0" "$D1"
expect '9c pull_request (no longer a trigger)' true

# 10. No merge base (unrelated history): full run, no failure.
fresh_repo unrelated
git checkout -q --orphan other
git commit -q -m root
scope push refs/heads/other "$zero" "$(git rev-parse HEAD)"
expect '10 branch without a merge base with origin/main' true

# 11. A code file renamed to *.md is a code change: the delete side must count.
fresh_repo rename
git checkout -q -b mv
git mv internal/a.go docs/moved.md
git commit -q -m 'rename code to markdown'
scope push refs/heads/mv "$C0" "$(git rev-parse HEAD)"
expect '11 branch renames a code file to *.md' true

# 12. Documentation mixed with code in one push.
fresh_repo mixed
git checkout -q -b feat
D1=$(commit mixed docs/a.md internal/a.go)
scope push refs/heads/feat "$C0" "$D1"
expect '12 branch with documentation and code' true

# 12b. Markdown under a code directory is judged as code by the path list.
fresh_repo md-in-code
git checkout -q -b feat
D1=$(commit notes internal/notes.md)
scope push refs/heads/feat "$C0" "$D1"
expect '12b branch changes only internal/notes.md' true

# 12c. The CI's own files need no run either: everything under .github/ is
# non-business, its Markdown included.
fresh_repo ci-files
git checkout -q -b ci
D1=$(commit ci .github/workflows/quality.yml .github/scripts/x.sh .github/pull_request_template.md)
scope push refs/heads/ci "$zero" "$D1"
expect '12c branch changing only .github files' false 'no business change \(documentation or CI files only\)'

# 12d. CI files mixed with business code still need the heavy checks.
fresh_repo ci-and-business
git checkout -q -b ci-mixed
D1=$(commit mixed .github/scripts/x.sh internal/a.go)
scope push refs/heads/ci-mixed "$zero" "$D1"
expect '12d branch with CI files and business code' true 'internal/a\.go needs CI'

# 13. Only the branch's own changes count: main moving on does not force a run.
fresh_repo main-moves
git checkout -q -b docs
D1=$(commit docs docs/a.md)
git checkout -q main
M1=$(commit code internal/a.go)
git update-ref refs/remotes/origin/main "$M1"
scope push refs/heads/docs "$zero" "$D1"
expect '13a docs-only branch, main has moved on with code' false
git checkout -q docs
git merge -q --no-ff -m 'merge main' main
scope push refs/heads/docs "$D1" "$(git rev-parse HEAD)"
expect '13b docs-only branch after merging that main' false

# 14. A force-pushed branch is judged by its content; before is not consulted.
fresh_repo branch-force-push
git checkout -q -b feat
C1=$(commit code internal/a.go)
scope push refs/heads/feat "$missing" "$C1"
expect '14a feature branch force-push, code, before missing' true
git checkout -q main
git checkout -q -b docs
D1=$(commit docs docs/a.md)
scope push refs/heads/docs "$missing" "$D1"
expect '14b feature branch force-push, docs only, before missing' false

# 15. A real clone made by the git commands actions/checkout runs with
# fetch-depth 0 (fetch every head into refs/remotes/origin/*, then check the
# pushed branch out): origin/main exists without being faked.
fresh_repo checkout-seed
remote=$work/remote.git
git init -q --bare -b main "$remote"
git remote add origin "$remote"
git push -q origin main
git checkout -q -b feat
C1=$(commit code internal/a.go)
D1=$(commit docs docs/a.md)
git push -q origin feat
git checkout -q main
git checkout -q -b docs
DOCS=$(commit docs docs/a.md)
git push -q origin docs
mkdir "$work/clone"
cd "$work/clone"
git init -q -b main
git remote add origin "$remote"
git -c protocol.version=2 fetch --quiet --no-tags --prune --no-recurse-submodules origin \
	'+refs/heads/*:refs/remotes/origin/*' '+refs/tags/*:refs/tags/*'
git rev-parse --verify --quiet refs/remotes/origin/main > /dev/null
git checkout --progress --force -q -B feat refs/remotes/origin/feat
scope push refs/heads/feat "$C1" "$D1"
expect '15a checkout clone, feature branch, docs push after a code push' true
git checkout --progress --force -q -B docs refs/remotes/origin/docs
scope push refs/heads/docs "$zero" "$DOCS"
expect '15b checkout clone, docs-only branch' false

printf '\n%s cases, %s failed\n' "$cases" "$failed"
[[ $failed -eq 0 ]]
