#!/usr/bin/env bash
# Synthetic accept and reject cases for verify-d1-evidence.sh. The script builds a
# `go test -json` event stream for a harness log that quotes every locked D1
# marker, runs the gate on it and requires one accept; it then mutates the stream
# one way per reject case and requires every mutation to be refused. The reject
# set covers each locked fact of the D1 gate, including a foreign and an empty
# GITHUB_SHA, so the gate cannot be quietly widened.
#
# The payloads carry no credential at all, which is the state a clean harness log
# is in: the generated MySQL, source and administrator secrets never reach a
# marker, and the gate never reads one.
set -euo pipefail

# Count the declared reject cases before the directory changes: $0 is relative to
# the caller, so the count has to happen while that path still resolves.
declared_rejects=$(grep -c '^reject ' "$0")
cd "$(dirname "$0")"
gate=./verify-d1-evidence.sh
package=github.com/flyingEirc/Intern/app/web/integration
harness=TestD1NativeContainers
cases=(a1-image a2-compose a3-business a4-lifecycle a5-guardrails)

source=1f2e3d4c5b6a79880123456789abcdef01234567
wrong_source=deadbeefdeadbeefdeadbeefdeadbeefdeadbeef
tag=1f2e3d4c5b6a
project=d18f3a2b4c
image=sha256:$(printf 'a%.0s' {1..64})
repeat_image=sha256:$(printf 'b%.0s' {1..64})
web=$(printf 'c%.0s' {1..64})
save=$(printf 'd%.0s' {1..64})
snapshot=$(printf 'e%.0s' {1..64})
other_snapshot=$(printf 'f%.0s' {1..64})
payload_sha=$(printf '0%.0s' {1..63})1
mysql_image='mysql:8.4.11@sha256:85b9bf2e29cf836ecb8c2a15a935d4ba0c606631dff1dd79531a11983c638f2a'
pxb_image='percona/percona-xtrabackup:8.4.0-7@sha256:bf1478c188271b2478faddb92a27e8378364b672b8e399c0df1c17fef8cb0ceb'
go_image='golang:1.27.0@sha256:4013ae0f9e7994f8535c58c811f8f863fbed38b72e0d51e6592156f758d66146'
repository=01a184e9-8f93-7448-80df-205f2e349aa1
source_id=01a184e9-8f93-7448-80df-205f2e349aa2
policy=01a184e9-8f93-7448-80df-205f2e349aa3
full_run=01a184e9-8f93-7448-80df-205f2e349aa4
full_backup=55555555555555555555555555555555
inc_run=01a184e9-8f93-7448-80df-205f2e349aa6
inc_backup=77777777777777777777777777777777
chain=88888888888888888888888888888888
restore=99999999999999999999999999999999
sigterm_run=01a184e9-8f93-7448-80df-205f2e349aa9
instance=7f3a91c0e5b24d8a
mysql_container=0123456789abcdef0123456789abcdef
intern_container=fedcba9876543210fedcba9876543210
upgrade_tag=intern:"$tag"-upgrade-mysql8.4

