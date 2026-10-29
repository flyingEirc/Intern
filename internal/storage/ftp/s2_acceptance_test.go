//go:build integration

package ftp

// This worker runs as uid 1000 on the FTPS fixture's private network, beside the
// server's own filesystem volume and the audit volume its taps write. Everything it
// reports is read back from one of four oracles: a real TLS handshake against a real
// Pure-FTPd, the bytes that actually crossed its sockets, the server's own
// account of each session, or the published object on the server's disk.
//
// The fixture serves six faces: explicit TLS with the server enforcing TLS on both
// channels, implicit TLS (a second compile of the same pinned source with
// IMPLICIT_TLS), a face with TLS disabled entirely, and three faces that differ only
// in the certificate they present - wrong hostname, expired, and a chain no pinned
// bundle trusts. A refusal is therefore a statement about one face, not about a fake
// server assembled in-process.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/internal/storage/ftp/s2fixture"
)

const (
	s2EnvHost    = "INTERN_S2_HOST"
	s2EnvAccount = "INTERN_S2_ACCOUNT"
	s2EnvAudit   = "INTERN_S2_AUDIT"
	s2EnvOracle  = "INTERN_S2_ORACLE"
	s2EnvMarker  = "INTERN_S2_MARKER"
)

// s2Worker is one fixture the worker is allowed to trust: the manifest it published
// about itself, the trust bundle it republished, the tap log it wrote and the disk it
// stores on. A missing piece is a refusal, never a reduced case set.
type s2Worker struct {
	host, account, password, marker string
	manifest                        s2fixture.Fixture
	auditPath, caPath, oracle       string
}

func TestS2FTPSWorker(t *testing.T) {
	if os.Getuid() == 0 {
		t.Fatal("the FTPS worker must not run as root")
	}
	worker := s2RequireWorker(t)
	cases := []struct {
		name string
		run  func(*testing.T, *s2Worker)
	}{
		{"explicit_publish", s2ExplicitPublish},
		{"implicit_publish", s2ImplicitPublish},
		{"data_protection", s2DataProtection},
		{"session_reuse", s2SessionReuse},
		{"credential_census", s2CredentialCensus},
		{"refusal", s2Refusals},
		{"no_downgrade", s2NoDowngrade},
		{"plain_baseline", s2PlainBaseline},
		{"instance_marker", s2InstanceMarker},
	}
	// The cases share one fixture and one audit stream: a case that failed leaves
	// the census of every later case uninterpretable, so the run stops here rather
	// than reporting facts it never observed.
	for _, next := range cases {
		if !t.Run(next.name, func(t *testing.T) { next.run(t, worker) }) {
			t.Fatalf("FTPS case %s failed: the shared fixture census it was to produce is unknown", next.name)
		}
	}
}

func s2RequireWorker(t *testing.T) *s2Worker {
	t.Helper()
	worker := &s2Worker{
		host: os.Getenv(s2EnvHost), account: os.Getenv(s2EnvAccount),
		password: os.Getenv("INTERN_S2_PASSWORD"), marker: os.Getenv(s2EnvMarker),
		auditPath: filepath.Join(os.Getenv(s2EnvAudit), "control.jsonl"),
		caPath:    filepath.Join(os.Getenv(s2EnvAudit), "ca.pem"),
		oracle:    os.Getenv(s2EnvOracle),
	}
	for name, value := range map[string]string{
		s2EnvHost: worker.host, s2EnvAccount: worker.account,
		"INTERN_S2_PASSWORD": worker.password, s2EnvMarker: worker.marker,
		s2EnvAudit: os.Getenv(s2EnvAudit), s2EnvOracle: worker.oracle,
	} {
		if value == "" {
			t.Fatalf("%s is required: this worker only runs against the real FTPS fixture", name)
		}
	}
	if len(worker.password) < 12 || len(worker.marker) < 16 {
		t.Fatal("the FTPS fixture injected a too-short credential or probe marker")
	}
	manifest, err := s2fixture.ReadFixture(filepath.Join(filepath.Dir(worker.auditPath), "fixture.txt"))
	if err != nil {
		t.Fatalf("read the fixture manifest: %v", err)
	}
	if matrixErr := manifest.MatchesMatrix(); matrixErr != nil {
		t.Fatalf("the fixture is not the six-face FTPS matrix: %v", matrixErr)
	}
	ca, err := os.ReadFile(worker.caPath)
	if err != nil || !ValidCAPem(string(ca)) {
		t.Fatalf("the fixture published no usable trust bundle: %v", err)
	}
	worker.manifest = manifest
	return worker
}

// address is the face's address by name. The port always comes from the manifest the
// fixture wrote about itself, never from a constant the worker hoped for.
func (w *s2Worker) address(t *testing.T, face string) string {
	t.Helper()
	address, err := w.manifest.Address(w.host, face)
	if err != nil {
		t.Fatalf("the fixture has no %s face: %v", face, err)
	}
	return address
}

