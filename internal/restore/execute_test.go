package restore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
