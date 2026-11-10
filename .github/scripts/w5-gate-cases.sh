#!/usr/bin/env bash
# Synthetic accept and reject cases for verify-w5-evidence.sh. The script builds a
# `go test -json` event stream for a worker log that quotes every locked W5
# marker, runs the gate on it and requires one accept; it then mutates the stream
# one way per reject case and requires every mutation to be refused. The reject
# set covers each locked fact of the W5 gate, including a foreign and an empty
# GITHUB_SHA, so the gate cannot be quietly widened.
#
# The generated credentials the gate must never see do not exist here: the
# payloads carry no secret at all, which is the state a clean worker log is in.
set -euo pipefail

# Count the declared reject cases before the directory changes: $0 is relative to
# the caller, so the count has to happen while that path still resolves.
declared_rejects=$(grep -c '^reject ' "$0")
cd "$(dirname "$0")"
gate=./verify-w5-evidence.sh
package=github.com/flyingEirc/Intern/app/web/integration
cases=(a2-plan a6-e2e a3-execute a4-exclusion a5-state)

source=1f2e3d4c5b6a79880123456789abcdef01234567
wrong_source=deadbeefdeadbeefdeadbeefdeadbeefdeadbeef
binary=00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff
other_snapshot=9999999999999999999999999999999999999999999999999999999999999999
export_inc1=0123456789abcdef0123456789abcd01
export_inc2=0123456789abcdef0123456789abcd02
export_full=0123456789abcdef0123456789abcd03
backup_inc1=11111111111111111111111111111111
backup_inc2=22222222222222222222222222222222
digest=$(printf 'a%.0s' {1..64})
snap1=$(printf 'b%.0s' {1..64})
snap2=$(printf 'c%.0s' {1..64})
snapf=$(printf 'd%.0s' {1..64})
mysql_image='mysql:8.4.11@sha256:85b9bf2e29cf836ecb8c2a15a935d4ba0c606631dff1dd79531a11983c638f2a'
legacy_source=06c34299f03becdaf0fdcdffff5fbd4319bedafb
export_continued=0123456789abcdef0123456789abcd04

