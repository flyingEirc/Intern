#!/usr/bin/env bash
# Evidence gate for the S2 FTPS native run. It accepts one `go test -json` event
# stream from `go test -tags=integration -run '^TestS2NativeContainers$'
# ./app/web/integration` and requires every locked S2 fact, both closed worker case
# sets, the six-face fixture identity, a clean source SHA bound to GITHUB_SHA, and no
# skip or failure anywhere. An empty or unset GITHUB_SHA is a rejection, never a
# wildcard.
#
# Both workers are compiled test binaries executed inside containers, so their
# per-case results reach this stream only as text the harness logged from the executed
# binary - never as events of the integration package. Every marker is therefore
# matched as text inside that output, and the counts are exact: a renamed, repeated,
# skipped or extra case is refused.
#
# Each value a marker carries was read back by the worker or the harness that printed
# it: the fixture's own manifest, the audit the server-side taps wrote, a real TLS
# handshake, the served API row, PostgreSQL, the FTPS namespace listing, or a brand-new
# MySQL 8.4.11 container started on a published export. The gate never reads a
# secret: it only inspects lines the workers already proved free of the generated
# credentials. Synthetic accept and reject cases live in s2-gate-cases.sh.
set -euo pipefail

if [[ "$#" -ne 1 ]]; then
	echo "usage: $0 <go-test-json-stream>" >&2
	exit 2
fi

