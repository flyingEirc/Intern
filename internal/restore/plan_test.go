package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/physical"
)

func requirePlanCode(t *testing.T, err error, code string) {
	t.Helper()
	var refusal *PlanError
	if !errors.As(err, &refusal) {
		t.Fatalf("error = %v, want a PlanError with code %s", err, code)
	}
	if refusal.Code != code {
		t.Fatalf("plan code = %q, want %q", refusal.Code, code)
	}
}

func TestBuildPlanLocalPreferred(t *testing.T) {
	fixture := newPlanFixture(t)
	target := fixture.backup(2)
	plan, err := BuildPlan(t.Context(), fixture.options(), target.ID)
	mustNoError(t, err)
	assertPlanIdentity(t, plan, target)
	assertPlanLocalLinks(t, fixture, plan)
	assertPlanSpace(t, fixture, plan)
	if !contains(plan.Risks, RiskNoAutoReplaceProduction) || !contains(plan.Risks, RiskServerUUIDDuplicate) ||
		!contains(plan.Risks, RiskPITRUnavailable) || !contains(plan.Risks, RiskRehearsalUnavailable) {
		t.Fatalf("plan risks = %v", plan.Risks)
	}
	if !strings.Contains(plan.ExportPath, "{restore_id}") {
		t.Fatal("the plan export path must keep the restore-id placeholder")
	}
	if len(plan.Digest) != 64 {
		t.Fatal("plan digest is not a sha256 hex string")
	}
}

func assertPlanIdentity(t *testing.T, plan Plan, target fixtureBackup) {
	t.Helper()
	if plan.TargetBackupID != target.ID || plan.ChainID != fixtureChain || plan.SourceID != fixtureSource {
		t.Fatal("plan identity differs from the fixture")
	}
	if plan.ServerVersion != "8.4.11" || plan.ToolVersion != "8.4.0-7" || plan.TargetToLSN != inc2LSN {
		t.Fatal("plan versions or target LSN differ from the chain")
	}
}

func assertPlanLocalLinks(t *testing.T, fixture *planFixture, plan Plan) {
	t.Helper()
	if len(plan.Links) != 3 {
		t.Fatalf("links = %d, want the full chain", len(plan.Links))
	}
	for index, link := range plan.Links {
		if link.Source != "local" || link.Seq != index+1 || link.BackupID != fixture.backup(index).ID {
			t.Fatalf("link %d is not the local chain member", index)
		}
		if index > 0 && link.FromLSN != plan.Links[index-1].ToLSN {
			t.Fatal("plan links are not LSN-continuous")
		}
	}
}

