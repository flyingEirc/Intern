#!/usr/bin/env bash
# Evidence gate for the D1 deployment acceptance. It accepts one `go test -json`
# event stream and requires every locked marker, the exact case set, a clean
# source identity bound to GITHUB_SHA, and no skip or failure anywhere. An empty
# or unset GITHUB_SHA is a rejection, not a wildcard.
#
# The five D1 cases are real subtests of this harness (there is no in-container
# worker), so each one must also answer as a pass event of the integration
# package, and the closed set is enforced by counting those events. Every marker
# line is matched as text inside the harness output as well: the gate judges the
# recorded facts, and a missing fact is a refusal even when the events are green.
#
# Every marker is emitted only after the harness' own assertions on read-back
# state passed: the image inspection, the deployed container's health, the API
# rows, the volume ownership, the export listing, or a brand-new pinned MySQL
# 8.4.11 container started on the published export. Where one line must agree with
# itself the agreement is a backreference inside that one regular expression, so
# the published export is named after the restore that wrote it, the new server's
# snapshot is the snapshot recorded at the restore point, and the upgrade label is
# the commit under test.
#
# The gate never reads a secret: it only inspects log lines the harness already
# proved free of the generated credentials. Synthetic accept and reject cases live
# in d1-gate-cases.sh: they feed this gate generated streams and require one
# accept and a refusal per mutated fact, so the gate cannot be silently widened.
set -euo pipefail

if [[ "$#" -ne 1 ]]; then
	echo "usage: verify-d1-evidence.sh <go-test-json-stream>" >&2
	exit 2
fi

