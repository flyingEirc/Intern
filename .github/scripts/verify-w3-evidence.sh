#!/usr/bin/env bash
# Evidence gate for the W3 native acceptance run. It accepts one `go test -json`
# event stream and requires every locked marker, the exact case set, a clean
# source identity bound to GITHUB_SHA, and no skip or failure anywhere. An
# empty or unset GITHUB_SHA is a rejection, not a wildcard.
#
# The gate never reads a secret: it only inspects the worker's own log lines,
# which the harness already proves are free of the generated credentials.
set -euo pipefail

if [[ "$#" -ne 1 ]]; then
    echo 'usage: verify-w3-evidence.sh <events.jsonl>' >&2
    exit 2
fi

jq -e -s --arg source "${GITHUB_SHA:-}" '
  ([.[] | .Output // ""] | join("")) as $output |
  [
    "a3-pass", "a4-atomic-enable", "a3-denied-grants", "a3-wrong-volume",
    "a3-matrix-miss", "a3-unwritable-ftp", "a2-ciphertext"
  ] as $cases |
  [
    "W3-A3 pass", "W3-A3 refused", "W3-A4 atomic", "W3-A2 ciphertext",
    "case=privileges_missing", "case=wrong_volume",
    "case=matrix_miss check=compatibility code=version_unsupported server_version=9.7.1",
    "case=unwritable_ftp", "reply_code=550",
    "warning=compatibility_candidate_unverified", "plaintext_ftp=audited",
    "failure_audit=present", "enabled=0",
    "responses=clean", "log=clean", "audit=clean", "database_bytes=clean",
    "cross_row=refused", "cross_field=refused", "unseal=exact",
    "audit_abort=500", "enabled_after_abort=0", "preflight_after_abort=absent",
    "persisted=1", "last_preflight=stored", "success_audit=same_tx"
  ] as $markers |
  length > 0 and
  all(.[]; .Action != "skip" and .Action != "fail") and
  ([.[] | select(.Action == "pass" and .Test == "TestW3NativeContainers" and .Package == "github.com/flyingEirc/Intern/app/web/integration")] | length == 1) and
  ([.[] | select(.Action == "pass" and .Test == null and .Package == "github.com/flyingEirc/Intern/app/web/integration")] | length == 1) and
  ($output | contains("--- FAIL:") or contains("--- SKIP:") | not) and
  ($output | split("--- PASS: TestW3Worker (") | length == 2) and
  ($output | split("--- PASS: TestW3Worker/") | length == ($cases | length) + 1) and
  all($cases[]; . as $case |
    ($output | split("--- PASS: TestW3Worker/" + $case + " (") | length == 2)) and
  all($markers[]; . as $marker | $output | contains($marker)) and
  ($output | test("W3 candidate source=[0-9a-f]{40} source_dirty=false binary_sha256=[0-9a-f]{64} ")) and
  ($output | test("worker_gid=[1-9][0-9]* tool_sha256=[0-9a-f]{64} ")) and
  ($source != "" and ($output | contains("W3 candidate source=" + $source + " source_dirty=false"))) and
  ($output | test("daemon=linux/(x86_64|amd64) platform=linux/amd64")) and
  ($output | test("worker_uid=[1-9][0-9]* worker_gid=[1-9][0-9]*")) and
  ($output | contains("server=Pure-FTPd-1.0.54 client=jlaffaye/ftp-v0.2.4 topology=private-network-no-NAT")) and
  ($output | contains("case=matrix_miss check=compatibility code=version_unsupported server_version=9.7.1")) and
  ($output | test("W3-A3 pass source=[0-9a-f-]+ local=[0-9a-f-]+ ftp=[0-9a-f-]+ policy=[0-9a-f-]+ enabled=1 ")) and
  ($output | test("reply_code=550 code=[a-z_]+ check=replica enabled=0 failure_audit=present"))
' "$1"
