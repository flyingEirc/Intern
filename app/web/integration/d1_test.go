//go:build integration

package integration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/pgtest"
	"github.com/flyingEirc/Intern/internal/physical"
)

// The D1 harness drives the repository's own Compose file with the real
// `docker compose` CLI. It copies nothing, renders nothing and simulates nothing:
// the only addition is an override file (loopback test ports, a CI-sized InnoDB
// buffer pool and the read-only refusal fixtures), so every locked fact below is
// proved against app/web/deploy/compose.yaml exactly as an operator runs it.
const (
	d1ComposePath      = "app/web/deploy/compose.yaml"
	d1DockerfilePath   = "app/web/deploy/Dockerfile"
	d1EnvExamplePath   = "app/web/deploy/.env.example"
	d1LicenseDirectory = "/usr/share/licenses/intern"
	d1SecretKEK        = "/run/secrets/intern_kek"
	d1ProbeKEK         = "/d1-secrets/intern_kek_0644"
	d1AbsentKEK        = "/d1-secrets/intern_kek_absent"
	d1ProbeMetadata    = "/tmp/d1-refusal-metadata"
	d1SocketPath       = "/var/run/mysqld/mysqld.sock"
	d1DataDirPath      = "/var/lib/mysql"
	d1MetadataPath     = "/var/lib/intern/metadata"
	d1BackupsPath      = "/var/lib/intern/backups"
	d1WorkPath         = "/var/lib/intern/work"
	d1ExportPath       = "/var/lib/intern/export"
	d1WebBinary        = "/usr/local/bin/intern-web"
	d1XtrabackupBinary = "/usr/bin/xtrabackup"
	d1XbstreamBinary   = "/usr/bin/xbstream"
	d1MysqldBinary     = "/usr/sbin/mysqld"
	d1AdminUsername    = "d1-admin"
	d1BackupUser       = "intern_bkp"
	d1Database         = "fixture"
	d1ArtifactDir      = "INTERN_D1_ARTIFACT_DIR"
)

// d1Cases is the locked D1 case set. The gate requires exactly these and nothing
// else.
var d1Cases = []string{"a1-image", "a2-compose", "a3-business", "a4-lifecycle", "a5-guardrails"}

// Each D1 evidence line begins with one of these keys, and the locked list below
// is built from the very same constants the emit sites use. A marker therefore
// cannot drift from the text that is printed: adding a case without a key, or
// rewording a line away from its key, fails compilation or the harness' own
// marker cross-check instead of silently losing evidence.
const (
	d1KeyCase      = "D1-CASE "
	d1KeyBuilt     = "D1-A1 built "
	d1KeyRepeat    = "D1-A1 repeatable="
	d1KeyBases     = "D1-A1 bases_pinned="
	d1KeyDownloads = "D1-A1 runtime_downloads="
	d1KeyUser      = "D1-A1 non_root="
	d1KeyVersions  = "D1-A1 versions "
	d1KeyGPL       = "D1-A1 gpl "
	d1KeyPins      = "D1-A1 compose_pins="
	d1KeyIdentity  = "D1-A2 identity "
	d1KeyStack     = "D1-A2 stack="
	d1KeyAdmin     = "D1-A2 setup="
	d1KeyTLS       = "D1-A2 https="
	d1KeyDataDir   = "D1-A2 datadir_ro="
	d1KeyKEK       = "D1-A2 kek "
	d1KeySocket    = "D1-A2 socket="
	d1KeyTopo      = "D1-A3 topology="
	d1KeyFull      = "D1-A3 full "
	d1KeyInc       = "D1-A3 incremental "
	d1KeyRestore   = "D1-A3 restore="
	d1KeyExport    = "D1-A3 export="
	d1KeyNewMySQL  = "D1-A3 export_mysql_started="
	d1KeyRestart   = "D1-A4 restart "
	d1KeyRecreate  = "D1-A4 recreate "
	d1KeyUpgrade   = "D1-A4 upgrade "
	d1KeyKEKKeep   = "D1-A4 kek="
	d1KeyFlock     = "D1-A5 second_container="
	d1KeyKEKRefuse = "D1-A5 kek_refusal="
	d1KeyStop      = "D1-A5 sigterm "
	d1KeyNoFalse   = "D1-A5 no_false_completed="
	d1KeyArtifact  = "D1-ARTIFACT "
)

// d1RunKey is the marker key of one completed run line.
func d1RunKey(kind string) string {
	if kind == "incremental" {
		return d1KeyInc
	}
	return d1KeyFull
}

// d1LifecycleKey is the marker key of one lifecycle retention line.
func d1LifecycleKey(label string) string {
	switch label {
	case "recreate":
		return d1KeyRecreate
	case "upgrade":
		return d1KeyUpgrade
	default:
		return d1KeyRestart
	}
}

// d1ResultMarkers are the fixed facts every D1 run must report. A marker is
// recorded only after the assertions on the read-back state it names have passed,
// and every one of them is printed by this harness, not by a worker.
var d1ResultMarkers = []string{
	d1KeyBuilt, d1KeyRepeat, d1KeyBases, d1KeyDownloads, d1KeyUser, d1KeyVersions,
	d1KeyGPL, d1KeyPins,
	d1KeyIdentity, d1KeyStack, d1KeyAdmin, d1KeyTLS, d1KeyDataDir, d1KeyKEK, d1KeySocket,
	d1KeyTopo, d1KeyFull, d1KeyInc, d1KeyRestore, d1KeyExport, d1KeyNewMySQL,
	d1KeyRestart, d1KeyRecreate, d1KeyUpgrade, d1KeyKEKKeep,
	d1KeyFlock, d1KeyKEKRefuse, d1KeyStop, d1KeyNoFalse,
	d1KeyArtifact,
}

var (
	// Backups, chains and restores are 32 lowercase hex; resources (repositories,
	// sources, policies) and runs are canonical UUIDv7 strings.
	d1Hex32       = regexp.MustCompile(`^[0-9a-f]{32}$`)
	d1UUID        = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	d1Hex64       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	d1VersionLine = regexp.MustCompile(`(Ver |version )([0-9][0-9.]*(?:-[0-9]+)?)`)
	// d1InstallPattern names the commands that would fetch code or binaries at
	// run time. The D1 image must contain none of them in its own layers.
	d1InstallPattern = regexp.MustCompile(`(?i)(apt-get|apt install|yum |dnf |microdnf|apk add|curl |wget |pip install|npm install|yarn add|go get|git clone)`)
	d1LabelImage     = regexp.MustCompile(`^[^ ]+@sha256:[0-9a-f]{64}$`)
)

// d1Fixture is one deployment under test: the Compose project, the secret and
// export fixtures it mounts, the image tags it builds, and the identities the
// acceptance drives through the public HTTPS API.
type d1Fixture struct {
	t              *testing.T
	root           string
	dir            string
	hostDir        string
	kekDigest      string
	secrets        string
	probes         string
	exports        string
	override       string
	project        string
	composeFile    string
	env            []string
	credential     []string
	source         string
	dirty          bool
	candidate      string
	upgrade        string
	candidateID    string
	upgradeID      string
	webDigest      string
	caPEM          []byte
	rootPass       string
	sourcePass     string
	adminPass      string
	databaseDigest string
	postgresPass   string
	mark           []string
	saw            map[string]bool
	containers     map[string]string
	layers         []string
	http           *http.Client
	apiBase        string
	mysqlPort      int
	instanceID     string
	serverUUID     string
	repositoryID   string
	sourceID       string
	policyID       string
	slowPolicyID   string
	fullBackup     string
	incBackup      string
	restoreID      string
	restoreExport  string
	snapshot       string
}

// TestD1NativeContainers is the D1 deployment acceptance. It builds the
// repository's Intern Web image from the pinned compatibility matrix, brings the
// repository's Compose file up with real `docker compose`, and proves the locked
// deployment facts through the public HTTPS API, through `docker compose exec`,
// and through the volumes the stack itself created. The pinned MySQL and
// XtraBackup images are linux/amd64; an emulated run is development evidence
// only and can never satisfy the native CI gate. The opt-in below is ignored in
// GitHub Actions.

func d1DevelopmentEmulation() bool {
	return os.Getenv("INTERN_D1_DEVELOPMENT_EMULATION") == "true" && os.Getenv("GITHUB_ACTIONS") != "true"
}

func d1RequirePlatform(t *testing.T, osType, architecture string) {
	t.Helper()
	if d1DevelopmentEmulation() {
		if osType != "linux" {
			t.Fatal("D1 development evidence requires a Linux Docker daemon")
		}
		t.Log("D1 DEVELOPMENT ONLY: amd64 emulation permitted; this run cannot satisfy the native CI evidence gate")
		return
	}
	requireNativeDaemon(t, osType, architecture)
}

func TestD1NativeContainers(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 55*time.Minute)
	defer cancel()
	probe, err := exec.CommandContext(ctx, "docker", "compose", "version").CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose v2 is required for the D1 deployment acceptance: %v\n%s", err, probe)
	}
	docker, err := tc.NewDockerClientWithOpts(ctx)
	require(t, err)
	defer func() { require(t, docker.Close()) }()
	info, err := docker.Info(ctx, client.InfoOptions{})
	require(t, err)
	d1RequirePlatform(t, info.Info.OSType, info.Info.Architecture)
	root, err := filepath.Abs("../../..")
	require(t, err)
	source, dirty := physicalSourceIdentity(t, ctx, root)
	fixture := newD1Fixture(t, root, source, dirty)
	t.Cleanup(func() {
		stop, cancelStop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancelStop()
		fixture.teardown(stop)
	})
	defer func() {
		for _, line := range fixture.mark {
			t.Log(line)
		}
		for _, marker := range d1ResultMarkers {
			if !fixture.sawMarker(marker) {
				t.Errorf("missing D1 evidence marker %q", marker)
			}
		}
		fixture.requireCaseSet(t)
	}()
	// Each case builds on the state the previous one produced (image → stack →
	// chain → lifecycle → guardrails), so the first failure ends the run instead of
	// letting later cases report consequences of a state that never existed.
	for _, step := range []struct {
		name string
		run  func(*testing.T, context.Context)
	}{
		{d1Cases[0], fixture.caseImage},
		{d1Cases[1], fixture.caseCompose},
		{d1Cases[2], fixture.caseBusiness},
		{d1Cases[3], fixture.caseLifecycle},
		{d1Cases[4], fixture.caseGuardrails},
	} {
		if !t.Run(step.name, func(t *testing.T) { step.run(t, ctx) }) {
			t.Fatalf("D1 case %s failed; the state it was to produce is unknown, so no later case runs", step.name)
		}
	}
}

// newD1Fixture creates the private fixture tree the deployment mounts: a real
// Intern KEK keyset, a test CA with its server certificate, the export
// directory, the refusal fixtures and the Compose override. Every pinned value
// comes from internal/physical/compat.json, never from a literal here.
func newD1Fixture(t *testing.T, root, source string, dirty bool) *d1Fixture {
	t.Helper()
	fixture := &d1Fixture{
		t: t, root: root, source: source, dirty: dirty, saw: map[string]bool{},
		containers: map[string]string{},
		project:    d1Project(),
		candidate:  "intern:" + d1Tag(source, dirty) + "-mysql8.4",
	}
	fixture.upgrade = strings.ReplaceAll(fixture.candidate, "-mysql8.4", "-upgrade-mysql8.4")
	fixture.composeFile = filepath.Join(root, d1ComposePath)
	fixture.dir = t.TempDir()
	// The Compose override is host-only: the ownership preparation chowns the
	// mounted tree to the container user, and a 0600 file inside it would then be
	// unreadable by the very process that has to pass it to `docker compose`.
	fixture.hostDir = t.TempDir()
	fixture.secrets = filepath.Join(fixture.dir, "secrets")
	fixture.probes = filepath.Join(fixture.dir, "probes")
	fixture.exports = filepath.Join(fixture.dir, "exports")
	for _, directory := range []string{fixture.secrets, fixture.probes, fixture.exports} {
		require(t, os.MkdirAll(directory, 0o755))
	}
	fixture.rootPass = randomSecret(t)
	fixture.sourcePass = randomSecret(t)
	fixture.adminPass = randomSecret(t)
	fixture.postgresPass = randomSecret(t)
	fixture.credential = []string{fixture.rootPass, fixture.sourcePass, fixture.adminPass, fixture.postgresPass}
	require(t, os.WriteFile(filepath.Join(fixture.secrets, "intern_postgres_password"), []byte(fixture.postgresPass), 0o600))
	require(t, os.WriteFile(filepath.Join(fixture.secrets, "intern_database_url"), []byte("postgres://intern:"+fixture.postgresPass+"@postgres:5432/intern?sslmode=disable"), 0o600))
	require(t, os.WriteFile(filepath.Join(fixture.probes, "intern_probe_database_url"), []byte("postgres://intern:"+fixture.postgresPass+"@postgres:5432/d1_refusal?sslmode=disable"), 0o600))
	fixture.caPEM = d1Certificate(t, fixture.secrets)
	require(t, artifact.GenerateKey(filepath.Join(fixture.secrets, "intern_kek")))
	require(t, os.Chmod(filepath.Join(fixture.secrets, "intern_kek"), 0o600))
	// The ownership preparation hands the secret to uid 999 with mode 0600, after
	// which the host test process can no longer read it; its digest is taken now,
	// while the host still owns the file it generated.
	kekDigest, err := d1FileDigest(filepath.Join(fixture.secrets, "intern_kek"))
	require(t, err)
	fixture.kekDigest = kekDigest
	databaseDigest, err := d1FileDigest(filepath.Join(fixture.secrets, "intern_database_url"))
	require(t, err)
	fixture.databaseDigest = databaseDigest
	require(t, os.Chmod(filepath.Join(fixture.secrets, "intern_tls_key"), 0o600))
	require(t, os.WriteFile(filepath.Join(fixture.probes, "intern_kek_0644"), []byte("not-a-keyset\n"), 0o644))
	require(t, fixture.writeOverride())
	mysqlImage, pxbImage := candidateImages(t)
	fixture.env = []string{
		d1Assign("MYSQL_ROOT_PASSWORD", fixture.rootPass),
		d1Assign("INTERN_MYSQL_IMAGE", mysqlImage),
		d1Assign("INTERN_PXB_IMAGE", pxbImage),
		d1Assign("INTERN_VERSION", d1Tag(source, dirty)),
		d1Assign("INTERN_SOURCE_SHA", source),
		d1Assign("INTERN_HOST_PORT", "0"),
		d1Assign("INTERN_KEK_FILE_HOST", filepath.Join(fixture.secrets, "intern_kek")),
		d1Assign("INTERN_TLS_CERT_HOST", filepath.Join(fixture.secrets, "intern_tls_cert")),
		d1Assign("INTERN_POSTGRES_PASSWORD_FILE", filepath.Join(fixture.secrets, "intern_postgres_password")),
		d1Assign("INTERN_DATABASE_URL_FILE", filepath.Join(fixture.secrets, "intern_database_url")),
		d1Assign("INTERN_TLS_KEY_HOST", filepath.Join(fixture.secrets, "intern_tls_key")),
		d1Assign("INTERN_EXPORT_HOST_DIR", fixture.exports),
		d1Assign("INTERN_MIN_WORK_BYTES", "268435456"),
		d1Assign("INTERN_MIN_BACKUP_BYTES", "268435456"),
		d1Assign("COMPOSE_PROGRESS", "quiet"),
		d1Assign("DOCKER_BUILDKIT", "1"),
	}
	return fixture
}

