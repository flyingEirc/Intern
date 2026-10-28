#!/usr/bin/env bash
# An empty or unset GITHUB_SHA is a rejection: a stream that names no commit
# proves nothing about this one.
set -euo pipefail

test "$#" -eq 1
jq -e -s --arg source "${GITHUB_SHA:-}" '
  ([.[] | .Output // ""] | join("")) as $output |
  [
    "a1-fifo-verified", "a2-local-corrupt", "a2-remote-tamper", "a2-remote-truncate",
    "a2-wrong-kek", "a2-quota-552", "a3-idempotent-enqueue", "a3-blocked-predecessor",
    "a3-kill-recover", "a4-backoff-exhausted", "a4-manual-retry", "a4-points",
    "a5-retention", "a6-handoff"
  ] as $cases |
  [
    "R1-A0 produced",
    "R1-A1 backup=0", "R1-A1 backup=1", "R1-A1 backup=2",
    "remote_sha256=exact authenticated_eof=true uploaded_before_verified=true",
    "R1-A2 local_corrupt", "R1-A2 remote_tamper", "R1-A2 remote_truncate",
    "R1-A2 wrong_kek", "R1-A2 quota_552", "reply=552",
    "R1-A3 idempotent", "R1-A3 blocked", "R1-A3 kill=SIGKILL",
    "R1-A4 backoff", "R1-A4 manual_retry", "R1-A4 points",
    "R1-A5 chains", "R1-A6 handoff"
  ] as $markers |
  length > 0 and
  all(.[]; .Action != "skip" and .Action != "fail") and
  ([.[] | select(.Action == "pass" and .Test == "TestR1NativeContainers" and .Package == "github.com/flyingEirc/Intern/app/web/integration")] | length == 1) and
  ([.[] | select(.Action == "pass" and .Test == null and .Package == "github.com/flyingEirc/Intern/app/web/integration")] | length == 1) and
  ($output | contains("--- FAIL:") or contains("--- SKIP:") | not) and
  ($output | split("--- PASS: TestR1Worker (") | length == 2) and
  ($output | split("--- PASS: TestR1Worker/") | length == ($cases | length) + 1) and
  all($cases[]; . as $case |
    ($output | split("--- PASS: TestR1Worker/" + $case + " (") | length == 2)) and
  all($markers[]; . as $marker | $output | contains($marker)) and
  ($output | test("R1 candidate source=[0-9a-f]{40} source_dirty=false binary_sha256=[0-9a-f]{64} ")) and
  ($source != "" and ($output | contains("R1 candidate source=" + $source + " source_dirty=false"))) and
  ($output | test("daemon=linux/(x86_64|amd64) platform=linux/amd64")) and
  ($output | test("worker_uid=[1-9][0-9]* worker_gid=[1-9][0-9]*")) and
  ($output | contains("server=Pure-FTPd-1.0.54 client=jlaffaye/ftp-v0.2.4 topology=private-network-no-NAT")) and
  ($output | contains("R1-A2 local_corrupt")) and
  ($output | contains("retried=false alert=set local_status=completed local_present=true local_bytes_untouched=true remote_payload=absent")) and
  ($output | contains("R1-A3 kill=SIGKILL")) and
  ($output | contains("cleaned=real-ftp recovered=verified")) and
  ($output | contains("R1-A4 backoff")) and
  ($output | contains("exhausted_attempts=")) and
  ($output | contains("R1-A5 chains")) and
  ($output | contains("refused=pinned,latest,unverified")) and
  ($output | contains("older_members=3")) and
  ($output | contains("middle_link_reclaimed=true")) and
  ($output | contains("whole_chain_only=true")) and
  ($output | contains("rows_retained=true")) and
  ($output | contains("kept_untouched=true")) and
  ($output | contains("automatic=none")) and
  ($output | test("R1-A6 handoff source=[a-z0-9-]+ chain=[0-9a-f]+ backup=[0-9a-f]+ seq=[0-9]+ keys=[^ ]+ members=[^ ]+ snapshot_sha256=[0-9a-f]{64} mysql=[0-9.]+ pxb=[0-9.-]+ point="))
' "$1"
