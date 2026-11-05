#!/usr/bin/env bash
# Accept and reject cases for needs-ci.sh, plus drift checks that keep the
# classification single-sourced.
#
# Every case builds a scratch Git repository, commits the paths under test and
# compares the script's exit status and exact stdout, which for a business
# change is the tier (cli, web or all) and the path that settled it. The last
# two checks compare quality.yml with the expected on.push.paths list and
# assert that quality-scope.sh delegates to needs-ci.sh instead of carrying its
# own copy of the case list, so the workflow filter and the script cannot
# silently diverge.
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

# 4-5. Markdown under a shared business path needs the full set.
fresh_repo internal-md
head=$(commit notes internal/notes.md)
classify "$base" "$head"
expect 'internal/notes.md needs CI' 1 'all internal/notes.md'
fresh_repo hooks-md
head=$(commit notes .githooks/notes.md)
classify "$base" "$head"
expect '.githooks/notes.md needs CI' 1 'all .githooks/notes.md'

# 6. A business file renamed to markdown: the diff lists both rename sides, and
# the deleted business path decides.
fresh_repo rename
git mv internal/a.go docs/moved.md
git commit -q -m rename
classify "$base" "$(git rev-parse HEAD)"
expect 'business file renamed to markdown' 1 'all internal/a.go'

# 7. A documentation file renamed into a business path needs CI.
fresh_repo rename-in
git mv docs/a.md internal/moved.md
git commit -q -m rename
classify "$base" "$(git rev-parse HEAD)"
expect 'documentation renamed to a business path' 1 'all internal/moved.md'

# 8. Documentation mixed with Web code.
fresh_repo mixed
head=$(commit mixed docs/c.md app/web/x.go)
classify "$base" "$head"
expect 'documentation and Web code' 1 'web app/web/x.go'

# 9. A shared path after a Web path settles all, and is the one reported.
fresh_repo first
head=$(commit first app/web/a.go internal/z.go)
classify "$base" "$head"
expect 'shared path after a Web path' 1 'all internal/z.go'

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
expect 'root go.md needs CI' 1 'all go.md'

# 16. docs/go.md is documentation: the go.* arm is root-only (like GitHub's
# go.*), so the *.md rule decides; 21f covers a path below a go.* directory.
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
expect 'a path outside the case list' 1 'all notes.txt'

# 19. A path with a space is reported verbatim; the caller quotes it.
fresh_repo space
head=$(commit notes 'internal/my file.go')
classify "$base" "$head"
expect 'a path with a space' 1 'all internal/my file.go'

# 20. A root file named .github is not under .github/ and needs CI, matching
# the filter's !.github/** which cannot match it either.
fresh_repo dot-github
head=$(commit notes .github)
classify "$base" "$head"
expect 'a root file named .github needs CI' 1 'all .github'

# 21. CI files mixed with business code: the .github arm must not consume the
# change, so the business path still needs CI.
fresh_repo ci-and-business
head=$(commit mixed .github/scripts/x.sh internal/z.go)
classify "$base" "$head"
expect 'CI files mixed with business code' 1 'all internal/z.go'

# 21b. CI Markdown mixed with Markdown below app/.
fresh_repo ci-md-and-app-md
head=$(commit mixed .github/notes.md app/web/notes.md)
classify "$base" "$head"
expect 'CI Markdown mixed with app Markdown' 1 'web app/web/notes.md'

# 21c. Markdown below app/web needs the Web tier even as the only change.
fresh_repo app-md
head=$(commit notes app/web/notes.md)
classify "$base" "$head"
expect 'app/web/notes.md needs CI' 1 'web app/web/notes.md'

# 21d. Non-ASCII paths survive the -z diff unchanged: without it git quotes the
# path, which would be judged business and printed escaped.
fresh_repo non-ascii
head=$(commit docs 'docs/数据库.md')
classify "$base" "$head"
expect 'non-ASCII documentation' 0 "needs-ci: $base..$head: no business change (documentation or CI files only)"
head=$(commit notes 'internal/数据库.md')
classify "$head~1" "$head"
expect 'non-ASCII business path' 1 'all internal/数据库.md'

# 21e. A failed diff is unusable input, never a no-business verdict.
fresh_repo broken-tree
head=$(commit notes docs/b.md)
tree=$(git rev-parse "${head}^{tree}")
rm -f ".git/objects/${tree:0:2}/${tree:2}"
classify "$base" "$head"
expect_unusable 'missing tree object (git diff fails)'