// d1Assign builds one environment assignment. Name and value stay apart so a
// generated credential is never written as a source literal.
func d1Assign(name, value string) string { return name + "=" + value }

// d1SetEnv replaces one assignment of the deployment environment, which is how
// the image-upgrade case swaps the tag without editing the repository file.
func (f *d1Fixture) d1SetEnv(name, value string) {
	for index, entry := range f.env {
		if strings.HasPrefix(entry, name+"=") {
			f.env[index] = d1Assign(name, value)
			return
		}
	}
	f.fatal("deployment environment has no %s assignment", name)
}

// d1Tag is the image tag component for one candidate: the exact commit, or the
// commit with a dirty marker when the acceptance runs on uncommitted source.
func d1Tag(source string, dirty bool) string {
	if dirty {
		return source[:12] + "-dirty"
	}
	return source[:12]
}

// d1Project names the Compose project. On a GitHub runner the name starts with
// d1 and the first 8 hex digits of sha256(RUNNER_NAME), so that
// .github/scripts/d1-compose-clean.sh can find the stacks this runner's jobs
// left behind: a cancelled job ends this process without running t.Cleanup.
// Elsewhere the name is d1 and the start time. Both are d1 plus hex digits, the
// shape the D1 evidence gate pins.
func d1Project() string {
	prefix := "d1"
	if runner := os.Getenv("RUNNER_NAME"); runner != "" && os.Getenv("GITHUB_ACTIONS") == "true" {
		sum := sha256.Sum256([]byte(runner))
		prefix += hex.EncodeToString(sum[:4])
	}
	return prefix + strconv.FormatInt(time.Now().UnixNano(), 16)
}

// writeOverride is the only test-side Compose document. It publishes loopback
// ports, sizes MySQL for the CI machine, and binds the refusal fixtures
// read-only; it declares no deployment semantics of its own.
func (f *d1Fixture) writeOverride() error {
	body := strings.Join([]string{
		"# Test-only Compose override for the D1 deployment acceptance.",
		"# It never restates what app/web/deploy/compose.yaml declares: loopback",
		"# test ports, a CI-sized InnoDB buffer pool, and the read-only refusal",
		"# fixtures the KEK and ownership cases point at.",
		"services:",
		"  mysql:",
		"    command:",
		"      - --innodb-buffer-pool-size=64M",
		"      - --innodb-redo-log-capacity=33554432",
		"    ports:",
		`      - "127.0.0.1::3306"`,
		"  intern:",
		"    volumes:",
		`      - "` + f.probes + `:/d1-secrets:ro"`,
		"",
	}, "\n")
	f.override = filepath.Join(f.hostDir, "override.yaml")
	return os.WriteFile(f.override, []byte(body), 0o600)
}

// teardown removes the stack, its one-off containers and every named volume it
// created, so a failed run never leaves a lock held on its PostgreSQL database. A run
// that is killed never gets here; the job runs .github/scripts/d1-compose-clean.sh
// before and after the test for that case.
func (f *d1Fixture) teardown(ctx context.Context) {
	if f.override == "" {
		return
	}
	f.composeAllow(ctx, "down", "-v", "--remove-orphans", "--timeout", "120")
	ids := f.dockerAllowLines(ctx, "ps", "-q", "--all",
		"--filter", "label=com.docker.compose.project="+f.project)
	if len(ids) > 0 {
		args := append([]string{"rm", "-f"}, ids...)
		f.dockerAllow(ctx, args...)
	}
	f.restoreOwnership(ctx)
}

// restoreOwnership hands the mounted fixture paths back to the host user, so the
// temporary directories the test registered can be removed even after the export
// tree was filled by uid 999 from inside the container. Failures are logged, not
// fatal: the acceptance outcome is already decided by the cases.
func (f *d1Fixture) restoreOwnership(ctx context.Context) {
	if f.dir == "" {
		return
	}
	mysqlImage, _ := candidateImages(f.t)
	script := "chown -R " + strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()) + " /d1"
	_, stderr, code := f.run(ctx, "docker", "run", "--rm", "--platform", "linux/amd64", "--user", "0:0",
		"--volume", f.dir+":/d1:rw", "--entrypoint", "/bin/sh", mysqlImage, "-c", script)
	if code != 0 {
		f.t.Log("restoring fixture ownership failed: " + strings.TrimSpace(stderr))
	}
}

// ---------------------------------------------------------------- host commands

// command builds one host command with the deployment environment.
func (f *d1Fixture) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = f.root
	command.Env = append(os.Environ(), f.env...)
	return command
}

// run executes one host command and returns its withheld stdout, stderr and exit
// code. A non-zero exit is a value: the refusal cases assert on it.
func (f *d1Fixture) run(ctx context.Context, name string, args ...string) (string, string, int) {
	f.t.Helper()
	command := f.command(ctx, name, args...)
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			f.fatal("%s %s: %v", name, strings.Join(args, " "), err)
		}
		code = exit.ExitCode()
	}
	return f.withhold(stdout.String()), f.withhold(stderr.String()), code
}

// docker runs one plain `docker` command and requires a zero exit.
func (f *d1Fixture) docker(ctx context.Context, args ...string) string {
	f.t.Helper()
	stdout, stderr, code := f.run(ctx, "docker", args...)
	if code != 0 {
		f.fatal("docker %s exit=%d: %s%s", strings.Join(args, " "), code, stdout, stderr)
	}
	return strings.TrimSpace(stdout)
}

// dockerAllow runs one plain `docker` command and keeps the exit code.
func (f *d1Fixture) dockerAllow(ctx context.Context, args ...string) (string, int) {
	f.t.Helper()
	stdout, stderr, code := f.run(ctx, "docker", args...)
	return strings.TrimSpace(stdout + stderr), code
}

// dockerAllowLines runs one plain `docker` command and returns its non-empty
// lines, whatever its exit code.
func (f *d1Fixture) dockerAllowLines(ctx context.Context, args ...string) []string {
	f.t.Helper()
	out, _ := f.dockerAllow(ctx, args...)
	if strings.TrimSpace(out) == "" {
		return nil
	}
	return strings.Split(strings.TrimSpace(out), "\n")
}

// composeArgs renders one `docker compose` invocation for this project: the
// repository file first, the test override second.
func (f *d1Fixture) composeArgs(args ...string) []string {
	base := make([]string, 0, 7+len(args))
	base = append(base, "compose", "--project-name", f.project, "--file", f.composeFile, "--file", f.override)
	return append(base, args...)
}

// composeAllow runs one `docker compose` command and keeps the exit code.
func (f *d1Fixture) composeAllow(ctx context.Context, args ...string) (string, int) {
	f.t.Helper()
	stdout, stderr, code := f.run(ctx, "docker", f.composeArgs(args...)...)
	return f.withhold(stdout + stderr), code
}

// composeOK runs one `docker compose` command and requires a zero exit.
func (f *d1Fixture) composeOK(ctx context.Context, args ...string) string {
	f.t.Helper()
	out, code := f.composeAllow(ctx, args...)
	if code != 0 {
		f.fatal("docker compose %s exit=%d: %s", strings.Join(args, " "), code, out)
	}
	return strings.TrimSpace(out)
}

// execIn runs one command inside a service container and returns its combined
// output with the exit code.
func (f *d1Fixture) execIn(ctx context.Context, service string, args ...string) (string, int) {
	f.t.Helper()
	full := append([]string{"exec", "-T", service}, args...)
	return f.composeAllow(ctx, full...)
}

// withhold fails when a generated credential reached any host output.
func (f *d1Fixture) withhold(text string) string {
	f.t.Helper()
	return withholdSecrets(f.t, []byte(text), f.credential...)
}

// fatal fails the running case with a message that names the command context.
// Every message passes through withhold first: command stderr and driver errors
// can quote an interpolated credential, and evidence must never carry one.
func (f *d1Fixture) fatal(format string, args ...any) {
	f.t.Helper()
	f.t.Fatal(f.withhold(fmt.Sprintf(format, args...)))
}

// report records one locked fact: it is logged for the CI gate and remembered
// for the marker cross-check that runs after every case.
func (f *d1Fixture) report(format string, args ...any) {
	f.t.Helper()
	line := fmt.Sprintf(format, args...)
	f.mark = append(f.mark, line)
	for _, marker := range d1ResultMarkers {
		if strings.HasPrefix(line, marker) {
			f.saw[marker] = true
		}
	}
}

// sawMarker reports whether one locked marker was recorded.
func (f *d1Fixture) sawMarker(marker string) bool { return f.saw[marker] }

// requireCaseSet proves the locked case set ran exactly once each: every Go
// subtest of the harness must have passed, and no case outside the locked set
// may carry a pass.
func (f *d1Fixture) requireCaseSet(t *testing.T) {
	t.Helper()
	seen := map[string]int{}
	for _, line := range f.mark {
		if !strings.HasPrefix(line, d1KeyCase) {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(line, "D1-CASE"))
		seen[name]++
	}
	for _, name := range d1Cases {
		if seen[name] != 1 {
			t.Errorf("D1 case %s ran %d times, want exactly 1", name, seen[name])
		}
		delete(seen, name)
	}
	for extra := range seen {
		t.Errorf("D1 ran a case outside the locked set: %s", extra)
	}
}

// containerID is the stack's one service container. One-off `compose run`
// containers carry com.docker.compose.oneoff=True and are excluded, so a refusal
// probe can never be mistaken for the deployed service.
func (f *d1Fixture) containerID(ctx context.Context, service string) string {
	f.t.Helper()
	ids := f.dockerAllowLines(ctx, "ps", "-q",
		"--filter", "label=com.docker.compose.project="+f.project,
		"--filter", "label=com.docker.compose.service="+service,
		"--filter", "label=com.docker.compose.oneoff=False")
	if len(ids) != 1 {
		f.fatal("service %s has %d deployed containers, want exactly 1: %v", service, len(ids), ids)
	}
	return ids[0]
}

// waitHealthy waits for one container's own healthcheck to report healthy. The
// Intern service's healthcheck is `intern-web healthcheck`, so this is the proof
// that the deployment health entry point works inside the built image.
func (f *d1Fixture) waitHealthy(ctx context.Context, service string, within time.Duration) string {
	f.t.Helper()
	deadline := time.Now().Add(within)
	id := f.containerID(ctx, service)
	for {
		status := f.docker(ctx, "inspect", "--format",
			"{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}", id)
		switch status {
		case "healthy":
			f.containers[service] = id
			return id
		case "none":
			f.fatal("%s declares no healthcheck", service)
		}
		if running := f.docker(ctx, "inspect", "--format", "{{.State.Status}}", id); running != "running" {
			logs := f.docker(ctx, "logs", "--tail", "40", id)
			f.fatal("%s is %s before it became healthy:\n%s", service, running, logs)
		}
		if time.Now().After(deadline) {
			probe := f.docker(ctx, "inspect", "--format", "{{json .State.Health}}", id)
			f.fatal("%s stayed %s for %s; health probe log: %s", service, status, within, probe)
		}
		time.Sleep(2 * time.Second)
	}
}

// publishedPort reads back one service's actual loopback host port.
func (f *d1Fixture) publishedPort(ctx context.Context, service, private string) string {
	f.t.Helper()
	// `docker compose port -q` is not supported by every Compose v2 release (the
	// CI machine rejects the shorthand), and the plain form already prints only
	// the host:port line.
	out := f.composeOK(ctx, "port", service, private)
	_, port, err := net.SplitHostPort(strings.TrimSpace(out))
	if err != nil {
		f.fatal("service %s published %q, want host:port", service, out)
	}
	if port == "" {
		f.fatal("service %s published no host port for %s", service, private)
	}
	return port
}

