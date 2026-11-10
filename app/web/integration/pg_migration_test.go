//go:build integration

package integration

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	tc "github.com/testcontainers/testcontainers-go"
)

const pgMigrationLegacySource = "06c34299f03becdaf0fdcdffff5fbd4319bedafb"

// buildMigrationPrograms keeps the upgrade source independent of the candidate:
// the retired production program creates its own SQLite schema and payloads.
func buildMigrationPrograms(t *testing.T, ctx context.Context, root string) (string, string) {
	t.Helper()
	archive := exec.CommandContext(ctx, "git", "archive", pgMigrationLegacySource)
	archive.Dir = root
	data, err := archive.Output()
	require(t, err)
	legacyRoot := t.TempDir()
	reader := tar.NewReader(bytes.NewReader(data))
	for {
		header, nextErr := reader.Next()
		if nextErr == io.EOF {
			break
		}
		require(t, nextErr)
		if header.Typeflag != tar.TypeReg {
			continue
		}
		name := filepath.Join(legacyRoot, filepath.FromSlash(header.Name))
		if !strings.HasPrefix(name, legacyRoot+string(os.PathSeparator)) {
			t.Fatal("legacy archive escaped its private build tree")
		}
		require(t, os.MkdirAll(filepath.Dir(name), 0o700))
		body, readErr := io.ReadAll(reader)
		require(t, readErr)
		require(t, os.WriteFile(name, body, 0o600))
	}
	build := func(source, output string) string {
		command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-buildvcs=false", "-o", output, "./app/web")
		command.Dir = source
		command.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
		if text, buildErr := command.CombinedOutput(); buildErr != nil {
			t.Fatalf("build migration production program: %v\n%s", buildErr, text)
		}
		return output
	}
	return build(legacyRoot, filepath.Join(t.TempDir(), "legacy-web")), build(root, filepath.Join(t.TempDir(), "candidate-web"))
}

func copyMigrationPrograms(t *testing.T, ctx context.Context, worker tc.Container, legacy, candidate string) {
	t.Helper()
	for _, item := range []struct{ source, target string }{{legacy, "/tmp/legacy-web"}, {candidate, "/tmp/candidate-web"}} {
		require(t, worker.CopyFileToContainer(ctx, item.source, item.target, 0o755))
		data, err := os.ReadFile(item.source)
		require(t, err)
		digest := fmt.Sprintf("%x", sha256.Sum256(data))
		actual := strings.Fields(execOutput(t, ctx, worker, "", []string{"sha256sum", item.target}))
		if len(actual) != 2 || actual[0] != digest {
			t.Fatal("migration production binary differs from build output")
		}
		t.Logf("PG-G3 binary path=%s sha256=%s legacy_source=%s", item.target, digest, pgMigrationLegacySource)
	}
}

var pgMigrationRestorePattern = regexp.MustCompile(`PG-G3 export point=(full|inc1|inc2|continued) id=([0-9a-f]{32}) snapshot=([0-9a-f]{64})`)

func verifyMigrationExports(t *testing.T, ctx context.Context, text, mysqlImage, rootPassword, volume string) {
	t.Helper()
	for _, marker := range []string{
		"PG-G3 legacy source=" + pgMigrationLegacySource + " schema=web:9,physical:1,replication:2",
		"PG-G3 import command=production tables=18 equal=true source_intact=true repeat=refused nonempty=refused rollback=empty",
		"PG-G3 continuity login=original setup=closed history=visible chain=1,2,3,4 lsn=continuous replicas=4 cleanup=recovered",
	} {
		if strings.Count(text, marker) != 1 {
			t.Fatalf("missing or repeated migration evidence %q", marker)
		}
	}
	matches := pgMigrationRestorePattern.FindAllStringSubmatch(text, -1)
	seen := make(map[string]bool)
	for _, match := range matches {
		if seen[match[1]] {
			t.Fatalf("repeated migration restore point %s", match[1])
		}
		seen[match[1]] = true
		fresh := startP4RestoredMySQL(t, ctx, mysqlImage, rootPassword, volume, "migration/"+match[2])
		db := openFixtureDB(t, ctx, fresh, "root", rootPassword)
		actual := p4Snapshot(t, ctx, db)
		require(t, db.Close())
		if actual != match[3] {
			t.Fatalf("migration restore %s snapshot=%s want=%s", match[1], actual, match[3])
		}
		t.Logf("PG-G3 restored point=%s actual_snapshot=%s expected_snapshot=%s match=true", match[1], actual, match[3])
	}
	if len(seen) != 4 {
		t.Fatalf("migration fresh MySQL restore points=%d want=4", len(seen))
	}
}
