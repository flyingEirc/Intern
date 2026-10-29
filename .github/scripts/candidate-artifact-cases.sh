#!/usr/bin/env bash
# Accept and reject cases for verify-candidate-artifact.sh. One synthetic run
# (the D1 log lines in real `go test -json` framing) and one candidate artifact
# that agrees with it and with internal/physical/compat.json must be accepted;
# every mutation of one bound fact must be refused, and every mutation must
# actually change its input, so the gate cannot be silently widened.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
gate="$root/.github/scripts/verify-candidate-artifact.sh"
compat="$root/internal/physical/compat.json"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

source_sha=0123456789abcdef0123456789abcdef01234567
wrong_source=fedcba9876543210fedcba9876543210fedcba98
image=sha256:$(printf 'a%.0s' {1..64})
save=$(printf 'b%.0s' {1..64})
web=$(printf 'c%.0s' {1..64})
layer=sha256:$(printf 'd%.0s' {1..64})
tag=intern:0123456789ab-mysql8.4
mysql_image=$(jq -r '.[0].mysql_image' "$compat")
pxb_image=$(jq -r '.[0].pxb_image' "$compat")
server=$(jq -r '.[0].server_version' "$compat")
tool=$(jq -r '.[0].tool_version' "$compat")
status=$(jq -r '.[0].status' "$compat")
verified=$(jq -r '.[0].verified' "$compat")
issue=$(jq -r '.[0].evidence_issue' "$compat")

{
	printf '    d1_test.go:690: D1-A1 built tag=%s image_id=%s repeat_image_id=%s source_sha=%s source_dirty=false mysql_image=%s pxb_image=%s\n' \
		"$tag" "$image" "$image" "$source_sha" "$mysql_image" "$pxb_image"
	printf '    d1_test.go:896: D1-A1 versions mysqld=%s xtrabackup=%s xbstream=%s matrix_server=%s matrix_tool=%s match=true\n' \
		"$server" "$tool" "$tool" "$server" "$tool"
	printf '    d1_test.go:1340: D1-A2 identity daemon=linux/x86_64 platform=linux/amd64 compose=2.35.1 project=intern-d1-1\n'
	printf '    d1_test.go:2363: D1-ARTIFACT file=candidate-artifact.json source_sha=%s image_id=%s docker_save_sha256=%s docker_save_bytes=812345678 intern_web_sha256=%s versions_bound=true\n' \
		"$source_sha" "$image" "$save" "$web"
} >"$work/log.txt"
jq -cn --rawfile raw "$work/log.txt" \
	'{Action: "output", Package: "github.com/flyingEirc/Intern/app/web/integration", Test: "TestD1NativeContainers", Output: $raw}' \
	>"$work/events.jsonl"

jq -n --arg source "$source_sha" --arg image "$image" --arg save "$save" --arg web "$web" \
	--arg layer "$layer" --arg tag "$tag" --arg mysql "$mysql_image" --arg pxb "$pxb_image" \
	--arg server "$server" --arg tool "$tool" --arg status "$status" --argjson verified "$verified" \
	--arg issue "$issue" '{
  source_sha: $source, platform: "linux/amd64", image_tag: $tag, image_id: $image,
  upgrade_image_tag: "intern:0123456789ab-upgrade-mysql8.4", docker_save_sha256: $save,
  docker_save_bytes: 812345678, intern_web_sha256: $web, rootfs_layers: [$layer, $layer],
  go_image: ("golang:1.27.0@sha256:" + ("e" * 64)), mysql_image: $mysql, pxb_image: $pxb,
  mysql_server_version: $server, xtrabackup_version: $tool,
  component_versions: {mysqld: $server, xtrabackup: $tool, xbstream: $tool, compose: "2.35.1",
    server: ("mysql-community:" + $server)},
  license_files: ["NOTICE-WEB.txt", "GPL-SOURCE-OFFER.txt", "VERSIONS.txt", "mysql-community-server/LICENSE",
    "percona-xtrabackup/COPYING", "procps-ng/COPYING", "procps-ng/COPYING.LIB", "go-internals/GO-MODULES.txt"],
  matrix_status: $status, matrix_verified: $verified,
  evidence_issue: $issue
}' >"$work/artifact.json"

run_gate() {
	GITHUB_SHA="$1" bash "$gate" "$2" "$3" "$compat" >/dev/null 2>&1
}

if ! run_gate "$source_sha" "$work/events.jsonl" "$work/artifact.json"; then
	echo "FAIL: the gate rejected the unmutated artifact" >&2
	GITHUB_SHA="$source_sha" bash "$gate" "$work/events.jsonl" "$work/artifact.json" "$compat" || true
	exit 1
fi
echo "ok: accept unmutated"
rejects=0

reject_artifact() {
	local name=$1 filter=$2
	jq "$filter" "$work/artifact.json" >"$work/artifact-$name.json"
	if cmp -s "$work/artifact.json" "$work/artifact-$name.json"; then
		echo "FAIL: the $name mutation did not change the artifact" >&2
		exit 1
	fi
	if run_gate "$source_sha" "$work/events.jsonl" "$work/artifact-$name.json"; then
		echo "FAIL: the gate accepted the $name case" >&2
		exit 1
	fi
	echo "ok: reject $name"
	rejects=$((rejects + 1))
}

