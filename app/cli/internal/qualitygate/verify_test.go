package qualitygate

import (
	"bytes"
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

const testRunID = "41871999890"

func fixture(t *testing.T) (Candidate, []Receipt, []byte, Verification) {
	t.Helper()
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p := c.Profiles[0]
	_, digest, _ := strings.Cut(p.Image, "@")
	setCIEnvironment(t, testRunID)
	candidate := Candidate{FormatVersion: 1, SourceSHA: strings.Repeat("a", 40), Profile: p.ID, Platform: "linux/amd64", BuilderImage: c.BuilderImage, DatabaseImage: p.Image, TestcontainersVersion: c.TestcontainersVersion, GoVersion: "go1.27.0", RuntimeVersion: "29.4.0", ImageID: "sha256:" + strings.Repeat("b", 64), BinarySHA256: strings.Repeat("c", 64), ToolVersions: map[string]string{"mysql": p.Version, "mysqldump": p.Version, "mysqlbinlog": p.Version}, CIURL: testRunURL(testRunID)}
	receipts := make([]Receipt, 0, len(c.Cases))
	var events bytes.Buffer
	start := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	for _, id := range c.Cases {
		receipt := Receipt{FormatVersion: 1, CaseID: id, SourceSHA: candidate.SourceSHA, Profile: p.ID, Platform: candidate.Platform, CandidateImageID: candidate.ImageID, BinarySHA256: candidate.BinarySHA256, StartedAt: start, FinishedAt: start.Add(time.Second), Status: "passed", Stage: "verified", Instances: []Instance{{Role: "source", UUID: "source", Version: p.Version, ImageID: digest}, {Role: "target", UUID: "target", Version: p.Version, ImageID: digest}}, Assertions: c.Assertions[id], ResourcesCreated: 4, ResourcesRemoved: 4, CleanupComplete: true}
		if id == "checkpoint_chain" {
			receipt.Checkpoints = checkpointChainFixture()
		}
		receipts = append(receipts, receipt)
		appendEvent(t, &events, Event{Action: "pass", Package: IntegrationPackage, Test: "TestMySQL/" + id})
	}
	appendEvent(t, &events, Event{Action: "pass", Package: IntegrationPackage})
	return candidate, receipts, events.Bytes(), Verification{SourceSHA: candidate.SourceSHA, Profile: p.ID, Platform: candidate.Platform, RequireClean: true}
}

func setCIEnvironment(t *testing.T, runID string) {
	t.Helper()
	t.Setenv("GITHUB_SERVER_URL", "https://github.com")
	t.Setenv("GITHUB_REPOSITORY", "flyingEirc/Intern")
	t.Setenv("GITHUB_RUN_ID", runID)
}

func appendEvent(t *testing.T, b *bytes.Buffer, event Event) {
	t.Helper()
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	b.Write(data)
	b.WriteByte('\n')
}

func testRunURL(runID string) string {
	return "https://github.com/flyingEirc/Intern/actions/runs/" + runID
}

// checkpointChainFixture is the checkpoint chain the scenario records: the
// full backup and two incrementals, in source binary log order.
func checkpointChainFixture() []Checkpoint {
	return []Checkpoint{
		{BackupID: strings.Repeat("1", 32), File: "binlog.000003", Position: 6986},
		{BackupID: strings.Repeat("2", 32), File: "binlog.000004", Position: 4},
		{BackupID: strings.Repeat("3", 32), File: "binlog.000006", Position: 4},
	}
}

func receiptOf(t *testing.T, receipts []Receipt, id string) *Receipt {
	t.Helper()
	for index := range receipts {
		if receipts[index].CaseID == id {
			return &receipts[index]
		}
	}
	t.Fatalf("fixture has no receipt for %s", id)
	return nil
}

func TestGateRequiresCompleteExecutedEvidence(t *testing.T) {
	candidate, receipts, events, want := fixture(t)
	if result, err := Verify(bytes.NewReader(events), candidate, receipts, want); err != nil || result.Status != "passed" {
		t.Fatalf("complete evidence rejected: %+v %v", result, err)
	}
	cases := []struct {
		name   string
		mutate func(*Candidate, *[]Receipt, *[]byte)
	}{
		{"missing-case", func(_ *Candidate, r *[]Receipt, _ *[]byte) { *r = (*r)[1:] }},
		{"duplicate-case", func(_ *Candidate, r *[]Receipt, _ *[]byte) { *r = append(*r, (*r)[0]) }},
		{"cleanup-failed", func(_ *Candidate, r *[]Receipt, _ *[]byte) { (*r)[0].CleanupComplete = false }},
		{"missing-oracle", func(_ *Candidate, r *[]Receipt, _ *[]byte) { (*r)[0].Assertions = (*r)[0].Assertions[1:] }},
		{"stale-source", func(c *Candidate, _ *[]Receipt, _ *[]byte) { c.SourceSHA = strings.Repeat("d", 40) }},
		{"wrong-binary", func(_ *Candidate, r *[]Receipt, _ *[]byte) { (*r)[0].BinarySHA256 = strings.Repeat("e", 64) }},
		{"dirty-ci-source", func(c *Candidate, _ *[]Receipt, _ *[]byte) { c.SourceDirty = true }},
		{"setup-only", func(_ *Candidate, r *[]Receipt, _ *[]byte) { (*r)[0].Stage = "setup" }},
		{"zero-events", func(_ *Candidate, _ *[]Receipt, e *[]byte) { *e = nil }},
		{"cached", func(_ *Candidate, _ *[]Receipt, e *[]byte) {
			*e = append(*e, []byte("{\"Action\":\"output\",\"Package\":\""+IntegrationPackage+"\",\"Output\":\"ok (cached)\"}\n")...)
		}},
		{"skipped", func(_ *Candidate, _ *[]Receipt, e *[]byte) {
			*e = append(*e, []byte("{\"Action\":\"skip\",\"Package\":\""+IntegrationPackage+"\",\"Test\":\"TestMySQL/full_objects\"}\n")...)
		}},
		{"failed", func(_ *Candidate, _ *[]Receipt, e *[]byte) {
			*e = append(*e, []byte("{\"Action\":\"fail\",\"Package\":\""+IntegrationPackage+"\"}\n")...)
		}},
		{"truncated-json", func(_ *Candidate, _ *[]Receipt, e *[]byte) { *e = append(*e, []byte("{\"Action\":")...) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, r, e, v := fixture(t)
			tc.mutate(&c, &r, &e)
			if _, err := Verify(bytes.NewReader(e), c, r, v); err == nil {
				t.Fatal("incomplete or failed evidence accepted")
			}
		})
	}
}

func TestRequiredJobsDoNotTreatSkipOrCancelAsSuccess(t *testing.T) {
	for _, state := range []string{"skipped", "cancelled", "failure", "", "neutral"} {
		t.Run(state, func(t *testing.T) {
			if err := ValidateJobs(Jobs{Required: true, Scope: "success", Fast: "success", Containers: state}); err == nil {
				t.Fatal("non-success container job passed required gate")
			}
		})
	}
	if err := ValidateJobs(Jobs{Required: false, Scope: "success", Fast: "skipped", Containers: "skipped"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateJobs(Jobs{Required: false, Scope: "failure", Fast: "skipped", Containers: "skipped"}); err == nil {
		t.Fatal("failed change classification bypassed gate")
	}
}

// TestGateRejectsUnboundEvidence covers every rule that binds the evidence to
// this workflow run and to the locked profile. Each mutation was accepted
// before the rule existed.
func TestGateRejectsUnboundEvidence(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testing.T, *Candidate, *[]Receipt)
	}{
		{"empty-ci-url", func(_ *testing.T, c *Candidate, _ *[]Receipt) { c.CIURL = "" }},
		{"ci-url-other-run", func(_ *testing.T, c *Candidate, _ *[]Receipt) { c.CIURL = testRunURL("41871999891") }},
		{"ci-url-other-repository", func(_ *testing.T, c *Candidate, _ *[]Receipt) {
			c.CIURL = "https://github.com/someone/Intern/actions/runs/" + testRunID
		}},
		{"ci-environment-missing", func(t *testing.T, _ *Candidate, _ *[]Receipt) { t.Setenv("GITHUB_RUN_ID", "") }},
		{"source-version-mismatch", func(t *testing.T, _ *Candidate, r *[]Receipt) {
			receiptOf(t, *r, "full_objects").Instances[0].Version = "8.4.11"
		}},
		{"target-version-mismatch", func(t *testing.T, _ *Candidate, r *[]Receipt) {
			receiptOf(t, *r, "full_objects").Instances[1].Version = "8.4.11"
		}},
		{"source-image-mismatch", func(t *testing.T, c *Candidate, r *[]Receipt) {
			receiptOf(t, *r, "full_objects").Instances[0].ImageID = c.ImageID
		}},
		{"target-image-mismatch", func(t *testing.T, c *Candidate, r *[]Receipt) {
			receiptOf(t, *r, "full_objects").Instances[1].ImageID = c.ImageID
		}},
		{"missing-source-role", func(t *testing.T, _ *Candidate, r *[]Receipt) {
			receiptOf(t, *r, "full_objects").Instances[0].Role = "origin"
		}},
		{"missing-target-role", func(t *testing.T, _ *Candidate, r *[]Receipt) {
			receiptOf(t, *r, "full_objects").Instances[1].Role = "replica"
		}},
		{"checkpoints-removed", func(t *testing.T, _ *Candidate, r *[]Receipt) {
			receiptOf(t, *r, "checkpoint_chain").Checkpoints = nil
		}},
		{"checkpoints-short", func(t *testing.T, _ *Candidate, r *[]Receipt) {
			receipt := receiptOf(t, *r, "checkpoint_chain")
			receipt.Checkpoints = receipt.Checkpoints[:len(receipt.Checkpoints)-1]
		}},
		{"checkpoints-reversed", func(t *testing.T, _ *Candidate, r *[]Receipt) {
			slices.Reverse(receiptOf(t, *r, "checkpoint_chain").Checkpoints)
		}},
		{"checkpoints-out-of-order-in-file", func(t *testing.T, _ *Candidate, r *[]Receipt) {
			receipt := receiptOf(t, *r, "checkpoint_chain")
			receipt.Checkpoints[1].File = receipt.Checkpoints[0].File
			receipt.Checkpoints[1].Position = receipt.Checkpoints[0].Position - 1
		}},
		{"checkpoints-duplicate", func(t *testing.T, _ *Candidate, r *[]Receipt) {
			receipt := receiptOf(t, *r, "checkpoint_chain")
			receipt.Checkpoints[1].BackupID = receipt.Checkpoints[0].BackupID
		}},
		{"checkpoint-not-in-source-log", func(t *testing.T, _ *Candidate, r *[]Receipt) {
			receiptOf(t, *r, "checkpoint_chain").Checkpoints[0].File = "../../etc/passwd"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate, receipts, events, want := fixture(t)
			tc.mutate(t, &candidate, &receipts)
			if _, err := Verify(bytes.NewReader(events), candidate, receipts, want); err == nil {
				t.Fatal("unbound or degraded evidence accepted")
			}
		})
	}
}

// TestCandidateWithoutCIURLCannotSatisfyCI proves that a candidate.json in
// which the emitter omitted ci_url entirely is rejected, not only an empty one.
func TestCandidateWithoutCIURLCannotSatisfyCI(t *testing.T) {
	candidate, receipts, events, want := fixture(t)
	candidate.CIURL = ""
	data, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("ci_url")) {
		t.Fatal("fixture did not omit ci_url")
	}
	var decoded Candidate
	if err = json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(bytes.NewReader(events), decoded, receipts, want); err == nil {
		t.Fatal("candidate without ci_url passed the CI gate")
	}
}

func TestLocalEvidenceDoesNotNeedCIRunURL(t *testing.T) {
	candidate, receipts, events, want := fixture(t)
	candidate.CIURL = ""
	want.RequireClean = false
	if _, err := Verify(bytes.NewReader(events), candidate, receipts, want); err != nil {
		t.Fatalf("local evidence rejected: %v", err)
	}
}

// TestGateAcceptsDeliberateOtherVersionInstance pins the exception the profile
// lock needs: version_refusal starts the other locked server version, so only
// the source and target roles are bound to the profile image.
func TestGateAcceptsDeliberateOtherVersionInstance(t *testing.T) {
	candidate, receipts, events, want := fixture(t)
	receipt := receiptOf(t, receipts, "version_refusal")
	receipt.Instances = append(receipt.Instances, Instance{Role: "other-version", UUID: "other-version", Version: "8.4.11", ImageID: "sha256:" + strings.Repeat("d", 64)})
	if _, err := Verify(bytes.NewReader(events), candidate, receipts, want); err != nil {
		t.Fatalf("deliberately different other-version instance rejected: %v", err)
	}
}
