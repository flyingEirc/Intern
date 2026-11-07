#!/usr/bin/env bash
# Accept and reject cases for install-ui-browser.sh. Fakes stand in for the
# network (curl serves local fixture archives), for unzip (it lays out a Chrome
# directory), for ldd and for fc-list; tar and sha256sum are the real ones, so
# the checksum gate is the script's own. The cases prove that an archive whose
# SHA-256 differs from the pin is never extracted, that missing Chrome
# libraries and a missing Chinese font fail the step together with the host
# command that fixes them, and that GITHUB_PATH and GITHUB_ENV are written only
# after every check passed.
#
# INSTALL_SCRIPT points the cases at another script than the one next to this file.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script=${INSTALL_SCRIPT:-$here/install-ui-browser.sh}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin" "$work/files"
export FAKE_FILES=$work/files

# The agent-browser fixture is a real npm-shaped tarball with the one member the
# script extracts; its "binary" prints the version like the real CLI.
mkdir -p "$work/package/package/bin"
printf '#!/bin/sh\necho "agent-browser 0.41.2"\n' > "$work/package/package/bin/agent-browser-linux-x64"
chmod +x "$work/package/package/bin/agent-browser-linux-x64"
tar -czf "$FAKE_FILES/agent-browser.tgz" -C "$work/package" package
printf 'not a real zip: the fake unzip lays out the directory\n' > "$FAKE_FILES/chrome-linux64.zip"
agent_sha=$(sha256sum "$FAKE_FILES/agent-browser.tgz" | cut -d' ' -f1)
chrome_sha=$(sha256sum "$FAKE_FILES/chrome-linux64.zip" | cut -d' ' -f1)
wrong_sha=0000000000000000000000000000000000000000000000000000000000000000

cat > "$work/bin/curl" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
[[ ${FAKE_CURL_DOWN:-} != 1 ]] || { echo 'curl: (6) Could not resolve host' >&2; exit 6; }
out='' url=''
while (($#)); do
	case $1 in
		-fsSL) ;;
		--retry|--retry-delay) shift ;;
		-o) out=$2; shift ;;
		-*) echo "fake curl: unsupported flag $1" >&2; exit 2 ;;
		*) url=$1 ;;
	esac
	shift
done
[[ -n $out && -n $url ]] || { echo 'fake curl: want -o FILE URL' >&2; exit 2; }
cp "$FAKE_FILES/${url##*/}" "$out"
FAKE

