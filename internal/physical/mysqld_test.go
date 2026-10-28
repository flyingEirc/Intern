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

func TestStartMysqldRefusesMissingDatadir(t *testing.T) {
	root := t.TempDir()
	_, err := StartMysqld(t.Context(), MysqldOptions{
		Binary: filepath.Join(root, "mysqld"), DataDir: filepath.Join(root, "missing"),
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
