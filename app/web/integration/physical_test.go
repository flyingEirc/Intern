//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	tc "github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	mysqlImage = "mysql:8.4.11@sha256:85b9bf2e29cf836ecb8c2a15a935d4ba0c606631dff1dd79531a11983c638f2a"
	pxbImage   = "percona/percona-xtrabackup:8.4.0-7@sha256:bf1478c188271b2478faddb92a27e8378364b672b8e399c0df1c17fef8cb0ceb"
)

func TestP2NativeContainers(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	docker, err := tc.NewDockerClientWithOpts(ctx)
	require(t, err)
	defer func() { require(t, docker.Close()) }()
	info, err := docker.Info(ctx, client.InfoOptions{})
	require(t, err)
	requireNativeDaemon(t, info.Info.OSType, info.Info.Architecture)
	root, err := filepath.Abs("../../..")
	require(t, err)
	source, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD").Output()
	require(t, err)
	binary := filepath.Join(t.TempDir(), "physical-worker.test")
	build := exec.CommandContext(ctx, "go", "test", "-c", "-tags=integration", "-o", binary, "./internal/physical/acceptance")
	build.Dir = root
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	output, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("build worker: %v\n%s", err, output)
	}
	data, err := os.ReadFile(binary)
	require(t, err)
	digest := sha256.Sum256(data)
	passwordBytes := make([]byte, 24)
	_, err = rand.Read(passwordBytes)
	require(t, err)
	password := hex.EncodeToString(passwordBytes)
	fixture, err := mysql.Run(ctx, mysqlImage, mysql.WithUsername("root"), mysql.WithPassword(password), mysql.WithDatabase("fixture"),
		tc.WithEnv(map[string]string{"MYSQL_ROOT_HOST": "%"}), tc.WithImagePlatform("linux/amd64"),
		tc.WithMounts(tc.VolumeMount("", "/var/lib/mysql"), tc.VolumeMount("", "/var/run/mysqld")),
		tc.WithCmd("--innodb-buffer-pool-size=64M"), tc.WithWaitStrategy(wait.ForLog("port: 3306  MySQL Community Server").WithStartupTimeout(2*time.Minute)))
	if fixture != nil {
		tc.CleanupContainer(t, fixture, tc.StopTimeout(5*time.Second))
	}
	require(t, err)
	host, err := fixture.Host(ctx)
	require(t, err)
	port, err := fixture.MappedPort(ctx, "3306/tcp")
	require(t, err)
	cfg := driver.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(host, port.Port())
	cfg.User = "root"
	cfg.Passwd = password
	cfg.Timeout = 5 * time.Second
	// Account DDL cannot use server-side prepared placeholders; the driver
	// safely quotes fixture secrets, matching the existing CLI test harness.
	cfg.InterpolateParams = true
	connector, err := driver.NewConnector(cfg)
	require(t, err)
	db := sql.OpenDB(connector)
	defer func() { require(t, db.Close()) }()
	for _, statement := range []string{
		"CREATE USER 'intern_bkp'@'localhost' IDENTIFIED BY ?",
		"CREATE USER 'intern_denied'@'localhost' IDENTIFIED BY ?",
	} {
		_, err = db.ExecContext(ctx, statement, password)
		require(t, err)
	}
	for _, statement := range []string{
		"GRANT BACKUP_ADMIN, PROCESS, RELOAD, LOCK TABLES, REPLICATION CLIENT ON *.* TO 'intern_bkp'@'localhost'",
		"GRANT SELECT ON performance_schema.log_status TO 'intern_bkp'@'localhost'",
		"GRANT SELECT ON performance_schema.keyring_component_status TO 'intern_bkp'@'localhost'",
		"GRANT SELECT ON performance_schema.replication_group_members TO 'intern_bkp'@'localhost'",
		"GRANT SELECT ON performance_schema.global_variables TO 'intern_bkp'@'localhost'",
		"GRANT SELECT ON mysql.component TO 'intern_bkp'@'localhost'",
		"GRANT SELECT, INSERT ON fixture.* TO 'intern_bkp'@'localhost'",
		"CREATE TABLE fixture.records(id BIGINT PRIMARY KEY AUTO_INCREMENT,value INT NOT NULL) ENGINE=InnoDB",
		"INSERT INTO fixture.records(value) VALUES (0)",
	} {
		_, err = db.ExecContext(ctx, statement)
		require(t, err)
	}
	inspected, err := fixture.Inspect(ctx)
	require(t, err)
	mounts := physicalMounts(t, inspected)
	uidText := execOutput(t, ctx, fixture, "", []string{"id", "-u", "mysql"})
	uid, err := strconv.Atoi(strings.TrimSpace(uidText))
	require(t, err)
	gidText := execOutput(t, ctx, fixture, "", []string{"id", "-g", "mysql"})
	gid, err := strconv.Atoi(strings.TrimSpace(gidText))
	require(t, err)
	worker, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		Image: pxbImage, ImagePlatform: "linux/amd64", Entrypoint: []string{"/bin/sleep"}, Cmd: []string{"infinity"}, Mounts: mounts,
		Env: map[string]string{"INTERN_P2_PASSWORD": password}, Files: []tc.ContainerFile{{HostFilePath: binary, ContainerFilePath: "/tmp/physical-worker.test", FileMode: 0o755}},
		ConfigModifier: func(config *container.Config) { config.User = fmt.Sprintf("%d:%d", uid, gid) },
		WaitingFor:     wait.ForExec([]string{"/usr/bin/test", "-x", "/tmp/physical-worker.test"}).WithStartupTimeout(30 * time.Second),
	}, Started: true})
	if worker != nil {
		tc.CleanupContainer(t, worker, tc.StopTimeout(5*time.Second))
	}
	require(t, err)
	workerInfo, err := worker.Inspect(ctx)
	require(t, err)
	t.Logf("candidate source=%s binary_sha256=%x mysql_image=%s pxb_image=%s daemon=%s/%s uid=%d gid=%d", strings.TrimSpace(string(source)), digest, inspected.Image, workerInfo.Image, info.Info.OSType, info.Info.Architecture, uid, gid)
	text := execOutput(t, ctx, worker, "", []string{"/tmp/physical-worker.test", "-test.v", "-test.run=^TestP2RealWorker$", "-test.timeout=6m"}, password)
	verifyPrimaryEvidence(t, text)
	t.Log(text)
	verifyWrongVolume(t, ctx, mounts, binary, password, uid, gid)
	enableEncryptedFixture(t, ctx, fixture, db, uid, gid)
	text = execOutput(t, ctx, worker, "", []string{"/tmp/physical-worker.test", "-test.v", "-test.run=^TestP2EncryptedWorker$", "-test.timeout=1m"}, password)
	if !strings.Contains(text, "P2-A2 encrypted tablespace rejected before backup") || strings.Contains(text, "SKIP") {
		t.Fatal("missing encrypted-source rejection evidence")
	}
	t.Log(text)
}

