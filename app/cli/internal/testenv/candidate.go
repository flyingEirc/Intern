//go:build integration

package testenv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	tc "github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
)

var toolVersion = regexp.MustCompile(`\b[0-9]+\.[0-9]+\.[0-9]+\b`)

func (s *Suite) buildCandidate(buildCtx context.Context) error {
	arch := strings.TrimPrefix(s.Candidate.Platform, "linux/")
	pkg, err := s.Profile.ClientPackage(arch)
	if err != nil {
		return err
	}
	values := map[string]string{"GO_IMAGE": s.Contract.BuilderImage, "MYSQL_IMAGE": s.Profile.Image, "CLIENT_RPM_URL": pkg.URL, "CLIENT_RPM_SHA256": pkg.SHA256, "SOURCE_SHA": s.Candidate.SourceSHA, "TARGETARCH": arch}
	args := make(map[string]*string, len(values))
	for key, value := range values {
		args[key] = &value
	}
	request := tc.ContainerRequest{FromDockerfile: tc.FromDockerfile{Context: s.Root, Dockerfile: "Dockerfile", Repo: "intern-quality", Tag: s.Profile.ID + "-" + s.Candidate.SourceSHA[:12] + "-" + arch, BuildArgs: args, KeepImage: true}, Entrypoint: []string{"/bin/sleep"}, Cmd: []string{"infinity"}, Labels: maps.Clone(s.Labels), ImagePlatform: s.Candidate.Platform, ConfigModifier: rootUser, HostConfigModifier: boundedHost, WaitingFor: wait.ForExec([]string{"/usr/bin/test", "-x", "/usr/local/bin/intern"}).WithStartupTimeout(30 * time.Second)}
	probe, err := tc.GenericContainer(buildCtx, tc.GenericContainerRequest{ContainerRequest: request, Started: true})
	tc.CleanupContainer(s.T, probe, tc.StopTimeout(5*time.Second))
	if err != nil {
		return fmt.Errorf("build real CLI candidate: %w", err)
	}
	ctx, cancel := context.WithTimeout(s.T.Context(), 5*time.Minute)
	defer cancel()
	inspected, err := probe.Inspect(ctx)
	if err != nil {
		return err
	}
	if inspected.Config.Labels["org.opencontainers.image.revision"] != s.Candidate.SourceSHA {
		return errors.New("candidate image source label mismatch")
	}
	s.Image = inspected.Image
	s.Candidate.ImageID = inspected.Image
	output, code, err := execContainer(ctx, probe, "10001", []string{"sha256sum", "/usr/local/bin/intern"})
	if err != nil || code != 0 {
		return errors.New("cannot fingerprint candidate executable")
	}
	fields := strings.Fields(output)
	if len(fields) != 2 {
		return errors.New("invalid candidate executable fingerprint")
	}
	s.Candidate.BinarySHA256 = fields[0]
	return s.measureTools(ctx, probe)
}

func (s *Suite) measureTools(ctx context.Context, probe tc.Container) error {
	for _, tool := range []string{"mysql", "mysqldump", "mysqlbinlog"} {
		output, code, err := execContainer(ctx, probe, "10001", []string{tool, "--no-defaults", "--version"})
		if err != nil || code != 0 {
			return fmt.Errorf("required official client missing: %s", tool)
		}
		version := toolVersion.FindString(output)
		if version != s.Profile.Version {
			return fmt.Errorf("%s does not match locked profile", tool)
		}
		s.Candidate.ToolVersions[tool] = version
	}
	return nil
}
func rootUser(config *container.Config) { config.User = "0:0" }
func boundedHost(config *container.HostConfig) {
	enabled := true
	config.Init = &enabled
	config.Memory = 768 << 20
	config.NanoCPUs = 2_000_000_000
}

func execContainer(ctx context.Context, c tc.Container, user string, args []string, extra ...tcexec.ProcessOption) (string, int, error) {
	options := append([]tcexec.ProcessOption{tcexec.WithUser(user), tcexec.Multiplexed()}, extra...)
	code, reader, err := c.Exec(ctx, args, options...)
	if err != nil {
		return "", code, err
	}
	data, err := io.ReadAll(io.LimitReader(reader, 2<<20+1))
	if err != nil {
		return "", code, err
	}
	if len(data) > 2<<20 {
		return "", code, errors.New("container command exceeded diagnostic limit")
	}
	return string(data), code, nil
}
