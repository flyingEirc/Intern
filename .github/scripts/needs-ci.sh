#!/usr/bin/env bash
# The single source of the needs-CI classification: whether any path changed
# between two commits needs a CI run, and which container tier it reaches. CI
# validates business code. Documentation and the CI's own files (.github/**:
# the workflow, the scripts, their self-tests and the templates) do not need a
# run; they still execute in the next business run, and the CI scripts must
# pass their self-tests locally before the push (docs/workflow/pr.md).
# quality-scope.sh calls this script for the scope job, and the on.push.paths
# filter in quality.yml mirrors its none/business split; needs-ci-cases.sh fails
# when the filter and this script drift.
#
#   needs-ci.sh BASE HEAD
#
#   exit 0  no business change, an empty diff included.
#           stdout: needs-ci: BASE..HEAD: no business change (documentation or CI files only)
#   exit 1  at least one business path. stdout is one line, TIER PATH:
#             cli  every business path is CLI-only: the CLI container matrix
#                  covers them and no Web container job reads them.
#             web  every business path is Web-only: the Web container jobs
#                  cover them and the CLI container matrix reads none of them.
#             all  a shared path (internal/**, the workspace and both images'
#                  inputs, the tooling) or both modules changed.
#           PATH is the path that settled the tier, verbatim, for the caller to
#           report: the first shared path, or the first path of the second
#           module.
#   exit 2  unusable input: the wrong number of arguments, a commit that is
#           missing or not in this clone, or a failed diff. The reason goes to
#           stderr and stdout stays empty.
#
# The diff disables rename detection (--no-renames): the default lists only the
# new name of a rename, which would hide a business file renamed to *.md.
#
# The case list, in order of precedence: .github/** is the CI's own files and
# never needs a run. Below app/cli and app/web, the files the other module also
# reads are shared: both Dockerfiles copy every module's go.mod and go.sum, and
# the Web physical and W3 scenarios read the CLI contract.json. The rest of
# app/cli is CLI-only and the rest of app/web is Web-only, their *.md included.
# The other business directories and the pinned tool and build inputs are
# shared even when they end in *.md (the root Dockerfile is built by both the
# CLI matrix and the Web physical scenario); a root go.* file is shared,
# matching GitHub's go.* which cannot match below the root; any other *.md is
# documentation; everything else is shared. A path that is not proven to reach
# only one module is shared.
set -euo pipefail

if [[ $# -ne 2 ]]; then
    printf 'usage: needs-ci.sh BASE HEAD\n' >&2
    exit 2
fi
base=$1
head=$2
for commit in "$base" "$head"; do
    if [[ $commit == -* ]] || ! git rev-parse --verify --quiet "${commit}^{commit}" > /dev/null; then
        printf 'needs-ci: %s is not a commit in this clone\n' "$commit" >&2
        exit 2
    fi
done
changes=$(mktemp)
trap 'rm -f "$changes"' EXIT
if ! git diff --name-only --no-renames -z "$base" "$head" -- > "$changes"; then
    printf 'needs-ci: git diff %s %s failed\n' "$base" "$head" >&2
    exit 2
fi
cli=false
web=false
decided=''
while IFS= read -r -d '' path; do
    case "$path" in
        .github/*)
            tier=none ;; # The CI's own workflow, scripts, self-tests and templates.
        app/cli/go.mod|app/cli/go.sum|app/web/go.mod|app/web/go.sum|app/cli/internal/qualitygate/contract.json)
            tier=all ;; # Read by the other module's image build or scenarios.
        app/cli/*)
            tier=cli ;;
        app/web/*)
            tier=web ;;
        internal/*|app/*|Dockerfile|.dockerignore|THIRD_PARTY_NOTICES|mise.toml|.golangci.yml|.githooks/*)
            tier=all ;;
        go.*)
            # GitHub's go.* matches a root file only, while this case pattern
            # also matches a path below a directory whose name starts with
            # "go.". Keep the script root-only like the filter.
            if [[ $path != */* ]]; then
                tier=all
            else
                case "$path" in
                    *.md) tier=none ;; # go.d/x.md: not a root go.* input, so documentation.
                    *) tier=all ;;
                esac
            fi
            ;;
        *.md)
            tier=none ;; # Documentation outside the business directories.
        *)
            tier=all ;;
    esac
    case $tier in
        none) continue ;;
        cli) $cli && continue; cli=true ;;
        web) $web && continue; web=true ;;
        all) cli=true; web=true ;;
    esac
    decided=$path
    if $cli && $web; then
        break
    fi
done < "$changes"
if $cli && $web; then
    printf 'all %s\n' "$decided"
elif $cli; then
    printf 'cli %s\n' "$decided"
elif $web; then
    printf 'web %s\n' "$decided"
else
    printf 'needs-ci: %s..%s: no business change (documentation or CI files only)\n' "$base" "$head"
    exit 0
fi
exit 1
