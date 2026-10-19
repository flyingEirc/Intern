package qualitygate

import (
	"bytes"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (Candidate, []Receipt, []byte, Verification) {
	t.Helper()
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p := c.Profiles[0]
	candidate := Candidate{FormatVersion: 1, SourceSHA: strings.Repeat("a", 40), Profile: p.ID, Platform: "linux/amd64", BuilderImage: c.BuilderImage, DatabaseImage: p.Image, TestcontainersVersion: c.TestcontainersVersion, GoVersion: "go1.27.0", RuntimeVersion: "29.4.0", ImageID: "sha256:" + strings.Repeat("b", 64), BinarySHA256: strings.Repeat("c", 64), ToolVersions: map[string]string{"mysql": p.Version, "mysqldump": p.Version, "mysqlbinlog": p.Version}}
	receipts := make([]Receipt, 0, len(c.Cases))
	var events bytes.Buffer
	start := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	for _, id := range c.Cases {
		receipts = append(receipts, Receipt{FormatVersion: 1, CaseID: id, SourceSHA: candidate.SourceSHA, Profile: p.ID, Platform: candidate.Platform, CandidateImageID: candidate.ImageID, BinarySHA256: candidate.BinarySHA256, StartedAt: start, FinishedAt: start.Add(time.Second), Status: "passed", Stage: "verified", Instances: []Instance{{Role: "source", UUID: "source", Version: p.Version, ImageID: candidate.ImageID}, {Role: "target", UUID: "target", Version: p.Version, ImageID: candidate.ImageID}}, Assertions: c.Assertions[id], ResourcesCreated: 4, ResourcesRemoved: 4, CleanupComplete: true})
		appendEvent(t, &events, Event{Action: "pass", Package: IntegrationPackage, Test: "TestMySQL/" + id})
	}
	appendEvent(t, &events, Event{Action: "pass", Package: IntegrationPackage})
	return candidate, receipts, events.Bytes(), Verification{SourceSHA: candidate.SourceSHA, Profile: p.ID, Platform: candidate.Platform, RequireClean: true}
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