# 21f. A path below a go.* directory: GitHub's go.* matches the root only, so
# go.d/x.md is documentation and go.d/x.txt falls through to business.
fresh_repo go-dir
head=$(commit notes go.d/x.md)
classify "$base" "$head"
expect 'go.d/x.md is documentation' 0 "needs-ci: $base..$head: no business change (documentation or CI files only)"
head=$(commit notes go.d/x.txt)
classify "$head~1" "$head"
expect 'go.d/x.txt needs CI' 1 'all go.d/x.txt'

# 22. CLI-only paths, Markdown below app/cli included.
fresh_repo cli-only
head=$(commit cli app/cli/main.go app/cli/integration/x_test.go)
classify "$base" "$head"
expect 'CLI code only, first path reported' 1 'cli app/cli/integration/x_test.go'
head=$(commit notes app/cli/README.md)
classify "$head~1" "$head"
expect 'app/cli/README.md is CLI-only' 1 'cli app/cli/README.md'

# 23. Web-only paths: code, migrations, fixtures and the deployment files.
fresh_repo web-only
head=$(commit web app/web/internal/store/migrations/00010_x.sql app/web/deploy/Dockerfile app/web/integration/ftp.Dockerfile)
classify "$base" "$head"
expect 'Web migrations and images only' 1 'web app/web/deploy/Dockerfile'

# 24. Both modules: the first path of the second module settles all.
fresh_repo both-modules
head=$(commit both docs/a.md app/cli/a.go app/cli/b.go app/web/a.go app/web/b.go)
classify "$base" "$head"
expect 'CLI and Web code' 1 'all app/web/a.go'

# 25. A CLI path followed by a shared path: the shared path settles all.
fresh_repo cli-and-shared
head=$(commit mixed app/cli/a.go internal/z.go)
classify "$base" "$head"
expect 'CLI code and a shared path' 1 'all internal/z.go'

# 26. Files below a module that the other module reads are shared: both
# Dockerfiles copy every module's go.mod and go.sum, and the Web physical and
# W3 scenarios read the CLI contract.json.
for shared in app/cli/go.mod app/cli/go.sum app/web/go.mod app/web/go.sum app/cli/internal/qualitygate/contract.json; do
	fresh_repo "shared-$(printf '%s' "$shared" | tr '/.' '--')"
	head=$(commit shared "$shared")
	classify "$base" "$head"
	expect "$shared is shared" 1 "all $shared"
done

# 27. The root image inputs are shared: the Web physical scenario builds the
# root Dockerfile's client-tools stage as well.
for shared in Dockerfile .dockerignore THIRD_PARTY_NOTICES mise.toml .golangci.yml go.work; do
	fresh_repo "root-$(printf '%s' "$shared" | tr '/.' '--')"
	head=$(commit shared "$shared")
	classify "$base" "$head"
	expect "root $shared is shared" 1 "all $shared"
done

# 28. Another directory below app/ is not proven to reach one module.
fresh_repo app-site
head=$(commit site app/site/index.html)
classify "$base" "$head"
expect 'app/site is shared' 1 'all app/site/index.html'

# 29. A file moved from app/cli to app/web: both rename sides count.
fresh_repo cross-rename
head=$(commit cli app/cli/moved.go)
git mv app/cli/moved.go app/web/moved.go
git commit -q -m move
classify "$head" "$(git rev-parse HEAD)"
expect 'a file moved from app/cli to app/web' 1 'all app/web/moved.go'

# The on.push.paths list that mirrors the none/business split of the case list,
# in workflow order:
#   '**'            a push that changes anything ...
#   '!**.md'        ... except Markdown, which needs-ci.sh calls documentation
#   '!.github/**'   ... and the CI's own files, which never need a run
#   'internal/**'   re-included: a *.md below a business path needs CI
#   'app/**'
#   '.githooks/**'
#   'go.*'          re-included: it matches a root go.md, which needs-ci.sh
#                   judges as business before *.md; both are root-only
# Dockerfile, .dockerignore, THIRD_PARTY_NOTICES, mise.toml and .golangci.yml
# cannot be *.md and are not under .github, so no entry is needed for them; a
# *.md variant is documentation for both the script and the filter. Nothing may
# re-include .github/**: it has to stay excluded, its Markdown included. The
# cli/web/all tiers do not change the list: any tier creates a run. Any change
# to this list is a change to the classification and needs the cases above.
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
