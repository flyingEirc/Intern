//go:build integration

package integration

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/client"
)

// None of these tests needs Docker: they pin how the fixture image tags are
// derived and that every Dockerfile build in this package goes through them.

func TestCopiedContextFiles(t *testing.T) {
	const stages = `# syntax=docker/dockerfile:1
FROM alpine AS build
COPY --chown=1:1 builder.sh /builder.sh
ADD https://example.com/source.tar.gz /source.tar.gz
FROM alpine AS Tools
COPY ./tools.sh /usr/bin/tools
RUN echo \
    # a comment inside the instruction
    done
FROM alpine
COPY --from=build /builder.sh /builder.sh
COPY \
    # another comment
    final.sh runtime.conf \
    /etc/
`
	for _, tt := range []struct {
		name       string
		dockerfile string
		target     string
		want       []string
		wantErr    string
	}{
		{name: "context file", dockerfile: "COPY a.sh /b", want: []string{"a.sh"}},
		{name: "lower case and several sources", dockerfile: "copy b a /dir/", want: []string{"a", "b"}},
		{name: "from another stage reads no context", dockerfile: "COPY --from=build /x /y"},
		{name: "flags are not sources", dockerfile: "COPY --chown=1:1 --chmod=755 a.sh /b", want: []string{"a.sh"}},
		{name: "remote ADD is pinned by the Dockerfile", dockerfile: "ADD https://example.com/x.tar.gz /x"},
		{name: "local ADD", dockerfile: "ADD local.tar /x", want: []string{"local.tar"}},
		{name: "paths are cleaned and deduplicated", dockerfile: "COPY ./a /x\nCOPY a /y", want: []string{"a"}},
		{name: "whole build includes every stage", dockerfile: stages, want: []string{"builder.sh", "final.sh", "runtime.conf", "tools.sh"}},
		{name: "target stage only", dockerfile: strings.Replace(stages, "COPY --from=build /builder.sh /builder.sh", "", 1), target: "tools", want: []string{"tools.sh"}},
		{name: "target stage must not copy from another stage", dockerfile: "FROM a AS x\nCOPY --from=y /1 /2", target: "x", wantErr: "copies from another stage"},
		{name: "unknown target", dockerfile: "FROM a AS x", target: "y", wantErr: `no stage named "y"`},
		{name: "glob", dockerfile: "COPY *.sh /b", wantErr: "pattern"},
		{name: "variable", dockerfile: "COPY $FILE /b", wantErr: "pattern"},
		{name: "JSON array", dockerfile: `COPY ["a b", "/c"]`, wantErr: "pattern"},
		{name: "here-document", dockerfile: "COPY <<EOF /b", wantErr: "pattern"},
		{name: "no destination", dockerfile: "COPY a", wantErr: "no source and destination"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := copiedContextFiles([]byte(tt.dockerfile), tt.target)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("copied = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFixtureTagIsDeterminedByEveryInput(t *testing.T) {
	base := fixtureDockerfile{name: "ftp", dockerfile: "ftp.Dockerfile", target: ""}
	inputs := buildInputs{dockerfile: []byte("FROM alpine\n"), files: []contextFile{{path: "a.sh", data: []byte("echo a\n")}}}
	args := map[string]string{"MYSQL_IMAGE": "mysql@sha256:1", "PXB_IMAGE": "pxb@sha256:2"}
	want := base.tag("amd64", inputs, args)
	if !regexp.MustCompile(`^ftp-[0-9a-f]{12}-amd64$`).MatchString(want) {
		t.Fatalf("tag %q is not <name>-<12 hex digits>-<arch>", want)
	}
	for range 20 {
		// Go randomizes map order; the tag must not depend on it.
		if got := base.tag("amd64", inputs, maps.Clone(args)); got != want {
			t.Fatalf("the tag changed between calls: %q, %q", got, want)
		}
	}
	changed := map[string]string{
		"name":            fixtureDockerfile{name: "ftps", dockerfile: "ftp.Dockerfile"}.tag("amd64", inputs, args),
		"target":          fixtureDockerfile{name: "ftp", dockerfile: "ftp.Dockerfile", target: "x"}.tag("amd64", inputs, args),
		"arch":            base.tag("arm64", inputs, args),
		"dockerfile":      base.tag("amd64", buildInputs{dockerfile: []byte("FROM alpine:3\n"), files: inputs.files}, args),
		"file content":    base.tag("amd64", buildInputs{dockerfile: inputs.dockerfile, files: []contextFile{{path: "a.sh", data: []byte("echo b\n")}}}, args),
		"file name":       base.tag("amd64", buildInputs{dockerfile: inputs.dockerfile, files: []contextFile{{path: "b.sh", data: []byte("echo a\n")}}}, args),
		"executable bit":  base.tag("amd64", buildInputs{dockerfile: inputs.dockerfile, files: []contextFile{{path: "a.sh", data: []byte("echo a\n"), executable: true}}}, args),
		"argument value":  base.tag("amd64", inputs, map[string]string{"MYSQL_IMAGE": "mysql@sha256:9", "PXB_IMAGE": "pxb@sha256:2"}),
		"argument name":   base.tag("amd64", inputs, map[string]string{"MYSQL_IMAGE": "mysql@sha256:1", "OTHER": "pxb@sha256:2"}),
		"argument absent": base.tag("amd64", inputs, nil),
		// Length prefixes keep a value that spans two fields apart from two fields.
		"field boundary": base.tag("amd64", inputs, map[string]string{"MYSQL_IMAGE": "mysql@sha256:1\narg PXB_IMAGE 14\npxb@sha256:2"}),
	}
	seen := map[string]string{want: "the base inputs"}
	for what, tag := range changed {
		if other, dup := seen[tag]; dup {
			t.Errorf("changing the %s gave the tag of %s: %s", what, other, tag)
		}
		seen[tag] = "the " + what
	}
}

func TestFixtureBuildKeepsTheImageUnderItsContentTag(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	root := repositoryRoot(t)
	args := map[string]string{"MYSQL_IMAGE": "mysql:8.4.11@sha256:" + strings.Repeat("a", 64), "PXB_IMAGE": "pxb@sha256:" + strings.Repeat("b", 64)}
	build := restoreImage.build(t, root, "amd64", args)
	if !build.KeepImage {
		t.Error("a fixture image must be kept: testcontainers removes a built image with all its unshared parents when its container terminates")
	}
	if build.Repo != fixtureImageRepo || !regexp.MustCompile(`^restore-[0-9a-f]{12}-amd64$`).MatchString(build.Tag) {
		t.Errorf("image = %s:%s, want %s:restore-<12 hex digits>-amd64", build.Repo, build.Tag, fixtureImageRepo)
	}
	if build.Context != filepath.Join(root, "app/web/integration") || build.Dockerfile != "restore.Dockerfile" {
		t.Errorf("build context %q Dockerfile %q", build.Context, build.Dockerfile)
	}
	if build.BuildOptionsModifier != nil {
		t.Error("the restore image builds its last stage and sets no target")
	}
}

func TestFixtureBuildPassesEveryArgumentByValue(t *testing.T) {
	args := map[string]string{"MYSQL_IMAGE": "mysql:8.4.11@sha256:" + strings.Repeat("a", 64), "PXB_IMAGE": "pxb@sha256:" + strings.Repeat("b", 64)}
	got := restoreImage.build(t, repositoryRoot(t), "amd64", args).BuildArgs
	if len(got) != len(args) {
		t.Fatalf("build arguments %v, want %v", got, args)
	}
	for key, value := range args {
		if got[key] == nil || *got[key] != value {
			t.Errorf("build argument %s = %v, want %q", key, got[key], value)
		}
	}
}

func TestFixtureBuildSelectsTheTargetStage(t *testing.T) {
	root := repositoryRoot(t)
	tools := clientToolsImage.build(t, root, "amd64", map[string]string{"MYSQL_IMAGE": "mysql@sha256:1"})
	if tools.BuildOptionsModifier == nil {
		t.Fatal("the client tools image must build the client-tools stage")
	}
	options := client.ImageBuildOptions{}
	tools.BuildOptionsModifier(&options)
	if options.Target != "client-tools" || tools.Context != root {
		t.Errorf("target %q context %q, want client-tools in %q", options.Target, tools.Context, root)
	}
}

// TestFixtureImagesCoverTheirDockerfiles fails as soon as a Dockerfile starts
// copying a context file the declaration above does not list, or stops
// copying one it lists, so a tag can never miss an input of its image.
func TestFixtureImagesCoverTheirDockerfiles(t *testing.T) {
	root := repositoryRoot(t)
	for _, fixture := range []fixtureDockerfile{ftpImage, ftpsImage, restoreImage, clientToolsImage} {
		if _, err := fixture.read(filepath.Join(root, fixture.contextDir)); err != nil {
			t.Errorf("%s: %v", fixture.name, err)
		}
	}
}

// TestEveryDockerfileBuildUsesAFixtureImage keeps the rule in force for new
// tests: an image built from a Dockerfile without KeepImage and a content tag is
// the race this package has to avoid on a Docker daemon shared by CI jobs.
func TestEveryDockerfileBuildUsesAFixtureImage(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if name == "image_test.go" {
			continue
		}
		parsed, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			if literal, ok := node.(*ast.CompositeLit); ok {
				if selector, ok := literal.Type.(*ast.SelectorExpr); ok && selector.Sel.Name == "FromDockerfile" {
					t.Errorf("%s builds an image with a FromDockerfile literal; use a fixtureDockerfile from image_test.go", fset.Position(literal.Pos()))
				}
			}
			return true
		})
	}
}

