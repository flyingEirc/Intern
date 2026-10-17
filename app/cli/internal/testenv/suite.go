//go:build integration

// Package testenv manages real, isolated database and CLI container fixtures.
package testenv

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
	tc "github.com/testcontainers/testcontainers-go"

	"github.com/flyingEirc/Intern/app/cli/internal/qualitygate"
	"github.com/flyingEirc/Intern/internal/fsutil"
)

type Suite struct {
	T               *testing.T
	Contract        qualitygate.Contract
	Profile         qualitygate.Profile
	Candidate       qualitygate.Candidate
	Root, ReportDir string
	Image           string
	slots           chan struct{}
}

func NewSuite(t *testing.T) *Suite {
	t.Helper()
	s := &Suite{T: t}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	must(t, s.loadSettings())
	must(t, s.preflight(ctx))
	must(t, s.buildCandidate(ctx))
	must(t, s.writeJSON("candidate.json", s.Candidate))
	return s
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func (s *Suite) loadSettings() error {
	var err error
	s.Contract, err = qualitygate.Load()
	if err != nil {
		return err
	}
	id := os.Getenv("INTERN_TEST_PROFILE")
	if id == "" {
		id = "mysql-9.7.1"
	}
	s.Profile, err = s.Contract.Profile(id)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	s.Root = filepath.Clean(filepath.Join(cwd, "..", "..", ".."))
	if _, err = os.Stat(filepath.Join(s.Root, "go.work")); err != nil {
		return errors.New("integration tests must run from app/cli/integration")
	}
	s.ReportDir = os.Getenv("INTERN_TEST_REPORT_DIR")
	if s.ReportDir == "" {
		return errors.New("INTERN_TEST_REPORT_DIR is required; use mise run test:containers")
	}
	if err = prepareReportDir(s.ReportDir); err != nil {
		return err
	}
	parallel := 1
	if os.Getenv("INTERN_CONTAINER_PARALLEL") == "2" {
		parallel = 2
	}
	s.slots = make(chan struct{}, parallel)
	return nil
}
func prepareReportDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != "events.jsonl" {
			return errors.New("report directory is not fresh")
		}
	}
	return os.Mkdir(filepath.Join(dir, "receipts"), 0700)
}
func (s *Suite) preflight(ctx context.Context) (retErr error) {
	if os.Getenv("TESTCONTAINERS_RYUK_DISABLED") == "true" {
		return errors.New("quality gate requires Ryuk cleanup")
	}
	docker, err := tc.NewDockerClientWithOpts(ctx)
	if err != nil {
		return fmt.Errorf("docker provider unavailable: %w", err)
	}
	defer fsutil.Close(&retErr, docker)
	info, err := docker.Info(ctx, client.InfoOptions{})
	if err != nil {
		return fmt.Errorf("docker provider unavailable: %w", err)
	}
	platform, err := nativePlatform(info.Info.OSType, info.Info.Architecture)
	if err != nil {
		return err
	}
	source, dirty, err := sourceIdentity(ctx, s.Root)
	if err != nil {
		return err
	}
	if expected := os.Getenv("INTERN_TEST_SOURCE_SHA"); expected != "" && expected != source {
		return errors.New("test source differs from requested commit")
	}
	s.Candidate = qualitygate.Candidate{FormatVersion: 1, SourceSHA: source, SourceDirty: dirty, Profile: s.Profile.ID, Platform: platform, HarnessPlatform: runtime.GOOS + "/" + runtime.GOARCH, GoVersion: runtime.Version(), RuntimeVersion: info.Info.ServerVersion, TestcontainersVersion: s.Contract.TestcontainersVersion, BuilderImage: s.Contract.BuilderImage, DatabaseImage: s.Profile.Image, ToolVersions: map[string]string{}, CIURL: ciURL()}
	return nil
}
func nativePlatform(osType, architecture string) (string, error) {
	arch := normalizeArch(architecture)
	if osType != "linux" || arch == "" {
		return "", errors.New("quality gate requires a native Linux amd64/arm64 daemon")
	}
	platform := "linux/" + arch
	if requested := os.Getenv("INTERN_TEST_PLATFORM"); requested != "" && requested != platform {
		return "", errors.New("requested platform does not match native Docker daemon")
	}
	return platform, nil
}
func normalizeArch(arch string) string {
	switch arch {
	case "amd64", "x86_64":
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	default:
		return ""
	}
}
func sourceIdentity(ctx context.Context, root string) (string, bool, error) {
	head, err := gitOutput(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return "", false, err
	}
	status, err := gitOutput(ctx, root, "status", "--porcelain", "--untracked-files=normal", "--", ".")
	return strings.TrimSpace(head), strings.TrimSpace(status) != "", err
}
func gitOutput(ctx context.Context, root string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git")
	cmd.Dir = root
	cmd.Args = append(cmd.Args, args...)
	out, err := cmd.Output()
	return string(out), err
}
func ciURL() string {
	if os.Getenv("GITHUB_RUN_ID") == "" {
		return ""
	}
	return os.Getenv("GITHUB_SERVER_URL") + "/" + os.Getenv("GITHUB_REPOSITORY") + "/actions/runs/" + os.Getenv("GITHUB_RUN_ID")
}
func (s *Suite) writeJSON(name string, value any) (retErr error) {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	path := filepath.Join(s.ReportDir, name)
	f, err := fsutil.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer fsutil.Close(&retErr, f)
	_, err = f.Write(data)
	return errors.Join(err, f.Sync())
}
