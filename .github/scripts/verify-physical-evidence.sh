#!/usr/bin/env bash
set -euo pipefail

test "$#" -eq 1
jq -e -s --arg source "${GITHUB_SHA:-}" '
  ([.[] | .Output // ""] | join("")) as $output |
  [
    "--- PASS: TestP2RealWorker (",
    "--- PASS: TestP2WrongVolumeWorker (",
    "--- PASS: TestP2UnreadableVolumeWorker (",
    "--- PASS: TestP2WritableVolumeWorker (",
    "--- PASS: TestP2EncryptedWorker (",
    "--- PASS: TestP2PIDOneWorker (",
    "--- PASS: TestP2RealWorker/preflight-refusals (",
    "--- PASS: TestP2RealWorker/real-downstream-failure (",
    "--- PASS: TestP2RealWorker/real-cancellation (",
    "source_dirty=false",
    "P2-A1 real versions",
    "P2-A4 backup=0",
    "P2-A4 backup=1",
    "P2-A4 backup=2",
    "P2-A3 real downstream",
    "P2-A3 real cancellation",
    "P2-A3 real watchdog",
    "xtrabackup processes gone",
    "extra_lsndir_bytes=exact",
    "stream_checkpoints=exact",
    "P2-A3 mysql_config_editor read the poisoned login file",
    "P2-A3 poisoned login path rejected",
    "login_path=ignored",
    "P2-A2 writable datadir mount rejected",
    "P2-A1 PID1 memfd backup",
    "--- PASS: TestP3RealWorker (",
    "--- PASS: TestP3DiskWorker (",
    "--- PASS: TestP3RecoveryWorker (",
    "P3-A1 backup=0",
    "P3-A1 backup=1",
    "P3-A1 backup=2",
    "raw_tool_bytes=exact authenticated_eof=true snapshot_sha256=",
    "P3-A2 fault=nonzero completed=false parent=refused ledger=empty pending=gone work=gone",
    "P3-A2 fault=enospc filesystem=tmpfs completed=false parent=refused ledger=empty pending=gone work=gone",
    "P3-A2 fault=cancel completed=false parent=refused ledger=empty pending=gone work=gone",
    "P3-A2 fault=short_write completed=false parent=refused ledger=empty pending=gone work=gone",
    "P3-A2 fault=close completed=false parent=refused ledger=empty pending=gone work=gone",
    "P3-A2 fault=fsync completed=false parent=refused ledger=empty pending=gone work=gone",
    "P3-A2 fault=metadata completed=false parent=refused ledger=empty pending=gone work=gone",
    "P3-A2 fault=publish completed=false parent=refused ledger=empty pending=gone work=gone",
    "P3-A2 fault=completion completed=false parent=refused ledger=empty pending=gone work=gone",
    "P3-A2 fault=late_cancel completed=true object=intact",
    "P3-A3 rejected=parent_missing requires_full=true",
    "P3-A3 rejected=parent_not_completed requires_full=true",
    "P3-A3 rejected=chain_broken requires_full=true",
    "P3-A3 rejected=server_uuid_changed requires_full=true",
    "P3-A3 rejected=server_version_changed requires_full=true",
    "P3-A3 rejected=tool_version_changed requires_full=true",
    "P3-A3 rejected=chain_length_reached requires_full=true",
    "P3-A4 kill=SIGKILL restart=real ledger=empty pending=gone work=gone active=untouched foreign=untouched second_recover=noop"
  ] as $required |
  length > 0 and
  all(.[]; .Action != "skip" and .Action != "fail") and
  ([.[] | select(.Action == "pass" and .Test == "TestP2NativeContainers" and .Package == "github.com/flyingEirc/Intern/app/web/integration")] | length == 1) and
  ([.[] | select(.Action == "pass" and .Test == null and .Package == "github.com/flyingEirc/Intern/app/web/integration")] | length == 1) and
  all($required[]; . as $marker | $output | contains($marker)) and
  ($output | contains("--- SKIP:") | not) and
  ($output | test("candidate source=[0-9a-f]{40} source_dirty=false")) and
  ($source == "" or ($output | contains("candidate source=" + $source + " source_dirty=false")))
' "$1"