// prepareOwnership makes the mounted fixtures readable by the container user.
// Compose file secrets and bind mounts keep host ownership, so this is the same
// preparation an operator performs on the host: root inside the pinned image
// chowns the tree, then the deployment modes are applied.
func (f *d1Fixture) prepareOwnership(ctx context.Context) {
	f.t.Helper()
	mysqlImage, _ := candidateImages(f.t)
	script := strings.Join([]string{
		// Only the paths the stack actually mounts are handed to uid 999. The
		// fixture root itself stays owned by the host user, so a failure here can
		// never lock the test process out of its own files.
		"chown -R 999:999 /d1/secrets /d1/probes /d1/exports &&",
		"chmod 0700 /d1/secrets /d1/probes &&",
		"chmod 0600 /d1/secrets/intern_kek /d1/secrets/intern_tls_key /d1/secrets/intern_database_url /d1/secrets/intern_postgres_password /d1/probes/intern_probe_database_url &&",
		"chmod 0644 /d1/secrets/intern_tls_cert /d1/secrets/d1-ca.pem &&",
		"chmod 0644 /d1/probes/intern_kek_0644 &&",
		"chmod 0750 /d1/exports",
	}, " ")
	_, stderr, code := f.run(ctx, "docker", "run", "--rm", "--platform", "linux/amd64", "--user", "0:0",
		"--volume", f.dir+":/d1:rw", "--entrypoint", "/bin/sh", mysqlImage, "-c", script)
	if code != 0 {
		f.fatal("the mounted fixtures could not be prepared for uid 999: %s", stderr)
	}
}

// ------------------------------------------------------------------ image (A1)

// caseImage proves the image itself: pinned bases, no run-time download, the
// three tool versions against compat.json, the non-root default, the GPL
// materials, a repeatable build, and the Compose pins recomputed from the
// matrix. It then captures the candidate-artifact evidence the G gate reuses.
func (f *d1Fixture) caseImage(t *testing.T, ctx context.Context) {
	t.Helper()
	f.t = t
	defer func() { f.report(d1KeyCase+"%s", d1Cases[0]) }()
	entry := d1Compatibility(t)
	mysqlImage, pxbImage := candidateImages(t)
	f.candidateID = f.buildImage(ctx, f.candidate, f.source, false)
	// The second build bypasses the build cache entirely, so equal content is a
	// real rebuild result rather than a cache hit. Layer IDs carry build-time
	// metadata and are not expected to match; the shipped file content must.
	repeat := f.buildImage(ctx, f.candidate+"-r", f.source, true)
	f.webDigest = f.digestInImage(ctx, f.candidateID, d1WebBinary)
	repeatWeb := f.digestInImage(ctx, repeat, d1WebBinary)
	f.layers = f.imageLayers(ctx, f.candidateID)
	if !d1Hex64.MatchString(f.webDigest) || f.webDigest != repeatWeb {
		f.fatal("the two candidate builds produced different binaries: %q vs %q", f.webDigest, repeatWeb)
	}
	content := f.contentManifest(ctx, f.candidateID)
	repeatContent := f.contentManifest(ctx, repeat)
	if content != repeatContent {
		f.fatal("a no-cache rebuild shipped different file content (%d vs %d lines)",
			strings.Count(content, "\n"), strings.Count(repeatContent, "\n"))
	}
	f.report(d1KeyBuilt+"tag=%s image_id=%s repeat_image_id=%s source_sha=%s source_dirty=%t mysql_image=%s pxb_image=%s",
		f.candidate, f.candidateID, repeat, f.source, f.dirty, mysqlImage, pxbImage)
	f.report(d1KeyRepeat+"true no_cache=true content_equal=true content_files=%d layer_count=%d intern_web_sha256=%s",
		strings.Count(content, "\n"), len(f.layers), f.webDigest)
	f.checkPinnedBases(ctx, f.candidateID, mysqlImage, pxbImage)
	f.checkRuntimeDownloads(ctx)
	f.checkDefaultUser(ctx, f.candidateID)
	versions := f.toolVersions(ctx, f.candidateID)
	f.checkToolVersions(versions, entry)
	f.checkGPLMaterials(ctx, entry)
	f.checkComposePins(mysqlImage, pxbImage)
	f.captureArtifact(ctx, versions, entry)
}

// buildImage builds one candidate tag from the repository Dockerfile with the
// pinned digests and the exact source revision as its provenance label.
func (f *d1Fixture) buildImage(ctx context.Context, tag, sourceLabel string, noCache bool) string {
	f.t.Helper()
	mysqlImage, pxbImage := candidateImages(f.t)
	args := []string{
		"build", "--platform", "linux/amd64",
		"--file", filepath.Join(f.root, d1DockerfilePath), "--tag", tag,
		"--build-arg", d1Assign("SOURCE_SHA", sourceLabel),
		"--build-arg", d1Assign("MYSQL_IMAGE", mysqlImage),
		"--build-arg", d1Assign("PXB_IMAGE", pxbImage),
	}
	if d1DevelopmentEmulation() {
		// The development host's container network cannot reach the module proxy;
		// this changes only the build-time transport, never the CI or runtime path.
		args = append(args, "--network", "host")
	}
	if noCache {
		args = append(args, "--no-cache")
	}
	_, stderr, code := f.run(ctx, "docker", append(args, f.root)...)
	if code != 0 {
		f.fatal("building %s failed exit=%d: %s", tag, code, stderr)
	}
	id := f.docker(ctx, "image", "inspect", "--format", "{{.Id}}", tag)
	if !strings.HasPrefix(id, "sha256:") {
		f.fatal("image %s reports id %q", tag, id)
	}
	return id
}

// contentManifest is the sorted SHA-256 list of every file the image adds on top
// of its base: the Web binary, the copied tools and libraries, and the license
// tree. Two builds of the same source must ship byte-identical content even when
// their layer metadata differs.
func (f *d1Fixture) contentManifest(ctx context.Context, image string) string {
	f.t.Helper()
	out, code := f.dockerAllow(ctx, "run", "--rm", "--platform", "linux/amd64", "--network", "none",
		"--entrypoint", "/bin/sh", image, "-c",
		"find /usr/local/bin/intern-web /usr/bin/xtrabackup /usr/bin/xbstream /usr/lib/private "+
			"/usr/lib64/libprocps.so.8.0.3 "+d1LicenseDirectory+" -type f -exec sha256sum {} + | LC_ALL=C sort")
	if code != 0 || strings.Count(out, "\n") < 10 {
		f.fatal("image %s content manifest exit=%d lines=%d", image, code, strings.Count(out, "\n"))
	}
	return out
}

// runOffline starts one short-lived container of the candidate image with no
// network namespace at all. It is the functional proof that nothing the image
// needs is fetched when it runs.
func (f *d1Fixture) runOffline(ctx context.Context, script string) string {
	f.t.Helper()
	out, code := f.dockerAllow(ctx, "run", "--rm", "--platform", "linux/amd64", "--network", "none",
		"--entrypoint", "/bin/sh", f.candidateID, "-c", script)
	if code != 0 {
		f.fatal("the image refused to run offline exit=%d: %s", code, out)
	}
	return out
}

// digestInImage is the sha256sum one image reports for one path.
func (f *d1Fixture) digestInImage(ctx context.Context, image, path string) string {
	f.t.Helper()
	out, code := f.dockerAllow(ctx, "run", "--rm", "--platform", "linux/amd64", "--network", "none",
		"--entrypoint", "/bin/sh", image, "-c", "sha256sum "+path)
	fields := strings.Fields(out)
	if code != 0 || len(fields) != 2 || !d1Hex64.MatchString(fields[0]) {
		f.fatal("sha256sum %s in %s exit=%d reported %q", path, image, code, out)
	}
	return fields[0]
}

// imageLayers is the ordered rootFS layer list of one image.
func (f *d1Fixture) imageLayers(ctx context.Context, image string) []string {
	f.t.Helper()
	out := f.docker(ctx, "image", "inspect", "--format", "{{range .RootFS.Layers}}{{.}} {{end}}", image)
	layers := strings.Fields(out)
	if len(layers) == 0 {
		f.fatal("image %s reports no layers", image)
	}
	return layers
}

// checkPinnedBases requires the image to record every base by digest and to name
// the exact compatibility-matrix images it was built against. A moving tag would
// prove nothing about repeatability.
func (f *d1Fixture) checkPinnedBases(ctx context.Context, image, mysqlImage, pxbImage string) {
	f.t.Helper()
	labels := f.imageLabels(ctx, image)
	for key, want := range map[string]string{
		"io.github.flyingeirc.intern.mysql-image": mysqlImage,
		"io.github.flyingeirc.intern.pxb-image":   pxbImage,
		"org.opencontainers.image.base.name":      mysqlImage,
	} {
		got, ok := labels[key]
		if !ok {
			f.fatal("image %s does not label %s", image, key)
		}
		if got != want || !d1LabelImage.MatchString(got) {
			f.fatal("label %s = %q, want the pinned digest %q", key, got, want)
		}
	}
	goImage, ok := labels["io.github.flyingeirc.intern.go-image"]
	if !ok || !d1LabelImage.MatchString(goImage) {
		f.fatal("the Go toolchain is not pinned by digest: %q", goImage)
	}
	if labels["org.opencontainers.image.revision"] != f.source {
		f.fatal("revision label %q is not the candidate source %q", labels["org.opencontainers.image.revision"], f.source)
	}
	f.report(d1KeyBases+"true go_image=%s licenses_label=%s revision=%s",
		goImage, labels["io.github.flyingeirc.intern.licenses"], f.source)
}

// imageLabels reads one image's label map back from the daemon.
func (f *d1Fixture) imageLabels(ctx context.Context, image string) map[string]string {
	f.t.Helper()
	raw := f.docker(ctx, "image", "inspect", "--format", "{{json .Config.Labels}}", image)
	labels := map[string]string{}
	require(f.t, json.Unmarshal([]byte(raw), &labels))
	return labels
}

// checkRuntimeDownloads compares the final image's history with the history of
// its base: everything the base does not already contain was added by the
// deployment Dockerfile, and none of it may fetch code or packages. The same
// claim is then proved functionally with no network namespace.
func (f *d1Fixture) checkRuntimeDownloads(ctx context.Context) {
	f.t.Helper()
	ours := f.ownHistory(ctx)
	for _, line := range ours {
		if d1InstallPattern.MatchString(line) {
			f.fatal("the runtime stage downloads or installs in %q", line)
		}
	}
	script := strings.Join([]string{
		"xtrabackup --version >/dev/null", "xbstream --version >/dev/null",
		"mysqld --version >/dev/null", "intern-web --help >/dev/null", "echo offline-ok",
	}, " && ")
	if !strings.Contains(f.runOffline(ctx, script), "offline-ok") {
		f.fatal("the image needs the network to run its own tools")
	}
	f.report(d1KeyDownloads+"0 added_commands=%d network_none=true tools=xtrabackup,xbstream,mysqld,intern-web",
		len(ours))
}

// ownHistory is the history entries the deployment Dockerfile added on top of
// its base image, newest first.
func (f *d1Fixture) ownHistory(ctx context.Context) []string {
	f.t.Helper()
	mysqlImage, _ := candidateImages(f.t)
	// BuildKit keeps the pinned base in its own cache, so the daemon store may not
	// have it; `docker history` then fails on CI even though the build succeeded.
	// Pull the exact digest first, and require it to be the same digest the matrix
	// pins, so the history comparison below is against the base we claim to use.
	pulled := f.docker(ctx, "pull", "--platform", "linux/amd64", mysqlImage)
	if !strings.Contains(pulled, mysqlImage) {
		f.fatal("pulling the pinned MySQL base %s did not confirm the digest:\n%s", mysqlImage, pulled)
	}
	final := f.historyLines(ctx, f.candidateID)
	base := f.historyLines(ctx, mysqlImage)
	if len(base) == 0 {
		f.fatal("the pinned MySQL image reports no history")
	}
	for index, line := range final {
		if line == base[0] {
			if index == 0 {
				f.fatal("the candidate image adds no layer of its own")
			}
			return final[:index]
		}
	}
	f.fatal("the candidate image does not contain the pinned MySQL base history")
	return nil
}

// historyLines is one image's full CreatedBy list, one entry per line, newest
// first. The JSON format keeps a multi-line RUN command in one entry.
func (f *d1Fixture) historyLines(ctx context.Context, image string) []string {
	f.t.Helper()
	out := f.docker(ctx, "history", "--no-trunc", "--format", "{{json .CreatedBy}}", image)
	lines := strings.Split(out, "\n")
	history := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry string
		require(f.t, json.Unmarshal([]byte(line), &entry))
		history = append(history, strings.Join(strings.Fields(entry), " "))
	}
	return history
}

// checkDefaultUser proves both the image configuration and a real container's
// effective identity, because the deployment must read the mysql datadir.
func (f *d1Fixture) checkDefaultUser(ctx context.Context, image string) {
	f.t.Helper()
	configured := f.docker(ctx, "image", "inspect", "--format", "{{.Config.User}}", image)
	if configured != "999:999" {
		f.fatal("the image default user is %q, want 999:999", configured)
	}
	out := f.runOffline(ctx, "id -u; id -g; cat /proc/1/comm")
	fields := strings.Fields(out)
	if len(fields) != 3 || fields[0] != "999" || fields[1] != "999" {
		f.fatal("a default container does not run as 999:999: %q", out)
	}
	entry := f.docker(ctx, "image", "inspect", "--format", "{{json .Config.Entrypoint}}", image)
	if !strings.Contains(entry, d1WebBinary) || strings.Contains(entry, "/bin/sh") {
		f.fatal("the ENTRYPOINT is not the binary itself: %s", entry)
	}
	f.report(d1KeyUser+"true image_user=%s container_uid=999 container_gid=999 entrypoint=%s",
		configured, entry)
}

// toolVersions captures the three version strings exactly as the binaries report
// them inside the built image.
func (f *d1Fixture) toolVersions(ctx context.Context, image string) map[string]string {
	f.t.Helper()
	versions := map[string]string{}
	for name, path := range map[string]string{"mysqld": d1MysqldBinary, "xtrabackup": d1XtrabackupBinary, "xbstream": d1XbstreamBinary} {
		out, code := f.dockerAllow(ctx, "run", "--rm", "--platform", "linux/amd64", "--network", "none",
			"--entrypoint", "/bin/sh", image, "-c", path+" --version 2>&1 | head -n 2")
		match := d1VersionLine.FindStringSubmatch(out)
		if code != 0 || len(match) != 3 {
			f.fatal("%s --version exit=%d reported no version: %q", name, code, out)
		}
		versions[name] = match[2]
		versions[name+"_line"] = strings.Join(strings.Fields(out), " ")
	}
	return versions
}

