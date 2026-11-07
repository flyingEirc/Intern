//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
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

	"github.com/flyingEirc/Intern/internal/physical"
)

const poisonedLoginHome = "/var/run/mysqld/intern-login-home"

func TestP2NativeContainers(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	docker, err := tc.NewDockerClientWithOpts(ctx)
	require(t, err)
	defer func() { require(t, docker.Close()) }()
	info, err := docker.Info(ctx, client.InfoOptions{})
	require(t, err)
	requireNativeDaemon(t, info.Info.OSType, info.Info.Architecture)
	root, err := filepath.Abs("../../..")
	require(t, err)
	binary := filepath.Join(t.TempDir(), "physical-worker.test")
	build := exec.CommandContext(ctx, "go", "test", "-c", "-tags=integration", "-o", binary, "./internal/physical/acceptance")
	build.Dir = root
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	output, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("build worker: %v\n%s", err, output)
	}
	source, dirty := physicalSourceIdentity(t, ctx, root)
	data, err := os.ReadFile(binary)
	require(t, err)
	digest := sha256.Sum256(data)
	mysqlImage, pxbImage := candidateImages(t)
	secrets := distinctSecrets(t, 3)
	rootPassword, backupPassword, deniedPassword := secrets[0], secrets[1], secrets[2]
	fixture, err := mysql.Run(ctx, mysqlImage, mysql.WithUsername("root"), mysql.WithPassword(rootPassword), mysql.WithDatabase("fixture"),
		tc.WithEnv(map[string]string{"MYSQL_ROOT_HOST": "%"}), tc.WithImagePlatform("linux/amd64"),
		tc.WithMounts(tc.VolumeMount("", "/var/lib/mysql"), tc.VolumeMount("", "/var/run/mysqld")),
		tc.WithCmd("--innodb-buffer-pool-size=64M", "--innodb-redo-log-capacity=33554432"), tc.WithWaitStrategy(wait.ForLog("port: 3306  MySQL Community Server").WithStartupTimeout(2*time.Minute)))
	if fixture != nil {
		tc.CleanupContainer(t, fixture, tc.StopTimeout(5*time.Second))
	}
	require(t, err)
	db := openFixtureDB(t, ctx, fixture, "root", rootPassword)
	defer func() { require(t, db.Close()) }()
	for _, statement := range []struct {
		query    string
		password string
	}{
		{query: "CREATE USER 'intern_bkp'@'localhost' IDENTIFIED BY ?", password: backupPassword},
		{query: "CREATE USER 'intern_denied'@'localhost' IDENTIFIED BY ?", password: deniedPassword},
	} {
		_, err = db.ExecContext(ctx, statement.query, statement.password)
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
		"CREATE TABLE fixture.lsn_pressure(id BIGINT PRIMARY KEY AUTO_INCREMENT,payload LONGBLOB NOT NULL) ENGINE=InnoDB",
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
	loginTools := startLoginToolCarrier(t, ctx, root, mysqlImage, mounts, uid, gid)
	installPoisonedLoginFile(t, ctx, loginTools, uid)
	worker, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		Image: pxbImage, ImagePlatform: "linux/amd64", Entrypoint: []string{"/bin/sleep"}, Cmd: []string{"infinity"}, Mounts: mounts,
		Env: map[string]string{"INTERN_P2_PASSWORD": backupPassword, "INTERN_P2_DENIED_PASSWORD": deniedPassword, "INTERN_P3_FULL_DISK": "/p3-full", "HOME": poisonedLoginHome, "TMPDIR": "/tmp"}, Files: []tc.ContainerFile{{HostFilePath: binary, ContainerFilePath: "/tmp/physical-worker.test", FileMode: 0o755}},
		Tmpfs:          map[string]string{"/p3-full": fmt.Sprintf("rw,size=1048576,mode=0700,uid=%d,gid=%d", uid, gid)},
		ConfigModifier: func(config *container.Config) { config.User = fmt.Sprintf("%d:%d", uid, gid) },
		WaitingFor:     wait.ForExec([]string{"/usr/bin/test", "-x", "/tmp/physical-worker.test"}).WithStartupTimeout(30 * time.Second),
	}, Started: true})
	if worker != nil {
		tc.CleanupContainer(t, worker, tc.StopTimeout(5*time.Second))
	}
	require(t, err)
	workerInfo, err := worker.Inspect(ctx)
	require(t, err)
	t.Logf("candidate source=%s source_dirty=%t binary_sha256=%x matrix_mysql=%s matrix_pxb=%s mysql_image=%s pxb_image=%s daemon=%s/%s uid=%d gid=%d", source, dirty, digest, mysqlImage, pxbImage, inspected.Image, workerInfo.Image, info.Info.OSType, info.Info.Architecture, uid, gid)
	text := execOutput(t, ctx, worker, "", []string{"/tmp/physical-worker.test", "-test.v", "-test.run=^TestP2RealWorker$", "-test.timeout=7m"}, backupPassword, deniedPassword)
	verifyPrimaryEvidence(t, text)
	t.Log(text)
	runP3Workers(t, ctx, worker, backupPassword, deniedPassword)
	verifyWritableVolume(t, ctx, mounts, binary, pxbImage, backupPassword, uid, gid)
	runPIDOneWorker(t, ctx, mounts, binary, pxbImage, backupPassword, uid, gid)
	verifyWrongVolume(t, ctx, mounts, binary, pxbImage, backupPassword, uid, gid)
	fresh := enableEncryptedFixture(t, ctx, fixture, db, uid, gid, rootPassword)
	require(t, db.Close())
	db = fresh
	text = execOutput(t, ctx, worker, "", []string{"/tmp/physical-worker.test", "-test.v", "-test.run=^TestP2EncryptedWorker$", "-test.timeout=1m"}, backupPassword)
	if !strings.Contains(text, "P2-A2 encrypted tablespace rejected before backup") || strings.Contains(text, "SKIP") {
		t.Fatal("missing encrypted-source rejection evidence")
	}
	t.Log(text)
}

