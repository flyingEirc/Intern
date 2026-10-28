//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// r1Root is the worker's private fixture tree: the 0700 repository, the work
// root, both KEKs, the control-plane database and the produced manifest. The
// worker creates it in the container's writable layer, as P3 does, so the
// unprivileged uid owns the whole tree.
const r1Root = "/tmp/r1"

// r1NetworkAlias is the private-network name the worker resolves the real FTP
// fixture by. The harness never reads the server's filesystem.
const r1NetworkAlias = "r1-ftp"

// r1WorkerTest is the single worker entry point whose pass and markers the
// evidence gate requires.
const r1WorkerTest = "TestR1Worker"

// TestR1NativeContainers is the combined R1 harness: one pinned MySQL, the PXB
// worker that produces and replicates real chains, and the real Pure-FTPd
// fixture on one private network. Emulation is refused.
func TestR1NativeContainers(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Minute)
	defer cancel()
	docker, err := tc.NewDockerClientWithOpts(ctx)
	require(t, err)
	defer func() { require(t, docker.Close()) }()
	info, err := docker.Info(ctx, client.InfoOptions{})
	require(t, err)
	requireNativeDaemon(t, info.Info.OSType, info.Info.Architecture)
	root, err := filepath.Abs("../../..")
	require(t, err)
	binary := filepath.Join(t.TempDir(), "replication-worker.test")
	build := exec.CommandContext(ctx, "go", "test", "-c", "-tags=integration", "-o", binary, "./internal/replication/acceptance")
	build.Dir = root
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("build replication worker: %v\n%s", buildErr, output)
	}
	source, dirty := physicalSourceIdentity(t, ctx, root)
	data, err := os.ReadFile(binary)
	require(t, err)
	digest := sha256.Sum256(data)
	mysqlImage, pxbImage := candidateImages(t)
	secrets := distinctSecrets(t, 4)
	rootPassword, backupPassword, ftpPassword, ftpQuotaPassword := secrets[0], secrets[1], secrets[2], secrets[3]
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
	worker := startR1Worker(t, ctx, nw, pxbImage, binary, mounts, uid, gid, map[string]string{
		"INTERN_R1_PASSWORD":           backupPassword,
		"INTERN_R1_FTP_ADDRESS":        r1NetworkAlias + ":2121",
		"INTERN_R1_FTP_PASSWORD":       ftpPassword,
		"INTERN_R1_FTP_QUOTA_PASSWORD": ftpQuotaPassword,
		"INTERN_R1_ROOT":               r1Root,
		"HOME":                         "/tmp",
		"TMPDIR":                       "/tmp",
	})
	workerInfo, err := worker.Inspect(ctx)
	require(t, err)
	serverInfo, err := server.Inspect(ctx)
	require(t, err)
	local := strings.Fields(execOutput(t, ctx, worker, "", []string{"sha256sum", "/tmp/replication-worker.test"}, secrets...))
	if len(local) != 2 || local[0] != fmt.Sprintf("%x", digest) {
		t.Fatal("the executed worker differs from the candidate binary")
	}
	requirePinnedFTP(t, ctx, server, secrets)
	t.Logf("R1 candidate source=%s source_dirty=%t binary_sha256=%x mysql_image=%s pxb_image=%s worker_image=%s ftp_image=%s daemon=%s/%s platform=linux/amd64 worker_uid=%d worker_gid=%d server=Pure-FTPd-1.0.54 client=jlaffaye/ftp-v0.2.4 topology=private-network-no-NAT",
		source, dirty, digest, mysqlImage, pxbImage, workerInfo.Image, serverInfo.Image,
		info.Info.OSType, info.Info.Architecture, uid, gid)
	text := execOutput(t, ctx, worker, "", []string{
		"/tmp/replication-worker.test", "-test.v", "-test.run=^" + r1WorkerTest + "$", "-test.timeout=26m",
	}, secrets...)
	verifyR1Evidence(t, text)
	t.Log(text)
}