func execOutput(t *testing.T, ctx context.Context, c tc.Container, user string, args []string, secrets ...string) string {
	t.Helper()
	options := []tcexec.ProcessOption{tcexec.Multiplexed()}
	if user != "" {
		options = append(options, tcexec.WithUser(user))
	}
	code, reader, err := c.Exec(ctx, args, options...)
	require(t, err)
	output, err := io.ReadAll(io.LimitReader(reader, 1<<20))
	require(t, err)
	for _, secret := range secrets {
		if secret != "" && strings.Contains(string(output), secret) {
			t.Fatal("container diagnostic contained a credential; output withheld")
		}
	}
	if code != 0 {
		t.Fatalf("container scenario failed code=%d: %s", code, output)
	}
	return string(output)
}

func require(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func verifyWrongVolume(t *testing.T, ctx context.Context, mounts []tc.ContainerMount, binary, password string, uid, gid int) {
	t.Helper()
	wrongMounts := []tc.ContainerMount{tc.VolumeMount("", "/var/lib/mysql")}
	for _, mount := range mounts {
		if mount.Target == "/var/run/mysqld" {
			wrongMounts = append(wrongMounts, mount)
		}
	}
	wrong, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		Image: pxbImage, ImagePlatform: "linux/amd64", Entrypoint: []string{"/bin/sleep"}, Cmd: []string{"infinity"}, Mounts: wrongMounts,
		Env: map[string]string{"INTERN_P2_PASSWORD": password},
		Files: []tc.ContainerFile{
			{HostFilePath: binary, ContainerFilePath: "/tmp/physical-worker.test", FileMode: 0o755},
			{Reader: strings.NewReader("[auto]\nserver-uuid=00000000-0000-0000-0000-000000000001\n"), ContainerFilePath: "/var/lib/mysql/auto.cnf", FileMode: 0o644},
		},
		ConfigModifier: func(config *container.Config) { config.User = fmt.Sprintf("%d:%d", uid, gid) },
		WaitingFor:     wait.ForExec([]string{"/usr/bin/test", "-x", "/tmp/physical-worker.test"}),
	}, Started: true})
	if wrong != nil {
		tc.CleanupContainer(t, wrong, tc.StopTimeout(5*time.Second))
	}
	require(t, err)
	t.Log(execOutput(t, ctx, wrong, "", []string{"/tmp/physical-worker.test", "-test.v", "-test.run=^TestP2WrongVolumeWorker$", "-test.timeout=1m"}, password))
	execOutput(t, ctx, wrong, "0", []string{"chmod", "000", "/var/lib/mysql"})
	t.Log(execOutput(t, ctx, wrong, "", []string{"/tmp/physical-worker.test", "-test.v", "-test.run=^TestP2UnreadableVolumeWorker$", "-test.timeout=1m"}, password))
}

