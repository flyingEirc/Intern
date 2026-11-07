//go:build integration

package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/client"
	tc "github.com/testcontainers/testcontainers-go"
)

// fixtureImageRepo is the repository of every image the Web acceptance harness
// builds from a Dockerfile. .github/scripts/web-image-clean.sh keeps the newest
// tags of each fixture in it and never touches an image outside it.
const fixtureImageRepo = "intern-quality-web"

// The Dockerfiles the harness builds. files are the build context files a
// Dockerfile copies; they are part of the image tag, and a Dockerfile that
// copies anything else is refused rather than built under a tag that does not
// cover it.
var (
	ftpImage = fixtureDockerfile{
		name: "ftp", contextDir: "app/web/integration", dockerfile: "ftp.Dockerfile",
		files: []string{"ftp_entrypoint.sh"}, log: os.Stderr,
	}
	ftpsImage = fixtureDockerfile{
		name: "ftps", contextDir: "app/web/integration", dockerfile: "ftps.Dockerfile",
		files: []string{"ftps_entrypoint.sh"}, log: os.Stderr,
	}
	restoreImage = fixtureDockerfile{
		name: "restore", contextDir: "app/web/integration", dockerfile: "restore.Dockerfile", log: os.Stderr,
	}
	// clientToolsImage is the client-tools stage of the repository Dockerfile.
	// Its build context is the whole repository, but the stage copies nothing
	// from it.
	clientToolsImage = fixtureDockerfile{name: "client-tools", dockerfile: "Dockerfile", target: "client-tools"}
)

// fixtureDockerfile describes one image the harness builds with testcontainers.
//
// The image is built under a tag that is determined by its inputs and is never
// removed by the test. Docker's classic builder, which testcontainers uses,
// shares every cached intermediate image between all builds of the same
// Dockerfile on a daemon, and Docker 29's containerd image store removes all of
// a removed image's untagged ancestors without asking whether another build
// still uses them. A testcontainers-built image that is not kept is removed
// when its container terminates, so one job's cleanup deleted the parent image
// of another job's build, which failed with "failed to export image: No such
// image". With KeepImage nothing is removed, and the content-determined tag
// means a job can only ever reuse or re-tag an image identical to its own.
type fixtureDockerfile struct {
	name       string
	contextDir string
	dockerfile string
	files      []string
	target     string
	log        io.Writer
}

// contextFile is one build context file the image copies.
type contextFile struct {
	path       string
	executable bool
	data       []byte
}

// buildInputs is everything that determines the image besides the arguments.
type buildInputs struct {
	dockerfile []byte
	files      []contextFile
}

// build returns the request to build the fixture image in the build context of
// root. The tag covers the Dockerfile, the context files it copies, the target
// stage, the architecture and every build argument, and inside GitHub Actions
// the runner (see runnerTagSuffix).
func (f fixtureDockerfile) build(t *testing.T, root, arch string, args map[string]string) tc.FromDockerfile {
	t.Helper()
	context := filepath.Join(root, f.contextDir)
	inputs, err := f.read(context)
	require(t, err)
	scope, err := runnerTagSuffix()
	require(t, err)
	build := tc.FromDockerfile{
		Context: context, Dockerfile: f.dockerfile, Repo: fixtureImageRepo, Tag: f.tag(arch, inputs, args) + scope,
		BuildArgs: buildArgs(args), BuildLogWriter: f.log, KeepImage: true,
	}
	if f.target != "" {
		build.BuildOptionsModifier = func(options *client.ImageBuildOptions) { options.Target = f.target }
	}
	return build
}

// runnerTagSuffix scopes a tag to the self-hosted runner that builds it. Two
// builds that finish at the same time under one name with different image ids,
// which is what the cold builds of one Dockerfile in two jobs do, make Docker's
// containerd image store delete the existing tag and create it again, and the
// loser fails with "AlreadyExists" (moby daemon/containerd/image_tag.go). A
// runner runs one job at a time, so a name of its own is never built twice at
// once, while identical builds of different runners still share every cached
// layer. The suffix is the one the CLI harness and cli-quality-clean.sh derive
// for their runner label, the first 8 hex digits of sha256(RUNNER_NAME); a run
// outside GitHub Actions has none.
func runnerTagSuffix() (string, error) {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		return "", nil
	}
	runner := os.Getenv("RUNNER_NAME")
	if strings.TrimSpace(runner) == "" {
		return "", errors.New("RUNNER_NAME is required in GitHub Actions: it keeps the fixture image tags of concurrent jobs apart")
	}
	sum := sha256.Sum256([]byte(runner))
	return "-" + hex.EncodeToString(sum[:4]), nil
}

// read loads the inputs and checks the declared context files against what the
// Dockerfile copies.
func (f fixtureDockerfile) read(context string) (buildInputs, error) {
	dockerfile, err := os.ReadFile(filepath.Join(context, f.dockerfile))
	if err != nil {
		return buildInputs{}, err
	}
	copied, err := copiedContextFiles(dockerfile, f.target)
	if err != nil {
		return buildInputs{}, fmt.Errorf("%s: %w", f.dockerfile, err)
	}
	declared := slices.Sorted(slices.Values(f.files))
	if !slices.Equal(copied, declared) {
		return buildInputs{}, fmt.Errorf("%s copies %q from its build context but the fixture declares %q: the image tag must cover exactly the copied files", f.dockerfile, copied, declared)
	}
	inputs := buildInputs{dockerfile: dockerfile}
	for _, name := range declared {
		file, err := readContextFile(context, name)
		if err != nil {
			return buildInputs{}, err
		}
		inputs.files = append(inputs.files, file)
	}
	return inputs, nil
}