accepted=0
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# payload prints exactly the marker lines a passing D1 run emits.
payload() {
	printf '%s\n' \
		"D1-A1 built tag=intern:$tag-mysql8.4 image_id=$image repeat_image_id=$repeat_image source_sha=$source source_dirty=false mysql_image=$mysql_image pxb_image=$pxb_image" \
		"D1-A1 repeatable=true no_cache=true content_equal=true content_files=402 layer_count=41 intern_web_sha256=$web" \
		"D1-A1 bases_pinned=true go_image=$go_image licenses_label=/usr/share/licenses/intern revision=$source" \
		"D1-A1 runtime_downloads=0 added_commands=11 network_none=true tools=xtrabackup,xbstream,mysqld,intern-web" \
		"D1-A1 non_root=true image_user=999:999 container_uid=999 container_gid=999 entrypoint=[\"/usr/local/bin/intern-web\"]" \
		"D1-A1 versions mysqld=8.4.11 xtrabackup=8.4.0-7 xbstream=8.4.0-7 matrix_server=8.4.11 matrix_tool=8.4.0-7 match=true" \
		"D1-A1 gpl files=8 notice=true offer=true versions=true mysql_gpl=18 pxb_gpl=6 source_sha_bound=true procps_lgpl=9 procps_named=3 go_modules=182 go_license_texts=182 copied_objects=89 matrix=8.4.11,8.4.0-7" \
		"D1-A1 compose_pins=true compose=app/web/deploy/compose.yaml env_example=app/web/deploy/.env.example dockerfile=app/web/deploy/Dockerfile matrix_images=2" \
		"D1-A2 identity daemon=linux/amd64 platform=linux/amd64 compose=2.35.1 project=$project" \
		"D1-A2 stack=healthy compose_file=app/web/deploy/compose.yaml project=$project mysql=$mysql_container intern=$intern_container api=https://127.0.0.1:49153 mysql_port=49154 healthcheck=intern-web" \
		"D1-A2 create_admin=created user=d1-admin login=200 negotiated=1.3 stdin=true" \
		"D1-A2 https=verified plaintext=refused untrusted_ca=refused" \
		"D1-A2 datadir_ro=refused metadata_dir=0700 owner=999:999" \
		"D1-A2 kek secret=/run/secrets/intern_kek mode=0600 owner=999:999 digest_bound=true container_user=999:999 mysql=$mysql_container" \
		"D1-A2 socket=preflight_passed checks=7 source=$source_id repository=$repository sealed=true" \
		"D1-A3 topology=compose-mysql->intern-web->local-repository policy=$policy source=$source_id repository=$repository replica=none" \
		"D1-A3 full run=$full_run backup=$full_backup status=completed seq=1 payload_bytes=268435456 sha256=$payload_sha" \
		"D1-A3 incremental run=$inc_run backup=$inc_backup status=completed seq=2 payload_bytes=41943040 sha256=$web" \
		"D1-A3 incremental parent=$full_backup chain=$chain server_uuid_bound=true" \
		"D1-A3 restore=$restore backup=$inc_backup status=completed export=/var/lib/intern/export/$restore bytes=301989888 rehearsal=true verify=not_requested" \
		"D1-A3 export=$restore mode_owner=750:999:999" \
		"D1-A3 export_mysql_started=1 expected_snapshot=$snapshot container_snapshot=$snapshot match=true" \
		"D1-A4 restart instance_id=$instance login=1.3 chain_members=3 a3_pair_readable=true restores=1 incremental=$inc_backup migration=ready kek_digest_bound=true volumes=5" \
		"D1-A4 recreate instance_id=$instance login=1.3 chain_members=4 a3_pair_readable=true restores=1 incremental=$full_backup migration=ready kek_digest_bound=true volumes=5" \
		"D1-A4 upgrade instance_id=$instance login=1.3 chain_members=5 a3_pair_readable=true restores=1 incremental=$inc_backup migration=ready kek_digest_bound=true volumes=5" \
		"D1-A4 upgrade from=intern:$tag-mysql8.4 to=$upgrade_tag source_label=$source-upgrade previous_label=$source labels_differ=true container=$intern_container" \
		"D1-A4 kek=preserved volumes=metadata,backups,work instance_id=$instance" \
		"D1-A5 second_container=refused flock_text=true exit=1 primary_healthy=true primary_api=200" \
		"D1-A5 kek_refusal=missing_permissions missing=refused permissions=refused fixed_text=true metadata_dir=/tmp/d1-refusal-metadata" \
		"D1-A5 sigterm run=$sigterm_run observed_running=true grace_seconds=120 stop_seconds=7 exit_code=0 oom_killed=false" \
		"D1-A5 no_false_completed=true run_status=failed error_class=interrupted backup_id=none completed_runs_before=3 completed_runs_after=3" \
		"D1-ARTIFACT file=candidate-artifact.json source_sha=$source image_id=$image docker_save_sha256=$save docker_save_bytes=1610612736 intern_web_sha256=$web versions_bound=true" \
		"D1-CASE a1-image" "D1-CASE a2-compose" "D1-CASE a3-business" "D1-CASE a4-lifecycle" "D1-CASE a5-guardrails"
}

# stream wraps one payload file in the pass events the gate counts: one pass per
# locked case, one harness pass and one package pass. The D1 cases are real
# subtests of this harness, so they reach the stream as events, not only as text.
stream() {
	local payload_file=$1
	jq -cn --rawfile raw "$payload_file" --arg pkg "$package" --arg harness "$harness" '
		def ev(action; test; output):
			{Action: action, Package: $pkg, Test: test}
			+ (if output == null then {} else {Output: output} end);
		["a1-image", "a2-compose", "a3-business", "a4-lifecycle", "a5-guardrails"] as $cases
		| [ ev("run"; $harness; null),
			ev("output"; $harness; $raw),
			ev("output"; $harness; "PASS\n"),
			ev("pass"; $harness; null) ]
			+ ($cases | map(ev("run"; $harness + "/" + .; null)))
			+ ($cases | map(ev("pass"; $harness + "/" + .; null)))
			+ [ ev("output"; null; "ok  \tgithub.com/flyingEirc/Intern/app/web/integration\n"),
				ev("pass"; null; null) ]
		| .[]
	' | jq -c .
}

