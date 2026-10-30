#!/usr/bin/env bash
# Runs a shell script inside the pinned Linux Go builder container with the
# committed tree copied into a throwaway Docker volume.
#
# The Windows self-hosted runner can neither execute Linux test binaries nor
# give NTFS POSIX modes, so the Go build, the race tests and the lint run here.
# The source goes in as `git archive HEAD`: the committed tree with its index
# modes, never the NTFS working tree. Module and build caches live in named
# volumes and survive between runs.
#
# usage: .github/scripts/linux-go.sh '<bash script, run in /src>'
set -euo pipefail
# Git Bash would rewrite arguments such as /src into Windows paths.
export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*'

script=${1:?usage: linux-go.sh '<bash script, run in /src>'}
# The same digest-pinned image the CLI container profiles build with.
image=$(jq -r '.builder_image' app/cli/internal/qualitygate/contract.json)
src="intern-src-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-$$"

docker volume create --label intern.ci=src "$src" >/dev/null
trap 'docker volume rm --force "$src" >/dev/null 2>&1 || true' EXIT

git archive --format=tar HEAD |
	docker run --rm --interactive --volume "$src:/src" "$image" tar --extract --directory /src

docker run --rm \
	--volume "$src:/src" \
	--volume intern-ci-gomod:/go/pkg/mod \
	--volume intern-ci-gocache:/root/.cache/go-build \
	--volume intern-ci-gobin:/go/bin \
	--workdir /src \
	--env GOTOOLCHAIN=local --env GOPROXY --env GOSUMDB --env GOFLAGS \
	"$image" bash -euo pipefail -c "$script"