func readContextFile(context, name string) (contextFile, error) {
	path := filepath.Join(context, filepath.FromSlash(name))
	info, err := os.Stat(path)
	if err != nil {
		return contextFile{}, err
	}
	if !info.Mode().IsRegular() {
		return contextFile{}, fmt.Errorf("%s is not a regular file", name)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return contextFile{}, err
	}
	return contextFile{path: name, executable: info.Mode().Perm()&0o111 != 0, data: data}, nil
}

// tag is <name>-<12 hex digits of the inputs' SHA-256>-<arch>, the shape of the
// CLI candidate tags (intern-quality:<profile>-<sha12>-<arch>). The inputs are
// length-prefixed so no two different inputs encode to the same bytes.
func (f fixtureDockerfile) tag(arch string, inputs buildInputs, args map[string]string) string {
	var encoded bytes.Buffer
	field := func(kind string, data []byte) {
		fmt.Fprintf(&encoded, "%s %d\n", kind, len(data))
		encoded.Write(data)
		encoded.WriteByte('\n')
	}
	field("name", []byte(f.name))
	field("arch", []byte(arch))
	field("target", []byte(f.target))
	field("dockerfile", inputs.dockerfile)
	for _, file := range inputs.files {
		mode := "0644"
		if file.executable {
			mode = "0755"
		}
		field("file "+file.path+" "+mode, file.data)
	}
	for _, key := range slices.Sorted(maps.Keys(args)) {
		field("arg "+key, []byte(args[key]))
	}
	sum := sha256.Sum256(encoded.Bytes())
	return fmt.Sprintf("%s-%x-%s", f.name, sum[:6], arch)
}

func buildArgs(args map[string]string) map[string]*string {
	if len(args) == 0 {
		return nil
	}
	pointers := make(map[string]*string, len(args))
	for key, value := range args {
		pointers[key] = &value
	}
	return pointers
}

// copiedContextFiles returns the sorted build context paths the Dockerfile's
// COPY and ADD instructions read. Without a target the whole Dockerfile is
// built, and copying from another stage reads no context. With a target only
// that stage is part of the image, and it must not copy from another stage,
// whose context files the tag would then have to cover as well.
func copiedContextFiles(dockerfile []byte, target string) ([]string, error) {
	var sources []string
	inStage, foundTarget := target == "", false
	for _, instruction := range dockerfileInstructions(dockerfile) {
		fields := strings.Fields(instruction)
		switch strings.ToUpper(fields[0]) {
		case "FROM":
			if target != "" {
				inStage = strings.EqualFold(stageName(fields), target)
				foundTarget = foundTarget || inStage
			}
		case "COPY", "ADD":
			if !inStage {
				continue
			}
			found, err := instructionSources(fields, target != "")
			if err != nil {
				return nil, fmt.Errorf("%s: %w", instruction, err)
			}
			sources = append(sources, found...)
		}
	}
	if target != "" && !foundTarget {
		return nil, fmt.Errorf("no stage named %q", target)
	}
	slices.Sort(sources)
	return slices.Compact(sources), nil
}

// dockerfileInstructions returns every instruction on one line: comment lines
// are dropped and a trailing backslash joins the next line.
func dockerfileInstructions(dockerfile []byte) []string {
	var instructions []string
	var pending strings.Builder
	flush := func() {
		if text := strings.TrimSpace(pending.String()); text != "" {
			instructions = append(instructions, text)
		}
		pending.Reset()
	}
	for line := range strings.SplitSeq(string(dockerfile), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if joined, found := strings.CutSuffix(line, "\\"); found {
			pending.WriteString(joined + " ")
			continue
		}
		pending.WriteString(line)
		flush()
	}
	flush()
	return instructions
}

// stageName is the name a FROM instruction gives its stage, or "".
func stageName(fields []string) string {
	for i := 1; i+1 < len(fields); i++ {
		if strings.EqualFold(fields[i], "AS") {
			return fields[i+1]
		}
	}
	return ""
}

// instructionSources returns the context paths one COPY or ADD instruction
// reads. Forms whose sources cannot be named without running Docker (globs,
// variables, JSON arrays, here-documents) are errors, never ignored.
func instructionSources(fields []string, rejectStageCopy bool) ([]string, error) {
	operands, fromStage := splitOperands(fields[1:])
	if fromStage {
		if rejectStageCopy {
			return nil, errors.New("copies from another stage")
		}
		return nil, nil
	}
	if len(operands) < 2 {
		return nil, errors.New("has no source and destination")
	}
	var sources []string
	for _, source := range operands[:len(operands)-1] {
		if isRemoteSource(fields[0], source) {
			continue
		}
		if strings.ContainsAny(source, "*?[]$<") {
			return nil, fmt.Errorf("source %q is a pattern, a variable or a form this check cannot resolve", source)
		}
		sources = append(sources, filepath.ToSlash(filepath.Clean(source)))
	}
	return sources, nil
}

// splitOperands separates a COPY or ADD instruction's operands from its flags
// and reports whether it copies from another stage (--from).
func splitOperands(args []string) (operands []string, fromStage bool) {
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--from="):
			return nil, true
		case strings.HasPrefix(arg, "--"):
		default:
			operands = append(operands, arg)
		}
	}
	return operands, false
}

// isRemoteSource reports an ADD source that is downloaded, so it reads no
// context file; the Dockerfile text pins what it downloads.
func isRemoteSource(command, source string) bool {
	return strings.EqualFold(command, "ADD") && (strings.HasPrefix(source, "https://") || strings.HasPrefix(source, "http://"))
}
