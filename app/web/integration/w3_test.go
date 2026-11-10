//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
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
	"github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// w3Root is the worker's private fixture tree: the metadata directory, both
// KEKs, the work root, the local repository and the control-plane database. The
// worker creates it in the container's writable layer so the unprivileged mysql
// uid owns the whole tree.
const w3Root = "/tmp/w3"

// w3NetworkAlias is the private-network name the worker resolves the real FTP
// fixture by. The harness never reads the server's filesystem.
const w3NetworkAlias = "w3-ftp"

// w3MatrixAlias is the private-network name of the out-of-matrix MySQL 9.7.1
// fixture. The worker talks to it through the remapped socket mount, not TCP.
const w3MatrixAlias = "w3-matrix-mysql"

// Worker-side mounts of the 9.7.1 datadir and socket volumes. Keep these
// synchronized with matrixSocket and matrixDataDir in the acceptance worker.
const (
	w3MatrixDataDir = "/w3-matrix/mysql"
	w3MatrixSocket  = "/w3-matrix/mysqld/mysqld.sock"
	w3MatrixVersion = "9.7.1"
)

// w3WorkerTest is the single worker entry point whose pass and markers the
// evidence gate requires.
const w3WorkerTest = "TestW3Worker"

// w3WorkerCases is the locked case set. Keep it synchronized with the worker and
// the evidence gate.
var w3WorkerCases = []string{
	"a3-pass", "a4-atomic-enable", "a3-denied-grants", "a3-wrong-volume",
	"a3-matrix-miss", "a3-unwritable-ftp", "a2-ciphertext",
}