func execOutput(t *testing.T, ctx context.Context, c tc.Container, user string, args []string, secrets ...string) string {
	t.Helper()
	text, code := execExec(t, ctx, c, user, args, secrets...)
	if code != 0 {
		t.Fatalf("container scenario failed code=%d: %s", code, text)
	}
	return text
}

func execExec(t *testing.T, ctx context.Context, c tc.Container, user string, args []string, secrets ...string) (string, int) {
	t.Helper()
	options := []tcexec.ProcessOption{tcexec.Multiplexed()}
	if user != "" {
		options = append(options, tcexec.WithUser(user))
	}
	code, reader, err := c.Exec(ctx, args, options...)
	require(t, err)
	output, err := io.ReadAll(io.LimitReader(reader, 1<<20))
	require(t, err)
	return withholdSecrets(t, output, secrets...), code
}

func withholdSecrets(t *testing.T, output []byte, secrets ...string) string {
	t.Helper()
	text := string(output)
	for _, secret := range secrets {
		if secret != "" && strings.Contains(text, secret) {
			t.Fatal("container diagnostic contained a credential; output withheld")
		}
	}
	return text
}

func require(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func verifyWrongVolume(t *testing.T, ctx context.Context, mounts []tc.ContainerMount, binary, pxbImage, backupPassword string, uid, gid int) {
	t.Helper()
	wrongMounts := []tc.ContainerMount{tc.VolumeMount("", "/var/lib/mysql")}
	for _, mount := range mounts {
		if mount.Target == "/var/run/mysqld" {
			wrongMounts = append(wrongMounts, mount)
		}
	}
	wrong, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		Image: pxbImage, ImagePlatform: "linux/amd64", Entrypoint: []string{"/bin/sleep"}, Cmd: []string{"infinity"}, Mounts: wrongMounts,
		Env: map[string]string{"INTERN_P2_PASSWORD": backupPassword},
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
	t.Log(execOutput(t, ctx, wrong, "", []string{"/tmp/physical-worker.test", "-test.v", "-test.run=^TestP2WrongVolumeWorker$", "-test.timeout=1m"}, backupPassword))
	execOutput(t, ctx, wrong, "0", []string{"chmod", "000", "/var/lib/mysql"})
	t.Log(execOutput(t, ctx, wrong, "", []string{"/tmp/physical-worker.test", "-test.v", "-test.run=^TestP2UnreadableVolumeWorker$", "-test.timeout=1m"}, backupPassword))
}

func enableEncryptedFixture(t *testing.T, ctx context.Context, fixture tc.Container, db *sql.DB, uid, gid int, rootPassword string) *sql.DB {
	t.Helper()
	var pluginDir string
	require(t, db.QueryRowContext(ctx, "SELECT @@plugin_dir").Scan(&pluginDir))
	execOutput(t, ctx, fixture, "0", []string{"mkdir", "-m", "0700", "/var/lib/intern-test-keyring"})
	execOutput(t, ctx, fixture, "0", []string{"chown", fmt.Sprintf("%d:%d", uid, gid), "/var/lib/intern-test-keyring"})
	// The image entrypoint probes mysqld configuration as root before dropping
	// privileges. Pre-create the empty store as mysql so that probe cannot
	// leave a root-owned keyring file that the real server cannot open.
	execOutput(t, ctx, fixture, strconv.Itoa(uid), []string{"install", "-m", "0600", "/dev/null", "/var/lib/intern-test-keyring/keys"})
	require(t, fixture.CopyToContainer(ctx, []byte(`{"path":"/var/lib/intern-test-keyring/keys","read_only":false}`), filepath.Join(pluginDir, "component_keyring_file.cnf"), 0o644))
	mysqldPath := mysqldExecutable(t, ctx, fixture, db, uid)
	require(t, fixture.CopyToContainer(ctx, []byte(`{"components":"file://component_keyring_file"}`), filepath.Join(filepath.Dir(mysqldPath), "mysqld.my"), 0o644))
	stopTimeout := 15 * time.Second
	require(t, fixture.Stop(ctx, &stopTimeout))
	require(t, fixture.Start(ctx))
	fresh := waitForFixture(t, ctx, fixture, rootPassword)
	var keyringStatus string
	statusErr := fresh.QueryRowContext(ctx, "SELECT STATUS_VALUE FROM performance_schema.keyring_component_status WHERE STATUS_KEY='Component_status'").Scan(&keyringStatus)
	if statusErr != nil || keyringStatus != "Active" {
		require(t, fresh.Close())
		require(t, statusErr)
		t.Fatal("encrypted refusal fixture requires an active native keyring component")
	}
	for _, statement := range []string{
		"CREATE TABLE fixture.encrypted_records(id INT PRIMARY KEY) ENCRYPTION='Y'",
		"INSERT INTO fixture.encrypted_records VALUES (1)",
	} {
		if _, err := fresh.ExecContext(ctx, statement); err != nil {
			require(t, fresh.Close())
			require(t, err)
		}
	}
	return fresh
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
	for _, marker := range []string{
		"P2-A1 real versions",
		"datadir=EROFS",
		"version_unsupported",
		"prepare_version_mismatch",
		"P2-A3 real downstream",
		"P2-A3 real cancellation",
		"P2-A3 real watchdog",
		"xtrabackup processes gone",
		"P2-A4 backup=0",
		"P2-A4 backup=1",
		"P2-A4 backup=2",
		"extra_lsndir_bytes=exact",
		"stream_checkpoints=exact",
		"stream_info_backup_size=",
		"extra_lsndir_backup_size=",
		"P2-A3 poisoned login file is worker-owned HOME/.mylogin.cnf",
		"--- PASS: TestP2RealWorker",
	} {
		if !strings.Contains(text, marker) {
			t.Fatalf("missing worker evidence %q", marker)
		}
	}
	assertEvidenceCounts(t, text)
}

func assertEvidenceCounts(t *testing.T, text string) {
	t.Helper()
	if strings.Count(text, "extra_lsndir_bytes=exact") != 3 || strings.Count(text, "stream_checkpoints=exact") != 3 {
		t.Fatal("missing extra-lsndir byte evidence for the full chain")
	}
	if strings.Count(text, "moved past parent to_lsn") != 2 {
		t.Fatal("missing incremental checkpoint advance evidence")
	}
	if strings.Count(text, "login_path=ignored") != 3 || !strings.Contains(text, "P2-A3 poisoned login path rejected") {
		t.Fatal("missing poisoned login-path evidence")
	}
	if strings.Count(text, "backup_uuid_provenance=xtrabackup_info") != 3 || strings.Count(text, "lock_time_informational") != 3 {
		t.Fatal("missing backup UUID provenance or informational lock_time")
	}
	if strings.Contains(text, "raw_metadata=byte_equal") || strings.Contains(text, "SKIP") {
		t.Fatal("worker skipped acceptance or claimed streamed info bytes equal extra-lsndir")
	}
}

func requireNativeDaemon(t *testing.T, osType, architecture string) {
	t.Helper()
	if osType != "linux" || (architecture != "x86_64" && architecture != "amd64") {
		t.Fatal("P2 requires native Linux amd64 Docker; emulation is not acceptance")
	}
	if tc.ReadConfig().Config.RyukDisabled {
		t.Fatal("resource reaper must be enabled")
	}
}

func candidateImages(t *testing.T) (string, string) {
	t.Helper()
	entries, err := physical.CompatibilityMatrix()
	require(t, err)
	var mysqlImage, pxbImage string
	for _, entry := range entries {
		if entry.Distribution != "mysql-community" || entry.Platform != "linux/amd64" {
			continue
		}
		if mysqlImage != "" {
			t.Fatal("multiple linux/amd64 MySQL Community matrix entries")
		}
		mysqlImage, pxbImage = entry.MySQLImage, entry.PXBImage
	}
	if mysqlImage == "" || pxbImage == "" {
		t.Fatal("compatibility matrix has no pinned linux/amd64 images")
	}
	return mysqlImage, pxbImage
}

func distinctSecrets(t *testing.T, count int) []string {
	t.Helper()
	secrets := make([]string, 0, count)
	seen := make(map[string]bool, count)
	for len(secrets) < count {
		secret := randomSecret(t)
		if seen[secret] {
			continue
		}
		seen[secret] = true
		secrets = append(secrets, secret)
	}
	return secrets
}

func randomSecret(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 24)
	_, err := rand.Read(buf)
	require(t, err)
	return hex.EncodeToString(buf)
}

