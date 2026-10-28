#!/usr/bin/env bash
# Evidence gate for the P4 native restore run. It accepts one `go test -json`
# event stream and requires every locked marker, the exact case set, a clean
# source identity bound to GITHUB_SHA, and no skip or failure anywhere. An empty
# or unset GITHUB_SHA is a rejection, not a wildcard.
#
# Every marker is emitted only after the worker's own assertions passed, and each
# value is read back from state: the prepared export on disk, a brand-new MySQL
# container started on that export, the real FTP namespace, Statfs observations,
# or the recorded prepare argv. The gate checks the fixed text around those
# values and the shape of the observed values themselves.
#
# The gate never reads a secret: it only inspects log lines the worker already
# proved free of the generated credentials. Synthetic accept and reject cases
# live in p4-gate-cases.sh: they feed this gate generated streams and require one
# accept and a refusal per mutated fact, so the gate cannot be silently widened.
set -euo pipefail

if [[ "$#" -ne 1 ]]; then
    echo 'usage: verify-p4-evidence.sh <events.jsonl>' >&2
    exit 2
fi

jq -e -s --arg source "${GITHUB_SHA:-}" '
  (.) as $events |
  ([$events[] | .Output // ""] | join("")) as $output |
  [
    "a1-plan", "a2-local", "a5-argv", "a6-enospc", "a3-ftp", "a4-reject", "a6-cancel", "a6-sigkill", "a7-security"
  ] as $cases |
  [
    "backup_not_found", "backup_not_completed", "chain_broken_missing_middle", "chain_broken_wrong_parent",
    "lsn_discontinuous", "key_unavailable", "tool_version_mismatch", "matrix_unsupported", "insufficient_space",
    "source_unavailable_local_truncated", "source_unavailable_replica_unverified",
    "authentication_failed_ftp_tampered", "source_unavailable_ftp_truncated", "unconfigured_export_symlink"
  ] as $rejects |
  ({
    "backup_not_found": "backup_not_found",
    "backup_not_completed": "backup_not_completed",
    "chain_broken_missing_middle": "chain_broken",
    "chain_broken_wrong_parent": "chain_broken",
    "lsn_discontinuous": "lsn_discontinuous",
    "key_unavailable": "key_unavailable",
    "tool_version_mismatch": "tool_version_mismatch",
    "matrix_unsupported": "matrix_unsupported",
    "insufficient_space": "insufficient_space",
    "source_unavailable_local_truncated": "source_unavailable",
    "source_unavailable_replica_unverified": "source_unavailable",
    "authentication_failed_ftp_tampered": "authentication_failed",
    "source_unavailable_ftp_truncated": "source_unavailable",
    "unconfigured_export_symlink": "unconfigured"
  }) as $codes |
  [
    "P4-A1 plan target=inc1", "P4-A1 plan target=inc2", "P4-A1 plan_readonly", "P4-A1 space", "P4-A1 matrix",
    "P4-A1 ftp_plan_readonly", "P4-A2 rehearsal passed", "P4-A2 export_perms",
    "P4-A3 plan target=inc1", "P4-A3 local_payloads=removed",
    "P4-A5 argv shapes=full-only,full+1,full+2",
    "P4-A6 enospc filesystem=tmpfs", "P4-A6 cancel during=extracting", "P4-A6 cancel during=preparing",
    "P4-A6 sigkill inject=prepare",
    "P4-A7 secrets=clean", "P4-A7 supervisor mysqld_pgid_separate=true", "P4-A7 export_root symlink=refused",
    "P4-A2 restore_count=4 exports_started=4 snapshots_match=true",
    "P4-A7 root_refusal mysqld=refused execute=rehearsal_unavailable"
  ] as $markers |
  ($events | length) > 0 and
  all($events[]; .Action != "skip" and .Action != "fail") and
  ([$events[] | select(.Action == "pass" and .Test == "TestP4NativeContainers" and .Package == "github.com/flyingEirc/Intern/app/web/integration")] | length == 1) and
  ([$events[] | select(.Action == "pass" and .Test == null and .Package == "github.com/flyingEirc/Intern/app/web/integration")] | length == 1) and
  ([$events[] | select(.Action == "pass" and .Test == "TestP4Worker")] | length == 1) and
  all($cases[]; . as $case | ([$events[] | select(.Action == "pass" and .Test == "TestP4Worker/" + $case)] | length == 1)) and
  ($output | contains("--- FAIL:") or contains("--- SKIP:") | not) and
  all($markers[]; . as $marker | $output | contains($marker)) and
  all($rejects[]; . as $name | ($output | split("P4-A4 rejected=" + $name + " ") | length == 2)) and
  all($rejects[]; . as $name |
    ($output | test("P4-A4 rejected=" + $name + " code=" + $codes[$name] + " stage=(plan|execute) [^\\n]*prepare_calls=0"))) and
  ($output | test("prepare_calls=1") | not) and
  # Each restore names its export and the snapshot taken at that backup, and the
  # harness compares a brand-new server on that export with the same value.
  ($output | test("P4-A2 restore=inc1 source=local export=[0-9a-f]{32} expected_snapshot=[0-9a-f]{64}")) and
  ($output | test("P4-A2 restore=inc2 source=local export=[0-9a-f]{32} expected_snapshot=[0-9a-f]{64}")) and
  ($output | test("P4-A3 restore=inc1 source=ftp injection=disconnect rest_offset=[1-9][0-9]* attempts=[2-9][0-9]* authenticated_eof=true export=[0-9a-f]{32} expected_snapshot=[0-9a-f]{64}")) and
  ($output | test("P4-A3 restore=inc2 source=ftp export=[0-9a-f]{32} expected_snapshot=[0-9a-f]{64}")) and
  ($output | split("container_snapshot=") | length == 5) and
  ($output | test("P4-A2 restored=inc1-local container_snapshot=[0-9a-f]{64} expected_snapshot=[0-9a-f]{64} match=true")) and
  ($output | test("P4-A2 restored=inc2-local container_snapshot=[0-9a-f]{64} expected_snapshot=[0-9a-f]{64} match=true")) and
  ($output | test("P4-A3 restored=inc1-ftp container_snapshot=[0-9a-f]{64} expected_snapshot=[0-9a-f]{64} match=true")) and
  ($output | test("P4-A3 restored=inc2-ftp container_snapshot=[0-9a-f]{64} expected_snapshot=[0-9a-f]{64} match=true")) and
  # The prepare sequence and the POST-prepare checkpoints are fixed facts.
  ($output | test("prepare_checkpoints=full-prepared to_lsn=[1-9][0-9]*")) and
  ($output | test("P4-A1 matrix server=8\\.4\\.11 tool=8\\.4\\.0-7 prepare_version=8\\.4\\.0-7 key_id=[0-9a-f]{8}")) and
  ($output | test("P4-A1 space required_work=[1-9][0-9]* required_export=[1-9][0-9]* available_work=[1-9][0-9]* available_export=[1-9][0-9]* matches_statfs=true")) and
  # The case set is closed: a skip anywhere is already refused above.
  ($output | test("P4 candidate source=[0-9a-f]{40} source_dirty=false binary_sha256=[0-9a-f]{64} ")) and
  ($source != "" and ($output | contains("P4 candidate source=" + $source + " source_dirty=false"))) and
  ($output | test("daemon=linux/(x86_64|amd64) platform=linux/amd64")) and
  ($output | test("worker_uid=[1-9][0-9]* worker_gid=[1-9][0-9]*")) and
  ($output | contains("server=Pure-FTPd-1.0.54"))
' "$1"
