package physical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMysqldArgsFixedShape(t *testing.T) {
	root := t.TempDir()
	args, err := MysqldArgs(MysqldOptions{
		Binary: filepath.Join(root, "mysqld"), DataDir: filepath.Join(root, "data"),
		Socket: filepath.Join(root, "mysqld.sock"), PIDFile: filepath.Join(root, "mysqld.pid"), LogError: filepath.Join(root, "mysqld.err"),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--no-defaults",
		"--datadir=" + filepath.Join(root, "data"),
		"--socket=" + filepath.Join(root, "mysqld.sock"),
		"--pid-file=" + filepath.Join(root, "mysqld.pid"),
		"--log-error=" + filepath.Join(root, "mysqld.err"),
		"--skip-networking",
		"--skip-grant-tables",
		"--skip-replica-start",
		"--skip-log-bin",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("argv = %q, want %q", args, want)
	}
}

func TestMysqldArgsRejectsRelativePaths(t *testing.T) {
	root := t.TempDir()
	base := MysqldOptions{
		Binary: filepath.Join(root, "mysqld"), DataDir: filepath.Join(root, "data"),
		Socket: filepath.Join(root, "s"), PIDFile: filepath.Join(root, "p"), LogError: filepath.Join(root, "e"),
	}
	mutations := []func(*MysqldOptions){
		func(o *MysqldOptions) { o.Binary = "" },
		func(o *MysqldOptions) { o.DataDir = "data" },
		func(o *MysqldOptions) { o.Socket = "s" },
		func(o *MysqldOptions) { o.PIDFile = "p" },
		func(o *MysqldOptions) { o.LogError = "e" },
		func(o *MysqldOptions) { o.DataDir = "data\x00" },
	}
	for index, mutate := range mutations {
		opts := base
		mutate(&opts)
		if _, err := MysqldArgs(opts); err == nil {
			t.Fatalf("case %d accepted invalid mysqld options", index)
		}
	}
}

// TestStartMysqldRefusesMissingDatadir requires the datadir check to be the one
// that refuses: the configured binary is a real executable, so the earlier binary
// validation passes and a missing datadir is what stops the launch.
func TestStartMysqldRefusesMissingDatadir(t *testing.T) {
	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, err = StartMysqld(t.Context(), MysqldOptions{
		Binary: executable, DataDir: filepath.Join(root, "missing"),
		Socket: filepath.Join(root, "s"), PIDFile: filepath.Join(root, "p"), LogError: filepath.Join(root, "e"),
	})
	if err == nil {
		t.Fatal("missing datadir accepted")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatal("filesystem error leaked into the public error")
	}
}

func TestQuerySpace(t *testing.T) {
	space, err := QuerySpace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if space.AvailableBytes < 0 {
		t.Fatal("negative available bytes")
	}
	if _, err = QuerySpace(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing path reported capacity")
	}
	if _, err = AvailableBytes(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

// TestStartMysqldRefusesUnusableBinary requires the configured server to be
// refused before anything is launched. The supervisor always starts /proc/self/exe,
// so on Linux a missing or non-executable binary would otherwise look like a
// successful start and only surface later as an opaque exit; the refusal must be
// the same on every platform.
func TestStartMysqldRefusesUnusableBinary(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	notExecutable := filepath.Join(root, "mysqld-noexec")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		binary string
	}{
		{"relative", "mysqld"},
		{"missing", filepath.Join(root, "missing")},
		{"directory", root},
		{"not executable", notExecutable},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := StartMysqld(t.Context(), MysqldOptions{
				Binary: test.binary, DataDir: data,
				Socket: filepath.Join(root, "s"), PIDFile: filepath.Join(root, "p"), LogError: filepath.Join(root, "e"),
			})
			if err == nil {
				t.Fatalf("StartMysqld accepted the %s binary", test.name)
			}
			if errors.Is(err, os.ErrNotExist) {
				t.Fatal("filesystem error leaked into the public error")
			}
		})
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err = validateMysqldBinary(executable); err != nil {
		t.Fatalf("a real executable was refused: %v", err)
	}
}
