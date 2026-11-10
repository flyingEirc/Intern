#!/usr/bin/env bash
# Evidence gate for the W5 native restore-API run. It accepts one `go test -json`
# event stream and requires every locked marker, the exact case set, a clean
# source identity bound to GITHUB_SHA, and no skip or failure anywhere. An empty
# or unset GITHUB_SHA is a rejection, not a wildcard.
#
# Every marker is emitted only after the worker's own assertions passed, and each
# value is read back from state: the restore row served by the API, PostgreSQL, the
# local repository, the real FTP namespace, the export volume, or a brand-new
# MySQL 8.4.11 container started on that export and digested there.
#
# The gate never reads a secret: it only inspects log lines the worker already
# proved free of the generated credentials. Synthetic accept and reject cases live
# in w5-gate-cases.sh: they feed this gate generated streams and require one
# accept and a refusal per mutated fact, so the gate cannot be silently widened.
set -euo pipefail

if [[ "$#" -ne 1 ]]; then
    echo 'usage: verify-w5-evidence.sh <events.jsonl>' >&2
    exit 2
fi

jq -e -s --arg source "${GITHUB_SHA:-}" '
  (.) as $events |
  ([$events[] | .Output // ""] | join("")) as $output |
  "TestW5Worker" as $worker |
  [
    "a2-plan", "a6-e2e", "a3-execute", "a4-exclusion", "a5-state"
  ] as $cases |
  ([
    "W5-A2 plan target=inc2", "W5-A2 plan_readonly", "W5-A2 refused",
    "W5-A6 e2e", "W5-A6 restore=inc1", "W5-A6 restore=inc2", "W5-A6 export=",
    "W5-A3 execute", "W5-A3 restore=full", "W5-A4 exclusion",
    "W5-A5 state", "W5-A5 sigkill",
    "W5-A6 restored=inc1", "W5-A6 restored=inc2", "W5-A3 restored=full",
    "W5-A6 exports_started=3 snapshots_match=true",
    "server=Pure-FTPd-1.0.54"
  ] + [
    "links=ftp:3", "rehearsal=true",
    "risks=no_auto_replace_production,server_uuid_duplicate,pitr_unavailable",
    "chain=seq:1,2,3", "replicas_verified=3", "local_payloads=removed", "logs_agree=postgres",
    "admin_only=true", "cross_origin=cross_origin_rejected", "confirm=403,confirmation_failed",
    "no_row=true", "limiter=429", "digest_changed=409,plan_changed", "replay_same_id=true",
    "conflict=409", "audit_atomic=true",
    "run_blocked_by_restore=true", "restore_blocked_by_run=true", "postgres_intervals=disjoint",
    "canceled_queued=true", "canceled_running=true", "side_effects=explicit",
    "sigkill=failed/interrupted", "published_intact=true", "cleanup=exact",
    "refused=source_unavailable:409", "recovery_repeated=true",
    "verify_status=passed", "verify_status=not_requested"
  ]) as $markers |
  ($events | length) > 0 and
  all($events[]; .Action != "skip" and .Action != "fail") and
  ([$events[] | select(.Action == "pass" and .Test == "TestW5NativeContainers" and
      .Package == "github.com/flyingEirc/Intern/app/web/integration")] | length == 1) and
  ([$events[] | select(.Action == "pass" and .Test == null and
      .Package == "github.com/flyingEirc/Intern/app/web/integration")] | length == 1) and
  # The W5 worker is a compiled test binary executed inside the container, so its
  # per-case results reach this stream only as output text the harness logged from
  # the executed binary stdout, never as events of this package. Require exactly
  # one PASS line for the worker and one per locked case in that text; the FAIL and
  # SKIP guard below still refuses any case that did not pass.
  (($output | split("--- PASS: " + $worker + " (") | length) == 2) and
  all($cases[]; . as $case |
    (($output | split("--- PASS: " + $worker + "/" + $case + " (") | length) == 2)) and
  # The case set is closed: exactly the locked number of worker case PASS lines in
  # the text, so an extra or renamed case is refused rather than silently accepted.
  (($output | split("--- PASS: " + $worker + "/") | length) == ($cases | length + 1)) and
  ($output | contains("--- FAIL:") or contains("--- SKIP:") or contains("diagnostics: begin") | not) and
  all($markers[]; . as $marker | $output | contains($marker)) and
  # The plan is a read-only FTP-only view of the real chain: links, digest shape,
  # the export placeholder path and the refusal mapping are all observed values.
  ($output | test("W5-A2 plan target=inc2 links=ftp:3 digest=[0-9a-f]{64} lsn=[1-9][0-9]* required_export=[1-9][0-9]* available_export=[1-9][0-9]* export_path=[^ ]+/\\{restore_id\\} rehearsal=true risks=[a-z_,]+")) and
  ($output | test("W5-A2 plan_readonly restores=0 export_dirs=0 ftp_namespace=[0-9a-f]{16} ftp_writes=0 local_payloads=removed")) and
  ($output | test("W5-A2 refused backup_not_found=404 unconfigured=503")) and
  ($output | test("W5-A6 e2e chain=seq:1,2,3 inc1=[0-9a-f]{32} inc2=[0-9a-f]{32} replicas_verified=3 local_payloads=removed logs_agree=postgres log_rows=[1-9][0-9]* phase_seen=fetching,authenticating,extracting,preparing,exporting,rehearsing")) and
  # The state line is matched whole, in the order the worker logged it: a marker
  # that merely appears somewhere in the output proves nothing about the case
  # that emitted it.
  ($output | test("W5-A5 state phase_seen=fetching,authenticating,extracting,preparing sources=ftp completed_restores=[1-9][0-9]* export_bytes=[1-9][0-9]* verify_status=(passed|not_requested) side_effects=explicit canceled_queued=true canceled_running=true logs_agree=postgres")) and
  # Each restore names its published export and the source snapshot recorded at
  # that restore point in the same run; the harness starts a brand-new pinned
  # server on that export and digests the data it finds there.
  ($output | test("W5-A6 restore=inc1 source=ftp export=[0-9a-f]{32} expected_snapshot=[0-9a-f]{64} verify_status=passed")) and
  ($output | test("W5-A6 restore=inc2 source=ftp export=[0-9a-f]{32} expected_snapshot=[0-9a-f]{64} verify_status=not_requested")) and
  ($output | test("W5-A3 restore=full source=ftp export=[0-9a-f]{32} expected_snapshot=[0-9a-f]{64} verify_status=not_requested")) and
  ($output | test("W5-A6 export=[0-9a-f]{32} bytes=[1-9][0-9]* data_digest=[0-9a-f]{16} phase=rehearsing links=ftp:2 mode=0750")) and
  # The two restores that asked for no rehearsal end at exporting; only the
  # rehearsed one may claim rehearsing, so each marker is tied to its own phase.
  # The mode is the published directory mode, which every marker prints.
  ($output | test("W5-A6 export=[0-9a-f]{32} bytes=[1-9][0-9]* data_digest=[0-9a-f]{16} phase=exporting links=ftp:3 mode=0750")) and
  ($output | test("W5-A6 export=[0-9a-f]{32} bytes=[1-9][0-9]* data_digest=[0-9a-f]{16} phase=exporting links=ftp:1 mode=0750")) and
  ($output | split("container_snapshot=") | length == 4) and
  ($output | test("W5-A6 restored=inc1 container_snapshot=[0-9a-f]{64} expected_snapshot=[0-9a-f]{64} match=true")) and
  ($output | test("W5-A6 restored=inc2 container_snapshot=[0-9a-f]{64} expected_snapshot=[0-9a-f]{64} match=true")) and
  ($output | test("W5-A3 restored=full container_snapshot=[0-9a-f]{64} expected_snapshot=[0-9a-f]{64} match=true")) and
  ($output | test("W5-A6 exports_started=3 snapshots_match=true new_mysql=mysql:8\\.4\\.11@sha256:[0-9a-f]{64}")) and
  # The execute gate facts: administrator-only, CSRF, password confirmation with
  # its failed audit and limiter, the stale-digest refusal and the idempotency
  # contract. Retry-After is an observed positive integer, never a constant.
  ($output | test("W5-A3 execute admin_only=true cross_origin=cross_origin_rejected confirm=403,confirmation_failed no_row=true audit_denied=[1-9][0-9]* limiter=429 retry_after=[1-9][0-9]* digest_changed=409,plan_changed replay_same_id=true conflict=409 audit_atomic=true")) and
  ($output | test("W5-A6 restored=inc1 container_snapshot=([0-9a-f]{64}) expected_snapshot=\\1 match=true")) and
  ($output | test("W5-A6 restored=inc2 container_snapshot=([0-9a-f]{64}) expected_snapshot=\\1 match=true")) and
  ($output | test("W5-A3 restored=full container_snapshot=([0-9a-f]{64}) expected_snapshot=\\1 match=true")) and
  # The mutual-exclusion line must itself carry running_max=1: the gate requires
  # the observation, it does not merely refuse a worse-looking one.
  ($output | test("W5-A4 exclusion run_blocked_by_restore=true restore_blocked_by_run=true running_max=1 queued_samples=[1-9][0-9]* postgres_intervals=disjoint")) and
  # The SIGKILL line must itself carry every recovery fact: they cannot be moved
  # onto some other line and still count.
  ($output | test("W5-A5 sigkill=failed/interrupted side_effects=explicit cleanup=exact published_intact=true refused=source_unavailable:409 recovery_repeated=true")) and
  # A false success after SIGKILL, a deleted published export, or a second
  # concurrency slot are refusals even when every marker above is present.
  ($output | test("sigkill=completed") | not) and
  ($output | test("published_intact=false") | not) and
  ($output | test("running_max=[02-9]") | not) and
  ($output | test("side_effects=none") | not) and
  # The candidate identity is the exact commit under test, clean, with a native
  # amd64 daemon and a pinned non-root worker.
  ($output | test("W5 candidate source=[0-9a-f]{40} source_dirty=false binary_sha256=[0-9a-f]{64} ")) and
  # The stream must name the exact commit under test, not merely a well-shaped
  # one: an empty or foreign GITHUB_SHA is a rejection, never a wildcard.
  ($source != "" and ($output | contains("W5 candidate source=" + $source + " source_dirty=false"))) and
  ($output | test("daemon=linux/(x86_64|amd64) platform=linux/amd64")) and
  ($output | test("worker_uid=[1-9][0-9]* worker_gid=[1-9][0-9]*"))
' "$1"
