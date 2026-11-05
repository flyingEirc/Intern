package testenv

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"os"
	"strings"
)

const (
	// runnerLabel identifies every CLI quality resource the harness of one
	// self-hosted runner creates, for .github/scripts/cli-quality-clean.sh.
	runnerLabel = "intern.quality.runner"
	// caseLabel identifies the resources of one scenario, so a leaked one names
	// the case that produced it.
	caseLabel = "intern.quality.case"
)

// runnerScope returns the labels every resource of the suite carries. Inside a
// GitHub Actions job the value is the sha256 prefix of RUNNER_NAME, the same
// scope the cleanup script derives, so labelled leftovers of a cancelled job
// can be removed without touching another runner's resources. A local run has
// no runner identity and therefore no runner label: no CI cleanup step can see
// it, and the normal t.Cleanup path owns it.
func runnerScope() (map[string]string, error) {
	labels := map[string]string{}
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		return labels, nil
	}
	runner := os.Getenv("RUNNER_NAME")
	if strings.TrimSpace(runner) == "" {
		return nil, errors.New("RUNNER_NAME is required in GitHub Actions: the cleanup step finds this run's labelled resources by it")
	}
	sum := sha256.Sum256([]byte(runner))
	labels[runnerLabel] = hex.EncodeToString(sum[:])[:8]
	return labels, nil
}

// caseScope adds the case label to the suite-wide runner scope.
func caseScope(runner map[string]string, prefix string) map[string]string {
	labels := make(map[string]string, len(runner)+1)
	maps.Copy(labels, runner)
	labels[caseLabel] = prefix
	return labels
}