// startR1MySQL starts the pinned source with a shared datadir volume (mounted
// read-only for the worker) and a shared socket directory.
func startR1MySQL(t *testing.T, ctx context.Context, image, rootPassword, backupPassword string) tc.Container {
	t.Helper()
	fixture, err := mysql.Run(ctx, image, mysql.WithUsername("root"), mysql.WithPassword(rootPassword), mysql.WithDatabase("fixture"),
		tc.WithEnv(map[string]string{"MYSQL_ROOT_HOST": "%"}), tc.WithImagePlatform("linux/amd64"),
		tc.WithMounts(tc.VolumeMount("", "/var/lib/mysql"), tc.VolumeMount("", "/var/run/mysqld")),
		tc.WithCmd("--innodb-buffer-pool-size=64M", "--innodb-redo-log-capacity=33554432"),
		tc.WithWaitStrategy(wait.ForLog("port: 3306  MySQL Community Server").WithStartupTimeout(2*time.Minute)))
	if fixture != nil {
		tc.CleanupContainer(t, fixture, tc.StopTimeout(5*time.Second))
	}
	require(t, err)
	db := openFixtureDB(t, ctx, fixture, "root", rootPassword)
	defer func() { require(t, db.Close()) }()
	_, err = db.ExecContext(ctx, "CREATE USER 'intern_bkp'@'localhost' IDENTIFIED BY ?", backupPassword)
	require(t, err)
	for _, statement := range []string{
		"GRANT BACKUP_ADMIN, PROCESS, RELOAD, LOCK TABLES, REPLICATION CLIENT ON *.* TO 'intern_bkp'@'localhost'",
		"GRANT SELECT ON performance_schema.log_status TO 'intern_bkp'@'localhost'",
		"GRANT SELECT ON performance_schema.keyring_component_status TO 'intern_bkp'@'localhost'",
		"GRANT SELECT ON performance_schema.replication_group_members TO 'intern_bkp'@'localhost'",
		"GRANT SELECT ON performance_schema.global_variables TO 'intern_bkp'@'localhost'",
		"GRANT SELECT ON mysql.component TO 'intern_bkp'@'localhost'",
		"GRANT SELECT, INSERT ON fixture.* TO 'intern_bkp'@'localhost'",
		"CREATE TABLE fixture.records(id BIGINT PRIMARY KEY AUTO_INCREMENT,value INT NOT NULL) ENGINE=InnoDB",
		"CREATE TABLE fixture.lsn_pressure(id BIGINT PRIMARY KEY AUTO_INCREMENT,payload LONGBLOB NOT NULL) ENGINE=InnoDB",
		"INSERT INTO fixture.records(value) VALUES (0)",
	} {
		_, err = db.ExecContext(ctx, statement)
		require(t, err)
	}
	return fixture
}

// mysqlIDs reads the source's mysql uid and gid, which the worker must share to
// read the datadir and the socket.
func mysqlIDs(t *testing.T, ctx context.Context, fixture tc.Container) (int, int) {
	t.Helper()
	uid, err := strconv.Atoi(strings.TrimSpace(execOutput(t, ctx, fixture, "", []string{"id", "-u", "mysql"})))
	require(t, err)
	gid, err := strconv.Atoi(strings.TrimSpace(execOutput(t, ctx, fixture, "", []string{"id", "-g", "mysql"})))
	require(t, err)
	if uid == 0 || gid == 0 {
		t.Fatal("the worker must not run as root")
	}
	return uid, gid
}

// startR1FTP starts the pinned Pure-FTPd fixture on the private network with one
// normal and one quota account, as the S1 harness does.
func startR1FTP(t *testing.T, ctx context.Context, root string, nw *tc.DockerNetwork, password, quotaPassword string) tc.Container {
	t.Helper()
	server, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		FromDockerfile: tc.FromDockerfile{Context: filepath.Join(root, "app/web/integration"), Dockerfile: "ftp.Dockerfile", BuildLogWriter: os.Stderr},
		ImagePlatform:  "linux/amd64",
		Networks:       []string{nw.Name}, NetworkAliases: map[string][]string{nw.Name: {r1NetworkAlias}},
		Mounts: []tc.ContainerMount{tc.VolumeMount("", "/data")},
		Files: []tc.ContainerFile{
			{Reader: strings.NewReader(password + "\n" + password + "\n"), ContainerFilePath: filepath.Join("/run", "s1-password"), FileMode: 0o600},
			{Reader: strings.NewReader(quotaPassword + "\n" + quotaPassword + "\n"), ContainerFilePath: filepath.Join("/run", "s1-quota-password"), FileMode: 0o600},
		},
		WaitingFor: wait.ForExec([]string{"/bin/sh", "-c", "test -s /data/users.pdb && nc -z 127.0.0.1 2121"}).WithStartupTimeout(60 * time.Second),
	}, Started: true})
	if server != nil {
		tc.CleanupContainer(t, server, tc.StopTimeout(5*time.Second))
	}
	require(t, err)
	return server
}

