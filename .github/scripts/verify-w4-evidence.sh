#!/usr/bin/env bash
# Evidence gate for the W4 native acceptance run. It accepts one `go test -json`
# event stream and requires every locked marker, the exact case set, a clean
# source identity bound to GITHUB_SHA, and no skip or failure anywhere.
#
# Every marker is emitted only after the worker's own assertions passed, and
# each value in it is read back from state: the API, SQLite, the local
# repository or raw FTP. The gate therefore checks the fixed text around the
# values plus the shape of the observed values themselves.
#
# The gate never reads a secret: it only inspects the worker's own log lines,
# which the harness already proves are free of the generated credentials.
# Synthetic accept and reject cases for this gate live in w4-gate-cases.sh:
# they feed it generated `go test -json` streams and require one accept and a
# rejection per mutated marker, so the gate itself stays honest.
set -euo pipefail

if [[ "$#" -ne 1 ]]; then
    echo 'usage: verify-w4-evidence.sh <events.jsonl>' >&2
    exit 2
fi

jq -e -s --arg source "${GITHUB_SHA:-}" '
  ([.[] | .Output // ""] | join("")) as $output |
  [
    "a1-a2-e2e", "a2-priority", "a3-cancel", "a4-sse", "a3-sigterm", "a3-sigkill"
  ] as $cases |
  [
    "W4-A1 e2e", "W4-A2 priority", "W4-A3 cancel", "W4-A3 sigterm", "W4-A3 sigkill", "W4-A4 sse",
    "chain=seq:1,2,3", "lsn_continuous=true", "conflict=409", "replicas_verified=3",
    "protection_requires_verified=true uploaded_window=observed",
    "logs_agree=sqlite", "lists_agree=sqlite,local,ftp",
    "running_max=1 claim_full_before_incremental=true queued_together=true order=incremental_first sqlite_intervals=disjoint",
    "canceled=true completed_backup=false staging_reclaimed=true work_reclaimed=true requires_full=true source_backups_unchanged=true",
    "sigterm_exit_bounded=true no_false_completed=true recovered=true",
    "sigkill=SIGKILL interrupted=true pending=gone work=gone published_untouched=true protection_held=true next_incremental=requires_full next_incremental_code=parent_not_completed admission_full=completed next_backup=completed remote_held=true sqlite_interrupted=true",
    "sse_end=true sse_ordered=true secrets=clean lifecycle=queued<claimed<preflight_ok<started<completed"
  ] as $markers |
  length > 0 and
  all(.[]; .Action != "skip" and .Action != "fail") and
  ([.[] | select(.Action == "pass" and .Test == "TestW4NativeContainers" and .Package == "github.com/flyingEirc/Intern/app/web/integration")] | length == 1) and
  ([.[] | select(.Action == "pass" and .Test == null and .Package == "github.com/flyingEirc/Intern/app/web/integration")] | length == 1) and
  ($output | contains("--- FAIL:") or contains("--- SKIP:") | not) and
  ($output | contains("W4 diagnostics: begin") | not) and
  ($output | split("--- PASS: TestW4Worker (") | length == 2) and
  ($output | split("--- PASS: TestW4Worker/") | length == ($cases | length) + 1) and
  all($cases[]; . as $case |
    ($output | split("--- PASS: TestW4Worker/" + $case + " (") | length == 2)) and
  all($markers[]; . as $marker | $output | contains($marker)) and
  # Every fault must have been injected while a real backup was running. The
  # harness matches the backup by resolved executable, `--backup` in argv and a
  # target dir inside the work root of the run, so the `--version` preflight
  # probe can never satisfy it; the marker quotes that match, it does not assert
  # it.
  all(["W4-A3 cancel", "W4-A3 sigterm", "W4-A3 sigkill"][]; . as $case |
    ($output | test($case + "[^\\n]*injected_during=backup backup_pid=[1-9][0-9]* backup_target=run-[0-9a-f]+/"))) and
  # SIGTERM and SIGKILL must record the outcome of the run they ended, and never
  # a false success. The fault is injected only after the harness matched a live
  # `--backup` process, so a failure can only be the chain failure the drain
  # records or the interruption restart recovery records; a fault that had
  # landed during preflight would record preflight or unavailable instead.
  #
  # SIGTERM may legitimately end either way, so each status carries its own
  # evidence: failed needs a class and code from the closed set, and canceled
  # needs the canceled class the store writes (CancelRunningRun leaves
  # error_class NULL, so the empty value is the same outcome).
  ($output | test("W4-A3 sigterm[^\\n]*status=(failed error_class=\"(backup_failed|interrupted|recovery_failed)\" error_code=\"[a-z_]+\"|canceled error_class=\"(canceled)?\" error_code=\"[a-z_]*\") exit=0 ")) and
  ($output | test("W4-A3 sigkill[^\\n]*status=failed[^\\n]*error_class=\"(interrupted|recovery_failed)\" error_code=\"[a-z_]+\" ")) and
  # The uploaded-but-unverified window is a counted observation of the real
  # state, never a claim: the sampler must have run, and it must name the
  # backups it caught between `uploaded` and the remote point.
  ($output | test("uploaded_window=observed samples=[1-9][0-9]* ")) and
  ($output | test("uploaded_backups=[^ ]+ ")) and
  ($output | contains("uploaded_window=missed") | not) and
  # The first SSE event is measured against the locked promptness bound.
  ($output | test("first_event_ms=[0-9]{1,4} ")) and
  ($output | test("W4 candidate source=[0-9a-f]{40} source_dirty=false binary_sha256=[0-9a-f]{64} ")) and
  ($output | test("worker_gid=[1-9][0-9]* tool_sha256=[0-9a-f]{64} ")) and
  ($source == "" or ($output | contains("W4 candidate source=" + $source + " source_dirty=false"))) and
  ($output | test("daemon=linux/(x86_64|amd64) platform=linux/amd64")) and
  ($output | test("worker_uid=[1-9][0-9]* worker_gid=[1-9][0-9]*")) and
  ($output | contains("server=Pure-FTPd-1.0.54 client=jlaffaye/ftp-v0.2.4 topology=private-network-no-NAT"))
' "$1"
