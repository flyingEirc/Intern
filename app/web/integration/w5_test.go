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

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// w5Root is the worker's private fixture tree inside the restore container. The
// export root is a named volume the harness also mounts into a brand-new server,
// so a published restore export is proved by starting MySQL on it.
const w5Root = "/tmp/w5"

const w5WorkerTest = "TestW5Worker"

// w5WorkerCases is the locked W5 case set. The gate requires exactly these and
// nothing else.
var w5WorkerCases = []string{"a2-plan", "a6-e2e", "a3-execute", "a4-exclusion", "a5-state"}

// w5ResultMarkers are the fixed facts every W5 worker run must report.
var w5ResultMarkers = []string{
	"W5-A2 plan target=inc2", "W5-A2 plan_readonly", "W5-A2 refused",
	"W5-A3 execute", "W5-A4 exclusion",
	"W5-A5 state", "W5-A5 sigkill",
	"W5-A6 e2e", "W5-A6 restore=inc1", "W5-A6 restore=inc2", "W5-A3 restore=full",
}

var w5StateMarkers = []string{
	"links=ftp:3", "rehearsal=true", "risks=no_auto_replace_production,server_uuid_duplicate,pitr_unavailable",
	"export_path=/export/{restore_id}", "ftp_writes=0",
	"chain=seq:1,2,3", "replicas_verified=3", "local_payloads=removed", "logs_agree=postgres",
	"phase_seen=fetching,authenticating,extracting,preparing,exporting,rehearsing",
	"admin_only=true", "cross_origin=cross_origin_rejected", "confirm=403,confirmation_failed", "no_row=true",
	"limiter=429", "retry_after=", "digest_changed=409,plan_changed", "replay_same_id=true",
	"conflict=409", "audit_atomic=true",
	"run_blocked_by_restore=true", "restore_blocked_by_run=true", "running_max=1", "postgres_intervals=disjoint",
	"canceled_queued=true", "canceled_running=true", "side_effects=explicit",
	"sigkill=failed/interrupted", "published_intact=true", "cleanup=exact", "refused=source_unavailable:409",
	"recovery_repeated=true", "verify_status=passed", "verify_status=not_requested",
}

// The fresh-container markers (container_snapshot=, exports_started=,
// snapshots_match=, new_mysql=) are printed by this harness after its own loop,
// so they are checked from its own facts below and never against the worker
// output. The CI gate still requires them on the whole stream.

// w5RestorePattern binds every restore to its published export id and the source
// snapshot taken at that restore point in the same run.
var w5RestorePattern = regexp.MustCompile(
	`W5-A([36]) restore=(inc1|inc2|full) source=ftp export=([0-9a-f]{32}) expected_snapshot=([0-9a-f]{64})`)

// w5FailurePattern is the recorded outcome of the SIGKILLed restore: an
// interrupted row, never a false success.
var w5FailurePattern = regexp.MustCompile(`W5-A5 sigkill=failed/interrupted[^\n]*side_effects=explicit`)