func openFixtureDB(t *testing.T, ctx context.Context, fixture tc.Container, user, password string) *sql.DB {
	t.Helper()
	host, err := fixture.Host(ctx)
	require(t, err)
	port, err := fixture.MappedPort(ctx, "3306/tcp")
	require(t, err)
	cfg := driver.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(host, port.Port())
	cfg.User = user
	cfg.Passwd = password
	cfg.Timeout = 5 * time.Second
	// Account DDL cannot use server-side prepared placeholders; the driver
	// safely quotes fixture secrets, matching the existing CLI test harness.
	cfg.InterpolateParams = true
	connector, err := driver.NewConnector(cfg)
	require(t, err)
	return sql.OpenDB(connector)
}

func mysqldExecutable(t *testing.T, ctx context.Context, fixture tc.Container, db *sql.DB, uid int) string {
	t.Helper()
	var basedir string
	require(t, db.QueryRowContext(ctx, "SELECT @@basedir").Scan(&basedir))
	if !filepath.IsAbs(basedir) {
		t.Fatalf("MySQL basedir %q is not absolute", basedir)
	}
	// Same uid as mysqld: /proc/1/exe is not readable as root without CAP_SYS_PTRACE.
	path := strings.TrimSpace(execOutput(t, ctx, fixture, strconv.Itoa(uid), []string{"readlink", "/proc/1/exe"}))
	relative, err := filepath.Rel(basedir, path)
	if err != nil || !filepath.IsAbs(path) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatalf("mysqld executable %q is outside basedir %s", path, basedir)
	}
	execOutput(t, ctx, fixture, strconv.Itoa(uid), []string{"test", "-x", path})
	return path
}

