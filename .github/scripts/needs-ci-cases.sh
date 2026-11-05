#!/usr/bin/env bash
# Accept and reject cases for needs-ci.sh, plus drift checks that keep the
# classification single-sourced.
#
# Every case builds a scratch Git repository, commits the paths under test and
# compares the script's exit status and exact stdout. The last two checks
# compare quality.yml with the expected on.push.paths list and assert that
# quality-scope.sh delegates to needs-ci.sh instead of carrying its own copy of
# the case list, so the workflow filter and the script cannot silently diverge.
#
# NEEDS_CI_SCRIPT, QUALITY_WORKFLOW and SCOPE_SCRIPT point the cases at another
# script or workflow than the ones next to this file.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script=${NEEDS_CI_SCRIPT:-$here/needs-ci.sh}
workflow=${QUALITY_WORKFLOW:-$here/../workflows/quality.yml}
scope_script=${SCOPE_SCRIPT:-$here/quality-scope.sh}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
cases=0
failed=0

# report NAME PROBLEM counts a case and prints its result.
report() {
	cases=$((cases + 1))
	if [[ -z $2 ]]; then
		printf 'ok   %s\n' "$1"
	else
		printf 'FAIL %s: %s\n' "$1" "$2"
		failed=$((failed + 1))
	fi
}

# fresh_repo NAME creates a repository with one base commit that holds a code
# file, documentation and the other path kinds the case list names (no .github
# directory, so a case can commit a root file named .github), and leaves the
# working directory inside it with base set to the commit.

