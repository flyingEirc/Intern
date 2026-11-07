//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/flyingEirc/Intern/internal/storage/ftp/s2fixture"
)

// s2NetworkAlias is the identity every FTPS certificate carries and every worker
// dials: a name, never an address, because the TLS server name is taken from the
// repository's own host and the data channel is opened against the same host with
// the server's passive port.
const s2NetworkAlias = "s2-ftps"

// s2APIRoot is the Web acceptance worker's private fixture tree. It lives in the
// container's writable layer so the unprivileged worker uid owns every path in it.
const s2APIRoot = "/tmp/s2-api"

const (
	s2TransportWorker = "TestS2FTPSWorker"
	s2ChainWorker     = "TestS2Worker"
	s2APIWorker       = "TestS2APIWorker"
)

// s2TransportCases is the locked transport case set: the two FTPS modes, the two
// server-side TLS obligations that make a downgrade impossible, the credential
// census, the four refusals and the plain baseline the refactor must not disturb.
var s2TransportCases = []string{
	"explicit_publish", "implicit_publish", "data_protection", "session_reuse",
	"credential_census", "refusal", "no_downgrade", "plain_baseline", "instance_marker",
}

// s2ChainCases is the locked control-plane case set.
var s2ChainCases = []string{"explicit-chain", "implicit-chain", "api-ftps", "refusal-api"}

// s2APICases is the locked Web-facing case set: the stored shape, the frozen 422
// transport rules and the probe of every face through the real handler.
var s2APICases = []string{"a3-create", "a3-reject", "a3-probe"}

// s2TransportFacts are fixed strings every transport run must report. Values are
// read back from the fixture's manifest, its audit sessions, the real handshakes
// and the server's own filesystem; the harness never accepts a fact it computed
// itself in place of one the worker observed.
var s2TransportFacts = []string{
	"S2-A1 explicit_publish", "S2-A1 implicit_publish",
	"S2-A1 data_protection", "S2-A1 session_reuse",
	"S2-A2 credential_census", "S2-A2 refusal kind=tls_untrusted",
	"S2-A2 refusal kind=tls_hostname", "S2-A2 refusal kind=tls_expired",
	"S2-A2 refusal kind=tls_negotiation", "S2-A2 no_downgrade",
	"S2-A4 plain_baseline", "S2-A3 instance_marker",
	"S2-A4 fixture_identity",
	"listeners=6", "tls_required=3",
}

// s2ChainFacts are the locked control-plane facts: both modes replicate a real
// chain to verified and restore from the FTPS copy, and the API admits FTPS
// repositories on the documented rules only.
var s2ChainFacts = []string{
	"S2-A1 explicit-chain probe", "S2-A1 explicit-chain upload", "S2-A1 explicit-chain readback",
	"S2-A1 explicit-chain restore", "S2-A1 implicit-chain probe", "S2-A1 implicit-chain upload",
	"S2-A1 implicit-chain readback", "S2-A1 implicit-chain restore",
	"S2-A3 api_ftps", "S2-A2 refusal_api",
	"plaintext_credentials=false", "invalid_ca=422", "mode_change=422",
}

// s2APIFacts are the locked Web-facing facts, written as the stable prefixes of the
// lines the worker emits: the case name and its observed fields, so a renamed marker
// is caught while a reformat cannot hide a missing fact.
var s2APIFacts = []string{
	"S2-A3 create=ok explicit=201 implicit=201 plain=201 plaintext_credentials explicit=false,implicit=false,plain=true ca_pem=stored",
	"S2-A3 reject: unparsable_ca=422 plain_with_ca=422 oversized_ca=422 unknown_mode=422",
	"S2-A3 probe: ok=explicit,implicit untrusted=tls_untrusted hostname=tls_hostname expired=tls_expired plain_only=tls_negotiation",
	"S2-A3 stored role=explicit tls_mode=explicit plaintext_credentials=false",
	"S2-A3 stored role=implicit tls_mode=implicit plaintext_credentials=false",
	"S2-A3 probe_ok role=explicit face=explicit",
	"S2-A3 probe_ok role=implicit face=implicit",
	"S2-A3 probe_refused repository=s2-probe-rogue-bundle",
	"S2-A3 census control=plain",
	"S2-A3 bundle_attribution face=explicit pinned=rogue-ca",
}

