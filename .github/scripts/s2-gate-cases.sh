#!/usr/bin/env bash
# Synthetic accept and reject cases for verify-s2-evidence.sh.
#
# The script builds a `go test -json` event stream in the shape the real run produces:
# one harness output event carrying the whole logged worker transcript (both compiled
# worker binaries are exec-ed inside containers, so their PASS lines and every S2 marker
# reach the stream as output text prefixed with `s2_test.go:NN: `, never as events of
# the integration package), then the harness pass event and the package pass event.
#
# It requires one accept for the unmutated stream, then refuses one mutation per locked
# S2 fact - each FTPS mode, the server-enforced data protection, session reuse, the
# credential census and its positive control, all four refusals at both layers, the
# downgrade door, the API contract, the plaintext baseline, namespace ownership, the
# fixture identity, both chains, every restore, the fresh-container comparisons and the
# candidate binding - plus an empty and a foreign GITHUB_SHA. No mutation may pass, so
# the gate cannot be quietly widened.
set -euo pipefail

# Count the declared reject cases before the directory changes: $0 is relative to
# the caller, so the count has to happen while that path still resolves.
declared_rejects=$(grep -c '^reject ' "$0")
cd "$(dirname "$0")"
gate=./verify-s2-evidence.sh
package=github.com/flyingEirc/Intern/app/web/integration
transport=TestS2FTPSWorker
chain=TestS2Worker
api=TestS2APIWorker

source=1f2e3d4c5b6a79880123456789abcdef01234567
wrong_source=deadbeefdeadbeefdeadbeefdeadbeefdeadbeef
sha64=$(printf 'a%.0s' {1..64})
leaf_good=$(printf '1%.0s' {1..64})
leaf_wrong=$(printf '2%.0s' {1..64})
leaf_expired=$(printf '3%.0s' {1..64})
leaf_rogue=$(printf '4%.0s' {1..64})
digest=$(printf '5%.0s' {1..64})
snap1=$(printf '6%.0s' {1..64})
snap2=$(printf '7%.0s' {1..64})
snap3=$(printf '8%.0s' {1..64})
snap4=$(printf '9%.0s' {1..64})
other_snapshot=$(printf '0%.0s' {1..64})
full1=0123456789abcdef0123456789abcd01
inc11=0123456789abcdef0123456789abcd02
inc12=0123456789abcdef0123456789abcd03
full2=0123456789abcdef0123456789abcd04
inc21=0123456789abcdef0123456789abcd05
inc22=0123456789abcdef0123456789abcd06
export_a=11111111111111111111111111111111
export_b=22222222222222222222222222222222
export_c=33333333333333333333333333333333
export_d=44444444444444444444444444444444
mysql_image='mysql:8.4.11@sha256:85b9bf2e29cf836ecb8c2a15a935d4ba0c606631dff1dd79531a11983c638f2a'

accepted=0
work=$(mktemp -d)
trap '[[ "${S2_GATE_WORK_KEEP:-0}" == 1 ]] || rm -rf "$work"' EXIT

