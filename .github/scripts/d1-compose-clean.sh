#!/usr/bin/env bash
# Removes the Docker resources of this runner's D1 Compose projects, and nothing
# else. On a GitHub runner the D1 harness (app/web/integration/d1_test.go) names
# its project d1 + the first 8 hex digits of sha256(RUNNER_NAME) + hex digits,
# and Compose labels every container, network and volume of a project with that
# name. Only projects of exactly that shape for this runner are touched: no other
# name, no other runner, no unlabelled container, no image, no cache.
#
# Why a job step. D1 starts its services with plain `docker compose`, the compose
# file gives them restart: unless-stopped, and the testcontainers reaper does not
# own Compose resources, so a stack outlives the test that started it. The test
# removes its stack in t.Cleanup, which a cancelled job never reaches: on Linux
# the runner stops only the step's shell, `go test` runs on as an orphan, and the
# job's last step ends it with SIGKILL. The former ephemeral CircleCI machine
# discarded such leftovers with the VM; this runner keeps them, with their
# memory and volumes, for every later job.
#
# A runner runs one job at a time, so while this script runs every matching
# project is either this job's own or a leftover of an earlier job (cancelled,
# timed out, or cut off by a host restart). The job runs it before the test, to
# clear leftovers (also what an orphan created after the previous job's own
# cleanup), and after it with if: always().
#
#   d1-compose-clean.sh        needs RUNNER_NAME (set by the runner) and docker
set -euo pipefail

runner=${RUNNER_NAME:?RUNNER_NAME is not set: this script runs inside a GitHub Actions job}
tag=$(printf '%s' "$runner" | sha256sum | cut -c1-8)
label=com.docker.compose.project

# A stack whose containers are gone can still own a network and volumes, so
# every resource kind names projects. Each listing is its own assignment: a
# failing docker must end the script, not look like an empty host.
containers=$(docker ps --all --format "{{.Label \"$label\"}}")
networks=$(docker network ls --format "{{.Label \"$label\"}}")
volumes=$(docker volume ls --format "{{.Label \"$label\"}}")
projects=$(printf '%s\n' "$containers" "$networks" "$volumes" | grep -E "^d1${tag}[0-9a-f]+\$" | sort -u || true)

if [[ -z $projects ]]; then
	echo "d1-compose-clean: no D1 Compose project of runner $runner"
	exit 0
fi

# Containers first: a volume in use or a network with endpoints cannot go. A
# command that fails is recorded and the rest still run, so one stuck container
# does not keep the remaining resources on the host.
status=0
for project in $projects; do
	echo "d1-compose-clean: removing Compose project $project"
	filter=label=$label=$project
	docker ps --all --quiet --filter "$filter" | xargs -r docker rm --force --volumes || status=1
	docker network ls --quiet --filter "$filter" | xargs -r docker network rm || status=1
	docker volume ls --quiet --filter "$filter" | xargs -r docker volume rm --force || status=1
done
exit "$status"