accepted=0
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# payload prints exactly the marker lines a passing W5 run emits.
payload() {
	printf '%s\n' \
		"W5-A2 plan target=inc2 links=ftp:3 digest=${digest} lsn=9000 required_export=209715200 available_export=900000000 export_path=/export/{restore_id} rehearsal=true risks=no_auto_replace_production,server_uuid_duplicate,pitr_unavailable" \
		"W5-A2 plan_readonly restores=0 export_dirs=0 ftp_namespace=0123456789abcdef ftp_writes=0 local_payloads=removed" \
		"W5-A2 refused backup_not_found=404 unconfigured=503" \
		"W5-A6 e2e chain=seq:1,2,3 inc1=${backup_inc1} inc2=${backup_inc2} replicas_verified=3 local_payloads=removed logs_agree=postgres log_rows=14 phase_seen=fetching,authenticating,extracting,preparing,exporting,rehearsing" \
		"W5-A6 export=${export_inc1} bytes=268435512 data_digest=0011223344556677 phase=rehearsing links=ftp:2 mode=0750" \
		"W5-A6 export=${export_inc2} bytes=268435612 data_digest=1122334455667788 phase=exporting links=ftp:3 mode=0750" \
		"W5-A6 restore=inc1 source=ftp export=${export_inc1} expected_snapshot=${snap1} verify_status=passed" \
		"W5-A6 restore=inc2 source=ftp export=${export_inc2} expected_snapshot=${snap2} verify_status=not_requested" \
		"W5-A3 execute admin_only=true cross_origin=cross_origin_rejected confirm=403,confirmation_failed no_row=true audit_denied=7 limiter=429 retry_after=11 digest_changed=409,plan_changed replay_same_id=true conflict=409 audit_atomic=true" \
		"W5-A6 export=${export_full} bytes=268435400 data_digest=2233445566778899 phase=exporting links=ftp:1 mode=0750" \
		"W5-A3 restore=full source=ftp export=${export_full} expected_snapshot=${snapf} verify_status=not_requested" \
		"W5-A4 exclusion run_blocked_by_restore=true restore_blocked_by_run=true running_max=1 queued_samples=48 postgres_intervals=disjoint" \
		"W5-A5 state phase_seen=fetching,authenticating,extracting,preparing sources=ftp completed_restores=3 export_bytes=805306524 verify_status=not_requested side_effects=explicit canceled_queued=true canceled_running=true logs_agree=postgres" \
		"W5-A5 sigkill=failed/interrupted side_effects=explicit cleanup=exact published_intact=true refused=source_unavailable:409 recovery_repeated=true" \
		"W5-A6 restored=inc1 container_snapshot=${snap1} expected_snapshot=${snap1} match=true" \
		"W5-A6 restored=inc2 container_snapshot=${snap2} expected_snapshot=${snap2} match=true" \
		"W5-A3 restored=full container_snapshot=${snapf} expected_snapshot=${snapf} match=true" \
		"W5-A6 exports_started=3 snapshots_match=true new_mysql=${mysql_image}" \
		"W5 candidate source=${source} source_dirty=false binary_sha256=${binary} mysql_image=${mysql_image} pxb_image=percona-xtrabackup@sha256:bf1 worker_image=sha256:aa ftp_image=sha256:bb daemon=linux/amd64 platform=linux/amd64 worker_uid=999 worker_gid=999 tool_sha256=${binary} xbstream_sha256=${binary} mysqld_sha256=${binary} server=Pure-FTPd-1.0.54 client=jlaffaye/ftp-v0.2.4 topology=private-network-no-NAT"
	printf '%s\n' \
		"PG-G3 legacy source=${legacy_source} schema=web:9,physical:1,replication:2 instance=11111111-1111-4111-8111-111111111111 sqlite_sha256=${digest} origin=retired-production-serve chain=full,inc1,inc2 fault_history=explicit-owned-crash-fixture" \
		"PG-G3 import command=production tables=18 equal=true source_intact=true repeat=refused nonempty=refused rollback=empty" \
		"PG-G3 continuity login=original setup=closed history=visible chain=1,2,3,4 lsn=continuous replicas=4 cleanup=recovered" \
		"PG-G3 binary path=/tmp/legacy-web sha256=${binary} legacy_source=${legacy_source}" \
		"PG-G3 binary path=/tmp/candidate-web sha256=${digest} legacy_source=${legacy_source}" \
		"PG-G3 export point=full id=${export_full} snapshot=${snapf} source=ftp" \
		"PG-G3 export point=inc1 id=${export_inc1} snapshot=${snap1} source=ftp" \
		"PG-G3 export point=inc2 id=${export_inc2} snapshot=${snap2} source=ftp" \
		"PG-G3 export point=continued id=${export_continued} snapshot=${digest} source=ftp" \
		"PG-G3 restored point=full actual_snapshot=${snapf} expected_snapshot=${snapf} match=true" \
		"PG-G3 restored point=inc1 actual_snapshot=${snap1} expected_snapshot=${snap1} match=true" \
		"PG-G3 restored point=inc2 actual_snapshot=${snap2} expected_snapshot=${snap2} match=true" \
		"PG-G3 restored point=continued actual_snapshot=${digest} expected_snapshot=${digest} match=true"
	local table
	for table in instance users sessions session_tombstones audit_logs repositories sources policies \
		runs run_logs restores restore_logs chains backups cleanup_ledger replicas replica_members replica_parts; do
		printf 'PG-G3 table=%s rows=1 sha256=%s equal=true\n' "$table" "$digest"
	done
}

# stream wraps one payload file in the pass events the gate counts: one pass per
# case, one worker pass, one harness pass and one package pass.
stream() {
	local payload_file=$1
	jq -cn --rawfile raw "$payload_file" --arg pkg "$package" '
		def ev(action; test; output):
			{Action: action, Package: $pkg, Test: test}
			+ (if output == null then {} else {Output: output} end);
		# The worker is a compiled test binary exec-ed inside the container, so its
		# per-case PASS lines reach the go test -json stream only as output text the
		# harness logs, never as events of this package: synthesize exactly that
		# shape - one harness output event carrying the whole worker log, then the
		# harness pass event and the package pass event.
		(["a2-plan", "a6-e2e", "a3-execute", "a4-exclusion", "a5-state"]
			| map("--- PASS: TestW5Worker/" + . + " (1.00s)") | join("\n")) as $cases
		| ($raw + "\n" + $cases + "\n--- PASS: TestW5Worker (1.00s)\n") as $text
		| [ ev("run"; "TestW5NativeContainers"; null),
			ev("output"; "TestW5NativeContainers"; $text),
			ev("pass"; "TestW5NativeContainers"; null),
			ev("pass"; null; null) ]
		| .[]
	' | jq -c .
}

