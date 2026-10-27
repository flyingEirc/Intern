//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	"a1-a2-e2e", "a2-priority", "a3-cancel", "a4-sse", "a3-sigterm", "a3-sigkill",
}

var w4ResultMarkers = []string{
	"W4-A1 e2e", "W4-A2 priority", "W4-A3 cancel", "W4-A3 sigterm", "W4-A3 sigkill", "W4-A4 sse",
}

var w4StateMarkers = []string{
	"chain=seq:1,2,3", "lsn_continuous=true", "conflict=409", "replicas_verified=3",
	"protection_requires_verified=true uploaded_window=observed",
	"logs_agree=sqlite", "lists_agree=sqlite,local,ftp",
	"running_max=1 claim_full_before_incremental=true queued_together=true order=incremental_first sqlite_intervals=disjoint",
	"canceled=true completed_backup=false staging_reclaimed=true work_reclaimed=true requires_full=true source_backups_unchanged=true",
	"sigterm_exit_bounded=true no_false_completed=true recovered=true",
	"sigkill=SIGKILL interrupted=true pending=gone work=gone published_untouched=true protection_held=true next_backup=completed remote_held=true sqlite_interrupted=true",
	"sse_end=true sse_ordered=true secrets=clean lifecycle=queued<claimed<preflight_ok<started<completed",
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
}
