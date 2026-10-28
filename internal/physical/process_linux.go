package physical

import (
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const supervisorArg = "--intern-physical-tool-supervisor"

const supervisorHandshake = "intern-physical-lifetime-v1\n"

const supervisorConfigLimit = 64 * 1024

const supervisorWaitDelay = 5 * time.Second

const descendantReapTimeout = 2 * time.Second

const (
	supervisorLifetimeFD   = 3
	supervisorCredentialFD = 4
)

type supervisorConfig struct {
	Parent                     int
	Cleanup, Directory, Binary string
	Args                       []string
}

// init is a private self-reexec entrypoint, never enabled by environment.
// Lifetime-pipe EOF starts cleanup when the parent exits. If container PID 1
// dies, the kernel kills the namespace instead; its credential-free work root
// remains the caller's persistent cleanup responsibility.
func init() {
	if len(os.Args) != 2 || os.Args[1] != supervisorArg {
		return
	}
	os.Exit(superviseTool())
}

func executeProcess(ctx context.Context, spec processSpec) error {
	payload, err := supervisorPayload(spec)
	if err != nil {
		return err
	}
	if credentialArg(spec.args) != (len(spec.credential) > 0) {
		return errors.New("physical: invalid supervisor configuration")
	}
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return err
	}
	defer func() { closePipe(readEnd) }()
	defer func() { closePipe(writeEnd) }()
	extra, err := supervisorFiles(readEnd, spec.credential)
	if err != nil {
		return err
	}
	return runSupervisorProcess(ctx, spec, extra, writeEnd, payload)
}

func supervisorFiles(lifetime *os.File, credential []byte) ([]*os.File, error) {
	if len(credential) == 0 {
		return []*os.File{lifetime}, nil
	}
	memfd, err := newCredentialMemfd(credential)
	if err != nil {
		return nil, errors.New("physical: cannot create private credential file")
	}
	return []*os.File{lifetime, memfd}, nil
}

func runSupervisorProcess(ctx context.Context, spec processSpec, extra []*os.File, writeEnd *os.File, payload []byte) error {
	if len(extra) > 1 {
		defer func() { closePipe(extra[1]) }()
	}
	// /proc/self/exe pins this process image. The supervisor is its own process
	// group so a terminal signal aimed at the parent does not skip tool teardown.
	// Only the activation token is public argv.
	// #nosec G204 -- fixed self-exe path and one fixed activation argument.
	cmd := exec.CommandContext(ctx, "/proc/self/exe", supervisorArg)
	cmd.ExtraFiles = extra
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.stdin, spec.stdout, spec.stderr
	cmd.Cancel = writeEnd.Close
	cmd.WaitDelay = supervisorWaitDelay
	written := make(chan error, 1)
	go func() { written <- writeSupervisorConfig(writeEnd, payload) }()
	runErr := cmd.Run()
	closePipe(extra[0])
	return errors.Join(runErr, <-written)
}

func supervisorPayload(spec processSpec) ([]byte, error) {
	if !utf8SupervisorConfig(spec) {
		return nil, errSupervisorEncoding
	}
	payload, err := json.Marshal(supervisorConfig{
		Parent: os.Getpid(), Cleanup: spec.cleanup, Directory: spec.dir, Binary: spec.binary, Args: spec.args,
	})
	if err != nil || len(payload) > supervisorConfigLimit {
		return nil, errors.New("physical: invalid supervisor configuration")
	}
	return payload, nil
}

func utf8SupervisorConfig(spec processSpec) bool {
	if !utf8.ValidString(spec.binary) || !utf8.ValidString(spec.dir) || !utf8.ValidString(spec.cleanup) {
		return false
	}
	for _, field := range spec.args {
		if !utf8.ValidString(field) {
			return false
		}
	}
	return true
}

func newCredentialMemfd(body []byte) (*os.File, error) {
	fd, err := unix.MemfdCreate("physical-cnf", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "physical-cnf")
	if file == nil {
		return nil, closeRawFD(fd)
	}
	if err = file.Chmod(0o600); err != nil {
		closePipe(file)
		return nil, err
	}
	if err = writeFull(file, body); err != nil {
		closePipe(file)
		return nil, err
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		closePipe(file)
		return nil, err
	}
	return file, nil
}

func closeRawFD(fd int) error {
	if err := unix.Close(fd); err != nil {
		return err
	}
	return errors.New("physical: cannot create private credential file")
}

