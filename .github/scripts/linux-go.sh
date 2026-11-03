#!/usr/bin/env bash
# Runs bash scripts in a Linux container on this machine's Docker engine, so the
# Go build, the unit tests, the lint and the real-component acceptance harness
# behave as on a Linux CI host even when the runner itself is Windows.
#
# Why the harness cannot run on a Windows host: it is written for a Linux (or
# macOS) host. It builds container paths with filepath.Join (backslashes on
# Windows), asks filepath.IsAbs about Linux paths, calls os.Getuid (-1 on
# Windows) and bind-mounts temp directories whose chown and chmod an NTFS mount
# does not honour, which the D1 deployment acceptance asserts (owner 999:999,
# modes 0600 and 0700). Inside this container none of that applies.
#
# The tree is the committed HEAD, materialised by `git checkout` from the
# workspace's own .git, never the NTFS working tree: modes and line endings are
# the index's, and `git status` is clean (the harness refuses a dirty tree).
# It lives on the Docker engine's own filesystem under $root, which every
# container here mounts at the same path. A directory the harness creates and
# bind-mounts into a container it starts therefore names the same files to the
# harness and to the engine.
#
# usage (from the repository root, after checkout):
#   linux-go.sh run [--docker] '<bash script>'   run it in the source tree
#       --docker adds the engine socket, host networking and the Testcontainers
#       host override: the container acceptance jobs. Plain runs get none of it.
#   linux-go.sh import <name> <host dir>         host dir -> $HARNESS_DATA/<name>
#   linux-go.sh export <name> <host dir>         $HARNESS_DATA/<name> -> host dir
#   linux-go.sh clean                            remove this job's scratch tree
#
# Inside the script: $HARNESS_DATA is a per-job data directory and $TMPDIR the
# per-job temp directory (what t.TempDir uses), both under the scratch tree.
# INTERN_* and TESTCONTAINERS_* variables, GITHUB_SHA and GITHUB_ACTIONS pass
# through; set per-job paths from inside the script, not in the caller's
# environment, because the caller's paths are Windows paths.
set -euo pipefail
# Git Bash would rewrite arguments and environment values that look like paths.
export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*' MSYS2_ENV_CONV_EXCL='*'
# macOS tar would add AppleDouble (._*) files for extended attributes; they corrupt .git.
export COPYFILE_DISABLE=1

