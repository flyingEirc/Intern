#!/usr/bin/env bash
# The single source of the needs-CI classification: whether any path changed
# between two commits needs a CI run. CI validates business code. Documentation
# and the CI's own files (.github/**: the workflow, the scripts, their
# self-tests and the templates) do not need a run; they still execute in the
# next business run, and the CI scripts must pass their self-tests locally
# before the push (docs/workflow/pr.md). quality-scope.sh calls this script for
# the scope job, and the on.push.paths filter in quality.yml mirrors its case
# list; needs-ci-cases.sh fails when the filter and this script drift.
#
#   needs-ci.sh BASE HEAD
#
#   exit 0  no business change, an empty diff included.
#           stdout: needs-ci: BASE..HEAD: no business change (documentation or CI files only)
#   exit 1  at least one business path. stdout is that path, verbatim, for the
#           caller to report.
#   exit 2  unusable input: the wrong number of arguments, a commit that is
#           missing or not in this clone, or a failed diff. The reason goes to
#           stderr and stdout stays empty.
#
# The diff disables rename detection (--no-renames): the default lists only the
# new name of a rename, which would hide a business file renamed to *.md.
#
# The case list, in order of precedence: .github/** is the CI's own files and
# never needs a run; paths under the business directories and the pinned tool
# and build inputs are business even when they end in *.md; any other *.md is
# documentation; everything else is business.
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
while IFS= read -r -d '' path; do
    case "$path" in
        .github/*)
            ;; # The CI's own workflow, scripts, self-tests and templates.
        internal/*|app/*|go.*|Dockerfile|.dockerignore|THIRD_PARTY_NOTICES|mise.toml|.golangci.yml|.githooks/*)
            printf '%s\n' "$path"
            exit 1 ;;
        *.md)
            ;; # Documentation outside the business directories.
        *)
            printf '%s\n' "$path"
            exit 1 ;;
    esac
done < "$changes"
printf 'needs-ci: %s..%s: no business change (documentation or CI files only)\n' "$base" "$head"
