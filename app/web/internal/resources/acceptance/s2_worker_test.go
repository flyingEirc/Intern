//go:build integration

package acceptance

// TestS2APIWorker is the Web-facing half of the S2 FTPS acceptance. It runs the real
// control-plane handler over a real TLS listener, exactly as TestW3Worker does, and
// points its repositories at the real six-face Pure-FTPd TLS fixture on the private
// network. The fixture stays the oracle for everything it can answer: the address and
// port of every face come from the manifest it published about itself, the trust
// bundle is the file it republished on the shared audit volume, and what actually
// reached the server is read back from the tap census. A missing fixture, manifest,
// bundle or census record is a failure, never a skip and never a reduced case set.
//
// The three cases are the locked S2-A3 set: a3-create (an FTPS repository is stored,
// echoed and audited as protected, while a plain repository in the same run keeps its
// plaintext flag and its audit detail), a3-reject (the frozen 422 rules of a
// transport configuration), and a3-probe (the two FTPS faces work, the four unsafe
// configurations are refused by name, and the census shows no credential ever reached
// a refused face).

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/storage/ftp"
	"github.com/flyingEirc/Intern/internal/storage/ftp/s2fixture"
)

// The FTPS fixture the harness shares with the transport worker: the DNS alias of the
// fixture container, the account every face accepts, the audit volume holding the
// manifest, the census and both public trust bundles, and the generated password of
// that account. These are the harness's own container environment names; the value of
// s2EnvPassword is never printed.
const (
	s2EnvHost     = "INTERN_S2_HOST"
	s2EnvAccount  = "INTERN_S2_ACCOUNT"
	s2EnvAudit    = "INTERN_S2_AUDIT"
	s2EnvPassword = "INTERN_S2_PASSWORD"
)

// The locked S2-A3 marker lines. Every value in them is an observed HTTP status, a
// read-back field, an audit row or a census count, never a restated expectation.
const (
	s2MarkerCreate = "S2-A3 create=ok explicit=%d implicit=%d plain=%d" +
		" plaintext_credentials explicit=%t,implicit=%t,plain=%t ca_pem=stored"
	s2MarkerReject = "S2-A3 reject: unparsable_ca=%d plain_with_ca=%d oversized_ca=%d" +
		" unknown_mode=%d mode_change=%d"
	s2MarkerProbe = "S2-A3 probe: ok=%s untrusted=%s hostname=%s expired=%s plain_only=%s" +
		" audit=plaintext_ftp=%t"

	// s2PlaintextDetail is the risk flag a plain FTP create records in its own success
	// audit. store.AuditDetail omits it when false, so its absence is what an FTPS
	// create must show.
	s2PlaintextDetail = `"plaintext_ftp":true`
)

// The four locked refusal codes the probe case reads out of the API's own failure
// body. They are the stable transport kinds of internal/storage/ftp.
const (
	s2KindUntrusted   = "tls_untrusted"
	s2KindHostname    = "tls_hostname"
	s2KindExpired     = "tls_expired"
	s2KindNegotiation = "tls_negotiation"
)

// s2CensusWait bounds how long a case waits for a tap to publish the session it just
// caused. A tap writes its record when the connection closes, so an assertion made
// immediately after a client-side failure would race the fixture instead of judging it.
const s2CensusWait = 30 * time.Second

// s2State is the fixture as it describes itself, plus the repositories this run stored
// through the API. Nothing in it is a port, a host or a certificate this worker hoped
// for.
type s2State struct {
	host, account, password        string
	manifest                       s2fixture.Fixture
	auditPath, caPath, rogueCAPath string
	ca, rogueCA                    string

	// The three repositories a3-create establishes, read back from their responses,
	// with the status each create answered.
	explicit, implicit, plain      store.Repository
	explicitStatus, implicitStatus int
	plainStatus                    int

	// ftps holds every explicit or implicit repository this run created, including the
	// ones the probe case adds for the unsafe faces. The audit sweep covers all of them.
	ftps []store.Repository
}

func TestS2APIWorker(t *testing.T) {
	f := newFixture(t)
	state := requireS2State(t)
	steps := []struct {
		name string
		run  func(*testing.T)
	}{
		{"a3-create", func(t *testing.T) { f.s2Create(t, state) }},
		{"a3-reject", func(t *testing.T) { f.s2Reject(t, state) }},
		{"a3-probe", func(t *testing.T) { f.s2Probe(t, state) }},
	}
	// The cases share one store, one fixture and one census stream: a case that failed
	// leaves the census every later case judges uninterpretable, so the run stops here
	// rather than reporting facts it never observed.
	for _, step := range steps {
		if !t.Run(step.name, func(t *testing.T) { step.run(t) }) {
			t.Fatalf("S2 case %s failed: the shared fixture state the later cases judge is unknown", step.name)
		}
	}
}

