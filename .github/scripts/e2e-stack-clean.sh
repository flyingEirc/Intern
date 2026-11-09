#!/usr/bin/env bash
# Removes the Docker resources of this runner's UI end-to-end stacks, and nothing
# else. On a GitHub runner app/web/frontend/e2e/stack.ts names its Compose
# project intern-e2e-<first 8 hex digits of sha256(RUNNER_NAME)>-<8 hex digits>.
# Compose labels the containers, networks and volumes it creates with
# com.docker.compose.project; everything the stack creates outside Compose (its
# data volumes, the KEK-less intern container, the MySQL servers started on
# exports) carries io.intern.e2e.project with the same name. Only projects of
# exactly that shape for this runner are touched: no other name, no other
# runner, no local run's stack (no runner digits), no unlabelled resource, no
# image, no cache.
#
# Why a job step. The stack starts its services with plain `docker compose`, the
# compose file gives them restart: unless-stopped, and the stack removes itself
# only in its own cleanup(), which a cancelled job never reaches: the runner
# stops the step's shell, the e2e script runs on as an orphan, and the job's last
# step ends it with SIGKILL. The leftovers would keep their memory and volumes
# on the shared engine for every later job (d1-compose-clean.sh, same reason).
#
# A runner runs one job at a time, so while this script runs every matching
# project is either this job's own or a leftover of an earlier job of this
# runner. The ui-e2e job runs it before the suites, to clear leftovers, and
# after them with if: always().
#
#   e2e-stack-clean.sh        needs RUNNER_NAME (set by the runner) and docker
set -euo pipefail

runner=${RUNNER_NAME:?RUNNER_NAME is not set: this script runs inside a GitHub Actions job}
tag=$(printf '%s' "$runner" | sha256sum | cut -c1-8)
labels=(com.docker.compose.project io.intern.e2e.project)

# A stack whose containers are gone can still own a network and volumes, so
# every resource kind names projects, under both labels. Each listing is its
# own assignment: a failing docker must end the script, not look like an empty
# host.
names=''
for label in "${labels[@]}"; do
	containers=$(docker ps --all --format "{{.Label \"$label\"}}")
	networks=$(docker network ls --format "{{.Label \"$label\"}}")
	volumes=$(docker volume ls --format "{{.Label \"$label\"}}")
	names+=$(printf '%s\n' "$containers" "$networks" "$volumes")$'\n'
done
projects=$(printf '%s' "$names" | grep -E "^intern-e2e-${tag}-[0-9a-f]{8}\$" | sort -u || true)

if [[ -z $projects ]]; then
	echo "e2e-stack-clean: no e2e stack of runner $runner"
	exit 0
fi

# Containers first: a volume in use or a network with endpoints cannot go. A
# command that fails is recorded and the rest still run, so one stuck container
# does not keep the remaining resources on the host.
status=0
for project in $projects; do
	echo "e2e-stack-clean: removing e2e stack $project"
	for label in "${labels[@]}"; do
		docker ps --all --quiet --filter "label=$label=$project" | xargs -r docker rm --force --volumes || status=1
	done
	for label in "${labels[@]}"; do
		docker network ls --quiet --filter "label=$label=$project" | xargs -r docker network rm || status=1
		docker volume ls --quiet --filter "label=$label=$project" | xargs -r docker volume rm --force || status=1
	done
done
exit "$status"