run_gate() {
	local sha=$1 file=$2 log=$3
	GITHUB_SHA="$sha" bash "$gate" "$file" >"$log" 2>&1
}

payload | sed 's/^/    d1_test.go:600: /' >"$work/payload.txt"
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
	# A mutation whose search text has drifted away would be a silent no-op, so the
	# case would prove nothing; require the stream to have actually changed.
	if cmp -s "$work/accept.jsonl" "$work/reject-$name.jsonl"; then
		echo "FAIL: the $name mutation did not change the stream (its target text drifted)" >&2
		exit 1
	fi
	if run_gate "$source" "$work/reject-$name.jsonl" "$work/reject-$name.log"; then
		echo "FAIL: the gate accepted the $name case" >&2
		exit 1
	fi
	echo "ok: reject $name"
}

reject missing_case 'select(.Test != "'$harness'/a5-guardrails")'
reject duplicate_case '. , {Action: "pass", Package: $pkg, Test: "'$harness'/a1-image"}'
reject extra_case '. , {Action: "pass", Package: $pkg, Test: "'$harness'/a6-extra"}'
reject renamed_case '. | if .Test == "'$harness'/a4-lifecycle" then .Test = "'$harness'/a4-lifecycle-v2" else . end'
reject harness_skipped '. , {Action: "skip", Package: $pkg, Test: "'$harness'"}'
reject harness_failed '. , {Action: "fail", Package: $pkg, Test: "'$harness'"}'
reject no_harness_pass 'select(.Test != "'$harness'" or .Action != "pass")'
reject no_package_pass 'select(.Action != "pass" or .Test != null)'
reject output_skip 'if has("Output") then .Output |= ("--- SKIP: TestD1NativeContainers/a3-business (1.00s)\n" + .) else . end'
reject output_fail 'if has("Output") then .Output |= ("--- FAIL: TestD1NativeContainers/a2-compose (1.00s)\n" + .) else . end'
reject missing_case_marker 'if has("Output") then .Output |= sub("D1-CASE a1-image\n"; "") else . end'
reject extra_case_marker 'if has("Output") then .Output |= (. + "D1-CASE a6-extra\n") else . end'
reject dirty_source 'if has("Output") then .Output |= sub("source_dirty=false"; "source_dirty=true") else . end'
reject foreign_build_sha "if has(\"Output\") then .Output |= sub(\"source_sha=${source}\"; \"source_sha=${wrong_source}\") else . end"
reject foreign_artifact_sha "if has(\"Output\") then .Output |= sub(\"D1-ARTIFACT file=candidate-artifact.json source_sha=${source}\"; \"D1-ARTIFACT file=candidate-artifact.json source_sha=${wrong_source}\") else . end"
reject artifact_image_mismatch "if has(\"Output\") then .Output |= sub(\"candidate-artifact.json source_sha=${source} image_id=${image}\"; \"candidate-artifact.json source_sha=${source} image_id=${repeat_image}\") else . end"
reject artifact_web_mismatch "if has(\"Output\") then .Output |= sub(\"docker_save_bytes=1610612736 intern_web_sha256=${web}\"; \"docker_save_bytes=1610612736 intern_web_sha256=${snapshot}\") else . end"
reject tiny_artifact 'if has("Output") then .Output |= sub("docker_save_bytes=1610612736"; "docker_save_bytes=1024") else . end'
reject no_repeatable 'if has("Output") then .Output |= sub("repeatable=true no_cache=true content_equal=true"; "repeatable=true no_cache=false content_equal=true") else . end'
reject unpinned_base "if has(\"Output\") then .Output |= sub(\"mysql_image=${mysql_image}\"; \"mysql_image=mysql:8.4\") else . end"
reject unpinned_go "if has(\"Output\") then .Output |= sub(\"go_image=${go_image}\"; \"go_image=golang:1.27\") else . end"
reject runtime_downloads 'if has("Output") then .Output |= sub("runtime_downloads=0"; "runtime_downloads=3") else . end'
reject network_required 'if has("Output") then .Output |= sub("network_none=true"; "network_none=false") else . end'
reject root_image_user 'if has("Output") then .Output |= sub("image_user=999:999 container_uid=999"; "image_user=0:0 container_uid=0") else . end'
reject versions_mismatch 'if has("Output") then .Output |= sub("matrix_server=8.4.11 matrix_tool=8.4.0-7"; "matrix_server=8.0.44 matrix_tool=8.0.4") else . end'
reject xbstream_version 'if has("Output") then .Output |= sub("xbstream=8.4.0-7 matrix_server"; "xbstream=8.4.0-6 matrix_server") else . end'
reject gpl_matrix_mismatch 'if has("Output") then .Output |= sub("copied_objects=89 matrix=8.4.11,8.4.0-7"; "copied_objects=89 matrix=8.4.11,8.4.0-6") else . end'
reject missing_gpl_file 'if has("Output") then .Output |= sub("D1-A1 gpl files=8"; "D1-A1 gpl files=7") else . end'
reject empty_license 'if has("Output") then .Output |= sub("mysql_gpl=18"; "mysql_gpl=0") else . end'
reject no_procps_license 'if has("Output") then .Output |= sub("procps_lgpl=9 procps_named=3"; "procps_lgpl=0 procps_named=0") else . end'
reject module_license_gap 'if has("Output") then .Output |= sub("go_modules=182 go_license_texts=182"; "go_modules=182 go_license_texts=179") else . end'
reject no_copied_provenance 'if has("Output") then .Output |= sub("copied_objects=89"; "copied_objects=0") else . end'
reject emulated 'if has("Output") then .Output |= sub("daemon=linux/amd64"; "daemon=linux/arm64") else . end'
reject foreign_project 'if has("Output") then .Output |= sub("compose=2.35.1 project=d18f3a2b4c"; "compose=2.35.1 project=d19999") else . end'
reject unhealthy_stack 'if has("Output") then .Output |= sub("stack=healthy"; "stack=unhealthy") else . end'
reject copied_compose 'if has("Output") then .Output |= sub("stack=healthy compose_file=app/web/deploy/compose.yaml"; "stack=healthy compose_file=/tmp/compose.yaml") else . end'
reject no_deployment_health 'if has("Output") then .Output |= sub("healthcheck=intern-web"; "healthcheck=none") else . end'
reject no_create_admin 'if has("Output") then .Output |= sub("create_admin=created user=d1-admin login=200"; "create_admin=absent user=d1-admin login=500") else . end'
reject weak_tls 'if has("Output") then .Output |= sub("negotiated=1.3"; "negotiated=1.0") else . end'
reject plaintext_api 'if has("Output") then .Output |= sub("api=https://127.0.0.1:49153"; "api=http://127.0.0.1:49153") else . end'
reject untrusted_ok 'if has("Output") then .Output |= sub("untrusted_ca=refused"; "untrusted_ca=accepted") else . end'
reject writable_datadir 'if has("Output") then .Output |= sub("datadir_ro=refused"; "datadir_ro=allowed") else . end'
reject open_metadata 'if has("Output") then .Output |= sub("metadata_dir=0700 owner=999:999"; "metadata_dir=0755 owner=999:999") else . end'
reject open_kek 'if has("Output") then .Output |= sub("mode=0600 owner=999:999 digest_bound=true"; "mode=0644 owner=999:999 digest_bound=true") else . end'
reject unbound_kek 'if has("Output") then .Output |= sub("digest_bound=true"; "digest_bound=false") else . end'
reject no_sealer 'if has("Output") then .Output |= sub("sealed=true"; "sealed=false") else . end'
reject prelight_failed 'if has("Output") then .Output |= sub("socket=preflight_passed checks=7"; "socket=preflight_failed checks=7") else . end'
reject zero_checks 'if has("Output") then .Output |= sub("checks=7"; "checks=0") else . end'
reject replica_added 'if has("Output") then .Output |= sub("replica=none"; "replica=ftp") else . end'
reject topology_drift "if has(\"Output\") then .Output |= sub(\"local-repository policy=${policy} source=${source_id}\"; \"local-repository policy=${policy} source=${inc_backup}\") else . end"
reject no_full_backup 'if has("Output") then .Output |= select((. // "") | contains("D1-A3 full run=") | not) else . end'
reject incremental_seq1 "if has(\"Output\") then .Output |= sub(\"incremental run=${inc_run} backup=${inc_backup} status=completed seq=2\"; \"incremental run=${inc_run} backup=${inc_backup} status=completed seq=1\") else . end"
reject same_backups "if has(\"Output\") then .Output |= sub(\"backup=${inc_backup} status=completed seq=2\"; \"backup=${full_backup} status=completed seq=2\") else . end"
reject wrong_parent "if has(\"Output\") then .Output |= sub(\"incremental parent=${full_backup}\"; \"incremental parent=${chain}\") else . end"
reject restore_from_full "if has(\"Output\") then .Output |= sub(\"restore=${restore} backup=${inc_backup}\"; \"restore=${restore} backup=${full_backup}\") else . end"
reject no_rehearsal 'if has("Output") then .Output |= sub("rehearsal=true"; "rehearsal=false") else . end'
reject export_not_restore "if has(\"Output\") then .Output |= sub(\"export=/var/lib/intern/export/${restore}\"; \"export=/var/lib/intern/export/${chain}\") else . end"
reject export_mode "if has(\"Output\") then .Output |= sub(\"export=${restore} mode_owner=750:999:999\"; \"export=${restore} mode_owner=777:0:0\") else . end"
reject failed_restore 'if has("Output") then .Output |= sub("restore=99999999999999999999999999999999 backup=77777777777777777777777777777777 status=completed"; "restore=99999999999999999999999999999999 backup=77779999999999999999999999999999 status=failed") else . end'
reject snapshot_mismatch "if has(\"Output\") then .Output |= sub(\"expected_snapshot=${snapshot} container_snapshot=${snapshot}\"; \"expected_snapshot=${snapshot} container_snapshot=${other_snapshot}\") else . end"
reject snapshot_false_match "if has(\"Output\") then .Output |= sub(\"container_snapshot=${snapshot} match=true\"; \"container_snapshot=${snapshot} match=false\") else . end"
reject export_not_started 'if has("Output") then .Output |= sub("export_mysql_started=1"; "export_mysql_started=0") else . end'
reject restart_instance_drift "if has(\"Output\") then .Output |= sub(\"D1-A4 restart instance_id=${instance}\"; \"D1-A4 restart instance_id=9999888877776666\") else . end"
reject migration_pending 'if has("Output") then .Output |= gsub("migration=ready"; "migration=pending") else . end'
reject lost_restore_record 'if has("Output") then .Output |= sub("a3_pair_readable=true restores=1"; "a3_pair_readable=true restores=0") else . end'
reject kek_not_preserved 'if has("Output") then .Output |= sub("kek_digest_bound=true"; "kek_digest_bound=false") else . end'
reject no_kek_line 'if has("Output") then .Output |= select((. // "") | contains("D1-A4 kek=preserved") | not) else . end'
reject upgrade_same_tag "if has(\"Output\") then .Output |= sub(\"to=${upgrade_tag}\"; \"to=intern:${tag}-mysql8.4\") else . end"
reject upgrade_foreign_label "if has(\"Output\") then .Output |= sub(\"source_label=${source}-upgrade previous_label=${source}\"; \"source_label=${wrong_source}-upgrade previous_label=${source}\") else . end"
reject upgrade_previous_foreign "if has(\"Output\") then .Output |= sub(\"previous_label=${source} labels_differ=true\"; \"previous_label=${wrong_source} labels_differ=true\") else . end"
reject second_container_allowed 'if has("Output") then .Output |= sub("second_container=refused flock_text=true exit=1"; "second_container=accepted flock_text=true exit=0") else . end'
reject no_flock_text 'if has("Output") then .Output |= sub("flock_text=true"; "flock_text=false") else . end'
reject kek_refusal_accepted 'if has("Output") then .Output |= sub("kek_refusal=missing_permissions missing=refused permissions=refused"; "kek_refusal=none missing=accepted permissions=accepted") else . end'
reject short_grace 'if has("Output") then .Output |= sub("grace_seconds=120"; "grace_seconds=30") else . end'
reject stop_beyond_grace 'if has("Output") then .Output |= sub("stop_seconds=7"; "stop_seconds=125") else . end'
reject stop_unobserved 'if has("Output") then .Output |= sub("observed_running=true"; "observed_running=false") else . end'
reject sigkilled 'if has("Output") then .Output |= sub("exit_code=0"; "exit_code=137") else . end'
reject oom_killed 'if has("Output") then .Output |= sub("oom_killed=false"; "oom_killed=true") else . end'
reject false_completed 'if has("Output") then .Output |= sub("run_status=failed error_class=interrupted backup_id=none"; "run_status=completed error_class=none backup_id=55555555555555555555555555555555") else . end'
reject completed_grew 'if has("Output") then .Output |= sub("completed_runs_before=3 completed_runs_after=3"; "completed_runs_before=3 completed_runs_after=4") else . end'
reject canceled_as_clean 'if has("Output") then .Output |= sub("error_class=interrupted"; "error_class=unavailable") else . end'
reject no_artifact 'if has("Output") then .Output |= select((. // "") | contains("D1-ARTIFACT file=") | not) else . end'

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
echo "d1 gate cases: $accepted accept, $((declared_rejects + 2)) rejects"