// s2RestorePattern binds each restore the chain worker published to its face, its
// export and the source snapshot recorded at that restore point in the same run.
var s2RestorePattern = regexp.MustCompile(
	`S2-A1 restore=([a-z0-9]+) face=(explicit|implicit) export=([0-9a-f]{32}) expected_snapshot=([0-9a-f]{64})`)

// s2LeafPattern reports the certificate a real handshake presented, so the harness
// can compare it with the identity it issued.
var s2LeafPattern = regexp.MustCompile(`leaf=([0-9a-f]{64}) dns=(\S+) not_after=(\S+)`)

type s2Restore struct {
	label    string
	face     string
	exportID string
	snapshot string
}

// TestS2NativeContainers is the S2 harness: a real Pure-FTPd 1.0.54 fixture that
// serves FTPS on five faces (explicit TLS-required, implicit TLS, a wrong-hostname
// certificate, an expired certificate and a rogue chain) plus one face with TLS
// disabled, every one of them behind an audit tap that records whether a credential
// ever reached the server. Against it run two workers: the transport worker, which
// proves the client behaviour, and the control-plane worker, which replicates a real
// XtraBackup chain over FTPS and restores from the FTPS copy. Emulation is refused.
// Every export the chain worker publishes is then started by this harness in a
// brand-new pinned MySQL 8.4.11 container and its data snapshot is compared with the
// snapshot the worker recorded at that restore point.
func TestS2NativeContainers(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 80*time.Minute)
	defer cancel()
	docker, err := tc.NewDockerClientWithOpts(ctx)
	require(t, err)
	defer func() { require(t, docker.Close()) }()
	info, err := docker.Info(ctx, client.InfoOptions{})
	require(t, err)
	requireNativeDaemon(t, info.Info.OSType, info.Info.Architecture)
	root, err := filepath.Abs("../../..")
	require(t, err)

	source, dirty := physicalSourceIdentity(t, ctx, root)
	bins := s2BuildCandidates(t, ctx, root)
	mysqlImage, pxbImage := candidateImages(t)
	secrets := distinctSecrets(t, 9)
	rootPassword, backupPassword := secrets[0], secrets[1]
	ftpPassword, marker := secrets[2], secrets[3]
	adminPassword, operatorPassword := secrets[4], secrets[5]
	deniedPassword, ftpLockedPassword := secrets[6], secrets[7]
	_ = secrets[8]

	bundle, err := s2fixture.Certificates(s2NetworkAlias, time.Now())
	require(t, err)

	nw, err := network.New(ctx)
	require(t, err)
	var dataVolume, auditVolume string
	t.Cleanup(func() {
		if t.Failed() || dataVolume == "" || auditVolume == "" {
			return
		}
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		inspector, clientErr := tc.NewDockerClientWithOpts(cleanup)
		require(t, clientErr)
		defer func() { require(t, inspector.Close()) }()
		for _, volume := range []string{dataVolume, auditVolume} {
			_, inspectErr := inspector.VolumeInspect(cleanup, volume, client.VolumeInspectOptions{})
			if !errdefs.IsNotFound(inspectErr) {
				t.Fatal("a private FTPS fixture volume remained after cleanup")
			}
		}
		t.Log("S2-A4 fixture_cleanup: workers/fixture removed; private volume absence verified; network removed")
	})
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		require(t, nw.Remove(cleanup))
	})

	auditVolume = fmt.Sprintf("s2-audit-%d", time.Now().UnixNano())
	fixture, fixtureInfo := startS2Fixture(t, ctx, root, nw, auditVolume, bins.tap, bundle, ftpPassword, marker, secrets)
	manifest := readS2Manifest(t, ctx, fixture, secrets)
	require(t, manifest.MatchesMatrix())

	sourceData := s2FixtureVolume(t, fixtureInfo)
	dataVolume = sourceData

	exportVolume := fmt.Sprintf("s2-export-%d", time.Now().UnixNano())
	mysql := startP4MySQL(t, ctx, mysqlImage, rootPassword, backupPassword, exportVolume)
	inspected, err := mysql.Inspect(ctx)
	require(t, err)
	mounts := append(physicalMounts(t, inspected), tc.VolumeMount(auditVolume, "/fixture-audit"))
	uid, gid := mysqlIDs(t, ctx, mysql)
	execOutput(t, ctx, mysql, "0", []string{"chown", fmt.Sprintf("%d:%d", uid, gid), "/export"})

	transport := startS2TransportWorker(t, ctx, bins.transport, nw, sourceData, auditVolume, ftpPassword, marker, secrets)
	transportInfo, err := transport.Inspect(ctx)
	require(t, err)
	bins.requireExecuted(t, ctx, transport, "/tmp/s2-transport-worker.test", bins.transportHash, "transport", secrets)
	transportText := execP4Output(t, ctx, transport, "", []string{
		"/tmp/s2-transport-worker.test", "-test.v", "-test.run=^" + s2TransportWorker + "$", "-test.timeout=15m",
	}, secrets...)
	verifyS2Evidence(t, transportText, s2TransportWorker, s2TransportCases, s2TransportFacts)
	s2VerifyLeaves(t, transportText, bundle)
	t.Log(transportText)

	chain := startS2ChainWorker(t, ctx, root, nw, mysqlImage, pxbImage, bins.chain, mounts, exportVolume, uid, gid,
		ftpPassword, marker, adminPassword, operatorPassword, backupPassword)
	chainInfo, err := chain.Inspect(ctx)
	require(t, err)
	bins.requireExecuted(t, ctx, chain, "/tmp/s2-chain-worker.test", bins.chainHash, "chain", secrets)
	chainText := execP4Output(t, ctx, chain, "", []string{
		"/tmp/s2-chain-worker.test", "-test.v", "-test.run=^" + s2ChainWorker + "$", "-test.timeout=52m",
	}, secrets...)
	verifyS2Evidence(t, chainText, s2ChainWorker, s2ChainCases, s2ChainFacts)
	t.Log(chainText)

	api := startS2APIWorker(t, ctx, nw, pxbImage, bins.api, auditVolume, uid, gid, adminPassword, operatorPassword,
		ftpPassword, deniedPassword, ftpLockedPassword, backupPassword)
	apiInfo, err := api.Inspect(ctx)
	require(t, err)
	bins.requireExecuted(t, ctx, api, "/tmp/s2-api-worker.test", bins.apiHash, "Web acceptance", secrets)
	apiText := execP4Output(t, ctx, api, "", []string{
		"/tmp/s2-api-worker.test", "-test.v", "-test.run=^" + s2APIWorker + "$", "-test.timeout=12m",
	}, secrets...)
	verifyS2Evidence(t, apiText, s2APIWorker, s2APICases, s2APIFacts)
	t.Log(apiText)

	t.Logf("S2 candidate source=%s source_dirty=%t transport_binary_sha256=%x chain_binary_sha256=%x"+
		" api_binary_sha256=%x fixture_image=%s worker_image=%s chain_image=%s api_image=%s mysql_image=%s pxb_image=%s base_image=%s"+
		" daemon_sha256=%s implicit_daemon_sha256=%s tap_sha256=%x ca_fingerprint=%s marker_sha256=%s"+
		" daemon=%s/%s platform=linux/amd64 worker_uid=%d worker_gid=%d"+
		" server=Pure-FTPd-1.0.54 client=jlaffaye/ftp-v0.2.4 listeners=6 tls_required=3 passive=50100-50619"+
		" topology=private-network-no-NAT",
		source, dirty, bins.transportHash, bins.chainHash, bins.apiHash,
		fixtureInfo.Image, transportInfo.Image, chainInfo.Image, apiInfo.Image, mysqlImage, pxbImage, s1AlpineImage,
		manifest.DaemonSHA256, manifest.ImplicitDaemonSHA256, bins.tapHash, bundle.CAFinger, manifest.MarkerSHA256,
		info.Info.OSType, info.Info.Architecture, uid, gid)

	restores := parseS2Restores(t, chainText)
	started := 0
	for _, restore := range restores {
		fresh := startP4RestoredMySQL(t, ctx, mysqlImage, rootPassword, exportVolume, restore.exportID)
		db := openFixtureDB(t, ctx, fresh, "root", rootPassword)
		actual := p4Snapshot(t, ctx, db)
		require(t, db.Close())
		if actual != restore.snapshot {
			t.Fatalf("restored %s on %s: snapshot %s, want %s", restore.label, restore.face, actual, restore.snapshot)
		}
		t.Logf("S2-A1 restored=%s face=%s container_snapshot=%s expected_snapshot=%s match=true",
			restore.label, restore.face, actual, restore.snapshot)
		started++
	}
	if started != len(restores) {
		t.Fatalf("exports started = %d, want %d", started, len(restores))
	}
	t.Logf("S2-A4 exports_started=%d snapshots_match=true new_mysql=%s", started, mysqlImage)
}

