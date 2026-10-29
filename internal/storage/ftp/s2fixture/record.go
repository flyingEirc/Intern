package s2fixture

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/flyingEirc/Intern/internal/fsutil"
)

// The listener faces the fixture must publish. A worker never dials a port it took
// from a constant: it reads the manifest back and the harness refuses a manifest
// whose ports differ from the matrix below, so the two cannot drift apart.
const (
	ListenerExplicit  = "explicit"
	ListenerPlain     = "plain"
	ListenerWrongHost = "wronghost"
	ListenerExpired   = "expired"
	ListenerRogue     = "rogue"
	ListenerImplicit  = "implicit"
)

// Face is one listener's address pair and the passive range behind it.
type Face struct {
	Client       int
	Daemon       int
	PassiveFirst int
	PassiveLast  int
	TLSLevel     string
	Cert         string
}

// Matrix is the fixture's own port plan, keyed by listener name.
var Matrix = map[string]Face{
	ListenerExplicit:  {Client: 2121, Daemon: 12121, PassiveFirst: 50100, PassiveLast: 50119, TLSLevel: "3", Cert: LeafGood},
	ListenerPlain:     {Client: 2122, Daemon: 12122, PassiveFirst: 50200, PassiveLast: 50219, TLSLevel: "0", Cert: "none"},
	ListenerWrongHost: {Client: 2123, Daemon: 12123, PassiveFirst: 50300, PassiveLast: 50319, TLSLevel: "3", Cert: LeafWrongHost},
	ListenerExpired:   {Client: 2124, Daemon: 12124, PassiveFirst: 50400, PassiveLast: 50419, TLSLevel: "3", Cert: LeafExpired},
	ListenerRogue:     {Client: 2125, Daemon: 12125, PassiveFirst: 50500, PassiveLast: 50519, TLSLevel: "3", Cert: LeafRogue},
	ListenerImplicit:  {Client: 990, Daemon: 1990, PassiveFirst: 50600, PassiveLast: 50619, TLSLevel: "3", Cert: LeafGood},
}

// Directions of one audited session.
const (
	ToServer   = "to_server"
	FromServer = "from_server"
)

// The needles a tap counts. They are fixed so a refusal cannot be arranged around a
// pattern the operator chose: "user" and "pass" are the two commands that carry a
// credential, "auth_tls" is the upgrade that must precede them, "secret" is the
// generated account password, "marker" is the cleartext probe, and "greeting" is the
// server's plaintext 220 line.
const (
	NeedleUser     = "user"
	NeedlePass     = "pass"
	NeedleAuthTLS  = "auth_tls"
	NeedleSecret   = "secret"
	NeedleMarker   = "marker"
	NeedleGreeting = "greeting"
)

// Audit is one session as the server-side tap recorded it. It carries counts, byte
// totals and digests, and never a byte it searched.
type Audit struct {
	Seq             int64          `json:"seq"`
	Name            string         `json:"name"`
	Client          string         `json:"client"`
	Started         int64          `json:"started"`
	Ended           int64          `json:"ended"`
	ToServer        map[string]int `json:"to_server"`
	FromServer      map[string]int `json:"from_server"`
	ToServerBytes   int64          `json:"to_server_bytes"`
	FromServerBytes int64          `json:"from_server_bytes"`
	ToServerSHA     string         `json:"to_server_sha256"`
	FromServerSHA   string         `json:"from_server_sha256"`
	Truncated       bool           `json:"truncated"`
	Error           string         `json:"error,omitempty"`
}

// Count reports one needle in one direction. A missing entry is a refusal of the
// record, not a zero: a census that silently reads an absent needle as "absent from
// the wire" is the exact mistake this file exists to prevent.
func (a Audit) Count(direction, needle string) (int, error) {
	var field map[string]int
	switch direction {
	case ToServer:
		field = a.ToServer
	case FromServer:
		field = a.FromServer
	default:
		return 0, fmt.Errorf("unknown audit direction %q", direction)
	}
	value, ok := field[needle]
	if !ok {
		return 0, fmt.Errorf("session %d (%s) carries no %s count for %s", a.Seq, a.Name, direction, needle)
	}
	return value, nil
}