func (w *s2Worker) ca(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(w.caPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func (w *s2Worker) config(t *testing.T, instance, face, mode, ca string) Config {
	t.Helper()
	return Config{
		Address: w.address(t, face), Username: w.account, Password: w.password,
		BasePath: "/", InstanceID: instance, TLSMode: mode, CAPem: ca,
		DialTimeout: 5 * time.Second, IdleTimeout: 20 * time.Second,
	}
}

// s2Observation is what one Repository's sockets really did: the bytes that crossed
// them and the handshake every TLS channel completed.
type s2Observation struct {
	mu       sync.Mutex
	opens    []string
	channels []s2Channel
	wire     []*s2Stream
	partial  bool
}

type s2Channel struct {
	remote      string
	version     uint16
	suite       uint16
	resumed     bool
	fingerprint string
	dns         string
	notAfter    time.Time
}

type s2Stream struct {
	address string
	bytes   int64
	plain   bytes.Buffer
}

func (o *s2Observation) opened(address string) *s2Stream {
	o.mu.Lock()
	defer o.mu.Unlock()
	stream := &s2Stream{address: address}
	o.opens = append(o.opens, address)
	o.wire = append(o.wire, stream)
	return stream
}

// verified records a completed handshake. Channels are paired with sockets in the
// order they were dialled: an FTPS data channel is dialled and handed over only after
// its handshake finished, and the control channel's handshake is triggered by the
// first command that must not be seen in cleartext, so both are settled before the
// next socket exists.
func (o *s2Observation) verified(cs tls.ConnectionState) {
	o.mu.Lock()
	defer o.mu.Unlock()
	channel := s2Channel{version: cs.Version, suite: cs.CipherSuite, resumed: cs.DidResume}
	if len(cs.PeerCertificates) > 0 {
		leaf := cs.PeerCertificates[0]
		sum := sha256.Sum256(leaf.Raw)
		channel.fingerprint = hex.EncodeToString(sum[:])
		channel.dns = strings.Join(leaf.DNSNames, ",")
		channel.notAfter = leaf.NotAfter
	}
	if len(o.opens) != len(o.channels)+1 {
		o.partial = true
		return
	}
	channel.remote = o.opens[len(o.channels)]
	o.channels = append(o.channels, channel)
}

// leaf returns the first observed handshake that carried a full chain. A resumed
// handshake presents no peer certificates at all, which is why chain facts are read
// from a full handshake and never asserted across every channel.
func (o *s2Observation) leaf() (s2Channel, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, channel := range o.channels {
		if channel.fingerprint != "" {
			return channel, true
		}
	}
	return s2Channel{}, false
}

func (o *s2Observation) census() (wire int64, marker, secret int, channels int, resumed int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, stream := range o.wire {
		wire += stream.bytes
		raw := stream.plain.Bytes()
		marker += bytes.Count(raw, []byte("s2probe-"+s2WorkerMarker()))
		secret += bytes.Count(raw, []byte(os.Getenv("INTERN_S2_PASSWORD")))
	}
	for _, channel := range o.channels {
		channels++
		if channel.resumed {
			resumed++
		}
	}
	return wire, marker, secret, channels, resumed
}

// s2WorkerMarker is kept as a function so the census searches the exact generated
// probe string rather than a literal a passing run could invent.
func s2WorkerMarker() string { return os.Getenv(s2EnvMarker) }

// observe wraps a Repository's dial seam: the bytes it returns are what the server
// receives, before any TLS layer, so a cleartext payload cannot hide.
func s2Instrument(t *testing.T, repository *Repository) *s2Observation {
	t.Helper()
	observation := &s2Observation{}
	dial := repository.dialContext
	repository.dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &s2Tap{Conn: conn, stream: observation.opened(address)}, nil
	}
	if repository.tlsConfig != nil {
		// The handshakes that follow all use this one config, which is exactly the
		// claim under test: one shared identity and one shared session cache for the
		// control channel and every data channel of the repository.
		repository.tlsConfig.VerifyConnection = func(cs tls.ConnectionState) error {
			observation.verified(cs)
			return nil
		}
	}
	t.Cleanup(func() {
		wire, marker, secret, channels, resumed := observation.census()
		t.Logf("S2-A1 census face_channels=%d resumed=%d wire_bytes=%d cleartext_marker=%d cleartext_secret=%d pairing_exact=%t",
			channels, resumed, wire, marker, secret, !observation.partial)
		if observation.partial {
			t.Error("socket opens and TLS handshakes did not pair one for one")
		}
		if channels > 0 && resumed == 0 {
			t.Error("no FTPS channel resumed the shared session")
		}
		if marker != 0 || secret != 0 {
			t.Errorf("cleartext census found marker=%d secret=%d on an FTPS run", marker, secret)
		}
	})
	return observation
}

type s2Tap struct {
	net.Conn
	stream *s2Stream
}

func (t *s2Tap) Read(p []byte) (int, error) {
	n, err := t.Conn.Read(p)
	t.stream.record(p[:n])
	return n, err
}

func (t *s2Tap) Write(p []byte) (int, error) {
	n, err := t.Conn.Write(p)
	t.stream.record(p[:n])
	return n, err
}

func (s *s2Stream) record(p []byte) {
	if len(p) == 0 {
		return
	}
	s.bytes += int64(len(p))
	if s.plain.Len() < 1<<20 {
		_, _ = s.plain.Write(p)
	}
}

// s2Payload carries the generated probe marker inside the bytes that travel over the
// data channel, so the cleartext census has something specific to look for.
func (w *s2Worker) payload(size int) []byte {
	head := []byte("s2probe-" + w.marker + "\n")
	body := bytes.Repeat([]byte("real-ftps-immutable-payload\x00"), size)
	return append(head, body...)
}

func (w *s2Worker) publish(t *testing.T, instance, face, mode string) (string, []byte, *s2Observation) {
	t.Helper()
	key := instance + "/payload.bin"
	var ca string
	if mode != TLSModePlain {
		ca = w.ca(t)
	}
	repository, err := Open(t.Context(), w.config(t, instance, face, mode, ca))
	if err != nil {
		s2Fatal(t, err)
	}
	observation := s2Instrument(t, repository)
	// The tap records a session only when its control connection closes, so the
	// session is closed here, before the caller waits for its census; the cleanup
	// only covers a case that failed before reaching that point.
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = repository.Close()
		}
	})
	data := w.payload(4096)
	sink, err := repository.Create(t.Context(), key)
	s2Require(t, err)
	s1Write(t, sink, data)
	receipt, err := sink.Commit(t.Context())
	s2Require(t, err)
	if receipt.Path != key || receipt.Size != int64(len(data)) {
		t.Fatalf("FTPS receipt differs: %+v want %s/%d", receipt, key, len(data))
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	size, sizeErr := repository.Size(t.Context(), key)
	if sizeErr != nil || size != receipt.Size {
		t.Fatalf("FTPS SIZE = %d (%v), want %d", size, sizeErr, receipt.Size)
	}
	body, err := repository.Open(t.Context(), key, 0)
	s2Require(t, err)
	got, err := io.ReadAll(body)
	s2Require(t, errors.Join(err, body.Close()))
	if !bytes.Equal(got, data) {
		t.Fatal("FTPS read-back differs from the published bytes")
	}
	if onDisk := w.disk(t, key); fmt.Sprintf("%x", sha256.Sum256(onDisk)) != digest {
		t.Fatal("the server's own filesystem does not hold the published bytes")
	}
	s2Require(t, repository.Close())
	closed = true
	return key, data, observation
}

// disk reads the object through the server's filesystem volume, which is an
// independent oracle: it is what the daemon wrote, not what the client was told.
func (w *s2Worker) disk(t *testing.T, key string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(w.oracle, w.account, filepath.FromSlash(key)))
	if err != nil {
		t.Fatalf("read the server filesystem for %s: %v", key, err)
	}
	return raw
}

func (w *s2Worker) sessions(t *testing.T, face string) []s2fixture.Audit {
	t.Helper()
	records, err := s2fixture.ReadAudit(w.auditPath)
	if err != nil {
		t.Fatalf("read the fixture audit: %v", err)
	}
	return s2fixture.SessionsFor(records, face)
}