# payload prints exactly the marker lines a passing S2 run emits: the transport
# worker's transcript, the control-plane worker's transcript, and the harness' own
# candidate and fresh-container facts.
payload() {
	printf '%s\n' \
		"S2-A1 census face_channels=6 resumed=3 wire_bytes=48123 cleartext_marker=0 cleartext_secret=0 pairing_exact=true" \
		"S2-A1 explicit_publish listener=explicit client=2121 tls=3 mode=explicit channels=4 control_version=TLS1.3 data_resumed=true wire_bytes=31452 cleartext_marker=0 cleartext_secret=0 leaf=${leaf_good} dns=s2-ftps not_after=2026-11-01T00:00:00Z oracle_sha256=${digest} session_sha256=${digest} audit_sessions=2 user_per_session=0 pass_per_session=0 secret_per_session=0" \
		"S2-A4 fixture_identity listeners=6 tls_required=3 advertise_private=true auth_backend=normal_bcrypt_cost=10 client=jlaffaye/ftp-v0.2.4 uid=1000 marker_sha256=${sha64} ca_sha256=${sha64}" \
		"S2-A1 implicit_publish listener=implicit client=990 tls=3 mode=implicit channels=4 control_version=TLS1.2 data_resumed=true wire_bytes=30120 cleartext_marker=0 cleartext_secret=0 leaf=${leaf_good} dns=s2-ftps not_after=2026-11-01T00:00:00Z oracle_sha256=${digest} session_sha256=${digest} audit_sessions=2 user_per_session=0 pass_per_session=0 secret_per_session=0" \
		"S2-A1 data_protection prot_clear=521 prot_private=226 plaintext_data=refused plaintext_probe_reply=0 cleartext_bytes_offered=124 cleartext_payload_on_server=0" \
		"S2-A1 session_reuse library_channels=7 library_resumed=5 probe_shared_resumed=true probe_fresh_resumed=false version=TLS1.3" \
		"S2-A2 credential_census explicit_user_per_session=0 explicit_pass_per_session=0 explicit_secret_per_session=0 implicit_user_per_session=0 implicit_pass_per_session=0 implicit_secret_per_session=0 plain_user_per_session=1 plain_pass_per_session=1 plain_secret_per_session=1 control=positive" \
		"S2-A2 refusal kind=tls_untrusted listener=rogue mode=explicit reply=0 audit_sessions=1 user_per_session=0 pass_per_session=0 secret_per_session=0 leaf=${leaf_rogue} dns=s2-ftps not_after=2026-11-01T00:00:00Z" \
		"S2-A2 refusal kind=tls_hostname listener=wronghost mode=explicit reply=0 audit_sessions=1 user_per_session=0 pass_per_session=0 secret_per_session=0 leaf=${leaf_wrong} dns=s2-some-other-host not_after=2026-11-01T00:00:00Z" \
		"S2-A2 refusal kind=tls_expired listener=expired mode=explicit reply=0 audit_sessions=1 user_per_session=0 pass_per_session=0 secret_per_session=0 leaf=${leaf_expired} dns=s2-ftps not_after=2026-10-28T00:00:00Z" \
		"S2-A2 refusal kind=tls_negotiation listener=plain mode=explicit reply=500 audit_sessions=1 user_per_session=0 pass_per_session=0 secret_per_session=0 leaf=none dns=none not_after=0001-01-01T00:00:00Z" \
		"S2-A2 refusal kind=tls_negotiation listener=plain mode=implicit reply=0 audit_sessions=1 user_per_session=0 pass_per_session=0 secret_per_session=0 leaf=none dns=none not_after=0001-01-01T00:00:00Z" \
		"S2-A2 no_downgrade plain_on_tls_listener=refused reply=421 explicit_on_plain_listener=refused reply=500 positive_control=ok plaintext_fallback=never" \
		"S2-A4 plain_baseline publish=verified listing=1 objects=1 oracle_digest=${digest} plaintext_credentials=true sessions=3" \
		"S2-A3 instance_marker marker_bytes=24 foreign=refused kind=ownership preserved=true reprobe=ok channels=3" \
		"S2-A1 explicit-chain probe face=explicit tls_mode=explicit ca_pem=pinned port=2121 tls_required=3 probe=ok" \
		"S2-A1 explicit-chain upload face=explicit chain=seq:1,2,3 replicas_verified=3 upload=full,inc1,inc2 backups=${full1},${inc11},${inc12}" \
		"S2-A1 explicit-chain readback face=explicit objects=3 local_sha_match=true ftps_sha_match=true ftp_payload_bytes=1048576" \
		"S2-A1 explicit-chain restore face=explicit restore=inc1 links=ftp:2 plan_digest=${digest}" \
		"S2-A1 explicit-chain restore face=explicit restore=inc2 links=ftp:3 plan_digest=${digest}" \
		"S2-A1 restore=inc1 face=explicit export=${export_a} expected_snapshot=${snap1}" \
		"S2-A1 restore=inc2 face=explicit export=${export_b} expected_snapshot=${snap2}" \
		"S2-A1 implicit-chain probe face=implicit tls_mode=implicit ca_pem=pinned port=990 tls_required=3 probe=ok" \
		"S2-A1 implicit-chain upload face=implicit chain=seq:1,2,3 replicas_verified=3 upload=full,inc1,inc2 backups=${full2},${inc21},${inc22}" \
		"S2-A1 implicit-chain readback face=implicit objects=3 local_sha_match=true ftps_sha_match=true ftp_payload_bytes=1048640" \
		"S2-A1 implicit-chain restore face=implicit restore=inc1 links=ftp:2 plan_digest=${digest}" \
		"S2-A1 implicit-chain restore face=implicit restore=inc2 links=ftp:3 plan_digest=${digest}" \
		"S2-A1 restore=inc1 face=implicit export=${export_c} expected_snapshot=${snap3}" \
		"S2-A1 restore=inc2 face=implicit export=${export_d} expected_snapshot=${snap4}" \
		"S2-A3 api_ftps create=201,1 update=200 preflight=ok,ok plaintext_credentials=false invalid_ca=422,2 mode_change=422 plain_with_ca=422 contract=ok faces=explicit,implicit" \
		"S2-A3 create=ok explicit=201 implicit=201 plain=201 plaintext_credentials explicit=false,implicit=false,plain=true ca_pem=stored" \
		"S2-A3 reject: unparsable_ca=422 plain_with_ca=422 oversized_ca=422 unknown_mode=422 mode_change=422" \
		"S2-A3 probe: ok=explicit,implicit untrusted=tls_untrusted hostname=tls_hostname expired=tls_expired plain_only=tls_negotiation audit=plaintext_ftp=false" \
		"S2-A3 stored role=explicit tls_mode=explicit plaintext_credentials=false ca_pem_bytes=1030 stored=ca_pem probe_face_port=2121" \
		"S2-A3 stored role=implicit tls_mode=implicit plaintext_credentials=false ca_pem_bytes=1030 stored=ca_pem probe_face_port=990" \
		"S2-A3 probe_ok role=explicit face=explicit sessions=3 auth_tls=3 to_server_bytes=812 credential_bytes=0" \
		"S2-A3 probe_ok role=implicit face=implicit sessions=2 auth_tls=0 to_server_bytes=640 credential_bytes=0" \
		"S2-A3 probe_refused repository=s2-probe-untrusted face=rogue tls_mode=explicit code=tls_untrusted reply_code=0 sessions=1 credential_bytes=0" \
		"S2-A3 probe_refused repository=s2-probe-rogue-bundle face=explicit tls_mode=explicit code=tls_untrusted reply_code=0 sessions=1 credential_bytes=0" \
		"S2-A3 census control=plain face=plain sessions=2 user=2 pass=2 secret=2 plaintext_credentials=true" \
		"S2-A3 bundle_attribution face=explicit pinned=rogue-ca code=tls_untrusted credential_bytes=0" \
		"S2-A2 refusal_api face=rogue bundle=pinned mode=explicit kind=tls_untrusted probe=refused reply=0 enable=409 audit_sessions=1 user_per_session=0 pass_per_session=0 secret_per_session=0" \
		"S2-A2 refusal_api face=explicit bundle=rogue mode=explicit kind=tls_untrusted probe=refused reply=0 enable=409 audit_sessions=1 user_per_session=0 pass_per_session=0 secret_per_session=0" \
		"S2-A2 refusal_api face=wronghost bundle=pinned mode=explicit kind=tls_hostname probe=refused reply=0 enable=409 audit_sessions=1 user_per_session=0 pass_per_session=0 secret_per_session=0" \
		"S2-A2 refusal_api face=expired bundle=pinned mode=explicit kind=tls_expired probe=refused reply=0 enable=409 audit_sessions=1 user_per_session=0 pass_per_session=0 secret_per_session=0" \
		"S2-A2 refusal_api face=plain bundle=pinned mode=explicit kind=tls_negotiation probe=refused reply=500 enable=409 audit_sessions=1 user_per_session=0 pass_per_session=0 secret_per_session=0" \
		"S2-A2 refusal_api census_control face=plain mode=plain user=3 pass=3 secret=3 marker=3" \
		"S2 candidate source=${source} source_dirty=false transport_binary_sha256=${sha64} chain_binary_sha256=${sha64} api_binary_sha256=${sha64} fixture_image=sha256:cc worker_image=${sha64:0:12} chain_image=sha256:dd api_image=sha256:ee mysql_image=${mysql_image} pxb_image=percona-xtrabackup@sha256:ee base_image=alpine:3.24.2@sha256:ff daemon_sha256=${sha64} implicit_daemon_sha256=${sha64} tap_sha256=${sha64} ca_fingerprint=${sha64} marker_sha256=${sha64} daemon=linux/x86_64 platform=linux/amd64 worker_uid=999 worker_gid=999 server=Pure-FTPd-1.0.54 client=jlaffaye/ftp-v0.2.4 listeners=6 tls_required=3 passive=50100-50619 topology=private-network-no-NAT" \
		"S2-A1 restored=inc1 face=explicit container_snapshot=${snap1} expected_snapshot=${snap1} match=true" \
		"S2-A1 restored=inc2 face=explicit container_snapshot=${snap2} expected_snapshot=${snap2} match=true" \
		"S2-A1 restored=inc1 face=implicit container_snapshot=${snap3} expected_snapshot=${snap3} match=true" \
		"S2-A1 restored=inc2 face=implicit container_snapshot=${snap4} expected_snapshot=${snap4} match=true" \
		"S2-A4 exports_started=4 snapshots_match=true new_mysql=${mysql_image}" \
		"S2-A4 fixture_cleanup: workers/fixture removed; private volume absence verified; network removed"
}

