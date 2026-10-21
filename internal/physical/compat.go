package physical

import (
	_ "embed"
	"encoding/json/v2"
	"regexp"
	"slices"
	"strings"
	"sync"

	version "github.com/hashicorp/go-version"
)

//go:embed compat.json
var compatibilityJSON []byte

// CompatibilityEntry pins a candidate independently of its verification status.
// An evidence issue is a tracking link, not proof that the candidate passed.
type CompatibilityEntry struct {
	Distribution     string `json:"distribution"`
	ServerVersion    string `json:"server_version"`
	ToolVersion      string `json:"tool_version"`
	Platform         string `json:"platform"`
	Status           string `json:"status"`
	Verified         bool   `json:"verified"`
	EvidenceIssue    string `json:"evidence_issue"`
	MySQLImage       string `json:"mysql_image"`
	PXBImage         string `json:"pxb_image"`
	PXBAMD64Manifest string `json:"pxb_amd64_manifest"`
}

// CompatibilityMatrix returns a fresh copy; callers cannot change admission.
func CompatibilityMatrix() ([]CompatibilityEntry, error) {
	entries, err := compatibilityEntries()
	return slices.Clone(entries), err
}

var compatibilityEntries = sync.OnceValues(func() ([]CompatibilityEntry, error) {
	return parseCompatibilityMatrix(compatibilityJSON)
})

func parseCompatibilityMatrix(raw []byte) ([]CompatibilityEntry, error) {
	var entries []CompatibilityEntry
	if err := json.Unmarshal(raw, &entries, json.RejectUnknownMembers(true)); err != nil {
		return nil, invalidCompatibilityMatrix()
	}
	if len(entries) == 0 {
		return nil, invalidCompatibilityMatrix()
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		key := entry.Distribution + ":" + entry.ServerVersion + ":" + entry.ToolVersion + ":" + entry.Platform
		if !validCompatibilityEntry(entry) || seen[key] {
			return nil, invalidCompatibilityMatrix()
		}
		seen[key] = true
	}
	return entries, nil
}

func validCompatibilityEntry(entry CompatibilityEntry) bool {
	validStatus := entry.Status == "candidate" || entry.Status == "verified"
	return validStatus && entry.Verified == (entry.Status == "verified") &&
		entry.Distribution == "mysql-community" && entry.Platform == "linux/amd64" &&
		serverReleasePattern.MatchString(entry.ServerVersion) && toolReleasePattern.MatchString(entry.ToolVersion) &&
		strings.HasPrefix(entry.EvidenceIssue, "https://github.com/flyingEirc/Intern/issues/") &&
		validImagePins(entry)
}

func validImagePins(entry CompatibilityEntry) bool {
	return imagePinPattern.MatchString(entry.MySQLImage) &&
		imagePinPattern.MatchString(entry.PXBImage) &&
		digestPattern.MatchString(entry.PXBAMD64Manifest)
}

func invalidCompatibilityMatrix() error {
	return preflightFailure("compatibility", "matrix_invalid", "The embedded compatibility matrix is invalid; reinstall the application.")
}

var (
	serverReleasePattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	toolReleasePattern   = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-([1-9][0-9]*)$`)
	imagePinPattern      = regexp.MustCompile(`^[^ \r\n]+@sha256:[0-9a-f]{64}$`)
	digestPattern        = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// CheckCompatibility admits only exact pinned versions on the stated platform.
// Candidate admission is not a verified-support claim; Preflight reports it.
func CheckCompatibility(server Identity, tool ToolVersion, platform string) error {
	_, err := matchingCompatibility(server, tool, platform)
	return err
}

func matchingCompatibility(server Identity, tool ToolVersion, platform string) (CompatibilityEntry, error) {
	var empty CompatibilityEntry
	if server.VersionComment != "MySQL Community Server - GPL" {
		return empty, preflightFailure("compatibility", "distribution_unsupported", "Only the pinned MySQL Community Server distribution is admitted.")
	}
	if !serverReleasePattern.MatchString(server.Version) || !toolReleasePattern.MatchString(tool.Version) {
		return empty, preflightFailure("compatibility", "version_invalid", "The server or XtraBackup version is not a recognized release version.")
	}
	entries, err := compatibilityEntries()
	if err != nil {
		return empty, err
	}
	for _, entry := range entries {
		if entry.Distribution == "mysql-community" && entry.Platform == platform && sameRelease(entry.ServerVersion, server.Version) && sameRelease(entry.ToolVersion, tool.Version) {
			return entry, nil
		}
	}
	return empty, preflightFailure("compatibility", "version_unsupported", "The server, XtraBackup and platform combination is outside the pinned compatibility matrix.")
}

func sameRelease(left, right string) bool {
	l, err := version.NewVersion(left)
	if err != nil {
		return false
	}
	r, err := version.NewVersion(right)
	return err == nil && l.Equal(r)
}

// CheckPrepareVersion requires admitted tool releases and the same major/minor
// series. It does not execute prepare or claim a backup is recoverable.
func CheckPrepareVersion(backupVersion, prepareVersion string) error {
	if !toolReleasePattern.MatchString(backupVersion) || !toolReleasePattern.MatchString(prepareVersion) {
		return preflightFailure("prepare", "version_invalid", "Backup and prepare require complete XtraBackup release versions.")
	}
	backup, err := version.NewVersion(backupVersion)
	if err != nil {
		return preflightFailure("prepare", "version_invalid", "The recorded backup tool version is invalid.")
	}
	prepare, err := version.NewVersion(prepareVersion)
	if err != nil {
		return preflightFailure("prepare", "version_invalid", "The prepare tool version is invalid.")
	}
	b, p := backup.Segments(), prepare.Segments()
	if b[0] != p[0] || b[1] != p[1] {
		return preflightFailure("prepare", "prepare_version_mismatch", "Prepare must use the same XtraBackup major and minor version as the backup.")
	}
	return checkPrepareReleases(backupVersion, prepareVersion)
}

func checkPrepareReleases(backup, prepare string) error {
	entries, err := compatibilityEntries()
	if err != nil {
		return err
	}
	var backupFound, prepareFound bool
	for _, entry := range entries {
		backupFound = backupFound || sameRelease(entry.ToolVersion, backup)
		prepareFound = prepareFound || sameRelease(entry.ToolVersion, prepare)
	}
	if !backupFound || !prepareFound {
		return preflightFailure("prepare", "version_unsupported", "Backup and prepare tool releases must both appear in the pinned compatibility matrix.")
	}
	return nil
}