// awaitSessions waits for the tap to finish at least one session after the baseline
// and returns exactly the sessions recorded since it. A tap writes its record when the
// connection closes, so an assertion made immediately after a client-side failure
// would race the fixture instead of judging it, and a slice that reached back past
// the baseline would credit this case with an earlier case's session.
func (w *s2Worker) awaitSessions(t *testing.T, face string, before int) []s2fixture.Audit {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		sessions := w.sessions(t, face)
		if len(sessions) > before {
			return sessions[before:]
		}
		if time.Now().After(deadline) {
			t.Fatalf("the %s face audit reached %d sessions, want more than %d", face, len(sessions), before)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// requireSilent demands a session that received bytes but never a credential, and
// returns the counts it recorded. A truncated census is refused: absence proved by a
// record that stopped searching proves nothing.
func s2RequireSilent(t *testing.T, session s2fixture.Audit, face string) (user, pass, secret int) {
	t.Helper()
	if session.Truncated {
		t.Fatalf("the %s face census was truncated and cannot prove absence", face)
	}
	if session.ToServerBytes == 0 {
		t.Fatalf("the %s face recorded a session that received no bytes at all", face)
	}
	user = session.MustCount(t, s2fixture.ToServer, s2fixture.NeedleUser)
	pass = session.MustCount(t, s2fixture.ToServer, s2fixture.NeedlePass)
	secret = session.MustCount(t, s2fixture.ToServer, s2fixture.NeedleSecret)
	if user != 0 || pass != 0 || secret != 0 {
		t.Fatalf("the %s face accepted user=%d pass=%d secret=%d in cleartext", face, user, pass, secret)
	}
	return user, pass, secret
}

func s2Require(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		s2Fatal(t, err)
	}
}

func s2Fatal(t *testing.T, err error) {
	t.Helper()
	var operation *Error
	if errors.As(err, &operation) {
		t.Fatalf("%v (operation=%s kind=%s reply_code=%d)", err, operation.Op, operation.Kind, operation.ReplyCode)
	}
	t.Fatal(err)
}

// s2Kind returns the classified transport refusal, insisting it is a TLS one and that
// the account secret never reached the reported error chain.
func s2Kind(t *testing.T, err error, password string) (kind string, reply int) {
	t.Helper()
	if err == nil {
		t.Fatal("a session completed where the transport had to be refused")
	}
	var operation *Error
	if !errors.As(err, &operation) {
		t.Fatalf("refusal arrived unclassified: %v", err)
	}
	if !strings.HasPrefix(operation.Kind, "tls_") || !errors.Is(err, ErrTLS) {
		t.Fatalf("refusal is not a TLS refusal: kind=%s err=%v", operation.Kind, err)
	}
	if strings.Contains(err.Error(), password) || strings.Contains(operation.Error(), password) {
		t.Fatal("the TLS refusal disclosed the account password")
	}
	return operation.Kind, operation.ReplyCode
}

// s2Identity is the certificate one face really presented, together with the verdict
// an independent verification of it reaches. The library's own strict refusal is
// asserted by every case above; this probe exists because a refused handshake reports
// no identity by design (the package never carries the peer's message into an error),
// and a claim about a wrong-host, expired or untrusted leaf has to name the
// certificate it refused. InsecureSkipVerify here defers the decision to the manual
// check below - it never makes the fixture trusted - and the returned conn is closed
// immediately, so no FTP byte is ever exchanged on this dial.
func (w *s2Worker) s2Identity(t *testing.T, face string) (fingerprint, dns string, notAfter time.Time, verdict error) {
	t.Helper()
	address := w.address(t, face)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(w.ca(t))) {
		t.Fatal("the fixture trust bundle is not loadable")
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	var captured []*x509.Certificate
	probe := &tls.Config{
		ServerName:         host,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // verification is performed by hand below, on this dial only
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			captured = captured[:0]
			for _, raw := range rawCerts {
				leaf, parseErr := x509.ParseCertificate(raw)
				if parseErr != nil {
					return parseErr
				}
				captured = append(captured, leaf)
			}
			if len(captured) == 0 {
				return errors.New("the face offered no certificate")
			}
			_, verdict = captured[0].Verify(x509.VerifyOptions{Roots: pool, DNSName: host, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
			return verdict
		},
	}
	dialer := &tls.Dialer{Config: probe, NetDialer: &net.Dialer{Timeout: 5 * time.Second}}
	conn, err := dialer.DialContext(t.Context(), "tcp", address)
	if err != nil {
		return "", "", time.Time{}, err
	}
	defer func() { _ = conn.Close() }()
	if len(captured) == 0 {
		t.Fatalf("the %s face presented no certificate at all", face)
	}
	leaf := captured[0]
	sum := sha256.Sum256(leaf.Raw)
	return hex.EncodeToString(sum[:]), strings.Join(leaf.DNSNames, ","), leaf.NotAfter, verdict
}

// s2VerdictKind names the reason an independent verification refused a presented leaf.
func s2VerdictKind(err error) string {
	var (
		hostname  x509.HostnameError
		authority x509.UnknownAuthorityError
		invalid   x509.CertificateInvalidError
	)
	switch {
	case errors.As(err, &hostname):
		return "tls_hostname"
	case errors.As(err, &invalid) && invalid.Reason == x509.Expired:
		return "tls_expired"
	case errors.As(err, &authority):
		return "tls_untrusted"
	case err == nil:
		return "accepted"
	}
	return "tls_untrusted"
}

// s2Publish is the shared happy path for both FTPS modes: one real publish, then the
// three independent accounts of that session - the fixture's own tap census, the
// bytes the client's TLS channels carried, and the certificate an observing dial sees
// when it dials the same face by hand.
func s2Publish(t *testing.T, worker *s2Worker, face, mode string) {
	t.Helper()
	before := len(worker.sessions(t, face))
	_, data, observation := worker.publish(t, "publish-"+face, face, mode)
	after := worker.awaitSessions(t, face, before)
	for _, session := range after {
		s2RequireSilent(t, session, face)
	}
	wire, marker, secret, channels, resumed := observation.census()
	s2RequireTLSChannels(t, face, observation, wire, channels)
	s2RequireCleartextFree(t, worker, face, data, marker, secret)
	control := s2RequirePresentedIdentity(t, worker, face, observation)
	listener := worker.manifest.Listeners[face]
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	t.Logf("S2-A1 %s_publish listener=%s client=%d tls=%s mode=%s channels=%d control_version=%s data_resumed=%t"+
		" wire_bytes=%d cleartext_marker=0 cleartext_secret=0 leaf=%s dns=%s not_after=%s"+
		" oracle_sha256=%s session_sha256=%s audit_sessions=%d user_per_session=0 pass_per_session=0 secret_per_session=0",
		mode, face, listener.Client, listener.TLSLevel, mode, channels, tlsVersion(control.version),
		resumed > 0, wire, control.fingerprint, control.dns, control.notAfter.UTC().Format(time.RFC3339Nano),
		digest, digest, len(after))
	t.Logf("S2-A4 fixture_identity listeners=%d tls_required=%s advertise_private=%t auth_backend=%s"+
		" client=%s uid=%d marker_sha256=%s ca_sha256=%s",
		len(worker.manifest.Listeners), listener.TLSLevel, privateAddress(worker.manifest.Advertise),
		worker.manifest.AuthBackend, s2ClientVersion(), os.Getuid(), worker.manifest.MarkerSHA256,
		worker.manifest.CAFingerprint)
	if resumed == 0 {
		t.Errorf("no %s channel resumed the shared TLS session", face)
	}
}

// s2RequireTLSChannels demands that the session really negotiated TLS on every channel
// it carried bytes over, and that none of them fell below the TLS 1.2 floor the
// transport is configured with. A zero-byte or zero-channel observation is a session
// that never happened, so it is refused rather than reported as an empty census.
func s2RequireTLSChannels(t *testing.T, face string, observation *s2Observation, wire int64, channels int) {
	t.Helper()
	if channels == 0 || wire == 0 {
		t.Fatal("the FTPS session negotiated no TLS channel or moved no bytes")
	}
	for _, channel := range observation.channels {
		if channel.version < tls.VersionTLS12 {
			t.Fatalf("the %s face carried a channel at %s, below the TLS 1.2 floor", face, tlsVersion(channel.version))
		}
	}
}

// s2RequireCleartextFree demands that the client's own census of the plaintext part of
// the stream found neither the probe marker nor the account password, and that the
// payload it searched for the marker in really carries it - otherwise a zero would
// only mean the needle was never there to find.
func s2RequireCleartextFree(t *testing.T, worker *s2Worker, face string, data []byte, marker, secret int) {
	t.Helper()
	if marker != 0 || secret != 0 {
		t.Fatalf("cleartext census found marker=%d secret=%d on %s", marker, secret, face)
	}
	if len(data) <= len(worker.marker) || !bytes.Contains(data, []byte("s2probe-"+worker.marker)) {
		t.Fatal("the published payload does not carry the probe marker the census searches for")
	}
}

// s2RequirePresentedIdentity compares the leaf the library negotiated with the leaf an
// independent observing dial sees on the same face, and requires the fixture's own CA
// to accept it. The refused faces are the reason this is a two-sided check: a face
// that presents the wrong certificate to the library would otherwise be reported only
// as a client-side failure, with no identity named.
func s2RequirePresentedIdentity(t *testing.T, worker *s2Worker, face string, observation *s2Observation) s2Channel {
	t.Helper()
	control, chained := observation.leaf()
	if !chained {
		t.Fatalf("no %s handshake carried a full chain, so the presented identity is unproven", face)
	}
	fingerprint, dns, notAfter, verdict := worker.s2Identity(t, face)
	if verdict != nil {
		t.Fatalf("the %s face presents a leaf an independent check refuses: %v", face, verdict)
	}
	if fingerprint != control.fingerprint || dns != control.dns || !notAfter.Equal(control.notAfter) {
		t.Fatalf("the %s face presented %s/%s/%s to an observing dial but %s/%s/%s to the library",
			face, fingerprint, dns, notAfter, control.fingerprint, control.dns, control.notAfter)
	}
	return control
}

func s2ExplicitPublish(t *testing.T, worker *s2Worker) {
	s2Publish(t, worker, s2fixture.ListenerExplicit, TLSModeExplicit)
}

func s2ImplicitPublish(t *testing.T, worker *s2Worker) {
	s2Publish(t, worker, s2fixture.ListenerImplicit, TLSModeImplicit)
}

// s2DataProtection proves on the real server that a data channel must be encrypted:
// the server refuses a transfer whose protection level is clear, accepts the same
// transfer under PROT P, and refuses to carry a byte over a plaintext data socket.
func s2DataProtection(t *testing.T, worker *s2Worker) {
	t.Helper()
	session := worker.openRaw(t, s2fixture.ListenerExplicit)
	defer func() { _ = session.Close() }()
	clearCode := worker.requireClearDataRefused(t, session)
	privateCode := worker.requirePrivateDataTransfer(t, session)
	completed, offered := worker.requireNoCleartextData(t, session)
	t.Logf("S2-A1 data_protection prot_clear=%d prot_private=%d plaintext_data=refused plaintext_probe_reply=%d"+
		" cleartext_bytes_offered=%d cleartext_payload_on_server=0",
		clearCode, privateCode, completed, offered)
}

// requireClearDataRefused sets PROT C and demands that the server refuse the transfer
// itself, not merely tolerate it: the STOR is answered by the server's own reply and
// the object never appears on its filesystem. The reply code is returned for the
// marker, which is why a tolerance that answered 200 would fail here rather than at
// the log line.
func (w *s2Worker) requireClearDataRefused(t *testing.T, session *s2RawSession) int {
	t.Helper()
	_, _, commandErr := session.command(t, "PROT C", 200)
	if commandErr != nil {
		t.Fatalf("PROT C: %v", commandErr)
	}
	data := w.dialPassive(t, session, "plaintext data dial after PROT C")
	code, _, err := session.expect(t, "STOR s2/protc-refused.bin", 521)
	_ = data.Close()
	if err != nil {
		t.Fatalf("STOR after PROT C: %v (reply=%d)", err, code)
	}
	if _, statErr := os.Stat(filepath.Join(w.oracle, w.account, "s2", "protc-refused.bin")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("the server accepted a cleartext data transfer: %v", statErr)
	}
	return code
}

// requirePrivateDataTransfer switches the same control session to PROT P and demands
// that the identical transfer complete over an encrypted data socket, verified against
// the server's own filesystem. It returns the completion reply the server gave.
func (w *s2Worker) requirePrivateDataTransfer(t *testing.T, session *s2RawSession) int {
	t.Helper()
	_, _, commandErr := session.command(t, "PROT P", 200)
	if commandErr != nil {
		t.Fatalf("PROT P: %v", commandErr)
	}
	data := w.dialPassive(t, session, "data dial after PROT P")
	secure := tls.Client(data, w.clientConfig(t, s2fixture.ListenerExplicit))
	payload := w.payload(64)
	_, _, storeErr := session.expect(t, "STOR s2/prot-private.bin", 150)
	if storeErr != nil {
		t.Fatalf("STOR under PROT P: %v", storeErr)
	}
	if _, writeErr := secure.Write(payload); writeErr != nil {
		t.Fatalf("encrypted data write: %v", writeErr)
	}
	_ = secure.Close()
	code, _, err := session.expect(t, "", 226)
	if err != nil {
		t.Fatalf("no transfer completion under PROT P: %v", err)
	}
	if !bytes.Equal(w.disk(t, "s2/prot-private.bin"), payload) {
		t.Fatal("the PROT P object differs on the server filesystem")
	}
	return code
}

// requireNoCleartextData offers the payload again, this time on a plaintext socket to
// the channel the server has already declared private. The transfer must not complete,
// and none of the bytes the probe offered may reach the object on disk: a server that
// accepted the socket silently would otherwise pass on the strength of the reply code
// alone.
func (w *s2Worker) requireNoCleartextData(t *testing.T, session *s2RawSession) (reply, offered int) {
	t.Helper()
	payload := w.payload(64)
	data := w.dialPassive(t, session, "plaintext data dial under PROT P")
	written, writeErr := data.Write(payload)
	if writeErr != nil && written == 0 {
		t.Fatalf("the plaintext data probe never reached the server: %v", writeErr)
	}
	_ = data.Close()
	code, _, probeErr := session.expect(t, "", 226)
	if probeErr == nil && code == 226 {
		t.Fatal("a plaintext data socket completed a transfer on a private channel")
	}
	if _, statErr := os.Stat(filepath.Join(w.oracle, w.account, "s2", "prot-private.bin")); statErr == nil {
		if onDisk := w.disk(t, "s2/prot-private.bin"); bytes.Contains(onDisk, []byte("s2probe-"+w.marker)) {
			t.Fatal("cleartext payload bytes reached the server filesystem")
		}
	}
	return code, written
}

// dialPassive opens the data socket for the passive port the server just announced on
// this control session. The label names the probe that is dialing, so a failed dial
// says which channel it could not open.
func (w *s2Worker) dialPassive(t *testing.T, session *s2RawSession, label string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(w.host, strconv.Itoa(session.passivePort(t))), 5*time.Second)
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	return conn
}

// s2SessionReuse proves TLS session reuse for data channels on the real server, both
// through the library (a shared cache across control and data) and as a two-sided
// comparison: the same cache resumes, a fresh configuration for the same server does
// not, so the resume is the cache's work and not a property of the server.
func s2SessionReuse(t *testing.T, worker *s2Worker) {
	t.Helper()
	repository, err := Open(t.Context(), worker.config(t, "reuse", s2fixture.ListenerExplicit, TLSModeExplicit, worker.ca(t)))
	s2Require(t, err)
	observation := s2Instrument(t, repository)
	worker.publishInto(t, repository, "s2/reuse/payload.bin")
	worker.deleteFrom(t, repository, "s2/reuse/payload.bin")
	s2Require(t, repository.Close())
	_, _, _, channels, resumed := observation.census()
	if channels < 3 || resumed < 2 {
		t.Fatalf("library reuse channels=%d resumed=%d, want at least 3 and 2", channels, resumed)
	}
	session := worker.openRaw(t, s2fixture.ListenerExplicit)
	defer func() { _ = session.Close() }()
	if _, _, err := session.command(t, "PROT P", 200); err != nil {
		t.Fatalf("PROT P: %v", err)
	}
	shared := worker.clientConfig(t, s2fixture.ListenerExplicit)
	sharedResumed := worker.resumeAttempt(t, session, shared)
	fresh := worker.clientConfig(t, s2fixture.ListenerExplicit)
	fresh.ClientSessionCache = nil
	freshResumed := worker.resumeAttempt(t, session, fresh)
	if !sharedResumed || freshResumed {
		t.Fatalf("session reuse probe: shared cache resumed=%t, fresh config resumed=%t", sharedResumed, freshResumed)
	}
	version := ""
	if len(observation.channels) > 0 {
		version = tlsVersion(observation.channels[0].version)
	}
	t.Logf("S2-A1 session_reuse library_channels=%d library_resumed=%d probe_shared_resumed=true probe_fresh_resumed=false version=%s",
		channels, resumed, version)
}

// resumeAttempt opens a passive port and handshakes it with one configuration,
// reporting whether the server accepted a resumed session.
func (w *s2Worker) resumeAttempt(t *testing.T, session *s2RawSession, config *tls.Config) bool {
	t.Helper()
	port := session.passivePort(t)
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(w.host, strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		t.Fatalf("data dial for the reuse probe: %v", err)
	}
	defer func() { _ = conn.Close() }()
	secure := tls.Client(conn, config)
	if err := secure.HandshakeContext(t.Context()); err != nil {
		t.Fatalf("data handshake for the reuse probe: %v", err)
	}
	return secure.ConnectionState().DidResume
}

func (w *s2Worker) clientConfig(t *testing.T, face string) *tls.Config {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(w.ca(t))) {
		t.Fatal("the fixture trust bundle is not loadable")
	}
	host, _, err := net.SplitHostPort(w.address(t, face))
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12, ServerName: host, RootCAs: pool,
		ClientSessionCache: tls.NewLRUClientSessionCache(16),
	}
}