func writeFull(file *os.File, body []byte) error {
	for len(body) > 0 {
		n, err := file.Write(body)
		body = body[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func closePipe(file *os.File) {
	if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return
	}
}

func superviseTool() int {
	// ExtraFiles clears O_NONBLOCK before exec. Restore it before NewFile so
	// signal-driven Close wakes the runtime-pollable lifetime reader.
	if err := unix.SetNonblock(supervisorLifetimeFD, true); err != nil {
		return 125
	}
	lifetime := os.NewFile(supervisorLifetimeFD, "physical-parent-lifetime")
	if lifetime == nil {
		return 125
	}
	var closeLifetime sync.Once
	release := func() { closeLifetime.Do(func() { closePipe(lifetime) }) }
	defer release()
	watchSupervisorSignals(release)
	config, err := readSupervisorConfig(lifetime)
	if err != nil || !supportedParent(config.Parent) || config.Binary == "" || !validCleanupDirectory(config.Cleanup) {
		return 125
	}
	return finishSupervision(lifetime, config)
}

func supportedParent(pid int) bool {
	// PID 1 is a supported container entrypoint. Liveness is the lifetime pipe.
	return pid >= 1
}

func finishSupervision(lifetime *os.File, config supervisorConfig) int {
	result := superviseChild(lifetime, config)
	if config.Cleanup == "" || result != 125 {
		return result
	}
	if err := os.RemoveAll(config.Cleanup); err != nil {
		return 125
	}
	return result
}

func watchSupervisorSignals(interrupt func()) {
	signals := make(chan os.Signal, 3)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-signals
		interrupt()
	}()
}

func validCleanupDirectory(path string) bool {
	if path == "" {
		return true
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.IndexByte(path, 0) >= 0 {
		return false
	}
	if !strings.HasPrefix(filepath.Base(path), "physical-run-") {
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
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0, 0); errno != 0 {
		return 125
	}
	if parentIdentityLost(config.Parent) {
		return 125
	}
	if !prepareToolDescriptors(config.Args) {
		return 125
	}
	dead := supervisorParentClosed(lifetime)
	if parentGone(dead) {
		return 125
	}
	// #nosec G204 -- inherited private descriptor carries the configured tool and structured argv; no shell or CLI-derived command.
	cmd := exec.Command(config.Binary, config.Args...)
	cmd.Dir = config.Directory
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = isolatedToolEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		return 125
	}
	result := waitTool(cmd, dead)
	if err := finishToolGroup(cmd.Process.Pid); err != nil {
		return 125
	}
	return supervisorResult(result, config.Parent, dead)
}

func parentIdentityLost(parent int) bool {
	if parent == 1 {
		// Reparenting to init is not observable when the parent is PID 1.
		// Lifetime-pipe EOF is the liveness signal.
		return false
	}
	return os.Getppid() != parent
}

func parentGone(dead <-chan struct{}) bool {
	select {
	case <-dead:
		return true
	default:
		return false
	}
}

func prepareToolDescriptors(args []string) bool {
	unix.CloseOnExec(supervisorLifetimeFD)
	if !credentialArg(args) {
		return true
	}
	return inheritCredentialFD()
}

func credentialArg(args []string) bool {
	for _, arg := range args {
		if arg == defaultsFileArgument {
			return true
		}
	}
	return false
}

func inheritCredentialFD() bool {
	flags, err := unix.FcntlInt(uintptr(supervisorCredentialFD), unix.F_GETFD, 0)
	if err != nil {
		return false
	}
	if _, err = unix.FcntlInt(uintptr(supervisorCredentialFD), unix.F_SETFD, flags&^unix.FD_CLOEXEC); err != nil {
		return false
	}
	var stat unix.Stat_t
	if err = unix.Fstat(supervisorCredentialFD, &stat); err != nil {
		return false
	}
	return stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Mode&0o777 == 0o600
}

func isolatedToolEnv() []string {
	values := os.Environ()
	kept := make([]string, 0, len(values))
	for _, value := range values {
		if mysqlOverrideEnv(value) {
			continue
		}
		kept = append(kept, value)
	}
	return kept
}

func mysqlOverrideEnv(value string) bool {
	return strings.HasPrefix(value, "MYSQL_PWD=") ||
		strings.HasPrefix(value, "MYSQL_TEST_LOGIN_FILE=") ||
		strings.HasPrefix(value, "MYSQL_HOME=")
}

func waitTool(cmd *exec.Cmd, dead <-chan struct{}) int {
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case err := <-wait:
		if err != nil {
			return 1
		}
		return 0
	case <-dead:
		if err := killToolGroup(cmd.Process.Pid); err != nil {
			return 125
		}
		<-wait
		return 125
	}
}

func finishToolGroup(pid int) error {
	if err := killToolGroup(pid); err != nil {
		return err
	}
	return reapDescendants()
}

func supervisorResult(result, parent int, dead <-chan struct{}) int {
	if parentIdentityLost(parent) {
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
	deadline := time.Now().Add(descendantReapTimeout)
	for {
		done, err := reapOnce()
		if done || err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("physical: tool descendants survived group teardown")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func reapOnce() (bool, error) {
	pid, err := syscall.Wait4(-1, nil, syscall.WNOHANG, nil)
	if errors.Is(err, syscall.ECHILD) {
		return true, nil
	}
	if errors.Is(err, syscall.EINTR) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if pid > 0 {
		return false, nil
	}
	children, err := directChildren()
	if err != nil || len(children) == 0 {
		return false, err
	}
	return false, signalChildren(children)
}

func directChildren() ([]int, error) {
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return nil, err
	}
	seen := make(map[int]struct{}, len(tasks))
	pids := make([]int, 0, 4)
	for _, task := range tasks {
		raw, readErr := os.ReadFile(filepath.Join("/proc/self/task", task.Name(), "children"))
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			return nil, readErr
		}
		pids = appendChildPIDs(pids, seen, raw)
	}
	return pids, nil
}

func appendChildPIDs(pids []int, seen map[int]struct{}, raw []byte) []int {
	for _, field := range strings.Fields(string(raw)) {
		pid, err := strconv.Atoi(field)
		if err != nil || pid <= 1 {
			continue
		}
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		pids = append(pids, pid)
	}
	return pids
}

func signalChildren(pids []int) error {
	var result error
	for _, pid := range pids {
		err := syscall.Kill(pid, syscall.SIGKILL)
		if err != nil && !errors.Is(err, syscall.ESRCH) && result == nil {
			result = err
		}
	}
	return result
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
