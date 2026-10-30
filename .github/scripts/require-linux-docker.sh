#!/usr/bin/env bash
# Fails unless this runner reaches a native Linux amd64 Docker engine. The real
# component gates must block on a missing or mismatched runtime; they never skip
# and never fall back to mocks (docs/guide/quality.md), so a runner without the
# engine gets one explicit failure here instead of a dozen opaque ones later.
set -euo pipefail
export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*'

fail() {
	echo "::error title=No native Linux amd64 Docker engine::$*"
	exit 1
}

command -v docker >/dev/null 2>&1 ||
	fail 'docker is not on PATH for the account this runner runs as. Install Docker Desktop (Linux containers, WSL2 backend) for that account; see docs/guide/quality.md, CI execution environment.'

platform=$(docker version --format '{{.Server.Os}}/{{.Server.Arch}}' 2>&1) ||
	fail "the Docker engine is not reachable by this account: ${platform//$'\n'/ }"

[[ "$platform" == linux/amd64 ]] ||
	fail "the Docker engine reports ${platform}; the gates accept only a native linux/amd64 engine."

echo "Docker engine: ${platform}"
