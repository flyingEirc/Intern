#!/usr/bin/env bash
# Installs the pinned browser stack of the ui-e2e job into DIR: the agent-browser
# CLI the end-to-end scripts drive (app/web/frontend/e2e) and the Chrome for
# Testing build it launches. Both archives are verified against the SHA-256
# pinned here before anything is extracted; nothing is taken from the network
# unverified and nothing is installed system-wide.
#
# agent-browser is the native binary from the npm package (the package ships
# one per platform under package/bin; the npm launcher and its postinstall need
# Node, which the runner does not have). The npm tarball's published integrity
# (sha512-/agCKB5rw1EEK8SKWo4zid7a5kOwlclqALC0P3c69buJvKdhrl/dJkKdfLqGGaiNIbWzRdqqT0PxZVkwVEf2fg==)
# was checked when the SHA-256 below was recorded. `agent-browser install` would
# fetch whatever Chrome is the current stable, so the job points the CLI at this
# pinned build through AGENT_BROWSER_EXECUTABLE_PATH instead (157.0.8103.38 is
# the build agent-browser 0.41.2 installed on the development machine that
# produced the earlier evidence; the zip's MD5 matched the bucket's x-goog-hash).
#
# The runner must provide Chrome's shared libraries and a CJK font: the pages
# are Chinese, and screenshots without one show empty boxes. Neither can be
# installed without root, so both are checked and every missing piece is named
# with the one-time host command, the require-runner.sh way.
#
#   install-ui-browser.sh DIR
#
# On success DIR joins GITHUB_PATH (agent-browser) and GITHUB_ENV gets
# AGENT_BROWSER_EXECUTABLE_PATH; on any failure neither file is written.
set -euo pipefail

agent_browser_version=0.41.2
agent_browser_url=${AGENT_BROWSER_URL:-https://registry.npmjs.org/agent-browser/-/agent-browser-${agent_browser_version}.tgz}
agent_browser_sha256=${AGENT_BROWSER_SHA256:-f0d26595c766e54eedcd44324b67fb93b73fd003bd05237414b2513e75444bb0}
chrome_version=157.0.8103.38
chrome_url=${CHROME_URL:-https://storage.googleapis.com/chrome-for-testing-public/${chrome_version}/linux64/chrome-linux64.zip}
chrome_sha256=${CHROME_SHA256:-8cdeb344e9b2cefe59f4aea8d1d93d5d93054c03cde217665c16fc1f62e72a66}
# Ubuntu 24.04 packages for Chrome's runtime libraries, fontconfig and fonts.
host_packages='libasound2t64 libatk-bridge2.0-0t64 libatk1.0-0t64 libatspi2.0-0t64 libcairo2 libcups2t64 libdbus-1-3 libdrm2 libgbm1 libglib2.0-0t64 libnspr4 libnss3 libpango-1.0-0 libx11-6 libxcb1 libxcomposite1 libxdamage1 libxext6 libxfixes3 libxkbcommon0 libxrandr2 fontconfig fonts-liberation fonts-noto-cjk'

if (($# != 1)) || [[ -z $1 ]]; then
	echo 'usage: install-ui-browser.sh DIR' >&2
	exit 1
fi
dir=$1
mkdir -p "$dir"

# fetch URL SHA256 FILE downloads URL to FILE and fails unless its SHA-256 is
# exactly the pinned one.
fetch() {
	local url=$1 want=$2 file=$3 got
	if ! curl -fsSL --retry 3 --retry-delay 5 -o "$file" "$url"; then
		echo "install-ui-browser: download failed: $url" >&2
		return 1
	fi
	got=$(sha256sum "$file" | cut -d' ' -f1)
	if [[ $got != "$want" ]]; then
		echo "install-ui-browser: $url has sha256 $got, want $want" >&2
		return 1
	fi
}

fetch "$agent_browser_url" "$agent_browser_sha256" "$dir/agent-browser.tgz"
tar -xzf "$dir/agent-browser.tgz" -O package/bin/agent-browser-linux-x64 > "$dir/agent-browser"
chmod 0755 "$dir/agent-browser"
fetch "$chrome_url" "$chrome_sha256" "$dir/chrome-linux64.zip"
unzip -q -o "$dir/chrome-linux64.zip" -d "$dir"
chrome=$dir/chrome-linux64/chrome
[[ -x $chrome ]] || { echo "install-ui-browser: $chrome is missing from the Chrome archive" >&2; exit 1; }

problems=()
missing=$(ldd "$chrome" | awk '/not found/ { print $1 }' | sort -u | tr '\n' ' ')
[[ -z $missing ]] || problems+=("Chrome cannot load: ${missing% }")
if ! command -v fc-list > /dev/null 2>&1; then
	problems+=('fontconfig is not installed, so no font can be found')
elif [[ -z $(fc-list :lang=zh family) ]]; then
	problems+=('no font covers Chinese (:lang=zh): the pages would render as empty boxes')
fi
if ((${#problems[@]})); then
	printf 'install-ui-browser: %s\n' "${problems[@]}" >&2
	printf 'install-ui-browser: install them once on the runner host: sudo apt-get install -y %s\n' "$host_packages" >&2
	exit 1
fi

"$dir/agent-browser" --version
printf '%s\n' "$dir" >> "$GITHUB_PATH"
printf 'AGENT_BROWSER_EXECUTABLE_PATH=%s\n' "$chrome" >> "$GITHUB_ENV"
echo "install-ui-browser: agent-browser ${agent_browser_version} and Chrome for Testing ${chrome_version} in $dir"