func waitForFixture(t *testing.T, ctx context.Context, fixture tc.Container, password string) *sql.DB {
	t.Helper()
	readyCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		db := openFixtureDB(t, readyCtx, fixture, "root", password)
		if db.PingContext(readyCtx) == nil {
			t.Log("P2-A2 restarted source reconnected")
			return db
		}
		require(t, db.Close())
		select {
		case <-readyCtx.Done():
			t.Fatal("keyring-enabled source did not become ready")
		case <-ticker.C:
		}
	}
}

func installPoisonedLoginFile(t *testing.T, ctx context.Context, fixture tc.Container, uid int) {
	t.Helper()
	setupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	writePoisonedLoginFile(t, setupCtx, fixture, uid)
	assertLoginFileOwner(t, setupCtx, fixture, uid)
	assertLoginFileReadback(t, setupCtx, fixture, uid)
}

func writePoisonedLoginFile(t *testing.T, ctx context.Context, fixture tc.Container, uid int) {
	t.Helper()
	user := strconv.Itoa(uid)
	execOutput(t, ctx, fixture, user, []string{"mkdir", "-m", "0700", poisonedLoginHome})
	// Let the official tool write its own native format. A denied username
	// needs no interactive password prompt and still overrides explicit defaults.
	execOutput(t, ctx, fixture, user, loginHomeCommand("/extract/usr/bin/mysql_config_editor", "set", "--login-path=client", "--user=intern_denied"))
}