root=${LINUX_GO_ROOT:-/tmp/intern-ci}
key=${LINUX_GO_KEY:-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-${GITHUB_JOB:-job}}
key=${key//[^A-Za-z0-9._-]/_}
scratch=$root/$key

# The Docker CLI, Compose and buildx the acceptance harness drives, pinned by
# digest. They are the versions Docker Desktop on the Windows runner ships
# (Docker 29.9.2, Compose v5.6.1, buildx v0.38.1).
docker_cli='docker:29.9.2-cli@sha256:5e6baf67325a79f64ed3bed6863954d5e05d0f3d4e57cbdb14809346814bbfdb'

sha256() { if command -v sha256sum >/dev/null 2>&1; then sha256sum; else shasum -a 256; fi; }

tools=
ensure_tools() {
	[[ -n "$tools" ]] && return 0
	local builder dockerfile
	# The digest-pinned image the CLI container profiles build with.
	builder=$(jq -r '.builder_image' app/cli/internal/qualitygate/contract.json)
	dockerfile="FROM ${builder}
COPY --from=${docker_cli} /usr/local/bin/docker /usr/local/bin/docker
COPY --from=${docker_cli} /usr/local/libexec/docker/cli-plugins/docker-buildx /usr/local/libexec/docker/cli-plugins/docker-buildx
COPY --from=${docker_cli} /usr/local/libexec/docker/cli-plugins/docker-compose /usr/local/libexec/docker/cli-plugins/docker-compose"
	tools=intern-ci-tools:$(printf '%s' "$dockerfile" | sha256 | cut -c1-12)
	docker image inspect "$tools" >/dev/null 2>&1 ||
		printf '%s\n' "$dockerfile" | docker build --quiet --tag "$tools" - >/dev/null
}

# helper runs a housekeeping script on the engine's filesystem; stdin is the
# caller's, which is how tar streams in.
helper() { docker run --rm --interactive --volume "$root:$root" "$tools" bash -euo pipefail -c "$1"; }

ensure_source() {
	ensure_tools
	# The marker sits outside the tree: an untracked file inside it would make
	# the tree dirty. A job calls this once per step; only the first builds.
	if helper "test -f '$scratch/.ready'" </dev/null 2>/dev/null; then return 0; fi
	tar -cf - .git | helper "
		rm -rf '$scratch' && mkdir -p '$scratch/src' '$scratch/data' '$scratch/tmp' &&
		tar -xf - --no-same-owner -C '$scratch/src' &&
		cd '$scratch/src' &&
		git config core.filemode true && git config core.symlinks true && git config core.autocrlf false &&
		git checkout --quiet --force HEAD &&
		touch '$scratch/.ready'"
}

run() {
	local docker_mode=0
	if [[ "${1:-}" == --docker ]]; then docker_mode=1; shift; fi
	local script=${1:?usage: linux-go.sh run [--docker] '<bash script>'}
	ensure_source
	# The script travels in the environment: no argument quoting between Git
	# Bash, the Windows docker client and the engine to get wrong.
	export HARNESS_SCRIPT=$script
	# The label lets clean remove what a cancelled step left running: killing
	# the docker client does not stop its container.
	local args=(--rm --label "intern.ci.key=$key" --workdir "$scratch/src" --volume "$root:$root"
		--volume intern-ci-gomod:/go/pkg/mod
		--volume intern-ci-gocache:/root/.cache/go-build
		--volume intern-ci-gobin:/go/bin
		--env HARNESS_SCRIPT --env "HARNESS_DATA=$scratch/data" --env "TMPDIR=$scratch/tmp"
		--env GOTOOLCHAIN=local --env GOPROXY --env GOSUMDB --env GOFLAGS
		--env GITHUB_SHA --env GITHUB_ACTIONS)
	if ((docker_mode)); then
		args+=(--network host --volume /var/run/docker.sock:/var/run/docker.sock
			--env TESTCONTAINERS_HOST_OVERRIDE=127.0.0.1)
	fi
	local name
	while IFS= read -r name; do args+=(--env "$name"); done \
		< <(compgen -e | grep -E '^(INTERN_|TESTCONTAINERS_)' || true)
	docker run "${args[@]}" "$tools" bash -euo pipefail -c 'eval "$HARNESS_SCRIPT"'
}

import() {
	local name=${1:?usage: linux-go.sh import <name> <host dir>} from=${2:?usage: linux-go.sh import <name> <host dir>}
	ensure_source
	tar -C "$from" -cf - . | helper "
		rm -rf '$scratch/data/$name' && mkdir -p '$scratch/data/$name' &&
		tar -xf - --no-same-owner -C '$scratch/data/$name'"
}

export_dir() {
	local name=${1:?usage: linux-go.sh export <name> <host dir>} to=${2:?usage: linux-go.sh export <name> <host dir>}
	ensure_tools
	mkdir -p "$to"
	helper "tar -C '$scratch/data/$name' -cf - ." </dev/null | tar -xf - -C "$to"
}

clean() {
	# No engine means nothing of ours is on it; do not fail a job for that.
	docker info >/dev/null 2>&1 || { echo "no Docker engine reachable; no scratch tree to remove"; return 0; }
	ensure_tools
	# A cancelled step leaves its container running; Testcontainers' reaper then
	# removes what that container started once its connection drops.
	local ids
	ids=$(docker ps --all --quiet --filter "label=intern.ci.key=$key")
	if [[ -n "$ids" ]]; then docker rm --force $ids >/dev/null || true; fi
	# Also sweep trees a killed job left behind.
	helper "rm -rf '$scratch'; find '$root' -mindepth 1 -maxdepth 1 -mmin +720 -exec rm -rf {} +" </dev/null
}

command=${1:-}
shift || true
case "$command" in
run) run "$@" ;;
import) import "$@" ;;
export) export_dir "$@" ;;
clean) clean ;;
*)
	echo "usage: linux-go.sh run [--docker] '<script>' | import <name> <dir> | export <name> <dir> | clean" >&2
	exit 2
	;;
esac
