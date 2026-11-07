//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
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
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// p4Root is the worker's private fixture tree. The export directory is a named
// volume so the harness can start a fresh server on each published datadir.
const p4Root = "/tmp/p4"

const p4WorkerTest = "TestP4Worker"

var p4WorkerCases = []string{
	"a1-plan", "a2-local", "a5-argv", "a6-enospc", "a3-ftp", "a4-reject", "a6-cancel", "a6-sigkill", "a7-security",
}

// p4ResultMarkers are the fixed facts every worker run must report.
var p4ResultMarkers = []string{
	"P4-A1 plan target=inc1", "P4-A1 plan target=inc2", "P4-A1 plan_readonly", "P4-A1 space", "P4-A1 matrix",
	"P4-A1 ftp_plan_readonly", "P4-A2 rehearsal passed", "P4-A2 export_perms",
	"P4-A3 plan target=inc1", "P4-A3 local_payloads=removed",
	"P4-A5 argv shapes=full-only,full+1,full+2",
	"P4-A6 enospc filesystem=tmpfs phase=extracting", "P4-A6 cancel during=extracting", "P4-A6 cancel during=preparing",
	"P4-A6 sigkill inject=prepare child_pgrp=own",
	"P4-A7 secrets=clean census=", "P4-A7 supervisor mysqld_pgid_separate=true", "P4-A7 export_root symlink=refused",
}

// p4Facts are the observed values behind the argv, interrupt and security
// markers. The harness requires exactly the shapes the CI gate requires, so a
// weakened worker line fails in the harness first and never reaches the gate:
// ENOSPC must be the extractor's, a cancel must have hit a live tool, the prepare
// argv must be exact and restore-scoped, and the secret census must be non-empty.
var p4Facts = []string{
	`P4-A5 argv shapes=full-only,full\+1,full\+2 no_defaults_first=true argv_exact=true apply_log_only=after_prepare target_dir_under=restore-[0-9a-f]{32} checked=3`,
	`P4-A6 enospc filesystem=tmpfs phase=extracting tool=xbstream filled=[1-9][0-9]* code=disk_full export=none work_gone=true published_untouched=true`,
	`P4-A6 cancel during=extracting tool=xbstream tool_live=true tool_gone=true code=canceled work_gone=true pending_gone=true export_unchanged=true`,
	`P4-A6 cancel during=preparing tool=xtrabackup tool_live=true tool_gone=true code=canceled work_gone=true pending_gone=true export_unchanged=true`,
	`P4-A6 sigkill inject=prepare child_pgrp=own tool=xtrabackup tool_gone=true process_tree=gone work_gone=true pending_gone=true export_unchanged=true others_untouched=true second_recover=noop`,
	`P4-A7 secrets=clean census=[1-9][0-9]* scanned=log=[1-9][0-9]*,progress=[1-9][0-9]*,chain=[1-9][0-9]*,rehearsal=[1-9][0-9]*`,
	`P4-A2 export_perms top=0750 dir=0750 file=0640 owner=exact`,
	`P4-A7 export_root symlink=refused export_perms=top:0750,dir:0750,file:0640 owner=exact`,
}

// p4Rejections are the locked P4-A4 cases. Every one must appear with its fixed
// code before prepare.
var p4Rejections = []string{
	"backup_not_found", "backup_not_completed", "chain_broken_missing_middle", "chain_broken_wrong_parent",
	"lsn_discontinuous", "key_unavailable", "tool_version_mismatch", "matrix_unsupported", "insufficient_space",
	"source_unavailable_local_truncated", "source_unavailable_replica_unverified",
	"authentication_failed_ftp_tampered", "source_unavailable_ftp_truncated", "unconfigured_export_symlink",
}

var p4RestorePattern = regexp.MustCompile(`P4-A([23]) restore=(inc[12]) [^\n]*export=([0-9a-f]{32}) expected_snapshot=([0-9a-f]{64})`)

