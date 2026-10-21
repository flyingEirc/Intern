#!/usr/bin/env bash
set -euo pipefail

test "$#" -eq 1
jq -e -s --arg source "${GITHUB_SHA:-}" '
  ([.[] | .Output // ""] | join("")) as $output |
  [
    ["A1", "publish"],
    ["A2", "interrupted_upload"], ["A2", "quota_552"], ["A2", "permission_550"],
    ["A2", "source_error"], ["A2", "short_write"], ["A2", "data_close_error"],
    ["A2", "stage_close_error"], ["A2", "cancel_upload"], ["A2", "cleanup_retry"],
    ["A3", "instance_isolation"], ["A3", "marker_conflict"], ["A3", "path_tls_rejection"],
    ["A4", "ciphertext_resume"], ["A5", "close_drain"], ["A5", "rolling_idle"],
    ["A5", "pipeline_completion"], ["A5", "rename_ack_lost"]
  ] as $cases |
  length > 0 and
  all(.[]; .Action != "skip" and .Action != "fail") and
  ([.[] | select(.Action == "pass" and .Test == "TestS1NativeContainers" and .Package == "github.com/flyingEirc/Intern/app/web/integration")] | length == 1) and
  ([.[] | select(.Action == "pass" and .Test == null and .Package == "github.com/flyingEirc/Intern/app/web/integration")] | length == 1) and
  ($output | contains("--- FAIL:") or contains("--- SKIP:") | not) and
  ($output | split("--- PASS: TestS1RepositoryWorker (") | length == 2) and
  ($output | split("--- PASS: TestS1RepositoryWorker/") | length == ($cases | length) + 1) and
  all($cases[]; . as $case |
    ($output | split("--- PASS: TestS1RepositoryWorker/" + $case[1] + " (") | length == 2) and
    ($output | contains("S1-" + $case[0] + " " + $case[1] + ":"))) and
  ($output | test("source=[0-9a-f]{40} source_dirty=false binary_sha256=[0-9a-f]{64} server_binary_sha256=[0-9a-f]{64}")) and
  ($source == "" or ($output | contains("source=" + $source + " source_dirty=false"))) and
  ($output | test("daemon=linux/(x86_64|amd64) platform=linux/amd64")) and
  ($output | contains("server=Pure-FTPd-1.0.54 client=jlaffaye/ftp-v0.2.4 uid=1000 passive=EPSV/PASV:50000-50020 topology=private-network-no-NAT")) and
  ($output | contains("S1-A5 fixture_cleanup:"))
' "$1"
