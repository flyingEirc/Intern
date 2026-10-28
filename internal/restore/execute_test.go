package restore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/flyingEirc/Intern/internal/physical"
)

func requireExecCode(t *testing.T, err error, code string) *ExecError {
	t.Helper()
	var execErr *ExecError
	if !errors.As(err, &execErr) {
		t.Fatalf("error = %v, want an ExecError with code %s", err, code)
	}
	if execErr.Code != code {
		t.Fatalf("exec code = %q, want %q", execErr.Code, code)
	}
	return execErr
}

func restoreRequest(plan Plan) Request {
	return Request{RestoreID: strings.Repeat("a", 32), Plan: plan}
}

func TestExecuteRejectsInvalidRequests(t *testing.T) {
	fixture := newPlanFixture(t)
	plan, err := BuildPlan(t.Context(), fixture.options(), fixture.backup(2).ID)
	mustNoError(t, err)
	cases := []struct {
		name   string
		mutate func(*Request)
		code   string
	}{
		{name: "short restore id", code: CodeInvalidRequest, mutate: func(r *Request) { r.RestoreID = "abc" }},
		{name: "uppercase restore id", code: CodeInvalidRequest, mutate: func(r *Request) { r.RestoreID = strings.Repeat("A", 32) }},
		{name: "rehearsal without mysqld", code: CodeRehearsalUnavailable, mutate: func(r *Request) { r.Rehearse = true }},
		{name: "invalid verify sql", code: CodeInvalidVerifySQL, mutate: func(r *Request) { r.VerifySQL = []string{"DELETE FROM t"} }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := restoreRequest(plan)
			test.mutate(&request)
			_, err := Execute(t.Context(), fixture.options(), request, nil)
			requireExecCode(t, err, test.code)
			assertNoRestoreArtifacts(t, fixture)
		})
	}
}

func TestExecuteRejectsChangedPlan(t *testing.T) {
	fixture := newPlanFixture(t)
	plan, err := BuildPlan(t.Context(), fixture.options(), fixture.backup(2).ID)
	mustNoError(t, err)
	request := restoreRequest(plan)
	request.Plan.Digest = strings.Repeat("0", 64)
	_, err = Execute(t.Context(), fixture.options(), request, nil)
	requirePlanCode(t, err, CodePlanChanged)
	assertNoRestoreArtifacts(t, fixture)
}

func TestExecuteAuthenticationFailureCleansUp(t *testing.T) {
	fixture := newPlanFixture(t)
	plan, err := BuildPlan(t.Context(), fixture.options(), fixture.backup(2).ID)
	mustNoError(t, err)
	// The plan checks only sizes; a same-size corruption is caught by the
	// artifact-free digest check in Execute before any extract or prepare.
	corrupted := filepath.Join(fixture.repoRoot, filepath.FromSlash(fixture.backup(2).ObjectPath), memberPayload)
	mustNoError(t, os.WriteFile(corrupted, repeated('x', fixture.backup(2).PayloadSize), 0o600))
	request := restoreRequest(plan)
	result, err := Execute(t.Context(), fixture.options(), request, nil)
	if result.ExportPath != "" {
		t.Fatal("a failed restore reported an export")
	}
	execErr := requireExecCode(t, err, CodeAuthenticationFailed)
	if execErr.Phase != string(PhaseAuthenticating) {
		t.Fatalf("failure phase = %q, want authenticating", execErr.Phase)
	}
	if !execErr.SideEffects.WorkDirRemoved || !execErr.SideEffects.PendingExportRemoved || execErr.SideEffects.ExportPublished {
		t.Fatalf("side effects = %+v", execErr.SideEffects)
	}
	assertNoRestoreArtifacts(t, fixture)
}

func assertNoRestoreArtifacts(t *testing.T, fixture *planFixture) {
	t.Helper()
	entries, err := os.ReadDir(fixture.workRoot)
	mustNoError(t, err)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "restore-") {
			t.Fatal("a work directory survived")
		}
	}
	entries, err = os.ReadDir(fixture.exportRoot)
	mustNoError(t, err)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pending-") {
			t.Fatal("a pending export survived")
		}
	}
}

