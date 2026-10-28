#!/usr/bin/env bash
# Synthetic accept and reject cases for verify-p4-evidence.sh. The script builds a
# `go test -json` event stream for a worker log that quotes every locked marker,
# runs the gate on it and requires one accept; it then mutates the stream one way
# per reject case and requires every mutation to be refused. The reject set covers
# each locked fact of the P4 gate, including an empty GITHUB_SHA, so the gate
# cannot be quietly widened.
#
# The generated credentials the gate must never see do not exist here: the payloads
# carry no secret at all, which is the state a clean worker log is in.
set -euo pipefail

# Count the declared reject cases before the directory changes: $0 is relative
# to the caller, so the count has to happen while that path still resolves.
declared_rejects=$(grep -c '^reject ' "$0")
cd "$(dirname "$0")"
gate=./verify-p4-evidence.sh
package=github.com/flyingEirc/Intern/app/web/integration
cases=(a1-plan a2-local a5-argv a6-enospc a3-ftp a4-reject a6-cancel a6-sigkill a7-security)
rejects=(
	"backup_not_found backup_not_found"
	"backup_not_completed backup_not_completed"
	"chain_broken_missing_middle chain_broken"
	"chain_broken_wrong_parent chain_broken"
	"lsn_discontinuous lsn_discontinuous"
	"key_unavailable key_unavailable"
	"tool_version_mismatch tool_version_mismatch"
	"matrix_unsupported matrix_unsupported"
	"insufficient_space insufficient_space"
	"source_unavailable_local_truncated source_unavailable"
	"source_unavailable_replica_unverified source_unavailable"
	"authentication_failed_ftp_tampered authentication_failed"
	"source_unavailable_ftp_truncated source_unavailable"
	"unconfigured_export_symlink unconfigured"
)

source=1f2e3d4c5b6a79880123456789abcdef01234567
wrong_source=deadbeefdeadbeefdeadbeefdeadbeefdeadbeef
binary=00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff
export1=0123456789abcdef0123456789abcd01
export2=0123456789abcdef0123456789abcd02
export3=0123456789abcdef0123456789abcd03
export4=0123456789abcdef0123456789abcd04
snap1=$(printf 'a%.0s' {1..64})
snap2=$(printf 'b%.0s' {1..64})

