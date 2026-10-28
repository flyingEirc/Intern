package restore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/physical/store"
)

// planningKind is the vocabulary internal/physical/store hands the chain ("full",
// "incremental"), while the backups row it plans is written with the catalog
// prefix "physical-" + that value.
func planningKind(recordKind string) string {
	return strings.TrimPrefix(recordKind, "physical-")
}

// realChainIdentity is the identity a physical backup writes into its tool
// manifest: the planning kind, never the catalog kind of the row.
func realChainIdentity(backup fixtureBackup, role string) identityManifest {
	return identityManifest{
		Version: 1, Role: role, InstanceID: fixtureInstance, SourceID: fixtureSource,
		BackupID: backup.ID, ChainID: fixtureChain, Seq: backup.Seq,
		Kind: planningKind(backup.Kind), ParentID: backup.ParentID,
		ServerUUID: "8f0f5b3f-3b57-4b0f-9f39-0a3f6a1b2c3d", ServerVersion: "8.4.11", ToolVersion: "8.4.0-7",
	}
}

func digestHex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// writeArtifact compresses and encrypts one payload with the KEK and stores the
// two members of the artifact under the given names, exactly as the chain writes
// a backup and its tool metadata.
func writeArtifact(t *testing.T, dir string, payload, manifest []byte, member, envelope, keyFile string) {
	t.Helper()
	staging := t.TempDir()
	mustNoError(t, artifact.Write(t.Context(), staging, bytes.NewReader(payload), manifest,
		artifact.Options{Compress: true, Encrypt: true, KeyFile: keyFile}))
	for _, pair := range [][2]string{{"payload.bin", member}, {"envelope.json", envelope}} {
		body, err := os.ReadFile(filepath.Join(staging, pair[0]))
		mustNoError(t, err)
		mustNoError(t, os.WriteFile(filepath.Join(dir, pair[1]), body, 0o600))
	}
}

// publishRealMembers replaces one fixture backup's four members with a real
// artifact pair and records the resulting ciphertext size and digest in the
// backups row, as the chain's completion does.
func publishRealMembers(t *testing.T, fixture *planFixture, backup fixtureBackup, payload []byte) {
	t.Helper()
	checkpoints := []byte("backup_type = full-prepared\nfrom_lsn = 0\nto_lsn = 100\nlast_lsn = 100\n")
	info := []byte("tool_name = xtrabackup\ntool_version = 8.4.0-7\n")
	members := filepath.Join(fixture.repoRoot, filepath.FromSlash(backup.ObjectPath))
	identityRaw, err := json.Marshal(realChainIdentity(backup, "xbstream"))
	mustNoError(t, err)
	writeArtifact(t, members, payload, identityRaw, memberPayload, memberEnvelope, fixture.keyFile)
	body, err := os.ReadFile(filepath.Join(members, memberPayload))
	mustNoError(t, err)
	payloadSize, payloadSum := int64(len(body)), digestHex(body)

	toolRaw, err := json.Marshal(toolManifest{
		Identity: realChainIdentity(backup, "tool-metadata"), CheckpointsRaw: checkpoints, InfoRaw: info,
		PayloadSize: payloadSize, PayloadSHA256: payloadSum,
	})
	mustNoError(t, err)
	writeArtifact(t, members, toolRaw, toolRaw, memberMetadata, memberMetaEnvelope, fixture.keyFile)
	metaBody, err := os.ReadFile(filepath.Join(members, memberMetadata))
	mustNoError(t, err)
	fixture.exec(`UPDATE backups SET payload_size = ?, payload_sha256 = ?, metadata_size = ?, metadata_sha256 = ?,
		checkpoints = ?, xtrabackup_info = ? WHERE id = ?`,
		payloadSize, payloadSum, int64(len(metaBody)), digestHex(metaBody), checkpoints, info, backup.ID)
}

// realExecution binds an execution to one published fixture backup with the given
// verified work space, and returns its link and the directory holding its
// members.
func realExecution(t *testing.T, fixture *planFixture, backup store.Backup, available int64) (*execution, PlanLink, string) {
	t.Helper()
	plan := Plan{
		ChainID: fixtureChain, SourceID: fixtureSource, ServerUUID: "8f0f5b3f-3b57-4b0f-9f39-0a3f6a1b2c3d",
		ServerVersion: "8.4.11", ToolVersion: "8.4.0-7", TargetBackupID: backup.ID, TargetToLSN: backup.ToLSN,
		AvailableWorkBytes: available,
	}
	link := PlanLink{
		BackupID: backup.ID, Seq: backup.Seq, Kind: backup.Kind, FromLSN: backup.FromLSN, ToLSN: backup.ToLSN,
		Source: "local", PayloadSize: backup.PayloadSize, MetadataSize: backup.MetadataSize, objectPath: backup.ObjectPath,
	}
	return &execution{opts: fixture.options(), plan: plan}, link,
		filepath.Join(fixture.repoRoot, filepath.FromSlash(backup.ObjectPath))
}

func storedBackup(t *testing.T, fixture *planFixture, id string) store.Backup {
	t.Helper()
	backup, err := fixture.store.Backup(t.Context(), id)
	mustNoError(t, err)
	return backup
}