// requirePinnedFTP requires the exact compiled server and its pinned packages.
func requirePinnedFTP(t *testing.T, ctx context.Context, server tc.Container, secrets []string) {
	t.Helper()
	version := execOutput(t, ctx, server, "", []string{"pure-ftpd", "--help"}, secrets...)
	if !strings.Contains(version, "1.0.54") {
		t.Fatal("the FTP fixture is not the exact pinned source")
	}
	packages := execOutput(t, ctx, server, "", []string{"apk", "list", "--installed", "musl", "libssl3", "libcrypto3"}, secrets...)
	for _, pin := range []string{"musl-1.2.6-r3", "libssl3-3.5.9-r0", "libcrypto3-3.5.9-r0"} {
		if !strings.Contains(packages, pin) {
			t.Fatalf("the FTP fixture runtime pin is missing: %s", pin)
		}
	}
}

// startR1Worker starts the PXB worker container as the mysql uid with the source
// mounted read-only and its own private fixture tree.
func startR1Worker(t *testing.T, ctx context.Context, nw *tc.DockerNetwork, image, binary string, mounts []tc.ContainerMount, uid, gid int, env map[string]string) tc.Container {
	t.Helper()
	worker, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		Image: image, ImagePlatform: "linux/amd64", Entrypoint: []string{"/bin/sleep"}, Cmd: []string{"infinity"},
		Networks: []string{nw.Name}, Mounts: mounts, Env: env,
		Files:          []tc.ContainerFile{{HostFilePath: binary, ContainerFilePath: "/tmp/replication-worker.test", FileMode: 0o755}},
		ConfigModifier: func(config *container.Config) { config.User = fmt.Sprintf("%d:%d", uid, gid) },
		WaitingFor:     wait.ForExec([]string{"/usr/bin/test", "-x", "/tmp/replication-worker.test"}).WithStartupTimeout(60 * time.Second),
	}, Started: true})
	if worker != nil {
		tc.CleanupContainer(t, worker, tc.StopTimeout(5*time.Second))
	}
	require(t, err)
	return worker
}

// verifyR1Evidence requires every R1 marker, no skip or failure, and exactly one
// passing worker entry point.
func verifyR1Evidence(t *testing.T, text string) {
	t.Helper()
	if strings.Contains(text, "SKIP") || strings.Contains(text, "--- FAIL:") {
		t.Fatal("the replication worker failed or skipped a case")
	}
	if strings.Count(text, "--- PASS: "+r1WorkerTest+" (") != 1 {
		t.Fatal("missing or repeated R1 worker entry point")
	}
	for _, name := range []string{
		"a1-fifo-verified", "a2-local-corrupt", "a2-remote-tamper", "a2-remote-truncate",
		"a2-wrong-kek", "a2-quota-552", "a3-idempotent-enqueue", "a3-blocked-predecessor",
		"a3-kill-recover", "a4-backoff-exhausted", "a4-manual-retry", "a4-points",
		"a5-retention", "a6-handoff",
	} {
		if strings.Count(text, "--- PASS: "+r1WorkerTest+"/"+name+" (") != 1 {
			t.Fatalf("missing or repeated R1 case: %s", name)
		}
	}
	for _, marker := range []string{
		"R1-A0 produced",
		"R1-A1 backup=0", "R1-A1 backup=1", "R1-A1 backup=2",
		"remote_sha256=exact authenticated_eof=true uploaded_before_verified=true",
		"R1-A2 local_corrupt", "R1-A2 remote_tamper", "R1-A2 remote_truncate",
		"R1-A2 wrong_kek", "R1-A2 quota_552", "reply=552",
		"R1-A3 idempotent", "R1-A3 blocked", "R1-A3 kill=SIGKILL",
		"R1-A4 backoff", "R1-A4 manual_retry", "R1-A4 points",
		"R1-A5 chains", "R1-A6 handoff",
	} {
		if !strings.Contains(text, marker) {
			t.Fatalf("missing R1 evidence marker %q", marker)
		}
	}
}