// TestFixtureTagsMatchTheCleanupScript pins the contract with
// .github/scripts/web-image-clean.sh, which keeps the newest tags of every
// fixture, architecture and runner by the repository and tag shape declared
// here.
func TestFixtureTagsMatchTheCleanupScript(t *testing.T) {
	root := repositoryRoot(t)
	shape := cleanupTagShape(t, root)
	args := map[string]string{"MYSQL_IMAGE": "mysql@sha256:1", "PXB_IMAGE": "pxb@sha256:2"}
	for _, scope := range []struct{ name, actions, runner, want string }{
		{"a local run", "", "", ""},
		{"a GitHub Actions runner", "true", "JackVance", "379e1e15"},
	} {
		t.Setenv("GITHUB_ACTIONS", scope.actions)
		t.Setenv("RUNNER_NAME", scope.runner)
		for _, build := range []struct{ image, arch, tag string }{
			{"ftp", "amd64", ftpImage.build(t, root, "amd64", nil).Tag},
			{"ftp", "arm64", ftpImage.build(t, root, "arm64", nil).Tag},
			{"ftps", "amd64", ftpsImage.build(t, root, "amd64", nil).Tag},
			{"restore", "amd64", restoreImage.build(t, root, "amd64", args).Tag},
			{"client-tools", "amd64", clientToolsImage.build(t, root, "amd64", args).Tag},
		} {
			// Groups: 1 fixture, 4 architecture, 6 runner.
			match := shape.FindStringSubmatch(build.tag)
			if match == nil {
				t.Errorf("%s: tag %q of the %s image is not a fixture image tag for web-image-clean.sh", scope.name, build.tag, build.image)
				continue
			}
			if match[1] != build.image || match[4] != build.arch || match[6] != scope.want {
				t.Errorf("%s: tag %q groups as fixture %q, architecture %q, runner %q; want %q, %q, %q", scope.name, build.tag, match[1], match[4], match[6], build.image, build.arch, scope.want)
			}
		}
	}
}