// TestW3NativeContainers serves the real Web handler inside the pinned PXB
// worker against a real MySQL 8.4.11 source, a real out-of-matrix MySQL 9.7.1
// source, and the real Pure-FTPd fixture on one private network. Emulation is
// refused.
func TestW3NativeContainers(t *testing.T) {
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
	binary := filepath.Join(t.TempDir(), "w3-worker.test")
	build := exec.CommandContext(ctx, "go", "test", "-c", "-tags=integration", "-o", binary, "./app/web/internal/resources/acceptance")
	build.Dir = root
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("build Web acceptance worker: %v\n%s", buildErr, output)
	}
	source, dirty := physicalSourceIdentity(t, ctx, root)
	data, err := os.ReadFile(binary)
	require(t, err)
	digest := sha256.Sum256(data)
	mysqlImage, pxbImage := candidateImages(t)
	matrixImage := matrixMissImage(t, root)
	secrets := distinctSecrets(t, 6)
	rootPassword, backupPassword, deniedPassword := secrets[0], secrets[1], secrets[2]
	ftpPassword, ftpLockedPassword, adminPassword := secrets[3], secrets[4], secrets[5]
	operatorPassword := randomSecret(t)
	nw, err := network.New(ctx)
	require(t, err)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		require(t, nw.Remove(cleanup))
	})
	fixture := startW3MySQL(t, ctx, mysqlImage, rootPassword, backupPassword, deniedPassword)
	inspected, err := fixture.Inspect(ctx)
	require(t, err)
	mounts := physicalMounts(t, inspected)
	mounts = append(mounts, w3WrongVolumeMount(t))
	// The 9.7.1 fixture must exist before the worker starts so its datadir and
	// socket volumes can be remounted at the worker paths the source row uses.
	matrix := startW3MatrixMySQL(t, ctx, nw, matrixImage, rootPassword, backupPassword)
	matrixInspected, err := matrix.Inspect(ctx)
	require(t, err)
	mounts = append(mounts, w3MatrixMounts(t, matrixInspected)...)
	uid, gid := mysqlIDs(t, ctx, fixture)
	matrixUID, matrixGID := mysqlIDs(t, ctx, matrix)
	if uid != matrixUID || gid != matrixGID {
		t.Fatalf("out-of-matrix MySQL uid:gid = %d:%d, want worker %d:%d", matrixUID, matrixGID, uid, gid)
	}
	server := startW3FTP(t, ctx, root, nw, ftpPassword, ftpLockedPassword)
	worker := startW3Worker(t, ctx, nw, pxbImage, binary, mounts, uid, gid, map[string]string{
		"INTERN_W3_PASSWORD": backupPassword, "INTERN_W3_DENIED_PASSWORD": deniedPassword,
		"INTERN_W3_FTP_PASSWORD": ftpPassword, "INTERN_W3_FTP_LOCKED_PASSWORD": ftpLockedPassword,
		"INTERN_W3_ADMIN_PASSWORD": adminPassword, "INTERN_W3_OPERATOR_PASSWORD": operatorPassword,
		"INTERN_W3_FTP_HOST": w3NetworkAlias, "INTERN_W3_ROOT": w3Root,
		"HOME": "/tmp", "TMPDIR": "/tmp",
	})
	workerInfo, err := worker.Inspect(ctx)
	require(t, err)
	serverInfo, err := server.Inspect(ctx)
	require(t, err)
	local := strings.Fields(execOutput(t, ctx, worker, "", []string{"sha256sum", "/tmp/w3-worker.test"}, secrets...))
	if len(local) != 2 || local[0] != fmt.Sprintf("%x", digest) {
		t.Fatal("the executed worker differs from the candidate binary")
	}
	requirePinnedFTP(t, ctx, server, secrets)
	tool := fileDigestSum(t, ctx, worker, "/usr/bin/xtrabackup", secrets...)
	requireW3MatrixVisible(t, ctx, worker, uid, secrets...)
	text := execOutput(t, ctx, worker, "", []string{
		"/tmp/w3-worker.test", "-test.v", "-test.run=^" + w3WorkerTest + "$", "-test.timeout=26m",
	}, append(secrets, operatorPassword)...)
	t.Logf("W3 candidate source=%s source_dirty=%t binary_sha256=%x mysql_image=%s matrix_mysql_image=%s pxb_image=%s worker_image=%s ftp_image=%s daemon=%s/%s platform=linux/amd64 worker_uid=%d worker_gid=%d tool_sha256=%s server=Pure-FTPd-1.0.54 client=jlaffaye/ftp-v0.2.4 topology=private-network-no-NAT",
		source, dirty, digest, mysqlImage, matrixImage, pxbImage, workerInfo.Image, serverInfo.Image,
		info.Info.OSType, info.Info.Architecture, uid, gid, tool)
	verifyW3Evidence(t, text)
	t.Log(text)
}

// fileDigestSum is sha256sum of one path that must already exist.
func fileDigestSum(t *testing.T, ctx context.Context, target tc.Container, path string, secrets ...string) string {
	t.Helper()
	fields := strings.Fields(execOutput(t, ctx, target, "", []string{"sha256sum", path}, secrets...))
	if len(fields) != 2 {
		t.Fatalf("sha256sum %s returned %q", path, strings.Join(fields, " "))
	}
	return fields[0]
}

// startW3MySQL starts the pinned source with a shared datadir volume (mounted
// read-only for the worker), a shared socket directory, and both fixture
// accounts: intern_bkp with the backup grants and intern_denied without them.
func startW3MySQL(t *testing.T, ctx context.Context, image, rootPassword, backupPassword, deniedPassword string) tc.Container {
	t.Helper()
	fixture := startR1MySQL(t, ctx, image, rootPassword, backupPassword)
	db := openFixtureDB(t, ctx, fixture, "root", rootPassword)
	defer func() { require(t, db.Close()) }()
	_, err := db.ExecContext(ctx, "CREATE USER 'intern_denied'@'localhost' IDENTIFIED BY ?", deniedPassword)
	require(t, err)
	_, err = db.ExecContext(ctx, "GRANT SELECT ON fixture.* TO 'intern_denied'@'localhost'")
	require(t, err)
	return fixture
}