// s2Binaries are the compiled workers and the audit tap, each with the digest of the
// exact bytes this candidate built. A container that runs different bytes proves nothing
// about the source under test, so every one of them is verified inside the container.
type s2Binaries struct {
	transport, chain, api, tap                 string
	transportHash, chainHash, apiHash, tapHash [sha256.Size]byte
}

// s2BuildCandidates cross-compiles the three acceptance workers, and the tap the fixture
// forwards through, for the platform the containers really run on.
func s2BuildCandidates(t *testing.T, ctx context.Context, root string) s2Binaries {
	t.Helper()
	bins := s2Binaries{
		transport: filepath.Join(t.TempDir(), "s2-transport-worker.test"),
		chain:     filepath.Join(t.TempDir(), "s2-chain-worker.test"),
		api:       filepath.Join(t.TempDir(), "s2-api-worker.test"),
		tap:       filepath.Join(t.TempDir(), "s2-tap"),
	}
	for _, build := range []struct {
		destination, pkg string
		hash             *[sha256.Size]byte
	}{
		{bins.transport, "./internal/storage/ftp", &bins.transportHash},
		{bins.chain, "./app/web/internal/dispatch/acceptance", &bins.chainHash},
		{bins.api, "./app/web/internal/resources/acceptance", &bins.apiHash},
		{bins.tap, "./app/web/integration/s2tap", &bins.tapHash},
	} {
		argv := []string{"test", "-c", "-tags=integration", "-o", build.destination, build.pkg}
		if build.pkg == "./app/web/integration/s2tap" {
			argv = []string{"build", "-o", build.destination, build.pkg}
		}
		command := exec.CommandContext(ctx, "go", argv...)
		command.Dir = root
		command.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
		if output, buildErr := command.CombinedOutput(); buildErr != nil {
			t.Fatalf("build %s: %v\n%s", filepath.Base(build.destination), buildErr, output)
		}
		raw, err := os.ReadFile(build.destination)
		require(t, err)
		*build.hash = sha256.Sum256(raw)
	}
	return bins
}

