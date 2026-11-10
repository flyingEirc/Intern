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

const w4Root = "/tmp/w4"

const w4WorkerTest = "TestW4Worker"

var w4WorkerCases = []string{
	"a1-a2-e2e", "a2-default-chain-rotation", "a2-priority", "a3-cancel", "a4-sse", "a3-sigterm", "a3-sigkill", "a3-postgres-owner-loss",
}

var w4ResultMarkers = []string{
	"W4-A1 e2e", "W4-A2 priority", "W4-A3 cancel", "W4-A3 sigterm", "W4-A3 sigkill", "W4-A4 sse",
}

var w4StateMarkers = []string{
	"chain=seq:1,2,3", "lsn_continuous=true", "conflict=409", "replicas_verified=3",
	"PG-G2 default_max=7 completed_chain=7 admission_fulls=1 next_incremental_seq=2 replicas_verified=9",
	"PG-P3 owner_terminated=true second_owner=refused serve_exit=nonzero tool_canceled=true completed=0 recovered=interrupted injected_during=backup",
	"protection_requires_verified=true uploaded_window=observed", "uploaded_backups=",
	"logs_agree=postgres", "lists_agree=postgres,local,ftp",
	"running_max=1 claim_full_before_incremental=true queued_together=true order=incremental_first postgres_intervals=disjoint",
	"canceled=true completed_backup=false staging_reclaimed=true work_reclaimed=true requires_full=true source_backups_unchanged=true",
	"sigterm_exit_bounded=true no_false_completed=true recovered=true",
	"sigkill=SIGKILL interrupted=true pending=gone work=gone published_untouched=true protection_held=true next_incremental=requires_full next_incremental_code=parent_not_completed admission_full=completed next_backup=completed remote_held=true postgres_interrupted=true",
	"sse_end=true sse_ordered=true secrets=clean lifecycle=queued<claimed<preflight_ok<started<completed",
}

// w4FaultMarkers are the cases whose marker must prove the fault was injected
// during a real backup, plus the class and code the run recorded.
var w4FaultMarkers = []string{"W4-A3 cancel", "W4-A3 sigterm", "W4-A3 sigkill"}

// w4InjectionPattern is the argv-derived proof of injection: a matched
// `--backup` process of the run's own work root. The `--version` probe cannot
// satisfy it.
var w4InjectionPattern = regexp.MustCompile(`injected_during=backup backup_pid=[1-9][0-9]* backup_target=run-[0-9a-f]+/`)

// w4TerminationPatterns require the recorded outcome of the run each process
// fault ended, and never a false success. The fault is injected only after the
// harness matched a live `--backup` process, so a failure can only be the chain
// failure the drain records or the interruption restart recovery records; a
// fault that had landed during preflight would record preflight or unavailable.
//
// SIGTERM may legitimately end either way, so each status carries its own
// evidence: failed needs a class and code from the closed set, and canceled
// needs the canceled class the store writes (CancelRunningRun leaves error_class
// NULL, so the empty value is the same outcome).
var w4TerminationPatterns = []*regexp.Regexp{
	regexp.MustCompile(`W4-A3 sigterm[^\n]*status=(failed error_class="(backup_failed|interrupted|recovery_failed)" error_code="[a-z_]+"|canceled error_class="(canceled)?" error_code="[a-z_]*") exit=0 `),
	regexp.MustCompile(`W4-A3 sigkill[^\n]*status=failed[^\n]*error_class="(interrupted|recovery_failed)" error_code="[a-z_]+" `),
}

// TestW4NativeContainers serves the real dispatcher and Web handler inside the
// pinned PXB worker against a real MySQL 8.4.11 source and the real Pure-FTPd
// fixture on one private network. Emulation is refused.
func TestW4NativeContainers(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Minute)
	defer cancel()
	docker, err := tc.NewDockerClientWithOpts(ctx)
	require(t, err)
	defer func() { require(t, docker.Close()) }()
	info, err := docker.Info(ctx, client.InfoOptions{})
	require(t, err)
	requireNativeDaemon(t, info.Info.OSType, info.Info.Architecture)
	root, err := filepath.Abs("../../..")
	require(t, err)
	binary := filepath.Join(t.TempDir(), "w4-worker.test")
	build := exec.CommandContext(ctx, "go", "test", "-c", "-tags=integration", "-o", binary, "./app/web/internal/dispatch/acceptance")
	build.Dir = root
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("build W4 acceptance worker: %v\n%s", buildErr, output)
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
	fixture := startR1MySQL(t, ctx, mysqlImage, rootPassword, backupPassword)
	inspected, err := fixture.Inspect(ctx)
	require(t, err)
	mounts := physicalMounts(t, inspected)
	uid, gid := mysqlIDs(t, ctx, fixture)
	server := startR1FTP(t, ctx, root, nw, ftpPassword, ftpQuotaPassword)
	worker := startW4Worker(t, ctx, nw, pxbImage, binary, mounts, uid, gid, map[string]string{
		"INTERN_W4_PASSWORD": backupPassword, "INTERN_W4_FTP_PASSWORD": ftpPassword,
		"INTERN_W4_ADMIN_PASSWORD": adminPassword, "INTERN_W4_OPERATOR_PASSWORD": operatorPassword,
		"INTERN_W4_FTP_HOST": r1NetworkAlias, "INTERN_W4_ROOT": w4Root,
		"HOME": "/tmp", "TMPDIR": "/tmp",
	})
	workerInfo, err := worker.Inspect(ctx)
	require(t, err)
	serverInfo, err := server.Inspect(ctx)
	require(t, err)
	local := strings.Fields(execOutput(t, ctx, worker, "", []string{"sha256sum", "/tmp/w4-worker.test"}, secrets...))
	if len(local) != 2 || local[0] != fmt.Sprintf("%x", digest) {
		t.Fatal("the executed worker differs from the candidate binary")
	}
	requirePinnedFTP(t, ctx, server, secrets)
	tool := fileDigestSum(t, ctx, worker, "/usr/bin/xtrabackup", secrets...)
	text := execOutput(t, ctx, worker, "", []string{
		"/tmp/w4-worker.test", "-test.v", "-test.run=^" + w4WorkerTest + "$", "-test.timeout=36m",
	}, secrets...)
	t.Logf("W4 candidate source=%s source_dirty=%t binary_sha256=%x mysql_image=%s pxb_image=%s worker_image=%s ftp_image=%s daemon=%s/%s platform=linux/amd64 worker_uid=%d worker_gid=%d tool_sha256=%s server=Pure-FTPd-1.0.54 client=jlaffaye/ftp-v0.2.4 topology=private-network-no-NAT",
		source, dirty, digest, mysqlImage, pxbImage, workerInfo.Image, serverInfo.Image,
		info.Info.OSType, info.Info.Architecture, uid, gid, tool)
	verifyW4Evidence(t, text)
	t.Log(text)
}