func assertLoginFileOwner(t *testing.T, ctx context.Context, fixture tc.Container, uid int) {
	t.Helper()
	user := strconv.Itoa(uid)
	home := strings.TrimSpace(execOutput(t, ctx, fixture, user, []string{"stat", "-c", "%u:%a", poisonedLoginHome}))
	file := strings.TrimSpace(execOutput(t, ctx, fixture, user, []string{"stat", "-c", "%u:%a", poisonedLoginHome + "/.mylogin.cnf"}))
	if home != user+":700" || file != user+":600" {
		t.Fatalf("poisoned login home:file owner:mode = %s %s", home, file)
	}
}

func assertLoginFileReadback(t *testing.T, ctx context.Context, fixture tc.Container, uid int) {
	t.Helper()
	user := strconv.Itoa(uid)
	printed := execOutput(t, ctx, fixture, user, loginHomeCommand("/extract/usr/bin/mysql_config_editor", "print", "--all"))
	if !strings.Contains(printed, "[client]") || !strings.Contains(printed, "intern_denied") {
		t.Fatal("mysql_config_editor did not read a client login path from HOME/.mylogin.cnf")
	}
	t.Log("P2-A3 mysql_config_editor read the poisoned login file")
	text, code := execExec(t, ctx, fixture, user, loginHomeCommand(
		"mysql", "--no-defaults", "--login-path=client", "--protocol=SOCKET",
		"--socket=/var/run/mysqld/mysqld.sock", "--connect-timeout=5", "-e", "SELECT 1",
	))
	lower := strings.ToLower(text)
	if code == 0 || !strings.Contains(text, "intern_denied") || (!strings.Contains(lower, "access denied") && !strings.Contains(text, "1045")) {
		t.Fatal("mysql did not apply the poisoned HOME/.mylogin.cnf login path")
	}
	t.Log("P2-A3 mysql read the poisoned login file from HOME")
}

func loginHomeCommand(tool string, args ...string) []string {
	command := make([]string, 0, 8+len(args))
	command = append(command,
		"env", "-u", "MYSQL_TEST_LOGIN_FILE", "-u", "MYSQL_PWD", "-u", "MYSQL_HOME",
		"HOME="+poisonedLoginHome, tool,
	)
	return append(command, args...)
}

func verifyWritableVolume(t *testing.T, ctx context.Context, mounts []tc.ContainerMount, binary, pxbImage, backupPassword string, uid, gid int) {
	t.Helper()
	writable := append([]tc.ContainerMount(nil), mounts...)
	for i := range writable {
		if writable[i].Target == "/var/lib/mysql" {
			writable[i].ReadOnly = false
		}
	}
	worker, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		Image: pxbImage, ImagePlatform: "linux/amd64", Entrypoint: []string{"/bin/sleep"}, Cmd: []string{"infinity"}, Mounts: writable,
		Env:            map[string]string{"INTERN_P2_PASSWORD": backupPassword, "TMPDIR": "/tmp"},
		Files:          []tc.ContainerFile{{HostFilePath: binary, ContainerFilePath: "/tmp/physical-worker.test", FileMode: 0o755}},
		ConfigModifier: func(config *container.Config) { config.User = fmt.Sprintf("%d:%d", uid, gid) },
		WaitingFor:     wait.ForExec([]string{"/usr/bin/test", "-x", "/tmp/physical-worker.test"}),
	}, Started: true})
	if worker != nil {
		tc.CleanupContainer(t, worker, tc.StopTimeout(5*time.Second))
	}
	require(t, err)
	text := execOutput(t, ctx, worker, "", []string{"/tmp/physical-worker.test", "-test.v", "-test.run=^TestP2WritableVolumeWorker$", "-test.timeout=1m"}, backupPassword)
	if !strings.Contains(text, "--- PASS: TestP2WritableVolumeWorker (") || !strings.Contains(text, "P2-A2 writable datadir mount rejected") {
		t.Fatal("missing writable-volume evidence")
	}
	t.Log(text)
}