func assertPlanSpace(t *testing.T, fixture *planFixture, plan Plan) {
	t.Helper()
	var work, export int64
	for _, link := range plan.Links {
		work += link.PayloadSize * (2 + 8)
		export += link.PayloadSize * 8
	}
	if plan.RequiredWorkBytes != work || plan.RequiredExportBytes != export {
		t.Fatalf("space estimate = %d/%d, want %d/%d", plan.RequiredWorkBytes, plan.RequiredExportBytes, work, export)
	}
	workSpace, err := physical.QuerySpace(fixture.workRoot)
	mustNoError(t, err)
	exportSpace, err := physical.QuerySpace(fixture.exportRoot)
	mustNoError(t, err)
	if !closeCapacity(plan.AvailableWorkBytes, workSpace.AvailableBytes) || !closeCapacity(plan.AvailableExportBytes, exportSpace.AvailableBytes) {
		t.Fatalf("plan capacity %d/%d differs from a fresh Statfs %d/%d",
			plan.AvailableWorkBytes, plan.AvailableExportBytes, workSpace.AvailableBytes, exportSpace.AvailableBytes)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// closeCapacity compares two observations of the same filesystem; other writers
// can change free space between them.
func closeCapacity(planned, observed int64) bool {
	difference := planned - observed
	if difference < 0 {
		difference = -difference
	}
	return difference <= max(int64(1<<20), observed/100)
}

func TestBuildPlanFTPFallback(t *testing.T) {
	fixture := newPlanFixture(t)
	mustNoError(t, os.Remove(filepath.Join(fixture.repoRoot, filepath.FromSlash(fixture.backup(1).ObjectPath), memberPayload)))
	plan, err := BuildPlan(t.Context(), fixture.options(), fixture.backup(2).ID)
	mustNoError(t, err)
	if sources := []string{plan.Links[0].Source, plan.Links[1].Source, plan.Links[2].Source}; strings.Join(sources, ",") != "local,ftp,local" {
		t.Fatalf("per-link sources = %v, want local, ftp, local", sources)
	}
	if plan.Links[1].RepositoryID != fixtureRepoID {
		t.Fatal("the FTP link does not name its repository")
	}
	if fixture.remote.opens != 0 {
		t.Fatal("the plan read remote bytes instead of recording sizes")
	}
}

func TestBuildPlanFTPWhenAllLocalPayloadsAreReclaimed(t *testing.T) {
	fixture := newPlanFixture(t)
	for _, backup := range fixture.backups {
		mustNoError(t, os.RemoveAll(filepath.Join(fixture.repoRoot, filepath.FromSlash(backup.ObjectPath))))
	}
	plan, err := BuildPlan(t.Context(), fixture.options(), fixture.backup(2).ID)
	mustNoError(t, err)
	for _, link := range plan.Links {
		if link.Source != "ftp" || link.RepositoryID != fixtureRepoID {
			t.Fatal("a fully reclaimed local object did not fall back to its verified replica")
		}
	}
}

func TestBuildPlanReplicaNotVerified(t *testing.T) {
	fixture := newPlanFixture(t)
	mustNoError(t, os.RemoveAll(filepath.Join(fixture.repoRoot, filepath.FromSlash(fixture.backup(1).ObjectPath))))
	fixture.exec("UPDATE replicas SET status = 'uploaded' WHERE backup_id = ? AND repository_id = ?", fixture.backup(1).ID, fixtureRepoID)
	_, err := BuildPlan(t.Context(), fixture.options(), fixture.backup(2).ID)
	requirePlanCode(t, err, CodeSourceUnavailable)
}

func TestBuildPlanRejections(t *testing.T) {
	cases := []struct {
		name   string
		code   string
		mutate func(*testing.T, *planFixture) (Options, string)
	}{
		{
			name: "unknown backup", code: CodeBackupNotFound,
			mutate: func(t *testing.T, f *planFixture) (Options, string) {
				return f.options(), "ffffffffffffffffffffffffffffffff"
			},
		},
		{
			name: "not completed", code: CodeBackupNotCompleted,
			mutate: func(t *testing.T, f *planFixture) (Options, string) {
				f.exec("UPDATE backups SET status = 'failed' WHERE id = ?", f.backup(1).ID)
				return f.options(), f.backup(1).ID
			},
		},
		{
			name: "missing middle link", code: CodeChainBroken,
			mutate: func(t *testing.T, f *planFixture) (Options, string) {
				f.exec("UPDATE backups SET seq = 9 WHERE id = ?", f.backup(1).ID)
				return f.options(), f.backup(2).ID
			},
		},
		{
			name: "wrong parent", code: CodeChainBroken,
			mutate: func(t *testing.T, f *planFixture) (Options, string) {
				f.exec("UPDATE backups SET parent_id = ? WHERE id = ?", f.backup(0).ID, f.backup(2).ID)
				return f.options(), f.backup(2).ID
			},
		},
		{
			name: "discontinuous lsn", code: CodeLSNDiscontinuous,
			mutate: func(t *testing.T, f *planFixture) (Options, string) {
				f.exec("UPDATE backups SET from_lsn = '999' WHERE id = ?", f.backup(1).ID)
				return f.options(), f.backup(2).ID
			},
		},
		{
			name: "wrong kek", code: CodeKeyUnavailable,
			mutate: func(t *testing.T, f *planFixture) (Options, string) {
				opts := f.options()
				opts.KeyFile = f.wrongKey
				return opts, f.backup(2).ID
			},
		},
		{
			name: "prepare version mismatch", code: CodeToolVersionMismatch,
			mutate: func(t *testing.T, f *planFixture) (Options, string) {
				opts := f.options()
				opts.probeTool = func(context.Context, string) (physical.ToolVersion, error) {
					return physical.ToolVersion{Version: "8.4.0-8"}, nil
				}
				return opts, f.backup(2).ID
			},
		},
		{
			name: "outside matrix", code: CodeMatrixUnsupported,
			mutate: func(t *testing.T, f *planFixture) (Options, string) {
				f.exec("UPDATE chains SET server_version = '8.4.12' WHERE id = ?", fixtureChain)
				return f.options(), f.backup(2).ID
			},
		},
		{
			name: "insufficient space", code: CodeInsufficientSpace,
			mutate: func(t *testing.T, f *planFixture) (Options, string) {
				space, err := physical.QuerySpace(f.workRoot)
				mustNoError(t, err)
				opts := f.options()
				opts.ExpansionFactor = int(space.AvailableBytes/int64(f.backup(0).PayloadSize)) + 100
				return opts, f.backup(2).ID
			},
		},
		{
			name: "source unavailable", code: CodeSourceUnavailable,
			mutate: func(t *testing.T, f *planFixture) (Options, string) {
				for _, backup := range f.backups {
					mustNoError(t, os.RemoveAll(filepath.Join(f.repoRoot, filepath.FromSlash(backup.ObjectPath))))
					f.exec("UPDATE replicas SET status = 'failed' WHERE backup_id = ?", backup.ID)
				}
				return f.options(), f.backup(2).ID
			},
		},
		{
			name: "unconfigured", code: CodeUnconfigured,
			mutate: func(t *testing.T, f *planFixture) (Options, string) {
				opts := f.options()
				opts.Locator = nil
				return opts, f.backup(2).ID
			},
		},
		{
			name: "export root inside work root", code: CodeUnconfigured,
			mutate: func(t *testing.T, f *planFixture) (Options, string) {
				inside := filepath.Join(f.workRoot, "export")
				mustNoError(t, os.Mkdir(inside, 0o700))
				opts := f.options()
				opts.ExportRoot = inside
				return opts, f.backup(2).ID
			},
		},
		{
			name: "export root is a symlink", code: CodeUnconfigured,
			mutate: func(t *testing.T, f *planFixture) (Options, string) {
				link := filepath.Join(t.TempDir(), "export-link")
				mustNoError(t, os.Symlink(f.exportRoot, link))
				opts := f.options()
				opts.ExportRoot = link
				return opts, f.backup(2).ID
			},
		},
		{
			name: "export root inside a restricted root", code: CodeUnconfigured,
			mutate: func(t *testing.T, f *planFixture) (Options, string) {
				metadata := filepath.Dir(f.exportRoot)
				opts := f.options()
				opts.RestrictedRoots = []string{metadata}
				return opts, f.backup(2).ID
			},
		},
		{
			name: "export root contains the kek", code: CodeKeyUnavailable,
			mutate: func(t *testing.T, f *planFixture) (Options, string) {
				key := filepath.Join(f.exportRoot, "kek.json")
				mustNoError(t, artifact.GenerateKey(key))
				opts := f.options()
				opts.KeyFile = key
				return opts, f.backup(2).ID
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPlanFixture(t)
			opts, backupID := test.mutate(t, fixture)
			plan, err := BuildPlan(t.Context(), opts, backupID)
			if err == nil {
				t.Fatalf("plan unexpectedly succeeded: %+v", plan)
			}
			requirePlanCode(t, err, test.code)
			assertPlanWroteNothing(t, fixture)
		})
	}
}

// assertPlanWroteNothing proves the read-only plan contract: no work directory,
// no export entry, no repository change and no prepare process.
func assertPlanWroteNothing(t *testing.T, fixture *planFixture) {
	t.Helper()
	entries, err := os.ReadDir(fixture.workRoot)
	mustNoError(t, err)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "restore-") {
			t.Fatal("plan created a restore work directory")
		}
	}
	entries, err = os.ReadDir(fixture.exportRoot)
	mustNoError(t, err)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pending-") {
			t.Fatal("plan created a pending export")
		}
	}
	if fixture.remote.opens != 0 {
		t.Fatal("plan opened remote content")
	}
}