// s2CredentialCensus is the two-sided credential proof: on both FTPS faces the
// server's own audit of every session carries no credential at all, while the plain
// face on the same fixture, same tap and same needle list carries exactly one USER,
// one PASS and one password occurrence per session.
func s2CredentialCensus(t *testing.T, worker *s2Worker) {
	t.Helper()
	report := map[string]int{}
	for _, entry := range []struct {
		face, mode string
		want       int
	}{
		{s2fixture.ListenerExplicit, TLSModeExplicit, 0},
		{s2fixture.ListenerImplicit, TLSModeImplicit, 0},
		{s2fixture.ListenerPlain, TLSModePlain, 1},
	} {
		before := len(worker.sessions(t, entry.face))
		ca := worker.ca(t)
		if entry.mode == TLSModePlain {
			ca = ""
		}
		repository, err := Open(t.Context(), worker.config(t, "census-"+entry.face, entry.face, entry.mode, ca))
		s2Require(t, err)
		key := "census-" + entry.face + "/" + worker.marker + ".bin"
		sink, err := repository.Create(t.Context(), key)
		s2Require(t, err)
		s1Write(t, sink, worker.payload(8))
		_, commitErr := sink.Commit(t.Context())
		s2Require(t, commitErr)
		s2Require(t, repository.Close())
		sessions := worker.awaitSessions(t, entry.face, before)
		user, pass, secret := 0, 0, 0
		for _, session := range sessions {
			if session.Truncated || session.ToServerBytes == 0 {
				t.Fatalf("the %s census session is unusable: truncated=%t bytes=%d", entry.face, session.Truncated, session.ToServerBytes)
			}
			user += session.MustCount(t, s2fixture.ToServer, s2fixture.NeedleUser)
			pass += session.MustCount(t, s2fixture.ToServer, s2fixture.NeedlePass)
			secret += session.MustCount(t, s2fixture.ToServer, s2fixture.NeedleSecret)
		}
		if user != entry.want*len(sessions) || pass != entry.want*len(sessions) || secret != entry.want*len(sessions) {
			t.Fatalf("%s credential census: user=%d pass=%d secret=%d over %d sessions, want %d each",
				entry.face, user, pass, secret, len(sessions), entry.want)
		}
		report[entry.face] = entry.want
		if entry.mode != TLSModePlain {
			_, _, _ = s2RequireSilent(t, sessions[0], entry.face)
		}
	}
	t.Logf("S2-A2 credential_census explicit_user_per_session=0 explicit_pass_per_session=0 explicit_secret_per_session=0" +
		" implicit_user_per_session=0 implicit_pass_per_session=0 implicit_secret_per_session=0" +
		" plain_user_per_session=1 plain_pass_per_session=1 plain_secret_per_session=1 control=positive")
}