// cleanupTagShape reads the fixture image tag shape out of web-image-clean.sh
// and checks that it cleans the repository the harness builds into.
func cleanupTagShape(t *testing.T, root string) *regexp.Regexp {
	t.Helper()
	script, err := os.ReadFile(filepath.Join(root, ".github/scripts/web-image-clean.sh"))
	require(t, err)
	repository := regexp.MustCompile(`(?m)^repository=(\S+)$`).FindSubmatch(script)
	pattern := regexp.MustCompile(`(?m)^tag_pattern='(.+)'$`).FindSubmatch(script)
	if repository == nil || pattern == nil {
		t.Fatal("web-image-clean.sh no longer declares repository= and tag_pattern=")
	}
	if string(repository[1]) != fixtureImageRepo {
		t.Fatalf("web-image-clean.sh cleans %q, the harness builds into %q", repository[1], fixtureImageRepo)
	}
	shape, err := regexp.Compile(string(pattern[1]))
	require(t, err)
	return shape
}

func TestRunnerTagSuffixIsTheRunnerLabelDigest(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("RUNNER_NAME", "ignored")
	if got, err := runnerTagSuffix(); err != nil || got != "" {
		t.Fatalf("outside GitHub Actions the suffix is %q, %v; want none", got, err)
	}
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("RUNNER_NAME", "JackVance")
	// The CLI harness and cli-quality-clean.sh derive the same digest prefix for their runner label.
	if got, err := runnerTagSuffix(); err != nil || got != "-379e1e15" {
		t.Fatalf("the suffix of runner JackVance is %q, %v; want -379e1e15", got, err)
	}
	t.Setenv("RUNNER_NAME", "OtherRunner")
	if got, err := runnerTagSuffix(); err != nil || got == "-379e1e15" {
		t.Fatalf("another runner got the suffix %q, %v; runners must not share a tag", got, err)
	}
	t.Setenv("RUNNER_NAME", " ")
	if got, err := runnerTagSuffix(); err == nil {
		t.Fatalf("a blank RUNNER_NAME gave the suffix %q; the runner is required in GitHub Actions", got)
	}
}

func TestFixtureBuildTagIsScopedToTheRunner(t *testing.T) {
	root := repositoryRoot(t)
	t.Setenv("GITHUB_ACTIONS", "")
	local := ftpImage.build(t, root, "amd64", nil).Tag
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("RUNNER_NAME", "JackVance")
	first := ftpImage.build(t, root, "amd64", nil).Tag
	t.Setenv("RUNNER_NAME", "OtherRunner")
	second := ftpImage.build(t, root, "amd64", nil).Tag
	if first != local+"-379e1e15" || second == first || !strings.HasPrefix(second, local+"-") {
		t.Fatalf("tags: local %q, runner JackVance %q, runner OtherRunner %q; want the local tag plus a different runner suffix each", local, first, second)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../../..")
	require(t, err)
	return root
}