func TestBuildPlanDigest(t *testing.T) {
	fixture := newPlanFixture(t)
	opts := fixture.options()
	first, err := BuildPlan(t.Context(), opts, fixture.backup(2).ID)
	mustNoError(t, err)
	second, err := BuildPlan(t.Context(), opts, fixture.backup(2).ID)
	mustNoError(t, err)
	if first.Digest != second.Digest {
		t.Fatal("identical plans produced different digests")
	}
	other := opts
	other.ExpansionFactor = 16
	wider, err := BuildPlan(t.Context(), other, fixture.backup(2).ID)
	mustNoError(t, err)
	if wider.Digest != first.Digest {
		t.Fatal("free-space-dependent numbers changed the digest")
	}
	if wider.RequiredWorkBytes == first.RequiredWorkBytes {
		t.Fatal("the expansion factor did not change the estimate")
	}
	mustNoError(t, os.RemoveAll(filepath.Join(fixture.repoRoot, filepath.FromSlash(fixture.backup(2).ObjectPath))))
	remotePlan, err := BuildPlan(t.Context(), opts, fixture.backup(2).ID)
	mustNoError(t, err)
	if remotePlan.Links[2].Source != "ftp" {
		t.Fatal("the reclaimed link did not fall back to FTP")
	}
	if remotePlan.Digest == first.Digest {
		t.Fatal("a different per-link source did not change the digest")
	}
}

