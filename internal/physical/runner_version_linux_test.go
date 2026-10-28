package physical

import (
	"context"
	"testing"
)

// TestProbeAcceptsAnchoredStderrBanner proves the positive half of the probe with
// a real process: Percona XtraBackup answers --version on stderr with the path it
// was invoked as, and that is exactly what the configured binary must produce.
func TestProbeAcceptsAnchoredStderrBanner(t *testing.T) {
	binary := fakeNamedTool(t, "xtrabackup",
		`printf '%s version 8.4.0-7 based on MySQL server 8.4.0 Linux (x86_64) (revision id: 4093cfd8)\n' "$0" >&2`)
	version, err := ProbeVersion(context.Background(), binary)
	if err != nil {
		t.Fatalf("probe the pinned banner: %v", err)
	}
	if version.Version != "8.4.0-7" {
		t.Fatalf("version = %q, want 8.4.0-7", version.Version)
	}
	if !versionBannerAnchored(version.Raw, binary) {
		t.Fatalf("the probed output is not anchored to %s: %q", binary, version.Raw)
	}
}

// TestProbeRefusesForwardedBannerIsUnanchored covers the wrapper shape: a script
// that runs the real tool but leaves its banner on the tool's own stderr reports
// another binary's path, which the probe must refuse instead of accepting.
func TestProbeRefusesForwardedBannerIsUnanchored(t *testing.T) {
	binary := fakeNamedTool(t, "xtrabackup", `printf '`+realVersionBanner+`' >&2`)
	if _, err := ProbeVersion(context.Background(), binary); err == nil {
		t.Fatal("a banner naming another binary was accepted")
	}
}
