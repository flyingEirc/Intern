package physical

import (
	"strings"
	"testing"
)

// realVersionBanner is the exact line Percona XtraBackup 8.4.0-7 prints for
// --version: on stderr, and naming the path it was invoked as.
const realVersionBanner = "/usr/bin/xtrabackup version 8.4.0-7 based on MySQL server 8.4.0 Linux (x86_64) (revision id: 4093cfd8)\n"

// TestParseToolVersionAcceptsRealBanner pins the positive half of the probe's
// anchor rule against the observed tool output, so a change to the anchor cannot
// silently start refusing the pinned image.
func TestParseToolVersionAcceptsRealBanner(t *testing.T) {
	version, err := ParseToolVersion(realVersionBanner)
	if err != nil {
		t.Fatalf("real banner: %v", err)
	}
	if version.Version != "8.4.0-7" {
		t.Fatalf("version = %q, want 8.4.0-7", version.Version)
	}
	if !versionBannerAnchored(realVersionBanner, "/usr/bin/xtrabackup") {
		t.Fatal("the real banner is not anchored to the configured binary")
	}
	// The anchor is the point of the probe: a banner produced by a different
	// binary than the configured one, including a wrapper that forwards to it
	// without rewriting stderr, must be refused.
	if versionBannerAnchored(realVersionBanner, "/tmp/restore/bin/xtrabackup") {
		t.Fatal("a banner from another path was anchored")
	}
	bare := strings.ReplaceAll(realVersionBanner, "/usr/bin/", "")
	if _, err = ParseToolVersion(bare); err != nil {
		t.Fatalf("bare banner: %v", err)
	}
	if versionBannerAnchored(bare, "/usr/bin/xtrabackup") {
		t.Fatal("a bare-name banner was anchored to an absolute path")
	}
}

// TestVersionBannerAnchoredIgnoresDiagnostics proves the tool's other output
// never shifts the anchor: only a line that starts with the configured path and
// " version " counts, and a second such line is ambiguous.
func TestVersionBannerAnchoredIgnoresDiagnostics(t *testing.T) {
	diagnostic := "2026-01-01T00:00:00.000000-00:00 0 [Note] [MY-011825] [Xtrabackup] recognized server arguments: --datadir=/var/lib/mysql \n"
	if !versionBannerAnchored(diagnostic+realVersionBanner, "/usr/bin/xtrabackup") {
		t.Fatal("a logged diagnostic before the banner broke the anchor")
	}
	if versionBannerAnchored(realVersionBanner+realVersionBanner, "/usr/bin/xtrabackup") {
		t.Fatal("two anchored banners were accepted")
	}
	if versionBannerAnchored(realVersionBanner, "/usr/bin/xtrabackup\n/usr/bin/other") {
		t.Fatal("a control character in the configured path was anchored")
	}
}