// s2Refusals exercises the four locked refusals on the faces that cause them, and
// requires the server's audit to show that each refused session reached it without a
// single credential byte.
func s2Refusals(t *testing.T, worker *s2Worker) {
	t.Helper()
	for _, entry := range []struct {
		face, mode, kind string
	}{
		{s2fixture.ListenerRogue, TLSModeExplicit, "tls_untrusted"},
		{s2fixture.ListenerWrongHost, TLSModeExplicit, "tls_hostname"},
		{s2fixture.ListenerExpired, TLSModeExplicit, "tls_expired"},
		{s2fixture.ListenerPlain, TLSModeExplicit, "tls_negotiation"},
		{s2fixture.ListenerPlain, TLSModeImplicit, "tls_negotiation"},
	} {
		before := len(worker.sessions(t, entry.face))
		err := func() error {
			repository, openErr := Open(t.Context(), worker.config(t, "refusal-"+entry.kind+"-"+entry.mode, entry.face, entry.mode, worker.ca(t)))
			if openErr != nil {
				return openErr
			}
			defer func() { _ = repository.Close() }()
			return repository.Probe(t.Context())
		}()
		kind, reply := s2Kind(t, err, worker.password)
		if kind != entry.kind {
			t.Fatalf("the %s face refused with kind=%s, want %s (%v)", entry.face, kind, entry.kind, err)
		}
		sessions := worker.awaitSessions(t, entry.face, before)
		for _, session := range sessions {
			s2RequireSilent(t, session, entry.face)
		}
		// The identity an independent verification sees is the second side of every
		// TLS refusal: the plain face serves no certificate at all, so there is no
		// leaf to name and no handshake to refuse it.
		fingerprint, dns, notAfter := "none", "none", time.Time{}
		if entry.face != s2fixture.ListenerPlain {
			var verdict error
			fingerprint, dns, notAfter, verdict = worker.s2Identity(t, entry.face)
			if observed := s2VerdictKind(verdict); observed != entry.kind {
				t.Fatalf("the %s face refuses as %s to the library and as %s to an independent verification",
					entry.face, entry.kind, observed)
			}
		}
		t.Logf("S2-A2 refusal kind=%s listener=%s mode=%s reply=%d audit_sessions=%d user_per_session=0 pass_per_session=0"+
			" secret_per_session=0 leaf=%s dns=%s not_after=%s",
			kind, entry.face, entry.mode, reply, len(sessions), fingerprint, dns, notAfter.UTC().Format(time.RFC3339Nano))
	}
}