func TestBuildPlanReadOnly(t *testing.T) {
	fixture := newPlanFixture(t)
	dbPath := filepath.Join(filepath.Dir(fixture.workRoot), "intern.db")
	beforeDB := fileDigestForTest(t, dbPath)
	beforeRepo := directoryDigest(t, fixture.repoRoot)
	plan, err := BuildPlan(t.Context(), fixture.options(), fixture.backup(2).ID)
	mustNoError(t, err)
	if plan.Digest == "" {
		t.Fatal("plan has no digest")
	}
	if after := fileDigestForTest(t, dbPath); after != beforeDB {
		t.Fatal("plan changed the control-plane database")
	}
	if after := directoryDigest(t, fixture.repoRoot); after != beforeRepo {
		t.Fatal("plan changed the local repository")
	}
	assertPlanWroteNothing(t, fixture)
}

func fileDigestForTest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	mustNoError(t, err)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func directoryDigest(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	mustNoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		digest := ""
		if entry.Type().IsRegular() {
			digest = fileDigestForTest(t, path)
		}
		lines = append(lines, path+"|"+info.Mode().String()+"|"+digest)
		return nil
	}))
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// TestBuildPlanProbeFailureIsUnconfigured keeps "the configured tool cannot be
// probed" apart from "the tool disagrees with the stored chain": only the latter
// is tool_version_mismatch, so a refused plan never hides a real matrix, source
// or space defect behind a version code.
func TestBuildPlanProbeFailureIsUnconfigured(t *testing.T) {
	fixture := newPlanFixture(t)
	opts := fixture.options()
	opts.probeTool = func(context.Context, string) (physical.ToolVersion, error) {
		return physical.ToolVersion{}, errors.New("physical: unsupported or ambiguous xtrabackup version output")
	}
	_, err := BuildPlan(t.Context(), opts, fixture.backup(2).ID)
	requirePlanCode(t, err, CodeUnconfigured)
}

// TestBuildPlanRefusesTheMoreSpecificConfigurationFault first proves that a
// version disagreement is still reported when the tool answers, then that an
// unusable export root wins over every tool condition: one request with two
// defects must report the configuration fault, never a version mismatch.
func TestBuildPlanRefusesTheMoreSpecificConfigurationFault(t *testing.T) {
	fixture := newPlanFixture(t)
	mismatch := fixture.options()
	mismatch.probeTool = func(context.Context, string) (physical.ToolVersion, error) {
		return physical.ToolVersion{Version: "8.4.0-8"}, nil
	}
	fixture.exec("UPDATE chains SET server_version = '8.4.12' WHERE id = ?", fixtureChain)
	_, err := BuildPlan(t.Context(), mismatch, fixture.backup(2).ID)
	requirePlanCode(t, err, CodeToolVersionMismatch)

	link := filepath.Join(t.TempDir(), "export-link")
	mustNoError(t, os.Symlink(fixture.exportRoot, link))
	mismatch.ExportRoot = link
	_, err = BuildPlan(t.Context(), mismatch, fixture.backup(2).ID)
	requirePlanCode(t, err, CodeUnconfigured)
}

// TestBuildPlanRejectsReorderedSequence covers a chain whose stored sequence is
// permuted: the plan must refuse instead of slicing a wrong link set.
func TestBuildPlanRejectsReorderedSequence(t *testing.T) {
	fixture := newPlanFixture(t)
	// Permuted sequence: the incremental-2 row now sits at seq 2 and the
	// incremental-1 row at seq 3, so the sorted rows no longer follow parentage.
	fixture.exec("UPDATE backups SET seq = 9 WHERE id = ?", fixture.backup(1).ID)
	fixture.exec("UPDATE backups SET seq = 2 WHERE id = ?", fixture.backup(2).ID)
	fixture.exec("UPDATE backups SET seq = 3 WHERE id = ?", fixture.backup(1).ID)
	_, err := BuildPlan(t.Context(), fixture.options(), fixture.backup(1).ID)
	requirePlanCode(t, err, CodeChainBroken)
	_, err = BuildPlan(t.Context(), fixture.options(), fixture.backup(2).ID)
	requirePlanCode(t, err, CodeChainBroken)
}

// TestExpandLimitIsTheVerifiedWorkSpace pins the artifact read bound: it follows
// the free space the plan measured in the private work root, because a real
// backup stream expands by far more than the recorded ciphertext multiple, and
// it keeps a floor so the small tool metadata artifact stays readable.
func TestExpandLimitIsTheVerifiedWorkSpace(t *testing.T) {
	const floor = int64(64 << 20)
	for _, test := range []struct{ available, want int64 }{
		{available: 0, want: floor},
		{available: floor, want: floor},
		{available: floor - 1, want: floor},
		{available: 3 * floor, want: 3 * floor},
	} {
		execution := &execution{plan: Plan{AvailableWorkBytes: test.available, Links: []PlanLink{{PayloadSize: 1}}}}
		if got := execution.expandLimit(); got != test.want {
			t.Errorf("available %d expand limit = %d, want %d", test.available, got, test.want)
		}
	}
}