// startW3MatrixMySQL starts the pinned CLI-matrix MySQL 9.7.1 image with its own
// datadir and socket volumes on the private network, and intern_bkp with the
// same backup grants as the in-matrix source. The worker remounts those volumes
// at w3MatrixDataDir (read-only) and the socket directory so Preflight can
// read identity and then refuse compatibility.
func startW3MatrixMySQL(t *testing.T, ctx context.Context, nw *tc.DockerNetwork, image, rootPassword, backupPassword string) tc.Container {
	t.Helper()
	fixture, err := mysql.Run(ctx, image, mysql.WithUsername("root"), mysql.WithPassword(rootPassword), mysql.WithDatabase("fixture"),
		tc.WithEnv(map[string]string{"MYSQL_ROOT_HOST": "%"}), tc.WithImagePlatform("linux/amd64"),
		network.WithNetwork([]string{w3MatrixAlias}, nw),
		tc.WithMounts(tc.VolumeMount("", "/var/lib/mysql"), tc.VolumeMount("", "/var/run/mysqld")),
		tc.WithCmd("--innodb-buffer-pool-size=64M", "--innodb-redo-log-capacity=33554432"),
		tc.WithWaitStrategy(wait.ForLog("port: 3306  MySQL Community Server").WithStartupTimeout(2*time.Minute)))
	if fixture != nil {
		tc.CleanupContainer(t, fixture, tc.StopTimeout(5*time.Second))
	}
	require(t, err)
	db := openFixtureDB(t, ctx, fixture, "root", rootPassword)
	defer func() { require(t, db.Close()) }()
	var version, comment string
	require(t, db.QueryRowContext(ctx, "SELECT @@version, @@version_comment").Scan(&version, &comment))
	if version != w3MatrixVersion {
		t.Fatalf("out-of-matrix MySQL version = %q, want %s", version, w3MatrixVersion)
	}
	if comment != "MySQL Community Server - GPL" {
		t.Fatalf("out-of-matrix MySQL distribution = %q", comment)
	}
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
	} {
		_, err = db.ExecContext(ctx, statement)
		require(t, err)
	}
	return fixture
}

// w3MatrixMounts remaps the 9.7.1 datadir and socket volumes onto the worker
// paths the source row uses. The destinations differ from the in-matrix 8.4.11
// mounts; read-only datadir and a writable socket directory match P2/W3.
func w3MatrixMounts(t *testing.T, inspected *container.InspectResponse) []tc.ContainerMount {
	t.Helper()
	var data, socket tc.ContainerMount
	var haveData, haveSocket bool
	for _, mount := range inspected.Mounts {
		switch mount.Destination {
		case "/var/lib/mysql":
			data = tc.VolumeMount(mount.Name, tc.ContainerMountTarget(w3MatrixDataDir))
			data.ReadOnly = true
			haveData = true
		case "/var/run/mysqld":
			socket = tc.VolumeMount(mount.Name, tc.ContainerMountTarget(filepath.Dir(w3MatrixSocket)))
			haveSocket = true
		}
	}
	if !haveData || !haveSocket {
		t.Fatal("out-of-matrix datadir/socket volumes missing")
	}
	return []tc.ContainerMount{data, socket}
}

// requireW3MatrixVisible proves the worker mysql uid can see the remapped 9.7.1
// socket and datadir before any API call uses those paths.
func requireW3MatrixVisible(t *testing.T, ctx context.Context, worker tc.Container, uid int, secrets ...string) {
	t.Helper()
	execOutput(t, ctx, worker, "", []string{"test", "-S", w3MatrixSocket}, secrets...)
	execOutput(t, ctx, worker, "", []string{"test", "-d", w3MatrixDataDir}, secrets...)
	owner := strings.TrimSpace(execOutput(t, ctx, worker, "", []string{"stat", "-c", "%u", w3MatrixDataDir}, secrets...))
	if owner != fmt.Sprintf("%d", uid) {
		t.Fatalf("out-of-matrix datadir uid = %s, want %d", owner, uid)
	}
}

