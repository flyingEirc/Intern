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
	"runtime"
	"strings"
	"syscall"
	"time"
)

const supervisorArg = "--intern-physical-tool-supervisor"

const supervisorHandshake = "intern-physical-lifetime-v1\n"

const supervisorConfigLimit = 64 * 1024

type supervisorConfig struct {
	Parent                     int
	Cleanup, Directory, Binary string
	Args                       []string
}

// init is a private self-reexec entrypoint, never enabled by environment. The
// inherited CLOEXEC lifetime pipe is closed by the kernel on parent death. This
// avoids Linux Pdeathsig's creating-thread semantics and the signal-handler
// installation race. The tool does not inherit the lifetime pipe's write end.
func init() {
	if len(os.Args) != 2 || os.Args[1] != supervisorArg {
		return
	}
	os.Exit(superviseTool())
}

func executeProcess(ctx context.Context, spec processSpec) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(supervisorConfig{
		Parent: os.Getpid(), Cleanup: spec.cleanup, Directory: spec.dir, Binary: spec.binary, Args: spec.args,
	})
	if err != nil || len(payload) > supervisorConfigLimit {
		return errors.New("physical: invalid supervisor configuration")
	}
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return err
	}
	defer func() { closePipe(readEnd) }()
	defer func() { closePipe(writeEnd) }()
	// Only the activation token is public argv. Tool selection and cleanup
	// ownership arrive over the inherited lifetime descriptor, not CLI input.
	// #nosec G204 -- os.Executable selects this running image, with one fixed activation argument.
	cmd := exec.CommandContext(ctx, executable, supervisorArg)
	cmd.ExtraFiles = []*os.File{readEnd}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.stdin, spec.stdout, spec.stderr
	cmd.Cancel = writeEnd.Close
	cmd.WaitDelay = 5 * time.Second
	written := make(chan error, 1)
	go func() { written <- writeSupervisorConfig(writeEnd, payload) }()
	runErr := cmd.Run()
	closePipe(readEnd)
	return errors.Join(runErr, <-written)
}

func closePipe(f *os.File) {
	if err := f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return // Pipe close has no durable data to flush.
	}
}

func superviseTool() int {
	lifetime := os.NewFile(3, "physical-parent-lifetime")
	if lifetime == nil {
		return 125
	}
	defer closePipe(lifetime)
	config, err := readSupervisorConfig(lifetime)
	if err != nil || config.Parent < 2 || config.Binary == "" || !validCleanupDirectory(config.Cleanup) {
		return 125
	}
	cleanup := config.Cleanup
	result := superviseChild(lifetime, config)
	if cleanup != "" && result == 125 {
		if err := os.RemoveAll(cleanup); err != nil {
			return 125
		}
	}
	return result
}

func validCleanupDirectory(path string) bool {
	if path == "" {
		return true
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !strings.HasPrefix(filepath.Base(path), "physical-run-") {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func superviseChild(lifetime *os.File, config supervisorConfig) int {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// Adopt descendants so group teardown also reaps orphaned grandchildren.
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, 36, 1, 0, 0, 0, 0); errno != 0 {
		return 125
	}
	if os.Getppid() != config.Parent {
		return 125
	}
	dead := supervisorParentClosed(lifetime)
	// #nosec G204 -- inherited private descriptor carries the configured tool and structured argv; no shell or CLI-derived command.
	cmd := exec.Command(config.Binary, config.Args...)
	cmd.Dir = config.Directory
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		return 125
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	var result int
	select {
	case err := <-wait:
		if err != nil {
			result = 1
		}
	case <-dead:
		result = 125
		if err := killToolGroup(cmd.Process.Pid); err != nil {
			return 125
		}
		<-wait
	}
	// Even a successful leader must not leave descendants holding pipes open.
	if err := killToolGroup(cmd.Process.Pid); err != nil {
		return 125
	}
	if err := reapDescendants(); err != nil {
		return 125
	}
	return supervisorResult(result, config.Parent, dead)
}

func supervisorResult(result, parent int, dead <-chan struct{}) int {
	if os.Getppid() != parent {
		return 125
	}
	select {
	case <-dead:
		return 125
	default:
		return result
	}
}

func killToolGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func reapDescendants() error {
	for {
		_, err := syscall.Wait4(-1, nil, 0, nil)
		if errors.Is(err, syscall.ECHILD) {
			return nil
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
	}
}

func writeSupervisorConfig(w io.Writer, payload []byte) error {
	if _, err := io.WriteString(w, supervisorHandshake); err != nil {
		return err
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(payload)))
	if _, err := w.Write(size[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readSupervisorConfig(lifetime *os.File) (supervisorConfig, error) {
	var config supervisorConfig
	stat, err := lifetime.Stat()
	if err != nil || stat.Mode()&os.ModeNamedPipe == 0 {
		return config, errors.New("invalid lifetime descriptor")
	}
	handshake := make([]byte, len(supervisorHandshake))
	if _, err = io.ReadFull(lifetime, handshake); err != nil || string(handshake) != supervisorHandshake {
		return config, errors.New("invalid lifetime handshake")
	}
	var size [4]byte
	if _, err = io.ReadFull(lifetime, size[:]); err != nil {
		return config, err
	}
	length := binary.BigEndian.Uint32(size[:])
	if length == 0 || length > supervisorConfigLimit {
		return config, errors.New("invalid supervisor configuration size")
	}
	payload := make([]byte, length)
	if _, err = io.ReadFull(lifetime, payload); err != nil {
		return config, err
	}
	err = json.Unmarshal(payload, &config)
	return config, err
}

func supervisorParentClosed(lifetime *os.File) <-chan struct{} {
	dead := make(chan struct{})
	go func() {
		defer close(dead)
		if _, err := io.Copy(io.Discard, lifetime); err != nil {
			return
		}
	}()
	return dead
}
