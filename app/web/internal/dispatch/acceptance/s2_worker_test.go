//go:build integration

package acceptance

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/dispatch"
	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/storage/ftp"
	"github.com/flyingEirc/Intern/internal/storage/ftp/s2fixture"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

// TestS2Worker is the S2 control-plane worker. It runs inside the pinned restore
// image (MySQL 8.4.11 plus Percona XtraBackup 8.4.0-7) as the source's own uid,
// against a real MySQL server, the real six-face Pure-FTPd FTPS fixture, and the real
// HTTP API with the real restore engine behind it.
//
// Every fact it reports is read back: the served repository row, the run and replica
// rows, the bytes the FTPS session returns, the fixture's own audit of what reached
// the server, or the export directory the engine published. Nothing here is a value
// the worker chose and then quoted back to itself.
func TestS2Worker(t *testing.T) {
	requireNative(t)
	environment := s2RequireFTPS(t)
	steps := []struct {
		name string
		run  func(*testing.T, *s2Environment)
	}{
		{"explicit-chain", func(t *testing.T, env *s2Environment) {
			s2Chain(t, env, s2fixture.ListenerExplicit, ftp.TLSModeExplicit)
		}},
		{"implicit-chain", func(t *testing.T, env *s2Environment) {
			s2Chain(t, env, s2fixture.ListenerImplicit, ftp.TLSModeImplicit)
		}},
		{"api-ftps", s2APIFTPS},
		{"refusal-api", s2RefusalAPI},
	}
	// The chains are independent deployments, but a failed one leaves its policy,
	// repository rows and exports in a state the next case cannot reason about, so
	// the run stops at the first failure like every other acceptance worker here.
	for _, step := range steps {
		if !t.Run(step.name, func(t *testing.T) { step.run(t, environment) }) {
			t.Fatalf("S2 case %s failed: the state it was to produce is unknown, so no later case runs", step.name)
		}
	}
}

// s2Environment is the fixture as the worker sees it: the manifest the six-face
// server published about itself, the trust bundle it republished, the account and
// cleartext probe marker it serves, and the tap log that records what actually
// reached it.
type s2Environment struct {
	host      string
	account   string
	password  string
	marker    string
	auditDir  string
	manifest  s2fixture.Fixture
	ca        string
	rogueAuth string
}

func s2RequireFTPS(t *testing.T) *s2Environment {
	t.Helper()
	environment := &s2Environment{
		host:     os.Getenv("INTERN_S2_HOST"),
		account:  os.Getenv("INTERN_S2_ACCOUNT"),
		password: os.Getenv("INTERN_S2_PASSWORD"),
		marker:   os.Getenv("INTERN_S2_MARKER"),
		auditDir: os.Getenv("INTERN_S2_AUDIT"),
	}
	environment.requireEnv(t)
	if environment.account != ftpUser {
		t.Fatalf("the FTPS fixture account is %q, and this build only knows %q", environment.account, ftpUser)
	}
	if len(environment.marker) < 16 {
		t.Fatal("the FTPS fixture injected a too-short probe marker")
	}
	environment.requireBundles(t)
	return environment
}

// requireEnv refuses a run that is not standing in front of the real fixture: every
// value the cases judge is injected by the harness and never defaulted here.
func (e *s2Environment) requireEnv(t *testing.T) {
	t.Helper()
	for name, value := range map[string]string{
		"INTERN_S2_HOST": e.host, "INTERN_S2_ACCOUNT": e.account,
		"INTERN_S2_PASSWORD": e.password, "INTERN_S2_MARKER": e.marker,
		"INTERN_S2_AUDIT": e.auditDir,
	} {
		if value == "" {
			t.Fatalf("%s is required: this worker only runs against the real FTPS fixture", name)
		}
	}
}

// requireBundles reads the manifest the fixture published about itself and both trust
// anchors it republished, and refuses a fixture that is not the locked six-face matrix.
func (e *s2Environment) requireBundles(t *testing.T) {
	t.Helper()
	manifest, err := s2fixture.ReadFixture(filepath.Join(e.auditDir, "fixture.txt"))
	if err != nil {
		t.Fatalf("read the FTPS fixture manifest: %v", err)
	}
	if matrixErr := manifest.MatchesMatrix(); matrixErr != nil {
		t.Fatalf("the FTPS fixture is not the locked six-face matrix: %v", matrixErr)
	}
	ca, caErr := os.ReadFile(filepath.Join(e.auditDir, "ca.pem"))
	if caErr != nil || !ftp.ValidCAPem(string(ca)) {
		t.Fatalf("the fixture published no usable trust bundle: %v", caErr)
	}
	// The rogue bundle is the second anchor: a repository that trusts it must still
	// refuse the faces the real CA issued, which is what makes an untrusted-chain
	// refusal a statement about trust rather than about a missing bundle.
	rogue, rogueErr := os.ReadFile(filepath.Join(e.auditDir, "rogue-ca.pem"))
	if rogueErr != nil || !ftp.ValidCAPem(string(rogue)) {
		t.Fatalf("the fixture published no rogue trust bundle: %v", rogueErr)
	}
	e.manifest = manifest
	e.ca = string(ca)
	e.rogueAuth = string(rogue)
}