// requireExecuted digests the binary the container holds and demands the candidate bytes.
func (b s2Binaries) requireExecuted(t *testing.T, ctx context.Context, target tc.Container, path string,
	want [sha256.Size]byte, what string, secrets []string,
) {
	t.Helper()
	fields := strings.Fields(execOutput(t, ctx, target, "", []string{"sha256sum", path}, secrets...))
	if len(fields) != 2 || fields[0] != fmt.Sprintf("%x", want) {
		t.Fatalf("the executed %s worker differs from the candidate binary", what)
	}
}

// startS2Fixture builds the FTPS fixture and starts it with every certificate, the
// generated account password, the cleartext probe marker and the audit tap injected
// as root-only files. Nothing here reaches a build argument, a command line or a log.
func startS2Fixture(t *testing.T, ctx context.Context, root string, nw *tc.DockerNetwork, auditVolume, tapBinary string,
	bundle s2fixture.Bundle, password, marker string, secrets []string,
) (tc.Container, *container.InspectResponse) {
	t.Helper()
	injected := make([]tc.ContainerFile, 0, 9)
	injected = append(injected, []tc.ContainerFile{
		{Reader: strings.NewReader(password + "\n" + password + "\n"), ContainerFilePath: "/run/s2-password", FileMode: 0o600},
		{Reader: strings.NewReader(marker + "\n"), ContainerFilePath: "/run/s2/marker", FileMode: 0o600},
		{Reader: strings.NewReader(bundle.CA), ContainerFilePath: "/run/s2/ca.pem", FileMode: 0o644},
		{Reader: strings.NewReader(bundle.RogueCA), ContainerFilePath: "/run/s2/rogue-ca.pem", FileMode: 0o644},
		{HostFilePath: tapBinary, ContainerFilePath: "/usr/local/bin/s2-tap", FileMode: 0o755},
	}...)
	for _, name := range []string{s2fixture.LeafGood, s2fixture.LeafWrongHost, s2fixture.LeafExpired, s2fixture.LeafRogue} {
		leaf, ok := bundle.Leaves[name]
		if !ok || leaf.ServerPEM == "" {
			t.Fatalf("the certificate bundle has no %s leaf", name)
		}
		injected = append(injected, tc.ContainerFile{
			Reader: strings.NewReader(leaf.ServerPEM), ContainerFilePath: "/run/s2/" + name + ".pem", FileMode: 0o600,
		})
	}
	fixture, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		FromDockerfile: ftpsImage.build(t, root, "amd64", nil),
		ImagePlatform:  "linux/amd64",
		Networks:       []string{nw.Name}, NetworkAliases: map[string][]string{nw.Name: {s2NetworkAlias}},
		Mounts: []tc.ContainerMount{tc.VolumeMount("", "/data"), tc.VolumeMount(auditVolume, "/audit")},
		Files:  injected,
		WaitingFor: wait.ForExec([]string{
			"/bin/sh", "-c",
			"test -s /audit/fixture.txt && for port in 2121 2122 2123 2124 2125 990; do nc -z 127.0.0.1 $port || exit 1; done",
		}).
			WithStartupTimeout(4 * time.Minute),
	}, Started: true})
	if fixture != nil {
		// The audit volume is named (every worker mounts it), and testcontainers only
		// removes anonymous volumes on terminate; name it explicitly so the final
		// absence check proves a real cleanup rather than failing on a leftover.
		tc.CleanupContainer(t, fixture, tc.StopTimeout(5*time.Second), tc.RemoveVolumes(auditVolume))
	}
	require(t, err)
	inspected, err := fixture.Inspect(ctx)
	require(t, err)
	return fixture, inspected
}