func runPIDOneWorker(t *testing.T, ctx context.Context, mounts []tc.ContainerMount, binary, pxbImage, backupPassword string, uid, gid int) {
	t.Helper()
	pid1, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		Image: pxbImage, ImagePlatform: "linux/amd64",
		Entrypoint: []string{"/tmp/physical-worker.test"},
		Cmd:        []string{"-test.v", "-test.run=^TestP2PIDOneWorker$", "-test.timeout=4m"},
		Mounts:     mounts,
		Env:        map[string]string{"INTERN_P2_PASSWORD": backupPassword, "HOME": poisonedLoginHome, "TMPDIR": "/tmp"},
		Files:      []tc.ContainerFile{{HostFilePath: binary, ContainerFilePath: "/tmp/physical-worker.test", FileMode: 0o755}},
		ConfigModifier: func(config *container.Config) {
			config.User = fmt.Sprintf("%d:%d", uid, gid)
			config.WorkingDir = "/tmp"
		},
		WaitingFor: wait.ForExit().WithExitTimeout(4 * time.Minute),
	}, Started: true})
	if pid1 != nil {
		tc.CleanupContainer(t, pid1, tc.StopTimeout(5*time.Second))
	}
	text := ""
	if pid1 != nil {
		text = containerLogs(t, ctx, pid1, backupPassword)
	}
	if err != nil || pid1 == nil {
		t.Fatalf("PID1 worker failed to finish: %v\n%s", err, text)
	}
	state, stateErr := pid1.State(ctx)
	require(t, stateErr)
	if !pid1Evidence(text, state.ExitCode) {
		t.Fatalf("PID1 worker evidence missing: code=%d\n%s", state.ExitCode, text)
	}
	t.Log(text)
}

func pid1Evidence(text string, code int) bool {
	return code == 0 &&
		strings.Contains(text, "--- PASS: TestP2PIDOneWorker (") &&
		strings.Contains(text, "P2-A1 PID1 memfd backup") &&
		strings.Contains(text, "P2-A3 poisoned login file is worker-owned HOME/.mylogin.cnf") &&
		strings.Contains(text, "backup_uuid_provenance=xtrabackup_info") &&
		strings.Contains(text, "lock_time_informational")
}

func containerLogs(t *testing.T, ctx context.Context, c tc.Container, secrets ...string) string {
	t.Helper()
	reader, err := c.Logs(ctx)
	require(t, err)
	defer func() { require(t, reader.Close()) }()
	data, err := io.ReadAll(io.LimitReader(reader, 1<<20))
	require(t, err)
	text := string(data)
	for _, secret := range secrets {
		if secret != "" && strings.Contains(text, secret) {
			t.Fatal("container diagnostic contained a credential; output withheld")
		}
	}
	return text
}

type physicalClientPackage struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

func loadPhysicalClientPackage(t *testing.T, root, mysqlImage string) physicalClientPackage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "app/cli/internal/qualitygate/contract.json"))
	require(t, err)
	var contract struct {
		Profiles []struct {
			Image    string                           `json:"image"`
			Packages map[string]physicalClientPackage `json:"client_packages"`
		} `json:"profiles"`
	}
	require(t, json.Unmarshal(raw, &contract))
	for _, profile := range contract.Profiles {
		if profile.Image == mysqlImage {
			pkg, found := profile.Packages["amd64"]
			if !found || pkg.URL == "" || pkg.SHA256 == "" {
				t.Fatal("matching native client package pin is missing")
			}
			return pkg
		}
	}
	t.Fatal("physical source image has no matching locked official client package")
	return physicalClientPackage{}
}