// s2NoDowngrade closes the downgrade question from both directions: a repository that
// selects plaintext against a server that requires TLS is refused by the server, and a
// repository that selects FTPS against a server without TLS is refused at the
// transport. The same fixture and account then succeed in the mode each face
// actually serves, so neither refusal is a dead server in disguise.
func s2NoDowngrade(t *testing.T, worker *s2Worker) {
	t.Helper()
	plainOnTLS := func() error {
		repository, err := Open(t.Context(), worker.config(t, "downgrade-plain", s2fixture.ListenerExplicit, TLSModePlain, ""))
		if err != nil {
			return err
		}
		defer func() { _ = repository.Close() }()
		return repository.Probe(t.Context())
	}()
	var operation *Error
	if !errors.As(plainOnTLS, &operation) {
		t.Fatalf("plaintext against a TLS-required server was accepted: %v", plainOnTLS)
	}
	if operation.ReplyCode != 421 {
		t.Fatalf("the TLS-required server refused plaintext with %d, want its own 421", operation.ReplyCode)
	}
	if errors.Is(plainOnTLS, ErrTLS) {
		t.Fatal("a plaintext session was classified as a TLS refusal")
	}
	before := len(worker.sessions(t, s2fixture.ListenerPlain))
	refusal := func() error {
		repository, err := Open(t.Context(), worker.config(t, "downgrade-explicit", s2fixture.ListenerPlain, TLSModeExplicit, worker.ca(t)))
		if err != nil {
			return err
		}
		defer func() { s2Require(t, repository.Close()) }()
		return repository.Probe(t.Context())
	}()
	kind, _ := s2Kind(t, refusal, worker.password)
	if kind != "tls_negotiation" {
		t.Fatalf("explicit against a server without TLS gave kind=%s", kind)
	}
	// The refused session is judged before the positive controls run: the plaintext
	// control on the same face carries the credential by design and must not be
	// counted as part of the refusal.
	for _, session := range worker.awaitSessions(t, s2fixture.ListenerPlain, before) {
		s2RequireSilent(t, session, s2fixture.ListenerPlain)
	}
	for _, control := range []struct {
		instance, face, mode, ca, what string
	}{
		{"downgrade-control-plain", s2fixture.ListenerPlain, TLSModePlain, "", "plaintext on the plain face"},
		{"downgrade-control-ftps", s2fixture.ListenerExplicit, TLSModeExplicit, worker.ca(t), "FTPS on the TLS face"},
	} {
		repository, err := Open(t.Context(), worker.config(t, control.instance, control.face, control.mode, control.ca))
		if err != nil {
			t.Fatalf("the positive control for %s failed: %v", control.what, err)
		}
		s2Require(t, repository.Close())
	}
	t.Logf("S2-A2 no_downgrade plain_on_tls_listener=refused reply=%d explicit_on_plain_listener=refused reply=%d"+
		" positive_control=ok plaintext_fallback=never", operation.ReplyCode, operation2Reply(refusal))
}

