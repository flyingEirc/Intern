package qualitygate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/flyingEirc/Intern/internal/fsutil"
)

type RunOptions struct {
	Profile, Platform, SourceSHA, Directory string
	CI                                      bool
}

func Run(ctx context.Context, opts RunOptions, stdout, stderr io.Writer) (Result, error) {
	contract, err := Load()
	if err != nil {
		return Result{}, err
	}
	if _, err = contract.Profile(opts.Profile); err != nil {
		return Result{}, err
	}
	if opts.CI && opts.SourceSHA == "" {
		return Result{}, errors.New("CI gate requires the expected source SHA")
	}
	dir, err := newReportDirectory(opts.Directory, opts.Profile)
	if err != nil {
		return Result{}, err
	}
	events, err := fsutil.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Result{}, err
	}
	environment, err := containerEnvironment(ctx, opts, dir)
	if err != nil {
		return Result{}, errors.Join(err, events.Close())
	}
	cmd := exec.CommandContext(ctx, "go")
	cmd.Args = append(cmd.Args, "test", "-json", "-tags=integration", "-count=1", "-shuffle=on", "-parallel=2", "-timeout=35m", "./app/cli/integration/...")
	cmd.Env = environment
	cmd.Stdout = io.MultiWriter(events, stdout)
	cmd.Stderr = stderr
	runErr := cmd.Run()
	closeErr := events.Close()
	if _, err = fmt.Fprintf(stderr, "container evidence: %s\n", dir); err != nil {
		return Result{}, errors.Join(err, runErr, closeErr)
	}
	return verifyRun(dir, opts, runErr, closeErr)
}

func verifyRun(dir string, opts RunOptions, runErr, closeErr error) (Result, error) {
	var candidate Candidate
	if err := ReadJSON(filepath.Join(dir, "candidate.json"), &candidate); err != nil {
		return Result{}, errors.Join(err, runErr, closeErr)
	}
	if opts.SourceSHA == "" {
		opts.SourceSHA = candidate.SourceSHA
	}
	if opts.Platform == "" {
		opts.Platform = candidate.Platform
	}
	result, verifyErr := VerifyDirectory(dir, Verification{SourceSHA: opts.SourceSHA, Profile: opts.Profile, Platform: opts.Platform, RequireClean: opts.CI})
	return result, errors.Join(runErr, closeErr, verifyErr)
}

func newReportDirectory(root, profile string) (string, error) {
	if root == "" {
		root = ".cache/quality"
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(abs, 0o700); err != nil {
		return "", err
	}
	return os.MkdirTemp(abs, profile+"-")
}

func containerEnvironment(ctx context.Context, opts RunOptions, dir string) ([]string, error) {
	overrides := map[string]string{"INTERN_TEST_PROFILE": opts.Profile, "INTERN_TEST_REPORT_DIR": dir, "INTERN_TEST_PLATFORM": opts.Platform, "INTERN_TEST_SOURCE_SHA": opts.SourceSHA}
	if os.Getenv("DOCKER_HOST") == "" {
		cmd := exec.CommandContext(ctx, "docker")
		cmd.Args = append(cmd.Args, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
		output, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("resolve active Docker context: %w", err)
		}
		overrides["DOCKER_HOST"] = strings.TrimSpace(string(output))
	}
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; !replaced {
			environment = append(environment, entry)
		}
	}
	for key, value := range overrides {
		environment = append(environment, key+"="+value)
	}
	return environment, nil
}