reject_artifact foreign_source ".source_sha = \"$wrong_source\""
reject_artifact arm64_platform '.platform = "linux/arm64"'
reject_artifact other_image '.image_id = ("sha256:" + ("f" * 64))'
reject_artifact other_tag '.image_tag = "intern:ffffffffffff-mysql8.4"'
reject_artifact other_save_digest '.docker_save_sha256 = ("f" * 64)'
reject_artifact other_save_bytes '.docker_save_bytes = 812345679'
reject_artifact other_web_digest '.intern_web_sha256 = ("f" * 64)'
reject_artifact no_layers '.rootfs_layers = []'
reject_artifact malformed_layer '.rootfs_layers = ["sha256:abc"]'
reject_artifact unpinned_go '.go_image = "golang:1.27.0"'
reject_artifact drifted_mysql '.mysql_image = "mysql:8.4.11"'
reject_artifact drifted_pxb '.pxb_image = "percona/percona-xtrabackup:8.4.0-7"'
reject_artifact other_server '.mysql_server_version = "8.4.12"'
reject_artifact claimed_verified '.matrix_verified = (.matrix_verified | not)'
reject_artifact other_status '.matrix_status = "unsupported"'
reject_artifact missing_licenses '.license_files = .license_files[0:7]'
reject_artifact no_components '.component_versions = {}'
reject_artifact renamed_licenses '.license_files = [range(8) | "license-\(.)"]'
reject_artifact other_mysqld_component '.component_versions.mysqld = "0.0.1"'
reject_artifact other_xtrabackup_component '.component_versions.xtrabackup = "8.4.0-6"'
reject_artifact other_xbstream_component '.component_versions.xbstream = "8.4.0-6"'
reject_artifact other_compose_component '.component_versions.compose = "2.0.0"'
reject_artifact other_server_component '.component_versions.server = "mysql-community:9.7.1"'
reject_artifact extra_component '.component_versions.extra = "1"'
reject_artifact other_upgrade_tag '.upgrade_image_tag = "intern:ffffffffffff-upgrade-mysql8.4"'
reject_artifact other_evidence_issue '.evidence_issue = "https://github.com/flyingEirc/Intern/issues/1"'

# Log-side mutations: the artifact must match the run that wrote it.
sed 's/docker_save_bytes=812345678/docker_save_bytes=812345670/' "$work/log.txt" >"$work/log-bytes.txt"
jq -cn --rawfile raw "$work/log-bytes.txt" \
	'{Action: "output", Package: "github.com/flyingEirc/Intern/app/web/integration", Test: "TestD1NativeContainers", Output: $raw}' \
	>"$work/events-bytes.jsonl"
if run_gate "$source_sha" "$work/events-bytes.jsonl" "$work/artifact.json"; then
	echo "FAIL: the gate accepted an artifact whose save size differs from the logged one" >&2
	exit 1
fi
echo "ok: reject log_save_bytes"
rejects=$((rejects + 1))
sed 's/compose=2\.35\.1/compose=2.36.0/' "$work/log.txt" >"$work/log-compose.txt"
cmp -s "$work/log.txt" "$work/log-compose.txt" && { echo "FAIL: the log_compose mutation did not change the log" >&2; exit 1; }
jq -cn --rawfile raw "$work/log-compose.txt" \
	'{Action: "output", Package: "github.com/flyingEirc/Intern/app/web/integration", Test: "TestD1NativeContainers", Output: $raw}' \
	>"$work/events-compose.jsonl"
if run_gate "$source_sha" "$work/events-compose.jsonl" "$work/artifact.json"; then
	echo "FAIL: the gate accepted an artifact whose compose version differs from the logged one" >&2
	exit 1
fi
echo "ok: reject log_compose"
rejects=$((rejects + 1))
grep -v 'D1-ARTIFACT' "$work/log.txt" >"$work/log-none.txt"
jq -cn --rawfile raw "$work/log-none.txt" \
	'{Action: "output", Package: "github.com/flyingEirc/Intern/app/web/integration", Test: "TestD1NativeContainers", Output: $raw}' \
	>"$work/events-none.jsonl"
if run_gate "$source_sha" "$work/events-none.jsonl" "$work/artifact.json"; then
	echo "FAIL: the gate accepted an artifact the run never logged" >&2
	exit 1
fi
echo "ok: reject log_missing_artifact"
rejects=$((rejects + 1))

if run_gate "$wrong_source" "$work/events.jsonl" "$work/artifact.json"; then
	echo "FAIL: the gate accepted a foreign GITHUB_SHA" >&2
	exit 1
fi
echo "ok: reject sha_mismatch"
if run_gate "" "$work/events.jsonl" "$work/artifact.json"; then
	echo "FAIL: the gate accepted an empty GITHUB_SHA" >&2
	exit 1
fi
echo "ok: reject sha_empty"
rejects=$((rejects + 2))
echo "candidate artifact cases: 1 accept, $rejects rejects"
