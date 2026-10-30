#!/usr/bin/env bash
# Installs the pinned native jq on the Windows self-hosted runner (Git for
# Windows ships no jq) and puts it on PATH for the following steps.
#
# The evidence gates compare jq output byte for byte. A native jq.exe turns
# every LF into CRLF under Git Bash unless it is given --binary, so the PATH
# entry is a wrapper that always passes it. The binary is verified against its
# SHA-256 on every run, not only when it is downloaded.
set -euo pipefail

# 1.7.1 is the jq the gates were written against (ubuntu-24.04).
version=1.7.1
sha256=7451fbbf37feffb9bf262bd97c54f0da558c63f0748e64152dd87b0a07b6d6ab
url="https://github.com/jqlang/jq/releases/download/jq-${version}/jq-windows-amd64.exe"

dir="${RUNNER_TOOL_CACHE:?RUNNER_TOOL_CACHE is not set}/jq/${version}/x64"
mkdir -p "$dir"
cd "$dir"

verified() { [[ -f jq-real.exe ]] && echo "$sha256  jq-real.exe" | sha256sum --check --status -; }

if ! verified; then
	rm -f jq-real.exe jq-real.exe.download
	# github.com is reached over a flaky route from this runner: retry hard.
	curl --fail --silent --show-error --location \
		--retry 8 --retry-all-errors --retry-delay 5 --connect-timeout 20 --max-time 300 \
		--output jq-real.exe.download "$url"
	mv jq-real.exe.download jq-real.exe
	verified || { echo "jq-windows-amd64.exe does not match the pinned SHA-256" >&2; exit 1; }
fi

printf '#!/usr/bin/env bash\nexec "%s/jq-real.exe" --binary "$@"\n' "$PWD" >jq
chmod +x jq
cygpath -w "$dir" >>"${GITHUB_PATH:?GITHUB_PATH is not set}"
"$dir/jq" --version