fresh_repo() {
	local dir=$work/$1
	mkdir -p "$dir/internal" "$dir/docs" "$dir/app/web" "$dir/.githooks"
	cd "$dir"
	git init -q -b main
	git config user.email needs-ci-cases@example.invalid
	git config user.name needs-ci-cases
	printf 'package a\n' > internal/a.go
	printf '# a\n' > docs/a.md
	printf '# readme\n' > README.md
	printf '#!/bin/sh\n' > .githooks/pre-commit
	printf 'module x\n' > go.mod
	printf 'FROM scratch\n' > Dockerfile
	printf '*\n' > .dockerignore
	printf 'notices\n' > THIRD_PARTY_NOTICES
	printf 'tools:\n' > mise.toml
	printf 'linters:\n' > .golangci.yml
	printf 'package main\n' > app/web/main.go
	git add -A
	git commit -q -m base
	base=$(git rev-parse HEAD)
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

# classify ARGS... runs the script with exactly these arguments and sets status,
# out and err.
classify() {
	: > "$work/stdout"
	: > "$work/stderr"
	status=0
	"$script" "$@" > "$work/stdout" 2> "$work/stderr" || status=$?
	out=$(cat "$work/stdout")
	err=$(cat "$work/stderr")
}

# expect NAME STATUS STDOUT checks the last classify call's exit status and its
# exact stdout.
expect() {
	local problem=''
	if [[ $status -ne $2 ]]; then
		problem="exit status $status, want $2"
	elif [[ $out != "$3" ]]; then
		problem="stdout $(printf '%q' "$out"), want $(printf '%q' "$3")"
	fi
	report "$1" "$problem"
}

# expect_unusable NAME checks an exit status of 2, empty stdout and a reason on
# stderr.
expect_unusable() {
	local problem=''
	if [[ $status -ne 2 ]]; then
		problem="exit status $status, want 2"
	elif [[ -n $out ]]; then
		problem="stdout is not empty: $(printf '%q' "$out")"
	elif [[ -z $err ]]; then
		problem='stderr is empty, the reason is not reported'
	fi
	report "$1" "$problem"
}

# 1. Documentation only, under any directory.
fresh_repo docs
head=$(commit docs docs/a.md README.md)
classify "$base" "$head"
expect 'documentation only' 0 "needs-ci: $base..$head: no business change (documentation or CI files only)"

# 2. The CI's own files only, its Markdown included.
fresh_repo ci-files
head=$(commit ci .github/workflows/quality.yml .github/scripts/x.sh .github/pull_request_template.md README.md)
classify "$base" "$head"
expect 'CI files and documentation only' 0 "needs-ci: $base..$head: no business change (documentation or CI files only)"

# 3. A Markdown file under .github is a CI file, not documentation that has to
# be told apart from its directory.
fresh_repo github-md
head=$(commit notes .github/notes.md)
classify "$base" "$head"
expect '.github/notes.md is a CI file' 0 "needs-ci: $base..$head: no business change (documentation or CI files only)"

# 4-5. Markdown under a business path needs CI.
fresh_repo internal-md
head=$(commit notes internal/notes.md)
classify "$base" "$head"
expect 'internal/notes.md needs CI' 1 'internal/notes.md'
fresh_repo hooks-md
head=$(commit notes .githooks/notes.md)
classify "$base" "$head"
expect '.githooks/notes.md needs CI' 1 '.githooks/notes.md'

# 6. A business file renamed to markdown: the diff lists both rename sides, and
# the deleted business path decides.
fresh_repo rename
git mv internal/a.go docs/moved.md
git commit -q -m rename
classify "$base" "$(git rev-parse HEAD)"
expect 'business file renamed to markdown' 1 'internal/a.go'

# 7. A documentation file renamed into a business path needs CI.
fresh_repo rename-in
git mv docs/a.md internal/moved.md
git commit -q -m rename
classify "$base" "$(git rev-parse HEAD)"
expect 'documentation renamed to a business path' 1 'internal/moved.md'

# 8. Documentation mixed with business code.
fresh_repo mixed
head=$(commit mixed docs/c.md app/web/x.go)
classify "$base" "$head"
expect 'documentation and business code' 1 'app/web/x.go'

# 9. The first business path is reported.
fresh_repo first
head=$(commit first app/web/a.go internal/z.go)
classify "$base" "$head"
expect 'first business path' 1 'app/web/a.go'

# 10. An empty diff needs no CI.
fresh_repo empty
classify "$base" "$base"
expect 'empty diff' 0 "needs-ci: $base..$base: no business change (documentation or CI files only)"

# 11-14. Unusable input exits 2 with the reason on stderr and empty stdout.
fresh_repo unusable
classify 1111111111111111111111111111111111111111 "$base"
expect_unusable 'unknown base commit'
classify "$base" 2222222222222222222222222222222222222222
expect_unusable 'unknown head commit'
classify "$base"
expect_unusable 'one argument'
classify
expect_unusable 'no arguments'

# 15. go.md at the root needs CI because go.* precedes *.md, which is why the
# workflow filter re-includes go.*.
fresh_repo go-md
head=$(commit notes go.md)
classify "$base" "$head"
expect 'root go.md needs CI' 1 'go.md'

# 16. docs/go.md is documentation because go.* matches the root only.
fresh_repo docs-go-md
head=$(commit notes docs/go.md)
classify "$base" "$head"
expect 'docs/go.md is documentation' 0 "needs-ci: $base..$head: no business change (documentation or CI files only)"

# 17. Dockerfile.md is documentation: the filter needs no Dockerfile entry for
# it, because Dockerfile does not match Dockerfile.md.
fresh_repo dockerfile-md
head=$(commit notes Dockerfile.md)
classify "$base" "$head"
expect 'Dockerfile.md is documentation' 0 "needs-ci: $base..$head: no business change (documentation or CI files only)"

# 18. A path outside the case list needs CI.
fresh_repo other
head=$(commit notes notes.txt)
classify "$base" "$head"
expect 'a path outside the case list' 1 'notes.txt'

# 19. A path with a space is reported verbatim; the caller quotes it.
fresh_repo space
head=$(commit notes 'internal/my file.go')
classify "$base" "$head"
expect 'a path with a space' 1 'internal/my file.go'

# 20. A root file named .github is not under .github/ and needs CI, matching
# the filter's !.github/** which cannot match it either.
fresh_repo dot-github
head=$(commit notes .github)
classify "$base" "$head"
expect 'a root file named .github needs CI' 1 '.github'

# The on.push.paths list that mirrors the case list, in workflow order:
#   '**'            a push that changes anything ...
#   '!**.md'        ... except Markdown, which needs-ci.sh calls documentation
#   '!.github/**'   ... and the CI's own files, which never need a run
#   'internal/**'   re-included: a *.md below a business path needs CI
#   'app/**'
#   '.githooks/**'
#   'go.*'          re-included: go.* also matches a root go.md, which
#                   needs-ci.sh judges as business before *.md
# Dockerfile, .dockerignore, THIRD_PARTY_NOTICES, mise.toml and .golangci.yml
# cannot be *.md and are not under .github, so no entry is needed for them; a
# *.md variant is documentation for both the script and the filter. Nothing may
# re-include .github/**: it has to stay excluded, its Markdown included. Any
# change to this list is a change to the classification and needs the cases
# above.
expected_paths='**
!**.md
!.github/**
internal/**
app/**
.githooks/**
go.*'

# workflow_paths prints the on.push.paths items of quality.yml one per line and
# fails when the trigger does not have the expected shape: only `paths` under
# `push` (branches and tags must stay unfiltered, or tag pushes and branch
# deletion events would stop creating runs), quoted items, then another key. It
# is deliberately strict: an unfamiliar key, an unquoted item or another
# indentation fails instead of being skipped, so a restructured trigger cannot
# pass unnoticed.
workflow_paths() {
	local line state=seek-on value=''
	while IFS= read -r line; do
		case $state in
		seek-on)
			[[ $line == 'on:' ]] && state=in-on
			;;
		in-on)
			case $line in
			'  push:') state=in-push ;;
			[![:space:]]*) state=seek-on ;;
			esac
			;;
		in-push)
			case $line in
			'    paths:') state=in-paths ;;
			''|'    #'*) ;; # blank line or a comment inside the push trigger
			*) state=bad ;;
			esac
			;;
		in-paths)
			case $line in
			"      - '"*"'")
				value=${line#"      - '"}
				value=${value%"'"}
				if [[ $value == *"'"* ]]; then
					state=bad
					continue
				fi
				printf '%s\n' "$value"
				;;
			''|'    #'*) ;; # blank line or a comment inside the list
			'      '*) state=bad ;; # an item that is not a quoted single line
			'    '[![:space:]]*) state=bad ;; # another key under push
			*) state=finished ;; # a key that ends the push trigger
			esac
			;;
		finished|bad) ;;
		esac
	done < "$workflow"
	[[ $state == finished ]]
}

drift() {
	local problem='' got=''
	if [[ ! -f $workflow ]]; then
		problem="no workflow at $workflow"
	elif ! got=$(workflow_paths); then
		problem='the on.push.paths block does not have the expected shape'
	elif [[ $got != "$expected_paths" ]]; then
		problem=$(printf 'on.push.paths is not the expected list:\n--- got ---\n%s\n--- want ---\n%s' "$got" "$expected_paths")
	fi
	report 'quality.yml on.push.paths mirrors needs-ci.sh' "$problem"
}

delegation() {
	local problem=''
	if [[ ! -f $scope_script ]]; then
		problem="no script at $scope_script"
	elif ! grep -qF 'needs-ci.sh' "$scope_script"; then
		problem='quality-scope.sh does not call needs-ci.sh'
	elif grep -qF 'internal/*|app/*' "$scope_script"; then
		problem='quality-scope.sh still carries its own case list'
	fi
	report 'quality-scope.sh delegates the case list' "$problem"
}

drift
delegation

printf '\n%s cases, %s failed\n' "$cases" "$failed"
[[ $failed -eq 0 ]]
