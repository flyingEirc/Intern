package physical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestPrepareArgsFixedShape(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "xtrabackup")
	target := filepath.Join(root, "0")
	incremental := filepath.Join(root, "1")
	cases := []struct {
		name string
		opts PrepareOptions
		want []string
	}{
		{
			name: "full only",
			opts: PrepareOptions{Binary: binary, TargetDir: target},
			want: []string{"--no-defaults", "--prepare", "--target-dir=" + target},
		},
		{
			name: "apply log only",
			opts: PrepareOptions{Binary: binary, TargetDir: target, ApplyLogOnly: true},
			want: []string{"--no-defaults", "--prepare", "--apply-log-only", "--target-dir=" + target},
		},
		{
			name: "intermediate incremental",
			opts: PrepareOptions{Binary: binary, TargetDir: target, ApplyLogOnly: true, IncrementalDir: incremental, UseMemory: 1 << 20},
			want: []string{"--no-defaults", "--prepare", "--apply-log-only", "--target-dir=" + target, "--incremental-dir=" + incremental, "--use-memory=1048576"},
		},
		{
			name: "final incremental",
			opts: PrepareOptions{Binary: binary, TargetDir: target, IncrementalDir: incremental},
			want: []string{"--no-defaults", "--prepare", "--target-dir=" + target, "--incremental-dir=" + incremental},
		},
		{
			name: "use memory",
			opts: PrepareOptions{Binary: binary, TargetDir: target, IncrementalDir: incremental, UseMemory: 256 << 20},
			want: []string{"--no-defaults", "--prepare", "--target-dir=" + target, "--incremental-dir=" + incremental, "--use-memory=268435456"},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			args, err := PrepareArgs(test.opts)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(args, test.want) {
				t.Fatalf("argv = %q, want %q", args, test.want)
			}
			if args[0] != "--no-defaults" {
				t.Fatal("--no-defaults must be the first argument")
			}
		})
	}
}

func TestPrepareArgsRejectsInvalidOptions(t *testing.T) {
	root := t.TempDir()
	cases := []PrepareOptions{
		{Binary: "/usr/bin/true", TargetDir: root},
		{Binary: "xtrabackup", TargetDir: root},
		{Binary: filepath.Join(root, "xtrabackup"), TargetDir: "relative"},
		{Binary: filepath.Join(root, "xtrabackup"), TargetDir: root, IncrementalDir: "relative"},
		{Binary: filepath.Join(root, "xtrabackup"), TargetDir: root, UseMemory: -1},
	}
	for index, opts := range cases {
		if _, err := PrepareArgs(opts); err == nil {
			t.Fatalf("case %d accepted invalid prepare options", index)
		}
	}
}

func TestPrepareRejectsMissingBinary(t *testing.T) {
	err := Prepare(t.Context(), PrepareOptions{Binary: filepath.Join(t.TempDir(), "xtrabackup"), TargetDir: t.TempDir(), Timeout: time.Second})
	if err == nil {
		t.Fatal("prepare accepted a missing binary")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatal("tool path error leaked into the public error")
	}
}
