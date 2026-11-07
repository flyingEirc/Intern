#!/usr/bin/env bash
# Removes the Docker resources of this runner's CLI real-container quality gate,
# and nothing else. The harness (app/cli/internal/testenv) labels every scenario
# container, network and named volume with
# intern.quality.runner=<sha256(RUNNER_NAME)[:8]> (inside GitHub Actions) plus
# intern.quality.case=<case prefix>; its candidate build probe carries the
# runner label too. It creates named volumes explicitly because testcontainers
# v0.44.0 does not (the daemon would create them unlabelled). This script
# filters on that one runner label, so no other runner, no unlabelled resource,
# no D1 Compose project and no other image is touched.
#
# Why a job step. The test removes its resources in t.Cleanup. A cancelled job
# never reaches it: on Linux the runner stops only the step's shell, `go test`
# runs on as an orphan, and the job's last step ends it with SIGKILL. The former
# ephemeral CircleCI machine discarded such leftovers with the VM; this runner
# keeps them, with their memory and volumes, for every later job. A runner runs
# one job at a time, so while this script runs every matching resource is either
# this job's own or a leftover of an earlier job (cancelled, timed out, or cut
# off by a host restart). The job runs it before the test, to clear leftovers,
# and after it with if: always(), then checks --inventory.
#
# Candidate images cannot carry a runner label (Docker builds them from the
# repository Dockerfile, which does not know the runner), so they are scoped by
# the name the harness gives them: intern-quality:<profile>-<sha12>-<arch>.
# Only images of this job's INTERN_TEST_PROFILE and native architecture are
# considered, only when GITHUB_SHA names the current commit. The newest
# keep_candidates images by creation time are kept, and the current SHA's image
# is always kept (it counts toward K when it is one of the newest): runs of
# different branches interleave on this runner, and pruning every other branch's
# image would force a cold rebuild in its next job. An old candidate goes
# through remove-images.sh, never `docker image rm`: that one also prunes every
# untagged ancestor, and those ancestors are the build cache that the
# candidates of other runs, and their builds in flight, share (issue #70).
# There is no image prune, so no unrelated image can be lost.
#
#   cli-quality-clean.sh              remove this runner's CLI quality leftovers
#   cli-quality-clean.sh --inventory  fail if any labelled resource remains
#
# Needs RUNNER_NAME (set by the runner) and docker. Exit status is non-zero when
# a removal failed or, for --inventory, when a labelled resource is still there;
# a removal failure is recorded and the remaining removals still run.
set -euo pipefail

if (($# > 1)); then
	echo 'usage: cli-quality-clean.sh [--inventory]' >&2
	exit 2
fi
inventory=0
case "${1:-}" in
'') ;;
--inventory) inventory=1 ;;
*)
	echo 'usage: cli-quality-clean.sh [--inventory]' >&2
	exit 2
	;;
esac

runner=${RUNNER_NAME:?RUNNER_NAME is not set: this script runs inside a GitHub Actions job}
tag=$(printf '%s' "$runner" | sha256sum | cut -c1-8)
label=intern.quality.runner
# Candidate images kept per profile besides the current commit's image.
keep_candidates=3
filter=label=$label=$tag
here=$(cd "$(dirname "$0")" && pwd)
# REMOVE_IMAGES points the cases at a stand-in for remove-images.sh.
remove_images=${REMOVE_IMAGES:-$here/remove-images.sh}

# Every listing is its own assignment: a failing docker must end the script, not
# look like an empty host. Candidate image listing is conditional because its
# scope needs both the profile and a current commit id; when either is missing
# the resources are still cleaned and the images are left alone.
containers=$(docker ps --all --quiet --filter "$filter")
networks=$(docker network ls --quiet --filter "$filter")
volumes=$(docker volume ls --quiet --filter "$filter")

profile=${INTERN_TEST_PROFILE:-}
platform=${INTERN_TEST_PLATFORM:-}
current=${GITHUB_SHA:-}
images=''
images_scoped=0
if [[ -n $profile && $platform == */* && $current =~ ^[0-9a-f]{40}$ ]]; then
	images_scoped=1
	images=$(docker image ls --format '{{.CreatedAt}} {{.Repository}}:{{.Tag}}')
fi
arch=${platform#*/}

if ((inventory)); then
	leftovers=0
	for entry in "container:$containers" "network:$networks" "volume:$volumes"; do
		kind=${entry%%:*}
		for id in ${entry#*:}; do
			echo "cli-quality-clean: leftover $kind $id (runner $runner)"
			leftovers=$((leftovers + 1))
		done
	done
	if ((leftovers)); then
		echo "::error title=CLI quality leftovers::${leftovers} labelled resources of runner ${runner} remain after cleanup" >&2
		exit 1
	fi
	echo "cli-quality-clean: no CLI quality resource of runner $runner remains"
	exit 0
fi

for entry in "container:$containers" "network:$networks" "volume:$volumes"; do
	kind=${entry%%:*}
	for id in ${entry#*:}; do
		echo "cli-quality-clean: found $kind $id (runner $runner)"
	done
done

# Containers first: a volume in use or a network with endpoints cannot go. A
# failing command is recorded and the remaining kinds still run, so one stuck
# container does not keep everything else on the host.
status=0
if [[ -n $containers ]]; then
	echo "cli-quality-clean: removing containers of runner $runner"
	# shellcheck disable=SC2086 # ids are newline/space separated by design
	docker rm --force --volumes $containers || status=1
fi
if [[ -n $networks ]]; then
	echo "cli-quality-clean: removing networks of runner $runner"
	# shellcheck disable=SC2086
	docker network rm $networks || status=1
fi
if [[ -n $volumes ]]; then
	echo "cli-quality-clean: removing volumes of runner $runner"
	# shellcheck disable=SC2086
	docker volume rm $volumes || status=1
fi

if ((images_scoped)); then
	entries=''
	while IFS= read -r entry; do
		[[ -n $entry ]] || continue
		ref=${entry##* }
		rest=${ref#intern-quality:}
		[[ $rest != "$ref" ]] || continue
		[[ $rest == "$profile-"* && $rest == *"-$arch" ]] || continue
		sha=${rest#"$profile-"}
		sha=${sha%"-$arch"}
		[[ $sha =~ ^[0-9a-f]{12}$ ]] || continue
		entries="$entries$entry"$'\n'
	done <<< "$images"
	kept=0
	while IFS= read -r entry; do
		[[ -n $entry ]] || continue
		ref=${entry##* }
		rest=${ref#intern-quality:}
		sha=${rest#"$profile-"}
		sha=${sha%"-$arch"}
		if ((kept < keep_candidates)) || [[ $sha == "${current:0:12}" ]]; then
			echo "cli-quality-clean: keeping candidate image $ref"
			if ((kept < keep_candidates)); then
				kept=$((kept + 1))
			fi
		else
			echo "cli-quality-clean: removing candidate image $ref"
			"$remove_images" "$ref" || status=1
		fi
	done <<< "$(printf '%s' "$entries" | LC_ALL=C sort -r)"
else
	echo "cli-quality-clean: candidate images kept: INTERN_TEST_PROFILE, INTERN_TEST_PLATFORM and GITHUB_SHA must all scope them"
fi

if [[ -z $containers && -z $networks && -z $volumes ]]; then
	echo "cli-quality-clean: no CLI quality resources of runner $runner"
fi
exit "$status"
