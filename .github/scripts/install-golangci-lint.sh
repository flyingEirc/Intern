#!/usr/bin/env bash
# Installs the pinned golangci-lint release into /go/bin of the Linux Go builder
# container (see linux-go.sh). The version is the one mise.toml locks; keep the
# two in step. The archive is checked against its published SHA-256, which also
# means the linter is neither compiled here nor fetched through the module
# checksum database.
set -euo pipefail

version=2.13.2
sha256=2277d43b98ec0054280f2ac26b53268bae97682444678a59a657dd565da021d6
bin=/go/bin/golangci-lint

if [[ -x "$bin" ]] && "$bin" --version 2>/dev/null | grep -q " version ${version} "; then
	exit 0
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
archive="golangci-lint-${version}-linux-amd64"
curl --fail --silent --show-error --location \
	--retry 8 --retry-all-errors --retry-delay 5 --connect-timeout 20 --max-time 300 \
	--output "$tmp/${archive}.tar.gz" \
	"https://github.com/golangci/golangci-lint/releases/download/v${version}/${archive}.tar.gz"
echo "${sha256}  ${tmp}/${archive}.tar.gz" | sha256sum --check --status -
tar --extract --gzip --file "$tmp/${archive}.tar.gz" --directory "$tmp" --strip-components=1 "${archive}/golangci-lint"
install -m 0755 "$tmp/golangci-lint" "$bin"
"$bin" --version