// address is one face's host and port, read from the manifest the fixture wrote. The
// manifest names the face as host:port; the repository body carries them apart.
func (e *s2Environment) address(t *testing.T, face string) (string, int) {
	t.Helper()
	value, err := e.manifest.Address(e.host, face)
	if err != nil {
		t.Fatalf("the fixture has no %s face: %v", face, err)
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		t.Fatalf("the fixture address of %s is not host:port: %v", face, err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("the fixture published no numeric port for %s: %v", face, err)
	}
	return host, number
}

// sessionsBefore is the audit baseline of one face. Records are appended when a
// session closes, so a case takes the count first and judges only what appeared after
// it, never a whole-file scan that could credit an earlier case's session.
func (e *s2Environment) sessionsBefore(t *testing.T, face string) int {
	t.Helper()
	return len(e.sessions(t, face))
}

func (e *s2Environment) sessions(t *testing.T, face string) []s2fixture.Audit {
	t.Helper()
	records, err := s2fixture.ReadAudit(filepath.Join(e.auditDir, "control.jsonl"))
	if err != nil {
		t.Fatalf("read the fixture audit: %v", err)
	}
	return s2fixture.SessionsFor(records, face)
}

// awaitNewSessions waits for the tap to publish a face's census, then returns every
// session recorded since the baseline. An empty delta is a refusal: a case that
// believed a session it never saw would report a census of nothing.
func (e *s2Environment) awaitNewSessions(t *testing.T, face string, before int) []s2fixture.Audit {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		sessions := e.sessions(t, face)
		if len(sessions) > before {
			return sessions[before:]
		}
		if time.Now().After(deadline) {
			t.Fatalf("the %s face recorded no session between %d and now", face, before)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// requireNoCredential demands that a face's sessions received bytes but never a
// credential. A truncated record is refused rather than read as a zero.
func requireNoCredential(t *testing.T, e *s2Environment, face string, before int) int {
	t.Helper()
	sessions := e.awaitNewSessions(t, face, before)
	for _, session := range sessions {
		if session.Truncated {
			t.Fatalf("the %s face census was truncated and cannot prove absence", face)
		}
		if session.ToServerBytes == 0 {
			t.Fatalf("the %s face recorded a session that received no bytes", face)
		}
		if user := session.MustCount(t, s2fixture.ToServer, s2fixture.NeedleUser); user != 0 {
			t.Fatalf("the %s face accepted %d cleartext USER commands", face, user)
		}
		if pass := session.MustCount(t, s2fixture.ToServer, s2fixture.NeedlePass); pass != 0 {
			t.Fatalf("the %s face accepted %d cleartext PASS commands", face, pass)
		}
		if secret := session.MustCount(t, s2fixture.ToServer, s2fixture.NeedleSecret); secret != 0 {
			t.Fatalf("the %s face accepted the account password in cleartext", face)
		}
	}
	return len(sessions)
}

// s2CreateFTPSRepository builds one FTPS repository row through the API. The body is
// the documented shape: tls_mode beside kind, ca_pem inside the ftp object, and the
// password only in the write, never served back.
func (f *fixture) s2CreateFTPSRepository(t *testing.T, name, mode, host string, port int, ca string) string {
	t.Helper()
	body := fmt.Sprintf(
		`{"name":%q,"kind":"ftp","tls_mode":%q,"ftp":{"host":%q,"port":%d,"base_path":"/",`+
			`"username":%q,"ca_pem":%q},"password":%q}`,
		name, mode, host, port, ftpUser, ca, f.secret.ftp)
	raw := f.admin.request(t, http.MethodPost, "/api/v1/repositories", body, http.StatusCreated)
	return f.decodeID(t, raw)
}

// s2ProbeRepository runs the control plane's own repository probe and returns the
// served outcome fields, which is the only probe a deployment operator can call.
func (f *fixture) s2ProbeRepository(t *testing.T, id string) (bool, string, int) {
	t.Helper()
	raw := f.admin.request(t, http.MethodPost, "/api/v1/repositories/"+id+"/probe", "", http.StatusOK)
	var served struct {
		OK    bool `json:"ok"`
		Error *struct {
			Code      string `json:"code"`
			ReplyCode int    `json:"reply_code"`
		} `json:"error"`
	}
	must(t, json.Unmarshal(raw, &served))
	if served.Error == nil {
		return served.OK, "", 0
	}
	return served.OK, served.Error.Code, served.Error.ReplyCode
}

// s2RepositoryView reads the served repository row.
func (f *fixture) s2RepositoryView(t *testing.T, id string) map[string]any {
	t.Helper()
	raw := f.admin.request(t, http.MethodGet, "/api/v1/repositories/"+id, "", http.StatusOK)
	var served map[string]any
	must(t, json.Unmarshal(raw, &served))
	return served
}

// s2Chain drives one FTPS face through the whole product path: probe, a real
// full + incremental chain replicated over FTPS to verified, a read-back of every
// published object, and two restores that must find their bytes on the FTPS copy.
func s2Chain(t *testing.T, environment *s2Environment, face, mode string) {
	t.Helper()
	// A per-face fixture root, so the two faces are genuinely separate deployments
	// with their own metadata database, work root and policy, on one live source.
	t.Setenv("INTERN_W4_ROOT", filepath.Join(os.Getenv("INTERN_W4_ROOT"), face))
	fixture := newFixture(t)
	fixture.s2Topology(t, environment, face, mode)
	fixture.startDispatch(t)

	probe, code, reply := fixture.s2ProbeRepository(t, fixture.ftpID)
	if !probe {
		t.Fatalf("the %s face probe failed code=%s reply=%d", face, code, reply)
	}
	listener := environment.manifest.Listeners[face]
	t.Logf("S2-A1 %s-chain probe face=%s tls_mode=%s ca_pem=pinned port=%d tls_required=%s probe=ok",
		face, face, mode, listener.Client, listener.TLSLevel)

	// The source must hold substantive data before the full backup, or restoring a
	// full backup would prove nothing about the bytes the FTPS copy carries.
	for round := range 4 {
		fixture.dml(t, round)
	}
	backupIDs, snapshots := s2RunChain(t, fixture, face)
	t.Logf("S2-A1 %s-chain upload face=%s chain=seq:1,2,3 replicas_verified=3 upload=full,inc1,inc2 backups=%s",
		face, face, strings.Join(backupIDs, ","))

	// Read back through an FTPS session of the same mode, over the same fixture, and
	// compare with the local copy: the replica must be byte-identical, not merely
	// acknowledged.
	members := e2eMembers(t, fixture)
	remote := s2OpenFTPS(t, fixture, environment, face, mode)
	objects, payload := s2ReadBack(t, fixture, remote, members, backupIDs, face)
	t.Logf("S2-A1 %s-chain readback face=%s objects=%d local_sha_match=true ftps_sha_match=true ftp_payload_bytes=%d",
		face, face, objects, payload)

	// Both restores must find their bytes on the FTPS copy only: the local payloads
	// are removed first, so a restore that silently used the local repository would
	// fail instead of passing on the wrong source.
	s2DropLocalPayloads(t, fixture, members)
	for index, label := range []string{"inc1", "inc2"} {
		s2RestoreFromFTPS(t, fixture, face, label, backupIDs[index+1], snapshots[index+1])
	}
	must(t, remote.Close())
}

// s2RunChain runs the full and both incrementals in order, each to completed with a
// verified replica on the face's FTPS repository, and returns the published backup
// ids with the source snapshot taken right after each run.
func s2RunChain(t *testing.T, fixture *fixture, face string) ([]string, [3]w5Snapshot) {
	t.Helper()
	var snapshots [3]w5Snapshot
	backupIDs := make([]string, 3)
	for index, kind := range [3]string{"full", "inc1", "inc2"} {
		if index > 0 {
			fixture.dml(t, index)
		}
		kind2 := store.RunKindFull
		if index > 0 {
			kind2 = store.RunKindIncremental
		}
		id := fixture.triggerRun(t, fixture.e2ePolicy, kind2, "s2-"+face+"-"+kind)
		run := fixture.waitRun(t, id, store.RunStatusCompleted)
		if run.BackupID == nil || *run.BackupID == "" {
			t.Fatalf("the %s %s run published no backup", face, kind)
		}
		replica := fixture.waitReplica(t, *run.BackupID, "verified")
		if replica.RepositoryID != fixture.ftpID {
			t.Fatalf("the verified replica of %s belongs to %s, not the %s FTPS repository",
				kind, replica.RepositoryID, face)
		}
		snapshots[index] = fixture.w5Snapshot(t)
		backupIDs[index] = *run.BackupID
	}
	return backupIDs, snapshots
}

// s2ReadBack digests every chain member over the FTPS session and the local copy,
// demanding they match, and returns the member count and total FTPS payload bytes.
func s2ReadBack(t *testing.T, fixture *fixture, remote *ftp.Repository, members []dispatch.BackupView,
	backupIDs []string, face string,
) (int, int64) {
	t.Helper()
	objects := make([]string, 0, 3)
	var payload int64
	for index, member := range members {
		if member.ID != backupIDs[index] {
			t.Fatalf("chain member %d is %s, the run published %s", index, member.ID, backupIDs[index])
		}
		local := fixture.hashLocal(t, member.ObjectPath)
		ftpsDigest := fixture.hashFTP(t, remote, member.ObjectPath)
		if local != ftpsDigest {
			t.Fatalf("the %s copy of %s digests %s, the local copy %s", face, member.ID, ftpsDigest, local)
		}
		size, err := remote.Size(t.Context(), member.ObjectPath+"/"+objectPayload)
		must(t, err)
		payload += size
		objects = append(objects, member.ID)
	}
	return len(objects), payload
}

// s2RestoreFromFTPS plans and executes one restore whose every link must read the
// face's FTPS repository, then waits for and judges its completion.
func s2RestoreFromFTPS(t *testing.T, fixture *fixture, face, label, backupID string, snapshot w5Snapshot) {
	t.Helper()
	plan, _ := fixture.w5Plan(t, backupID, http.StatusOK)
	if len(plan.Links) == 0 {
		t.Fatalf("the %s %s plan has no links", face, label)
	}
	for _, link := range plan.Links {
		if link.Source != "ftp" || link.RepositoryID != fixture.ftpID {
			t.Fatalf("the %s %s plan reads %s/%s, want the %s FTPS repository",
				face, label, link.Source, link.RepositoryID, face)
		}
	}
	id, _ := fixture.w5Execute(t, w5ExecuteBody(backupID, plan.Digest, fixture.secret.admin, false, nil),
		"s2-"+face+"-restore-"+label, http.StatusAccepted)
	s2RequireCompleted(t, fixture, id, plan, face, label)
	t.Logf("S2-A1 %s-chain restore face=%s restore=%s links=ftp:%d plan_digest=%s",
		face, face, label, len(plan.Links), plan.Digest)
	// The harness starts a brand-new pinned server on this export and compares
	// its data snapshot with the source snapshot recorded above, in this run.
	t.Logf("S2-A1 restore=%s face=%s export=%s expected_snapshot=%s", label, face, id, snapshot.digest)
}

// s2Topology builds the per-face deployment: a local repository, an FTPS replica on
// the requested face, the live source and one chain policy, enabled through its
// preflight, which itself probes the FTPS replica.
func (f *fixture) s2Topology(t *testing.T, environment *s2Environment, face, mode string) {
	t.Helper()
	f.localID = f.createLocal(t)
	host, port := environment.address(t, face)
	f.ftpID = f.s2CreateFTPSRepository(t, "s2-"+face, mode, host, port, environment.ca)
	f.sourceID = f.createSource(t)
	f.e2ePolicy = f.createPolicy(t, "s2-"+face, 3)
	f.enable(t, f.e2ePolicy)
}

// s2OpenFTPS opens the fixture's namespace directly, so the read-back is a real FTPS
// transfer rather than a value the control plane reported about itself.
func s2OpenFTPS(t *testing.T, f *fixture, environment *s2Environment, face, mode string) *ftp.Repository {
	t.Helper()
	host, port := environment.address(t, face)
	repository, err := ftp.Open(t.Context(), ftp.Config{
		Address: host + ":" + strconv.Itoa(port), Username: environment.account, Password: f.secret.ftp,
		BasePath: "/", InstanceID: f.instanceID, TLSMode: mode, CAPem: environment.ca,
		DialTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second,
	})
	must(t, err)
	t.Cleanup(func() { _ = repository.Close() })
	return repository
}

// s2DropLocalPayloads removes the local copy of every published chain member, so a
// later restore has one possible source left: the FTPS replica.
func s2DropLocalPayloads(t *testing.T, f *fixture, members []dispatch.BackupView) {
	t.Helper()
	repository, err := local.Open(f.layout.local)
	must(t, err)
	defer func() { must(t, repository.Close()) }()
	for _, member := range members {
		must(t, repository.ReclaimPublished(member.ObjectPath))
	}
	if keys := f.listLocalObjects(t); len(keys) != 0 {
		t.Fatalf("local objects survived the payload removal: %s", strings.Join(keys, ","))
	}
}

// s2RequireCompleted demands every persisted fact of a completed restore: the export
// path and size the row carries, the directory the engine published (0750, with a data
// directory inside it), and the phase sequence that proves the bytes really were read
// from the FTPS copy rather than assembled locally.
func s2RequireCompleted(t *testing.T, f *fixture, id string, plan store.RestorePlan, face, label string) {
	t.Helper()
	restore := f.w5Wait(t, id, store.RestoreStatusCompleted)
	s2RequireCompletedRow(t, f, id, restore, plan, face, label)
	s2RequireCompletedExport(t, f, id, face, label)
}

// s2RequireCompletedRow judges the stored row: the export path and size it carries, the
// plan it was confirmed against, and a failure-free terminal state.
func s2RequireCompletedRow(t *testing.T, f *fixture, id string, restore store.Restore, plan store.RestorePlan, face, label string) {
	t.Helper()
	if restore.ExportPath == nil || *restore.ExportPath != filepath.Join(f.layout.export, id) {
		t.Fatalf("%s %s export_path = %v, want %s", face, label, restore.ExportPath, filepath.Join(f.layout.export, id))
	}
	if restore.ExportBytes == nil || *restore.ExportBytes <= 0 {
		t.Fatalf("%s %s export_bytes = %v", face, label, restore.ExportBytes)
	}
	if restore.WorkDir == nil || *restore.WorkDir == "" {
		t.Fatalf("%s %s records no work directory", face, label)
	}
	if restore.ErrorClass != nil || restore.ErrorCode != nil {
		t.Fatalf("%s %s completed with failure %v/%v", face, label, restore.ErrorClass, restore.ErrorCode)
	}
	s2RequireConfirmedPlan(t, f, restore, plan, face, label)
}

// s2RequireConfirmedPlan judges that the stored plan is the one confirmed and that
// every stored link reads the face's FTPS repository.
func s2RequireConfirmedPlan(t *testing.T, f *fixture, restore store.Restore, plan store.RestorePlan, face, label string) {
	t.Helper()
	if restore.PlanDigest != plan.Digest || restore.BackupID != plan.BackupID {
		t.Fatalf("%s %s stores plan_digest=%s backup=%s, confirmed %s for %s",
			face, label, restore.PlanDigest, restore.BackupID, plan.Digest, plan.BackupID)
	}
	if len(restore.Plan.Links) != len(plan.Links) {
		t.Fatalf("%s %s stored %d links, the plan offered %d", face, label, len(restore.Plan.Links), len(plan.Links))
	}
	for _, link := range restore.Plan.Links {
		if link.Source != "ftp" || link.RepositoryID != f.ftpID {
			t.Fatalf("%s %s completed from a %s link (%s), not the %s FTPS repository",
				face, label, link.Source, link.RepositoryID, face)
		}
	}
}

// s2RequireCompletedExport judges the published directory itself: the mode the engine
// gave it, a non-empty data directory, and the phase sequence that shows the bytes came
// over the FTPS copy.
func s2RequireCompletedExport(t *testing.T, f *fixture, id, face, label string) {
	t.Helper()
	mode, bytes, _ := f.w5ExportState(t, id)
	if mode != "0750" {
		t.Fatalf("%s %s export directory mode = %s, want 0750", face, label, mode)
	}
	if bytes <= 0 {
		t.Fatalf("%s %s export directory holds %d bytes", face, label, bytes)
	}
	seen := f.w5PhaseSeen(t, id)
	for _, phase := range []string{"fetching", "extracting", "preparing", "exporting"} {
		if !slices.Contains(seen, phase) {
			t.Fatalf("%s %s never reported the %s phase, saw %s", face, label, phase, strings.Join(seen, ","))
		}
	}
}

// s2APIFTPS exercises the documented repository contract over FTPS: both modes are
// admitted, the bundle is public data that can be rotated, an unusable bundle is
// refused where it is written, the transport is immutable, and no FTPS row is ever
// described as carrying plaintext credentials.
func s2APIFTPS(t *testing.T, environment *s2Environment) {
	t.Helper()
	t.Setenv("INTERN_W4_ROOT", filepath.Join(os.Getenv("INTERN_W4_ROOT"), "api"))
	fixture := newFixture(t)
	fixture.localID = fixture.createLocal(t)
	fixture.sourceID = fixture.createSource(t)
	explicitHost, explicitPort := environment.address(t, s2fixture.ListenerExplicit)
	implicitHost, implicitPort := environment.address(t, s2fixture.ListenerImplicit)
	created := []string{
		fixture.s2CreateFTPSRepository(t, "s2-api-explicit", ftp.TLSModeExplicit, explicitHost, explicitPort, environment.ca),
		fixture.s2CreateFTPSRepository(t, "s2-api-implicit", ftp.TLSModeImplicit, implicitHost, implicitPort, environment.ca),
	}
	for index, id := range created {
		s2RequireServedFTPS(t, fixture, environment, id, []string{ftp.TLSModeExplicit, ftp.TLSModeImplicit}[index])
	}
	// Rotation: a new bundle that still contains the same anchor is admitted, and the
	// row that is served afterwards carries it. A replacement carries name and kind
	// like every PUT, and omits the password, which keeps the stored secret.
	rotated := environment.ca + environment.ca
	body := s2RepositoryBody("s2-api-explicit", ftp.TLSModeExplicit, explicitHost, explicitPort, rotated, "")
	fixture.admin.request(t, http.MethodPut, "/api/v1/repositories/"+created[0], body, http.StatusOK)
	if config, _ := fixture.s2RepositoryView(t, created[0])["ftp"].(map[string]any); config == nil || config["ca_pem"] != rotated {
		t.Fatal("the rotated trust bundle was not served back")
	}
	// Each refused body is valid except for its bundle, so the 422 is the ca_pem rule.
	refused := []struct {
		why  string
		body string
	}{
		{"not pem", s2RepositoryBody("s2-api-not-pem", ftp.TLSModeExplicit, explicitHost, explicitPort,
			"-----BEGIN CER----", fixture.secret.ftp)},
		{"oversized", s2RepositoryBody("s2-api-oversized", ftp.TLSModeExplicit, explicitHost, explicitPort,
			strings.Repeat("A", ftp.MaxCAPemBytes+1), fixture.secret.ftp)},
	}
	for _, case_ := range refused {
		raw := fixture.admin.request(t, http.MethodPost, "/api/v1/repositories", case_.body, http.StatusUnprocessableEntity)
		if strings.Contains(string(raw), fixture.secret.ftp) {
			t.Fatalf("the %q refusal echoes the submitted password", case_.why)
		}
	}
	// The transport is immutable: the same row cannot be moved from one mode to the
	// other; the handler refuses the change as a 422 shape violation rather than
	// silently re-selecting the transport.
	mutation := s2RepositoryBody("s2-api-explicit", ftp.TLSModeImplicit, explicitHost, explicitPort, environment.ca, "")
	modeChange := http.StatusUnprocessableEntity
	fixture.admin.request(t, http.MethodPut, "/api/v1/repositories/"+created[0], mutation, modeChange)
	if view := fixture.s2RepositoryView(t, created[0]); view["tls_mode"] != ftp.TLSModeExplicit {
		t.Fatalf("the refused mode change left tls_mode=%v", view["tls_mode"])
	}
	// And a plain repository may not carry a bundle at all.
	plainWithCA := s2RepositoryBody("s2-api-plain-ca", ftp.TLSModePlain, explicitHost, explicitPort,
		environment.ca, fixture.secret.ftp)
	fixture.admin.request(t, http.MethodPost, "/api/v1/repositories", plainWithCA, http.StatusUnprocessableEntity)

	preflight := make([]string, 0, len(created))
	for _, id := range created {
		probe, code, reply := fixture.s2ProbeRepository(t, id)
		if !probe {
			t.Fatalf("the %s repository probe failed code=%s reply=%d", id, code, reply)
		}
		preflight = append(preflight, "ok")
	}
	t.Logf("S2-A3 api_ftps create=201,%d update=200 preflight=%s plaintext_credentials=false invalid_ca=422,%d"+
		" mode_change=%d plain_with_ca=422 contract=ok faces=%s",
		len(created)-1, strings.Join(preflight, ","), len(refused), modeChange,
		strings.Join([]string{s2fixture.ListenerExplicit, s2fixture.ListenerImplicit}, ","))
}

// s2RepositoryBody is one complete FTP repository write: name, kind, transport, the
// fixture account and the given bundle. An empty password is omitted, which on PUT
// keeps the stored secret.
func s2RepositoryBody(name, mode, host string, port int, ca, password string) string {
	body := fmt.Sprintf(`{"name":%q,"kind":"ftp","tls_mode":%q,"ftp":{"host":%q,"port":%d,"base_path":"/",`+
		`"username":%q,"ca_pem":%q}`, name, mode, host, port, ftpUser, ca)
	if password != "" {
		body += fmt.Sprintf(`,"password":%q`, password)
	}
	return body + "}"
}

// s2RequireServedFTPS judges one served FTPS repository row: the selected transport,
// a false plaintext_credentials, every documented field, the submitted bundle, and
// no trace of the account password.
func s2RequireServedFTPS(t *testing.T, fixture *fixture, environment *s2Environment, id, mode string) {
	t.Helper()
	view := fixture.s2RepositoryView(t, id)
	if view["tls_mode"] != mode {
		t.Fatalf("served tls_mode = %v, want the selected transport", view["tls_mode"])
	}
	if plaintext, ok := view["plaintext_credentials"].(bool); !ok || plaintext {
		t.Fatalf("an FTPS repository is served as plaintext_credentials=%v", view["plaintext_credentials"])
	}
	for _, key := range []string{"kind", "tls_mode", "ftp", "plaintext_credentials"} {
		if _, present := view[key]; !present {
			t.Fatalf("the served repository carries no %q field", key)
		}
	}
	// ca_pem is part of the FTP configuration object (openapi Repository.ftp.ca_pem).
	config, _ := view["ftp"].(map[string]any)
	if config == nil || config["ca_pem"] != environment.ca {
		t.Fatalf("the served ca_pem is not the bundle that was submitted")
	}
	if strings.Contains(fmt.Sprint(view), fixture.secret.ftp) {
		t.Fatal("the served repository row discloses the account password")
	}
}

// s2RefusalAPI proves the four locked refusals through the control plane: the API
// accepts the configuration (a wrong bundle is a configuration the operator owns), the
// probe refuses it with the transport class, the policy cannot be enabled, and the
// server's own audit records that no credential ever arrived. A census with nothing in
// it proves nothing, so the plain face is also driven in plaintext and its audit is
// required to show the credential it really received.
func s2RefusalAPI(t *testing.T, environment *s2Environment) {
	t.Helper()
	t.Setenv("INTERN_W4_ROOT", filepath.Join(os.Getenv("INTERN_W4_ROOT"), "refusal"))
	fixture := newFixture(t)
	fixture.localID = fixture.createLocal(t)
	fixture.sourceID = fixture.createSource(t)
	// bundle names the trust anchor each repository pins: "pinned" is the fixture
	// CA, "rogue" the second anchor that issued nothing the good faces serve. The
	// rogue face is refused with the real CA pinned; the explicit face, which the
	// real CA did issue, is refused only because the rogue anchor is pinned.
	for _, entry := range []s2Refusal{
		{s2fixture.ListenerRogue, ftp.TLSModeExplicit, "tls_untrusted", "pinned", environment.ca},
		{s2fixture.ListenerWrongHost, ftp.TLSModeExplicit, "tls_hostname", "pinned", environment.ca},
		{s2fixture.ListenerExpired, ftp.TLSModeExplicit, "tls_expired", "pinned", environment.ca},
		{s2fixture.ListenerPlain, ftp.TLSModeExplicit, "tls_negotiation", "pinned", environment.ca},
		{s2fixture.ListenerExplicit, ftp.TLSModeExplicit, "tls_untrusted", "rogue", environment.rogueAuth},
	} {
		s2RequireRefused(t, fixture, environment, entry)
	}
	s2CensusControl(t, environment)
}

// s2RequireRefused drives one locked refusal: the probe fails with the expected
// transport class, the policy cannot be enabled, the face's audit shows no
// credential, and the row is still not described as plaintext.
func s2RequireRefused(t *testing.T, fixture *fixture, environment *s2Environment, entry s2Refusal) {
	t.Helper()
	face, mode, expect := entry.face, entry.mode, entry.expect
	host, port := environment.address(t, face)
	id := fixture.s2CreateFTPSRepository(t, "s2-refuse-"+face+"-"+mode, mode, host, port, entry.ca)
	before := environment.sessionsBefore(t, face)
	probe, code, reply := fixture.s2ProbeRepository(t, id)
	if probe {
		t.Fatalf("the %s face (%s bundle) was accepted by the probe", face, entry.bundle)
	}
	if code != expect {
		t.Fatalf("the %s face refused with code=%s reply=%d, want %s", face, code, reply, expect)
	}
	// An operator cannot enable a policy whose replica the transport refuses: the
	// preflight that runs on enable is the gate, and a disabled policy means no
	// backup was ever queued, so no credential was ever offered for a transfer.
	policy := fixture.createPolicyNamed(t, "s2-refuse-policy-"+face, fixture.sourceID, fixture.localID, id)
	fixture.admin.requestWithID(t, http.MethodPost, "/api/v1/policies/"+policy+"/enable", "", "enable-"+policy,
		http.StatusConflict)
	sessions := requireNoCredential(t, environment, face, before)
	view := fixture.s2RepositoryView(t, id)
	if plaintext, _ := view["plaintext_credentials"].(bool); plaintext {
		t.Fatalf("a refused %s repository is still described as plaintext", face)
	}
	t.Logf("S2-A2 refusal_api face=%s bundle=%s mode=%s kind=%s probe=refused reply=%d enable=409"+
		" audit_sessions=%d user_per_session=0 pass_per_session=0 secret_per_session=0",
		face, entry.bundle, mode, code, reply, sessions)
}

// s2CensusControl is the census control: the same tap on the same fixture, driven
// in plaintext, must see the credential it claims to have never seen in the
// refusals. Without this the zeros are just an empty file.
func s2CensusControl(t *testing.T, environment *s2Environment) {
	t.Helper()
	host, port := environment.address(t, s2fixture.ListenerPlain)
	before := environment.sessionsBefore(t, s2fixture.ListenerPlain)
	repository, err := ftp.Open(t.Context(), ftp.Config{
		Address: host + ":" + strconv.Itoa(port), Username: environment.account, Password: environment.password,
		BasePath: "/", InstanceID: "census-control", TLSMode: ftp.TLSModePlain,
		DialTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second,
	})
	must(t, err)
	// The object path is cleartext on this face, so it carries both generated needles -
	// the probe marker and the account password - and the census has values only this
	// run can produce. The key starts with the instance, which is the namespace rule
	// every repository write obeys.
	key := "census-control/" + environment.marker + "-" + environment.password + ".bin"
	sink, err := repository.Create(t.Context(), key)
	must(t, err)
	_, err = sink.Write([]byte("census control payload"))
	must(t, err)
	_, err = sink.Commit(t.Context())
	must(t, err)
	must(t, repository.Close())
	user, pass, secret, marker := 0, 0, 0, 0
	for _, session := range environment.awaitNewSessions(t, s2fixture.ListenerPlain, before) {
		if session.Truncated {
			t.Fatal("the census control session was truncated")
		}
		user += session.MustCount(t, s2fixture.ToServer, s2fixture.NeedleUser)
		pass += session.MustCount(t, s2fixture.ToServer, s2fixture.NeedlePass)
		secret += session.MustCount(t, s2fixture.ToServer, s2fixture.NeedleSecret)
		marker += session.MustCount(t, s2fixture.ToServer, s2fixture.NeedleMarker)
	}
	if user == 0 || pass == 0 || secret == 0 || marker == 0 {
		t.Fatalf("the plaintext census control saw user=%d pass=%d secret=%d marker=%d, so no zero above means anything",
			user, pass, secret, marker)
	}
	t.Logf("S2-A2 refusal_api census_control face=plain mode=plain user=%d pass=%d secret=%d marker=%d",
		user, pass, secret, marker)
}

// s2Refusal is one locked refusal: the face, the transport it selects, the transport
// class the control plane must report, and the name and contents of the bundle it
// trusts.
type s2Refusal struct {
	face   string
	mode   string
	expect string
	bundle string
	ca     string
}

// createPolicyNamed is the API-shaped policy the refusal case needs: it names the
// repository whose transport is refused, so the enable preflight must stop it.
func (f *fixture) createPolicyNamed(t *testing.T, name, sourceID, localID, replicaID string) string {
	t.Helper()
	body := fmt.Sprintf(
		`{"name":%q,"source_id":%q,"local_repository_id":%q,"replica_repository_id":%q,`+
			`"full_cron":%q,"incremental_cron":%q,"timezone":"UTC","max_chain_length":3,`+
			`"parallel":1,"throttle":0,"zstd_level":3,"page_tracking":false,"enabled":false}`,
		name, sourceID, localID, replicaID, cronFull, cronInc)
	return f.decodeID(t, f.admin.request(t, http.MethodPost, "/api/v1/policies", body, http.StatusCreated))
}