func TestExecuteCanceledContextDoesNotPublish(t *testing.T) {
	fixture := newPlanFixture(t)
	plan, err := BuildPlan(t.Context(), fixture.options(), fixture.backup(2).ID)
	mustNoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = Execute(ctx, fixture.options(), restoreRequest(plan), nil)
	if err == nil {
		t.Fatal("a canceled execution reported success")
	}
	var execErr *ExecError
	if errors.As(err, &execErr) && execErr.Code != CodeCanceled {
		t.Fatalf("canceled execution code = %q", execErr.Code)
	}
	assertNoRestoreArtifacts(t, fixture)
}

// TestToolFailureCode covers the disk-full classification of a tool step. The
// extractor and the preparer write the bytes themselves, so a real ENOSPC under a
// tool never reaches the parent as an errno: the exhausted work filesystem is the
// observation that names it, while an ordinary tool failure keeps its stage code
// and a cancellation still wins over both.
func TestToolFailureCode(t *testing.T) {
	toolErr := errors.New("physical: native tool failed to start, finish, or drain its pipes")
	cases := []struct {
		name      string
		err       error
		exhausted bool
		stageCode string
		want      string
	}{
		{"parent errno", fmt.Errorf("copy member: %w", syscall.ENOSPC), false, CodeExtractFailed, CodeDiskFull},
		{"parent quota", syscall.EDQUOT, false, CodeFetchFailed, CodeDiskFull},
		{"exhausted extractor", toolErr, true, CodeExtractFailed, CodeDiskFull},
		{"exhausted preparer", toolErr, true, CodePrepareFailed, CodeDiskFull},
		{"tool failure on a free disk", toolErr, false, CodeExtractFailed, CodeExtractFailed},
		{"prepare failure on a free disk", toolErr, false, CodePrepareFailed, CodePrepareFailed},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := toolFailureCode(test.err, test.exhausted, test.stageCode); got != test.want {
				t.Fatalf("code = %q, want %q", got, test.want)
			}
		})
	}
	staged := stage(CodeDiskFull, toolErr)
	if got := (&execution{}).failureCode(t.Context(), "", staged); got != CodeDiskFull {
		t.Fatalf("failure code = %q, want %q", got, CodeDiskFull)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if got := (&execution{}).failureCode(canceled, "", staged); got != CodeCanceled {
		t.Fatalf("canceled failure code = %q, want %s", got, CodeCanceled)
	}
}

// TestSpaceExhaustedUsesTheReportedBlockSize requires the exhaustion threshold to
// be the block size the filesystem reports, never a fixed page size: a filesystem
// with 64 KiB blocks and 63 KiB free cannot take another write, while a 4 KiB one
// with 63 KiB free is not exhausted. An observation that reports no block size
// only counts a measured zero, so the threshold is never invented.
func TestSpaceExhaustedUsesTheReportedBlockSize(t *testing.T) {
	cases := []struct {
		name  string
		space physical.Space
		want  bool
	}{
		{"page blocks, empty", physical.Space{AvailableBytes: 0, BlockBytes: 4096}, true},
		{"page blocks, below one block", physical.Space{AvailableBytes: 4095, BlockBytes: 4096}, true},
		{"page blocks, exactly one block", physical.Space{AvailableBytes: 4096, BlockBytes: 4096}, false},
		{"large blocks, below one block", physical.Space{AvailableBytes: 63 << 10, BlockBytes: 64 << 10}, true},
		{"large blocks, exactly one block", physical.Space{AvailableBytes: 64 << 10, BlockBytes: 64 << 10}, false},
		{"large blocks, plenty", physical.Space{AvailableBytes: 4 << 20, BlockBytes: 128 << 10}, false},
		{"unreported block size, empty", physical.Space{AvailableBytes: 0, BlockBytes: 0}, true},
		{"unreported block size, bytes left", physical.Space{AvailableBytes: 1, BlockBytes: 0}, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := spaceExhausted(test.space); got != test.want {
				t.Fatalf("exhausted = %v, want %v for %+v", got, test.want, test.space)
			}
		})
	}
}
