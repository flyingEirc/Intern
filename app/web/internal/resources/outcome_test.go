package resources

import (
	"testing"

	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/physical"
)

// TestPreflightOutcomeCarriesTheToolVersion is F4: the pure mapping from the
// physical observation to the service outcome is where the probed XtraBackup
// version crosses the boundary, so a change that drops the field fails here
// rather than only in the native container run.
func TestPreflightOutcomeCarriesTheToolVersion(t *testing.T) {
	// The two versions differ on every pinned fixture, so a mapping that reads
	// the server version cannot pass.
	result := physical.PreflightResult{
		Identity: physical.Identity{UUID: "0f0e0d0c-0b0a-4908-8706-050403020100", Version: "8.4.11"},
		Tool:     physical.ToolVersion{Version: "8.4.0-7", Raw: "/usr/bin/xtrabackup version 8.4.0-7\n"},
		Checks:   []string{"identity", "compatibility"},
		WarningDetails: []physical.PreflightWarning{
			{Code: "compatibility_candidate_unverified", Message: "candidate"},
		},
	}
	assertOutcomeCarriesObservation(t, PreflightOutcomeOf(result), result)
}

// assertOutcomeCarriesObservation requires every field of the physical
// observation to survive the mapping.
func assertOutcomeCarriesObservation(t *testing.T, outcome PreflightOutcome, result physical.PreflightResult) {
	t.Helper()
	if outcome.ToolVersion != result.Tool.Version {
		t.Fatalf("tool version = %q, want the probed %q", outcome.ToolVersion, result.Tool.Version)
	}
	if outcome.ToolVersion == outcome.ServerVersion {
		t.Fatalf("tool version %q is the server version", outcome.ToolVersion)
	}
	if outcome.ServerVersion != result.Identity.Version || outcome.ServerUUID != result.Identity.UUID {
		t.Fatalf("outcome identity = %+v, want %+v", outcome, result.Identity)
	}
	assertOutcomeCarriesWarnings(t, outcome, result)
}

// assertOutcomeCarriesWarnings requires the checks, warnings and success state
// to survive the mapping too.
func assertOutcomeCarriesWarnings(t *testing.T, outcome PreflightOutcome, result physical.PreflightResult) {
	t.Helper()
	if len(outcome.Checks) != len(result.Checks) || len(outcome.Warnings) != len(result.WarningDetails) {
		t.Fatalf("outcome = %+v, want the checks and warnings carried through", outcome)
	}
	if outcome.Warnings[0].Code != result.WarningDetails[0].Code {
		t.Fatalf("warning = %+v", outcome.Warnings[0])
	}
	if !outcome.OK() || outcome.Failure != nil {
		t.Fatalf("outcome = %+v, want a success", outcome)
	}
	if record := outcome.Record(); record.ServerVersion != result.Identity.Version {
		t.Fatalf("record = %+v", record)
	}
}

// TestPreflightOutcomeKeepsAMissingToolVersionEmpty requires the mapping to
// report an absent probe result as absent instead of substituting anything: the
// physical chain refuses an empty tool version rather than writing a wrong one.
func TestPreflightOutcomeKeepsAMissingToolVersionEmpty(t *testing.T) {
	outcome := PreflightOutcomeOf(physical.PreflightResult{
		Identity: physical.Identity{Version: "8.4.11"},
	})
	if outcome.ToolVersion != "" {
		t.Fatalf("tool version = %q, want the empty observation", outcome.ToolVersion)
	}
	if outcome.Checks != nil || outcome.Warnings != nil {
		t.Fatalf("outcome = %+v, want the empty slices to stay empty", outcome)
	}
	if record := outcome.Record(); record.Checks == nil || record.Warnings == nil {
		t.Fatalf("record = %+v, want non-nil slices for the wire shape", record)
	}
	_ = store.PreflightRecord{}
}
