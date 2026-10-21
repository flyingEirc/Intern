#!/usr/bin/env bash
set -euo pipefail

test "$#" -eq 1
jq -e -s '
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
    "P2-A1 PID1 memfd backup"
  ] as $required |
  length > 0 and
  all(.[]; .Action != "skip" and .Action != "fail") and
  ([.[] | select(.Action == "pass" and .Test == "TestP2NativeContainers" and .Package == "github.com/flyingEirc/Intern/app/web/integration")] | length == 1) and
  ([.[] | select(.Action == "pass" and .Test == null and .Package == "github.com/flyingEirc/Intern/app/web/integration")] | length == 1) and
  all($required[]; . as $marker | $output | contains($marker)) and
  ($output | contains("--- SKIP:") | not)
' "$1"