jq -e -s --arg source "${GITHUB_SHA:-}" '
  (.) as $events |
  ([$events[] | .Output // ""] | join("")) as $output |
  "TestD1NativeContainers" as $worker |
  "github.com/flyingEirc/Intern/app/web/integration" as $pkg |
  [
    "a1-image", "a2-compose", "a3-business", "a4-lifecycle", "a5-guardrails"
  ] as $cases |
  [
    "D1-A1 built tag=", "D1-A1 repeatable=true", "D1-A1 bases_pinned=true",
    "D1-A1 runtime_downloads=0", "D1-A1 non_root=true", "D1-A1 versions ",
    "D1-A1 gpl files=", "D1-A1 compose_pins=true",
    "D1-A2 identity daemon=linux/", "D1-A2 stack=healthy", "D1-A2 create_admin=created",
    "D1-A2 https=verified plaintext=refused untrusted_ca=refused",
    "D1-A2 datadir_ro=refused metadata_dir=0700 owner=999:999",
    "D1-A2 kek secret=/run/secrets/intern_kek mode=0600 owner=999:999 digest_bound=true",
    "D1-A2 socket=preflight_passed", "D1-A3 topology=", "D1-A3 full ", "D1-A3 incremental ",
    "D1-A3 restore=", "D1-A3 export_mysql_started=1", "D1-A4 restart ", "D1-A4 recreate ",
    "D1-A4 upgrade ", "D1-A4 kek=preserved", "D1-A5 second_container=refused",
    "D1-A5 kek_refusal=missing_permissions", "D1-A5 sigterm ", "D1-A5 no_false_completed=true",
    "D1-ARTIFACT file=candidate-artifact.json"
  ] as $markers |
  ($events | length) > 0 and
  all($events[]; .Action != "skip" and .Action != "fail") and
  ([$events[] | select(.Action == "pass" and .Test == $worker and .Package == $pkg)] | length == 1) and
  ([$events[] | select(.Action == "pass" and .Test == null and .Package == $pkg)] | length == 1) and
  # The D1 cases are subtests of this harness, so each must answer as exactly one
  # pass event, and the set is closed: five prefixed pass events, one per locked
  # name, so a renamed, duplicated or extra case is refused.
  all($cases[]; . as $case |
    ([$events[] | select(.Action == "pass" and .Test == ($worker + "/" + $case) and
      .Package == $pkg)] | length == 1)) and
  ([$events[] | select(.Action == "pass" and (.Test | type) == "string" and
    (.Test | startswith($worker + "/")))] | length) == ($cases | length) and
  # The harness records its own case set once per locked case and nothing else.
  (($output | split("D1-CASE ") | length) == ($cases | length + 1)) and
  all($cases[]; . as $case | (($output | split("D1-CASE " + $case) | length) == 2)) and
  ($output | contains("--- FAIL:") or contains("--- SKIP:") or contains("diagnostics: begin") | not) and
  all($markers[]; . as $marker | $output | contains($marker)) and
  # The candidate identity is the exact commit under test, clean, on a native
  # amd64 daemon: an empty or foreign GITHUB_SHA is a rejection, never a wildcard.
  ($source != "") and
  ($output | contains("source_sha=" + $source + " source_dirty=false")) and
  ($output | contains("D1-ARTIFACT file=candidate-artifact.json source_sha=" + $source)) and
  # The image itself: pinned bases, a repeatable build, no run-time download, the
  # matrix versions, the non-root default, the GPL materials and the Compose pins.
  ($output | test("D1-A1 built tag=intern:[0-9a-f]{12}(-dirty)?-mysql8\\.4 image_id=sha256:[0-9a-f]{64} repeat_image_id=sha256:[0-9a-f]{64} source_sha=[0-9a-f]{40} source_dirty=false mysql_image=mysql:8\\.4\\.11@sha256:[0-9a-f]{64} pxb_image=percona/percona-xtrabackup:8\\.4\\.0-7@sha256:[0-9a-f]{64}")) and
  ($output | test("D1-A1 repeatable=true no_cache=true content_equal=true content_files=[1-9][0-9]* layer_count=[1-9][0-9]* intern_web_sha256=[0-9a-f]{64}")) and
  ($output | test("D1-A1 bases_pinned=true go_image=golang:1\\.27\\.0@sha256:[0-9a-f]{64} licenses_label=/usr/share/licenses/intern revision=[0-9a-f]{40}")) and
  ($output | test("D1-A1 runtime_downloads=0 added_commands=[1-9][0-9]* network_none=true tools=xtrabackup,xbstream,mysqld,intern-web")) and
  ($output | test("D1-A1 non_root=true image_user=999:999 container_uid=999 container_gid=999")) and
  ($output | test("D1-A1 versions mysqld=8\\.4\\.11 xtrabackup=8\\.4\\.0-7 xbstream=8\\.4\\.0-7 matrix_server=8\\.4\\.11 matrix_tool=8\\.4\\.0-7 match=true")) and
  ($output | test("D1-A1 gpl files=8 notice=true offer=true versions=true mysql_gpl=[1-9][0-9]* pxb_gpl=[1-9][0-9]* source_sha_bound=true procps_lgpl=[1-9][0-9]* procps_named=[1-9][0-9]* go_modules=([1-9][0-9]*) go_license_texts=\\1 copied_objects=[1-9][0-9]* matrix=8\\.4\\.11,8\\.4\\.0-7")) and
  ($output | test("D1-A1 compose_pins=true compose=app/web/deploy/compose\\.yaml env_example=app/web/deploy/\\.env\\.example dockerfile=app/web/deploy/Dockerfile matrix_images=2")) and
  # The stack: the repository Compose file, native amd64, the deployment health
  # entry point, the administrator bootstrap, verified TLS, the read-only datadir
  # and the file-secret KEK that is really read.
  ($output | test("D1-A2 identity daemon=linux/(x86_64|amd64) platform=linux/amd64 compose=[0-9]+\\.[0-9]+\\.[0-9]+ project=d1[0-9a-f]+")) and
  ($output | test("D1-A2 stack=healthy compose_file=app/web/deploy/compose\\.yaml project=d1[0-9a-f]+ mysql=[0-9a-f]{12,64} intern=[0-9a-f]{12,64} api=https://127\\.0\\.0\\.1:[1-9][0-9]* mysql_port=[1-9][0-9]* healthcheck=intern-web")) and
  ($output | test("D1-A2 create_admin=created user=d1-admin login=200 negotiated=1\\.[23] stdin=true")) and
  ($output | test("D1-A2 kek secret=/run/secrets/intern_kek mode=0600 owner=999:999 digest_bound=true container_user=999:999")) and
  ($output | test("D1-A2 socket=preflight_passed checks=[1-9][0-9]* source=[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12} repository=[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12} sealed=true")) and
  ($output | test("D1-A2 identity [^\\n]*project=(d1[0-9a-f]+)[\\s\\S]*D1-A2 stack=healthy compose_file=app/web/deploy/compose\\.yaml project=\\1")) and
  ($output | test("D1-A2 stack=healthy[^\\n]*mysql=([0-9a-f]{12,64})[^\\n]*intern=([0-9a-f]{12,64})[\\s\\S]*D1-A2 kek secret=/run/secrets/intern_kek[^\\n]*mysql=\\1")) and
  ($output | test("D1-A2 socket=preflight_passed checks=[0-9]+ source=([0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}) repository=([0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12})[\\s\\S]*D1-A3 topology=[^\\n]* source=\\1 repository=\\2")) and
  (([$output | scan("D1-A3 (?:full|incremental) run=[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12} backup=([0-9a-f]{32})")] | map(.[0]) | unique) | length == 2) and
  (([$output | scan("D1-A4 (restart|recreate|upgrade) instance_id=([0-9a-zA-Z_-]{8,})")] | map(.[1]) | unique) | length == 1) and
  # The business loop: one chain of full then incremental, a restore of the
  # incremental whose published export is named after the restore itself, and a
  # brand-new pinned server that reproduces the recorded snapshot exactly.
  ($output | test("D1-A3 topology=compose-mysql->intern-web->local-repository policy=[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12} source=[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12} repository=[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12} replica=none")) and
  ($output | test("D1-A3 full run=[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12} backup=[0-9a-f]{32} status=completed seq=1 payload_bytes=[1-9][0-9]* sha256=[0-9a-f]{64}")) and
  ($output | test("D1-A3 incremental run=[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12} backup=[0-9a-f]{32} status=completed seq=2 payload_bytes=[1-9][0-9]* sha256=[0-9a-f]{64}")) and
  ($output | test("D1-A3 incremental parent=[0-9a-f]{32} chain=[0-9a-f]{32} server_uuid_bound=true")) and
  ($output | test("D1-A3 full run=[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12} backup=([0-9a-f]{32})[\\s\\S]*?D1-A3 incremental run=[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12} backup=([0-9a-f]{32}) status=completed seq=2[\\s\\S]*?D1-A3 incremental parent=\\1[\\s\\S]*?D1-A3 restore=[0-9a-f]{32} backup=\\2 status=completed")) and
  ($output | test("D1-A3 restore=([0-9a-f]{32}) backup=[0-9a-f]{32} status=completed export=/var/lib/intern/export/\\1[\\s\\S]*D1-A3 export=\\1 mode_owner=750:999:999")) and
  ($output | test("D1-A3 export_mysql_started=1 expected_snapshot=([0-9a-f]{64}) container_snapshot=\\1 match=true")) and
  # Lifecycle: restart, recreate and image upgrade keep one metadata instance,
  # both backups, the restore record, the bound KEK and the Ready migration state.
  ($output | test("D1-A4 restart instance_id=[0-9a-zA-Z_-]{8,} login=1\\.[23] chain_members=[2-9][0-9]* a3_pair_readable=true restores=1 incremental=[0-9a-f]{32} migration=ready kek_digest_bound=true volumes=[1-9][0-9]*")) and
  ($output | test("D1-A4 recreate instance_id=[0-9a-zA-Z_-]{8,} login=1\\.[23] chain_members=[2-9][0-9]* a3_pair_readable=true restores=1 incremental=[0-9a-f]{32} migration=ready kek_digest_bound=true volumes=[1-9][0-9]*")) and
  ($output | test("D1-A4 upgrade instance_id=[0-9a-zA-Z_-]{8,} login=1\\.[23] chain_members=[2-9][0-9]* a3_pair_readable=true restores=1 incremental=[0-9a-f]{32} migration=ready kek_digest_bound=true volumes=[1-9][0-9]*")) and
  ($output | test("D1-A4 upgrade from=intern:[0-9a-f]{12}(?:-dirty)?-mysql8\\.4 to=intern:[0-9a-f]{12}(?:-dirty)?-upgrade-mysql8\\.4 source_label=([0-9a-f]{40})-upgrade previous_label=\\1 labels_differ=true")) and
  ($output | test("D1-A4 kek=preserved volumes=metadata,backups,work instance_id=[0-9a-zA-Z_-]{8,}")) and
  # Guardrails: one owner per metadata volume, a KEK that must exist and be
  # private, and a SIGTERM that lands strictly inside the declared grace period
  # without ever producing a completed run.
  ($output | test("D1-A5 second_container=refused flock_text=true exit=[1-9][0-9]* primary_healthy=true primary_api=200")) and
  ($output | test("D1-A5 kek_refusal=missing_permissions missing=refused permissions=refused fixed_text=true")) and
  ($output | test("D1-A5 sigterm run=[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12} observed_running=true grace_seconds=1[0-9][0-9] stop_seconds=([0-9]|[1-8][0-9]) exit_code=0 oom_killed=false")) and
  ($output | test("D1-A5 no_false_completed=true run_status=(?:failed|canceled) error_class=(?:canceled|interrupted) backup_id=none completed_runs_before=([0-9]+) completed_runs_after=\\1")) and
  # The candidate artifact binds the exact commit, the image that was built, the
  # tarball digest, and the binary digest the repeatable build compared.
  ($output | test("D1-A1 repeatable=true[^\\n]*intern_web_sha256=([0-9a-f]{64})[\\s\\S]*D1-ARTIFACT file=candidate-artifact\\.json[^\\n]*intern_web_sha256=\\1")) and
  ($output | test("D1-A1 built tag=intern:[0-9a-f]{12}(?:-dirty)?-mysql8\\.4 image_id=(sha256:[0-9a-f]{64})[\\s\\S]*D1-ARTIFACT file=candidate-artifact\\.json source_sha=[0-9a-f]{40} image_id=\\1")) and
  ($output | test("D1-ARTIFACT file=candidate-artifact\\.json source_sha=[0-9a-f]{40} image_id=sha256:[0-9a-f]{64} docker_save_sha256=[0-9a-f]{64} docker_save_bytes=[1-9][0-9]{6,} intern_web_sha256=[0-9a-f]{64} versions_bound=true")) and
  # Refusals that must never appear, even beside an otherwise complete marker set.
  ($output | test("source_dirty=true") | not) and
  ($output | test("match=false") | not) and
  ($output | test("oom_killed=true") | not) and
  ($output | test("exit_code=137") | not) and
  ($output | test("run_status=completed") | not) and
  ($output | test("migration=(pending|failed)") | not) and
  ($output | test("image_user=0|container_uid=0|daemon=linux/arm64|api=http://|mode=0[0-7][1-9][0-9]|digest_bound=false|sealed=false|rehearsal=false|flock_text=false|primary_healthy=false|layers_equal=false|network_none=false|runtime_downloads=[1-9]") | not)
' "$1"
