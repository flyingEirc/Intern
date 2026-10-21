package physical

import (
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPhysicalParentHelper(t *testing.T) {
	if os.Getenv("INTERN_PHYSICAL_PARENT_HELPER") != "1" {
		return
	}
	root := os.Getenv("INTERN_PHYSICAL_HELPER_ROOT")
	dir, err := newWorkDir(root)
	if err != nil {
		t.Fatal(err)
	}
	tool := os.Getenv("INTERN_PHYSICAL_HELPER_TOOL")
	body := []byte("[client]\nuser=\"backup\"\npassword=\"private-test-value\"\n")
	err = runProcess(context.Background(), processSpec{
		binary: tool, args: []string{defaultsFileArgument, "--no-login-paths", root}, credential: body, cleanup: dir, stdout: io.Discard, stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func waitForCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func assertPIDsGone(t *testing.T, pidFile string) {
	t.Helper()
	waitForCondition(t, "tool and grandchild reaped", func() bool {
		pids := readToolPIDs(pidFile)
		if len(pids) != 2 {
			return false
		}
		for _, pid := range pids {
			if !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				return false
			}
		}
		return true
	})
}

func TestParentDeathCleansProcessGroupAndCredentials(t *testing.T) {
	root := t.TempDir()
	tool := fakeTool(t, `[ "$1" = "--defaults-file=/proc/self/fd/4" ]
[ "$2" = "--no-login-paths" ]
[ -f /proc/self/fd/4 ]
[ "$(stat -L -c %a /proc/self/fd/4)" = 600 ]
for d in "$3"/physical-run-*; do
 [ -e "$d/client.cnf" ] && exit 73
done
sleep 30 &
child=$!
printf '%s %s' "$$" "$child" > "$3/pids"
wait
`)
	cmd := exec.Command("/proc/self/exe", "-test.run=^TestPhysicalParentHelper$")
	cmd.Env = append(os.Environ(), "INTERN_PHYSICAL_PARENT_HELPER=1", "INTERN_PHYSICAL_HELPER_ROOT="+root, "INTERN_PHYSICAL_HELPER_TOOL="+tool)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	var pids []int
	waitForCondition(t, "tool and grandchild running", func() bool {
		pids = readToolPIDs(filepath.Join(root, "pids"))
		return len(pids) == 2
	})
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed parent succeeded")
	}
	waitForCondition(t, "process group reaped", func() bool {
		for _, pid := range pids {
			if !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				return false
			}
		}
		return true
	})
	waitForCondition(t, "private work directory removed", func() bool {
		entries, err := os.ReadDir(root)
		return err == nil && len(entries) == 1 && entries[0].Name() == "pids"
	})
}

func TestSupervisorRejectsHostileCleanupOverPipe(t *testing.T) {
	ordinary := t.TempDir()
	marker := filepath.Join(ordinary, "keep")
	os.WriteFile(marker, []byte("keep"), 0o600)
	sibling, err := newWorkDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sibling)
	tool := fakeTool(t, "printf started > /dev/null\n")
	for name, cleanup := range map[string]string{
		"ordinary":  ordinary,
		"traversal": ordinary + "/physical-run-x/../..",
		"relative":  "physical-run-x",
		"sibling":   sibling + "/../" + filepath.Base(sibling),
	} {
		t.Run(name, func(t *testing.T) {
			exit := runSupervisorIPC(t, supervisorConfig{Parent: os.Getpid(), Cleanup: cleanup, Binary: tool, Args: []string{"ok"}})
			if exit == 0 {
				t.Fatal("hostile cleanup accepted")
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatal("ordinary directory deleted")
			}
			if _, err := os.Stat(sibling); err != nil {
				t.Fatal("unrelated physical-run directory swept")
			}
		})
	}
}

func TestValidCleanupDirectoryRejectsHostilePaths(t *testing.T) {
	root := t.TempDir()
	dir, err := newWorkDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if !validCleanupDirectory(dir) {
		t.Fatal("private work directory rejected")
	}
	if !validCleanupDirectory("") {
		t.Fatal("empty cleanup must be allowed")
	}
	link := filepath.Join(root, "physical-run-link")
	os.Symlink(dir, link)
	os.Chmod(dir, 0o755)
	filePath := filepath.Join(root, "physical-run-file")
	os.WriteFile(filePath, []byte("x"), 0o600)
	for _, path := range []string{
		root,
		link,
		dir,
		filePath,
		filepath.Join(dir, "..", filepath.Base(dir)),
		"physical-run-relative",
		"/tmp/physical-run-x/../secret",
	} {
		if validCleanupDirectory(path) {
			t.Fatalf("accepted %q", path)
		}
	}
}

