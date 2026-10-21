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

const s1AlpineImage = "alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6"

// Keep these exact case names synchronized with the worker and CI evidence gate.
var s1WorkerCases = []string{
	"publish", "interrupted_upload", "quota_552", "permission_550", "source_error",
	"short_write", "data_close_error", "stage_close_error", "cancel_upload", "cleanup_retry",
	"instance_isolation", "marker_conflict", "path_tls_rejection", "ciphertext_resume",
	"close_drain", "rolling_idle", "pipeline_completion", "rename_ack_lost",
}

func TestS1NativeContainers(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()
	docker, err := tc.NewDockerClientWithOpts(ctx)
	require(t, err)
	defer func() { require(t, docker.Close()) }()
	info, err := docker.Info(ctx, client.InfoOptions{})
	require(t, err)
	arch := s1NativeArchitecture(t, info.Info.OSType, info.Info.Architecture)
	root, err := filepath.Abs("../../..")
	require(t, err)
	binary := filepath.Join(t.TempDir(), "ftp-worker.test")
	build := exec.CommandContext(ctx, "go", "test", "-c", "-tags=integration", "-o", binary, "./internal/storage/ftp")
	build.Dir = root
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	output, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("build FTP candidate: %v\n%s", err, output)
	}
	source, dirty := physicalSourceIdentity(t, ctx, root)
	data, err := os.ReadFile(binary)
	require(t, err)
	digest := sha256.Sum256(data)
	secrets := distinctSecrets(t, 2)
	password, quotaPassword := secrets[0], secrets[1]
	nw, err := network.New(ctx)
	require(t, err)
	// Registered first, this marker is emitted only after all registered cleanup
	// actions have completed successfully; canceled test context is not reused.
	t.Cleanup(func() {
		if !t.Failed() {
			t.Log("S1-A5 fixture_cleanup: worker/server removed; private volume and network removed")
		}
	})
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		require(t, nw.Remove(cleanup))
	})
	server, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		FromDockerfile: tc.FromDockerfile{Context: filepath.Join(root, "app/web/integration"), Dockerfile: "ftp.Dockerfile"},
		ImagePlatform:  "linux/" + arch,
		Networks:       []string{nw.Name}, NetworkAliases: map[string][]string{nw.Name: {"s1-ftp"}},
		Mounts: []tc.ContainerMount{tc.VolumeMount("", "/data")},
		Files: []tc.ContainerFile{
			{Reader: strings.NewReader(password + "\n" + password + "\n"), ContainerFilePath: "/run/s1-password", FileMode: 0o600},
			{Reader: strings.NewReader(quotaPassword + "\n" + quotaPassword + "\n"), ContainerFilePath: "/run/s1-quota-password", FileMode: 0o600},
		},
		WaitingFor: wait.ForExec([]string{"/bin/sh", "-c", "test -s /data/users.pdb && nc -z 127.0.0.1 2121"}).WithStartupTimeout(30 * time.Second),
	}, Started: true})
	if server != nil {
		t.Cleanup(func() { require(t, tc.TerminateContainer(server, tc.StopTimeout(5*time.Second))) })
	}
	require(t, err)
	serverInfo, err := server.Inspect(ctx)
	require(t, err)
	volume := s1FixtureVolume(t, serverInfo)
	serverHash := s1ServerIdentity(t, ctx, server, secrets)
	worker, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		Image: s1AlpineImage, ImagePlatform: "linux/" + arch,
		Entrypoint: []string{"/bin/sleep"}, Cmd: []string{"infinity"},
		Networks: []string{nw.Name}, Mounts: []tc.ContainerMount{tc.VolumeMount(volume, "/fixture")},
		Env:            map[string]string{"INTERN_S1_ADDRESS": "s1-ftp:2121", "INTERN_S1_PASSWORD": password, "INTERN_S1_QUOTA_PASSWORD": quotaPassword},
		Files:          []tc.ContainerFile{{HostFilePath: binary, ContainerFilePath: "/tmp/ftp-worker.test", FileMode: 0o755}},
		ConfigModifier: func(config *container.Config) { config.User = "1000:1000" },
		WaitingFor:     wait.ForExec([]string{"/usr/bin/test", "-x", "/tmp/ftp-worker.test"}),
	}, Started: true})
	if worker != nil {
		t.Cleanup(func() { require(t, tc.TerminateContainer(worker, tc.StopTimeout(5*time.Second))) })
	}
	require(t, err)
	workerInfo, err := worker.Inspect(ctx)
	require(t, err)
	workerHash := strings.Fields(execOutput(t, ctx, worker, "", []string{"sha256sum", "/tmp/ftp-worker.test"}, secrets...))
	if len(workerHash) != 2 || workerHash[0] != fmt.Sprintf("%x", digest) {
		t.Fatal("executed worker differs from candidate binary")
	}
	t.Logf("S1 candidate source=%s source_dirty=%t binary_sha256=%x server_binary_sha256=%s server_image=%s worker_image=%s base_image=%s daemon=%s/%s platform=linux/%s server=Pure-FTPd-1.0.54 client=jlaffaye/ftp-v0.2.4 uid=1000 passive=EPSV/PASV:50000-50020 topology=private-network-no-NAT", source, dirty, digest, serverHash, serverInfo.Image, workerInfo.Image, s1AlpineImage, info.Info.OSType, info.Info.Architecture, arch)
	text := execOutput(t, ctx, worker, "", []string{"/tmp/ftp-worker.test", "-test.v", "-test.run=^TestS1RepositoryWorker$", "-test.timeout=5m"}, secrets...)
	s1VerifyEvidence(t, text)
	t.Log(text)
}