// TestP4NativeContainers is the combined P4 harness: one pinned MySQL source, the
// pinned Pure-FTPd fixture and a worker image that carries xtrabackup, xbstream
// and mysqld 8.4.11 together. Emulation is refused. Each published export is
// started in a brand-new pinned MySQL container and its data snapshot is compared
// with the snapshot taken at that restore point.
func TestP4NativeContainers(t *testing.T) {
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
	binary := filepath.Join(t.TempDir(), "restore-worker.test")
	build := exec.CommandContext(ctx, "go", "test", "-c", "-tags=integration", "-o", binary, "./internal/restore/acceptance")
	build.Dir = root
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("build restore worker: %v\n%s", buildErr, output)
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
	exportVolume := fmt.Sprintf("p4-export-%d", time.Now().UnixNano())
	fixture := startP4MySQL(t, ctx, mysqlImage, rootPassword, backupPassword, exportVolume)
	inspected, err := fixture.Inspect(ctx)
	require(t, err)
	mounts := physicalMounts(t, inspected)
	uid, gid := mysqlIDs(t, ctx, fixture)
	execOutput(t, ctx, fixture, "0", []string{"chown", fmt.Sprintf("%d:%d", uid, gid), "/export"})
	server := startR1FTP(t, ctx, root, nw, ftpPassword, ftpQuotaPassword)
	worker := startP4Worker(t, ctx, root, nw, mysqlImage, pxbImage, binary, mounts, exportVolume, uid, gid, map[string]string{
		"INTERN_P4_PASSWORD": backupPassword, "INTERN_P4_FTP_ADDRESS": r1NetworkAlias + ":2121",
		"INTERN_P4_FTP_PASSWORD": ftpPassword, "INTERN_P4_ROOT": p4Root,
		"INTERN_P4_EXPORT": "/export", "INTERN_P4_SMALL_WORK": "/small-work", "INTERN_P4_SMALL_EXPORT": "/small-export",
		"HOME": "/tmp", "TMPDIR": "/tmp",
	})
	workerInfo, err := worker.Inspect(ctx)
	require(t, err)
	serverInfo, err := server.Inspect(ctx)
	require(t, err)
	requirePinnedFTP(t, ctx, server, secrets)
	t.Logf("P4 candidate source=%s source_dirty=%t binary_sha256=%x mysql_image=%s pxb_image=%s worker_image=%s ftp_image=%s daemon=%s/%s platform=linux/amd64 worker_uid=%d worker_gid=%d server=Pure-FTPd-1.0.54 client=jlaffaye/ftp-v0.2.4 topology=private-network-no-NAT",
		source, dirty, digest, mysqlImage, pxbImage, workerInfo.Image, serverInfo.Image,
		info.Info.OSType, info.Info.Architecture, uid, gid)
	text := execP4Output(t, ctx, worker, "", []string{
		"/tmp/restore-worker.test", "-test.v", "-test.run=^" + p4WorkerTest + "$", "-test.timeout=34m",
	}, secrets...)
	verifyP4Evidence(t, text)
	t.Log(text)
	rootRefusal := execP4Output(t, ctx, worker, "0", []string{
		"/tmp/restore-worker.test", "-test.v", "-test.run=^TestP4RootRefusalWorker$", "-test.timeout=2m",
	}, secrets...)
	if !strings.Contains(rootRefusal, "P4-A7 root_refusal mysqld=refused execute=rehearsal_unavailable") || strings.Contains(rootRefusal, "--- FAIL:") {
		t.Fatal("missing root-refusal evidence")
	}
	t.Log(rootRefusal)
	restores := parseP4Restores(t, text)
	checked := 0
	for _, restored := range restores {
		fresh := startP4RestoredMySQL(t, ctx, mysqlImage, rootPassword, exportVolume, restored.exportID)
		db := openFixtureDB(t, ctx, fresh, "root", rootPassword)
		actual := p4Snapshot(t, ctx, db)
		require(t, db.Close())
		if actual != restored.snapshot {
			t.Fatalf("restored %s %s snapshot = %s, want %s", restored.label, restored.exportID, actual, restored.snapshot)
		}
		t.Logf("P4-A%c restored=%s container_snapshot=%s expected_snapshot=%s match=true", restored.letter, restored.label, actual, restored.snapshot)
		checked++
	}
	if checked != 4 {
		t.Fatalf("fresh-container comparisons = %d, want inc1/inc2 local and inc1/inc2 FTP", checked)
	}
	t.Logf("P4-A2 restore_count=4 exports_started=4 snapshots_match=true")
}