// checkToolVersions requires the three versions to equal the compatibility
// matrix, so an image can never carry a tool this project has not verified.
func (f *d1Fixture) checkToolVersions(versions map[string]string, entry physical.CompatibilityEntry) {
	f.t.Helper()
	if versions["mysqld"] != entry.ServerVersion {
		f.fatal("mysqld %s, want the matrix server %s", versions["mysqld"], entry.ServerVersion)
	}
	for _, tool := range []string{"xtrabackup", "xbstream"} {
		if versions[tool] != entry.ToolVersion {
			f.fatal("%s %s, want the matrix tool %s", tool, versions[tool], entry.ToolVersion)
		}
	}
	f.report(d1KeyVersions+"mysqld=%s xtrabackup=%s xbstream=%s matrix_server=%s matrix_tool=%s match=true",
		versions["mysqld"], versions["xtrabackup"], versions["xbstream"], entry.ServerVersion, entry.ToolVersion)
}

// d1LicenseFiles is the locked license material set the image must carry: the
// Web notice, the source-provision statement, the build-time provenance record,
// the license text of every redistributed component, the procps-ng texts for the
// copied libprocps, and the manifest of Go modules linked into intern-web.
var d1LicenseFiles = []string{
	"NOTICE-WEB.txt", "GPL-SOURCE-OFFER.txt", "VERSIONS.txt",
	"mysql-community-server/LICENSE", "percona-xtrabackup/COPYING",
	"procps-ng/COPYING", "procps-ng/COPYING.LIB", "go-internals/GO-MODULES.txt",
}

func (f *d1Fixture) checkGPLMaterials(ctx context.Context, entry physical.CompatibilityEntry) {
	f.t.Helper()
	checks := make([]string, 0, len(d1LicenseFiles)+4)
	for _, name := range d1LicenseFiles {
		checks = append(checks, "test -s "+d1LicenseDirectory+"/"+name)
	}
	checks = append(checks,
		"grep -ci 'gnu general public license' "+d1LicenseDirectory+"/mysql-community-server/LICENSE",
		"grep -ci 'gnu general public license' "+d1LicenseDirectory+"/percona-xtrabackup/COPYING",
		"grep -ci 'source' "+d1LicenseDirectory+"/GPL-SOURCE-OFFER.txt",
		"grep -c 'intern_source_sha="+f.source+"' "+d1LicenseDirectory+"/VERSIONS.txt",
		// libprocps.so.8 ships without belonging to a package here, so its own
		// license texts must be present and named by the Web notice. The pinned
		// image carries the GNU Library General Public License v2 text.
		"grep -ci 'library general public license' "+d1LicenseDirectory+"/procps-ng/COPYING.LIB",
		"grep -ci 'libprocps' "+d1LicenseDirectory+"/NOTICE-WEB.txt",
		// The manifest lists one line per linked module, and the shipped tree must
		// carry a license text for each of them (counted, not reconstructed).
		"awk 'NF>=3 {n++} END {print n+0}' "+d1LicenseDirectory+"/go-internals/GO-MODULES.txt",
		"find "+d1LicenseDirectory+"/go-internals/go -mindepth 2 -type f | wc -l",
		// The provenance record enumerates each copied shared object with a digest.
		"grep -c '^copied /usr/lib' "+d1LicenseDirectory+"/VERSIONS.txt",
	)
	out := f.runOffline(ctx, strings.Join(checks, " && "))
	// The `test -s` presence checks print nothing; every other check prints one
	// count, in order. A missing file fails the whole && chain above.
	counts := strings.Fields(out)
	if want := len(checks) - len(d1LicenseFiles); len(counts) != want {
		f.fatal("the license material probe reported %q, want %d values", out, want)
	}
	for index, value := range counts {
		count, err := strconv.Atoi(value)
		require(f.t, err)
		if count == 0 {
			f.fatal("license material check %d found nothing", index)
		}
	}
	if counts[6] != counts[7] {
		f.fatal("GO-MODULES.txt lists %s modules but %s license texts ship with them", counts[6], counts[7])
	}
	f.report(d1KeyGPL+"files=%d notice=true offer=true versions=true mysql_gpl=%s pxb_gpl=%s source_sha_bound=true procps_lgpl=%s procps_named=%s go_modules=%s go_license_texts=%s copied_objects=%s matrix=%s,%s",
		len(d1LicenseFiles), counts[0], counts[1], counts[4], counts[5], counts[6], counts[7], counts[8],
		entry.ServerVersion, entry.ToolVersion)
}

// checkComposePins recomputes the compatibility matrix pins from the repository
// files, so the Compose example, its environment sample and the Dockerfile can
// never drift from internal/physical/compat.json without failing here.
func (f *d1Fixture) checkComposePins(mysqlImage, pxbImage string) {
	f.t.Helper()
	compose := f.readRepositoryFile(d1ComposePath)
	example := f.readRepositoryFile(d1EnvExamplePath)
	dockerfile := f.readRepositoryFile(d1DockerfilePath)
	for _, pin := range []string{mysqlImage, pxbImage} {
		if !strings.Contains(compose, pin) {
			f.fatal("%s does not pin %s", d1ComposePath, pin)
		}
		if !strings.Contains(dockerfile, pin) {
			f.fatal("%s does not pin %s", d1DockerfilePath, pin)
		}
		if !strings.Contains(example, pin) {
			f.fatal("%s does not pin %s", d1EnvExamplePath, pin)
		}
	}
	for _, required := range []string{pgtest.Image, "intern-postgres:", "intern_database_url:", "intern_postgres_password:", "INTERN_DATABASE_URL_FILE"} {
		if !strings.Contains(compose, required) {
			f.fatal("%s lost the PostgreSQL deployment contract %s", d1ComposePath, required)
		}
	}
	if !strings.Contains(compose, "stop_grace_period") || !strings.Contains(compose, "blkio_config") {
		f.fatal("%s lost the stop window or the I/O isolation the design requires", d1ComposePath)
	}
	f.report(d1KeyPins+"true compose=%s env_example=%s dockerfile=%s matrix_images=2 postgres_pin=true",
		d1ComposePath, d1EnvExamplePath, d1DockerfilePath)
}

// refreshAPI re-reads the published HTTPS port and starts a fresh client. The
// deployment publishes on an ephemeral host port, and Docker assigns a new one
// whenever the container is restarted, recreated or started again, so every such
// step must re-resolve the API address (and log in again: the cookie jar is keyed
// by host and port).
func (f *d1Fixture) refreshAPI(ctx context.Context) {
	f.t.Helper()
	f.apiBase = "https://127.0.0.1:" + f.publishedPort(ctx, "intern", "8443")
	f.newAPIClient()
}

// readRepositoryFile reads one repository file the deployment must keep in sync
// with the compatibility matrix.
func (f *d1Fixture) readRepositoryFile(path string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, path))
	require(f.t, err)
	return string(data)
}

// d1Compatibility is the single linux/amd64 MySQL Community matrix entry.
func d1Compatibility(t *testing.T) physical.CompatibilityEntry {
	t.Helper()
	entries, err := physical.CompatibilityMatrix()
	require(t, err)
	for _, entry := range entries {
		if entry.Distribution == "mysql-community" && entry.Platform == "linux/amd64" {
			return entry
		}
	}
	t.Fatal("compatibility matrix has no linux/amd64 MySQL Community entry")
	return physical.CompatibilityEntry{}
}

// ------------------------------------------------------------------- HTTPS client

