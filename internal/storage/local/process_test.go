package local

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/flyingEirc/Intern/internal/pipeline"
)

const helperEnvironment = "INTERN_P1_HELPER"

func helperPayload() []byte {
	return bytes.Repeat([]byte("deterministic external process pipeline payload\x00\xff\n"), 4096)
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnvironment) != "1" {
		return
	}
	payload := helperPayload()
	mode := os.Getenv("INTERN_P1_HELPER_MODE")
	if mode != "success" {
		payload = payload[:len(payload)/2]
	}
	if _, err := os.Stdout.Write(payload); err != nil {
		os.Exit(3)
	}
	switch mode {
	case "failure":
		os.Exit(17)
	case "cancel", "watchdog":
		if _, err := io.WriteString(os.Stderr, "ready to block\n"); err != nil {
			os.Exit(4)
		}
		var input [1]byte
		if _, err := os.Stdin.Read(input[:]); err != nil {
			os.Exit(5)
		}
		os.Exit(6)
	default:
		os.Exit(0)
	}
}

type processSource struct {
	mode    string
	cancel  context.CancelFunc
	command *exec.Cmd
	ready   bool
}

func (s *processSource) Run(ctx context.Context, w io.Writer) (result error) {
	runCtx := ctx
	if s.mode == "watchdog" {
		var stop context.CancelCauseFunc
		runCtx, stop = context.WithCancelCause(ctx)
		s.cancel = func() { stop(errors.New("watchdog")) }
		defer stop(errors.New("watchdog"))
	}
	command := exec.CommandContext(runCtx, os.Args[0], "-test.run=^TestHelperProcess$")
	command.Env = append(os.Environ(), helperEnvironment+"=1", "INTERN_P1_HELPER_MODE="+s.mode)
	command.Stdout = w
	s.command = command
	if s.mode != "cancel" && s.mode != "watchdog" {
		return command.Run()
	}
	input, hold, err := os.Pipe()
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, input.Close(), hold.Close()) }()
	command.Stdin = input
	command.Stderr = cancelWhenReady{source: s}
	if s.mode == "watchdog" {
		return errors.Join(command.Run(), runCtx.Err())
	}
	return errors.Join(command.Run(), ctx.Err())
}

type cancelWhenReady struct{ source *processSource }

func (w cancelWhenReady) Write(p []byte) (int, error) {
	w.source.ready = true
	w.source.cancel()
	return len(p), nil
}

func TestExternalProcessPipeline(t *testing.T) {
	for _, mode := range []string{"success", "failure", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			repo, root := newRepository(t)
			staging := begin(t, repo, "inst/src/chain/0001-backup")
			sink := create(t, staging, "payload.zst")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			source := &processSource{mode: mode, cancel: cancel}
			var digest pipeline.SHA256Tap
			stages := []pipeline.Stage{zstdStage, digest.Stage}
			receipt, err := pipeline.Run(ctx, source, stages, sink)
			assertProcessReaped(t, source)
			if mode == "success" {
				assertProcessSuccess(t, repo, staging, receipt, digest.Sum(), err)
			} else {
				assertProcessFailure(t, repo, staging, source, receipt, err)
			}
			assertNoPending(t, root)
		})
	}
}

func TestExternalProcessWatchdogDoesNotPublish(t *testing.T) {
	repo, root := newRepository(t)
	staging := begin(t, repo, "inst/src/chain/0001-backup")
	sink := create(t, staging, "payload.zst")
	parent := context.Background()
	source := &processSource{mode: "watchdog"}
	receipt, err := pipeline.Run(parent, source, []pipeline.Stage{zstdStage}, sink)
	assertProcessReaped(t, source)
	if err == nil || !errors.Is(err, context.Canceled) || receipt != (pipeline.Receipt{}) {
		t.Fatalf("watchdog result = %+v, %v", receipt, err)
	}
	if parent.Err() != nil {
		t.Fatalf("parent context = %v", parent.Err())
	}
	assertInvisible(t, repo, staging.dir+"/payload.zst")
	if err = staging.Abort(); err != nil {
		t.Fatal(err)
	}
	assertNoPending(t, root)
}

func zstdStage(next io.Writer) (io.WriteCloser, error) {
	return zstd.NewWriter(next, zstd.WithEncoderConcurrency(1))
}

func assertProcessReaped(t *testing.T, source *processSource) {
	t.Helper()
	if source.command == nil || source.command.ProcessState == nil {
		t.Fatal("external process was not waited for")
	}
	if err := source.command.Process.Signal(os.Interrupt); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("external process was not reaped: %v", err)
	}
}

func assertProcessSuccess(t *testing.T, repo *Repository, staging *Staging, receipt pipeline.Receipt, sum [32]byte, runErr error) {
	t.Helper()
	if runErr != nil {
		t.Fatal(runErr)
	}
	if err := staging.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	file, err := repo.Open(receipt.Path)
	if err != nil {
		t.Fatal(err)
	}
	compressed, readErr := io.ReadAll(file)
	if err = errors.Join(readErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	if receipt.Size != int64(len(compressed)) || sum != sha256.Sum256(compressed) {
		t.Fatalf("receipt or digest mismatch: %+v, digest %x", receipt, sum)
	}
	assertDecompressedPayload(t, compressed)
}

func assertDecompressedPayload(t *testing.T, compressed []byte) {
	t.Helper()
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	decoded, err := decoder.DecodeAll(compressed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, helperPayload()) {
		t.Fatal("decompressed process bytes differ")
	}
}

func assertProcessFailure(t *testing.T, repo *Repository, staging *Staging, source *processSource, receipt pipeline.Receipt, runErr error) {
	t.Helper()
	if runErr == nil || receipt != (pipeline.Receipt{}) {
		t.Fatalf("failed process result = %+v, %v", receipt, runErr)
	}
	if source.mode == "cancel" {
		if !source.ready || !errors.Is(runErr, context.Canceled) {
			t.Fatalf("process was not canceled at its blocking point: ready=%v, error=%v", source.ready, runErr)
		}
	} else {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 17 {
			t.Fatalf("nonzero exit cause lost: %v", runErr)
		}
	}
	if err := staging.Abort(); err != nil {
		t.Fatal(err)
	}
	assertInvisible(t, repo, staging.dir+"/payload.zst")
}