// requireS2State refuses any run that is not standing in front of the real FTPS
// fixture. It reads the manifest the fixture wrote about itself, requires it to be the
// six-face matrix the harness started, and proves the trust bundle it is about to hand
// the API is the file the fixture published.
func requireS2State(t *testing.T) *s2State {
	t.Helper()
	state := &s2State{
		host: os.Getenv(s2EnvHost), account: os.Getenv(s2EnvAccount),
		password: os.Getenv(s2EnvPassword),
	}
	audit := os.Getenv(s2EnvAudit)
	for name, value := range map[string]string{
		s2EnvHost: state.host, s2EnvAccount: state.account,
		s2EnvPassword: state.password, s2EnvAudit: audit,
	} {
		if value == "" {
			t.Fatalf("%s is required: this worker only runs against the real FTPS fixture", name)
		}
	}
	if len(state.password) < 12 {
		t.Fatal("the FTPS fixture injected an unusable account credential")
	}
	state.auditPath = filepath.Join(audit, "control.jsonl")
	state.caPath = filepath.Join(audit, "ca.pem")
	state.rogueCAPath = filepath.Join(audit, "rogue-ca.pem")

	manifest, err := s2fixture.ReadFixture(filepath.Join(audit, "fixture.txt"))
	if err != nil {
		t.Fatalf("read the fixture manifest: %v", err)
	}
	if err := manifest.MatchesMatrix(); err != nil {
		t.Fatalf("the fixture is not the six-face FTPS matrix: %v", err)
	}
	state.manifest = manifest
	state.ca = state.bundle(t, state.caPath)
	state.rogueCA = state.bundle(t, state.rogueCAPath)
	if state.ca == state.rogueCA {
		t.Fatal("the fixture published one bundle for both ca.pem and rogue-ca.pem")
	}
	// The bundle the API stores must be exactly the file the fixture published, which
	// is the only way an untrusted refusal can be attributed to the wrong trust anchor
	// rather than to a bundle this worker made up.
	digest := sha256.Sum256([]byte(state.ca))
	if got := hex.EncodeToString(digest[:]); got != manifest.CAFingerprint {
		t.Fatalf("ca.pem is not the bundle the fixture published: sha256=%s manifest=%s", got, manifest.CAFingerprint)
	}
	return state
}

// bundle reads one public trust bundle and refuses a file the transport itself could
// not dial with, so an admitted configuration is always a usable one.
func (st *s2State) bundle(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the published trust bundle: %v", err)
	}
	bundle := string(raw)
	if !ftp.ValidCAPem(bundle) {
		t.Fatalf("%s is not a trust bundle the FTPS transport accepts", path)
	}
	return bundle
}

// address names one face by the port the fixture published for it. The host is always
// the fixture's DNS alias, because the certificate identity the transport checks is
// that name.
func (st *s2State) address(t *testing.T, face string) (string, int64) {
	t.Helper()
	hostPort, err := st.manifest.Address(st.host, face)
	if err != nil {
		t.Fatalf("the fixture has no %s face: %v", face, err)
	}
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		t.Fatalf("the fixture address for %s is not host:port: %v", face, err)
	}
	number, err := strconv.ParseInt(port, 10, 64)
	if err != nil {
		t.Fatalf("the fixture published no numeric port for %s: %v", face, err)
	}
	return host, number
}

// requireFace proves what one face serves, from the manifest record the fixture wrote:
// the certificate it holds and the TLS level it runs at.
func (st *s2State) requireFace(t *testing.T, face, cert string) {
	t.Helper()
	listener, ok := st.manifest.Listeners[face]
	if !ok {
		t.Fatalf("the fixture manifest has no %s face", face)
	}
	if listener.Cert != cert {
		t.Fatalf("the %s face serves certificate %q, want %q", face, listener.Cert, cert)
	}
	if want := s2fixture.Matrix[face]; listener.TLSLevel != want.TLSLevel {
		t.Fatalf("the %s face runs at TLS level %q, want %q", face, listener.TLSLevel, want.TLSLevel)
	}
}