// newAPIClient trusts exactly the test CA the deployment presents. The harness
// verifies the server certificate rather than disabling verification, so "the
// HTTPS API answers" means a real authenticated TLS session with the deployed
// listener, and a client that does not trust it is refused.
func (f *d1Fixture) newAPIClient() {
	f.t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(f.caPEM) {
		f.fatal("the test CA could not be added to a trust pool")
	}
	jar, err := cookiejar.New(nil)
	require(f.t, err)
	f.http = &http.Client{
		Timeout: 60 * time.Second, Jar: jar,
		Transport: &http.Transport{
			Proxy: nil, ForceAttemptHTTP2: true,
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// apiIssue performs one request against the deployment and returns the status,
// the withheld body and the negotiated TLS version. State-changing requests
// carry the same-origin header the API requires and, when a key is given, the
// idempotency header the contract declares.
func (f *d1Fixture) apiIssue(method, path, body, idempotency string) (int, string, string) {
	f.t.Helper()
	if f.http == nil {
		f.fatal("the API client was not created before %s %s; the case that starts the stack must call newAPIClient first", method, path)
	}
	request, err := http.NewRequestWithContext(context.Background(), method, f.apiBase+path, strings.NewReader(body))
	require(f.t, err)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet && method != http.MethodHead {
		request.Header.Set("Origin", f.apiBase)
	}
	if idempotency != "" {
		request.Header.Set("Idempotency-Key", idempotency)
	}
	response, err := f.http.Do(request)
	if err != nil {
		return 0, f.withhold(err.Error()), ""
	}
	defer func() { _ = response.Body.Close() }() //nolint:errcheck // the caller already has the status
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	require(f.t, err)
	version := ""
	if response.TLS != nil {
		version = tlsVersionName(response.TLS.Version)
	}
	return response.StatusCode, f.withhold(string(raw)), version
}

// api requires one exact status and returns the body.
func (f *d1Fixture) api(method, path, body, idempotency string, want int) string {
	f.t.Helper()
	code, raw, _ := f.apiIssue(method, path, body, idempotency)
	if code != want {
		f.fatal("%s %s answered %d want %d: %s", method, path, code, want, raw)
	}
	return raw
}

// tlsVersionName names the negotiated TLS version, so the marker records what
// the deployment really offered instead of what was configured.
func tlsVersionName(version uint16) string {
	switch version {
	case tls.VersionTLS12:
		return "1.2"
	case tls.VersionTLS13:
		return "1.3"
	default:
		return "unknown"
	}
}

// createdID decodes the `{"id":...}` answer every resource POST returns.
func (f *d1Fixture) createdID(raw string) string {
	f.t.Helper()
	var created struct {
		ID string `json:"id"`
	}
	f.decode(raw, &created)
	if !d1UUID.MatchString(created.ID) {
		f.fatal("created resource id %q is not a UUIDv7", created.ID)
	}
	return created.ID
}

// decode parses one response body.
func (f *d1Fixture) decode(raw string, target any) {
	f.t.Helper()
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		f.fatal("cannot decode %q: %v", raw, err)
	}
}

// login authenticates one account and reports the negotiated TLS version.
func (f *d1Fixture) login(username, password string) string {
	f.t.Helper()
	body, err := json.Marshal(map[string]string{"username": username, "password": password})
	require(f.t, err)
	code, raw, version := f.apiIssue(http.MethodPost, "/api/v1/auth/login", string(body), "")
	if code != http.StatusOK {
		f.fatal("login for %s answered %d: %s", username, code, raw)
	}
	return version
}

// readyz answers the readiness route, which is the migration gate: a database
// that is not at the exact migration version answers 503.
func (f *d1Fixture) readyz() (int, string) {
	f.t.Helper()
	code, raw, _ := f.apiIssue(http.MethodGet, "/readyz", "", "")
	return code, raw
}

// ------------------------------------------------------------------- wire types

// d1Run is the run row the API serves. Only the fields the deployment evidence
// reads are decoded; the API shape stays owned by the server.
type d1Run struct {
	ID          string  `json:"id"`
	Kind        string  `json:"kind"`
	Status      string  `json:"status"`
	BackupID    *string `json:"backup_id"`
	ErrorClass  *string `json:"error_class"`
	ErrorCode   *string `json:"error_code"`
	PayloadSize *int64  `json:"payload_size"`
}

// d1Restore is the restore row the API serves.
type d1Restore struct {
	ID           string  `json:"id"`
	BackupID     string  `json:"backup_id"`
	Status       string  `json:"status"`
	ExportPath   *string `json:"export_path"`
	ExportBytes  *int64  `json:"export_bytes"`
	ErrorClass   *string `json:"error_class"`
	ErrorCode    *string `json:"error_code"`
	VerifyStatus string  `json:"verify_status"`
}

// d1Plan is the read-only restore plan the API builds from real state.
type d1Plan struct {
	BackupID           string   `json:"backup_id"`
	Digest             string   `json:"plan_digest"`
	ExportPath         string   `json:"export_path"`
	Risks              []string `json:"risks"`
	RehearsalAvailable bool     `json:"rehearsal_available"`
	Links              []struct {
		BackupID string `json:"backup_id"`
		Seq      int64  `json:"seq"`
		Source   string `json:"source"`
	} `json:"links"`
}

// d1Backup is one catalog member as the API serves it.
type d1Backup struct {
	ID            string `json:"id"`
	ParentID      string `json:"parent_id"`
	Kind          string `json:"kind"`
	Status        string `json:"status"`
	Seq           int    `json:"seq"`
	PayloadSize   int64  `json:"payload_size"`
	PayloadSHA256 string `json:"payload_sha256"`
	LocalPresent  bool   `json:"local_present"`
}

// d1Chain is one chain with its members.
type d1Chain struct {
	ID         string     `json:"id"`
	ServerUUID string     `json:"server_uuid"`
	SealedAt   string     `json:"sealed_at"`
	Members    []d1Backup `json:"members"`
}

// d1Migrate is the migration status `intern-web migrate status` prints.
type d1Migrate struct {
	CurrentVersion            int64  `json:"current_version"`
	LatestVersion             int64  `json:"latest_version"`
	PendingMigrations         int    `json:"pending_migrations"`
	InstanceID                string `json:"instance_id"`
	PhysicalPendingMigrations int    `json:"physical_pending_migrations"`
	ReplicationPending        int    `json:"replication_pending_migrations"`
}

// d1Preflight is the source preflight answer, including the identity the server
// observed over the shared socket.
type d1Preflight struct {
	OK     bool     `json:"ok"`
	Checks []string `json:"checks"`
	Error  *struct {
		Check   string `json:"check"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// d1Certificate writes the test CA and the server certificate the deployment
// mounts as file secrets, and returns the CA PEM the client trusts. The
// certificate is valid for localhost and 127.0.0.1, which is what a same-host
// deployment uses.
func d1Certificate(t *testing.T, directory string) []byte {
	t.Helper()
	serial := func() *big.Int {
		value, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
		require(t, err)
		return value
	}
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require(t, err)
	ca := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "Intern D1 test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	require(t, err)
	leaf := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:    []string{"localhost"},
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require(t, err)
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, caKey)
	require(t, err)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	require(t, err)
	require(t, os.WriteFile(filepath.Join(directory, "intern_tls_cert"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), 0o644))
	require(t, os.WriteFile(filepath.Join(directory, "intern_tls_key"),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))
	require(t, os.WriteFile(filepath.Join(directory, "d1-ca.pem"), caPEM, 0o644))
	return caPEM
}

// --------------------------------------------------------------- compose (A2)

// caseCompose brings the repository Compose file up with real `docker compose`
// and proves the stack itself: readiness of PostgreSQL, MySQL and the deployment,
// the one-time setup over HTTPS, a verified session, the read-only datadir, the
// shared socket, and the file secrets that are actually read.
func (f *d1Fixture) caseCompose(t *testing.T, ctx context.Context) {
	t.Helper()
	f.t = t
	defer func() { f.report(d1KeyCase+"%s", d1Cases[1]) }()
	f.prepareOwnership(ctx)
	f.composeOK(ctx, "up", "--detach")
	mysqlID := f.waitHealthy(ctx, "mysql", 4*time.Minute)
	postgresID := f.waitHealthy(ctx, "postgres", 4*time.Minute)
	internID := f.waitHealthy(ctx, "intern", 4*time.Minute)
	// `docker compose port` takes the bare private port; the protocol defaults to tcp.
	mysqlPort := f.publishedPort(ctx, "mysql", "3306")
	apiPort := f.publishedPort(ctx, "intern", "8443")
	f.mysqlPort, _ = strconv.Atoi(mysqlPort)
	f.apiBase = "https://127.0.0.1:" + apiPort
	f.newAPIClient()
	f.requireHealth(t)
	f.seedDatabase(ctx)
	f.createUser(t)
	f.requireTransportFacts(t)
	f.requireReadOnlyDataDir(t, ctx)
	f.requireSecretsRead(t, ctx, mysqlID, internID)
	f.requireSocketAndKEK(t, ctx)
	f.report(d1KeyIdentity+"daemon=%s platform=linux/amd64 compose=%s project=%s",
		f.daemonPlatform(ctx), f.composeVersion(ctx), f.project)
	f.report(d1KeyStack+"healthy compose_file=%s project=%s mysql=%s intern=%s api=%s mysql_port=%d healthcheck=intern-web postgres=%s",
		d1ComposePath, f.project, mysqlID, internID, f.apiBase, f.mysqlPort, postgresID)
}

// requireHealth proves both unauthenticated routes answer over the deployed
// listener. /readyz is the migration gate: it answers 503 until the PostgreSQL
// control plane is at the exact migration version.
func (f *d1Fixture) requireHealth(t *testing.T) {
	t.Helper()
	code, body, _ := f.apiIssue(http.MethodGet, "/healthz", "", "")
	if code != http.StatusOK || !strings.Contains(body, `"status":"ok"`) {
		f.fatal("/healthz answered %d: %s", code, body)
	}
	for attempt := 0; attempt < 60; attempt++ {
		code, body, _ = f.apiIssue(http.MethodGet, "/readyz", "", "")
		if code == http.StatusOK {
			return
		}
		time.Sleep(2 * time.Second)
	}
	f.fatal("/readyz never became ready, last answer %d: %s", code, body)
}

// createUser bootstraps the first administrator through the public setup API.
func (f *d1Fixture) createUser(t *testing.T) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"username": d1AdminUsername, "password": f.adminPass})
	require(t, err)
	code, raw, version := f.apiIssue(http.MethodPost, "/api/v1/auth/setup", string(body), "")
	var result struct {
		User struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"user"`
	}
	f.decode(raw, &result)
	if code != http.StatusCreated || !d1UUID.MatchString(result.User.ID) || result.User.Username != d1AdminUsername || result.User.Role != "admin" {
		f.fatal("setup returned no first administrator: status=%d body=%s", code, raw)
	}
	f.api(http.MethodGet, "/api/v1/auth/session", "", "", http.StatusOK)
	if code, _, _ := f.apiIssue(http.MethodPost, "/api/v1/auth/setup", string(body), ""); code != http.StatusConflict {
		f.fatal("completed setup accepted a second bootstrap: status=%d", code)
	}
	if version != "1.3" && version != "1.2" {
		f.fatal("the administrator session did not negotiate TLS: %q", version)
	}
	f.report(d1KeyAdmin+"created user=%s status=201 session=200 negotiated=%s once=true", d1AdminUsername, version)
}

// requireTransportFacts proves the listener really terminates TLS: a client that
// does not trust the deployment CA is refused, and a plaintext request to the
// same port is not served.
func (f *d1Fixture) requireTransportFacts(t *testing.T) {
	t.Helper()
	plain, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		strings.Replace(f.apiBase, "https://", "http://", 1)+"/healthz", nil)
	require(t, err)
	// Go's TLS server answers a plaintext HTTP request on the TLS port with a fixed
	// 400 "Client sent an HTTP request to an HTTPS server" and serves nothing else;
	// any outcome other than that refusal or a transport error means the port
	// served HTTP without TLS.
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(plain)
	if err == nil {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		_ = response.Body.Close() //nolint:errcheck // the status and body already decide the outcome
		if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "HTTP request to an HTTPS server") {
			f.fatal("the TLS listener answered a plaintext request with %d %q", response.StatusCode, string(raw))
		}
	}
	untrusted := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: x509.NewCertPool()},
	}}
	_, err = untrusted.Get(f.apiBase + "/healthz")
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		f.fatal("an untrusted certificate was accepted: %v", err)
	}
	f.report(d1KeyTLS + "verified plaintext=refused untrusted_ca=refused")
}

// requireReadOnlyDataDir attempts the write the design forbids: xtrabackup must
// never touch the source datadir. The refusal is the kernel's, not a wrapper's.
func (f *d1Fixture) requireReadOnlyDataDir(t *testing.T, ctx context.Context) {
	t.Helper()
	out, code := f.execIn(ctx, "intern", "touch", filepath.Join(d1DataDirPath, ".d1-must-not-exist"))
	if code == 0 || !strings.Contains(strings.ToLower(out), "read-only file system") {
		f.fatal("the datadir mount accepted a write (exit=%d): %s", code, out)
	}
	out, code = f.execIn(ctx, "intern", "stat", "-c", "%a %u %g", d1MetadataPath)
	if code != 0 || !strings.HasPrefix(strings.TrimSpace(out), "700 999 999") {
		f.fatal("the metadata directory is not a private directory owned by the deployment user: %s", out)
	}
	f.report(d1KeyDataDir + "refused metadata_dir=0700 owner=999:999")
}

// requireSecretsRead proves the Compose file secrets really are the files the
// harness wrote: the KEK and PostgreSQL URL are read inside the container and
// digested there, and each digest equals its host file. The modes are read back
// from the container too, because the KEK and DSN readers refuse a world-readable
// file.
func (f *d1Fixture) requireSecretsRead(t *testing.T, ctx context.Context, mysqlID, internID string) {
	t.Helper()
	out, code := f.execIn(ctx, "intern", "stat", "-c", "%a %u %g", d1SecretKEK)
	if code != 0 || !strings.HasPrefix(strings.TrimSpace(out), "600 999 999") {
		f.fatal("the KEK file secret is not a private file owned by the deployment user: %s", out)
	}
	out, code = f.execIn(ctx, "intern", "sha256sum", d1SecretKEK)
	if code != 0 {
		f.fatal("the container cannot read the KEK secret: %s", out)
	}
	container := strings.Fields(out)[0]
	host := f.kekDigest
	if container != host {
		f.fatal("the KEK in the container is %s, the host secret is %s", container, host)
	}
	f.requireDatabaseURLSecret(t, ctx)
	out, code = f.execIn(ctx, "intern", "ls", d1LicenseDirectory)
	if code != 0 {
		f.fatal("the running container cannot see the GPL materials: %s", out)
	}
	user := f.docker(ctx, "inspect", "--format", "{{.Config.User}}", internID)
	if user != "999:999" {
		f.fatal("the deployed container runs as %q, want 999:999", user)
	}
	f.report(d1KeyKEK+"secret=%s mode=0600 owner=999:999 digest_bound=true database_url_secret=0600 database_url_digest_bound=true container_user=%s mysql=%s",
		d1SecretKEK, user, mysqlID)
}

// requireDatabaseURLSecret proves the mounted PostgreSQL DSN is the host's
// private file and that its digest reaches the container unchanged.
func (f *d1Fixture) requireDatabaseURLSecret(t *testing.T, ctx context.Context) {
	t.Helper()
	out, code := f.execIn(ctx, "intern", "stat", "-c", "%a %u %g", "/run/secrets/intern_database_url")
	if code != 0 || !strings.HasPrefix(strings.TrimSpace(out), "600 999 999") {
		f.fatal("the PostgreSQL URL secret is not private: %s", out)
	}
	out, code = f.execIn(ctx, "intern", "sha256sum", "/run/secrets/intern_database_url")
	if code != 0 || strings.Fields(out)[0] != f.databaseDigest {
		f.fatal("the deployed PostgreSQL URL secret differs from the host secret: %s", out)
	}
}

// requireSocketAndKEK performs the two operations that need both: a source row
// whose password is sealed with the file-secret KEK, and a real preflight that
// connects through the shared socket. Without the KEK the first answer is 503;
// without the socket the preflight fails.
func (f *d1Fixture) requireSocketAndKEK(t *testing.T, ctx context.Context) {
	t.Helper()
	f.repositoryID = f.createdID(f.api(http.MethodPost, "/api/v1/repositories",
		`{"name":"d1-local","kind":"local","tls_mode":"none","root":"`+d1BackupsPath+`"}`, "", http.StatusCreated))
	f.sourceID = f.createdID(f.api(http.MethodPost, "/api/v1/sources", fmt.Sprintf(
		`{"name":"d1-source","socket":%q,"datadir":%q,"username":%q,"password":%q}`,
		d1SocketPath, d1DataDirPath, d1BackupUser, f.sourcePass), "", http.StatusCreated))
	body := f.api(http.MethodPost, "/api/v1/sources/"+f.sourceID+"/preflight",
		`{"local_repository_id":"`+f.repositoryID+`"}`, "", http.StatusOK)
	var outcome d1Preflight
	f.decode(body, &outcome)
	if !outcome.OK || outcome.Error != nil {
		f.fatal("the source preflight did not pass: %s", body)
	}
	if !containsAll(outcome.Checks, "identity", "compatibility", "volume", "mount", "privileges", "capacity") {
		f.fatal("the preflight did not run the locked checks: %v", outcome.Checks)
	}
	f.report(d1KeySocket+"preflight_passed checks=%d source=%s repository=%s sealed=true",
		len(outcome.Checks), f.sourceID, f.repositoryID)
}