func TestReadSupervisorConfigRejectsHostileInput(t *testing.T) {
	valid, err := json.Marshal(supervisorConfig{Parent: 8, Binary: "/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, handshake string
		length          uint32
		payload         []byte
	}{
		{"wrong-handshake", "wrong-handshake\n", uint32(len(valid)), valid},
		{"zero-length", supervisorHandshake, 0, valid},
		{"oversized", supervisorHandshake, supervisorConfigLimit + 1, nil},
		{"truncated", supervisorHandshake, uint32(len(valid) + 1), valid},
		{"invalid-json", supervisorHandshake, 1, []byte("{")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, decodeErr := decodeSupervisorPayload(t, test.handshake, test.length, test.payload); decodeErr == nil {
				t.Fatal("hostile payload accepted")
			}
		})
	}
	got, err := decodeSupervisorPayload(t, supervisorHandshake, uint32(len(valid)), valid)
	if err != nil || got.Parent != 8 || got.Binary != "/bin/true" {
		t.Fatalf("valid config rejected: %+v %v", got, err)
	}
}

func TestSupervisorRejectsNonPipeDescriptor(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "not-pipe")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err = readSupervisorConfig(file); err == nil {
		t.Fatal("non-pipe descriptor accepted")
	}
}

func decodeSupervisorPayload(t *testing.T, handshake string, length uint32, payload []byte) (supervisorConfig, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer closePipe(r)
	go func() {
		_, _ = io.WriteString(w, handshake)
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], length)
		_, _ = w.Write(size[:])
		_, _ = w.Write(payload)
		closePipe(w)
	}()
	return readSupervisorConfig(r)
}

func runSupervisorIPC(t *testing.T, config supervisorConfig) int {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/proc/self/exe", supervisorArg)
	cmd.ExtraFiles = []*os.File{r}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	closePipe(r)
	payload, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeSupervisorConfig(w, payload); err != nil {
		closePipe(w)
		_ = cmd.Process.Kill()
		t.Fatal(err)
	}
	closePipe(w)
	err = cmd.Wait()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	t.Fatal(err)
	return -1
}

func TestCancellationCannotBecomeSuccess(t *testing.T) {
	source := testSource(t, fakeNamedTool(t, "xtrabackup", backupFixtureBody()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := source.Run(ctx, cancelOnWrite{cancel: cancel}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled success-looking backup returned %v", err)
	}
	assertWorkEmpty(t, source.WorkRoot)
}

func TestSuccessfulLeaderCannotLeakGrandchild(t *testing.T) {
	root := t.TempDir()
	tool := fakeTool(t, "sleep 30 &\nprintf '%s' \"$!\" > \"$1/pid\"\nexit 0\n")
	if err := runProcess(context.Background(), processSpec{binary: tool, args: []string{root}, stdout: io.Discard, stderr: io.Discard}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil || !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		t.Fatalf("grandchild not reaped: pid=%d parse=%v", pid, err)
	}
}

func TestCrashDoesNotSweepSiblingWorkRoot(t *testing.T) {
	sibling, err := newWorkDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sibling)
	marker := filepath.Join(sibling, "keep")
	os.WriteFile(marker, []byte("keep"), 0o600)
	exit := runSupervisorIPC(t, supervisorConfig{Parent: 0, Cleanup: t.TempDir(), Binary: "/bin/true"})
	if exit == 0 {
		t.Fatal("invalid parent or unrelated temp dir accepted as cleanup")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("prefix sweep deleted a live sibling work directory")
	}
}

func TestIsolatedEnvStripsMySQLOverrides(t *testing.T) {
	t.Setenv("MYSQL_PWD", "secret")
	t.Setenv("MYSQL_TEST_LOGIN_FILE", "/tmp/login")
	t.Setenv("MYSQL_HOME", "/tmp/home")
	kept := isolatedToolEnv()
	for _, value := range kept {
		if mysqlOverrideEnv(value) {
			t.Fatalf("override leaked: %s", value)
		}
	}
}

func TestUTF8LimitDoesNotStartTool(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	err := runProcess(context.Background(), processSpec{
		binary: fakeTool(t, "printf x > "+shellQuote(marker)+"\n"), args: []string{"\xff"}, stdout: io.Discard, stderr: io.Discard,
	})
	if !errors.Is(err, errSupervisorEncoding) {
		t.Fatalf("got %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("tool started")
	}
}

func readToolPIDs(path string) []int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 {
		return nil
	}
	pids := make([]int, 0, 2)
	for _, field := range fields {
		pid, err := strconv.Atoi(field)
		if err != nil || pid < 2 {
			return nil
		}
		pids = append(pids, pid)
	}
	return pids
}