run_gate() {
	local sha=$1 file=$2 log=$3
	GITHUB_SHA="$sha" bash "$gate" "$file" >"$log" 2>&1
}

payload >"$work/payload.txt"
stream "$work/payload.txt" >"$work/accept.jsonl"

if run_gate "$source" "$work/accept.jsonl" "$work/accept.log"; then
	accepted=1
	echo "ok: accept unmutated"
else
	echo 'FAIL: the gate rejected the unmutated stream:' >&2
	cat "$work/accept.log" >&2
	exit 1
fi

# reject runs one mutation of the accepted stream and requires the gate to refuse
# it. Every mutation removes or falsifies exactly one locked fact.
reject() {
	local name=$1 filter=$2
	jq -c --arg pkg "$package" "$filter" "$work/accept.jsonl" >"$work/reject-$name.jsonl"
	if run_gate "$source" "$work/reject-$name.jsonl" "$work/reject-$name.log"; then
		echo "FAIL: the gate accepted the $name case" >&2
		exit 1
	fi
	echo "ok: reject $name"
}

reject missing_case 'if has("Output") then .Output |= sub("--- PASS: TestW5Worker/a5-state \\(1\\.00s\\)\n"; "") else . end'
reject extra_case 'if has("Output") then .Output |= (. + "--- PASS: TestW5Worker/a9-extra (1.00s)\n") else . end'
reject renamed_case 'if has("Output") then .Output |= sub("--- PASS: TestW5Worker/a4-exclusion \\("; "--- PASS: TestW5Worker/a4-excluded-case (") else . end'
reject skipped_case 'if has("Output") then .Output |= sub("--- PASS: TestW5Worker/a6-e2e \\("; "--- SKIP: TestW5Worker/a6-e2e (") else . end'
reject failed_case 'if has("Output") then .Output |= sub("--- PASS: TestW5Worker/a3-execute \\("; "--- FAIL: TestW5Worker/a3-execute (") else . end'
reject harness_skipped '. , {Action: "skip", Package: $pkg, Test: "TestW5NativeContainers"}'
reject harness_failed '. , {Action: "fail", Package: $pkg, Test: "TestW5NativeContainers"}'
reject no_harness_pass 'select(.Test != "TestW5NativeContainers" or .Action != "pass")'
reject no_package_pass 'select(.Action != "pass" or .Test != null)'
reject no_plan 'if has("Output") then .Output |= select((. // "") | contains("W5-A2 plan target=inc2") | not) else . end'
reject local_source 'if has("Output") then .Output |= sub("plan target=inc2 links=ftp:3"; "plan target=inc2 links=local:3") else . end'
reject weak_digest "if has(\"Output\") then .Output |= sub(\"digest=${digest}\"; \"digest=abc\") else . end"
reject ftp_wrote 'if has("Output") then .Output |= sub("ftp_writes=0"; "ftp_writes=3") else . end'
reject plan_wrote_rows 'if has("Output") then .Output |= sub("plan_readonly restores=0"; "plan_readonly restores=2") else . end'
reject short_phases 'if has("Output") then .Output |= sub("phase_seen=fetching,authenticating,extracting,preparing,exporting,rehearsing"; "phase_seen=fetching,authenticating,extracting,preparing") else . end'
reject no_rehearsal "if has(\"Output\") then .Output |= sub(\"restore=inc1 source=ftp export=${export_inc1} expected_snapshot=${snap1} verify_status=passed\"; \"restore=inc1 source=ftp export=${export_inc1} expected_snapshot=${snap1} verify_status=not_requested\") else . end"
reject fabricated_match "if has(\"Output\") then .Output |= sub(\"restored=inc1 container_snapshot=${snap1}\"; \"restored=inc1 container_snapshot=${other_snapshot}\") else . end"
reject false_match 'if has("Output") then .Output |= gsub("expected_snapshot=[0-9a-f]{64} match=true"; "expected_snapshot=0000000000000000000000000000000000000000000000000000000000000000 match=false") else . end'
reject exports_short 'if has("Output") then .Output |= sub("exports_started=3"; "exports_started=2") else . end'
reject export_phase_wrong 'if has("Output") then .Output |= sub("data_digest=0011223344556677 phase=rehearsing links=ftp:2 mode=0750"; "data_digest=0011223344556677 phase=exporting links=ftp:2 mode=0750") else . end'
reject export_phase_inc2 'if has("Output") then .Output |= sub("data_digest=1122334455667788 phase=exporting links=ftp:3 mode=0750"; "data_digest=1122334455667788 phase=rehearsing links=ftp:3 mode=0750") else . end'
reject export_phase_full 'if has("Output") then .Output |= sub("data_digest=2233445566778899 phase=exporting links=ftp:1 mode=0750"; "data_digest=2233445566778899 phase=rehearsing links=ftp:1 mode=0750") else . end'
reject backup_id_dashed 'if has("Output") then .Output |= sub("inc1=11111111111111111111111111111111"; "inc1=11111111-1111-7111-8111-111111111111") else . end'
reject mysql_unpinned "if has(\"Output\") then .Output |= sub(\"new_mysql=${mysql_image}\"; \"new_mysql=mysql:8.4\") else . end"
reject no_confirm 'if has("Output") then .Output |= sub("confirm=403,confirmation_failed"; "confirm=200") else . end'
reject row_created 'if has("Output") then .Output |= sub("confirm=403,confirmation_failed no_row=true"; "confirm=403,confirmation_failed no_row=false") else . end'
reject no_limiter 'if has("Output") then .Output |= sub("limiter=429"; "limiter=200") else . end'
reject no_retry_after 'if has("Output") then .Output |= sub("retry_after=11"; "retry_after=0") else . end'
reject no_audit 'if has("Output") then .Output |= sub("audit_denied=7"; "audit_denied=0") else . end'
reject audit_split 'if has("Output") then .Output |= sub("audit_atomic=true"; "audit_atomic=false") else . end'
reject replay_new_id 'if has("Output") then .Output |= sub("replay_same_id=true"; "replay_same_id=false") else . end'
reject digest_accepted 'if has("Output") then .Output |= sub("digest_changed=409,plan_changed"; "digest_changed=202") else . end'
reject not_admin_only 'if has("Output") then .Output |= sub("admin_only=true"; "admin_only=false") else . end'
reject no_blocker 'if has("Output") then .Output |= sub("run_blocked_by_restore=true"; "run_blocked_by_restore=false") else . end'
reject no_blockee 'if has("Output") then .Output |= sub("restore_blocked_by_run=true"; "restore_blocked_by_run=false") else . end'
reject two_slots 'if has("Output") then .Output |= sub("running_max=1"; "running_max=2") else . end'
reject overlap 'if has("Output") then .Output |= sub("postgres_intervals=disjoint"; "postgres_intervals=overlap") else . end'
reject no_cancel_queued 'if has("Output") then .Output |= sub("canceled_queued=true"; "canceled_queued=false") else . end'
reject no_cancel_running 'if has("Output") then .Output |= sub("canceled_running=true"; "canceled_running=false") else . end'
reject implicit_effects 'if has("Output") then .Output |= gsub("side_effects=explicit"; "side_effects=none") else . end'
reject false_completed 'if has("Output") then .Output |= sub("sigkill=failed/interrupted"; "sigkill=completed") else . end'
reject published_deleted 'if has("Output") then .Output |= gsub("published_intact=true"; "published_intact=false") else . end'
reject no_refusal 'if has("Output") then .Output |= sub("refused=source_unavailable:409"; "refused=none") else . end'
reject recovery_twice 'if has("Output") then .Output |= sub("recovery_repeated=true"; "recovery_repeated=false") else . end'
reject diagnostics_dump 'if has("Output") then .Output |= ("W4 diagnostics: begin (bounded to 400 lines)\n" + .) else . end'
reject a4_running_max_dropped 'if has("Output") then .Output |= sub("restore_blocked_by_run=true running_max=1 queued_samples=48"; "restore_blocked_by_run=true queued_samples=48") else . end'
reject a4_facts_reordered 'if has("Output") then .Output |= sub("running_max=1 queued_samples=48 postgres_intervals=disjoint"; "queued_samples=48 running_max=1 postgres_intervals=disjoint") else . end'
reject a4_zero_slots 'if has("Output") then .Output |= sub("running_max=1"; "running_max=0") else . end'
reject a5_state_facts_reordered 'if has("Output") then .Output |= sub("side_effects=explicit canceled_queued=true"; "canceled_queued=true side_effects=explicit") else . end'
reject a5_sigkill_facts_moved 'if has("Output") then .Output |= sub("W5-A5 sigkill=failed/interrupted side_effects=explicit"; "W5-A5 sigkill=failed/interrupted\nW5-A5 recovery side_effects=explicit") else . end'
reject a5_sigkill_facts_reordered 'if has("Output") then .Output |= sub("cleanup=exact published_intact=true"; "published_intact=true cleanup=exact") else . end'
reject dirty_source 'if has("Output") then .Output |= sub("source_dirty=false"; "source_dirty=true") else . end'
reject root_worker 'if has("Output") then .Output |= sub("worker_uid=999 worker_gid=999"; "worker_uid=0 worker_gid=0") else . end'
reject emulated 'if has("Output") then .Output |= sub("daemon=linux/amd64"; "daemon=linux/arm64") else . end'
reject migration_absent 'if has("Output") then .Output |= gsub("PG-G3 [^\n]*\n"; "") else . end'
reject migration_legacy_missing 'if has("Output") then .Output |= sub("PG-G3 legacy [^\n]*\n"; "") else . end'
reject migration_legacy_foreign 'if has("Output") then .Output |= sub("PG-G3 legacy source=[0-9a-f]{40}"; "PG-G3 legacy source=deadbeefdeadbeefdeadbeefdeadbeefdeadbeef") else . end'
reject migration_wrong_schema 'if has("Output") then .Output |= sub("schema=web:9,physical:1,replication:2"; "schema=web:8,physical:1,replication:2") else . end'
reject migration_not_production 'if has("Output") then .Output |= sub("command=production tables=18"; "command=store-helper tables=18") else . end'
reject migration_source_changed 'if has("Output") then .Output |= sub("source_intact=true"; "source_intact=false") else . end'
reject migration_repeat_accepted 'if has("Output") then .Output |= sub("repeat=refused"; "repeat=accepted") else . end'
reject migration_nonempty_accepted 'if has("Output") then .Output |= sub("nonempty=refused"; "nonempty=accepted") else . end'
reject migration_partial_rollback 'if has("Output") then .Output |= sub("rollback=empty"; "rollback=partial") else . end'
reject migration_missing_table 'if has("Output") then .Output |= sub("PG-G3 table=replica_parts [^\n]*\n"; "") else . end'
reject migration_wrong_table 'if has("Output") then .Output |= sub("PG-G3 table=replica_parts "; "PG-G3 table=replica_members ") else . end'
reject migration_table_changed 'if has("Output") then .Output |= sub("PG-G3 table=users rows=1 sha256=[0-9a-f]{64} equal=true"; "PG-G3 table=users rows=1 sha256=abc equal=false") else . end'
reject migration_extra_table 'if has("Output") then .Output |= (. + "PG-G3 table=unlisted rows=1 sha256=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa equal=true\n") else . end'
reject migration_login_failed 'if has("Output") then .Output |= sub("login=original"; "login=failed") else . end'
reject migration_setup_open 'if has("Output") then .Output |= sub("setup=closed"; "setup=open") else . end'
reject migration_history_missing 'if has("Output") then .Output |= sub("history=visible"; "history=missing") else . end'
reject migration_chain_short 'if has("Output") then .Output |= sub("chain=1,2,3,4"; "chain=1,2,3") else . end'
reject migration_lsn_broken 'if has("Output") then .Output |= sub("lsn=continuous"; "lsn=broken") else . end'
reject migration_replica_missing 'if has("Output") then .Output |= sub("replicas=4"; "replicas=3") else . end'
reject migration_cleanup_pending 'if has("Output") then .Output |= sub("cleanup=recovered"; "cleanup=pending") else . end'
reject migration_missing_export 'if has("Output") then .Output |= sub("PG-G3 export point=continued [^\n]*\n"; "") else . end'
reject migration_duplicate_export 'if has("Output") then .Output |= sub("PG-G3 export point=continued "; "PG-G3 export point=inc2 ") else . end'
reject migration_reused_export_id 'if has("Output") then .Output |= sub("PG-G3 export point=continued id=0123456789abcdef0123456789abcd04"; "PG-G3 export point=continued id=0123456789abcdef0123456789abcd03") else . end'
reject migration_missing_restored 'if has("Output") then .Output |= sub("PG-G3 restored point=continued [^\n]*\n"; "") else . end'
reject migration_duplicate_restored 'if has("Output") then .Output |= sub("PG-G3 restored point=continued "; "PG-G3 restored point=inc2 ") else . end'
reject migration_false_match 'if has("Output") then .Output |= sub("PG-G3 restored point=full actual_snapshot=[0-9a-f]{64} expected_snapshot=[0-9a-f]{64} match=true"; "PG-G3 restored point=full actual_snapshot=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd expected_snapshot=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd match=false") else . end'
reject migration_actual_mismatch 'if has("Output") then .Output |= sub("PG-G3 restored point=inc1 actual_snapshot=[0-9a-f]{64}"; "PG-G3 restored point=inc1 actual_snapshot=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd") else . end'
reject migration_unbound_snapshot 'if has("Output") then .Output |= sub("PG-G3 export point=continued id=0123456789abcdef0123456789abcd04 snapshot=[0-9a-f]{64}"; "PG-G3 export point=continued id=0123456789abcdef0123456789abcd04 snapshot=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb") else . end'
reject migration_missing_binary 'if has("Output") then .Output |= sub("PG-G3 binary path=/tmp/legacy-web [^\n]*\n"; "") else . end'
reject migration_binary_source_changed 'if has("Output") then .Output |= sub("legacy_source=[0-9a-f]{40}"; "legacy_source=deadbeefdeadbeefdeadbeefdeadbeefdeadbeef") else . end'
reject migration_full_not_ftp 'if has("Output") then .Output |= sub("PG-G3 export point=full id=[0-9a-f]{32} snapshot=[0-9a-f]{64} source=ftp"; "PG-G3 export point=full id=0123456789abcdef0123456789abcd03 snapshot=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd source=local") else . end'
reject migration_inc1_not_ftp 'if has("Output") then .Output |= sub("PG-G3 export point=inc1 id=[0-9a-f]{32} snapshot=[0-9a-f]{64} source=ftp"; "PG-G3 export point=inc1 id=0123456789abcdef0123456789abcd01 snapshot=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb source=local") else . end'
reject migration_inc2_not_ftp 'if has("Output") then .Output |= sub("PG-G3 export point=inc2 id=[0-9a-f]{32} snapshot=[0-9a-f]{64} source=ftp"; "PG-G3 export point=inc2 id=0123456789abcdef0123456789abcd02 snapshot=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc source=local") else . end'
reject migration_continued_not_ftp 'if has("Output") then .Output |= sub("PG-G3 export point=continued id=[0-9a-f]{32} snapshot=[0-9a-f]{64} source=ftp"; "PG-G3 export point=continued id=0123456789abcdef0123456789abcd04 snapshot=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa source=local") else . end'

if run_gate "$wrong_source" "$work/accept.jsonl" "$work/sha.log"; then
	echo 'FAIL: the gate accepted a foreign GITHUB_SHA' >&2
	exit 1
fi
echo 'ok: reject sha_mismatch'

if run_gate '' "$work/accept.jsonl" "$work/empty-sha.log"; then
	echo 'FAIL: the gate accepted an empty GITHUB_SHA' >&2
	exit 1
fi
echo 'ok: reject sha_empty'

# The two commit-identity cases are declared outside the reject() list.
echo "w5 gate cases: $accepted accept, $((declared_rejects + 2)) rejects"