// TestW5NativeContainers serves the real restore API inside the pinned restore
// image against a real MySQL 8.4.11 source and the real Pure-FTPd fixture on one
// private network, restoring an FTP-only chain through HTTP. Emulation is
// refused. Each published export is then started in a brand-new pinned MySQL
// container and its data snapshot is compared with the snapshot recorded at that
// restore point in the same run.
func TestW5NativeContainers(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Minute)
	defer cancel()
	docker, err := tc.NewDockerClientWithOpts(ctx)
	require(t, err)
	defer func() { require(t, docker.Close()) }()
	info, err := docker.Info(ctx, client.InfoOptions{})
	require(t, err)
	requireNativeDaemon(t, info.Info.OSType, info.Info.Architecture)
	root, err := filepath.Abs("../../..")
	require(t, err)
	binary := filepath.Join(t.TempDir(), "w5-worker.test")
	build := exec.CommandContext(ctx, "go", "test", "-c", "-tags=integration", "-o", binary,
		"./app/web/internal/dispatch/acceptance")
	build.Dir = root
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("build W5 restore API worker: %v\n%s", buildErr, output)
	}
	source, dirty := physicalSourceIdentity(t, ctx, root)
	data, err := os.ReadFile(binary)
	require(t, err)
	digest := sha256.Sum256(data)
	mysqlImage, pxbImage := candidateImages(t)
	secrets := distinctSecrets(t, 6)
	rootPassword, backupPassword := secrets[0], secrets[1]
	ftpPassword, ftpQuotaPassword := secrets[2], secrets[3]
	adminPassword, operatorPassword := secrets[4], secrets[5]
	nw, err := network.New(ctx)
	require(t, err)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		require(t, nw.Remove(cleanup))
	})
	exportVolume := fmt.Sprintf("w5-export-%d", time.Now().UnixNano())
	fixture := startP4MySQL(t, ctx, mysqlImage, rootPassword, backupPassword, exportVolume)
	inspected, err := fixture.Inspect(ctx)
	require(t, err)
	mounts := physicalMounts(t, inspected)
	uid, gid := mysqlIDs(t, ctx, fixture)
	execOutput(t, ctx, fixture, "0", []string{"chown", fmt.Sprintf("%d:%d", uid, gid), "/export"})
	server := startR1FTP(t, ctx, root, nw, ftpPassword, ftpQuotaPassword)
	worker := startW5Worker(t, ctx, root, nw, mysqlImage, pxbImage, binary, mounts, exportVolume, uid, gid,
		map[string]string{
			"INTERN_W4_PASSWORD": backupPassword, "INTERN_W4_FTP_PASSWORD": ftpPassword,
			"INTERN_W4_ADMIN_PASSWORD": adminPassword, "INTERN_W4_OPERATOR_PASSWORD": operatorPassword,
			"INTERN_W4_FTP_HOST": r1NetworkAlias, "INTERN_W4_ROOT": w5Root,
			"INTERN_W5_EXPORT_ROOT": "/export", "INTERN_W5_XBSTREAM": "/usr/bin/xbstream",
			"INTERN_W5_MYSQLD": "/usr/sbin/mysqld",
			"HOME":             "/tmp", "TMPDIR": "/tmp",
		})
	workerInfo, err := worker.Inspect(ctx)
	require(t, err)
	serverInfo, err := server.Inspect(ctx)
	require(t, err)
	requirePinnedFTP(t, ctx, server, secrets)
	local := strings.Fields(execOutput(t, ctx, worker, "", []string{"sha256sum", "/tmp/w5-worker.test"}, secrets...))
	if len(local) != 2 || local[0] != fmt.Sprintf("%x", digest) {
		t.Fatal("the executed worker differs from the candidate binary")
	}
	xbstream := fileDigestSum(t, ctx, worker, "/usr/bin/xbstream", secrets...)
	mysqld := fileDigestSum(t, ctx, worker, "/usr/sbin/mysqld", secrets...)
	tool := fileDigestSum(t, ctx, worker, "/usr/bin/xtrabackup", secrets...)
	text := execP4Output(t, ctx, worker, "", []string{
		"/tmp/w5-worker.test", "-test.v", "-test.run=^" + w5WorkerTest + "$", "-test.timeout=38m",
	}, secrets...)
	t.Logf("W5 candidate source=%s source_dirty=%t binary_sha256=%x mysql_image=%s pxb_image=%s worker_image=%s ftp_image=%s daemon=%s/%s platform=linux/amd64 worker_uid=%d worker_gid=%d tool_sha256=%s xbstream_sha256=%s mysqld_sha256=%s server=Pure-FTPd-1.0.54 client=jlaffaye/ftp-v0.2.4 topology=private-network-no-NAT",
		source, dirty, digest, mysqlImage, pxbImage, workerInfo.Image, serverInfo.Image,
		info.Info.OSType, info.Info.Architecture, uid, gid, tool, xbstream, mysqld)
	verifyW5Evidence(t, text)
	t.Log(text)
	restores := parseW5Restores(t, text)
	started := 0
	for _, restored := range restores {
		fresh := startP4RestoredMySQL(t, ctx, mysqlImage, rootPassword, exportVolume, restored.exportID)
		db := openFixtureDB(t, ctx, fresh, "root", rootPassword)
		actual := p4Snapshot(t, ctx, db)
		require(t, db.Close())
		if actual != restored.snapshot {
			t.Fatalf("restored %s %s snapshot = %s, want %s", restored.label, restored.exportID, actual, restored.snapshot)
		}
		t.Logf("W5-A%c restored=%s container_snapshot=%s expected_snapshot=%s match=true",
			restored.letter, restored.label, actual, restored.snapshot)
		started++
	}
	if started != len(restores) {
		t.Fatalf("exports started = %d, want %d", started, len(restores))
	}
	verifyW5HarnessEvidence(t, started, len(restores), mysqlImage)
	t.Logf("W5-A6 exports_started=%d snapshots_match=true new_mysql=%s", started, mysqlImage)
}