// matrixMissImage is the CLI quality-matrix pin for MySQL 9.7.1, including its digest.
func matrixMissImage(t *testing.T, root string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "app/cli/internal/qualitygate/contract.json"))
	require(t, err)
	var contract struct {
		Profiles []struct {
			ID      string `json:"id"`
			Version string `json:"version"`
			Image   string `json:"image"`
		} `json:"profiles"`
	}
	require(t, json.Unmarshal(raw, &contract))
	for _, profile := range contract.Profiles {
		if profile.ID == "mysql-9.7.1" && profile.Version == w3MatrixVersion && strings.HasPrefix(profile.Image, "mysql:9.7.1@sha256:") {
			return profile.Image
		}
	}
	t.Fatal("CLI quality matrix does not pin MySQL 9.7.1")
	return ""
}

// w3WrongVolumeMount is a second real volume at a different path. The source
// row points at it, so the volume check compares a real mount with the real
// @@datadir instead of a path that does not exist.
func w3WrongVolumeMount(t *testing.T) tc.ContainerMount {
	t.Helper()
	return tc.VolumeMount("", "/w3-wrong")
}

// startW3FTP starts the pinned Pure-FTPd fixture with the normal account and the
// locked account whose base directory the server uid cannot write. The
// entrypoint owns that directory's ownership and mode, so the refusal is the
// fixture's real state and not a post-start edit.
func startW3FTP(t *testing.T, ctx context.Context, root string, nw *tc.DockerNetwork, password, lockedPassword string) tc.Container {
	t.Helper()
	server, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		FromDockerfile: ftpImage.build(t, root, "amd64", nil),
		ImagePlatform:  "linux/amd64",
		Networks:       []string{nw.Name}, NetworkAliases: map[string][]string{nw.Name: {w3NetworkAlias}},
		Mounts: []tc.ContainerMount{tc.VolumeMount("", "/data")},
		Files: []tc.ContainerFile{
			{Reader: strings.NewReader(password + "\n" + password + "\n"), ContainerFilePath: "/run/s1-password", FileMode: 0o600},
			{Reader: strings.NewReader(lockedPassword + "\n" + lockedPassword + "\n"), ContainerFilePath: "/run/s1-locked-password", FileMode: 0o600},
		},
		WaitingFor: wait.ForExec([]string{"/bin/sh", "-c", "test -s /data/users.pdb && nc -z 127.0.0.1 2121"}).WithStartupTimeout(60 * time.Second),
	}, Started: true})
	if server != nil {
		tc.CleanupContainer(t, server, tc.StopTimeout(5*time.Second))
	}
	require(t, err)
	requireLockedBaseDir(t, ctx, server)
	return server
}

// requireLockedBaseDir proves the fixture really left the locked account's base
// directory unwritable by the server uid.
func requireLockedBaseDir(t *testing.T, ctx context.Context, server tc.Container) {
	t.Helper()
	owner := strings.TrimSpace(execOutput(t, ctx, server, "0", []string{"stat", "-c", "%u:%a", "/data/locked"}))
	if owner != "0:555" {
		t.Fatalf("locked FTP base directory = %s, want 0:555", owner)
	}
	backend := execOutput(t, ctx, server, "", []string{"cat", "/run/s1-auth-backend"})
	if backend != "normal bcrypt cost=10\nlocked bcrypt cost=10\n" {
		t.Fatalf("fixture authentication backend = %q", backend)
	}
}