// MustCount is Count for a test that has no way to continue without the number.
func (a Audit) MustCount(t interface {
	Helper()
	Fatal(...any)
}, direction, needle string,
) int {
	t.Helper()
	value, err := a.Count(direction, needle)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// ReadAudit parses a tap's JSONL output. Every line must be a whole record; a
// truncated final line is a torn write, and reading it as nothing would drop a
// session off the census.
func ReadAudit(path string) (_ []Audit, retErr error) {
	// #nosec G304 -- the path is one fixture filename the harness chose, never
	// caller-supplied content: the census and the manifest of the FTPS fixture.
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { fsutil.Close(&retErr, file) }()
	var (
		records []Audit
		scanner = bufio.NewScanner(file)
		line    int
	)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		record, err := parseAudit(text, line)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

// parseAudit reads one census line. A line that names no listener is refused:
// a record cannot be attributed to one of the six faces, so counting it would
// invent which session it proved.
func parseAudit(text string, line int) (Audit, error) {
	var record Audit
	if err := json.Unmarshal([]byte(text), &record); err != nil {
		return Audit{}, fmt.Errorf("audit line %d: %w", line, err)
	}
	if record.Name == "" {
		return Audit{}, fmt.Errorf("audit line %d names no listener", line)
	}
	return record, nil
}

// SessionsFor returns the records of one listener, oldest first.
func SessionsFor(records []Audit, name string) []Audit {
	var matching []Audit
	for _, record := range records {
		if record.Name == name {
			matching = append(matching, record)
		}
	}
	return matching
}

// Listener is one face of the fixture as the fixture itself described it.
type Listener struct {
	Name            string
	Client          int
	Daemon          int
	PassiveFirst    int
	PassiveLast     int
	TLSLevel        string
	Cert            string
	CertFingerprint string
}

// Fixture is the manifest file the entrypoint publishes before it reports
// readiness.
type Fixture struct {
	Advertise            string
	DaemonSHA256         string
	ImplicitDaemonSHA256 string
	TapSHA256            string
	AuthBackend          string
	MarkerSHA256         string
	CAFingerprint        string
	Listeners            map[string]Listener
}

// ReadFixture parses the manifest and refuses a face missing from it, so a fixture
// that came up with five listeners instead of six is a failure rather than a case
// that silently never ran.
func ReadFixture(path string) (Fixture, error) {
	// #nosec G304 -- one fixed manifest file the harness chose inside the fixture
	// volume, never a path that arrives from a request.
	raw, err := os.ReadFile(path)
	if err != nil {
		return Fixture{}, err
	}
	fixture := Fixture{Listeners: make(map[string]Listener, len(Matrix))}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if err := fixture.readLine(line); err != nil {
			return Fixture{}, err
		}
	}
	for name := range Matrix {
		if _, ok := fixture.Listeners[name]; !ok {
			return Fixture{}, fmt.Errorf("the fixture manifest has no %s listener", name)
		}
	}
	return fixture, nil
}

// readLine applies one manifest line: a listener record, or a single top-level
// fact. An unknown key is refused rather than skipped, because a fixture that
// renamed what it publishes must not be able to hide a missing fact.
func (f *Fixture) readLine(line string) error {
	if strings.HasPrefix(line, "listener ") {
		listener, err := parseListener(line)
		if err != nil {
			return err
		}
		f.Listeners[listener.Name] = listener
		return nil
	}
	key, value, ok := strings.Cut(line, "=")
	if !ok {
		return fmt.Errorf("manifest line %q is not key=value", line)
	}
	return f.setField(key, value)
}

func (f *Fixture) setField(key, value string) error {
	switch key {
	case "advertise":
		f.Advertise = value
	case "daemon_sha256":
		f.DaemonSHA256 = value
	case "implicit_daemon_sha256":
		f.ImplicitDaemonSHA256 = value
	case "tap_sha256":
		f.TapSHA256 = value
	case "auth_backend":
		f.AuthBackend = value
	case "marker_sha256":
		f.MarkerSHA256 = value
	case "ca_sha256":
		f.CAFingerprint = value
	default:
		return fmt.Errorf("unknown manifest key %q", key)
	}
	return nil
}

func parseListener(line string) (Listener, error) {
	listener := Listener{}
	for _, field := range strings.Fields(strings.TrimPrefix(line, "listener ")) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return listener, fmt.Errorf("listener field %q is not key=value", field)
		}
		if err := listener.setField(key, value); err != nil {
			return listener, err
		}
	}
	if listener.Name == "" || listener.Client == 0 || listener.TLSLevel == "" {
		return listener, errors.New("an incomplete listener record")
	}
	return listener, nil
}