// faceByCertificate returns the single face the fixture says serves one certificate.
// The refused faces are named this way rather than by a port, so a fixture that moved
// a certificate would fail here instead of silently testing something else.
func (st *s2State) faceByCertificate(t *testing.T, cert string) string {
	t.Helper()
	var found []string
	for _, name := range sortedFaces(st.manifest) {
		if st.manifest.Listeners[name].Cert == cert {
			found = append(found, name)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d faces serve certificate %q (%v), want exactly one", len(found), cert, found)
	}
	return found[0]
}

// faceWithoutTLS returns the one face the fixture says serves no TLS at all.
func (st *s2State) faceWithoutTLS(t *testing.T) string {
	t.Helper()
	level := s2fixture.Matrix[s2fixture.ListenerPlain].TLSLevel
	var found []string
	for _, name := range sortedFaces(st.manifest) {
		if st.manifest.Listeners[name].TLSLevel == level {
			found = append(found, name)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d faces serve TLS level %q (%v), want exactly one", len(found), level, found)
	}
	return found[0]
}

// sortedFaces orders the manifest's listener names so a refusal message is stable.
func sortedFaces(manifest s2fixture.Fixture) []string {
	names := make([]string, 0, len(manifest.Listeners))
	for name := range manifest.Listeners {
		names = append(names, name)
	}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}

// body builds one FTP repository body for one face of the fixture. An empty bundle is
// a plain configuration; every FTPS body here pins the fixture's published CA.
func (st *s2State) body(t *testing.T, name, mode, face, bundle string) repositoryWrite {
	t.Helper()
	host, port := st.address(t, face)
	return repositoryWrite{
		Name: name, Kind: store.RepositoryFTP, TLSMode: mode,
		FTP:      &ftpWrite{Host: host, Port: port, BasePath: "/", Username: st.account, CAPem: bundle},
		Password: &st.password,
	}
}

// sessions reads the census records of one face, oldest first.
func (st *s2State) sessions(t *testing.T, face string) []s2fixture.Audit {
	t.Helper()
	records, err := s2fixture.ReadAudit(st.auditPath)
	if err != nil {
		t.Fatalf("read the fixture census: %v", err)
	}
	return s2fixture.SessionsFor(records, face)
}

// awaitNewSessions waits until the tap has published every session this case caused
// since before, and returns exactly those new records. Reading the whole census
// instead would let a session from an earlier case carry a fact this case did not
// establish.
func (st *s2State) awaitNewSessions(t *testing.T, face string, before int) []s2fixture.Audit {
	t.Helper()
	deadline := time.Now().Add(s2CensusWait)
	for {
		sessions := st.sessions(t, face)
		if len(sessions) > before {
			return sessions[before:]
		}
		if time.Now().After(deadline) {
			t.Fatalf("the %s face census reached %d sessions, want more than %d", face, len(sessions), before)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// a3-create stores one explicit and one implicit FTPS repository plus one plain FTP
// repository through the real handler, and requires each to be read back exactly as
// the transport needs it: the CA bundle stored and echoed, credentials protected only
// where TLS protects them, and the plaintext risk flag recorded only for plain.
func (f *fixture) s2Create(t *testing.T, st *s2State) {
	t.Helper()
	st.requireFace(t, s2fixture.ListenerExplicit, s2fixture.LeafGood)
	st.requireFace(t, s2fixture.ListenerImplicit, s2fixture.LeafGood)

	st.explicitStatus, st.explicit = f.createRepository(t, st.body(t, "s2-explicit",
		store.TLSModeExplicit, s2fixture.ListenerExplicit, st.ca))
	st.implicitStatus, st.implicit = f.createRepository(t, st.body(t, "s2-implicit",
		store.TLSModeImplicit, s2fixture.ListenerImplicit, st.ca))
	st.ftps = append(st.ftps, st.explicit, st.implicit)
	for _, entry := range []struct {
		role string
		repo store.Repository
		want string
	}{{"explicit", st.explicit, store.TLSModeExplicit}, {"implicit", st.implicit, store.TLSModeImplicit}} {
		f.requireFTPSStored(t, st, entry.role, entry.repo, entry.want)
	}

	plainFace := st.faceWithoutTLS(t)
	st.plainStatus, st.plain = f.createRepository(t, st.body(t, "s2-plain",
		store.TLSModePlain, plainFace, ""))
	if st.plain.Kind != store.RepositoryFTP || st.plain.TLSMode != store.TLSModePlain {
		t.Fatalf("the plain repository reads back as %+v", st.plain)
	}
	if !st.plain.PlaintextCredentials {
		t.Fatal("the plain FTP repository did not report plaintext_credentials=true")
	}
	if st.plain.FTP == nil || st.plain.FTP.CAPem != "" {
		t.Fatalf("the plain FTP repository carries a trust bundle: %+v", st.plain.FTP)
	}
	if !strings.Contains(f.auditDetails(t, "repositories.create", st.plain.ID), s2PlaintextDetail) {
		t.Fatalf("the plain create recorded no plaintext_ftp flag: %s",
			f.auditDetails(t, "repositories.create", st.plain.ID))
	}
	if count := f.audit(t, "repositories.create", "success"); count != len(st.ftps)+1 {
		t.Fatalf("repositories.create success audits = %d, want %d", count, len(st.ftps)+1)
	}
	// A created repository is not probed here: the probe sessions belong to a3-probe,
	// which reads the census back against exactly those sessions.
	t.Logf(s2MarkerCreate, st.explicitStatus, st.implicitStatus, st.plainStatus,
		st.explicit.PlaintextCredentials, st.implicit.PlaintextCredentials, st.plain.PlaintextCredentials)
}

// requireFTPSStored judges one stored FTPS repository from the read-back response, the
// served GET body and the persisted row: protected credentials, the public bundle
// echoed byte-for-byte, and the secret kept out of the configuration.
func (f *fixture) requireFTPSStored(t *testing.T, st *s2State, role string, repo store.Repository, wantMode string) {
	t.Helper()
	requireFTPSResponse(t, st, role, repo, wantMode)
	served := f.requireFTPSServed(t, st, role, repo, wantMode)
	stored := f.requireFTPSRow(t, st, role, repo.ID)
	t.Logf("S2-A3 stored role=%s tls_mode=%s plaintext_credentials=%t ca_pem_bytes=%d stored=ca_pem probe_face_port=%d",
		role, served.TLSMode, served.PlaintextCredentials, len(stored.CAPem), served.FTP.Port)
}

// requireFTPSResponse judges the repository the create answered with.
func requireFTPSResponse(t *testing.T, st *s2State, role string, repo store.Repository, wantMode string) {
	t.Helper()
	if repo.Kind != store.RepositoryFTP || repo.TLSMode != wantMode {
		t.Fatalf("the %s repository reads back as %+v, want ftp/%s", role, repo, wantMode)
	}
	if repo.PlaintextCredentials {
		t.Fatalf("the %s FTPS repository reported plaintext_credentials=true", role)
	}
	if !repo.HasSecret {
		t.Fatalf("the %s FTPS repository stored no credential", role)
	}
	if repo.FTP == nil || repo.FTP.CAPem != st.ca {
		t.Fatalf("the %s FTPS repository did not echo the published trust bundle back", role)
	}
}

// requireFTPSServed judges the GET body: the transport, the bundle and the address of
// the face the role names, as the manifest published it.
func (f *fixture) requireFTPSServed(t *testing.T, st *s2State, role string, repo store.Repository, wantMode string) store.Repository {
	t.Helper()
	served := f.readRepository(t, repo.ID)
	if served.TLSMode != wantMode || served.PlaintextCredentials || served.FTP == nil || served.FTP.CAPem != st.ca {
		t.Fatalf("the %s FTPS repository reads back over GET as %+v", role, served)
	}
	host, port := st.address(t, f.faceOf(t, role, repo))
	if served.FTP.Host != host || served.FTP.Port != port || served.FTP.Username != st.account {
		t.Fatalf("the %s FTPS repository configuration = %s:%d %q, want %s:%d %q",
			role, served.FTP.Host, served.FTP.Port, served.FTP.Username, host, port, st.account)
	}
	return served
}

// requireFTPSRow judges the persisted row: the bundle in the configuration column, no
// credential beside it, and no plaintext risk flag in the create audit.
func (f *fixture) requireFTPSRow(t *testing.T, st *s2State, role, id string) store.FTPRepositoryConfig {
	t.Helper()
	stored := f.storedConfig(t, id)
	config := decode[store.FTPRepositoryConfig](t, []byte(stored))
	if config.CAPem != st.ca {
		t.Fatalf("the %s FTPS row stores ca_pem of %d bytes, want the published bundle", role, len(config.CAPem))
	}
	if strings.Contains(stored, st.password) {
		t.Fatalf("the %s FTPS row stores the account credential in its configuration", role)
	}
	if detail := f.auditDetails(t, "repositories.create", id); strings.Contains(detail, s2PlaintextDetail) {
		t.Fatalf("the %s FTPS create recorded the plaintext risk flag: %s", role, detail)
	}
	return config
}

// faceOf maps a created repository back to the fixture face it addresses, so the
// read-back address is checked against the manifest rather than a constant.
func (f *fixture) faceOf(t *testing.T, role string, repo store.Repository) string {
	t.Helper()
	switch role {
	case "explicit":
		return s2fixture.ListenerExplicit
	case "implicit":
		return s2fixture.ListenerImplicit
	}
	t.Fatalf("no fixture face for repository role %q", role)
	return ""
}

// a3-reject pins the frozen 422 rules of a transport configuration. Each body is valid
// except for the one violation under test, so a refusal cannot be attributed to an
// unrelated field, and each refusal is read back: no row, no success audit, and for the
// immutability rule an unchanged stored repository.
func (f *fixture) s2Reject(t *testing.T, st *s2State) {
	t.Helper()
	if st.explicit.ID == "" || st.implicit.ID == "" || st.plain.ID == "" {
		t.Fatal("the create case did not establish the FTPS and plain repositories")
	}
	before := f.audit(t, "repositories.create", "success")

	unparsable := st.body(t, "s2-reject-unparsable", store.TLSModeExplicit, s2fixture.ListenerExplicit, st.ca)
	unparsable.FTP.CAPem = "not a certificate"
	unparsableStatus := f.rejectRepository(t, unparsable.Name, unparsable)

	withCAOnPlain := st.body(t, "s2-reject-plain-ca", store.TLSModePlain, st.faceWithoutTLS(t), "")
	withCAOnPlain.FTP.CAPem = st.ca
	plainWithCAStatus := f.rejectRepository(t, withCAOnPlain.Name, withCAOnPlain)

	// A bundle larger than the transport's own limit. ftp.MaxCAPemBytes is the size the
	// handler quotes in its rule, so this body is refused by that rule and nothing else.
	oversized := st.body(t, "s2-reject-oversized-ca", store.TLSModeExplicit, s2fixture.ListenerExplicit, st.ca)
	oversized.FTP.CAPem = strings.Repeat("a", ftp.MaxCAPemBytes+1)
	oversizedStatus := f.rejectRepository(t, oversized.Name, oversized)

	unknownMode := st.body(t, "s2-reject-unknown-mode", store.TLSModeExplicit, s2fixture.ListenerExplicit, st.ca)
	unknownMode.TLSMode = "starttls"
	unknownModeStatus := f.rejectRepository(t, unknownMode.Name, unknownMode)

	changed := st.body(t, "s2-explicit", store.TLSModeImplicit, s2fixture.ListenerExplicit, st.ca)
	modeChangeStatus := f.rejectUpdate(t, st.explicit.ID, changed)
	after := f.readRepository(t, st.explicit.ID)
	if after.TLSMode != store.TLSModeExplicit || after.PlaintextCredentials ||
		after.FTP == nil || after.FTP.CAPem != st.ca || after.UpdatedAt != st.explicit.UpdatedAt {
		t.Fatalf("the refused mode change left the stored repository as %+v", after)
	}
	f.requireRejectedLeftNoTrace(t, before, unparsable.Name, withCAOnPlain.Name, oversized.Name, unknownMode.Name)
	t.Logf(s2MarkerReject, unparsableStatus, plainWithCAStatus, oversizedStatus, unknownModeStatus, modeChangeStatus)
}

// requireRejectedLeftNoTrace demands that no rejected body reached the database and
// that no create or update success audit was recorded for them; before is the create
// success count taken ahead of the rejected posts.
func (f *fixture) requireRejectedLeftNoTrace(t *testing.T, before int, names ...string) {
	t.Helper()
	for _, name := range names {
		if id, found := f.repositoryByName(t, name); found {
			t.Fatalf("the rejected repository %s exists as %s", name, id)
		}
	}
	if count := f.audit(t, "repositories.create", "success"); count != before {
		t.Fatalf("repositories.create success audits rose from %d to %d for rejected bodies", before, count)
	}
	if count := f.audit(t, "repositories.update", "success"); count != 0 {
		t.Fatalf("repositories.update success audits = %d, want 0 after a refused mode change", count)
	}
}

// rejectRepository posts one body that must be refused as an unsafe shape and returns
// the observed status.
func (f *fixture) rejectRepository(t *testing.T, name string, body repositoryWrite) int {
	t.Helper()
	reply := f.admin.request(t, http.MethodPost, "/api/v1/repositories", marshal(t, body), 0)
	f.requireRefused(t, "POST /api/v1/repositories "+name, reply.status, reply.body)
	return reply.status
}

// rejectUpdate puts one body over an existing repository and returns the status.
func (f *fixture) rejectUpdate(t *testing.T, id string, body repositoryWrite) int {
	t.Helper()
	reply := f.admin.request(t, http.MethodPut, "/api/v1/repositories/"+id, marshal(t, body), 0)
	f.requireRefused(t, "PUT /api/v1/repositories/"+id, reply.status, reply.body)
	return reply.status
}

// requireRefused demands the frozen 422 shape refusal and names the transport rule that
// owns it. A larger body is refused earlier by the request-size limit, which is a
// different rule with a different code: this case is about a bundle the handler's own
// ftp.MaxCAPemBytes rule rejects, so only that refusal counts.
func (f *fixture) requireRefused(t *testing.T, what string, status int, body []byte) {
	t.Helper()
	if status == http.StatusUnprocessableEntity {
		envelope := decode[errorEnvelope](t, body)
		if envelope.Error.Code != render.ValidationFailed {
			t.Fatalf("%s answered %d with code %q, want %q", what, status, envelope.Error.Code, render.ValidationFailed)
		}
		return
	}
	t.Fatalf("%s = %d, want %d (%s): %s", what, status, http.StatusUnprocessableEntity,
		"a repository body carrying an unusable transport configuration is a 422 validation_failed refusal; "+
			"any other code means the request never reached the handler's ca_pem and tls_mode rules", body)
}

// a3-probe drives the real transport through the real handler. The two FTPS faces
// answer ok, the four unsafe configurations answer their locked refusal code, and the
// fixture's own census decides what reached the server: the tap sits in front of the
// daemon and only ever sees the cleartext part of a control channel, so a credential
// count of zero on an FTPS face is the encryption proof. The plain face is the oracle
// that makes those zeros mean something: the same census records that session carrying
// the credential in cleartext.
func (f *fixture) s2Probe(t *testing.T, st *s2State) {
	t.Helper()
	if st.explicit.ID == "" || st.implicit.ID == "" || st.plain.ID == "" {
		t.Fatal("the create case did not establish the FTPS and plain repositories")
	}
	oks := f.requireProtectedFaces(t, st)
	f.requireCensusLive(t, st)

	codes := map[string]string{}
	for _, entry := range []struct {
		label, name, mode, want string
		// cert names the certificate the refused face must serve; the empty name means
		// the face that serves no TLS at all.
		cert string
	}{
		{"untrusted", "s2-probe-untrusted", store.TLSModeExplicit, s2KindUntrusted, s2fixture.LeafRogue},
		{"hostname", "s2-probe-hostname", store.TLSModeExplicit, s2KindHostname, s2fixture.LeafWrongHost},
		{"expired", "s2-probe-expired", store.TLSModeExplicit, s2KindExpired, s2fixture.LeafExpired},
		{"plain_only", "s2-probe-plain-only", store.TLSModeExplicit, s2KindNegotiation, ""},
	} {
		face := st.faceWithoutTLS(t)
		if entry.cert != "" {
			face = st.faceByCertificate(t, entry.cert)
			st.requireFace(t, face, entry.cert)
		}
		codes[entry.label] = f.refusedProbe(t, st, entry.name, entry.mode, face, st.ca, entry.want)
	}
	// The same face that succeeds with the fixture CA is refused when the pinned bundle
	// did not issue its certificate. This is what attributes the untrusted refusal to
	// the trust anchor the operator supplied rather than to the server or the network.
	bundleFace := s2fixture.ListenerExplicit
	st.requireFace(t, bundleFace, s2fixture.LeafGood)
	untrustedByBundle := f.refusedProbe(t, st, "s2-probe-rogue-bundle", store.TLSModeExplicit,
		bundleFace, st.rogueCA, s2KindUntrusted)
	t.Logf("S2-A3 bundle_attribution face=%s pinned=rogue-ca code=%s credential_bytes=0", bundleFace, untrustedByBundle)

	plaintextRecorded := false
	for _, repo := range st.ftps {
		if strings.Contains(f.auditDetails(t, "repositories.create", repo.ID), s2PlaintextDetail) {
			plaintextRecorded = true
		}
	}
	if plaintextRecorded {
		t.Fatal("an FTPS create recorded the plaintext risk flag")
	}
	if !strings.Contains(f.auditDetails(t, "repositories.create", st.plain.ID), s2PlaintextDetail) {
		t.Fatal("the plain create recorded no plaintext risk flag, so the FTPS sweep is vacuous")
	}
	assertNoSecret(t, []byte(f.received.String()+"\n"+f.logs.String()+"\n"+f.auditText(t)), []string{st.password})
	t.Logf(s2MarkerProbe, strings.Join(oks, ","), codes["untrusted"], codes["hostname"],
		codes["expired"], codes["plain_only"], plaintextRecorded)
}

// requireProtectedFaces probes the two FTPS faces and requires the census to show that
// each session really reached the fixture carrying encrypted bytes and no credential in
// the clear. The explicit face must show its AUTH TLS upgrade in cleartext, because
// that command is the one thing an FTPS control channel says before encryption.
func (f *fixture) requireProtectedFaces(t *testing.T, st *s2State) []string {
	t.Helper()
	oks := make([]string, 0, 2)
	for _, entry := range []struct {
		role, face string
		repo       store.Repository
	}{
		{"explicit", s2fixture.ListenerExplicit, st.explicit},
		{"implicit", s2fixture.ListenerImplicit, st.implicit},
	} {
		before := len(st.sessions(t, entry.face))
		f.requireProbeOK(t, entry.role, entry.repo.ID)
		sessions := st.awaitNewSessions(t, entry.face, before)
		for _, session := range sessions {
			counts := f.requireNoCredential(t, session, entry.face)
			// Only explicit FTPS says anything in the clear: an implicit session is TLS
			// from its first byte, so its census can never show the AUTH TLS upgrade.
			if entry.face == s2fixture.ListenerExplicit && counts.authTLS < 1 {
				t.Fatalf("the %s face census recorded no AUTH TLS command, so the FTPS session was never seen", entry.face)
			}
		}
		oks = append(oks, entry.role)
		t.Logf("S2-A3 probe_ok role=%s face=%s sessions=%d auth_tls=%d to_server_bytes=%d credential_bytes=0",
			entry.role, entry.face, len(sessions), sessions[len(sessions)-1].MustCount(t,
				s2fixture.ToServer, s2fixture.NeedleAuthTLS), sessions[len(sessions)-1].ToServerBytes)
	}
	return oks
}

// requireCensusLive proves the plain face is the vacuity oracle: the same tap census
// that reports zero credential bytes for every FTPS session must report the credential
// in the clear for the plaintext repository this run created. Without it a census that
// recorded nothing at all would look like encryption.
func (f *fixture) requireCensusLive(t *testing.T, st *s2State) {
	t.Helper()
	face := st.faceWithoutTLS(t)
	if host, _ := st.address(t, face); st.plain.FTP.Host != host {
		t.Fatalf("the plain repository addresses %s, not the %s face at %s", st.plain.FTP.Host, face, host)
	}
	before := len(st.sessions(t, face))
	f.requireProbeOK(t, "plain", st.plain.ID)
	user, pass, secret := 0, 0, 0
	sessions := st.awaitNewSessions(t, face, before)
	for _, session := range sessions {
		if session.Truncated {
			t.Fatalf("the %s face census is truncated and cannot prove anything", face)
		}
		user += session.MustCount(t, s2fixture.ToServer, s2fixture.NeedleUser)
		pass += session.MustCount(t, s2fixture.ToServer, s2fixture.NeedlePass)
		secret += session.MustCount(t, s2fixture.ToServer, s2fixture.NeedleSecret)
	}
	if user < 1 || pass < 1 || secret < 1 {
		t.Fatalf("the plaintext face census recorded user=%d pass=%d secret=%d over %d sessions: the oracle is dead",
			user, pass, secret, len(sessions))
	}
	t.Logf("S2-A3 census control=plain face=%s sessions=%d user=%d pass=%d secret=%d plaintext_credentials=true",
		face, len(sessions), user, pass, secret)
}

// refusedProbe stores one configuration the transport must refuse, probes it through
// the handler and requires the locked refusal code plus a census that reached the
// fixture without a single credential byte.
func (f *fixture) refusedProbe(t *testing.T, st *s2State, name, mode, face, bundle, want string) string {
	t.Helper()
	status, repo := f.createRepository(t, st.body(t, name, mode, face, bundle))
	if status != http.StatusCreated {
		t.Fatalf("the refused configuration %s was not stored: %d", name, status)
	}
	if repo.PlaintextCredentials {
		t.Fatalf("the %s FTPS configuration reported plaintext_credentials=true", name)
	}
	st.ftps = append(st.ftps, repo)
	before := len(st.sessions(t, face))
	probe := decode[probeReply](t, f.admin.request(t, http.MethodPost,
		"/api/v1/repositories/"+repo.ID+"/probe", "", http.StatusOK).body)
	if probe.OK || probe.Error == nil {
		t.Fatalf("the %s face accepted a repository it must refuse: %+v", face, probe)
	}
	if probe.Error.Code != want {
		t.Fatalf("the %s face refused with code %q, want %s", face, probe.Error.Code, want)
	}
	sessions := st.awaitNewSessions(t, face, before)
	for _, session := range sessions {
		counts := f.requireNoCredential(t, session, face)
		if counts.authTLS < 1 {
			t.Fatalf("the %s face census recorded no AUTH TLS command: the refusal never reached the fixture", face)
		}
	}
	f.requireProbeAudit(t, repo.ID)
	t.Logf("S2-A3 probe_refused repository=%s face=%s tls_mode=%s code=%s reply_code=%d sessions=%d credential_bytes=0",
		name, face, mode, probe.Error.Code, probe.Error.ReplyCode, len(sessions))
	return probe.Error.Code
}

// requireProbeOK probes one repository and demands the transport accepted it.
func (f *fixture) requireProbeOK(t *testing.T, role, id string) {
	t.Helper()
	reply := f.admin.request(t, http.MethodPost, "/api/v1/repositories/"+id+"/probe", "", http.StatusOK)
	probe := decode[probeReply](t, reply.body)
	if !probe.OK {
		t.Fatalf("the %s repository probe = %+v", role, probe)
	}
}

// censusCounts is what one census record said about the credential.
type censusCounts struct {
	user, pass, secret, authTLS int
	bytes                       int64
}

// requireNoCredential refuses a record that cannot prove absence and then demands the
// session reached the fixture with bytes on the wire while carrying no credential in
// the clear. A truncated census is refused outright: absence proved by a record that
// stopped searching proves nothing.
func (f *fixture) requireNoCredential(t *testing.T, session s2fixture.Audit, face string) censusCounts {
	t.Helper()
	counts := censusCounts{
		user:    session.MustCount(t, s2fixture.ToServer, s2fixture.NeedleUser),
		pass:    session.MustCount(t, s2fixture.ToServer, s2fixture.NeedlePass),
		secret:  session.MustCount(t, s2fixture.ToServer, s2fixture.NeedleSecret),
		authTLS: session.MustCount(t, s2fixture.ToServer, s2fixture.NeedleAuthTLS),
		bytes:   session.ToServerBytes,
	}
	if session.Truncated {
		t.Fatalf("the %s face census is truncated and cannot prove absence: %+v", face, session)
	}
	if counts.bytes == 0 {
		t.Fatalf("the %s face recorded a session that received no bytes at all", face)
	}
	if counts.user != 0 || counts.pass != 0 || counts.secret != 0 {
		t.Fatalf("the %s face accepted user=%d pass=%d secret=%d in cleartext", face, counts.user, counts.pass, counts.secret)
	}
	return counts
}

// requireProbeAudit demands the refused probe left its own failed audit row, so the
// refusal is visible to an operator and not only to this worker.
func (f *fixture) requireProbeAudit(t *testing.T, id string) {
	t.Helper()
	var result string
	query := `SELECT result FROM audit_logs WHERE action = 'repositories.probe' AND resource_id = ? ORDER BY id DESC LIMIT 1`
	if err := f.analysis.QueryRowContext(t.Context(), query, id).Scan(&result); err != nil {
		t.Fatalf("the refused probe of %s left no audit row: %v", id, err)
	}
	if result != "failed" {
		t.Fatalf("the probe audit of %s reads %q, want failed", id, result)
	}
}

// createRepository posts one body and returns the observed status with the stored
// repository it served.
func (f *fixture) createRepository(t *testing.T, body repositoryWrite) (int, store.Repository) {
	t.Helper()
	reply := f.admin.request(t, http.MethodPost, "/api/v1/repositories", marshal(t, body), 0)
	if reply.status != http.StatusCreated {
		t.Fatalf("POST /api/v1/repositories %s = %d, want %d: %s",
			body.Name, reply.status, http.StatusCreated, reply.body)
	}
	return reply.status, decode[store.Repository](t, reply.body)
}

// readRepository serves GET for one repository id.
func (f *fixture) readRepository(t *testing.T, id string) store.Repository {
	t.Helper()
	return decode[store.Repository](t, f.admin.request(t, http.MethodGet, "/api/v1/repositories/"+id, "",
		http.StatusOK).body)
}

// storedConfig reads the persisted configuration column, which is where a public CA
// bundle belongs and where a credential must never appear.
func (f *fixture) storedConfig(t *testing.T, id string) string {
	t.Helper()
	var stored string
	if err := f.analysis.QueryRowContext(t.Context(),
		`SELECT config_json FROM repositories WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	return stored
}

// repositoryByName reports whether one repository name reached the database.
func (f *fixture) repositoryByName(t *testing.T, name string) (string, bool) {
	t.Helper()
	var id string
	err := f.analysis.QueryRowContext(t.Context(), `SELECT id FROM repositories WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return id, true
}