// startW3Worker starts the PXB worker as the mysql uid with the in-matrix
// source mounted read-only, the out-of-matrix 9.7.1 mounts, the wrong-volume
// mount, and its own private fixture tree.
func startW3Worker(t *testing.T, ctx context.Context, nw *tc.DockerNetwork, image, binary string, mounts []tc.ContainerMount, uid, gid int, env map[string]string) tc.Container {
	t.Helper()
	env = workerPostgres(t, ctx, nw, env)
	worker, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		Image: image, ImagePlatform: "linux/amd64", Entrypoint: []string{"/bin/sleep"}, Cmd: []string{"infinity"},
		Networks: []string{nw.Name}, Mounts: mounts, Env: env,
		Files:          []tc.ContainerFile{{HostFilePath: binary, ContainerFilePath: "/tmp/w3-worker.test", FileMode: 0o755}},
		ConfigModifier: func(config *container.Config) { config.User = fmt.Sprintf("%d:%d", uid, gid) },
		WaitingFor:     wait.ForExec([]string{"/usr/bin/test", "-x", "/tmp/w3-worker.test"}).WithStartupTimeout(60 * time.Second),
	}, Started: true})
	if worker != nil {
		tc.CleanupContainer(t, worker, tc.StopTimeout(5*time.Second))
	}
	require(t, err)
	// The fixture root must be creatable by the worker uid, so it is created here
	// by that uid rather than lazily by a root-owned helper.
	execOutput(t, ctx, worker, "", []string{"mkdir", "-m", "0700", w3Root})
	return worker
}

// verifyW3Evidence requires every marker, no skip or failure, and exactly one
// passing worker entry point with the locked case set.
func verifyW3Evidence(t *testing.T, text string) {
	t.Helper()
	if strings.Contains(text, "SKIP") || strings.Contains(text, "--- FAIL:") {
		t.Fatal("the Web acceptance worker failed or skipped a case")
	}
	requireW3Cases(t, text)
	requireAllPresent(t, text, w3ResultMarkers)
	requireAllPresent(t, text, w3StateMarkers)
	if strings.Count(text, "--- PASS: "+w3WorkerTest+" (") != 1 {
		t.Fatal("missing or repeated W3 worker entry point")
	}
	if strings.Count(text, "enabled=0") < len(w3RefusalCases) {
		t.Fatal("every refused enable must report the persisted disabled state")
	}
}

// w3ResultMarkers are the four acceptance results this run must produce.
var w3ResultMarkers = []string{
	"W3-A3 pass", "W3-A3 refused", "W3-A4 atomic", "W3-A2 ciphertext",
}

// w3StateMarkers are the state assertions behind those results. Each one names
// an observation, so a case that ran without reaching the state cannot pass.
var w3StateMarkers = []string{
	"case=privileges_missing", "case=wrong_volume",
	"case=matrix_miss check=compatibility code=version_unsupported server_version=9.7.1",
	"case=unwritable_ftp", "reply_code=550", "enabled=0",
	"failure_audit=present", "warning=compatibility_candidate_unverified",
	"plaintext_ftp=audited", "responses=clean", "log=clean", "audit=clean",
	"database_bytes=clean", "cross_row=refused", "cross_field=refused",
	"audit_abort=500", "enabled=1 persisted", "unseal=exact",
}

// w3RefusalCases are the four enable refusals, each of which must persist
// enabled=0.
var w3RefusalCases = []string{"privileges_missing", "wrong_volume", "matrix_miss", "unwritable_ftp"}

func requireW3Cases(t *testing.T, text string) {
	t.Helper()
	for _, name := range w3WorkerCases {
		if strings.Count(text, "--- PASS: "+w3WorkerTest+"/"+name+" (") != 1 {
			t.Fatalf("missing or repeated W3 case: %s", name)
		}
	}
	if strings.Count(text, "--- PASS: "+w3WorkerTest+"/") != len(w3WorkerCases) {
		t.Fatal("the worker case set differs from the locked evidence carrier")
	}
}

func requireAllPresent(t *testing.T, text string, markers []string) {
	t.Helper()
	for _, marker := range markers {
		if !strings.Contains(text, marker) {
			t.Fatalf("missing W3 evidence marker %q", marker)
		}
	}
}