accepted=0
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# payload prints exactly the marker lines a passing P4 run emits.
payload() {
	printf '%s\n' \
		"P4-A1 plan target=inc1 sources=local,local links=2 lsn_continuous=true" \
		"P4-A1 plan target=inc2 sources=local,local,local links=3 lsn_continuous=true digest=${snap1}" \
		"P4-A1 plan_readonly db=unchanged repo=unchanged export=empty ftp_writes=0" \
		"P4-A1 space required_work=12345 required_export=10000 available_work=900000 available_export=800000 matches_statfs=true" \
		"P4-A1 matrix server=8.4.11 tool=8.4.0-7 prepare_version=8.4.0-7 key_id=00112233" \
		"P4-A1 ftp_plan_readonly writes=0 remote_state=unchanged" \
		"P4-A2 restore=inc1 source=local export=${export1} expected_snapshot=${snap1} rows=10 max_id=10 prepare_checkpoints=full-prepared to_lsn=200" \
		"P4-A2 restore=inc2 source=local export=${export2} expected_snapshot=${snap2} rows=20 max_id=20 prepare_checkpoints=full-prepared to_lsn=300" \
		"P4-A2 rehearsal passed system_tables=true schemas=1 check_table=2 verify_sql=2 verify_sql_match=true" \
		"P4-A2 export_perms dir=0750 file=0640 owner=exact" \
		"P4-A3 plan target=inc1 sources=ftp,ftp verified_only=true ftp_writes=0" \
		"P4-A3 restore=inc1 source=ftp injection=disconnect rest_offset=65536 attempts=2 authenticated_eof=true export=${export3} expected_snapshot=${snap1}" \
		"P4-A3 restore=inc2 source=ftp export=${export4} expected_snapshot=${snap2} rows=20 max_id=20" \
		"P4-A3 local_payloads=removed replicas=3 verified=true" \
		"P4-A5 argv shapes=full-only,full+1,full+2 no_defaults_first=true matches=true" \
		"P4-A6 enospc filesystem=tmpfs code=disk_full export=none work_gone=true published_untouched=true" \
		"P4-A6 cancel during=extracting code=canceled work_gone=true pending_gone=true export_unchanged=true" \
		"P4-A6 cancel during=preparing code=canceled work_gone=true pending_gone=true export_unchanged=true" \
		"P4-A6 sigkill inject=prepare process_tree=gone work_gone=true pending_gone=true export_unchanged=true others_untouched=true second_recover=noop" \
		"P4-A7 secrets=clean" \
		"P4-A7 supervisor mysqld_pgid_separate=true tool_group=supervisor no_leftover_tools=true" \
		"P4-A7 export_root symlink=refused export_perms=0750/0640 owner=exact" \
		"P4-A7 root_refusal mysqld=refused execute=rehearsal_unavailable" \
		"P4-A2 restored=inc1-local container_snapshot=${snap1} expected_snapshot=${snap1} match=true" \
		"P4-A2 restored=inc2-local container_snapshot=${snap2} expected_snapshot=${snap2} match=true" \
		"P4-A3 restored=inc1-ftp container_snapshot=${snap1} expected_snapshot=${snap1} match=true" \
		"P4-A3 restored=inc2-ftp container_snapshot=${snap2} expected_snapshot=${snap2} match=true" \
		"P4-A2 restore_count=4 exports_started=4 snapshots_match=true" \
		"P4 candidate source=${source} source_dirty=false binary_sha256=${binary} mysql_image=mysql@sha256:85 pxb_image=percona@sha256:bf worker_image=sha256:aa ftp_image=sha256:bb daemon=linux/amd64 platform=linux/amd64 worker_uid=999 worker_gid=999 server=Pure-FTPd-1.0.54 client=jlaffaye/ftp-v0.2.4 topology=private-network-no-NAT"
	for entry in "${rejects[@]}"; do
		name=${entry%% *}
		code=${entry##* }
		stage=plan
		if [[ "$name" == authentication_failed_ftp_tampered ]]; then
			stage=execute
		fi
		printf 'P4-A4 rejected=%s code=%s stage=%s export_unchanged=true work_gone=true prepare_calls=0\n' "$name" "$code" "$stage"
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
		(["a1-plan", "a2-local", "a5-argv", "a6-enospc", "a3-ftp", "a4-reject", "a6-cancel", "a6-sigkill", "a7-security"]
			| [ .[] | ev("pass"; "TestP4Worker/" + .; "--- PASS: TestP4Worker/" + . + " (1.00s)\n") ])
		+ [ ev("pass"; "TestP4Worker"; "--- PASS: TestP4Worker (900.00s)\n"),
			ev("output"; null; $raw),
			ev("pass"; "TestP4NativeContainers"; null),
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

reject missing_case 'select(.Test != "TestP4Worker/a6-sigkill")'
reject skipped_case '. , {Action: "skip", Package: $pkg, Test: "TestP4Worker/a3-ftp"}'
reject failed_case '. , {Action: "fail", Package: $pkg, Test: "TestP4Worker/a2-local"}'
reject no_rejection 'if has("Output") then .Output |= select((. // "") | contains("P4-A4 rejected=key_unavailable ") | not) else . end'
reject widened_rejection 'if has("Output") then .Output |= sub("P4-A4 rejected=matrix_unsupported code=matrix_unsupported"; "P4-A4 rejected=matrix_unsupported code=unconfigured") else . end'
reject prepare_called 'if has("Output") then .Output |= sub("chain_broken_missing_middle code=chain_broken stage=plan export_unchanged=true work_gone=true prepare_calls=0"; "chain_broken_missing_middle code=chain_broken stage=plan export_unchanged=true work_gone=true prepare_calls=1") else . end'
reject space_unobserved 'if has("Output") then .Output |= sub("matches_statfs=true"; "matches_statfs=false") else . end'
reject no_resume 'if has("Output") then .Output |= sub("rest_offset=65536"; "rest_offset=0") else . end'
reject snapshot_mismatch 'if has("Output") then .Output |= sub("P4-A3 restored=inc1-ftp container_snapshot="; "P4-A3 restored=inc1-ftp container_nomatch=") else . end'
reject false_match 'if has("Output") then .Output |= gsub("expected_snapshot='"${snap2}"' match=true"; "expected_snapshot='"${snap2}"' match=false") else . end'
reject no_checkpoints 'if has("Output") then .Output |= gsub("prepare_checkpoints=full-prepared"; "prepare_checkpoints=full-backuped") else . end'
reject missing_marker 'if has("Output") then .Output |= sub("argv shapes=[^ ]+"; "argv shapes=none") else . end'
reject dirty_source 'if has("Output") then .Output |= sub("source_dirty=false"; "source_dirty=true") else . end'

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
echo "p4 gate cases: $accepted accept, $((declared_rejects + 2)) rejects"