func s1FixtureVolume(t *testing.T, inspected *container.InspectResponse) string {
	t.Helper()
	for _, mount := range inspected.Mounts {
		if mount.Destination == "/data" && mount.Name != "" {
			return mount.Name
		}
	}
	t.Fatal("missing real server filesystem oracle volume")
	return ""
}

func s1ServerIdentity(t *testing.T, ctx context.Context, server tc.Container, secrets []string) string {
	t.Helper()
	version := execOutput(t, ctx, server, "", []string{"pure-ftpd", "--help"}, secrets...)
	if !strings.Contains(version, "1.0.54") {
		t.Fatal("compiled server version does not match exact source pin")
	}
	serverHash := strings.Fields(execOutput(t, ctx, server, "", []string{"sha256sum", "/usr/sbin/pure-ftpd"}, secrets...))
	if len(serverHash) != 2 || len(serverHash[0]) != 64 {
		t.Fatal("missing compiled daemon digest")
	}
	packages := execOutput(t, ctx, server, "", []string{"apk", "list", "--installed", "libsodium", "libssl3", "libcrypto3"}, secrets...)
	for _, pin := range []string{"libsodium-1.0.22-r0", "libssl3-3.5.9-r0", "libcrypto3-3.5.9-r0"} {
		if !strings.Contains(packages, pin) {
			t.Fatalf("runtime package pin missing: %s", pin)
		}
	}
	return serverHash[0]
}

func s1NativeArchitecture(t *testing.T, osType, architecture string) string {
	t.Helper()
	if osType != "linux" || tc.ReadConfig().Config.RyukDisabled {
		t.Fatal("S1 requires native Linux Docker and enabled resource reaper")
	}
	var arch string
	switch architecture {
	case "amd64", "x86_64":
		arch = "amd64"
	case "arm64", "aarch64":
		arch = "arm64"
	default:
		t.Fatal("unsupported native daemon architecture")
	}
	if expected := os.Getenv("INTERN_S1_ARCH"); expected != "" && arch != expected {
		t.Fatal("daemon does not match required native matrix architecture; emulation refused")
	}
	return arch
}

func s1VerifyEvidence(t *testing.T, text string) {
	t.Helper()
	if strings.Contains(text, "SKIP") || strings.Contains(text, "--- FAIL:") || !strings.Contains(text, "--- PASS: TestS1RepositoryWorker (") {
		t.Fatal("worker failed, skipped, or omitted the required entrypoint")
	}
	for _, name := range s1WorkerCases {
		if strings.Count(text, "--- PASS: TestS1RepositoryWorker/"+name+" (") != 1 {
			t.Fatalf("missing or repeated required S1 case: %s", name)
		}
		if !strings.Contains(text, " "+name+":") {
			t.Fatalf("missing required S1 state assertion marker: %s", name)
		}
	}
	if strings.Count(text, "--- PASS: TestS1RepositoryWorker/") != len(s1WorkerCases) {
		t.Fatal("worker case set differs from locked evidence carrier")
	}
}