// operation2Reply reads the server's own reply code out of a classified failure, and
// reports zero when the refusal happened before any FTP reply could exist.
func operation2Reply(err error) int {
	var operation *Error
	if errors.As(err, &operation) {
		return operation.ReplyCode
	}
	return 0
}

// s2PlainBaseline keeps the S1 facts true on this fixture: a plaintext session still
// publishes, lists, reads back and deletes exactly as before, and its credentials
// still travel in the clear, which is what makes the FTPS census meaningful.
func s2PlainBaseline(t *testing.T, worker *s2Worker) {
	t.Helper()
	before := len(worker.sessions(t, s2fixture.ListenerPlain))
	repository, err := Open(t.Context(), worker.config(t, "baseline", s2fixture.ListenerPlain, TLSModePlain, ""))
	s2Require(t, err)
	key := "baseline/payload.bin"
	data := worker.payload(1024)
	sink, err := repository.Create(t.Context(), key)
	s2Require(t, err)
	s1Write(t, sink, data)
	receipt, err := sink.Commit(t.Context())
	s2Require(t, err)
	body, err := repository.Open(t.Context(), key, 0)
	s2Require(t, err)
	got, err := io.ReadAll(body)
	s2Require(t, errors.Join(err, body.Close()))
	if !bytes.Equal(got, data) || receipt.Size != int64(len(data)) {
		t.Fatal("the plaintext baseline read back different bytes")
	}
	listed, err := repository.List(t.Context(), "baseline")
	s2Require(t, err)
	if len(listed) != 1 || listed[0] != key {
		t.Fatalf("plaintext listing = %v, want the one published key", listed)
	}
	s2Require(t, repository.Delete(t.Context(), key))
	s2Require(t, repository.Close())
	if _, err := os.Stat(filepath.Join(worker.oracle, worker.account, filepath.FromSlash(key))); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the deleted object is still on the server filesystem")
	}
	sessions := worker.awaitSessions(t, s2fixture.ListenerPlain, before)
	user, secret := 0, 0
	for _, session := range sessions {
		user += session.MustCount(t, s2fixture.ToServer, s2fixture.NeedleUser)
		secret += session.MustCount(t, s2fixture.ToServer, s2fixture.NeedleSecret)
	}
	if user == 0 || secret == 0 {
		t.Fatal("the plaintext baseline sent no credential, so the FTPS census proves nothing")
	}
	t.Logf("S2-A4 plain_baseline publish=verified listing=%d objects=%d oracle_digest=%s plaintext_credentials=true sessions=%d",
		len(listed), 1, fmt.Sprintf("%x", sha256.Sum256(data)), len(sessions))
}

// publishInto writes and commits one object through an already-open repository, and
// deleteFrom removes it again. The session-reuse case needs several transfers on one
// repository so that a later data channel has a cached session to resume.
func (w *s2Worker) publishInto(t *testing.T, repository *Repository, key string) {
	t.Helper()
	sink, err := repository.Create(t.Context(), key)
	s2Require(t, err)
	s1Write(t, sink, w.payload(16))
	_, err = sink.Commit(t.Context())
	s2Require(t, err)
	body, err := repository.Open(t.Context(), key, 0)
	s2Require(t, err)
	_, err = io.Copy(io.Discard, body)
	s2Require(t, errors.Join(err, body.Close()))
}

func (w *s2Worker) deleteFrom(t *testing.T, repository *Repository, key string) {
	t.Helper()
	s2Require(t, repository.Delete(t.Context(), key))
}

// privateAddress is the private-network half of the topology claim, read from the
// address the fixture itself published.
func privateAddress(value string) bool {
	ip := net.ParseIP(value)
	return ip != nil && ip.IsPrivate()
}

// s2ClientVersion names the FTP client library this binary really linked, from its
// own build information rather than a literal in a log line.
func s2ClientVersion() string {
	if information, ok := debug.ReadBuildInfo(); ok {
		for _, dependency := range information.Deps {
			if dependency.Path == "github.com/jlaffaye/ftp" {
				return "jlaffaye/ftp-" + dependency.Version
			}
		}
	}
	return "jlaffaye/ftp-unreadable"
}

// s2InstanceMarker proves that FTPS transfers keep the namespace rules intact: the
// instance marker really is written into the server's own filesystem, a namespace whose
// marker was replaced by another instance's is refused as an ownership refusal rather
// than a transport one, the published object survives that refusal byte for byte, and
// the owning instance is admitted again once its own marker is back.
func s2InstanceMarker(t *testing.T, worker *s2Worker) {
	t.Helper()
	ca := worker.ca(t)
	const instance = "marker"
	key := instance + "/payload.bin"
	repository, err := Open(t.Context(), worker.config(t, instance, s2fixture.ListenerExplicit, TLSModeExplicit, ca))
	s2Require(t, err)
	observation := s2Instrument(t, repository)
	data := worker.payload(8)
	sink, err := repository.Create(t.Context(), key)
	s2Require(t, err)
	s1Write(t, sink, data)
	_, err = sink.Commit(t.Context())
	s2Require(t, err)
	s2Require(t, repository.Close())
	// The marker is the namespace's own record of which instance wrote it, and it is
	// only a fact once the server's filesystem shows it carrying that instance id.
	markerPath := filepath.Join(worker.oracle, worker.account, instance, markerName)
	owned, readErr := os.ReadFile(markerPath)
	if readErr != nil || string(owned) != instance {
		t.Fatalf("the FTPS namespace carries no marker for %q: %q err=%v", instance, owned, readErr)
	}
	requireForeignMarkerRefused(t, worker, ca, instance, key, data, markerPath)
	// Re-opening the namespace as its own instance must still be allowed after the
	// refusal, and the committed object is never silently overwritten.
	require2(t, os.WriteFile(markerPath, []byte(instance), 0o600))
	reopened, err := Open(t.Context(), worker.config(t, instance, s2fixture.ListenerExplicit, TLSModeExplicit, ca))
	s2Require(t, err)
	if err := reopened.Probe(t.Context()); err != nil {
		t.Fatalf("the owning instance could not re-probe its FTPS namespace: %v", err)
	}
	if _, err := reopened.Create(t.Context(), key); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("a committed object was open to being overwritten: %v", err)
	}
	s2Require(t, reopened.Close())
	_, _, _, channels, _ := observation.census()
	if channels == 0 {
		t.Fatal("the marker case observed no encrypted channel")
	}
	t.Logf("S2-A3 instance_marker marker_bytes=%d foreign=refused kind=%s preserved=true reprobe=ok channels=%d",
		len(markerFile(worker)), "ownership", channels)
}

