#!/usr/bin/env bash
# Candidate-artifact gate (stage E gate #44). It opens the candidate-artifact.json
# the D1 deployment acceptance wrote and requires every identity in it to equal
# what the same run proved in its log, and the pinned images and versions to equal
# internal/physical/compat.json. The D1 evidence gate proves the facts happened;
# this gate proves the record a release would publish names exactly those facts.
#
# Usage: verify-candidate-artifact.sh <go-test-json-stream> <candidate-artifact.json> <compat.json>
# GITHUB_SHA must be set and equal the artifact's source_sha; empty is a refusal.
set -euo pipefail

if [[ "$#" -ne 3 ]]; then
	echo "usage: verify-candidate-artifact.sh <go-test-json-stream> <candidate-artifact.json> <compat.json>" >&2
	exit 2
fi

jq -e -n --arg source "${GITHUB_SHA:-}" \
	--slurpfile events "$1" --slurpfile artifacts "$2" --slurpfile matrices "$3" '
  ([$events[] | .Output // ""] | join("")) as $output |
  ($artifacts | if length == 1 then .[0] else error("expected exactly one artifact document") end) as $a |
  ([$matrices[0][] | select(.distribution == "mysql-community" and .platform == "linux/amd64")]) as $entries |
  ($entries | if length == 1 then .[0] else error("expected one linux/amd64 matrix entry") end) as $m |
  # The artifact line the harness logged after writing the file.
  ([$output | capture("D1-ARTIFACT file=candidate-artifact\\.json source_sha=(?<source>[0-9a-f]{40}) image_id=(?<image>sha256:[0-9a-f]{64}) docker_save_sha256=(?<save>[0-9a-f]{64}) docker_save_bytes=(?<bytes>[1-9][0-9]*) intern_web_sha256=(?<web>[0-9a-f]{64}) versions_bound=true")]) as $lines |
  ([$output | capture("D1-A1 versions mysqld=(?<server>[0-9.]+) xtrabackup=(?<tool>[0-9.]+-[0-9]+) ")]) as $versions |
  ([$output | capture("D1-A1 built tag=(?<tag>intern:[^ ]+) image_id=")]) as $built |
  $source != "" and
  ($lines | length) == 1 and ($versions | length) == 1 and ($built | length) == 1 and
  $a.source_sha == $source and $lines[0].source == $source and
  $a.platform == "linux/amd64" and
  $a.image_id == $lines[0].image and
  $a.image_tag == $built[0].tag and
  $a.docker_save_sha256 == $lines[0].save and
  ($a.docker_save_bytes | tostring) == $lines[0].bytes and
  $a.intern_web_sha256 == $lines[0].web and
  ($a.rootfs_layers | type) == "array" and ($a.rootfs_layers | length) > 0 and
  all($a.rootfs_layers[]; test("^sha256:[0-9a-f]{64}$")) and
  ($a.go_image | test("^golang:[^@ ]+@sha256:[0-9a-f]{64}$")) and
  $a.mysql_image == $m.mysql_image and $a.pxb_image == $m.pxb_image and
  $a.mysql_server_version == $m.server_version and $a.xtrabackup_version == $m.tool_version and
  $versions[0].server == $m.server_version and $versions[0].tool == $m.tool_version and
  $a.matrix_status == $m.status and $a.matrix_verified == $m.verified and
  ($a.license_files | type) == "array" and ($a.license_files | length) >= 8 and
  ($a.component_versions | type) == "object" and ($a.component_versions | length) > 0
' >/dev/null
echo "candidate artifact verified: source=${GITHUB_SHA} matrix=$(jq -r '.[0].status' "$3")"
