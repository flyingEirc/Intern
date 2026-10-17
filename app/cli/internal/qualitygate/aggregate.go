package qualitygate

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/flyingEirc/IotProject/internal/fsutil"
)

type Jobs struct {
	Required                bool
	Scope, Fast, Containers string
}

func ValidateJobs(j Jobs) error {
	if j.Scope != "success" {
		return errors.New("change-scope analysis did not succeed")
	}
	if j.Required {
		if j.Fast != "success" || j.Containers != "success" {
			return errors.New("required quality jobs did not all succeed")
		}
		return nil
	}
	if !optionalJob(j.Fast) || !optionalJob(j.Containers) {
		return errors.New("documentation-only run contains a failed or canceled job")
	}
	return nil
}
func optionalJob(state string) bool { return state == "success" || state == "skipped" }
func VerifyDirectory(dir string, want Verification) (result Result, retErr error) {
	var candidate Candidate
	if err := ReadJSON(filepath.Join(dir, "candidate.json"), &candidate); err != nil {
		return result, err
	}
	receipts, err := ReadReceipts(filepath.Join(dir, "receipts"))
	if err != nil {
		return result, err
	}
	events, err := fsutil.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return result, err
	}
	defer fsutil.Close(&retErr, events)
	return Verify(events, candidate, receipts, want)
}
func Aggregate(dir, source, platform string, jobs Jobs) ([]Result, error) {
	if err := ValidateJobs(jobs); err != nil {
		return nil, err
	}
	if !jobs.Required {
		return []Result{{Status: "not-required", SourceSHA: source}}, nil
	}
	contract, err := Load()
	if err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(contract.Profiles))
	for _, profile := range contract.Profiles {
		result, verifyErr := VerifyDirectory(filepath.Join(dir, "container-"+profile.ID), Verification{SourceSHA: source, Profile: profile.ID, Platform: platform, RequireClean: true})
		if verifyErr != nil {
			return nil, fmt.Errorf("%s: %w", profile.ID, verifyErr)
		}
		results = append(results, result)
	}
	return results, nil
}