// containsAll reports whether every required check name is present.
func containsAll(checks []string, required ...string) bool {
	for _, want := range required {
		found := false
		for _, name := range checks {
			if strings.Contains(name, want) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// d1FileDigest is the sha256 of one host file.
func d1FileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }() //nolint:errcheck // the digest already answered or failed
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// ---------------------------------------------------------------- business (A3)

// caseBusiness closes the loop inside the deployed stack: a local repository, a
// source, a policy, a real full backup, a real incremental, and a restore that
// publishes its export into the Compose bind mount. The export is then started
// by a brand-new pinned MySQL 8.4.11 container, and the data snapshot taken from
// that container must equal the snapshot of the source at that restore point.
func (f *d1Fixture) caseBusiness(t *testing.T, ctx context.Context) {
	t.Helper()
	f.t = t
	defer func() { f.report(d1KeyCase+"%s", d1Cases[2]) }()
	f.policyID = f.createPolicy(t, "d1-policy")
	f.report(d1KeyTopo+"compose-mysql->intern-web->local-repository policy=%s source=%s repository=%s replica=none",
		f.policyID, f.sourceID, f.repositoryID)
	f.fullBackup = f.backup(t, ctx, "full", "d1-a3-full")
	f.seedRows(t, ctx, 4)
	f.incBackup = f.backup(t, ctx, "incremental", "d1-a3-inc")
	f.requireChain(t, f.fullBackup, f.incBackup)
	f.snapshot = f.sourceSnapshot(t, ctx)
	f.restoreID = f.restore(t, backupOptions{backupID: f.incBackup, key: "d1-a3-restore", rehearse: true})
	f.requireExport(t, ctx)
	actual := f.exportSnapshot(t, ctx)
	if actual != f.snapshot {
		f.fatal("the export started by a new MySQL 8.4.11 container holds snapshot %s, want %s", actual, f.snapshot)
	}
	f.report(d1KeyNewMySQL+"1 expected_snapshot=%s container_snapshot=%s match=true",
		f.snapshot, actual)
}

// createPolicy installs one policy through the public API and returns its id.
// The body is only the business fields the contract accepts. max_chain_length,
// parallel, throttle, zstd_level and page_tracking stay server defaults; the
// API rejects them. The SIGTERM case pins throttle afterwards with
// pinSIGTERMThrottle, a test-fixture write, not this request.
func (f *d1Fixture) createPolicy(t *testing.T, name string) string {
	t.Helper()
	// Both schedules sit half a day away from now, so the in-process scheduler can
	// never fire a run of its own during the acceptance and every run, parent and
	// counter the cases assert is one the harness triggered itself.
	quiet := (time.Now().UTC().Hour() + 12) % 24
	body := fmt.Sprintf(`{"name":%q,"source_id":%q,"local_repository_id":%q,`+
		`"full_cron":"0 %d * * *","incremental_cron":"30 %d * * *","timezone":"UTC","enabled":false}`,
		name, f.sourceID, f.repositoryID, quiet, quiet)
	return f.createdID(f.api(http.MethodPost, "/api/v1/policies", body, "", http.StatusCreated))
}

// pinSIGTERMThrottle is a D1 test fixture, not a product path, and only the
// SIGTERM case may call it. The policy API refuses throttle, so after create
// this writes throttle=1 on that one d1-throttled row. xtrabackup then receives
// --throttle=1 and the full backup stays in flight until the stop lands.
func (f *d1Fixture) pinSIGTERMThrottle(ctx context.Context, policyID string) {
	f.t.Helper()
	if !d1UUID.MatchString(policyID) {
		f.fatal("refusing to pin throttle on policy id %q", policyID)
	}
	statement := `UPDATE policies SET throttle = 1 WHERE id = '` + policyID +
		`' AND name = 'd1-throttled' RETURNING throttle`
	out, code := f.execIn(ctx, "postgres", "psql", "-U", "intern", "-d", "intern",
		"-v", "ON_ERROR_STOP=1", "-t", "-A", "-c", statement)
	// psql prints the RETURNING value and then the command tag, so only the
	// first line is the pinned value.
	pinned := strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
	if code != 0 || pinned != "1" {
		f.fatal("the SIGTERM fixture could not pin throttle=1 on %s: exit=%d %s", policyID, code, out)
	}
}

// enablePolicy turns one policy on through the API, which runs the policy
// preflight inside the deployed container before it flips the row.
func (f *d1Fixture) enablePolicy(t *testing.T, id string) {
	t.Helper()
	f.api(http.MethodPost, "/api/v1/policies/"+id+"/enable", "", "d1-enable-"+id, http.StatusOK)
}

// backup triggers one run, waits for the terminal state the deployment must
// reach, and returns the completed backup id the run published.
func (f *d1Fixture) backup(t *testing.T, ctx context.Context, kind, key string) string {
	t.Helper()
	f.enablePolicy(t, f.policyID)
	runID := f.trigger(t, kind, key)
	run := f.waitRun(t, runID, 20*time.Minute)
	if run.Status != "completed" {
		f.fatal("the %s run %s ended %s class=%s code=%s",
			kind, runID, run.Status, d1Text(run.ErrorClass), d1Text(run.ErrorCode))
	}
	if run.BackupID == nil || !d1Hex32.MatchString(*run.BackupID) {
		f.fatal("the %s run %s published no backup id", kind, runID)
	}
	backup := f.backupView(t, *run.BackupID)
	if backup.Status != "completed" || !backup.LocalPresent || backup.PayloadSize <= 0 {
		f.fatal("backup %s is not a completed local payload: %+v", *run.BackupID, backup)
	}
	f.report(d1RunKey(kind)+"run=%s backup=%s status=%s seq=%d payload_bytes=%d sha256=%s",
		runID, *run.BackupID, backup.Status, backup.Seq, d1Int(run.PayloadSize), backup.PayloadSHA256)
	return *run.BackupID
}

// trigger asks the deployed dispatcher for one manual run and returns its id.
func (f *d1Fixture) trigger(t *testing.T, kind, key string) string {
	t.Helper()
	raw := f.api(http.MethodPost, "/api/v1/policies/"+f.policyID+"/runs",
		fmt.Sprintf(`{"kind":%q}`, kind), key, http.StatusAccepted)
	var created struct {
		RunID string `json:"run_id"`
	}
	f.decode(raw, &created)
	if !d1UUID.MatchString(created.RunID) {
		f.fatal("trigger returned run_id %q", created.RunID)
	}
	return created.RunID
}

// waitRun tolerates queued and running, and returns as soon as the row reaches a
// terminal state. It never asserts a state before it is observed.
func (f *d1Fixture) waitRun(t *testing.T, id string, within time.Duration) d1Run {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		run := f.runView(t, id)
		switch run.Status {
		case "completed", "failed", "canceled":
			return run
		}
		if time.Now().After(deadline) {
			f.fatal("run %s stayed %s for %s", id, run.Status, within)
		}
		time.Sleep(2 * time.Second)
	}
}

// waitRestore is the restore equivalent of waitRun.
func (f *d1Fixture) waitRestore(t *testing.T, id string, within time.Duration) d1Restore {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		restore := f.restoreView(t, id)
		switch restore.Status {
		case "completed", "failed", "canceled":
			return restore
		}
		if time.Now().After(deadline) {
			f.fatal("restore %s stayed %s for %s", id, restore.Status, within)
		}
		time.Sleep(2 * time.Second)
	}
}

// runView, restoreView, backupView and chains read the API rows back.
func (f *d1Fixture) runView(t *testing.T, id string) d1Run {
	t.Helper()
	var run d1Run
	f.decode(f.api(http.MethodGet, "/api/v1/runs/"+id, "", "", http.StatusOK), &run)
	return run
}

func (f *d1Fixture) restoreView(t *testing.T, id string) d1Restore {
	t.Helper()
	var restore d1Restore
	f.decode(f.api(http.MethodGet, "/api/v1/restores/"+id, "", "", http.StatusOK), &restore)
	return restore
}

func (f *d1Fixture) backupView(t *testing.T, id string) d1Backup {
	t.Helper()
	var backup d1Backup
	f.decode(f.api(http.MethodGet, "/api/v1/backups/"+id, "", "", http.StatusOK), &backup)
	return backup
}

func (f *d1Fixture) chains(t *testing.T) []d1Chain {
	t.Helper()
	var page struct {
		Items []d1Chain `json:"items"`
	}
	f.decode(f.api(http.MethodGet, "/api/v1/sources/"+f.sourceID+"/chains", "", "", http.StatusOK), &page)
	return page.Items
}

// requireChain proves the two members really form one chain: the incremental has
// the full backup as its parent, and both sit in one chain of the one source.
func (f *d1Fixture) requireChain(t *testing.T, full, incremental string) {
	t.Helper()
	chains := f.chains(t)
	if len(chains) != 1 {
		f.fatal("the source has %d chains, want exactly 1", len(chains))
	}
	if len(chains[0].Members) != 2 {
		f.fatal("the chain has %d members, want the full backup and the incremental", len(chains[0].Members))
	}
	members := map[string]d1Backup{}
	for _, member := range chains[0].Members {
		members[member.ID] = member
	}
	if members[incremental].ParentID != full {
		f.fatal("the incremental %s has parent %s, want the full backup %s",
			incremental, members[incremental].ParentID, full)
	}
	f.serverUUID = chains[0].ServerUUID
	f.report(d1KeyInc+"parent=%s chain=%s server_uuid_bound=true",
		members[incremental].ParentID, chains[0].ID)
}

// backupOptions is one restore request's locked shape.
type backupOptions struct {
	backupID string
	key      string
	rehearse bool
}

// restore plans and executes one isolated restore through the API. The plan
// digest is the confirmation the execute request must carry, and the export is
// published under the Compose bind mount.
func (f *d1Fixture) restore(t *testing.T, options backupOptions) string {
	t.Helper()
	plan := f.plan(t, options.backupID)
	if !strings.Contains(plan.ExportPath, "{restore_id}") || !strings.HasPrefix(plan.ExportPath, d1ExportPath) {
		f.fatal("the plan publishes into %q, want a placeholder below %s", plan.ExportPath, d1ExportPath)
	}
	body := fmt.Sprintf(`{"backup_id":%q,"plan_digest":%q,"password":%q,"rehearse":%t,"verify_sql":[]}`,
		options.backupID, plan.Digest, f.adminPass, options.rehearse)
	raw := f.api(http.MethodPost, "/api/v1/restores", body, options.key, http.StatusAccepted)
	var accepted struct {
		RestoreID string `json:"restore_id"`
	}
	f.decode(raw, &accepted)
	if !d1Hex32.MatchString(accepted.RestoreID) {
		f.fatal("restore accepted with id %q", accepted.RestoreID)
	}
	restore := f.waitRestore(t, accepted.RestoreID, 25*time.Minute)
	if restore.Status != "completed" {
		f.fatal("restore %s ended %s class=%s code=%s", accepted.RestoreID, restore.Status,
			d1Text(restore.ErrorClass), d1Text(restore.ErrorCode))
	}
	if restore.ExportPath == nil || *restore.ExportPath == "" {
		f.fatal("the completed restore %s published no export path", accepted.RestoreID)
	}
	f.restoreExport = *restore.ExportPath
	f.report(d1KeyRestore+"%s backup=%s status=%s export=%s bytes=%d rehearsal=%t verify=%s",
		accepted.RestoreID, options.backupID, restore.Status, *restore.ExportPath,
		d1Int(restore.ExportBytes), options.rehearse, restore.VerifyStatus)
	return accepted.RestoreID
}

// plan requests the read-only restore plan for one backup.
func (f *d1Fixture) plan(t *testing.T, backupID string) d1Plan {
	t.Helper()
	var plan d1Plan
	f.decode(f.api(http.MethodPost, "/api/v1/restores/plan",
		fmt.Sprintf(`{"backup_id":%q}`, backupID), "", http.StatusOK), &plan)
	if !d1Hex64.MatchString(plan.Digest) || len(plan.Links) == 0 {
		f.fatal("the plan for %s is incomplete: %q", backupID, plan.Digest)
	}
	for _, link := range plan.Links {
		if link.Source != "local" {
			f.fatal("the plan reads link %s from %q, want the local repository", link.BackupID, link.Source)
		}
	}
	return plan
}

// requireExport proves the published export is a real directory below the host
// bind mount the deployment declared, and that it holds a MySQL datadir.
func (f *d1Fixture) requireExport(t *testing.T, ctx context.Context) {
	t.Helper()
	if !strings.HasPrefix(f.restoreExport, d1ExportPath+"/") {
		f.fatal("the export %s is not below the mounted export root", f.restoreExport)
	}
	data := filepath.Join(f.restoreExport, "data")
	out, code := f.execIn(ctx, "intern", "ls", data)
	if code != 0 || !strings.Contains(out, "ibdata1") {
		f.fatal("the exported datadir %s is not a MySQL data directory: %s", data, out)
	}
	out, code = f.execIn(ctx, "intern", "stat", "-c", "%a %u %g", data)
	if code != 0 {
		f.fatal("the exported datadir cannot be inspected: %s", out)
	}
	f.report(d1KeyExport+"%s mode_owner=%s", filepath.Base(f.restoreExport), strings.Join(strings.Fields(out), ":"))
}

// exportSnapshot starts a brand-new pinned MySQL 8.4.11 container on the host
// export directory and digests the data it finds there.
func (f *d1Fixture) exportSnapshot(t *testing.T, ctx context.Context) string {
	t.Helper()
	mysqlImage, _ := candidateImages(t)
	fresh, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		Image: mysqlImage, ImagePlatform: "linux/amd64",
		ExposedPorts: []string{"3306/tcp"},
		// The export must be the host directory the Compose file binds, so the
		// new server starts on exactly the bytes an operator would find there.
		HostConfigModifier: func(host *container.HostConfig) {
			host.Binds = []string{f.exports + ":/exports"}
		},
		Cmd: []string{
			"--datadir=/exports/" + f.restoreID + "/data", "--socket=/tmp/mysql.sock",
			"--pid-file=/tmp/mysqld.pid", "--port=3306", "--skip-log-bin", "--innodb-buffer-pool-size=64M",
		},
		ConfigModifier: func(config *container.Config) { config.User = "999:999" },
		WaitingFor:     wait.ForLog("port: 3306  MySQL Community Server").WithStartupTimeout(3 * time.Minute),
	}, Started: true})
	if fresh != nil {
		tc.CleanupContainer(t, fresh, tc.StopTimeout(10*time.Second))
	}
	require(t, err)
	db := openFixtureDB(t, ctx, fresh, "root", f.rootPass)
	defer func() { require(t, db.Close()) }()
	return p4Snapshot(t, ctx, db)
}

// d1Text and d1Int render the optional API fields the failure markers quote, so
// a refusal always names its machine code instead of an empty string.
func d1Text(value *string) string {
	if value == nil {
		return "none"
	}
	return *value
}

