package restore

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func prepareWork(t *testing.T, count int) string {
	t.Helper()
	root := t.TempDir()
	for index := range count {
		mustNoError(t, os.Mkdir(filepath.Join(root, string(rune('0'+index))), 0o700))
	}
	return root
}

func TestPrepareStepsShapes(t *testing.T) {
	cases := []struct {
		name  string
		links int
		want  [][]string
	}{
		{
			name: "full only", links: 1,
			want: [][]string{{"--no-defaults", "--prepare", "--target-dir={work}/0"}},
		},
		{
			name: "full plus one incremental", links: 2,
			want: [][]string{
				{"--no-defaults", "--prepare", "--apply-log-only", "--target-dir={work}/0"},
				{"--no-defaults", "--prepare", "--target-dir={work}/0", "--incremental-dir={work}/1"},
			},
		},
		{
			name: "full plus two incrementals", links: 3,
			want: [][]string{
				{"--no-defaults", "--prepare", "--apply-log-only", "--target-dir={work}/0"},
				{"--no-defaults", "--prepare", "--apply-log-only", "--target-dir={work}/0", "--incremental-dir={work}/1"},
				{"--no-defaults", "--prepare", "--target-dir={work}/0", "--incremental-dir={work}/2"},
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			assertPrepareShape(t, test.links, test.want)
		})
	}
}

func assertPrepareShape(t *testing.T, links int, want [][]string) {
	t.Helper()
	work := prepareWork(t, links)
	binary := filepath.Join(work, "xtrabackup")
	steps, err := prepareSteps(prepareLinks(links), binary, work, 0)
	mustNoError(t, err)
	if len(steps) != len(want) {
		t.Fatalf("steps = %d, want %d", len(steps), len(want))
	}
	for index, step := range steps {
		assertPrepareArgs(t, work, index, step.args, want[index])
	}
}

func assertPrepareArgs(t *testing.T, work string, index int, got, want []string) {
	t.Helper()
	if got[0] != "--no-defaults" {
		t.Fatalf("step %d argv = %q, want --no-defaults first", index, got)
	}
	expect := make([]string, len(want))
	for position, arg := range want {
		expect[position] = replaceWork(arg, work)
	}
	if !reflect.DeepEqual(got, expect) {
		t.Fatalf("step %d argv = %q, want %q", index, got, expect)
	}
}

func prepareLinks(count int) []PlanLink {
	out := make([]PlanLink, count)
	for index := range count {
		out[index] = PlanLink{BackupID: "backup", Seq: index + 1}
	}
	return out
}

func replaceWork(arg, work string) string {
	return strings.ReplaceAll(arg, "{work}", work)
}

func TestPrepareStepsRequiresExtractedDirectories(t *testing.T) {
	work := prepareWork(t, 1)
	if _, err := prepareSteps([]PlanLink{{Seq: 1}, {Seq: 2}}, filepath.Join(work, "xtrabackup"), work, 0); err == nil {
		t.Fatal("missing incremental directory accepted")
	}
	if _, err := prepareSteps(nil, filepath.Join(work, "xtrabackup"), work, 0); err == nil {
		t.Fatal("empty link list accepted")
	}
}

func TestPrepareStepsUseMemory(t *testing.T) {
	work := prepareWork(t, 1)
	steps, err := prepareSteps([]PlanLink{{Seq: 1}}, filepath.Join(work, "xtrabackup"), work, 256<<20)
	mustNoError(t, err)
	if got := steps[0].args[len(steps[0].args)-1]; got != "--use-memory=268435456" {
		t.Fatalf("use-memory argv = %q", got)
	}
}