// TestAuthenticateRealArtifactAcceptsChainVocabulary is the regression for the
// blocker that refused every native execute path: the tool manifest records the
// planning kind while the backups row records the catalog kind, and the plan only
// compares sizes, so a real artifact was refused at the payload step while every
// plan succeeded.
func TestAuthenticateRealArtifactAcceptsChainVocabulary(t *testing.T) {
	fixture := newPlanFixture(t)
	seed := fixture.backup(0)
	publishRealMembers(t, fixture, seed, bytes.Repeat([]byte("xbstream block "), 4000))
	backup := storedBackup(t, fixture, seed.ID)
	execution, link, dir := realExecution(t, fixture, backup, 1<<30)
	if err := execution.authenticate(t.Context(), link, dir); err != nil {
		t.Fatalf("authenticate a real artifact: %v", err)
	}
}

// TestAuthenticateRefusesForeignKind keeps the vocabulary mapping honest: a
// manifest naming a different kind than the row is still refused, and the fixed
// sub-step label says which check refused.
func TestAuthenticateRefusesForeignKind(t *testing.T) {
	fixture := newPlanFixture(t)
	seed := fixture.backup(0)
	publishRealMembers(t, fixture, seed, bytes.Repeat([]byte("payload"), 2000))
	fixture.exec(`UPDATE backups SET kind = 'physical-incremental', from_lsn = '100', to_lsn = '200' WHERE id = ?`, seed.ID)
	backup := storedBackup(t, fixture, seed.ID)
	execution, link, dir := realExecution(t, fixture, backup, 1<<30)
	err := execution.authenticate(t.Context(), link, dir)
	if err == nil {
		t.Fatal("an incremental row authenticated a full-backup manifest")
	}
	if !strings.Contains(err.Error(), "restore: authenticate payload:") {
		t.Fatalf("error = %v, want the payload sub-step label", err)
	}
}

// TestAuthenticateBindsWorkSpace proves the artifact read bound: an expanded
// payload may not exceed the free space the plan verified in the private work
// root, and the same artifact is admitted when that space is real.
func TestAuthenticateBindsWorkSpace(t *testing.T) {
	fixture := newPlanFixture(t)
	seed := fixture.backup(0)
	// InnoDB data is highly compressible: this expands well past the fixed floor
	// while its ciphertext stays small, which is what a real backup looks like.
	publishRealMembers(t, fixture, seed, bytes.Repeat([]byte("i"), 72<<20))
	backup := storedBackup(t, fixture, seed.ID)
	if backup.PayloadSize > 8<<20 {
		t.Fatalf("the fixture ciphertext is %d bytes, want a compressible stream", backup.PayloadSize)
	}
	small, link, dir := realExecution(t, fixture, backup, 8<<20)
	err := small.authenticate(t.Context(), link, dir)
	if err == nil {
		t.Fatal("a payload expanding past the verified work space authenticated")
	}
	if !strings.Contains(err.Error(), "restore: authenticate payload:") {
		t.Fatalf("error = %v, want the payload sub-step label", err)
	}
	ample, ampleLink, ampleDir := realExecution(t, fixture, backup, 1<<30)
	if err := ample.authenticate(t.Context(), ampleLink, ampleDir); err != nil {
		t.Fatalf("the same payload refused inside the verified work space: %v", err)
	}
}

// TestKindsMatch pins the manifest-to-row kind vocabulary.
func TestKindsMatch(t *testing.T) {
	for _, test := range []struct {
		manifest, record string
		ok               bool
	}{
		{"full", "physical-full", true},
		{"incremental", "physical-incremental", true},
		{"physical-full", "physical-full", true},
		{"full", "physical-incremental", false},
		{"incremental", "physical-full", false},
		{"full", "full-plus", false},
		{"", "physical-full", false},
	} {
		if got := kindsMatch(test.manifest, test.record); got != test.ok {
			t.Errorf("kindsMatch(%q, %q) = %v, want %v", test.manifest, test.record, got, test.ok)
		}
	}
}

// TestRemoveExactDirectoryRefusesSwappedSymlink proves the no-follow removal: an
// entry that is a symlink is refused and its target is never touched, while the
// exact directory of the same name is removed and a repeated call is idempotent.
func TestRemoveExactDirectoryRefusesSwappedSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	mustNoError(t, os.Mkdir(outside, 0o700))
	mustNoError(t, os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0o600))
	link := filepath.Join(root, "restore-"+strings.Repeat("a", 32))
	mustNoError(t, os.Symlink(outside, link))
	if err := removeExactDirectory(link, root); err == nil {
		t.Fatal("a symlinked restore target was removed")
	}
	if _, err := os.Lstat(filepath.Join(outside, "keep")); err != nil {
		t.Fatalf("the removal reached outside the parent: %v", err)
	}
	mustNoError(t, os.Remove(link))
	target := filepath.Join(root, "restore-"+strings.Repeat("b", 32))
	mustNoError(t, os.Mkdir(target, 0o700))
	mustNoError(t, os.Mkdir(filepath.Join(target, "inner"), 0o700))
	mustNoError(t, os.WriteFile(filepath.Join(target, "inner", "body"), []byte("body"), 0o600))
	if err := removeExactDirectory(target, root); err != nil {
		t.Fatalf("remove an exact directory: %v", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("the target survived: %v", err)
	}
	if _, err := os.Lstat(outside); err != nil {
		t.Fatal("the removal touched an unrelated directory")
	}
	if err := removeExactDirectory(target, root); err != nil {
		t.Fatalf("a repeated removal: %v", err)
	}
	if err := removeExactDirectory(filepath.Join(root, "..", "escape"), root); err == nil {
		t.Fatal("a target outside the parent was accepted")
	}
}
