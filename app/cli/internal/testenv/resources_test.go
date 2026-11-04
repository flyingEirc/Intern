package testenv

import "testing"

func TestRunnerScopeOutsideActionsHasNoRunnerLabel(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("RUNNER_NAME", "ignored")
	labels, err := runnerScope()
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 0 {
		t.Fatalf("local scope is %v, want no labels: no CI cleanup step may see it", labels)
	}
}

func TestRunnerScopeIsTheRunnerNameDigest(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("RUNNER_NAME", "JackVance")
	labels, err := runnerScope()
	if err != nil {
		t.Fatal(err)
	}
	// The cleanup script derives the same value with `sha256sum | cut -c1-8`.
	if got, want := labels[runnerLabel], "379e1e15"; got != want {
		t.Fatalf("runner label is %q, want %q: the Go harness and cli-quality-clean.sh must agree", got, want)
	}
	if len(labels) != 1 {
		t.Fatalf("scope is %v, want only the runner label", labels)
	}
}

func TestRunnerScopeRequiresRunnerName(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("RUNNER_NAME", "")
	if labels, err := runnerScope(); err == nil {
		t.Fatalf("empty RUNNER_NAME produced scope %v, want an explicit failure", labels)
	}
}

func TestCaseScopeAddsTheCaseLabel(t *testing.T) {
	runner := map[string]string{runnerLabel: "379e1e15"}
	labels := caseScope(runner, "intern-abc")
	if got, want := labels[caseLabel], "intern-abc"; got != want {
		t.Fatalf("case label is %q, want %q", got, want)
	}
	if got, want := labels[runnerLabel], "379e1e15"; got != want {
		t.Fatalf("runner label is %q, want %q", got, want)
	}
	if _, ok := runner[caseLabel]; ok {
		t.Fatal("caseScope mutated the suite-wide runner labels")
	}
}
