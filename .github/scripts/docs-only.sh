#!/usr/bin/env bash
# The single source of the documentation-only classification: whether every
# path changed between two commits is documentation. quality-scope.sh calls it
# for the scope job, and the on.push.paths filter in quality.yml mirrors its
# case list; docs-only-cases.sh fails when the filter and this script drift.
#
#   docs-only.sh BASE HEAD
#
#   exit 0  every changed path is documentation, an empty diff included.
#           stdout: docs-only: BASE..HEAD: only documentation changed
#   exit 1  at least one changed path is not documentation. stdout is that
#           path, verbatim, for the caller to report.
#   exit 2  unusable input: the wrong number of arguments, a commit that is
#           missing or not in this clone, or a failed diff. The reason goes to
#           stderr and stdout stays empty.
#
# The diff disables rename detection (--no-renames): the default lists only the
# new name of a rename, which would hide a code file renamed to *.md.
#
# The case list, in order of precedence: paths under the code directories and
# the pinned tool and build inputs are code even when they end in *.md; any
# other *.md is documentation; everything else is code.
set -euo pipefail

if [[ $# -ne 2 ]]; then
    printf 'usage: docs-only.sh BASE HEAD\n' >&2
    exit 2
fi
base=$1
head=$2
for commit in "$base" "$head"; do
    if [[ $commit == -* ]] || ! git rev-parse --verify --quiet "${commit}^{commit}" > /dev/null; then
        printf 'docs-only: %s is not a commit in this clone\n' "$commit" >&2
        exit 2
    fi
done
changes=$(mktemp)
trap 'rm -f "$changes"' EXIT
if ! git diff --name-only --no-renames -z "$base" "$head" -- > "$changes"; then
    printf 'docs-only: git diff %s %s failed\n' "$base" "$head" >&2
    exit 2
fi
while IFS= read -r -d '' path; do
    case "$path" in
        internal/*|app/*|go.*|Dockerfile|.dockerignore|THIRD_PARTY_NOTICES|mise.toml|.golangci.yml|.githooks/*|.github/*)
            printf '%s\n' "$path"
            exit 1 ;;
        *.md)
            ;; # Pure documentation is explicitly not a container-test pass.
        *)
            printf '%s\n' "$path"
            exit 1 ;;
    esac
done < "$changes"
printf 'docs-only: %s..%s: only documentation changed\n' "$base" "$head"
