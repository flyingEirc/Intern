#!/usr/bin/env bash
# Decides whether the heavy business and container jobs must run, and prints
# the locked profile matrix. required=false means no path changed since the
# base needs CI: all of them are documentation or the CI's own files;
# anything else, and anything this script cannot prove, is required=true. The
# classification itself, including its case list, is needs-ci.sh; this script
# only picks the base and reports the verdict.
#
# The base depends on the ref, because the workflow's concurrency rule decides
# whose verdict still stands:
#   push to main
#       github.event.before. Every main commit has its own run, never canceled,
#       so the commits before the push have their own verdicts and the push
#       delta is enough.
#   push to any other branch
#       git merge-base origin/main HEAD, so the whole branch over main is
#       judged. A newer push cancels the previous run (or follows its failure);
#       judging only the delta would let a documentation/CI-only follow-up push
#       pass a head whose code never had a completed matrix.
#   tag, schedule, workflow_dispatch, any other event
#       no base: always required=true.
# A base that is missing or unusable (zero or unreachable before, no
# origin/main, no merge base, failed diff) is required=true with the reason
# logged; it never fails the job. The scope job checks out with fetch-depth 0,
# which fetches every branch into refs/remotes/origin/*, so origin/main exists.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
needs_ci=$here/needs-ci.sh

main_ref=refs/remotes/origin/main

required=true
base=''
how=''
why=''
case "${EVENT_NAME}:${REF:-}" in
    push:refs/heads/main)
        before=${BEFORE_SHA:-}
        if [[ ! $before =~ ^[0-9a-f]{40}$ || $before == 0000000000000000000000000000000000000000 ]]; then
            why='no usable base, github.event.before is not a commit id (empty or all zeros)'
        elif ! git cat-file -e "${before}^{commit}" 2>/dev/null; then
            why="no usable base, github.event.before ${before} is not in this clone"
        else
            base=$before
            how='push delta from github.event.before'
        fi
        ;;
    push:refs/heads/*)
        if ! git rev-parse --verify --quiet "${main_ref}^{commit}" > /dev/null; then
            why="no usable base, ${main_ref} is not in this clone"
        elif merge_base=$(git merge-base "$main_ref" "$HEAD_SHA"); then
            base=$merge_base
            how='branch diff from the merge base with origin/main'
        else
            why="no usable base, git merge-base origin/main ${HEAD_SHA} failed"
        fi
        ;;
    push:*)
        why="${REF:-an unknown ref} is not a branch, always the full set"
        ;;
    *)
        why="${EVENT_NAME} events have no base, always the full set"
        ;;
esac
if [[ -n "$base" ]]; then
    status=0
    detail=$("$needs_ci" "$base" "$HEAD_SHA") || status=$?
    case $status in
        0)
            required=false
            why="${how} ${base}: no business change (documentation or CI files only)"
            ;;
        1)
            why="${how} ${base}: $(printf '%q' "$detail") needs CI"
            ;;
        *)
            why="no usable base, git diff ${base} ${HEAD_SHA} failed"
            ;;
    esac
fi
matrix=$(jq -c '{profile: [.profiles[].id]}' app/cli/internal/qualitygate/contract.json)
printf 'required=%s\nmatrix=%s\n' "$required" "$matrix" >> "$GITHUB_OUTPUT"
printf 'quality-scope: %s\n' "$why"
printf 'Business/container checks required: %s\n' "$required"
