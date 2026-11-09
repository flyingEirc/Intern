#!/usr/bin/env bash
# Decides which job tiers of this run execute, and prints the locked profile
# matrix. Three outputs, each exactly true or false:
#   fast  quality-fast (build, race tests, lint, the gate self-tests)
#   cli   container-mysql, the locked CLI matrix, and its aggregate gate
#   web   the nine Web container jobs (container-physical ... container-ftps)
# cli or web implies fast. The classification itself, including its case list
# and the cli/web/all tiers, is needs-ci.sh; this script only picks the base,
# applies the rule of the event and reports the verdict.
#
# The rule and the base depend on the event and the ref, because the workflow's
# concurrency rule decides whose verdict still stands:
#   push to main
#       base github.event.before. Any business change runs every tier, whatever
#       its needs-ci.sh tier: every merge commit gets a complete run of its own,
#       never canceled, so the commits before the push have their own verdicts
#       and the push delta is enough.
#   push to any other branch
#       base git merge-base origin/main HEAD, so the whole branch over main is
#       judged; a newer push cancels the previous run, and judging only the
#       delta would let a documentation/CI-only follow-up push skip a head
#       whose code never had a completed run. A business change runs fast
#       only: container jobs never run for a branch push, so a push run is not
#       merge-gate evidence.
#   workflow_dispatch on any other branch
#       the pre-merge run (docs/workflow/pr.md). Same merge base as a branch
#       push; a business change runs fast plus the container tiers of its
#       needs-ci.sh tier (cli, web, or both for all). The full input (true)
#       runs every tier regardless of the diff.
#   workflow_dispatch on main, tag, schedule, any other event
#       no base: every tier.
#   workflow_dispatch with the ui input (true), on any branch (issue #74)
#       the UI acceptance run: fast only, whatever the diff, so the ui-e2e job
#       (which needs quality-fast) is the only heavy job of the run. No
#       container tier runs, so it is never merge-gate evidence. ui and full
#       together are refused (exit 1): they ask for opposite runs.
# A base that is missing or unusable (zero or unreachable before, no
# origin/main, no merge base, failed diff) is the widest verdict of that event
# with the reason logged: every tier, or fast for a branch push. It never fails
# the job. The scope job checks out with fetch-depth 0, which fetches every
# branch into refs/remotes/origin/*, so origin/main exists.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
needs_ci=$here/needs-ci.sh

main_ref=refs/remotes/origin/main

# rule: main (business runs every tier), push (business runs fast only) or
# dispatch (business runs fast plus its tier). Empty: no base, every tier.
rule=''
base=''
how=''
why=''
fast=true
cli=true
web=true

# merge_base sets base and how, or why when origin/main or the merge base is
# missing.
merge_base() {
    local found
    if ! git rev-parse --verify --quiet "${main_ref}^{commit}" > /dev/null; then
        why="no usable base, ${main_ref} is not in this clone"
    elif found=$(git merge-base "$main_ref" "$HEAD_SHA"); then
        base=$found
        how='branch diff from the merge base with origin/main'
    else
        why="no usable base, git merge-base origin/main ${HEAD_SHA} failed"
    fi
}

event=$EVENT_NAME
if [[ $event == workflow_dispatch && ${UI_INPUT:-} == true ]]; then
    event=workflow_dispatch_ui
fi
case "${event}:${REF:-}" in
    workflow_dispatch_ui:*)
        if [[ ${FULL_INPUT:-} == true ]]; then
            echo 'quality-scope: ui=true and full=true ask for opposite runs; dispatch them separately' >&2
            exit 1
        fi
        cli=false
        web=false
        why='workflow_dispatch with ui=true: the fast checks and the UI end-to-end job, no container tier (not merge-gate evidence)'
        ;;
    push:refs/heads/main)
        rule=main
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
        rule=push
        cli=false
        web=false
        merge_base
        ;;
    workflow_dispatch:refs/heads/main)
        why='workflow_dispatch on main, always every tier'
        ;;
    workflow_dispatch:refs/heads/*)
        if [[ ${FULL_INPUT:-} == true ]]; then
            why='workflow_dispatch with full=true, always every tier'
        else
            rule=dispatch
            merge_base
        fi
        ;;
    push:*)
        why="${REF:-an unknown ref} is not a branch, always every tier"
        ;;
    *)
        why="${EVENT_NAME} events have no base, always every tier"
        ;;
esac
if [[ -n $base ]]; then
    status=0
    detail=$("$needs_ci" "$base" "$HEAD_SHA") || status=$?
    case $status in
        0)
            fast=false
            cli=false
            web=false
            why="${how} ${base}: no business change (documentation or CI files only)"
            ;;
        1)
            tier=${detail%% *}
            path=${detail#* }
            case $rule in
                main)
                    why="${how} ${base}: $(printf '%q' "$path") needs CI (${tier}), main runs every tier"
                    ;;
                push)
                    why="${how} ${base}: $(printf '%q' "$path") needs CI (${tier}), a branch push runs the fast checks only"
                    ;;
                dispatch)
                    case $tier in
                        cli) web=false ;;
                        web) cli=false ;;
                    esac
                    why="${how} ${base}: $(printf '%q' "$path") needs the ${tier} tier"
                    ;;
            esac
            ;;
        *)
            why="no usable base, git diff ${base} ${HEAD_SHA} failed"
            ;;
    esac
fi
matrix=$(jq -c '{profile: [.profiles[].id]}' app/cli/internal/qualitygate/contract.json)
printf 'fast=%s\ncli=%s\nweb=%s\nmatrix=%s\n' "$fast" "$cli" "$web" "$matrix" >> "$GITHUB_OUTPUT"
printf 'quality-scope: %s\n' "$why"
printf 'Fast checks: %s; CLI container matrix: %s; Web container jobs: %s\n' "$fast" "$cli" "$web"
