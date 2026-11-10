package resources

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/credential"
	"github.com/flyingEirc/Intern/internal/pgtest"
)

func newService(t *testing.T) (*Service, *credential.Sealer, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "metadata")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(t.Context(), pgtest.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	keyFile := filepath.Join(t.TempDir(), "kek.json")
	if err = artifact.GenerateKey(keyFile); err != nil {
		t.Fatal(err)
	}
	sealer, err := credential.Open(keyFile, dir)
	if err != nil {
		t.Fatal(err)
	}
	workRoot := filepath.Join(t.TempDir(), "work")
	if err = os.MkdirAll(workRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	return &Service{Store: database, Settings: Settings{
		Sealer: sealer, KeyFile: keyFile, MetadataDir: dir, WorkRoot: workRoot,
		XtrabackupPath: "/usr/bin/false", MinWorkBytes: 1, MinBackupBytes: 1,
	}}, sealer, dir
}

func createSource(t *testing.T, service *Service, sealer store.SecretSealer, name, socket string) store.Source {
	t.Helper()
	source, err := service.Store.CreateSource(t.Context(), store.SourceInput{
		Name: name, Socket: socket, Datadir: "/var/lib/mysql", Username: "backup",
		Secret: []byte("{{SECRET_service_source}}"),
	}, sealer, store.SystemAudit("sources.create", store.AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func createRepository(t *testing.T, service *Service, sealer store.SecretSealer, in store.RepositoryInput) store.Repository {
	t.Helper()
	repository, err := service.Store.CreateRepository(t.Context(), in, sealer, store.SystemAudit("repositories.create", store.AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func closedPort(t *testing.T) int64 {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	return int64(port)
}

// TestUnconfiguredServiceRefusesDependentOperations covers the 503 contract: with
// no KEK and no preflight inputs, the operations report ErrUnavailable instead of
// failing per request or panicking.
func TestUnconfiguredServiceRefusesDependentOperations(t *testing.T) {
	service := &Service{Store: nil}
	if _, err := service.PreflightSource(t.Context(), "any", "any"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("preflight = %v", err)
	}
	if service.Settings.Credentials() || service.Settings.Preflight() {
		t.Fatal("an empty settings value reported capabilities")
	}
}

// TestProbeLocalDoesNotRequireSealer proves a local repository probe writes and
// removes a staged object without a KEK, because it carries no secret.
func TestProbeLocalDoesNotRequireSealer(t *testing.T) {
	database, err := store.Open(t.Context(), pgtest.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	root := filepath.Join(t.TempDir(), "root")
	repository, err := database.CreateRepository(t.Context(), store.RepositoryInput{
		Name: "local", Kind: store.RepositoryLocal, TLSMode: store.TLSModeNone, Root: root,
	}, nil, store.SystemAudit("repositories.create", store.AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: database}
	outcome, err := service.ProbeRepository(t.Context(), repository.ID)
	if err != nil || !outcome.OK() {
		t.Fatalf("local probe without sealer = %+v %v", outcome, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("probe left entries: %v", entries)
	}
}

// TestProbeFTPWithoutSealerIsUnavailable covers the secret-bearing probe: FTP
// still requires a KEK, even when the row already exists.
func TestProbeFTPWithoutSealerIsUnavailable(t *testing.T) {
	service, sealer, _ := newService(t)
	repository := createRepository(t, service, sealer, store.RepositoryInput{
		Name: "ftp", Kind: store.RepositoryFTP, TLSMode: store.TLSModePlain,
		FTP:    &store.FTPRepositoryConfig{Host: "127.0.0.1", Port: closedPort(t), BasePath: "/b", Username: "u"},
		Secret: []byte("{{SECRET_service_ftp}}"),
	})
	unconfigured := &Service{Store: service.Store}
	if _, err := unconfigured.ProbeRepository(t.Context(), repository.ID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ftp probe without sealer = %v", err)
	}
}

func TestProbeLocalRequiresExistingPrivateRoot(t *testing.T) {
	service, sealer, _ := newService(t)
	repository := createRepository(t, service, sealer, store.RepositoryInput{
		Name: "local", Kind: store.RepositoryLocal, TLSMode: store.TLSModeNone, Root: filepath.Join(t.TempDir(), "root"),
	})
	outcome, err := service.ProbeRepository(t.Context(), repository.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.OK() {
		t.Fatalf("probe = %+v", outcome.Failure)
	}
	// A root with the wrong mode is refused by local.Open.
	root := filepath.Join(t.TempDir(), "loose")
	if err = os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	loose := createRepository(t, service, sealer, store.RepositoryInput{
		Name: "loose", Kind: store.RepositoryLocal, TLSMode: store.TLSModeNone, Root: root,
	})
	outcome, err = service.ProbeRepository(t.Context(), loose.ID)
	if err != nil || outcome.Failure == nil || outcome.Failure.Code != "storage_unwritable" {
		t.Fatalf("loose root = %+v %v", outcome, err)
	}
}

func TestProbeFTPSurfacesSafeTransportFailure(t *testing.T) {
	service, sealer, _ := newService(t)
	repository := createRepository(t, service, sealer, store.RepositoryInput{
		Name: "ftp", Kind: store.RepositoryFTP, TLSMode: store.TLSModePlain,
		FTP:    &store.FTPRepositoryConfig{Host: "127.0.0.1", Port: closedPort(t), BasePath: "/b", Username: "u"},
		Secret: []byte("{{SECRET_service_ftp}}"),
	})
	outcome, err := service.ProbeRepository(t.Context(), repository.ID)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Failure == nil || outcome.Failure.Code == "" || outcome.Failure.ReplyCode != 0 {
		t.Fatalf("probe = %+v", outcome.Failure)
	}
}

func TestPreflightSourceRejectsNonLocalRepository(t *testing.T) {
	service, sealer, _ := newService(t)
	source := createSource(t, service, sealer, "source", filepath.Join(t.TempDir(), "absent.sock"))
	ftp := createRepository(t, service, sealer, store.RepositoryInput{
		Name: "ftp", Kind: store.RepositoryFTP, TLSMode: store.TLSModePlain,
		FTP:    &store.FTPRepositoryConfig{Host: "127.0.0.1", Port: closedPort(t), BasePath: "/b", Username: "u"},
		Secret: []byte("{{SECRET_service_ftp}}"),
	})
	if _, err := service.PreflightSource(t.Context(), source.ID, ftp.ID); !errors.Is(err, ErrLocalRepositoryRequired) {
		t.Fatalf("ftp backup root = %v", err)
	}
}

// TestPreflightFailureKeepsTheOutcomeStructured runs the real physical preflight
// against an absent socket; the failure must be a preflight outcome, not a Go
// error, so the caller can render 200 with ok=false.
func TestPreflightFailureKeepsTheOutcomeStructured(t *testing.T) {
	service, sealer, _ := newService(t)
	source := createSource(t, service, sealer, "source", filepath.Join(t.TempDir(), "absent.sock"))
	local := createRepository(t, service, sealer, store.RepositoryInput{
		Name: "local", Kind: store.RepositoryLocal, TLSMode: store.TLSModeNone, Root: filepath.Join(t.TempDir(), "root"),
	})
	outcome, err := service.PreflightSource(t.Context(), source.ID, local.ID)
	if err != nil {
		t.Fatalf("preflight returned an error: %v", err)
	}
	assertStructuredPreflightFailure(t, outcome)
}

// TestExpiredContextIsAnError proves an already-expired context is an error, not
// a preflight observation. Cancellation inside the native checks becomes the
// "canceled" code, which the physical package covers.
func TestExpiredContextIsAnError(t *testing.T) {
	service, sealer, _ := newService(t)
	source := createSource(t, service, sealer, "source", filepath.Join(t.TempDir(), "absent.sock"))
	local := createRepository(t, service, sealer, store.RepositoryInput{
		Name: "local", Kind: store.RepositoryLocal, TLSMode: store.TLSModeNone, Root: filepath.Join(t.TempDir(), "root"),
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Nanosecond)
	defer cancel()
	if _, err := service.PreflightSource(ctx, source.ID, local.ID); err == nil {
		t.Fatal("an expired context reported success")
	}
}

func assertStructuredPreflightFailure(t *testing.T, outcome PreflightOutcome) {
	t.Helper()
	if outcome.OK() || outcome.Failure == nil {
		t.Fatalf("outcome = %+v", outcome)
	}
	if outcome.Failure.Check == "" || outcome.Failure.Code == "" || outcome.Failure.Message == "" {
		t.Fatalf("failure = %+v", outcome.Failure)
	}
	if record := outcome.Record(); record.OK || record.Checks == nil || record.Warnings == nil {
		t.Fatalf("record = %+v", record)
	}
}

func TestPolicyPreflightStopsAtSourceFailure(t *testing.T) {
	service, sealer, _ := newService(t)
	source := createSource(t, service, sealer, "source", filepath.Join(t.TempDir(), "absent.sock"))
	local := createRepository(t, service, sealer, store.RepositoryInput{
		Name: "local", Kind: store.RepositoryLocal, TLSMode: store.TLSModeNone, Root: filepath.Join(t.TempDir(), "root"),
	})
	replica := createRepository(t, service, sealer, store.RepositoryInput{
		Name: "ftp", Kind: store.RepositoryFTP, TLSMode: store.TLSModePlain,
		FTP:    &store.FTPRepositoryConfig{Host: "127.0.0.1", Port: closedPort(t), BasePath: "/b", Username: "u"},
		Secret: []byte("{{SECRET_service_ftp}}"),
	})
	replicaID := replica.ID
	outcome, err := service.PolicyPreflight(t.Context(), store.Policy{
		SourceID: source.ID, LocalRepositoryID: local.ID, ReplicaRepositoryID: &replicaID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.OK() || outcome.Failure == nil || outcome.Failure.Check == "replica" {
		t.Fatalf("policy preflight = %+v", outcome.Failure)
	}
}

func TestProbeReportsUnwritableReplicaForPolicy(t *testing.T) {
	service, sealer, _ := newService(t)
	source := createSource(t, service, sealer, "source", filepath.Join(t.TempDir(), "absent.sock"))
	local := createRepository(t, service, sealer, store.RepositoryInput{
		Name: "local", Kind: store.RepositoryLocal, TLSMode: store.TLSModeNone, Root: filepath.Join(t.TempDir(), "root"),
	})
	outcome, err := service.ProbeRepository(t.Context(), local.ID)
	if err != nil || !outcome.OK() {
		t.Fatalf("local probe = %+v %v", outcome, err)
	}
	localOutcome, err := service.PolicyPreflight(t.Context(), store.Policy{
		SourceID: source.ID, LocalRepositoryID: local.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if localOutcome.OK() || localOutcome.Failure.Check != "identity" {
		t.Fatalf("policy preflight = %+v", localOutcome.Failure)
	}
}
