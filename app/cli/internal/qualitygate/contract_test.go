package qualitygate

import (
	"slices"
	"testing"
)

// Changing the quality matrix is a deliberate decision: this pin fails whenever
// the locked cases or profiles are added, removed, renamed or reordered without
// updating it. Without it a scenario deleted from both the integration tests and
// contract.json would shrink the matrix silently, because every remaining
// check is relative to the contract.
var (
	pinnedCases = []string{
		"full_objects", "empty_database", "codec_matrix", "checkpoint_chain",
		"recovery_guards", "source_discontinuity", "permissions_tls",
		"interruptions", "storage_failures", "version_refusal", "scope_rejections",
		"scheduler", "lifecycle", "configuration",
	}
	pinnedProfiles = []string{"mysql-9.7.1", "mysql-8.4.11"}
)

func TestContractMatrixIsPinned(t *testing.T) {
	contract, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(contract.Cases, pinnedCases) {
		t.Fatalf("locked cases differ from the pinned matrix: %v", contract.Cases)
	}
	profiles := make([]string, 0, len(contract.Profiles))
	for _, profile := range contract.Profiles {
		profiles = append(profiles, profile.ID)
	}
	if !slices.Equal(profiles, pinnedProfiles) {
		t.Fatalf("locked profiles differ from the pinned matrix: %v", profiles)
	}
}