// execP4Output runs one worker command and returns its withheld output. The P4
// worker reports every prepare argv, plan, side effect and rehearsal result, so
// the shared 1 MiB helper limit is too small: truncation there would silently
// drop locked markers. This keeps the same secret withholding with 8 MiB.
func execP4Output(t *testing.T, ctx context.Context, target tc.Container, user string, args []string, secrets ...string) string {
	t.Helper()
	options := []tcexec.ProcessOption{tcexec.Multiplexed()}
	if user != "" {
		options = append(options, tcexec.WithUser(user))
	}
	code, reader, err := target.Exec(ctx, args, options...)
	require(t, err)
	output, err := io.ReadAll(io.LimitReader(reader, 8<<20))
	require(t, err)
	text := withholdSecrets(t, output, secrets...)
	if code != 0 {
		t.Fatalf("restore worker scenario failed code=%d: %s", code, text)
	}
	return text
}

func startP4MySQL(t *testing.T, ctx context.Context, image, rootPassword, backupPassword, exportVolume string) tc.Container {
	t.Helper()
	fixture, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		Image: image, ImagePlatform: "linux/amd64",
		Env: map[string]string{"MYSQL_ROOT_PASSWORD": rootPassword, "MYSQL_ROOT_HOST": "%", "MYSQL_DATABASE": "fixture"},
		Mounts: []tc.ContainerMount{
			tc.VolumeMount("", "/var/lib/mysql"), tc.VolumeMount("", "/var/run/mysqld"),
			tc.VolumeMount(exportVolume, "/export"),
		},
		Cmd:        []string{"--innodb-buffer-pool-size=64M", "--innodb-redo-log-capacity=33554432"},
		WaitingFor: wait.ForLog("port: 3306  MySQL Community Server").WithStartupTimeout(2 * time.Minute),
	}, Started: true})
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

func startP4Worker(t *testing.T, ctx context.Context, root string, nw *tc.DockerNetwork, mysqlImage, pxbImage, binary string,
	mounts []tc.ContainerMount, exportVolume string, uid, gid int, env map[string]string,
) tc.Container {
	t.Helper()
	mounts = append(mounts, tc.VolumeMount(exportVolume, "/export"))
	worker, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		FromDockerfile: restoreImage.build(t, root, "amd64", map[string]string{"MYSQL_IMAGE": mysqlImage, "PXB_IMAGE": pxbImage}),
		ImagePlatform:  "linux/amd64", Entrypoint: []string{"/bin/sleep"}, Cmd: []string{"infinity"},
		Networks: []string{nw.Name}, Mounts: mounts, Env: env,
		Files: []tc.ContainerFile{{HostFilePath: binary, ContainerFilePath: "/tmp/restore-worker.test", FileMode: 0o755}},
		Tmpfs: map[string]string{
			// 256 MiB: big enough that a real chain authenticates (its expansion
			// guard is the measured free space), so the a6-enospc case can fill what
			// is left and make the extractor itself run out of space. The export root
			// stays tiny: that is what the insufficient_space refusal observes.
			"/small-work":   fmt.Sprintf("rw,size=268435456,mode=0700,uid=%d,gid=%d", uid, gid),
			"/small-export": fmt.Sprintf("rw,size=1048576,mode=0700,uid=%d,gid=%d", uid, gid),
		},
		ConfigModifier: func(config *container.Config) { config.User = fmt.Sprintf("%d:%d", uid, gid) },
		WaitingFor:     wait.ForExec([]string{"/usr/bin/test", "-x", "/tmp/restore-worker.test"}).WithStartupTimeout(60 * time.Second),
	}, Started: true})
	if worker != nil {
		tc.CleanupContainer(t, worker, tc.StopTimeout(10*time.Second))
	}
	require(t, err)
	return worker
}

// startP4RestoredMySQL starts a brand-new pinned server on one published datadir.
func startP4RestoredMySQL(t *testing.T, ctx context.Context, image, rootPassword, exportVolume, id string) tc.Container {
	t.Helper()
	fresh, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		Image: image, ImagePlatform: "linux/amd64",
		Mounts:         []tc.ContainerMount{tc.VolumeMount(exportVolume, "/exports")},
		ExposedPorts:   []string{"3306/tcp"},
		Cmd:            []string{"--datadir=/exports/" + id + "/data", "--socket=/tmp/mysql.sock", "--pid-file=/tmp/mysqld.pid", "--port=3306", "--skip-log-bin", "--innodb-buffer-pool-size=64M"},
		ConfigModifier: func(config *container.Config) { config.User = "999:999" },
		WaitingFor:     wait.ForLog("port: 3306  MySQL Community Server").WithStartupTimeout(2 * time.Minute),
	}, Started: true})
	if fresh != nil {
		tc.CleanupContainer(t, fresh, tc.StopTimeout(10*time.Second))
	}
	require(t, err)
	return fresh
}