// readS2Manifest returns the fixture's own account of itself, read out of the shared
// audit volume through the container that owns it.
func readS2Manifest(t *testing.T, ctx context.Context, fixture tc.Container, secrets []string) s2fixture.Fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.txt")
	dump := execOutput(t, ctx, fixture, "0", []string{"cat", "/audit/fixture.txt"}, secrets...)
	require(t, os.WriteFile(path, []byte(dump), 0o600))
	manifest, err := s2fixture.ReadFixture(path)
	require(t, err)
	return manifest
}

// s2FixtureVolume is the server's real filesystem oracle: the volume the daemons
// write published objects into, which every worker also mounts read-back-only.
func s2FixtureVolume(t *testing.T, inspected *container.InspectResponse) string {
	t.Helper()
	for _, mount := range inspected.Mounts {
		if mount.Destination == "/data" && mount.Name != "" {
			return mount.Name
		}
	}
	t.Fatal("missing the FTPS fixture filesystem volume")
	return ""
}

func startS2TransportWorker(t *testing.T, ctx context.Context, binary string, nw *tc.DockerNetwork,
	dataVolume, auditVolume, password, marker string, secrets []string,
) tc.Container {
	t.Helper()
	worker, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		Image: s1AlpineImage, ImagePlatform: "linux/amd64",
		Entrypoint: []string{"/bin/sleep"}, Cmd: []string{"infinity"},
		Networks: []string{nw.Name},
		Mounts: []tc.ContainerMount{
			tc.VolumeMount(dataVolume, "/fixture"), tc.VolumeMount(auditVolume, "/fixture-audit"),
		},
		Env: map[string]string{
			"INTERN_S2_HOST": s2NetworkAlias, "INTERN_S2_ACCOUNT": "normal",
			"INTERN_S2_PASSWORD": password, "INTERN_S2_MARKER": marker,
			"INTERN_S2_AUDIT": "/fixture-audit", "INTERN_S2_ORACLE": "/fixture",
			"HOME": "/tmp", "TMPDIR": "/tmp",
		},
		ConfigModifier: func(config *container.Config) { config.User = "1000:1000" },
		Files:          []tc.ContainerFile{{HostFilePath: binary, ContainerFilePath: "/tmp/s2-transport-worker.test", FileMode: 0o755}},
		WaitingFor:     wait.ForExec([]string{"/usr/bin/test", "-x", "/tmp/s2-transport-worker.test"}),
	}, Started: true})
	if worker != nil {
		tc.CleanupContainer(t, worker, tc.StopTimeout(5*time.Second))
	}
	require(t, err)
	return worker
}