# stream wraps one payload file in the events and worker transcripts the gate counts.
stream() {
	local payload_file=$1
	jq -cn --rawfile raw "$payload_file" --arg pkg "$package" \
		--arg transport "$transport" --arg chain "$chain" --arg api "$api" '
		def ev(action; test; output):
			{Action: action, Package: $pkg, Test: test}
			+ (if output == null then {} else {Output: output} end);
		(["explicit_publish", "implicit_publish", "data_protection", "session_reuse",
		  "credential_census", "refusal", "no_downgrade", "plain_baseline", "instance_marker"]
			| map("--- PASS: " + $transport + "/" + . + " (1.00s)")) as $transportCases
		| (["explicit-chain", "implicit-chain", "api-ftps", "refusal-api"]
			| map("--- PASS: " + $chain + "/" + . + " (1.00s)")) as $chainCases
		| (["a3-create", "a3-reject", "a3-probe"]
			| map("--- PASS: " + $api + "/" + . + " (1.00s)")) as $apiCases
		| ($chainCases + $transportCases + $apiCases +
		   ["--- PASS: " + $transport + " (1.00s)", "--- PASS: " + $chain + " (1.00s)",
		    "--- PASS: " + $api + " (1.00s)"] +
		   [$raw]) as $text
		| [ ev("run"; "TestS2NativeContainers"; null),
			ev("output"; "TestS2NativeContainers"; ($text | join("\n") + "\n")),
			ev("pass"; "TestS2NativeContainers"; null),
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

# reject runs one mutation of the accepted stream and requires the gate to refuse it.
# Every mutation removes or falsifies exactly one locked fact. A mutation that changed
# nothing is refused here as well: a no-op would be reported as a passing reject case
# and make the gate look stronger than it is.
reject() {
	local name=$1 filter=$2
	jq -c "$filter" "$work/accept.jsonl" >"$work/reject-$name.jsonl"
	if cmp -s "$work/accept.jsonl" "$work/reject-$name.jsonl"; then
		echo "FAIL: the $name mutation changed nothing, so it proves nothing about the gate" >&2
		exit 1
	fi
	if run_gate "$source" "$work/reject-$name.jsonl" "$work/reject-$name.log"; then
		echo "FAIL: the gate accepted the $name case" >&2
		exit 1
	fi
	echo "ok: reject $name"
}

# Case-set integrity: the workers are text inside one harness event, so a renamed,
# dropped, added or skipped case must be caught there and not in the event stream.
reject missing_transport_case 'if has("Output") then .Output |= sub("--- PASS: TestS2FTPSWorker/data_protection \\(1\\.00s\\)\n"; "") else . end'
reject extra_transport_case 'if has("Output") then .Output |= (. + "--- PASS: TestS2FTPSWorker/magic_mode (1.00s)\n") else . end'
reject renamed_chain_case 'if has("Output") then .Output |= sub("--- PASS: TestS2Worker/refusal-api \\("; "--- PASS: TestS2Worker/refusal_api (") else . end'
reject skipped_transport_case 'if has("Output") then .Output |= sub("--- PASS: TestS2FTPSWorker/plain_baseline \\("; "--- SKIP: TestS2FTPSWorker/plain_baseline (") else . end'
reject duplicate_transport_case 'if has("Output") then .Output |= (. + "--- PASS: TestS2FTPSWorker/session_reuse (1.00s)\n") else . end'
reject missing_chain_entry 'if has("Output") then .Output |= sub("--- PASS: TestS2Worker \\(1\\.00s\\)\n"; "") else . end'
reject harness_fail 'if has("Action") and .Test == "TestS2NativeContainers" then .Action = "fail" else . end'
reject harness_skip 'if has("Action") and .Test == "TestS2NativeContainers" then .Action = "skip" else . end'
reject missing_harness_pass 'select(.Test != "TestS2NativeContainers" or .Action != "pass")'
reject missing_package_pass 'select(.Test != null or .Action != "pass")'
reject no_output 'if has("Output") then .Output = "" else . end'

# Candidate binding: a dirty tree, a foreign commit, an empty commit and a missing
# digest each prove nothing about the source under test.
reject dirty_source 'if has("Output") then .Output |= sub("source_dirty=false"; "source_dirty=true") else . end'
reject foreign_sha 'if has("Output") then .Output |= sub("source=1f2e3d4c5b6a79880123456789abcdef01234567"; "source=deadbeefdeadbeefdeadbeefdeadbeefdeadbeef") else . end'
reject missing_digest 'if has("Output") then .Output |= sub("transport_binary_sha256="; "transport_binary=") else . end'
reject emulated_platform 'if has("Output") then .Output |= sub("platform=linux/amd64"; "platform=linux/arm64") else . end'
reject root_worker 'if has("Output") then .Output |= sub("worker_uid=999"; "worker_uid=0") else . end'
reject wrong_server_build 'if has("Output") then .Output |= sub("server=Pure-FTPd-1.0.54"; "server=Pure-FTPd-1.0.47") else . end'
reject missing_daemon_digest 'if has("Output") then .Output |= sub("daemon_sha256=[0-9a-f]{64} implicit"; "implicit"; "g") else . end'

# S2-A1: both modes, the encrypted census, server-enforced data protection, reuse,
# and both chains with their read-backs and restores.
reject cleartext_marker_seen 'if has("Output") then .Output |= sub("cleartext_marker=0 cleartext_secret=0 pairing_exact=true"; "cleartext_marker=1 cleartext_secret=0 pairing_exact=true") else . end'
reject cleartext_secret_seen 'if has("Output") then .Output |= sub("cleartext_marker=0 cleartext_secret=0 pairing_exact=true"; "cleartext_marker=0 cleartext_secret=1 pairing_exact=true") else . end'
reject partial_pairing 'if has("Output") then .Output |= sub("pairing_exact=true"; "pairing_exact=false") else . end'
reject no_resumption 'if has("Output") then .Output |= (sub("mode=explicit channels=4 control_version=TLS1.3 data_resumed=true"; "mode=explicit channels=4 control_version=TLS1.3 data_resumed=false") | sub("S2-A1 session_reuse library_channels=7 library_resumed=5"; "S2-A1 session_reuse library_channels=7 library_resumed=0")) else . end'
reject fresh_config_resumes 'if has("Output") then .Output |= sub("probe_fresh_resumed=false"; "probe_fresh_resumed=true") else . end'
reject implicit_mode_missing 'if has("Output") then .Output |= sub("S2-A1 implicit_publish listener=implicit"; "S2-A1 implicit_publish listener=explicit") else . end'
reject plaintext_data_accepted 'if has("Output") then .Output |= sub("plaintext_data=refused"; "plaintext_data=accepted") else . end'
reject prot_clear_accepted 'if has("Output") then .Output |= sub("prot_clear=521"; "prot_clear=226") else . end'
reject chain_replicas_short 'if has("Output") then .Output |= sub("face=explicit chain=seq:1,2,3 replicas_verified=3"; "face=explicit chain=seq:1,2,3 replicas_verified=2") else . end'
reject readback_mismatch 'if has("Output") then .Output |= sub("local_sha_match=true ftps_sha_match=true ftp_payload_bytes=1048576"; "local_sha_match=true ftps_sha_match=false ftp_payload_bytes=1048576") else . end'
reject restore_source_local 'if has("Output") then .Output |= sub("restore=inc1 links=ftp:2"; "restore=inc1 links=local:2") else . end'
reject export_shape 'if has("Output") then .Output |= sub("export=11111111111111111111111111111111"; "export=notanid") else . end'
reject implicit_chain_missing 'if has("Output") then .Output |= sub("S2-A1 implicit-chain probe face=implicit"; "S2-A1 implicit-chain probeX face=implicit") else . end'

# S2-A2: the refusals, the census that makes them mean anything, and the downgrade door.
reject refusal_untrusted_gone 'if has("Output") then .Output |= sub("kind=tls_untrusted listener=rogue"; "kind=transport listener=rogue") else . end'
reject refusal_hostname_gone 'if has("Output") then .Output |= sub("dns=s2-some-other-host"; "dns=s2-ftps") else . end'
reject refusal_expired_gone 'if has("Output") then .Output |= sub("S2-A2 refusal kind=tls_expired listener=expired"; "S2-A2 refusal kind=tls_untrusted listener=expired") else . end'
reject refusal_no_tls_gone 'if has("Output") then .Output |= sub("kind=tls_negotiation listener=plain mode=explicit"; "kind=tls_negotiation listener=rogue mode=explicit") else . end'
reject api_refusal_missing 'if has("Output") then .Output |= sub("S2-A2 refusal_api face=wronghost"; "S2-A2 refusal_apiX face=wronghost") else . end'
reject census_control_missing 'if has("Output") then .Output |= sub("S2-A2 refusal_api census_control[^\n]*\n"; "") else . end'
reject census_all_zero 'if has("Output") then .Output |= sub("census_control face=plain mode=plain user=3 pass=3 secret=3 marker=3"; "census_control face=plain mode=plain user=0 pass=0 secret=0 marker=0") else . end'
reject ftps_user_seen 'if has("Output") then .Output |= gsub("audit_sessions=2 user_per_session=0 pass_per_session=0 secret_per_session=0"; "audit_sessions=2 user_per_session=1 pass_per_session=1 secret_per_session=0") else . end'
reject credential_census_weakened 'if has("Output") then .Output |= sub("plain_user_per_session=1"; "plain_user_per_session=0") else . end'
reject downgrade_allowed 'if has("Output") then .Output |= sub("plaintext_fallback=never"; "plaintext_fallback=auto") else . end'
reject plaintext_login_ok 'if has("Output") then .Output |= sub("plain_on_tls_listener=refused reply=421"; "plain_on_tls_listener=ok reply=230") else . end'
reject truncated_census_read_as_zero 'if has("Output") then .Output |= sub("S2-A1 census face_channels=6"; "S2-A1 census truncated face_channels=6") else . end'

# S2-A3 and S2-A4: the API contract, namespace ownership, the plaintext baseline and
# the fixture identity every one of them is computed against.
reject api_plaintext_credentials 'if has("Output") then .Output |= sub("plaintext_credentials=false"; "plaintext_credentials=true") else . end'
reject api_invalid_ca_accepted 'if has("Output") then .Output |= sub("invalid_ca=422,2"; "invalid_ca=201,1") else . end'
reject api_mode_change_accepted 'if has("Output") then .Output |= sub("mode_change=422 plain_with_ca=422"; "mode_change=200 plain_with_ca=422") else . end'
reject api_preflight_failed 'if has("Output") then .Output |= sub("preflight=ok,ok"; "preflight=ok,refused") else . end'
reject marker_not_enforced 'if has("Output") then .Output |= sub("foreign=refused kind=ownership"; "foreign=accepted kind=ownership") else . end'
reject plain_baseline_missing 'if has("Output") then .Output |= sub("S2-A4 plain_baseline publish=verified[^\n]*\n"; "") else . end'
reject fixture_identity_drift 'if has("Output") then .Output |= sub("tls_required=3 advertise_private=true"; "tls_required=1 advertise_private=true") else . end'
reject fixture_faces_short 'if has("Output") then .Output |= sub("S2-A4 fixture_identity listeners=6"; "S2-A4 fixture_identity listeners=5") else . end'
reject identity_backend_missing 'if has("Output") then .Output |= sub("auth_backend=normal_bcrypt_cost=10 "; "") else . end'
reject api_rogue_bundle_attribution 'if has("Output") then .Output |= sub("refusal_api face=explicit bundle=rogue"; "refusal_api face=explicit bundle=pinned") else . end'
reject api_implicit_cleartext_upgrade 'if has("Output") then .Output |= sub("probe_ok role=implicit face=implicit sessions=2 auth_tls=0"; "probe_ok role=implicit face=implicit sessions=2 auth_tls=1") else . end'

# The Web acceptance worker: its entry point, its three cases, and one mutation per
# locked S2-A3 fact it reads back.
reject missing_api_entry 'if has("Output") then .Output |= sub("--- PASS: TestS2APIWorker \\(1\\.00s\\)\n"; "") else . end'
reject missing_api_case 'if has("Output") then .Output |= sub("--- PASS: TestS2APIWorker/a3-reject \\(1\\.00s\\)\n"; "") else . end'
reject extra_api_case 'if has("Output") then .Output |= (. + "--- PASS: TestS2APIWorker/a3-magic (1.00s)\n") else . end'
reject api_create_missing 'if has("Output") then .Output |= sub("S2-A3 create=ok[^\n]*\n"; "") else . end'
reject api_ftps_plaintext_flag 'if has("Output") then .Output |= sub("plaintext_credentials explicit=false,implicit=false,plain=true"; "plaintext_credentials explicit=true,implicit=false,plain=true") else . end'
reject api_plain_loses_flag 'if has("Output") then .Output |= sub("plaintext_credentials explicit=false,implicit=false,plain=true"; "plaintext_credentials explicit=false,implicit=false,plain=false") else . end'
reject api_reject_weakened 'if has("Output") then .Output |= sub("unparsable_ca=422 plain_with_ca=422 oversized_ca=422 unknown_mode=422"; "unparsable_ca=400 plain_with_ca=422 oversized_ca=422 unknown_mode=422") else . end'
reject api_transport_change_weakened 'if has("Output") then .Output |= sub("unknown_mode=422 mode_change=422"; "unknown_mode=422 mode_change=200") else . end'
reject api_probe_refusal_weakened 'if has("Output") then .Output |= sub("S2-A3 probe: ok=explicit,implicit untrusted=tls_untrusted"; "S2-A3 probe: ok=explicit,implicit untrusted=transport") else . end'
reject api_stored_bundle_missing 'if has("Output") then .Output |= sub("stored=ca_pem probe_face_port=2121"; "stored=missing probe_face_port=2121") else . end'
reject api_probe_ok_credential 'if has("Output") then .Output |= sub("probe_ok role=explicit face=explicit sessions=3 auth_tls=3 to_server_bytes=812 credential_bytes=0"; "probe_ok role=explicit face=explicit sessions=3 auth_tls=3 to_server_bytes=812 credential_bytes=9") else . end'
reject api_probe_refused_credential 'if has("Output") then .Output |= sub("code=tls_untrusted reply_code=0 sessions=1 credential_bytes=0"; "code=tls_untrusted reply_code=0 sessions=1 credential_bytes=9") else . end'
reject api_census_control_missing 'if has("Output") then .Output |= sub("S2-A3 census control=plain[^\n]*\n"; "") else . end'
reject api_census_control_dead 'if has("Output") then .Output |= sub("census control=plain face=plain sessions=2 user=2 pass=2 secret=2"; "census control=plain face=plain sessions=2 user=0 pass=0 secret=0") else . end'
reject api_bundle_attribution_missing 'if has("Output") then .Output |= sub("pinned=rogue-ca"; "pinned=fixture-ca") else . end'
reject missing_api_digest 'if has("Output") then .Output |= sub("api_binary_sha256="; "api_binary=") else . end'

# The harness half: fresh-container comparisons and the cleanup of the private volumes.
reject snapshot_mismatch 'if has("Output") then .Output |= sub("restored=inc1 face=explicit container_snapshot=[0-9a-f]{64}"; "restored=inc1 face=explicit container_snapshot=0000000000000000000000000000000000000000000000000000000000000000") else . end'
reject restore_comparison_missing 'if has("Output") then .Output |= sub("S2-A1 restored=inc2 face=implicit[^\n]*\n"; "") else . end'
reject exports_started_short 'if has("Output") then .Output |= sub("exports_started=4 snapshots_match=true"; "exports_started=3 snapshots_match=true") else . end'
reject new_mysql_unpinned 'if has("Output") then .Output |= sub("new_mysql=mysql:8\\.4\\.11@sha256:85b9bf2e29cf836ecb8c2a15a935d4ba0c606631dff1dd79531a11983c638f2a"; "new_mysql=mysql:latest") else . end'
reject cleanup_unverified 'if has("Output") then .Output |= sub("S2-A4 fixture_cleanup:[^\n]*\n"; "") else . end'

# An empty or unset commit is a rejection, not a wildcard.
if GITHUB_SHA="" bash "$gate" "$work/accept.jsonl" >"$work/empty-sha.log" 2>&1; then
	echo 'FAIL: the gate accepted an empty GITHUB_SHA' >&2
	exit 1
fi
echo 'ok: reject empty_sha'
if GITHUB_SHA="$wrong_source" bash "$gate" "$work/accept.jsonl" >"$work/foreign-sha.log" 2>&1; then
	echo 'FAIL: the gate accepted a foreign GITHUB_SHA' >&2
	exit 1
fi
echo 'ok: reject foreign_sha'
declared_rejects=$((declared_rejects + 2))

if [[ "$accepted" -ne 1 ]]; then
	echo 'FAIL: the accept case never ran' >&2
	exit 1
fi
echo "s2 gate cases: 1 accept, ${declared_rejects} rejects"
