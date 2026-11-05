#!/usr/bin/env bash
# Accept and reject cases for docs-only.sh, plus drift checks that keep the
# classification single-sourced.
#
# Every case builds a scratch Git repository, commits the paths under test and
# compares the script's exit status and exact stdout. The last two checks
# compare quality.yml with the expected on.push.paths list and assert that
# quality-scope.sh delegates to docs-only.sh instead of carrying its own copy
# of the case list, so the workflow filter and the script cannot silently
# diverge.
#
# DOCS_ONLY_SCRIPT, QUALITY_WORKFLOW and SCOPE_SCRIPT point the cases at
# another script or workflow than the ones next to this file.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script=${DOCS_ONLY_SCRIPT:-$here/docs-only.sh}
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
# file, documentation and the other path kinds the case list names, and leaves
# the working directory inside it with base set to the commit.
fresh_repo() {
	local dir=$work/$1
	mkdir -p "$dir/internal" "$dir/docs" "$dir/app/web" "$dir/.github" "$dir/.githooks"
	cd "$dir"
	git init -q -b main
	git config user.email docs-only-cases@example.invalid
	git config user.name docs-only-cases
	printf 'package a\n' > internal/a.go
	printf '# a\n' > docs/a.md
	printf '# readme\n' > README.md
	printf 'name: ci\n' > .github/ci.yml
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

# 1. Only documentation, under any directory.
fresh_repo docs
head=$(commit docs docs/a.md README.md)
classify "$base" "$head"
expect 'documentation only' 0 "docs-only: $base..$head: only documentation changed"

# 2. A code path.
fresh_repo code
head=$(commit code internal/a.go)
classify "$base" "$head"
expect 'code under internal/' 1 'internal/a.go'

# 3-5. Markdown under a code path is code, not documentation.
fresh_repo internal-md
head=$(commit notes internal/notes.md)
classify "$base" "$head"
expect 'internal/notes.md is code' 1 'internal/notes.md'
fresh_repo github-md
head=$(commit notes .github/notes.md)
classify "$base" "$head"
expect '.github/notes.md is code' 1 '.github/notes.md'
fresh_repo hooks-md
head=$(commit notes .githooks/notes.md)
classify "$base" "$head"
expect '.githooks/notes.md is code' 1 '.githooks/notes.md'

# 6. A code file renamed to markdown: the diff lists both rename sides, and the
# deleted code path decides.
fresh_repo rename
git mv internal/a.go docs/moved.md
git commit -q -m rename
classify "$base" "$(git rev-parse HEAD)"
expect 'code renamed to markdown' 1 'internal/a.go'

# 7. Documentation mixed with code.
fresh_repo mixed
head=$(commit mixed docs/c.md app/web/x.go)
classify "$base" "$head"
expect 'documentation and code' 1 'app/web/x.go'

# 8. The first non-documentation path is reported.
fresh_repo first
head=$(commit first app/web/a.go internal/z.go)
classify "$base" "$head"
expect 'first non-documentation path' 1 'app/web/a.go'

# 9. An empty diff is documentation only.
fresh_repo empty
classify "$base" "$base"
expect 'empty diff' 0 "docs-only: $base..$base: only documentation changed"

# 10-13. Unusable input exits 2 with the reason on stderr and empty stdout.
fresh_repo unusable
classify 1111111111111111111111111111111111111111 "$base"
expect_unusable 'unknown base commit'
classify "$base" 2222222222222222222222222222222222222222
expect_unusable 'unknown head commit'
classify "$base"
expect_unusable 'one argument'
classify
expect_unusable 'no arguments'

# 14. go.md at the root is code because go.* precedes *.md, which is why the
# workflow filter re-includes go.*.
fresh_repo go-md
head=$(commit notes go.md)
classify "$base" "$head"
expect 'root go.md is code' 1 'go.md'

# 15. docs/go.md is documentation because go.* matches the root only.
fresh_repo docs-go-md
head=$(commit notes docs/go.md)
classify "$base" "$head"
expect 'docs/go.md is documentation' 0 "docs-only: $base..$head: only documentation changed"

# 16. Dockerfile.md is documentation: the filter needs no Dockerfile entry for
# it, because Dockerfile does not match Dockerfile.md.
fresh_repo dockerfile-md
head=$(commit notes Dockerfile.md)
classify "$base" "$head"
expect 'Dockerfile.md is documentation' 0 "docs-only: $base..$head: only documentation changed"

# 17. A path outside the case list is code.
fresh_repo other
head=$(commit notes notes.txt)
classify "$base" "$head"
expect 'a path outside the case list' 1 'notes.txt'

# 18. A path with a space is reported verbatim; the caller quotes it.
fresh_repo space
head=$(commit notes 'internal/my file.go')
classify "$base" "$head"
expect 'a path with a space' 1 'internal/my file.go'

# The on.push.paths list that mirrors the case list, in workflow order:
#   '**'            a push that changes anything ...
#   '!**.md'        ... except Markdown, which docs-only.sh calls documentation
#   'internal/**'   re-included: a *.md below a code path is code
#   'app/**'
#   '.github/**'
#   '.githooks/**'
#   'go.*'          re-included: go.* also matches a root go.md, which
#                   docs-only.sh judges as code before *.md
# Dockerfile, .dockerignore, THIRD_PARTY_NOTICES, mise.toml and .golangci.yml
# cannot be *.md, so no entry is needed for them; a *.md variant is
# documentation for both the script and the filter. Any change to this list is
# a change to the classification and needs the cases above.
expected_paths='**
!**.md
internal/**
app/**
.github/**
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
	report 'quality.yml on.push.paths mirrors docs-only.sh' "$problem"
}

delegation() {
	local problem=''
	if [[ ! -f $scope_script ]]; then
		problem="no script at $scope_script"
	elif ! grep -qF 'docs-only.sh' "$scope_script"; then
		problem='quality-scope.sh does not call docs-only.sh'
	elif grep -qF 'internal/*|app/*' "$scope_script"; then
		problem='quality-scope.sh still carries its own case list'
	fi
	report 'quality-scope.sh delegates the case list' "$problem"
}

drift
delegation

printf '\n%s cases, %s failed\n' "$cases" "$failed"
[[ $failed -eq 0 ]]
