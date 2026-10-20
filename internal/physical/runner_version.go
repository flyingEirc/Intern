package physical

import (
	"context"
	"errors"
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

// ProbeVersion invokes an explicit binary with --version and a bounded watchdog.
func ProbeVersion(ctx context.Context, binary string) (ToolVersion, error) {
	if binary == "" {
		return ToolVersion{}, errors.New("physical: xtrabackup binary is required")
	}
	ctx, cancel := toolContext(ctx, 10*time.Second)
	defer cancel()
	var output boundedOutput
	err := runProcess(ctx, processSpec{binary: binary, args: []string{"--version"}, stdout: &output, stderr: &output})
	if err != nil {
		return ToolVersion{}, err
	}
	if output.truncated {
		return ToolVersion{}, errors.New("physical: version output exceeded diagnostic limit")
	}
	return ParseToolVersion(string(output.data))
}