jq -e -s --arg source "${GITHUB_SHA:-}" '
  (.) as $events |
  ([$events[] | .Output // ""] | join("")) as $output |
  "TestS2FTPSWorker" as $transport |
  "TestS2Worker" as $chain |
  "TestS2APIWorker" as $api |
  [
    "explicit_publish", "implicit_publish", "data_protection", "session_reuse",
    "credential_census", "refusal", "no_downgrade", "plain_baseline", "instance_marker"
  ] as $transportCases |
  [
    "explicit-chain", "implicit-chain", "api-ftps", "refusal-api"
  ] as $chainCases |
  [
    "a3-create", "a3-reject", "a3-probe"
  ] as $apiCases |
  ([
    # S2-A1: both modes complete on the real server, and the server - not only the
    # client - requires the data channel to be encrypted and resumable.
    "S2-A1 explicit_publish listener=explicit", "S2-A1 implicit_publish listener=implicit",
    "S2-A1 data_protection", "S2-A1 session_reuse",
    # S2-A2: the census, the four refusals, the downgrade door and the API refusals.
    "S2-A2 credential_census", "S2-A2 refusal kind=tls_untrusted",
    "S2-A2 refusal kind=tls_hostname", "S2-A2 refusal kind=tls_expired",
    "S2-A2 refusal kind=tls_negotiation", "S2-A2 no_downgrade",
    "S2-A2 refusal_api face=rogue", "S2-A2 refusal_api face=wronghost",
    "S2-A2 refusal_api face=expired", "S2-A2 refusal_api face=plain",
    "S2-A2 refusal_api face=explicit bundle=rogue",
    "S2-A2 refusal_api census_control",
    # S2-A3: the API contract over FTPS from both workers, and namespace ownership
    # over an encrypted transport.
    "S2-A3 api_ftps", "S2-A3 instance_marker", "S2-A3 create=ok", "S2-A3 reject:",
    "S2-A3 probe:", "S2-A3 stored role=explicit", "S2-A3 stored role=implicit",
    "S2-A3 probe_ok role=explicit", "S2-A3 probe_ok role=implicit",
    "S2-A3 probe_refused repository=s2-probe-rogue-bundle", "S2-A3 census control=plain",
    "S2-A3 bundle_attribution",
    # S2-A4: plain behaviour, fixture identity, and the exports the harness started.
    "S2-A4 plain_baseline", "S2-A4 fixture_identity", "S2-A4 fixture_cleanup:",
    "S2-A4 exports_started=4",
    # Both chains, both modes, on one live source.
    "S2-A1 explicit-chain probe", "S2-A1 explicit-chain upload", "S2-A1 explicit-chain readback",
    "S2-A1 explicit-chain restore", "S2-A1 implicit-chain probe", "S2-A1 implicit-chain upload",
    "S2-A1 implicit-chain readback", "S2-A1 implicit-chain restore"
  ]) as $markers |
  ($events | length) > 0 and
  all($events[]; .Action != "skip" and .Action != "fail") and
  ([$events[] | select(.Action == "pass" and .Test == "TestS2NativeContainers" and
      .Package == "github.com/flyingEirc/Intern/app/web/integration")] | length == 1) and
  ([$events[] | select(.Action == "pass" and .Test == null and
      .Package == "github.com/flyingEirc/Intern/app/web/integration")] | length == 1) and
  # One pass line per worker entry point, one per locked case, and no case outside
  # the carrier: the worker streams are text, so the harness is the only witness.
  (($output | split("--- PASS: " + $transport + " (") | length) == 2) and
  (($output | split("--- PASS: " + $chain + " (") | length) == 2) and
  (($output | split("--- PASS: " + $api + " (") | length) == 2) and
  all($transportCases[]; . as $case |
    (($output | split("--- PASS: " + $transport + "/" + $case + " (") | length) == 2)) and
  all($chainCases[]; . as $case |
    (($output | split("--- PASS: " + $chain + "/" + $case + " (") | length) == 2)) and
  all($apiCases[]; . as $case |
    (($output | split("--- PASS: " + $api + "/" + $case + " (") | length) == 2)) and
  (($output | split("--- PASS: " + $transport + "/") | length) == ($transportCases | length + 1)) and
  (($output | split("--- PASS: " + $chain + "/") | length) == ($chainCases | length + 1)) and
  (($output | split("--- PASS: " + $api + "/") | length) == ($apiCases | length + 1)) and
  ($output | contains("--- FAIL:") or contains("--- SKIP:") | not) and
  all($markers[]; . as $marker | $output | contains($marker)) and
  # The candidate is the exact commit under test, clean, with both worker binaries,
  # the compiled daemon pair and the audit tap bound by digest.
  ($output | test("S2 candidate source=[0-9a-f]{40} source_dirty=false transport_binary_sha256=[0-9a-f]{64} chain_binary_sha256=[0-9a-f]{64} api_binary_sha256=[0-9a-f]{64}")) and
  ($source != "" and ($output | contains("S2 candidate source=" + $source + " source_dirty=false"))) and
  ($output | test("daemon=linux/(x86_64|amd64) platform=linux/amd64")) and
  ($output | test("worker_uid=[1-9][0-9]* worker_gid=[1-9][0-9]*")) and
  ($output | test("daemon_sha256=[0-9a-f]{64} implicit_daemon_sha256=[0-9a-f]{64} tap_sha256=[0-9a-f]{64}")) and
  ($output | test("ca_fingerprint=[0-9a-f]{64} marker_sha256=[0-9a-f]{64}")) and
  ($output | contains("server=Pure-FTPd-1.0.54 client=jlaffaye/ftp-v0.2.4 listeners=6 tls_required=3 passive=50100-50619 topology=private-network-no-NAT")) and
  # The fixture identity the transport worker read out of the manifest the server
  # itself published, not out of a constant it hoped for.
  ($output | test("S2-A4 fixture_identity listeners=6 tls_required=3 advertise_private=true auth_backend=normal_bcrypt_cost=10 client=jlaffaye/ftp-v[0-9.]* uid=1000 marker_sha256=[0-9a-f]{64} ca_sha256=[0-9a-f]{64}")) and
  # S2-A1: every FTPS publish moved bytes over more than one encrypted channel, the
  # census of that traffic found no cleartext marker or secret, and the socket and
  # handshake counts paired exactly - a plaintext channel would leave a socket with no
  # handshake.
  ($output | test("S2-A1 explicit_publish listener=explicit client=2121 tls=3 mode=explicit channels=[1-9][0-9]* control_version=TLS1\\.[23] data_resumed=true wire_bytes=[1-9][0-9]* cleartext_marker=0 cleartext_secret=0 leaf=[0-9a-f]{64} dns=s2-ftps not_after=[0-9T:.Z-]+ oracle_sha256=[0-9a-f]{64} session_sha256=[0-9a-f]{64} audit_sessions=[1-9][0-9]* user_per_session=0 pass_per_session=0 secret_per_session=0")) and
  ($output | test("S2-A1 implicit_publish listener=implicit client=990 tls=3 mode=implicit channels=[1-9][0-9]* control_version=TLS1\\.[23] data_resumed=true wire_bytes=[1-9][0-9]* cleartext_marker=0 cleartext_secret=0 leaf=[0-9a-f]{64} dns=s2-ftps not_after=[0-9T:.Z-]+ oracle_sha256=[0-9a-f]{64} session_sha256=[0-9a-f]{64} audit_sessions=[1-9][0-9]* user_per_session=0 pass_per_session=0 secret_per_session=0")) and
  ($output | test("S2-A1 census face_channels=[1-9][0-9]* resumed=[1-9][0-9]* wire_bytes=[1-9][0-9]* cleartext_marker=0 cleartext_secret=0 pairing_exact=true")) and
  # The server itself: 521 for a clear data channel, 226 under PROT P, and a plaintext
  # socket that never completed a transfer.
  ($output | test("S2-A1 data_protection prot_clear=521 prot_private=226 plaintext_data=refused plaintext_probe_reply=(0|2[0-9][0-9]|1[0-9][0-9]|4[0-9][0-9]|5[0-9][0-9]) cleartext_bytes_offered=[1-9][0-9]* cleartext_payload_on_server=0")) and
  ($output | test("S2-A1 session_reuse library_channels=[1-9][0-9]* library_resumed=[1-9][0-9]* probe_shared_resumed=true probe_fresh_resumed=false version=TLS1\\.[23]")) and
  # S2-A2: the credential census is two-sided - FTPS sessions carry nothing readable,
  # the plain session on the same fixture carries one of each.
  ($output | test("S2-A2 credential_census explicit_user_per_session=0 explicit_pass_per_session=0 explicit_secret_per_session=0 implicit_user_per_session=0 implicit_pass_per_session=0 implicit_secret_per_session=0 plain_user_per_session=1 plain_pass_per_session=1 plain_secret_per_session=1 control=positive")) and
  ($output | test("S2-A2 refusal kind=tls_untrusted listener=rogue mode=explicit reply=[0-9]+ audit_sessions=[1-9][0-9]* user_per_session=0 pass_per_session=0 secret_per_session=0 leaf=[0-9a-f]{64}")) and
  ($output | test("S2-A2 refusal kind=tls_hostname listener=wronghost mode=explicit reply=[0-9]+ audit_sessions=[1-9][0-9]* user_per_session=0 pass_per_session=0 secret_per_session=0 leaf=[0-9a-f]{64} dns=s2-some-other-host")) and
  ($output | test("S2-A2 refusal kind=tls_expired listener=expired mode=explicit reply=[0-9]+ audit_sessions=[1-9][0-9]* user_per_session=0 pass_per_session=0 secret_per_session=0 leaf=[0-9a-f]{64}")) and
  ($output | test("S2-A2 refusal kind=tls_negotiation listener=plain mode=explicit reply=[0-9]+ audit_sessions=[1-9][0-9]* user_per_session=0 pass_per_session=0 secret_per_session=0")) and
  ($output | test("S2-A2 refusal kind=tls_negotiation listener=plain mode=implicit reply=[0-9]+ audit_sessions=[1-9][0-9]* user_per_session=0 pass_per_session=0 secret_per_session=0")) and
  # The downgrade door, closed from both sides with the reply codes the server itself gave.
  ($output | test("S2-A2 no_downgrade plain_on_tls_listener=refused reply=421 explicit_on_plain_listener=refused reply=[0-9]+ positive_control=ok plaintext_fallback=never")) and
  ($output | test("S2-A2 refusal_api face=rogue bundle=pinned mode=explicit kind=tls_untrusted probe=refused reply=[0-9]+ enable=409 audit_sessions=[1-9][0-9]* user_per_session=0 pass_per_session=0 secret_per_session=0")) and
  ($output | test("S2-A2 refusal_api face=wronghost bundle=pinned mode=explicit kind=tls_hostname probe=refused reply=[0-9]+ enable=409 audit_sessions=[1-9][0-9]* user_per_session=0 pass_per_session=0 secret_per_session=0")) and
  ($output | test("S2-A2 refusal_api face=expired bundle=pinned mode=explicit kind=tls_expired probe=refused reply=[0-9]+ enable=409 audit_sessions=[1-9][0-9]* user_per_session=0 pass_per_session=0 secret_per_session=0")) and
  ($output | test("S2-A2 refusal_api face=plain bundle=pinned mode=explicit kind=tls_negotiation probe=refused reply=[0-9]+ enable=409 audit_sessions=[1-9][0-9]* user_per_session=0 pass_per_session=0 secret_per_session=0")) and
  # The fifth refusal is the attribution one: the face the real CA issued is refused
  # only because the pinned bundle is the rogue anchor.
  ($output | test("S2-A2 refusal_api face=explicit bundle=rogue mode=explicit kind=tls_untrusted probe=refused reply=[0-9]+ enable=409 audit_sessions=[1-9][0-9]* user_per_session=0 pass_per_session=0 secret_per_session=0")) and
  ($output | test("S2-A2 refusal_api census_control face=plain mode=plain user=[1-9][0-9]* pass=[1-9][0-9]* secret=[1-9][0-9]* marker=[1-9][0-9]*")) and
  # S2-A3: the API admits FTPS on the documented rules only, and namespace ownership
  # survives the encrypted transport. A tls_mode change is a 422 shape refusal from
  # the handler, never a silent re-selection.
  ($output | test("S2-A3 api_ftps create=201,1 update=200 preflight=ok,ok plaintext_credentials=false invalid_ca=422,2 mode_change=422 plain_with_ca=422 contract=ok")) and
  ($output | test("S2-A3 instance_marker marker_bytes=[1-9][0-9]* foreign=refused kind=ownership preserved=true reprobe=ok")) and
  # The Web acceptance worker: both FTPS modes stored, echoed and audited as
  # protected while the plain repository in the same run keeps its plaintext flag;
  # the five frozen 422 transport rules; and the probe of every face, with the
  # census as the judge of what reached the server.
  ($output | test("S2-A3 create=ok explicit=201 implicit=201 plain=201 plaintext_credentials explicit=false,implicit=false,plain=true ca_pem=stored")) and
  ($output | test("S2-A3 reject: unparsable_ca=422 plain_with_ca=422 oversized_ca=422 unknown_mode=422 mode_change=422")) and
  ($output | test("S2-A3 probe: ok=explicit,implicit untrusted=tls_untrusted hostname=tls_hostname expired=tls_expired plain_only=tls_negotiation audit=plaintext_ftp=false")) and
  ($output | test("S2-A3 stored role=explicit tls_mode=explicit plaintext_credentials=false ca_pem_bytes=[1-9][0-9]* stored=ca_pem probe_face_port=2121")) and
  ($output | test("S2-A3 stored role=implicit tls_mode=implicit plaintext_credentials=false ca_pem_bytes=[1-9][0-9]* stored=ca_pem probe_face_port=990")) and
  ($output | test("S2-A3 probe_ok role=explicit face=explicit sessions=[1-9][0-9]* auth_tls=[1-9][0-9]* to_server_bytes=[1-9][0-9]* credential_bytes=0")) and
  ($output | test("S2-A3 probe_ok role=implicit face=implicit sessions=[1-9][0-9]* auth_tls=0 to_server_bytes=[1-9][0-9]* credential_bytes=0")) and
  ($output | test("S2-A3 probe_refused repository=s2-probe-untrusted face=rogue tls_mode=explicit code=tls_untrusted reply_code=[0-9]+ sessions=[1-9][0-9]* credential_bytes=0")) and
  ($output | test("S2-A3 probe_refused repository=s2-probe-rogue-bundle face=explicit tls_mode=explicit code=tls_untrusted reply_code=[0-9]+ sessions=[1-9][0-9]* credential_bytes=0")) and
  ($output | test("S2-A3 bundle_attribution face=explicit pinned=rogue-ca code=tls_untrusted credential_bytes=0")) and
  ($output | test("S2-A3 census control=plain face=plain sessions=[1-9][0-9]* user=[1-9][0-9]* pass=[1-9][0-9]* secret=[1-9][0-9]* plaintext_credentials=true")) and
  # S2-A4: the plaintext suite still passes, and says so honestly.
  ($output | test("S2-A4 plain_baseline publish=verified listing=[1-9][0-9]* objects=1 oracle_digest=[0-9a-f]{64} plaintext_credentials=true sessions=[1-9][0-9]*")) and
  # Both chains: six replicas verified over FTPS, read back byte-for-byte, and two
  # restores per face planned from the FTPS copy alone.
  ($output | test("S2-A1 explicit-chain probe face=explicit tls_mode=explicit ca_pem=pinned port=2121 tls_required=3 probe=ok")) and
  ($output | test("S2-A1 implicit-chain probe face=implicit tls_mode=implicit ca_pem=pinned port=990 tls_required=3 probe=ok")) and
  # Each face carries its own chain: three verified replicas, three byte-identical
  # read-backs and two restores planned from the FTPS copy. Written per face, because an
  # alternation over the two faces would let one face answer for both.
  ($output | test("S2-A1 explicit-chain upload face=explicit chain=seq:1,2,3 replicas_verified=3 upload=full,inc1,inc2 backups=[0-9a-f]{32},[0-9a-f]{32},[0-9a-f]{32}")) and
  ($output | test("S2-A1 implicit-chain upload face=implicit chain=seq:1,2,3 replicas_verified=3 upload=full,inc1,inc2 backups=[0-9a-f]{32},[0-9a-f]{32},[0-9a-f]{32}")) and
  ($output | test("S2-A1 explicit-chain readback face=explicit objects=3 local_sha_match=true ftps_sha_match=true ftp_payload_bytes=[1-9][0-9]*")) and
  ($output | test("S2-A1 implicit-chain readback face=implicit objects=3 local_sha_match=true ftps_sha_match=true ftp_payload_bytes=[1-9][0-9]*")) and
  ($output | test("S2-A1 explicit-chain restore face=explicit restore=inc1 links=ftp:[1-9][0-9]* plan_digest=[0-9a-f]{64}")) and
  ($output | test("S2-A1 explicit-chain restore face=explicit restore=inc2 links=ftp:[1-9][0-9]* plan_digest=[0-9a-f]{64}")) and
  ($output | test("S2-A1 implicit-chain restore face=implicit restore=inc1 links=ftp:[1-9][0-9]* plan_digest=[0-9a-f]{64}")) and
  ($output | test("S2-A1 implicit-chain restore face=implicit restore=inc2 links=ftp:[1-9][0-9]* plan_digest=[0-9a-f]{64}")) and
  ($output | test("S2-A1 restore=inc[12] face=(explicit|implicit) export=[0-9a-f]{32} expected_snapshot=[0-9a-f]{64}")) and
  # Every restore is bound to the source snapshot recorded at that restore point, and
  # the harness then starts a brand-new pinned server on that export and digests what
  # it finds there. The backreference makes the comparison one the harness earned, not
  # a pair of independently invented values.
  # The fresh-container comparison is the fact the harness earned: four restored rows,
  # and in every one of them the digest the new server reported equals the digest the
  # worker recorded at that restore point. Written as a scan plus a value comparison
  # because a backreference is not portable across jq builds.
  ([$output | scan("S2-A1 restored=inc[12] face=(explicit|implicit) container_snapshot=[0-9a-f]{64} expected_snapshot=[0-9a-f]{64} match=true")] | length == 4) and
  ([$output | scan("container_snapshot=([0-9a-f]{64}) expected_snapshot=([0-9a-f]{64})")] | length > 0 and all(.[0] == .[1])) and
  (($output | split("container_snapshot=") | length) == 5) and
  (($output | split("S2-A1 restored=") | length) == 5) and
  (($output | test("S2-A1 restore=inc1 face=explicit export=[0-9a-f]{32} expected_snapshot=[0-9a-f]{64}")) and
   ($output | test("S2-A1 restore=inc2 face=explicit export=[0-9a-f]{32} expected_snapshot=[0-9a-f]{64}")) and
   ($output | test("S2-A1 restore=inc1 face=implicit export=[0-9a-f]{32} expected_snapshot=[0-9a-f]{64}")) and
   ($output | test("S2-A1 restore=inc2 face=implicit export=[0-9a-f]{32} expected_snapshot=[0-9a-f]{64}"))) and
  ($output | test("S2-A4 exports_started=4 snapshots_match=true new_mysql=mysql:8\\.4\\.11@sha256:[0-9a-f]{64}")) and
  # A downgrade, a false completion or a census that saw nothing is a refusal even
  # when every marker above is present.
  ($output | test("cleartext_marker=[1-9]") | not) and
  ($output | test("cleartext_secret=[1-9]") | not) and
  ($output | test("pairing_exact=false") | not) and
  ($output | test("plaintext_data=accepted") | not) and
  ($output | test("plaintext_fallback=(auto|ok|true)") | not) and
  ($output | test("data_resumed=false") | not) and
  # No FTPS face may ever report a credential of any kind; the plain face is allowed
  # to, and is required to, because that is what makes the zeros mean something.
  ($output | test("(explicit|implicit)_(user|pass|secret)_per_session=[1-9]") | not) and
  ($output | test("refusal kind=tls_[a-z]+ listener=[a-z]+ mode=[a-z]+ reply=[0-9]+ audit_sessions=[1-9][0-9]* user_per_session=[1-9]") | not) and
  ($output | test("S2-A3 (stored|probe_ok|probe_refused|bundle_attribution)[^\n]*credential_bytes=[1-9]") | not) and
  ($output | test("S2-A3 create=ok[^\n]*explicit=true") | not) and
  ($output | test("S2-A3 (reject|create=ok)[^\n]*plain=false") | not) and
  ($output | test("match=false") | not) and
  ($output | test("exports_started=[01235-9]") | not) and
  ($output | test("source_dirty=true") | not) and
  ($output | test("platform=linux/(arm64|arm|386)") | not)
' "$1"
