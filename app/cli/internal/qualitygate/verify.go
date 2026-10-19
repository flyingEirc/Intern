package qualitygate

import (
	"bufio"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/flyingEirc/Intern/internal/fsutil"
)

const IntegrationPackage = "github.com/flyingEirc/Intern/app/cli/integration"

var (
	checksum = regexp.MustCompile(`^[a-f0-9]{64}$`)
	sourceID = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)
)

type Verification struct {
	SourceSHA, Profile, Platform string
	RequireClean                 bool
}
type (
	Event  struct{ Action, Package, Test, Output string }
	Result struct {
		Status       string `json:"status"`
		SourceSHA    string `json:"source_sha"`
		Profile      string `json:"profile"`
		Platform     string `json:"platform"`
		Cases        int    `json:"cases"`
		BinarySHA256 string `json:"binary_sha256"`
		ImageID      string `json:"candidate_image_id"`
	}
)

func Verify(events io.Reader, candidate Candidate, receipts []Receipt, want Verification) (Result, error) {
	contract, err := Load()
	if err != nil {
		return Result{}, err
	}
	if err = validateCandidate(contract, candidate, want); err != nil {
		return Result{}, err
	}
	passed, err := readEvents(events)
	if err != nil {
		return Result{}, err
	}
	if err = validateReceipts(contract, candidate, receipts, passed); err != nil {
		return Result{}, err
	}
	return Result{Status: "passed", SourceSHA: want.SourceSHA, Profile: want.Profile, Platform: want.Platform, Cases: len(contract.Cases), BinarySHA256: candidate.BinarySHA256, ImageID: candidate.ImageID}, nil
}

func validateCandidate(c Contract, got Candidate, want Verification) error {
	profile, err := c.Profile(want.Profile)
	if err != nil {
		return err
	}
	if got.FormatVersion != 1 || !sourceID.MatchString(want.SourceSHA) || got.SourceSHA != want.SourceSHA {
		return errors.New("candidate source identity mismatch")
	}
	if got.Profile != want.Profile || got.Platform != want.Platform || !slices.Contains(c.Platforms, want.Platform) {
		return errors.New("candidate matrix identity mismatch")
	}
	if want.RequireClean && got.SourceDirty {
		return errors.New("CI candidate was built from an uncommitted tree")
	}
	return validateToolchain(c, profile, got)
}

func validateToolchain(c Contract, p Profile, got Candidate) error {
	if got.BuilderImage != c.BuilderImage || got.DatabaseImage != p.Image || got.TestcontainersVersion != c.TestcontainersVersion {
		return errors.New("candidate toolchain differs from locked contract")
	}
	if !checksum.MatchString(got.BinarySHA256) || !validImageID(got.ImageID) || got.RuntimeVersion == "" || got.GoVersion == "" {
		return errors.New("candidate provenance is incomplete")
	}
	for _, tool := range []string{"mysql", "mysqldump", "mysqlbinlog"} {
		if got.ToolVersions[tool] != p.Version {
			return fmt.Errorf("%s version differs from locked server profile", tool)
		}
	}
	return nil
}

func validImageID(id string) bool {
	return strings.HasPrefix(id, "sha256:") && checksum.MatchString(strings.TrimPrefix(id, "sha256:"))
}

func readEvents(r io.Reader) (map[string]bool, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	passed := map[string]bool{}
	packagePassed := false
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, errors.New("invalid or truncated go test JSON event")
		}
		finished, err := consumeEvent(event, passed)
		if err != nil {
			return nil, err
		}
		packagePassed = packagePassed || finished
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if !packagePassed {
		return nil, errors.New("integration package did not finish successfully")
	}
	return passed, nil
}

func consumeEvent(event Event, passed map[string]bool) (bool, error) {
	if event.Package == IntegrationPackage && strings.Contains(event.Output, "(cached)") {
		return false, errors.New("cached integration results cannot satisfy the gate")
	}
	if event.Action == "fail" || event.Action == "skip" {
		return false, fmt.Errorf("test stream contains %s: %s", event.Action, event.Test)
	}
	if event.Package != IntegrationPackage || event.Action != "pass" {
		return false, nil
	}
	if event.Test == "" {
		return true, nil
	}
	passed[event.Test] = true
	return false, nil
}

func validateReceipts(c Contract, candidate Candidate, receipts []Receipt, passed map[string]bool) error {
	seen := map[string]bool{}
	for _, r := range receipts {
		if !c.HasCase(r.CaseID) || seen[r.CaseID] {
			return errors.New("unknown or duplicate scenario receipt")
		}
		seen[r.CaseID] = true
		if err := validateReceipt(candidate, r); err != nil {
			return fmt.Errorf("%s: %w", r.CaseID, err)
		}
		for _, assertion := range c.Assertions[r.CaseID] {
			if !slices.Contains(r.Assertions, assertion) {
				return fmt.Errorf("%s: required oracle did not run: %s", r.CaseID, assertion)
			}
		}
	}
	for _, id := range c.Cases {
		if !seen[id] || !passed["TestMySQL/"+id] {
			return fmt.Errorf("required scenario did not execute and pass: %s", id)
		}
	}
	return nil
}

func validateReceipt(c Candidate, r Receipt) error {
	if !matchesCandidate(c, r) {
		return errors.New("scenario belongs to another source, matrix or candidate")
	}
	if !completedReceipt(r) {
		return errors.New("scenario assertions or cleanup did not complete")
	}
	if len(r.Assertions) == 0 || len(r.Instances) < 2 {
		return errors.New("scenario lacks database/oracle evidence")
	}
	return validateInstances(r.Instances)
}

func matchesCandidate(c Candidate, r Receipt) bool {
	return r.FormatVersion == 1 && r.SourceSHA == c.SourceSHA && r.Profile == c.Profile && r.Platform == c.Platform && r.CandidateImageID == c.ImageID && r.BinarySHA256 == c.BinarySHA256
}

func completedReceipt(r Receipt) bool {
	return r.Status == "passed" && r.Stage == "verified" && !r.StartedAt.IsZero() && !r.FinishedAt.Before(r.StartedAt) && r.CleanupComplete && r.ResourcesCreated >= 4 && r.ResourcesCreated == r.ResourcesRemoved
}

func validateInstances(instances []Instance) error {
	identities := map[string]bool{}
	for _, i := range instances {
		if i.Role == "" || i.UUID == "" || i.Version == "" || !validImageID(i.ImageID) {
			return errors.New("incomplete database identity evidence")
		}
		if identities[i.UUID] {
			return errors.New("source and target did not use independent instances")
		}
		identities[i.UUID] = true
	}
	return nil
}

func ReadJSON(path string, dst any) (retErr error) {
	f, err := fsutil.Open(path)
	if err != nil {
		return err
	}
	defer fsutil.Close(&retErr, f)
	data, err := io.ReadAll(io.LimitReader(f, 4<<20+1))
	if err != nil {
		return err
	}
	if len(data) > 4<<20 {
		return errors.New("oversized gate JSON file")
	}
	return json.Unmarshal(data, dst)
}

func ReadReceipts(dir string) ([]Receipt, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	receipts := make([]Receipt, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return nil, errors.New("unexpected receipt file")
		}
		var r Receipt
		if err = ReadJSON(filepath.Join(dir, entry.Name()), &r); err != nil {
			return nil, err
		}
		receipts = append(receipts, r)
	}
	return receipts, nil
}