// startS2ChainWorker starts the control-plane worker in the pinned restore image as the
// source's own uid, with the in-matrix source, the fixture audit volume, the export
// volume and its own private tree.
func startS2ChainWorker(t *testing.T, ctx context.Context, root string, nw *tc.DockerNetwork,
	mysqlImage, pxbImage, binary string, mounts []tc.ContainerMount, exportVolume string, uid, gid int,
	ftpPassword, probeMarker, adminPassword, operatorPassword, backupPassword string,
) tc.Container {
	t.Helper()
	mounts = append(mounts, tc.VolumeMount(exportVolume, "/export"))
	worker, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		FromDockerfile: restoreImage.build(t, root, "amd64", map[string]string{"MYSQL_IMAGE": mysqlImage, "PXB_IMAGE": pxbImage}),
		ImagePlatform:  "linux/amd64", Entrypoint: []string{"/bin/sleep"}, Cmd: []string{"infinity"},
		Networks: []string{nw.Name}, Mounts: mounts,
		Env: map[string]string{
			"INTERN_W4_PASSWORD":          backupPassword,
			"INTERN_W4_FTP_PASSWORD":      ftpPassword,
			"INTERN_W4_ADMIN_PASSWORD":    adminPassword,
			"INTERN_W4_OPERATOR_PASSWORD": operatorPassword,
			"INTERN_W4_FTP_HOST":          s2NetworkAlias,
			"INTERN_W4_ROOT":              "/tmp/s2",
			"INTERN_S2_HOST":              s2NetworkAlias,
			"INTERN_S2_ACCOUNT":           "normal",
			"INTERN_S2_PASSWORD":          ftpPassword,
			"INTERN_S2_MARKER":            probeMarker,
			"INTERN_S2_AUDIT":             "/fixture-audit",
			"INTERN_W5_EXPORT_ROOT":       "/export",
			"INTERN_W5_XBSTREAM":          "/usr/bin/xbstream",
			"INTERN_W5_MYSQLD":            "/usr/sbin/mysqld",
			"HOME":                        "/tmp", "TMPDIR": "/tmp",
		},
		Files:          []tc.ContainerFile{{HostFilePath: binary, ContainerFilePath: "/tmp/s2-chain-worker.test", FileMode: 0o755}},
		ConfigModifier: func(config *container.Config) { config.User = fmt.Sprintf("%d:%d", uid, gid) },
		WaitingFor:     wait.ForExec([]string{"/usr/bin/test", "-x", "/tmp/s2-chain-worker.test"}).WithStartupTimeout(60 * time.Second),
	}, Started: true})
	if worker != nil {
		tc.CleanupContainer(t, worker, tc.StopTimeout(10*time.Second))
	}
	require(t, err)
	return worker
}