func enableEncryptedFixture(t *testing.T, ctx context.Context, fixture tc.Container, db *sql.DB, uid, gid int) {
	t.Helper()
	var err error
	var pluginDir string
	require(t, db.QueryRowContext(ctx, "SELECT @@plugin_dir").Scan(&pluginDir))
	execOutput(t, ctx, fixture, "0", []string{"mkdir", "-m", "0700", "/var/lib/intern-test-keyring"})
	execOutput(t, ctx, fixture, "0", []string{"chown", fmt.Sprintf("%d:%d", uid, gid), "/var/lib/intern-test-keyring"})
	require(t, fixture.CopyToContainer(ctx, []byte(`{"path":"/var/lib/intern-test-keyring/keys","read_only":false}`), filepath.Join(pluginDir, "component_keyring_file.cnf"), 0o644))
	mysqldPath := strings.TrimSpace(execOutput(t, ctx, fixture, "0", []string{"readlink", "/proc/1/exe"}))
	require(t, fixture.CopyToContainer(ctx, []byte(`{"components":"file://component_keyring_file"}`), filepath.Join(filepath.Dir(mysqldPath), "mysqld.my"), 0o644))
	stopTimeout := 15 * time.Second
	require(t, fixture.Stop(ctx, &stopTimeout))
	require(t, fixture.Start(ctx))
	readyCtx, readyCancel := context.WithTimeout(ctx, 30*time.Second)
	defer readyCancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for db.PingContext(readyCtx) != nil {
		select {
		case <-readyCtx.Done():
			t.Fatal("keyring-enabled source did not become ready")
		case <-ticker.C:
		}
	}
	for _, statement := range []string{
		"CREATE TABLE fixture.encrypted_records(id INT PRIMARY KEY) ENCRYPTION='Y'",
		"INSERT INTO fixture.encrypted_records VALUES (1)",
	} {
		_, err = db.ExecContext(ctx, statement)
		require(t, err)
	}
}

func physicalMounts(t *testing.T, inspected *container.InspectResponse) []tc.ContainerMount {
	t.Helper()
	mounts := make([]tc.ContainerMount, 0, 2)
	for _, mount := range inspected.Mounts {
		if mount.Destination == "/var/lib/mysql" || mount.Destination == "/var/run/mysqld" {
			m := tc.VolumeMount(mount.Name, tc.ContainerMountTarget(mount.Destination))
			m.ReadOnly = mount.Destination == "/var/lib/mysql"
			mounts = append(mounts, m)
		}
	}
	if len(mounts) != 2 {
		t.Fatal("shared datadir/socket volumes missing")
	}
	return mounts
}

func verifyPrimaryEvidence(t *testing.T, text string) {
	t.Helper()
	for _, marker := range []string{"P2-A1 real versions", "P2-A3 real downstream", "P2-A4 backup=0", "P2-A4 backup=1", "P2-A4 backup=2", "--- PASS: TestP2RealWorker"} {
		if !strings.Contains(text, marker) {
			t.Fatalf("missing worker evidence %q", marker)
		}
	}
	if strings.Contains(text, "SKIP") {
		t.Fatal("worker skipped acceptance")
	}
}

func requireNativeDaemon(t *testing.T, osType, architecture string) {
	t.Helper()
	if osType != "linux" || (architecture != "x86_64" && architecture != "amd64") {
		t.Fatal("P2 requires native Linux amd64 Docker; emulation is not acceptance")
	}
	if os.Getenv("TESTCONTAINERS_RYUK_DISABLED") == "true" {
		t.Fatal("resource reaper must be enabled")
	}
}