func startLoginToolCarrier(t *testing.T, ctx context.Context, root, mysqlImage string, mounts []tc.ContainerMount, uid, gid int) tc.Container {
	t.Helper()
	pkg := loadPhysicalClientPackage(t, root, mysqlImage)
	values := map[string]string{"MYSQL_IMAGE": mysqlImage, "CLIENT_RPM_URL": pkg.URL, "CLIENT_RPM_SHA256": pkg.SHA256}
	carrier, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		FromDockerfile: clientToolsImage.build(t, root, "amd64", values),
		ImagePlatform:  "linux/amd64", Entrypoint: []string{"/bin/sleep"}, Cmd: []string{"infinity"}, Mounts: mounts,
		ConfigModifier: func(config *container.Config) { config.User = fmt.Sprintf("%d:%d", uid, gid) },
		WaitingFor:     wait.ForExec([]string{"/usr/bin/test", "-x", "/extract/usr/bin/mysql_config_editor"}),
	}, Started: true})
	if carrier != nil {
		tc.CleanupContainer(t, carrier, tc.StopTimeout(5*time.Second))
	}
	require(t, err)
	info, err := carrier.Inspect(ctx)
	require(t, err)
	version := execOutput(t, ctx, carrier, "", []string{"/extract/usr/bin/mysql_config_editor", "--version"})
	t.Logf("official login-file tool image=%s package_sha256=%s version=%s", info.Image, pkg.SHA256, strings.TrimSpace(version))
	return carrier
}

func physicalSourceIdentity(t *testing.T, ctx context.Context, root string) (string, bool) {
	t.Helper()
	raw, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD").Output()
	require(t, err)
	source := strings.TrimSpace(string(raw))
	if expected := os.Getenv("GITHUB_SHA"); expected != "" && source != expected {
		t.Fatal("physical candidate source does not match the CI commit")
	}
	status, err := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain", "--untracked-files=normal").Output()
	require(t, err)
	dirty := len(status) != 0
	if os.Getenv("GITHUB_ACTIONS") == "true" && dirty {
		t.Fatal("CI physical candidate contains uncommitted source changes")
	}
	return source, dirty
}

func runP3Workers(t *testing.T, ctx context.Context, worker tc.Container, secrets ...string) {
	t.Helper()
	text := execOutput(t, ctx, worker, "", []string{"/tmp/physical-worker.test", "-test.v", "-test.run=^TestP3(Real|Disk|Recovery)Worker$", "-test.timeout=7m"}, secrets...)
	for _, marker := range []string{
		"--- PASS: TestP3RealWorker (",
		"--- PASS: TestP3DiskWorker (",
		"--- PASS: TestP3RecoveryWorker (",
		"P3-A1 backup=0", "P3-A1 backup=1", "P3-A1 backup=2",
		"P3-A2 fault=nonzero", "P3-A2 fault=enospc", "P3-A2 fault=cancel",
		"P3-A2 fault=short_write", "P3-A2 fault=close", "P3-A2 fault=fsync",
		"P3-A2 fault=metadata", "P3-A2 fault=publish", "P3-A2 fault=completion",
		"P3-A2 fault=late_cancel completed=true object=intact",
		"P3-A3 rejected=parent_missing", "P3-A3 rejected=parent_not_completed",
		"P3-A3 rejected=chain_broken", "P3-A3 rejected=server_uuid_changed",
		"P3-A3 rejected=server_version_changed", "P3-A3 rejected=tool_version_changed",
		"P3-A3 rejected=chain_length_reached",
		"P3-A4 kill=SIGKILL restart=real ledger=empty pending=gone work=gone active=untouched foreign=untouched second_recover=noop",
	} {
		if !strings.Contains(text, marker) {
			t.Fatalf("missing P3 worker evidence %q", marker)
		}
	}
	if strings.Contains(text, "SKIP") || strings.Count(text, "raw_tool_bytes=exact authenticated_eof=true snapshot_sha256=") != 3 {
		t.Fatal("P3 worker skipped a case or omitted authenticated snapshot evidence")
	}
	t.Log(text)
}