cat > "$work/bin/unzip" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
[[ $1 == -q && $2 == -o && $4 == -d && $# -eq 5 ]] || { echo "fake unzip: unsupported arguments $*" >&2; exit 2; }
[[ -f $3 ]] || { echo "fake unzip: no archive $3" >&2; exit 9; }
echo "$3" >> "$FAKE_FILES/extracted"
mkdir -p "$5/chrome-linux64"
printf '#!/bin/sh\n' > "$5/chrome-linux64/chrome"
chmod +x "$5/chrome-linux64/chrome"
FAKE

cat > "$work/bin/ldd" <<'FAKE'
#!/usr/bin/env bash
echo '	linux-vdso.so.1 (0x00007ffc)'
for lib in ${FAKE_LDD_MISSING:-}; do
	printf '\t%s => not found\n' "$lib"
done
echo '	libc.so.6 => /lib/x86_64-linux-gnu/libc.so.6 (0x00007f00)'
FAKE

cat > "$work/bin/fc-list" <<'FAKE'
#!/usr/bin/env bash
[[ $* == ':lang=zh family' ]] || { echo "fake fc-list: unsupported arguments $*" >&2; exit 2; }
[[ ${FAKE_NO_ZH_FONT:-} == 1 ]] || echo 'Noto Sans CJK SC,Noto Sans CJK SC Regular'
FAKE
chmod +x "$work/bin/curl" "$work/bin/unzip" "$work/bin/ldd" "$work/bin/fc-list"

cases=0
failed=0
status=0
log=''

# install [ENV=VALUE...] runs the script in a fresh target directory with the
# fakes first on PATH, fixture URLs, and the fixtures' own checksums unless a
# case overrides them.
install() {
	target=$work/target-$cases
	rm -rf "$target" "$FAKE_FILES/extracted"
	: > "$work/github_path"
	: > "$work/github_env"
	if log=$(
		env PATH="$work/bin:$PATH" GITHUB_PATH="$work/github_path" GITHUB_ENV="$work/github_env" \
			AGENT_BROWSER_URL=https://fixture.invalid/agent-browser.tgz AGENT_BROWSER_SHA256="$agent_sha" \
			CHROME_URL=https://fixture.invalid/chrome-linux64.zip CHROME_SHA256="$chrome_sha" \
			"$@" "$script" "$target" 2>&1
	); then
		status=0
	else
		status=$?
	fi
}

# expect NAME STATUS [LOG-PATTERN] checks the last install call. A success must
# leave an executable agent-browser that answers --version, the Chrome binary,
# and exactly one line in each GitHub file; a failure must leave both files
# empty. PATTERN, when given, must match the log.
expect() {
	local name=$1 want=$2 pattern=${3:-} problem=''
	cases=$((cases + 1))
	if [[ $status -ne $want ]]; then
		problem="exit status $status, want $want"
	elif [[ -n $pattern && ! $log =~ $pattern ]]; then
		problem="the log does not match: $pattern"
	elif ((want == 0)); then
		if [[ $("$target/agent-browser" --version 2>&1) != 'agent-browser 0.41.2' ]]; then
			problem='agent-browser is not the extracted binary'
		elif [[ ! -x $target/chrome-linux64/chrome ]]; then
			problem='no Chrome binary'
		elif [[ $(cat "$work/github_path") != "$target" ]]; then
			problem="GITHUB_PATH is [$(cat "$work/github_path")], want [$target]"
		elif [[ $(cat "$work/github_env") != "AGENT_BROWSER_EXECUTABLE_PATH=$target/chrome-linux64/chrome" ]]; then
			problem="GITHUB_ENV is [$(cat "$work/github_env")]"
		fi
	elif [[ -s $work/github_path || -s $work/github_env ]]; then
		problem='a failed install wrote GITHUB_PATH or GITHUB_ENV'
	fi
	if [[ -z $problem ]]; then
		printf 'ok   %s\n' "$name"
	else
		printf 'FAIL %s: %s\n%s\n' "$name" "$problem" "$log"
		failed=$((failed + 1))
	fi
}

# extracted_nothing fails the case when the fake unzip ran.
extracted_nothing() {
	if [[ -e $FAKE_FILES/extracted ]]; then
		printf 'FAIL %s: an unverified archive was extracted\n' "$1"
		failed=$((failed + 1))
	fi
}

install
expect '1 pinned archives, libraries and a Chinese font: installed' 0 'agent-browser 0\.41\.2 and Chrome for Testing 157\.0\.8103\.38'

install AGENT_BROWSER_SHA256=$wrong_sha
expect '2 agent-browser archive with another checksum is rejected' 1 "agent-browser\.tgz has sha256 $agent_sha, want $wrong_sha"
extracted_nothing '2 agent-browser archive with another checksum is rejected'
[[ ! -e $target/agent-browser ]] || { printf 'FAIL 2b: an unverified agent-browser was extracted\n'; failed=$((failed + 1)); }

install CHROME_SHA256=$wrong_sha
expect '3 Chrome archive with another checksum is rejected' 1 "chrome-linux64\.zip has sha256 $chrome_sha, want $wrong_sha"
extracted_nothing '3 Chrome archive with another checksum is rejected'

install FAKE_LDD_MISSING='libnss3.so libgbm.so.1'
expect '4 missing Chrome libraries name themselves and the host command' 1 'Chrome cannot load: libgbm\.so\.1 libnss3\.so.*sudo apt-get install -y .*libnss3'

install FAKE_NO_ZH_FONT=1
expect '5 no Chinese font is rejected with the host command' 1 'no font covers Chinese.*fonts-noto-cjk'

install FAKE_LDD_MISSING=libnss3.so FAKE_NO_ZH_FONT=1
expect '6 every problem is reported in one failure' 1 'Chrome cannot load: libnss3\.so.*no font covers Chinese'

# Without fontconfig there is no fc-list at all: a PATH that holds the fakes
# except fc-list plus only the system directories the script needs.
rm "$work/bin/fc-list"
sysbin=$work/sysbin
mkdir "$sysbin"
for tool in bash tar gzip sha256sum cut awk sort tr mkdir chmod cat env cp rm; do
	ln -s "$(command -v "$tool")" "$sysbin/$tool"
done
install PATH="$work/bin:$sysbin"
expect '7 fontconfig not installed is rejected' 1 'fontconfig is not installed'

install FAKE_CURL_DOWN=1 PATH="$work/bin:$sysbin"
expect '8 a failed download is rejected' 1 'download failed: https://fixture\.invalid/agent-browser\.tgz'

cases=$((cases + 1))
if "$script" > /dev/null 2>&1; then
	printf 'FAIL 9 no directory argument was accepted\n'
	failed=$((failed + 1))
else
	printf 'ok   9 no directory argument is a usage error\n'
fi

printf '\n%s cases, %s failed\n' "$cases" "$failed"
[[ $failed -eq 0 ]]