func d1Int(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

// --------------------------------------------------------------- lifecycle (A4)

// caseLifecycle proves that a restart, a recreate and an image replacement keep
// the deployment's own state: the users, the resources, the backups, the restore
// records and the KEK survive, the chain continues, and the migration status is
// Ready on every start.
func (f *d1Fixture) caseLifecycle(t *testing.T, ctx context.Context) {
	t.Helper()
	f.t = t
	defer func() { f.report(d1KeyCase+"%s", d1Cases[3]) }()
	volumes := f.volumeNames(ctx)
	f.composeOK(ctx, "restart", "intern")
	f.waitHealthy(ctx, "intern", 4*time.Minute)
	f.refreshAPI(ctx)
	f.requireRetained(t, ctx, "restart", volumes)
	f.composeOK(ctx, "up", "--detach", "--force-recreate", "intern")
	f.waitHealthy(ctx, "intern", 4*time.Minute)
	f.refreshAPI(ctx)
	f.requireRetained(t, ctx, "recreate", volumes)
	f.upgradeImage(t, ctx)
}

// volumeNames reads back the named volumes the stack actually created, so the
// retention claim is about those volumes and not about a directory the harness
// made up.
func (f *d1Fixture) volumeNames(ctx context.Context) []string {
	f.t.Helper()
	names := f.dockerAllowLines(ctx, "volume", "ls", "--filter", "name="+f.project, "--format", "{{.Name}}")
	wanted := []string{"intern-postgres", "intern-metadata", "intern-backups", "intern-work", "mysql-data", "mysql-run"}
	for _, want := range wanted {
		found := false
		for _, name := range names {
			if strings.HasSuffix(name, want) {
				found = true
				break
			}
		}
		if !found {
			f.fatal("the stack never created a volume ending in %s: %v", want, names)
		}
	}
	return names
}

// requireRetained is the shared lifecycle assertion: the same administrator, the
// same rows, the same instance, a Ready migration, and one more incremental that
// the running deployment can still complete.
func (f *d1Fixture) requireRetained(t *testing.T, ctx context.Context, label string, volumes []string) {
	t.Helper()
	version := f.login(d1AdminUsername, f.adminPass)
	for _, path := range []string{
		"/api/v1/repositories/" + f.repositoryID,
		"/api/v1/sources/" + f.sourceID,
		"/api/v1/policies/" + f.policyID,
		"/api/v1/backups/" + f.fullBackup,
		"/api/v1/backups/" + f.incBackup,
		"/api/v1/restores/" + f.restoreID,
	} {
		if code, _, _ := f.apiIssue(http.MethodGet, path, "", ""); code != http.StatusOK {
			f.fatal("after %s the deployment lost %s (status %d)", label, path, code)
		}
	}
	if code, _ := f.readyz(); code != http.StatusOK {
		f.fatal("after %s /readyz answered %d", label, code)
	}
	status := f.migrateStatus(ctx)
	f.requireMigrationReady(t, status, label)
	previous := f.incBackup
	f.seedRows(t, ctx, 2)
	f.incBackup = f.continueIncremental(t, ctx, "d1-"+label+"-inc")
	backup := f.backupView(t, f.incBackup)
	if backup.ParentID != previous {
		f.fatal("after %s the new incremental parented %s, want the previous head %s", label, backup.ParentID, previous)
	}
	kek := f.containerKEKDigest(ctx)
	host := f.kekDigest
	if kek != host {
		f.fatal("after %s the file secret KEK is %s, the host secret is %s", label, kek, host)
	}
	members := len(f.chains(t)[0].Members)
	if members < 2 {
		f.fatal("after %s the chain has %d members, want at least the full and the incremental", label, members)
	}
	f.report(d1LifecycleKey(label)+"instance_id=%s login=%s chain_members=%d a3_pair_readable=true restores=1 incremental=%s migration=ready kek_digest_bound=true volumes=%d",
		f.instanceID, version, members, f.incBackup, len(volumes))
}

// continueIncremental triggers one incremental on the deployed policy and
// returns the backup it published.
func (f *d1Fixture) continueIncremental(t *testing.T, ctx context.Context, key string) string {
	t.Helper()
	runID := f.triggerOn(t, f.policyID, "incremental", key)
	run := f.waitRun(t, runID, 20*time.Minute)
	if run.Status != "completed" || run.BackupID == nil {
		f.fatal("the continue incremental %s ended %s class=%s code=%s",
			runID, run.Status, d1Text(run.ErrorClass), d1Text(run.ErrorCode))
	}
	return *run.BackupID
}

// triggerOn asks one policy's queue for a manual run.
func (f *d1Fixture) triggerOn(t *testing.T, policyID, kind, key string) string {
	t.Helper()
	raw := f.api(http.MethodPost, "/api/v1/policies/"+policyID+"/runs",
		fmt.Sprintf(`{"kind":%q}`, kind), key, http.StatusAccepted)
	var created struct {
		RunID string `json:"run_id"`
	}
	f.decode(raw, &created)
	if !d1UUID.MatchString(created.RunID) {
		f.fatal("trigger returned run_id %q", created.RunID)
	}
	return created.RunID
}

// migrateStatus asks the deployed container itself, so the answer is the
// migration state of the PostgreSQL control plane.
func (f *d1Fixture) migrateStatus(ctx context.Context) d1Migrate {
	f.t.Helper()
	out, code := f.execIn(ctx, "intern", d1WebBinary, "migrate", "status")
	if code != 0 {
		f.fatal("migrate status failed exit=%d: %s", code, out)
	}
	var status d1Migrate
	f.decode(lastJSONLine(out), &status)
	return status
}

// requireMigrationReady proves every migration family in the PostgreSQL control
// plane is at its latest version, which is the Ready state the deployment
// contract names.
func (f *d1Fixture) requireMigrationReady(t *testing.T, status d1Migrate, label string) {
	t.Helper()
	if status.PendingMigrations != 0 || status.PhysicalPendingMigrations != 0 || status.ReplicationPending != 0 {
		f.fatal("after %s the PostgreSQL control plane still has pending migrations: %+v", label, status)
	}
	if status.CurrentVersion != status.LatestVersion || status.InstanceID == "" {
		f.fatal("after %s the migration state is not Ready: %+v", label, status)
	}
	if f.instanceID != "" && f.instanceID != status.InstanceID {
		f.fatal("after %s the PostgreSQL control plane is a different instance: %s then %s",
			label, f.instanceID, status.InstanceID)
	}
	f.instanceID = status.InstanceID
}

// lastJSONLine returns the final JSON object one command printed. The container
// logs nothing else on this path, but the harness never assumes it.
func lastJSONLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "{") {
			return trimmed
		}
	}
	return strings.TrimSpace(out)
}

// containerKEKDigest digests the file secret inside the deployed container.
func (f *d1Fixture) containerKEKDigest(ctx context.Context) string {
	f.t.Helper()
	out, code := f.execIn(ctx, "intern", "sha256sum", d1SecretKEK)
	if code != 0 {
		f.fatal("the deployed container cannot read the KEK secret: %s", out)
	}
	return strings.Fields(out)[0]
}

// upgradeImage replaces the running image with a second candidate tag built from
// the same Dockerfile with a different provenance label, then re-runs every
// retention assertion. The marker states the exact image ids, because the point
// of the case is that a different image reads the same volumes.
func (f *d1Fixture) upgradeImage(t *testing.T, ctx context.Context) {
	t.Helper()
	before := f.containerID(ctx, "intern")
	label := f.source + "-upgrade"
	f.upgradeID = f.buildImage(ctx, f.upgrade, label, false)
	if f.upgradeID == f.candidateID {
		f.fatal("the upgrade candidate reused the same image id %s", f.upgradeID)
	}
	f.d1SetEnv("INTERN_VERSION", strings.TrimSuffix(strings.TrimPrefix(f.upgrade, "intern:"), "-mysql8.4"))
	f.composeOK(ctx, "up", "--detach", "intern")
	f.waitHealthy(ctx, "intern", 4*time.Minute)
	f.refreshAPI(ctx)
	after := f.containerID(ctx, "intern")
	if after == before {
		f.fatal("the upgrade kept the same container %s, so no image was replaced", after)
	}
	running := f.docker(ctx, "inspect", "--format", "{{.Image}}", after)
	if running != f.upgradeID {
		f.fatal("the deployed container runs image %s, want the upgrade %s", running, f.upgradeID)
	}
	labels := f.imageLabels(ctx, f.upgradeID)
	if labels["org.opencontainers.image.revision"] != label {
		f.fatal("the upgrade image labels revision %q, want %q", labels["org.opencontainers.image.revision"], label)
	}
	volumes := f.volumeNames(ctx)
	f.requireRetained(t, ctx, "upgrade", volumes)
	f.report(d1KeyUpgrade+"from=%s to=%s source_label=%s previous_label=%s labels_differ=true container=%s",
		f.candidate, f.upgrade, label, f.source, after)
	f.report(d1KeyKEKKeep+"preserved volumes=postgres,metadata,backups,work instance_id=%s", f.instanceID)
}

// ------------------------------------------------------------- guardrails (A5)

// caseGuardrails proves the three deployment invariants that a single-owner
// control plane depends on: one process per PostgreSQL database, a KEK that must
// be present and private, and a stop that finishes inside the declared grace
// period without ever writing a false completed row.
func (f *d1Fixture) caseGuardrails(t *testing.T, ctx context.Context) {
	t.Helper()
	f.t = t
	defer func() { f.report(d1KeyCase+"%s", d1Cases[4]) }()
	f.requireSingleOwner(t, ctx)
	f.requireKEKRefusals(t, ctx)
	f.requireGracefulStop(t, ctx)
}

// requireSingleOwner uses a different local metadata directory against the same
// PostgreSQL database and requires the database owner lock to refuse it.
func (f *d1Fixture) requireSingleOwner(t *testing.T, ctx context.Context) {
	t.Helper()
	out, code := f.composeAllow(ctx, "run", "--rm", "-e", d1Assign("INTERN_METADATA_DIR", d1ProbeMetadata), "intern")
	if code == 0 {
		f.fatal("a second container accepted the same PostgreSQL database:\n%s", out)
	}
	if !strings.Contains(out, "PostgreSQL control plane is already owned by another process") {
		f.fatal("the second container refused for a different reason:\n%s", out)
	}
	id := f.waitHealthy(ctx, "intern", 2*time.Minute)
	if health := f.docker(ctx, "inspect", "--format", "{{.State.Health.Status}}", id); health != "healthy" {
		f.fatal("the deployed container is %s after the refusal", health)
	}
	if healthStatus, _, _ := f.apiIssue(http.MethodGet, "/healthz", "", ""); healthStatus != http.StatusOK {
		f.fatal("the deployed API stopped answering after the refusal")
	}
	f.report(d1KeyFlock+"refused pg_owner_lock=true different_metadata_dir=true exit=%d primary_healthy=true primary_api=200", code)
}

// requireKEKRefusals proves the file secret is really the key the deployment
// uses: a missing path and a world-readable copy are both startup refusals with
// the fixed sentences, and neither leaves the deployed container affected.
func (f *d1Fixture) requireKEKRefusals(t *testing.T, ctx context.Context) {
	t.Helper()
	if out, code := f.execIn(ctx, "postgres", "psql", "-U", "intern", "-d", "intern", "-v", "ON_ERROR_STOP=1", "-c", "CREATE DATABASE d1_refusal"); code != 0 {
		f.fatal("create isolated PostgreSQL database for KEK refusal probes: %s", out)
	}
	missing := f.refuseServe(ctx, d1Assign("INTERN_KEK_FILE", d1AbsentKEK))
	if !strings.Contains(missing, "cannot read key file") {
		f.fatal("a missing KEK was not refused with the fixed text:\n%s", missing)
	}
	permissions := f.refuseServe(ctx, d1Assign("INTERN_KEK_FILE", d1ProbeKEK))
	if !strings.Contains(permissions, "key file must be a regular private file (chmod 600), not a symlink") {
		f.fatal("a 0644 KEK was not refused with the fixed text:\n%s", permissions)
	}
	if code, _, _ := f.apiIssue(http.MethodGet, "/healthz", "", ""); code != http.StatusOK {
		f.fatal("the deployed container was affected by the refusal probes")
	}
	f.report(d1KeyKEKRefuse+"missing_permissions missing=refused permissions=refused fixed_text=true metadata_dir=%s",
		d1ProbeMetadata)
}

// refuseServe starts one throwaway serve with one overridden setting and returns
// its output. The refusal must arrive before any listener, so the metadata
// directory is a private scratch path the deployed owner never holds.
func (f *d1Fixture) refuseServe(ctx context.Context, override string) string {
	f.t.Helper()
	out, code := f.composeAllow(ctx, "run", "--rm", "-e", override,
		"-e", d1Assign("INTERN_METADATA_DIR", d1ProbeMetadata), "-e", d1Assign("INTERN_DATABASE_URL_FILE", "/d1-secrets/intern_probe_database_url"), "intern")
	if code == 0 {
		f.fatal("serve accepted an unusable KEK (%s):\n%s", override, out)
	}
	return out
}

// requireGracefulStop is the SIGTERM case. The policy is created with business
// fields only; pinSIGTERMThrottle then sets throttle=1 so a real xtrabackup
// stays in flight. The stop is issued only after that run is observed running,
// must finish inside the declared grace period, and after the restart the run
// is failed/backup_failed rather than a completed backup. The evidence lines
// keep observed_running=true, grace_seconds, stop_seconds and no_false_completed.
func (f *d1Fixture) requireGracefulStop(t *testing.T, ctx context.Context) {
	t.Helper()
	before := f.completedRuns(t)
	f.slowPolicyID = f.createPolicy(t, "d1-throttled")
	f.pinSIGTERMThrottle(ctx, f.slowPolicyID)
	f.enablePolicy(t, f.slowPolicyID)
	f.seedRows(t, ctx, 64)
	// The throttled full copies about 10 MB per second. A few hundred megabytes of
	// fresh pages keep that run in flight for tens of seconds, so the stop lands
	// before the chain's success point rather than racing it.
	f.seedPressure(t, ctx, 320)
	runID := f.triggerOn(t, f.slowPolicyID, "full", "d1-a5-sigterm")
	f.waitRunRunning(t, runID, 5*time.Minute)
	grace := f.stopGraceSeconds(ctx)
	id := f.containerID(ctx, "intern")
	started := time.Now()
	out, code := f.composeAllow(ctx, "stop", "-t", strconv.Itoa(grace+30), "intern")
	elapsed := time.Since(started)
	if code != 0 {
		f.fatal("docker compose stop failed exit=%d: %s", code, out)
	}
	if elapsed >= time.Duration(grace)*time.Second {
		f.fatal("the stop took %s, at or beyond the declared %ds grace period", elapsed.Truncate(time.Millisecond), grace)
	}
	state := f.docker(ctx, "inspect", "--format",
		"{{.State.ExitCode}} {{.State.OOMKilled}} {{.State.Status}}", id)
	fields := strings.Fields(state)
	if len(fields) != 3 || fields[2] != "exited" || fields[1] != "false" || fields[0] != "0" {
		f.fatal("the container did not exit cleanly on SIGTERM: %s", state)
	}
	f.composeOK(ctx, "start", "intern")
	f.waitHealthy(ctx, "intern", 4*time.Minute)
	f.refreshAPI(ctx)
	f.login(d1AdminUsername, f.adminPass)
	run := f.waitRun(t, runID, 5*time.Minute)
	f.requireInterruptedRun(t, run, runID, before)
	f.report(d1KeyStop+"run=%s observed_running=true grace_seconds=%d stop_seconds=%d exit_code=%s oom_killed=false",
		runID, grace, int(elapsed.Seconds()), fields[0])
	f.report(d1KeyNoFalse+"true run_status=%s error_class=%s backup_id=none completed_runs_before=%d completed_runs_after=%d",
		run.Status, d1Text(run.ErrorClass), before, f.completedRuns(t))
}