func (l *Listener) setField(key, value string) error {
	switch key {
	case "name":
		l.Name = value
	case "client":
		l.Client = atoi(value)
	case "daemon":
		l.Daemon = atoi(value)
	case "tls":
		l.TLSLevel = value
	case "cert":
		l.Cert = value
	case "cert_sha256":
		l.CertFingerprint = value
	case "passive":
		first, last, ok := strings.Cut(value, ":")
		if !ok {
			return fmt.Errorf("passive range %q is not first:last", value)
		}
		l.PassiveFirst, l.PassiveLast = atoi(first), atoi(last)
	default:
		return fmt.Errorf("unknown listener key %q", key)
	}
	return nil
}

func atoi(value string) int {
	number, err := strconv.Atoi(value)
	if err != nil {
		return -1
	}
	return number
}

// MatchesMatrix requires the published manifest to be the fixture the harness
// started: same ports, same TLS levels, same certificate per face. A fixture that
// drifted - a listener that came up with TLS disabled, say - must not be able to
// carry a green run.
func (f Fixture) MatchesMatrix() error {
	if err := f.hasFaces(); err != nil {
		return err
	}
	return f.hasIdentity()
}

// hasFaces compares every published face with the matrix the harness started. A
// listener that came up with TLS disabled, on another port or with a different
// certificate is a different fixture, and a refusal against it would prove nothing
// about the face the run claims to have exercised.
func (f Fixture) hasFaces() error {
	for name, want := range Matrix {
		got, ok := f.Listeners[name]
		if !ok {
			return fmt.Errorf("listener %s is missing", name)
		}
		if got.Face() != want {
			return fmt.Errorf("listener %s is %s, want %s", name, describe(got), formatFace(want))
		}
	}
	return nil
}

// hasIdentity requires the fixture to have published the digests and account binding a
// green run is computed from. An empty one is a fixture that never finished writing
// its own manifest.
func (f Fixture) hasIdentity() error {
	for _, required := range []struct{ key, value string }{
		{"advertise", f.Advertise},
		{"daemon_sha256", f.DaemonSHA256},
		{"implicit_daemon_sha256", f.ImplicitDaemonSHA256},
		{"tap_sha256", f.TapSHA256},
		{"auth_backend", f.AuthBackend},
		{"marker_sha256", f.MarkerSHA256},
		{"ca_sha256", f.CAFingerprint},
	} {
		if required.value == "" || required.value == "-" {
			return fmt.Errorf("the manifest has no %s", required.key)
		}
	}
	return nil
}

// Face is the published shape of one listener, as a matrix entry.
func (l Listener) Face() Face {
	return Face{
		Client: l.Client, Daemon: l.Daemon, PassiveFirst: l.PassiveFirst,
		PassiveLast: l.PassiveLast, TLSLevel: l.TLSLevel, Cert: l.Cert,
	}
}

func formatFace(want Face) string {
	return fmt.Sprintf("client=%d daemon=%d passive=%d:%d tls=%s cert=%s",
		want.Client, want.Daemon, want.PassiveFirst, want.PassiveLast, want.TLSLevel, want.Cert)
}

func describe(l Listener) string {
	return fmt.Sprintf("client=%d daemon=%d passive=%d:%d tls=%s cert=%s",
		l.Client, l.Daemon, l.PassiveFirst, l.PassiveLast, l.TLSLevel, l.Cert)
}

// Address is how a worker names one face: the fixture's DNS alias, never its IP, so
// the TLS server name is the identity the certificate carries.
func (f Fixture) Address(host, name string) (string, error) {
	listener, ok := f.Listeners[name]
	if !ok {
		return "", fmt.Errorf("the fixture has no %s listener", name)
	}
	return net.JoinHostPort(host, strconv.Itoa(listener.Client)), nil
}