// requireForeignMarkerRefused replaces the namespace marker with another instance's
// claim and demands that the owning instance is refused as an ownership refusal, never
// as a transport one, with its published object untouched.
func requireForeignMarkerRefused(t *testing.T, worker *s2Worker, ca, instance, key string, data []byte, markerPath string) {
	t.Helper()
	// Somebody else claims the namespace: the marker content is replaced through the
	// volume, which is exactly what a second deployment would leave behind.
	require2(t, os.WriteFile(markerPath, []byte("written by somebody else"), 0o600))
	foreignErr := func() error {
		other, openErr := Open(t.Context(), worker.config(t, instance, s2fixture.ListenerExplicit, TLSModeExplicit, ca))
		if openErr != nil {
			return openErr
		}
		defer func() { _ = other.Close() }()
		return other.Probe(t.Context())
	}()
	if foreignErr == nil {
		t.Fatal("a namespace with a foreign marker was adopted over FTPS")
	}
	if !errors.Is(foreignErr, ErrOwnership) {
		s2Fatal(t, foreignErr)
	}
	if strings.HasPrefix(kindOfError(foreignErr), "tls_") {
		t.Fatalf("the ownership refusal was reported as a transport refusal: %v", foreignErr)
	}
	// The published object is untouched by the refusal: ownership is refused before
	// any write of the namespace is attempted.
	if onDisk := worker.disk(t, key); !bytes.Equal(onDisk, data) {
		t.Fatal("the refused instance changed the published object")
	}
}

// markerFile reads the namespace marker again so the logged byte count is the file the
// server holds, not the one the worker intended to write.
func markerFile(worker *s2Worker) []byte {
	raw, err := os.ReadFile(filepath.Join(worker.oracle, worker.account, "marker", markerName))
	if err != nil {
		return nil
	}
	return raw
}

// kindOfError names the classified transport kind of an error, or "unclassified".
func kindOfError(err error) string {
	var operation *Error
	if errors.As(err, &operation) {
		return operation.Kind
	}
	return "unclassified"
}

// require2 fails a test on any error; the package's own s1Require belongs to the S1
// worker and carries its diagnostics.
func require2(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func tlsVersion(version uint16) string {
	switch version {
	case tls.VersionTLS13:
		return "TLS1.3"
	case tls.VersionTLS12:
		return "TLS1.2"
	case tls.VersionTLS11:
		return "TLS1.1"
	case tls.VersionTLS10:
		return "TLS1.0"
	}
	return "unknown"
}

// s2RawSession is a deliberately small FTP-over-TLS client. The library under test
// never issues PROT C, never selects plaintext against a TLS-required server and
// never opens an unencrypted data socket, so the server's own obligations can only be
// observed by a client that will.
type s2RawSession struct {
	conn   net.Conn
	text   *textproto.Conn
	face   string
	closed bool
}

func (w *s2Worker) openRaw(t *testing.T, face string) *s2RawSession {
	t.Helper()
	address := w.address(t, face)
	control, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		t.Fatalf("dial the %s face: %v", face, err)
	}
	session := &s2RawSession{conn: control, text: textproto.NewConn(control), face: face}
	t.Cleanup(func() { _ = session.Close() })
	if _, _, err := session.read(220); err != nil {
		t.Fatalf("%s greeting: %v", face, err)
	}
	if _, _, err := session.command(t, "AUTH TLS", 234); err != nil {
		t.Fatalf("%s AUTH TLS: %v", face, err)
	}
	// The upgrade, the protection level and the buffer size are all answered only
	// before a credential is offered, so this order is the server's own contract.
	secure := tls.Client(control, w.clientConfig(t, face))
	if err := secure.HandshakeContext(t.Context()); err != nil {
		t.Fatalf("%s control handshake: %v", face, err)
	}
	session.conn = secure
	session.text = textproto.NewConn(secure)
	if _, _, err := session.command(t, "PBSZ 0", 200); err != nil {
		t.Fatalf("%s PBSZ: %v", face, err)
	}
	if _, _, err := session.command(t, "PROT P", 200); err != nil {
		t.Fatalf("%s PROT P: %v", face, err)
	}
	if _, _, err := session.command(t, "USER "+w.account, 331); err != nil {
		t.Fatalf("%s USER: %v", face, err)
	}
	if _, _, err := session.command(t, "PASS "+w.password, 230); err != nil {
		t.Fatalf("%s PASS: %v", face, err)
	}
	if _, _, err := session.command(t, "TYPE I", 200); err != nil {
		t.Fatalf("%s TYPE I: %v", face, err)
	}
	return session
}

// command sends one FTP line and requires one exact reply code, returning the code and
// the server's own message so a refusal can be quoted from what was actually said.
func (s *s2RawSession) command(t *testing.T, line string, want int) (int, string, error) {
	t.Helper()
	code, message, err := s.expect(t, line, want)
	return code, message, err
}

// expect sends one command (an empty line sends nothing, so a reply can be read for
// the transfer already in flight) and requires one exact reply code.
func (s *s2RawSession) expect(t *testing.T, line string, want int) (code int, got string, err error) {
	t.Helper()
	if line != "" {
		if _, err = s.text.Cmd("%s", line); err != nil {
			return 0, "", err
		}
	}
	code, got, err = s.read(want)
	if err != nil {
		return code, got, err
	}
	return code, got, nil
}

func (s *s2RawSession) read(want int) (int, string, error) {
	code, message, err := s.text.ReadResponse(want)
	if err != nil {
		return code, message, err
	}
	return code, message, nil
}

// passivePort asks for a passive data port. The address is always the face's own host:
// the server names a port, and a data socket must be opened against the identity the
// certificate was issued for.
func (s *s2RawSession) passivePort(t *testing.T) int {
	t.Helper()
	_, message, err := s.expect(t, "PASV", 227)
	if err != nil {
		t.Fatalf("PASV: %v", err)
	}
	open := strings.IndexByte(message, '(')
	closeIndex := strings.IndexByte(message, ')')
	if open < 0 || closeIndex < open {
		t.Fatalf("unparsable PASV reply %q", message)
	}
	parts := strings.Split(message[open+1:closeIndex], ",")
	if len(parts) != 6 {
		t.Fatalf("unparsable PASV reply %q", message)
	}
	high, err := strconv.Atoi(parts[4])
	if err != nil {
		t.Fatalf("unparsable PASV port %q: %v", message, err)
	}
	low, err := strconv.Atoi(parts[5])
	if err != nil {
		t.Fatalf("unparsable PASV port %q: %v", message, err)
	}
	return high<<8 + low
}

func (s *s2RawSession) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.conn.Close()
}
