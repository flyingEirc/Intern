package physical

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	toolVersionPattern   = regexp.MustCompile(`(?m)^(?:[^\r\n]*/)?xtrabackup version ([0-9]+\.[0-9]+\.[0-9]+-[0-9]+)(?:[ \t]|$)`)
	nativeVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9]+)?$`)
	serverVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9._-]+)?$`)
)

// ParseToolVersion accepts exactly one unambiguous native version banner.
func ParseToolVersion(raw string) (ToolVersion, error) {
	if len(raw) > diagnosticLimit || strings.IndexByte(raw, 0) >= 0 {
		return ToolVersion{}, errors.New("physical: invalid version output encoding or size")
	}
	matches := toolVersionPattern.FindAllStringSubmatch(raw, 2)
	if len(matches) != 1 {
		return ToolVersion{}, errors.New("physical: unsupported or ambiguous xtrabackup version output")
	}
	return ToolVersion{Version: matches[0][1], Raw: raw}, nil
}

// ProbeVersion runs the configured xtrabackup with isolated defaults.
// The binary base name must be xtrabackup, and the banner must be that executable's own version line.
func ProbeVersion(ctx context.Context, binary string) (ToolVersion, error) {
	if err := validateXtrabackupBinary(binary); err != nil {
		return ToolVersion{}, err
	}
	ctx, cancel := toolContext(ctx, 10*time.Second)
	defer cancel()
	var output boundedOutput
	err := runProcess(ctx, processSpec{
		binary: binary, args: []string{"--no-defaults", "--no-login-paths", "--version"}, stdout: &output, stderr: &output,
	})
	if err != nil {
		return ToolVersion{}, err
	}
	if output.truncated {
		return ToolVersion{}, errors.New("physical: version output exceeded diagnostic limit")
	}
	raw := string(output.data)
	version, err := ParseToolVersion(raw)
	if err != nil {
		return ToolVersion{}, err
	}
	if !versionBannerAnchored(raw, binary) {
		return ToolVersion{}, errors.New("physical: unsupported or ambiguous xtrabackup version output")
	}
	return version, nil
}

func validateXtrabackupBinary(binary string) error {
	if filepath.Base(binary) != "xtrabackup" || strings.ContainsAny(binary, "\x00\r\n") {
		return errors.New("physical: xtrabackup binary is required")
	}
	return nil
}

func versionBannerAnchored(raw, binary string) bool {
	prefix := binary + " version "
	found := false
	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, prefix) {
			if found {
				return false
			}
			found = true
		}
	}
	return found
}
