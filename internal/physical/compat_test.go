package physical

import (
	"testing"
)

func TestCompatibilityPinnedCandidate(t *testing.T) {
	identity := Identity{Version: "8.4.11", VersionComment: "MySQL Community Server - GPL"}
	tool := ToolVersion{Version: "8.4.0-7"}
	entry, err := matchingCompatibility(identity, tool, "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Verified || entry.Status != "candidate" || entry.EvidenceIssue == "" {
		t.Fatalf("candidate must not claim verification: %+v", entry)
	}
	matrix, err := CompatibilityMatrix()
	if err != nil {
		t.Fatal(err)
	}
	matrix[0].ToolVersion = "9.0.0-1"
	if err := CheckCompatibility(identity, tool, "linux/amd64"); err != nil {
		t.Fatalf("caller changed embedded admission: %v", err)
	}
}

func TestCompatibilityRejectsOutsideMatrix(t *testing.T) {
	for _, test := range []struct {
		name, server, tool, comment, platform string
	}{
		{"server_patch", "8.4.10", "8.4.0-7", "MySQL Community Server - GPL", "linux/amd64"},
		{"tool_patch", "8.4.11", "8.4.0-6", "MySQL Community Server - GPL", "linux/amd64"},
		{"tool_future", "8.4.11", "8.4.0-8", "MySQL Community Server - GPL", "linux/amd64"},
		{"mariadb", "8.4.11", "8.4.0-7", "MariaDB Server", "linux/amd64"},
		{"percona", "8.4.11", "8.4.0-7", "Percona Server (GPL)", "linux/amd64"},
		{"macos", "8.4.11", "8.4.0-7", "MySQL Community Server - GPL", "darwin/amd64"},
		{"arm64", "8.4.11", "8.4.0-7", "MySQL Community Server - GPL", "linux/arm64"},
		{"prerelease", "8.4.11-rc1", "8.4.0-7", "MySQL Community Server - GPL", "linux/amd64"},
		{"build_metadata", "8.4.11", "8.4.0-7+unverified", "MySQL Community Server - GPL", "linux/amd64"},
		{"leading_zero", "08.4.11", "8.4.0-7", "MySQL Community Server - GPL", "linux/amd64"},
		{"missing_patch", "8.4", "8.4.0-7", "MySQL Community Server - GPL", "linux/amd64"},
		{"prefix_collision", "8.4.110", "8.4.0-7", "MySQL Community Server - GPL", "linux/amd64"},
		{"empty", "", "", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := CheckCompatibility(Identity{Version: test.server, VersionComment: test.comment}, ToolVersion{Version: test.tool}, test.platform)
			if err == nil {
				t.Fatal("out-of-matrix combination admitted")
			}
		})
	}
}

func TestPrepareAdmission(t *testing.T) {
	if err := CheckPrepareVersion("8.4.0-7", "8.4.0-7"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ backup, prepare, code string }{
		{"8.4.0-7", "8.0.35-30", "prepare_version_mismatch"},
		{"8.4.0-7", "9.4.0-1", "prepare_version_mismatch"},
		{"8.4.0-6", "8.4.0-7", "version_unsupported"},
		{"8.4.0-7", "8.4.0-8", "version_unsupported"},
		{"8.4", "8.4.0-7", "version_invalid"},
		{"8.4.0-7", "v8.4.0-7", "version_invalid"},
		{"8.4.0-7", "8.4.0-7+custom", "version_invalid"},
		{"", "", "version_invalid"},
	} {
		t.Run(test.backup+"_"+test.prepare, func(t *testing.T) {
			requirePreflightCode(t, CheckPrepareVersion(test.backup, test.prepare), test.code)
		})
	}
}