func startW4Worker(t *testing.T, ctx context.Context, nw *tc.DockerNetwork, image, binary string, mounts []tc.ContainerMount, uid, gid int, env map[string]string) tc.Container {
	t.Helper()
	env = workerPostgres(t, ctx, nw, env)
	worker, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		Image: image, ImagePlatform: "linux/amd64", Entrypoint: []string{"/bin/sleep"}, Cmd: []string{"infinity"},
		Networks: []string{nw.Name}, Mounts: mounts, Env: env,
		Files:          []tc.ContainerFile{{HostFilePath: binary, ContainerFilePath: "/tmp/w4-worker.test", FileMode: 0o755}},
		ConfigModifier: func(config *container.Config) { config.User = fmt.Sprintf("%d:%d", uid, gid) },
		WaitingFor:     wait.ForExec([]string{"/usr/bin/test", "-x", "/tmp/w4-worker.test"}).WithStartupTimeout(60 * time.Second),
	}, Started: true})
	if worker != nil {
		tc.CleanupContainer(t, worker, tc.StopTimeout(5*time.Second))
	}
	require(t, err)
	execOutput(t, ctx, worker, "", []string{"mkdir", "-m", "0700", w4Root})
	return worker
}

// requireMarkers requires every locked marker of one kind in the worker log.
func requireMarkers(t *testing.T, text string, markers []string, kind string) {
	t.Helper()
	for _, marker := range markers {
		if !strings.Contains(text, marker) {
			t.Fatalf("missing W4 %s marker %q", kind, marker)
		}
	}
}

func verifyW4Evidence(t *testing.T, text string) {
	t.Helper()
	if strings.Contains(text, "SKIP") || strings.Contains(text, "--- FAIL:") {
		t.Fatal("the W4 acceptance worker failed or skipped a case")
	}
	for _, name := range w4WorkerCases {
		if strings.Count(text, "--- PASS: "+w4WorkerTest+"/"+name+" (") != 1 {
			t.Fatalf("missing or repeated W4 case: %s", name)
		}
	}
	if strings.Count(text, "--- PASS: "+w4WorkerTest+"/") != len(w4WorkerCases) {
		t.Fatal("the worker case set differs from the locked evidence carrier")
	}
	if strings.Count(text, "--- PASS: "+w4WorkerTest+" (") != 1 {
		t.Fatal("missing or repeated W4 worker entry point")
	}
	requireMarkers(t, text, w4ResultMarkers, "result")
	requireMarkers(t, text, w4StateMarkers, "state")
	requireFaultEvidence(t, text)
}

// requireFaultEvidence requires every fault marker to quote a live backup
// process of the run's own work root, and every process fault to name the class
// and code the ended run recorded. A dump means a scenario failed: the harness
// only emits one from a failed scenario, so it is a failure here too.
func requireFaultEvidence(t *testing.T, text string) {
	t.Helper()
	if strings.Contains(text, "W4 diagnostics: begin") {
		t.Fatal("a W4 scenario emitted failure diagnostics")
	}
	for _, name := range w4FaultMarkers {
		index := strings.Index(text, name)
		if index < 0 {
			t.Fatalf("missing W4 fault marker %q", name)
		}
		end := strings.IndexByte(text[index:], '\n')
		if end < 0 {
			end = len(text) - index
		}
		line := text[index : index+end]
		if !w4InjectionPattern.MatchString(line) {
			t.Fatalf("%s does not prove injection during a backup: %q", name, line)
		}
	}
	for _, pattern := range w4TerminationPatterns {
		if !pattern.MatchString(text) {
			t.Fatalf("a process fault did not record its run class and code: %s", pattern)
		}
	}
}
