// Package qualitygate validates complete, version-bound real-container test evidence.
package qualitygate

import (
	"embed"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"
	"time"
)

//go:embed contract.json
var contractFiles embed.FS

type ClientPackage struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}
type Profile struct {
	ID             string                   `json:"id"`
	Version        string                   `json:"version"`
	Image          string                   `json:"image"`
	ClientPackages map[string]ClientPackage `json:"client_packages"`
}
type Contract struct {
	FormatVersion         int                 `json:"format_version"`
	TestcontainersVersion string              `json:"testcontainers_version"`
	BuilderImage          string              `json:"builder_image"`
	Platforms             []string            `json:"platforms"`
	Profiles              []Profile           `json:"profiles"`
	Cases                 []string            `json:"cases"`
	Assertions            map[string][]string `json:"assertions"`
}

func Load() (Contract, error) {
	data, err := contractFiles.ReadFile("contract.json")
	if err != nil {
		return Contract{}, err
	}
	var c Contract
	if err = json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	if c.FormatVersion != 1 || len(c.Cases) == 0 || len(c.Profiles) == 0 {
		return c, errors.New("empty or unsupported gate contract")
	}
	if err = uniqueNames(c.Cases); err != nil {
		return c, err
	}
	if err = validateAssertionsContract(c); err != nil {
		return c, err
	}
	profiles := make([]string, 0, len(c.Profiles))
	for _, p := range c.Profiles {
		if !strings.Contains(p.Image, "@sha256:") {
			return c, errors.New("unlocked database image")
		}
		profiles = append(profiles, p.ID)
	}
	return c, uniqueNames(profiles)
}
func uniqueNames(names []string) error {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if name == "" || strings.ContainsAny(name, "/\\ \t\r\n") || seen[name] {
			return errors.New("empty, unsafe or duplicate matrix identifier")
		}
		seen[name] = true
	}
	return nil
}
func (c Contract) Profile(id string) (Profile, error) {
	for _, p := range c.Profiles {
		if p.ID == id {
			return p, nil
		}
	}
	return Profile{}, errors.New("profile is not in the locked matrix")
}
func (c Contract) HasCase(id string) bool { return slices.Contains(c.Cases, id) }
func (p Profile) ClientPackage(arch string) (ClientPackage, error) {
	pkg, ok := p.ClientPackages[arch]
	if !ok || !strings.HasPrefix(pkg.URL, "https://repo.mysql.com/") || !checksum.MatchString(pkg.SHA256) {
		return ClientPackage{}, errors.New("no verified official client package for platform")
	}
	return pkg, nil
}

type Candidate struct {
	FormatVersion         int               `json:"format_version"`
	SourceSHA             string            `json:"source_sha"`
	SourceDirty           bool              `json:"source_dirty"`
	Profile               string            `json:"profile"`
	Platform              string            `json:"platform"`
	HarnessPlatform       string            `json:"harness_platform"`
	GoVersion             string            `json:"go_version"`
	RuntimeVersion        string            `json:"runtime_version"`
	TestcontainersVersion string            `json:"testcontainers_version"`
	BuilderImage          string            `json:"builder_image"`
	DatabaseImage         string            `json:"database_image"`
	ImageID               string            `json:"candidate_image_id"`
	BinarySHA256          string            `json:"binary_sha256"`
	ToolVersions          map[string]string `json:"tool_versions"`
	CIURL                 string            `json:"ci_url,omitempty"`
}
type Instance struct {
	Role    string `json:"role"`
	UUID    string `json:"uuid"`
	Version string `json:"version"`
	ImageID string `json:"image_id"`
}
type Checkpoint struct {
	BackupID string `json:"backup_id"`
	File     string `json:"file"`
	Position int64  `json:"position"`
}
type Receipt struct {
	FormatVersion    int          `json:"format_version"`
	CaseID           string       `json:"case_id"`
	SourceSHA        string       `json:"source_sha"`
	Profile          string       `json:"profile"`
	Platform         string       `json:"platform"`
	CandidateImageID string       `json:"candidate_image_id"`
	BinarySHA256     string       `json:"binary_sha256"`
	StartedAt        time.Time    `json:"started_at"`
	FinishedAt       time.Time    `json:"finished_at"`
	Status           string       `json:"status"`
	Stage            string       `json:"stage"`
	Instances        []Instance   `json:"instances"`
	Checkpoints      []Checkpoint `json:"checkpoints,omitempty"`
	Assertions       []string     `json:"assertions"`
	ResourcesCreated int          `json:"resources_created"`
	ResourcesRemoved int          `json:"resources_removed"`
	CleanupComplete  bool         `json:"cleanup_complete"`
}

func validateAssertionsContract(c Contract) error {
	for _, id := range c.Cases {
		required := c.Assertions[id]
		if len(required) == 0 {
			return errors.New("scenario has no required semantic assertions")
		}
		if err := uniqueNames(required); err != nil {
			return err
		}
	}
	return nil
}