// startW5Worker builds the W5 worker from the P4 restore image: the pinned MySQL
// server plus the pinned Percona XtraBackup tools, so one container really runs
// xtrabackup, xbstream and a rehearsal mysqld. The image is built from the
// compat.json digests, never a tag, and the export volume is shared with the
// source and with every brand-new server the harness starts afterwards.
func startW5Worker(t *testing.T, ctx context.Context, root string, nw *tc.DockerNetwork,
	mysqlImage, pxbImage, binary string, mounts []tc.ContainerMount, exportVolume string,
	uid, gid int, env map[string]string,
) tc.Container {
	t.Helper()
	env = workerPostgres(t, ctx, nw, env)
	mounts = append(mounts, tc.VolumeMount(exportVolume, "/export"))
	worker, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		FromDockerfile: restoreImage.build(t, root, "amd64", map[string]string{"MYSQL_IMAGE": mysqlImage, "PXB_IMAGE": pxbImage}),
		ImagePlatform:  "linux/amd64", Entrypoint: []string{"/bin/sleep"}, Cmd: []string{"infinity"},
		Networks: []string{nw.Name}, Mounts: mounts, Env: env,
		Files:          []tc.ContainerFile{{HostFilePath: binary, ContainerFilePath: "/tmp/w5-worker.test", FileMode: 0o755}},
		ConfigModifier: func(config *container.Config) { config.User = fmt.Sprintf("%d:%d", uid, gid) },
		WaitingFor:     wait.ForExec([]string{"/usr/bin/test", "-x", "/tmp/w5-worker.test"}).WithStartupTimeout(60 * time.Second),
	}, Started: true})
	if worker != nil {
		tc.CleanupContainer(t, worker, tc.StopTimeout(10*time.Second))
	}
	require(t, err)
	execOutput(t, ctx, worker, "", []string{"mkdir", "-p", "-m", "0700", w5Root})
	return worker
}

// w5Restore is one published export and the source snapshot it must reproduce.
type w5Restore struct {
	letter   byte
	label    string
	exportID string
	snapshot string
}

func parseW5Restores(t *testing.T, text string) []w5Restore {
	t.Helper()
	matches := w5RestorePattern.FindAllStringSubmatch(text, -1)
	restores := make([]w5Restore, 0, len(matches))
	seen := map[string]bool{}
	for _, match := range matches {
		if seen[match[2]] {
			t.Fatalf("duplicate restore result %s", match[2])
		}
		seen[match[2]] = true
		restores = append(restores, w5Restore{
			letter: match[1][0], label: match[2], exportID: match[3], snapshot: match[4],
		})
	}
	for _, label := range []string{"inc1", "inc2", "full"} {
		if !seen[label] {
			t.Fatalf("missing restore evidence for %s", label)
		}
	}
	if len(restores) != 3 {
		t.Fatalf("parsed restores = %d, want inc1, inc2 and full", len(restores))
	}
	return restores
}

func verifyW5Evidence(t *testing.T, text string) {
	t.Helper()
	if strings.Contains(text, "--- SKIP:") || strings.Contains(text, "--- FAIL:") {
		t.Fatal("the W5 restore worker skipped or failed a case")
	}
	if strings.Contains(text, "W4 diagnostics: begin") {
		t.Fatal("a W5 scenario emitted failure diagnostics")
	}
	requireW5CasePasses(t, text)
	requireMarkers(t, text, w5ResultMarkers, "result")
	requireMarkers(t, text, w5StateMarkers, "state")
	requireW5RestoreCounts(t, text)
	if !w5FailurePattern.MatchString(text) {
		t.Fatal("the SIGKILLed restore did not record an interrupted failure with explicit side effects")
	}
	if strings.Contains(text, `status="completed"`) && strings.Contains(text, "sigkill") {
		t.Fatal("a SIGKILLed restore reported a completed status")
	}
}

// requireW5CasePasses requires the locked case set exactly: the worker entry
// point once, every locked case once, and no case outside the carrier.
func requireW5CasePasses(t *testing.T, text string) {
	t.Helper()
	if strings.Count(text, "--- PASS: "+w5WorkerTest+" (") != 1 {
		t.Fatal("missing or repeated W5 worker entry point")
	}
	for _, name := range w5WorkerCases {
		if strings.Count(text, "--- PASS: "+w5WorkerTest+"/"+name+" (") != 1 {
			t.Fatalf("missing or repeated W5 case: %s", name)
		}
	}
	if strings.Count(text, "--- PASS: "+w5WorkerTest+"/") != len(w5WorkerCases) {
		t.Fatal("the worker case set differs from the locked evidence carrier")
	}
}

// requireW5RestoreCounts requires one restore marker per locked target and one
// fresh-container comparison per published export, never a repeated or invented
// one.
func requireW5RestoreCounts(t *testing.T, text string) {
	t.Helper()
	for _, label := range []string{"inc1", "inc2", "full"} {
		if strings.Count(text, "restore="+label+" source=ftp export=") != 1 {
			t.Fatalf("restore=%s evidence count is not exactly one", label)
		}
	}
}

// verifyW5HarnessEvidence requires the fresh-container facts this harness
// produced itself: one comparison per published export, all matching (the loop
// fails on a mismatch), and the pinned image the new servers were started from.
// These are harness markers, so they are checked from the harness's own values
// rather than against the worker output it prints later.
func verifyW5HarnessEvidence(t *testing.T, started, published int, mysqlImage string) {
	t.Helper()
	if started != 3 || published != 3 {
		t.Fatalf("exports started = %d of %d published, want three", started, published)
	}
	if !strings.HasPrefix(mysqlImage, "mysql:8.4.11@sha256:") {
		t.Fatalf("the fresh servers were started from %s, want the pinned mysql:8.4.11 digest", mysqlImage)
	}
}