// startS2APIWorker starts the Web acceptance worker in the pinned PXB image as the
// source's own uid, with only the fixture's audit volume: every repository it stores
// points at the FTPS fixture, so its cases need the published manifest, the two trust
// bundles and the census, and nothing else.
func startS2APIWorker(t *testing.T, ctx context.Context, nw *tc.DockerNetwork, image, binary, auditVolume string,
	uid, gid int, adminPassword, operatorPassword, ftpPassword, deniedPassword, ftpLockedPassword, backupPassword string,
) tc.Container {
	t.Helper()
	worker, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		Image: image, ImagePlatform: "linux/amd64", Entrypoint: []string{"/bin/sleep"}, Cmd: []string{"infinity"},
		Networks: []string{nw.Name},
		Mounts:   []tc.ContainerMount{tc.VolumeMount(auditVolume, "/fixture-audit")},
		Env: map[string]string{
			"INTERN_W3_ROOT": s2APIRoot, "INTERN_W3_FTP_HOST": s2NetworkAlias,
			"INTERN_W3_PASSWORD": backupPassword, "INTERN_W3_DENIED_PASSWORD": deniedPassword,
			"INTERN_W3_FTP_PASSWORD": ftpPassword, "INTERN_W3_FTP_LOCKED_PASSWORD": ftpLockedPassword,
			"INTERN_W3_ADMIN_PASSWORD": adminPassword, "INTERN_W3_OPERATOR_PASSWORD": operatorPassword,
			"INTERN_S2_HOST": s2NetworkAlias, "INTERN_S2_ACCOUNT": "normal",
			"INTERN_S2_PASSWORD": ftpPassword, "INTERN_S2_AUDIT": "/fixture-audit",
			"HOME": "/tmp", "TMPDIR": "/tmp",
		},
		Files:          []tc.ContainerFile{{HostFilePath: binary, ContainerFilePath: "/tmp/s2-api-worker.test", FileMode: 0o755}},
		ConfigModifier: func(config *container.Config) { config.User = fmt.Sprintf("%d:%d", uid, gid) },
		WaitingFor:     wait.ForExec([]string{"/usr/bin/test", "-x", "/tmp/s2-api-worker.test"}).WithStartupTimeout(60 * time.Second),
	}, Started: true})
	if worker != nil {
		tc.CleanupContainer(t, worker, tc.StopTimeout(5*time.Second))
	}
	require(t, err)
	// The private fixture tree is created by the worker uid itself, so every path
	// inside it is owned by the process that has to write to it.
	execOutput(t, ctx, worker, "", []string{"mkdir", "-m", "0700", s2APIRoot}, backupPassword, adminPassword,
		operatorPassword, ftpPassword, deniedPassword, ftpLockedPassword)
	return worker
}

// verifyS2Evidence requires the locked case set and every locked fact in one worker's
// output. The workers run as compiled test binaries inside containers, so their
// results reach this stream only as text this harness logged, never as events.
func verifyS2Evidence(t *testing.T, text, entry string, cases, facts []string) {
	t.Helper()
	if strings.Contains(text, "SKIP") || strings.Contains(text, "--- FAIL:") {
		t.Fatalf("%s failed or skipped a case", entry)
	}
	if strings.Count(text, "--- PASS: "+entry+" (") != 1 {
		t.Fatalf("%s did not run exactly once", entry)
	}
	for _, name := range cases {
		if strings.Count(text, "--- PASS: "+entry+"/"+name+" (") != 1 {
			t.Fatalf("missing or repeated required %s case: %s", entry, name)
		}
	}
	if strings.Count(text, "--- PASS: "+entry+"/") != len(cases) {
		t.Fatalf("%s case set differs from the locked evidence carrier", entry)
	}
	for _, fact := range facts {
		if !strings.Contains(text, " "+fact) && !strings.Contains(text, fact) {
			t.Fatalf("missing required S2 fact: %s", fact)
		}
	}
}