// requireInterruptedRun is the SIGTERM's other half: a run that was streaming
// when the deployment was stopped is recorded as canceled or interrupted, never
// as a completed backup, and the catalog of completed runs does not grow.
func (f *d1Fixture) requireInterruptedRun(t *testing.T, run d1Run, runID string, before int) {
	t.Helper()
	if run.Status == "completed" || run.BackupID != nil {
		f.fatal("the interrupted run reported completed with backup %s", d1Text(run.BackupID))
	}
	// A graceful SIGTERM cancels the worker loop; the dispatcher records a run that
	// was still copying as failed/backup_failed (dispatch/backup.go failChain). Only
	// a run found still running at the next start becomes interrupted, and only an
	// explicit cancel becomes canceled, so neither is the contract for this stop.
	class := d1Text(run.ErrorClass)
	if run.Status != "failed" || class != "backup_failed" {
		f.fatal("the SIGTERM-stopped run %s ended %s class=%s code=%s, want failed/backup_failed",
			runID, run.Status, class, d1Text(run.ErrorCode))
	}
	if after := f.completedRuns(t); after != before {
		f.fatal("the SIGTERM produced %d completed runs where there were %d", after, before)
	}
}

// stopGraceSeconds reads the grace period back from the deployed container, so
// the assertion measures the window the stack really declared.
func (f *d1Fixture) stopGraceSeconds(ctx context.Context) int {
	f.t.Helper()
	id := f.containerID(ctx, "intern")
	// Compose maps stop_grace_period to the container's Config.StopTimeout (whole
	// seconds); there is no HostConfig.StopGracePeriod field.
	raw := f.docker(ctx, "inspect", "--format", "{{json .Config.StopTimeout}}", id)
	grace, err := strconv.Atoi(strings.TrimSpace(raw))
	require(f.t, err)
	if grace < 90 {
		f.fatal("the deployed stop_grace_period is %ds, the design requires at least 90s", grace)
	}
	return grace
}

// waitRunRunning requires the dispatcher to be working on the run before the
// stop is issued: an unclaimed row would prove nothing about SIGTERM.
func (f *d1Fixture) waitRunRunning(t *testing.T, id string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		run := f.runView(t, id)
		if run.Status == "running" {
			return
		}
		if run.Status != "queued" {
			f.fatal("the throttled run left the queue as %s before the stop", run.Status)
		}
		if time.Now().After(deadline) {
			f.fatal("the throttled run stayed queued for %s", within)
		}
		time.Sleep(time.Second)
	}
}

// completedRuns counts the catalog of completed runs, the closed set the SIGTERM
// case must not grow.
func (f *d1Fixture) completedRuns(t *testing.T) int {
	t.Helper()
	var page struct {
		Items []d1Run `json:"items"`
	}
	f.decode(f.api(http.MethodGet, "/api/v1/runs?status=completed", "", "", http.StatusOK), &page)
	return len(page.Items)
}

// ------------------------------------------------------------------ data access

// dial opens one MySQL connection through the loopback port the override
// publishes. It exists only so the harness can seed data and digest snapshots
// independently of the deployment.
func (f *d1Fixture) dial(port int, user, password string) *sql.DB {
	f.t.Helper()
	config := driver.NewConfig()
	config.Net = "tcp"
	config.Addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	config.User = user
	config.Passwd = password
	config.Timeout = 10 * time.Second
	config.InterpolateParams = true
	connector, err := driver.NewConnector(config)
	require(f.t, err)
	return sql.OpenDB(connector)
}

// sourceDB is the deployment's own MySQL container as the harness sees it.
func (f *d1Fixture) sourceDB() *sql.DB { return f.dial(f.mysqlPort, "root", f.rootPass) }

// seedDatabase creates the account the physical chain uses, the schema and the
// first row. The account is 'intern_bkp'@'localhost' because every connection
// the deployment makes is a local socket connection.
func (f *d1Fixture) seedDatabase(ctx context.Context) {
	f.t.Helper()
	db := f.sourceDB()
	defer func() { require(f.t, db.Close()) }()
	for _, statement := range []string{
		"CREATE USER IF NOT EXISTS '" + d1BackupUser + "'@'localhost' IDENTIFIED BY ?",
		"GRANT BACKUP_ADMIN, PROCESS, RELOAD, LOCK TABLES, REPLICATION CLIENT ON *.* TO 'intern_bkp'@'localhost'",
		"GRANT SELECT ON performance_schema.log_status TO 'intern_bkp'@'localhost'",
		"GRANT SELECT ON performance_schema.keyring_component_status TO 'intern_bkp'@'localhost'",
		"GRANT SELECT ON performance_schema.replication_group_members TO 'intern_bkp'@'localhost'",
		"GRANT SELECT ON performance_schema.global_variables TO 'intern_bkp'@'localhost'",
		"GRANT SELECT ON mysql.component TO 'intern_bkp'@'localhost'",
		"CREATE DATABASE IF NOT EXISTS " + d1Database,
		"GRANT SELECT, INSERT ON " + d1Database + ".* TO 'intern_bkp'@'localhost'",
		"CREATE TABLE " + d1Database + ".records(id BIGINT PRIMARY KEY AUTO_INCREMENT,value INT NOT NULL) ENGINE=InnoDB",
		"CREATE TABLE " + d1Database + ".lsn_pressure(id BIGINT PRIMARY KEY AUTO_INCREMENT,payload LONGBLOB NOT NULL) ENGINE=InnoDB",
	} {
		if strings.HasPrefix(statement, "CREATE USER") {
			_, err := db.ExecContext(ctx, statement, f.sourcePass)
			if err != nil {
				f.fatal("creating the backup account failed: %v", err)
			}
			continue
		}
		_, err := db.ExecContext(ctx, statement)
		require(f.t, err)
	}
	f.seedRowsLocked(ctx, db, 1)
}

// seedRows writes DML so the incremental and the SIGTERM case have real changed
// pages to copy, and the snapshot the restore must reproduce is not trivially
// empty.
func (f *d1Fixture) seedRows(t *testing.T, ctx context.Context, rows int) {
	t.Helper()
	db := f.sourceDB()
	defer func() { require(t, db.Close()) }()
	f.seedRowsLocked(ctx, db, rows)
}

// seedRowsLocked performs the inserts on one open handle.
func (f *d1Fixture) seedRowsLocked(ctx context.Context, db *sql.DB, rows int) {
	f.t.Helper()
	for range rows {
		_, err := db.ExecContext(ctx,
			"INSERT INTO "+d1Database+".records(value) VALUES (ROUND(RAND()*100000))")
		require(f.t, err)
		_, err = db.ExecContext(ctx,
			"INSERT INTO "+d1Database+".lsn_pressure(payload) VALUES(REPEAT(SHA2(RAND(),256),32))")
		require(f.t, err)
	}
}

// seedPressure appends megabytes of page data in 1 MiB rows, so a throttled
// backup has a long, real copy phase to interrupt.
func (f *d1Fixture) seedPressure(t *testing.T, ctx context.Context, megabytes int) {
	t.Helper()
	db := f.sourceDB()
	defer func() { require(t, db.Close()) }()
	for range megabytes {
		_, err := db.ExecContext(ctx,
			"INSERT INTO "+d1Database+".lsn_pressure(payload) VALUES(REPEAT(RANDOM_BYTES(1024),1024))")
		require(t, err)
	}
}

// sourceSnapshot digests the data at the current restore point, independently of
// anything Intern wrote: the same two queries the P4 and W5 harnesses use.
func (f *d1Fixture) sourceSnapshot(t *testing.T, ctx context.Context) string {
	t.Helper()
	db := f.sourceDB()
	defer func() { require(t, db.Close()) }()
	snapshot := p4Snapshot(t, ctx, db)
	if !d1Hex64.MatchString(snapshot) {
		f.fatal("the source snapshot digest is malformed: %q", snapshot)
	}
	return snapshot
}

// ------------------------------------------------------------- candidate artifact

// d1Artifact is the candidate-artifact evidence record. It binds the built image
// to the exact source revision and to the component versions the compatibility
// matrix admitted, which is what the release gate needs before anything is
// published. It is captured here, in the job that builds the image, so the G
// stage can consume it without rebuilding anything.
type d1Artifact struct {
	SourceSHA         string            `json:"source_sha"`
	Platform          string            `json:"platform"`
	ImageTag          string            `json:"image_tag"`
	ImageID           string            `json:"image_id"`
	UpgradeTag        string            `json:"upgrade_image_tag"`
	DockerSaveSHA256  string            `json:"docker_save_sha256"`
	DockerSaveBytes   int64             `json:"docker_save_bytes"`
	InternWebSHA256   string            `json:"intern_web_sha256"`
	RootFSLayers      []string          `json:"rootfs_layers"`
	GoImage           string            `json:"go_image"`
	MySQLImage        string            `json:"mysql_image"`
	PXBImage          string            `json:"pxb_image"`
	ServerVersion     string            `json:"mysql_server_version"`
	ToolVersion       string            `json:"xtrabackup_version"`
	ComponentVersions map[string]string `json:"component_versions"`
	Licenses          []string          `json:"license_files"`
	MatrixStatus      string            `json:"matrix_status"`
	MatrixVerified    bool              `json:"matrix_verified"`
	EvidenceIssue     string            `json:"evidence_issue"`
}

// captureArtifact writes the candidate record and the component-version JSON
// next to the other allowlisted evidence, and streams `docker save` through a
// digest so the tarball a distributor would ship has one recorded identity.
func (f *d1Fixture) captureArtifact(ctx context.Context, versions map[string]string, entry physical.CompatibilityEntry) {
	f.t.Helper()
	directory := os.Getenv(d1ArtifactDir)
	if directory == "" {
		directory = filepath.Join(f.root, ".cache", "quality", "deploy")
	}
	require(f.t, os.MkdirAll(directory, 0o755))
	mysqlImage, pxbImage := candidateImages(f.t)
	digest, bytes := f.saveDigest(ctx)
	record := d1Artifact{
		SourceSHA: f.source, Platform: "linux/amd64", ImageTag: f.candidate, ImageID: f.candidateID,
		UpgradeTag: f.upgrade, DockerSaveSHA256: digest, DockerSaveBytes: bytes,
		InternWebSHA256: f.webDigest, RootFSLayers: f.layers,
		GoImage:    f.imageLabels(ctx, f.candidateID)["io.github.flyingeirc.intern.go-image"],
		MySQLImage: mysqlImage, PXBImage: pxbImage,
		ServerVersion: entry.ServerVersion, ToolVersion: entry.ToolVersion,
		ComponentVersions: map[string]string{
			"mysqld": versions["mysqld"], "xtrabackup": versions["xtrabackup"], "xbstream": versions["xbstream"],
			"compose": f.composeVersion(ctx), "server": "mysql-community:" + entry.ServerVersion,
		},
		Licenses: d1LicenseFiles, MatrixStatus: entry.Status, MatrixVerified: entry.Verified,
		EvidenceIssue: entry.EvidenceIssue,
	}
	data, err := json.Marshal(record)
	require(f.t, err)
	require(f.t, os.WriteFile(filepath.Join(directory, "candidate-artifact.json"), append(data, '\n'), 0o644))
	f.report(d1KeyArtifact+"file=candidate-artifact.json source_sha=%s image_id=%s docker_save_sha256=%s docker_save_bytes=%d intern_web_sha256=%s versions_bound=true",
		f.source, f.candidateID, digest, bytes, f.webDigest)
}

// saveDigest streams `docker save` of the candidate tag through SHA-256 without
// keeping the tarball, so the shipped bytes have one recorded identity.
func (f *d1Fixture) saveDigest(ctx context.Context) (string, int64) {
	f.t.Helper()
	command := f.command(ctx, "docker", "save", f.candidate)
	pipe, err := command.StdoutPipe()
	require(f.t, err)
	require(f.t, command.Start())
	hash := sha256.New()
	written, err := io.Copy(hash, pipe)
	require(f.t, err)
	require(f.t, command.Wait())
	return hex.EncodeToString(hash.Sum(nil)), written
}

// composeVersion records the exact Compose implementation the deployment was
// accepted with, because the stack behaviour depends on it.
func (f *d1Fixture) composeVersion(ctx context.Context) string {
	f.t.Helper()
	out, _, _ := f.run(ctx, "docker", "compose", "version", "--short")
	return strings.TrimPrefix(strings.TrimSpace(out), "v")
}

// daemonPlatform records the daemon the stack actually ran on. The acceptance
// refuses emulation, so this is the recorded fact and not a wish.
func (f *d1Fixture) daemonPlatform(ctx context.Context) string {
	f.t.Helper()
	// `.Server.*` exists only in `docker version`; `docker info` exposes OSType
	// and Architecture (linux/x86_64, which the gate accepts alongside amd64).
	return f.docker(ctx, "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}")
}