func p4Snapshot(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	hash := sha256.New()
	for _, query := range []string{
		"SELECT id, CAST(value AS CHAR) FROM fixture.records ORDER BY id",
		"SELECT id, SHA2(payload,256) FROM fixture.lsn_pressure ORDER BY id",
	} {
		rows, err := db.QueryContext(ctx, query)
		require(t, err)
		for rows.Next() {
			var id int64
			var value string
			require(t, rows.Scan(&id, &value))
			_, err = fmt.Fprintf(hash, "%d:%s\n", id, value)
			require(t, err)
		}
		require(t, rows.Err())
		require(t, rows.Close())
		_, err = hash.Write([]byte{0})
		require(t, err)
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

type p4Restore struct {
	letter   byte
	label    string
	exportID string
	snapshot string
}

func parseP4Restores(t *testing.T, text string) []p4Restore {
	t.Helper()
	matches := p4RestorePattern.FindAllStringSubmatch(text, -1)
	restores := make([]p4Restore, 0, len(matches))
	seen := map[string]bool{}
	for _, match := range matches {
		label := restoreLabel(match[1], match[2])
		if seen[label] {
			t.Fatalf("duplicate restore result %s", label)
		}
		seen[label] = true
		restores = append(restores, p4Restore{letter: match[1][0], label: label, exportID: match[3], snapshot: match[4]})
	}
	if len(restores) != 4 {
		t.Fatalf("parsed restores = %d, want 4", len(restores))
	}
	return restores
}

func restoreLabel(letter, target string) string {
	if letter == "3" {
		return target + "-ftp"
	}
	return target + "-local"
}

func verifyP4Evidence(t *testing.T, text string) {
	t.Helper()
	if strings.Contains(text, "--- SKIP:") || strings.Contains(text, "--- FAIL:") {
		t.Fatal("the restore worker skipped or failed a case")
	}
	requireP4CasePasses(t, text)
	requireP4Markers(t, text)
	requireP4Facts(t, text)
	requireP4Rejections(t, text)
	if strings.Count(text, "P4-A2 restore=") != 2 || strings.Count(text, "P4-A3 restore=") != 2 {
		t.Fatal("missing local or FTP restore evidence")
	}
	if strings.Contains(text, `status="failed"`) {
		t.Fatal("a worker case reported a failed status")
	}
}

func requireP4CasePasses(t *testing.T, text string) {
	t.Helper()
	if strings.Count(text, "--- PASS: "+p4WorkerTest+" (") != 1 {
		t.Fatal("missing or repeated P4 worker entry point")
	}
	for _, name := range p4WorkerCases {
		if strings.Count(text, "--- PASS: "+p4WorkerTest+"/"+name+" (") != 1 {
			t.Fatalf("missing or repeated P4 case: %s", name)
		}
	}
}

func requireP4Markers(t *testing.T, text string) {
	t.Helper()
	for _, marker := range p4ResultMarkers {
		if !strings.Contains(text, marker) {
			t.Fatalf("missing P4 evidence marker %q", marker)
		}
	}
}

// requireP4Facts checks the observed values behind the interrupt, argv and
// security markers, in the same shapes the CI gate requires.
func requireP4Facts(t *testing.T, text string) {
	t.Helper()
	for _, pattern := range p4Facts {
		if !regexp.MustCompile(pattern).MatchString(text) {
			t.Fatalf("missing or malformed P4 evidence fact %s", pattern)
		}
	}
}

func requireP4Rejections(t *testing.T, text string) {
	t.Helper()
	for _, name := range p4Rejections {
		if strings.Count(text, "P4-A4 rejected="+name+" ") != 1 {
			t.Fatalf("missing or repeated P4 rejection: %s", name)
		}
	}
}