// s2VerifyLeaves compares every certificate a real handshake presented with the
// identity this harness issued. A worker cannot satisfy it by naming a fingerprint it
// made up: the digest, the DNS identity and the validity window all have to agree
// with the bundle the fixture was built from, and the expired face has to be the one
// whose window is already closed.
func s2VerifyLeaves(t *testing.T, text string, bundle s2fixture.Bundle) {
	t.Helper()
	seen := map[string]bool{}
	expired := 0
	for _, match := range s2LeafPattern.FindAllStringSubmatch(text, -1) {
		fingerprint, dns, notAfter := match[1], match[2], match[3]
		if seen[fingerprint+dns] {
			continue
		}
		seen[fingerprint+dns] = true
		if s2RequireIssuedLeaf(t, fingerprint, dns, notAfter, bundle) {
			expired++
		}
	}
	if len(seen) < 3 {
		t.Fatalf("only %d distinct presented certificates were reported, want at least the good, wrong-host and expired leaves", len(seen))
	}
	// The expired face is the one whose refusal can only be the validity window, so a
	// run in which no presented leaf is already past its NotAfter never identified it.
	if expired == 0 {
		t.Fatal("no handshake presented a leaf whose validity window is already closed")
	}
}

// s2RequireIssuedLeaf matches one presented identity against the bundle this harness
// issued, demanding the digest, the DNS names and the validity window all agree. It
// reports whether that leaf is already expired.
func s2RequireIssuedLeaf(t *testing.T, fingerprint, dns, notAfter string, bundle s2fixture.Bundle) bool {
	t.Helper()
	for _, leaf := range bundle.Leaves {
		if leaf.Fingerprint != fingerprint {
			continue
		}
		if strings.Join(leaf.DNSNames, ",") != dns {
			t.Fatalf("leaf %s presented dns=%s, issued dns=%s", fingerprint, dns, leaf.DNSNames)
		}
		watched, err := time.Parse(time.RFC3339Nano, notAfter)
		require(t, err)
		if !watched.UTC().Truncate(time.Second).Equal(leaf.NotAfter.UTC().Truncate(time.Second)) {
			t.Fatalf("leaf %s presented not_after=%s, issued %s", fingerprint, watched, leaf.NotAfter)
		}
		return leaf.NotAfter.Before(time.Now())
	}
	t.Fatalf("a handshake presented leaf %s, which this harness never issued", fingerprint)
	return false
}

func parseS2Restores(t *testing.T, text string) []s2Restore {
	t.Helper()
	matches := s2RestorePattern.FindAllStringSubmatch(text, -1)
	restores := make([]s2Restore, 0, len(matches))
	seen := map[string]bool{}
	for _, match := range matches {
		key := match[1] + "/" + match[2]
		if seen[key] {
			t.Fatalf("duplicate restore result %s", key)
		}
		seen[key] = true
		restores = append(restores, s2Restore{label: match[1], face: match[2], exportID: match[3], snapshot: match[4]})
	}
	if len(restores) != 4 {
		t.Fatalf("parsed restores = %d, want 4 (inc1 and inc2 on both FTPS faces)", len(restores))
	}
	for _, face := range []string{"explicit", "implicit"} {
		for _, label := range []string{"inc1", "inc2"} {
			if !seen[label+"/"+face] {
				t.Fatalf("no restore was published for %s on the %s face", label, face)
			}
		}
	}
	return restores
}
