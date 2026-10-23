//go:build integration && linux

package acceptance

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/flyingEirc/Intern/internal/physical/chain"
	"github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

func TestP3DiskWorker(t *testing.T) {
	fixture := newP3Fixture(t)
	mount := os.Getenv("INTERN_P3_FULL_DISK")
	if mount != "/p3-full" {
		t.Fatal("real size-limited tmpfs mount is required")
	}
	var stat syscall.Statfs_t
	must(t, syscall.Statfs(mount, &stat))
	if stat.Type != 0x01021994 || stat.Blocks*uint64(stat.Bsize) > 1<<20 {
		t.Fatal("disk fixture is not a <=1 MiB tmpfs")
	}
	root, err := os.MkdirTemp(mount, "repo-")
	must(t, err)
	t.Cleanup(func() { must(t, os.RemoveAll(root)) })
	repo, err := local.Open(root)
	must(t, err)
	t.Cleanup(func() { must(t, repo.Close()) })
	// Consume the actual mount, leaving room for the private directory and an
	// initial payload fragment but not a full authenticated stream segment.
	fill := filepath.Join(mount, "fill")
	must(t, os.WriteFile(fill, bytes.Repeat([]byte{'x'}, (1<<20)-(16<<10)), 0o600))
	t.Cleanup(func() { must(t, os.Remove(fill)) })
	job := fixture.job
	job.Repo, job.SourceID = repo, "full-disk"
	result, runErr := chain.Run(t.Context(), job)
	if !errors.Is(runErr, syscall.ENOSPC) {
		t.Fatalf("real full disk error=%v, want ENOSPC", runErr)
	}
	backup, err := job.Store.Backup(t.Context(), result.BackupID)
	must(t, err)
	if backup.Status != "failed" || backup.LocalPresent {
		t.Fatal("full disk produced a usable backup")
	}
	_, err = job.Store.Plan(t.Context(), store.PlanRequest{SourceID: job.SourceID, ServerUUID: job.Identity.UUID, ServerVersion: job.Identity.Version, ToolVersion: job.ToolVersion, Kind: "incremental", MaxChainLength: 8})
	assertP3Admission(t, err, "parent_not_completed")
	assertP3Ledger(t, fixture.db, backup.ID, 0)
	assertWorkGone(t, job.WorkRoot)
	entries, err := os.ReadDir(root)
	must(t, err)
	if len(entries) != 0 {
		t.Fatal("full disk left a staging or published object")
	}
	t.Log("P3-A2 fault=enospc filesystem=tmpfs completed=false parent=refused ledger=empty pending=gone work=gone")
}
