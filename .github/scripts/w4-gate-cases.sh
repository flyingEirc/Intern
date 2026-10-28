#!/usr/bin/env bash
# Synthetic accept and reject cases for verify-w4-evidence.sh. The script
# generates a `go test -json` event stream for a worker log that quotes every
# locked marker, runs the gate on it, and requires one accept; it then mutates
# the stream one way per reject case and requires every mutation to be refused.
# The sigkill marker is taken verbatim from the gate, so the accept case tracks
# the frozen text instead of a second copy of it.
#
# The generated credentials the gate must never see do not exist here: the
# payloads carry no secret at all, which is the state a clean worker log is in.
set -euo pipefail

cd "$(dirname "$0")"
gate=./verify-w4-evidence.sh
package=github.com/flyingEirc/Intern/app/web/integration
cases=("a1-a2-e2e" "a2-priority" "a3-cancel" "a4-sse" "a3-sigterm" "a3-sigkill")

source=1f2e3d4c5b6a79880123456789abcdef01234567
wrong_source=deadbeefdeadbeefdeadbeefdeadbeefdeadbeef
binary=00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff
tool=fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210

sigkill_marker=$(grep -o 'sigkill=SIGKILL[^"]*' "$gate" | head -n1)
if [[ -z "$sigkill_marker" ]]; then
	echo 'cannot extract the sigkill marker from the gate' >&2
	exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# payload prints the marker lines a passing worker emits: one line per
# scenario, each quoting the state the gate requires.
payload() {
	printf '%s\n' \
		"W4-A1 e2e chain=seq:1,2,3 lsn_continuous=true replay_run_id=01a17e5a725e4d6f conflict=409 replicas_verified=3 protection_local=10315917a1 protection_remote=e84d23ebc2 protection_requires_verified=true uploaded_window=observed samples=1189 uploaded_backups=10315917a1,363040e2b3,e84d23ebc4 logs_agree=sqlite log_rows=21 lists_agree=sqlite,local,ftp" \
		"W4-A2 priority running_max=1 claim_full_before_incremental=true queued_together=true order=incremental_first sqlite_intervals=disjoint" \
		"W4-A3 cancel canceled=true completed_backup=false staging_reclaimed=true work_reclaimed=true requires_full=true source_backups_unchanged=true injected_during=backup backup_pid=309 backup_target=run-6b5ba956/physical-run-2249959983/xb" \
		"W4-A4 sse sse_end=true sse_ordered=true secrets=clean lifecycle=queued<claimed<preflight_ok<started<completed first_event_ms=803 first_live_event=claimed replayed_last_seq=1 logs_agree=sqlite log_rows=6" \
		"W4-A3 sigterm sigterm_exit_bounded=true no_false_completed=true recovered=true status=failed error_class=\"backup_failed\" error_code=\"backup_failed\" exit=0 injected_during=backup backup_pid=447 backup_target=run-2801ef2f/physical-run-4461075/xb" \
		"W4-A3 sigkill $sigkill_marker status=failed error_class=\"interrupted\" error_code=\"interrupted\" injected_during=backup backup_pid=512 backup_target=run-e84d23eb/physical-run-7/xb" \
		"W4 candidate source=$source source_dirty=false binary_sha256=$binary daemon=linux/amd64 platform=linux/amd64 worker_uid=999 worker_gid=999 tool_sha256=$tool server=Pure-FTPd-1.0.54 client=jlaffaye/ftp-v0.2.4 topology=private-network-no-NAT"
}

# stream wraps one payload file in the pass events the gate counts: one pass
# per case, one worker pass, one package pass and one top-level pass.
stream() {
	local payload_file=$1
	jq -cn --rawfile raw "$payload_file" --arg pkg "$package" '
		def ev(action; test; output):
			{Action: action, Package: $pkg, Test: test}
			+ (if output == null then {} else {Output: output} end);
		(["a1-a2-e2e", "a2-priority", "a3-cancel", "a4-sse", "a3-sigterm", "a3-sigkill"]
			| [ .[]
				| (ev("run"; "TestW4Worker/" + .; "=== RUN   TestW4Worker/" + . + "\n"),
					ev("output"; "TestW4Worker/" + .; "--- PASS: TestW4Worker/" + . + " (1.00s)\n"),
					ev("pass"; "TestW4Worker/" + .; null)) ])
		+ [ ev("run"; "TestW4Worker"; "=== RUN   TestW4Worker\n"),
			ev("output"; "TestW4Worker"; "--- PASS: TestW4Worker (40.00s)\n"),
			ev("pass"; "TestW4Worker"; null),
			ev("output"; null; $raw),
			ev("pass"; "TestW4NativeContainers"; null),
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
	echo "ok: accept unmutated"
else
	echo 'FAIL: the gate rejected the unmutated stream:' >&2
	cat "$work/accept.log" >&2
	exit 1
fi

# reject runs one mutation of the accepted stream and requires the gate to
# refuse it. Every mutation removes or falsifies exactly one locked fact.
reject() {
	local name=$1 filter=$2
	jq -c --arg pkg "$package" "$filter" "$work/accept.jsonl" >"$work/reject-$name.jsonl"
	if run_gate "$source" "$work/reject-$name.jsonl" "$work/reject-$name.log"; then
		echo "FAIL: the gate accepted the $name case" >&2
		exit 1
	fi
	echo "ok: reject $name"
}

reject old_marker 'if has("Output") then .Output |= sub("next_incremental=requires_full next_incremental_code=parent_not_completed admission_full=completed "; "") else . end'
reject missing_case 'select((.Output // "") | contains("--- PASS: TestW4Worker/a3-sigkill (") | not)'
reject skipped_case '. , {Action: "skip", Package: $pkg, Test: "TestW4Worker/a3-sigkill"}'
reject failed_case '. , {Action: "output", Package: $pkg, Test: "TestW4Worker/a4-sse", Output: "--- FAIL: TestW4Worker/a4-sse (0.10s)\n"}, {Action: "fail", Package: $pkg, Test: "TestW4Worker/a4-sse"}'
reject diagnostics 'if has("Output") then .Output |= . + "W4 diagnostics: begin\n" else . end'
# A sigkill run that reports completed instead of failed is a false success.
reject false_success 'if has("Output") then .Output |= gsub("status=failed error_class=\"interrupted\""; "status=completed error_class=\"interrupted\"") else . end'

if run_gate "$wrong_source" "$work/accept.jsonl" "$work/sha.log"; then
	echo 'FAIL: the gate accepted a foreign GITHUB_SHA' >&2
	exit 1
fi
echo 'ok: reject sha_mismatch'

# An empty or unset GITHUB_SHA is never a wildcard: without a bound commit the
# stream is not evidence for any revision, so the gate must refuse it.
if run_gate '' "$work/accept.jsonl" "$work/empty-sha.log"; then
	echo 'FAIL: the gate accepted an empty GITHUB_SHA' >&2
	exit 1
fi
echo 'ok: reject sha_empty'

echo 'w4 gate cases: 1 accept, 8 rejects'
