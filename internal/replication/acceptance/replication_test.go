//go:build integration

package acceptance

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/replication"
)

// TestR1Worker is the single R1 entry point. It runs inside the pinned PXB
// container as the mysql uid: it first produces the real chains, then runs every
// replication scenario against the real FTP fixture. A missing case or a skip is
// a failure, never a downgrade.
func TestR1Worker(t *testing.T) {
	w := newWorker(t)
	w.produce()
	w.loadManifest()
	steps := []struct {
		name string
		run  func(*testing.T, *worker)
	}{
		{"a1-fifo-verified", a1},
		{"a2-local-corrupt", a2LocalCorrupt},
		{"a2-remote-tamper", a2RemoteTamper},
		{"a2-remote-truncate", a2RemoteTruncate},
		{"a2-wrong-kek", a2WrongKEK},
		{"a2-quota-552", a2Quota},
		{"a3-idempotent-enqueue", a3Idempotent},
		{"a3-blocked-predecessor", a3Blocked},
		{"a3-kill-recover", a3KillRecover},
		{"a4-backoff-exhausted", a4Backoff},
		{"a4-manual-retry", a4ManualRetry},
		{"a4-points", a4Points},
		{"a5-retention", a5Retention},
		{"a6-handoff", a6Handoff},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			w.t = t
			step.run(t, w)
		})
	}
}

// backoff is the documented delay min(BaseDelay·2^(attempts-1), MaxDelay),
// recomputed here so the observed schedule is compared with the formula and not
// with the implementation's own helper.
func backoff(policy replication.Policy, attempts int) time.Duration {
	delay := policy.BaseDelay
	for range attempts - 1 {
		if delay >= policy.MaxDelay/2 {
			return policy.MaxDelay
		}
		delay *= 2
	}
	return min(delay, policy.MaxDelay)
}

// fresh restarts the injected clock just after the previous scenario.
func fresh(w *worker) { w.now = w.now.Add(time.Minute) }

// step runs one attempt and returns its outcome.
func step(t *testing.T, r *replication.Replicator) (bool, error) {
	t.Helper()
	return r.Step(t.Context())
}

// attemptStep runs one attempt that must be attempted, and returns its error.
func attemptStep(t *testing.T, r *replication.Replicator) error {
	t.Helper()
	processed, err := step(t, r)
	if !processed {
		t.Fatalf("no item was attempted: %v", err)
	}
	return err
}

// expectFailure runs one attempt that must fail.
func expectFailure(t *testing.T, r *replication.Replicator, why string) {
	t.Helper()
	if err := attemptStep(t, r); err == nil {
		t.Fatal(why)
	}
}

// expectIdle requires that no item is due, which is how a blocked or exhausted
// item is observed without relying on its own status accessor.
func expectIdle(t *testing.T, r *replication.Replicator, why string) {
	t.Helper()
	if processed, err := step(t, r); processed || err != nil {
		t.Fatalf("%s: processed=%v err=%v", why, processed, err)
	}
}

// drive processes every due item in one repository until none is due, and
// requires every attempt to succeed.
func drive(t *testing.T, r *replication.Replicator) int {
	t.Helper()
	for attempts := 0; ; attempts++ {
		processed, err := step(t, r)
		if !processed {
			return attempts
		}
		must(t, err)
		if attempts > 64 {
			t.Fatal("queue did not drain")
		}
	}
}

// verify advances to the recorded retry time when needed and requires one
// verified replica.
func verify(t *testing.T, w *worker, r *replication.Replicator, backupID string) replication.Replica {
	t.Helper()
	if next := w.nextAttempt(t, r.RepositoryID, backupID); next.After(w.now) {
		w.now = next
	}
	must(t, attemptStep(t, r))
	record := w.replica(t, r, backupID)
	if record.Status != replication.StatusVerified {
		t.Fatalf("replica status = %q, want verified", record.Status)
	}
	return record
}

func enqueue(t *testing.T, r *replication.Replicator, backupID string) {
	t.Helper()
	must(t, r.Enqueue(t.Context(), backupID))
}

// a1 replicates one real full plus two incrementals in FIFO order, proves that a
// copy passes through uploaded before verified, and authenticates the remote
// bytes independently of the engine.
func a1(t *testing.T, w *worker) {
	fresh(w)
	r := w.primary(replication.Policy{})
	files := w.produced(t, "r1")
	if len(files) != 3 {
		t.Fatalf("produced %d backups, want a full plus two incrementals", len(files))
	}
	for _, file := range files {
		enqueue(t, r, file.BackupID)
	}
	first := observeUploaded(t, w, r, files)
	for _, file := range files[1:] {
		verify(t, w, r, file.BackupID)
	}
	raw := w.rawNormal(t)
	for index, file := range files {
		assertMembers(t, w, raw, r, file)
		t.Logf("R1-A1 backup=%d chain=%s seq=%d keys=%s remote_sha256=exact authenticated_eof=true uploaded_before_verified=true uploaded_at=%s verified_at=%s",
			index, file.ChainID, file.Seq, strings.Join(remoteKeys(file), ","),
			w.replica(t, r, file.BackupID).UploadedAt.UTC().Format(time.RFC3339),
			w.replica(t, r, file.BackupID).VerifiedAt.UTC().Format(time.RFC3339))
	}
	if rows := w.partRows(t, r.RepositoryID); rows != 0 {
		t.Fatalf("committed uploads left %d temporary part rows", rows)
	}
	if first != files[0].BackupID {
		t.Fatalf("the first FIFO item was not the one that reached uploaded: %s", first)
	}
}

// observeUploaded runs the first attempt and requires the durable status to be
// uploaded, not verified, at the boundary after every commit.
func observeUploaded(t *testing.T, w *worker, r *replication.Replicator, files []producedFile) string {
	t.Helper()
	observed := ""
	_, err := r.StepWithFault(t.Context(), func(point string) error {
		if point != "uploaded" {
			return nil
		}
		for _, file := range files {
			if w.replica(t, r, file.BackupID).Status == replication.StatusUploaded {
				if observed != "" {
					t.Fatal("more than one item was uploaded at once")
				}
				observed = file.BackupID
			}
		}
		return nil
	})
	must(t, err)
	if observed == "" {
		t.Fatal("status never passed through uploaded before verified")
	}
	record := w.replica(t, r, observed)
	if record.UploadedAt == nil || record.VerifiedAt == nil || record.UploadedAt.After(*record.VerifiedAt) {
		t.Fatal("the uploaded status does not precede verification")
	}
	return observed
}

// assertMembers requires one verified replica whose four remote members carry the
// local ciphertext, authenticate to EOF, and are recorded as verified.
func assertMembers(t *testing.T, w *worker, client *raw, r *replication.Replicator, file producedFile) {
	t.Helper()
	record := w.replica(t, r, file.BackupID)
	if record.Status != replication.StatusVerified || record.VerifiedAt == nil || record.UploadedAt == nil {
		t.Fatalf("backup %s is not verified with both times", file.BackupID)
	}
	if record.UploadedAt.After(*record.VerifiedAt) {
		t.Fatalf("backup %s was recorded verified before it was uploaded", file.BackupID)
	}
	for _, member := range members {
		remote, size := client.retrieve(t, file.ObjectPath+"/"+member)
		local, localSize := w.localDigest(t, file, member)
		if remote != local || size != localSize {
			t.Fatalf("remote %s differs from the local member", member)
		}
		if want := remoteDigests(file)[member]; remote != want {
			t.Fatalf("remote %s does not carry the produced ciphertext", member)
		}
		if status := w.memberStatuses(t, file.BackupID, r.RepositoryID)[member]; status != "verified" {
			t.Fatalf("member %s status = %q, want verified", member, status)
		}
	}
	authenticateRemote(t, w, client, file)
}

// authenticateRemote downloads the four remote members with an independent
// client and authenticates both v1 artifacts to EOF with the chain KEK.
func authenticateRemote(t *testing.T, w *worker, client *raw, file producedFile) {
	t.Helper()
	dir := t.TempDir()
	for _, member := range members {
		response, err := client.conn.Retr(file.ObjectPath + "/" + member)
		must(t, err)
		target, err := os.Create(filepath.Join(dir, member)) //nolint:gosec // Private test directory.
		must(t, err)
		_, err = io.Copy(target, response)
		must(t, errors.Join(err, response.Close(), target.Close()))
	}
	opts := artifact.ReadOptions{KeyFile: w.paths.key, Encrypt: true, WorkDirectory: dir, MaxBytes: 4 << 30}
	if _, err := artifact.ReadFiles(t.Context(), filepath.Join(dir, "envelope.json"), filepath.Join(dir, "payload.bin"), io.Discard, opts); err != nil {
		t.Fatalf("remote payload artifact did not authenticate to EOF: %v", err)
	}
	if _, err := artifact.ReadFiles(t.Context(), filepath.Join(dir, "metadata.envelope.json"), filepath.Join(dir, "metadata.bin"), io.Discard, opts); err != nil {
		t.Fatalf("remote metadata artifact did not authenticate to EOF: %v", err)
	}
}

// a2LocalCorrupt corrupts the local payload of a real completed backup before the
// copy and requires a non-retryable local_corrupt that leaves everything intact.
func a2LocalCorrupt(t *testing.T, w *worker) {
	fresh(w)
	r := w.replicator("ftp-corrupt", replication.NewFTPRemote(w.ftp), w.paths.key, replication.Policy{})
	file := w.produced(t, "r1-corrupt")[0]
	original := w.corruptLocal(t, file, "payload.bin")
	defer w.restoreLocal(t, file, "payload.bin", original)
	corruptedDigest, corruptedSize := w.localDigest(t, file, "payload.bin")
	enqueue(t, r, file.BackupID)
	expectFailure(t, r, "corrupt local ciphertext was not refused")
	assertLocalCorrupt(t, w, r, file)
	// No retry is scheduled: a much later step still must not attempt the item.
	w.now = w.now.Add(48 * time.Hour)
	expectIdle(t, r, "local corruption was retried")
	if after := w.replica(t, r, file.BackupID); after.Attempts != 1 {
		t.Fatalf("attempts after waiting = %d, want 1", after.Attempts)
	}
	// The local row, its presence flag, and the bytes the engine read are intact.
	backup := w.backup(t, file.BackupID)
	if backup.Status != "completed" || !backup.LocalPresent || backup.PayloadSHA256 != file.PayloadSHA256 {
		t.Fatalf("local row changed: status=%q present=%t", backup.Status, backup.LocalPresent)
	}
	if digest, size := w.localDigest(t, file, "payload.bin"); digest != corruptedDigest || size != corruptedSize {
		t.Fatal("the engine altered the local payload")
	}
	w.rawNormal(t).absent(t, file.ObjectPath+"/payload.bin")
	t.Logf("R1-A2 local_corrupt backup=%s attempts=1 retried=false alert=set local_status=completed local_present=true local_bytes_untouched=true remote_payload=absent",
		file.BackupID)
}

// assertLocalCorrupt requires the recorded local_corrupt state and its alert.
func assertLocalCorrupt(t *testing.T, w *worker, r *replication.Replicator, file producedFile) {
	t.Helper()
	record := w.replica(t, r, file.BackupID)
	if record.Status != replication.StatusFailed || record.LastErrorCode != replication.CodeLocalCorrupt {
		t.Fatalf("status=%q code=%q, want failed/local_corrupt", record.Status, record.LastErrorCode)
	}
	if record.Attempts != 1 {
		t.Fatalf("attempts = %d, want one", record.Attempts)
	}
	if w.alertSince(t, r.RepositoryID, file.BackupID) == nil {
		t.Fatal("local corruption did not raise the alert")
	}
}

// a2RemoteTamper replaces a committed remote member with other real bytes between
// the upload and the readback, using the real FTP protocol.
func a2RemoteTamper(t *testing.T, w *worker) {
	fresh(w)
	file := w.produced(t, "r1-tamper")[0]
	r := w.replicator("ftp-tamper", replication.NewFTPRemote(w.ftp), w.paths.key, replication.Policy{})
	enqueue(t, r, file.BackupID)
	client := w.rawNormal(t)
	_, err := r.StepWithFault(t.Context(), func(point string) error {
		if point == "before_verify" {
			client.store(t, file.ObjectPath+"/payload.bin", []byte("replaced-by-the-harness"))
		}
		return nil
	})
	if err == nil {
		t.Fatal("a tampered remote member was accepted as a verified copy")
	}
	record := w.replica(t, r, file.BackupID)
	if record.Status == replication.StatusVerified {
		t.Fatal("a tampered copy reached verified")
	}
	if status := w.memberStatuses(t, file.BackupID, r.RepositoryID)["payload.bin"]; status == "verified" {
		t.Fatal("the tampered member was marked verified")
	}
	t.Logf("R1-A2 remote_tamper backup=%s status=%s member=payload.bin tampered_via=real-ftp", file.BackupID, record.Status)
}

// a2RemoteTruncate rewrites a committed remote member with a shorter body.
func a2RemoteTruncate(t *testing.T, w *worker) {
	fresh(w)
	file := w.produced(t, "r1-tamper")[1]
	r := w.replicator("ftp-truncate", replication.NewFTPRemote(w.ftp), w.paths.key, replication.Policy{})
	enqueue(t, r, file.BackupID)
	client := w.rawNormal(t)
	_, err := r.StepWithFault(t.Context(), func(point string) error {
		if point == "before_verify" {
			client.truncate(t, file.ObjectPath+"/metadata.bin", []byte{})
		}
		return nil
	})
	if err == nil {
		t.Fatal("a truncated remote member was accepted as a verified copy")
	}
	record := w.replica(t, r, file.BackupID)
	if record.Status == replication.StatusVerified {
		t.Fatal("a truncated copy reached verified")
	}
	if size := client.exists(t, file.ObjectPath+"/metadata.bin"); size != 0 {
		t.Fatalf("truncated member size = %d, want 0", size)
	}
	t.Logf("R1-A2 remote_truncate backup=%s status=%s member=metadata.bin remote_size=0", file.BackupID, record.Status)
}

// a2WrongKEK replicates an intact real copy with a KEK that did not seal it, so
// only authentication can fail.
func a2WrongKEK(t *testing.T, w *worker) {
	fresh(w)
	file := w.produced(t, "r1-kek")[0]
	r := w.replicator("ftp-wrong-kek", replication.NewFTPRemote(w.ftp), w.paths.wrongKey, replication.Policy{})
	enqueue(t, r, file.BackupID)
	expectFailure(t, r, "a copy authenticated with the wrong KEK was accepted")
	record := w.replica(t, r, file.BackupID)
	if record.Status == replication.StatusVerified || record.LastErrorCode != replication.CodeVerifyArtifact {
		t.Fatalf("status=%q code=%q, want not verified with verify_artifact", record.Status, record.LastErrorCode)
	}
	client := w.rawNormal(t)
	remote, size := client.retrieve(t, file.ObjectPath+"/payload.bin")
	local, localSize := w.localDigest(t, file, "payload.bin")
	if remote != local || size != localSize {
		t.Fatal("the wrong-KEK scenario changed the remote ciphertext")
	}
	t.Logf("R1-A2 wrong_kek backup=%s code=%s status=%s ciphertext=intact", file.BackupID, record.LastErrorCode, record.Status)
}

// a2Quota exceeds the real one-file quota account: the server answers 552 during
// the real transfer, the copy is never verified, and its part is cleaned.
func a2Quota(t *testing.T, w *worker) {
	fresh(w)
	w.resetQuota(t)
	file := w.produced(t, "r1-quota")[0]
	r := w.replicator(quotaRepo, replication.NewFTPRemote(w.quota), w.paths.key, replication.Policy{})
	enqueue(t, r, file.BackupID)
	expectFailure(t, r, "the quota account accepted the copy")
	record := w.replica(t, r, file.BackupID)
	if record.Status == replication.StatusVerified || record.LastErrorCode != replication.CodeRemoteQuota {
		t.Fatalf("status=%q code=%q, want not verified with remote_quota", record.Status, record.LastErrorCode)
	}
	if record.LastReplyCode != 552 {
		t.Fatalf("reply code = %d, want the real 552", record.LastReplyCode)
	}
	if rows := w.partRows(t, quotaRepo); rows != 0 {
		t.Fatalf("the quota failure left %d recorded parts", rows)
	}
	published := w.countRemote(t, w.rawQuota(t), file)
	if published == len(members) {
		t.Fatal("the quota refusal still published every member")
	}
	if parts := w.rawQuota(t).parts(t, file.ObjectPath); len(parts) != 0 {
		t.Fatalf("the server still lists temporary parts: %v", parts)
	}
	t.Logf("R1-A2 quota_552 backup=%s code=%s reply=552 published_members=%d/%d parts=cleaned",
		file.BackupID, record.LastErrorCode, published, len(members))
}

// countRemote counts the published members of one backup on the real server.
func (w *worker) countRemote(t *testing.T, client *raw, file producedFile) int {
	t.Helper()
	published := 0
	for _, key := range remoteKeys(file) {
		if _, err := client.conn.FileSize(key); err == nil {
			published++
		}
	}
	return published
}

// a3Idempotent repeats Enqueue and EnqueueCompleted and requires a single row.
func a3Idempotent(t *testing.T, w *worker) {
	fresh(w)
	r := w.replicator("ftp-idempotent", replication.NewFTPRemote(w.ftp), w.paths.key, replication.Policy{})
	file := w.produced(t, "r1-retry")[0]
	single := "SELECT COUNT(*) FROM replicas WHERE backup_id = $1 AND repository_id = $2"
	for range 3 {
		enqueue(t, r, file.BackupID)
		must(t, r.EnqueueCompleted(t.Context(), "r1-retry"))
	}
	if rows := w.count(t, single, file.BackupID, r.RepositoryID); rows != 1 {
		t.Fatalf("replica rows for one backup = %d, want 1", rows)
	}
	verify(t, w, r, file.BackupID)
	// A repeated trigger after verification changes nothing.
	enqueue(t, r, file.BackupID)
	must(t, r.EnqueueCompleted(t.Context(), "r1-retry"))
	if rows := w.count(t, single, file.BackupID, r.RepositoryID); rows != 1 {
		t.Fatalf("rows after repeated triggers = %d, want 1", rows)
	}
	if record := w.replica(t, r, file.BackupID); record.Status != replication.StatusVerified {
		t.Fatalf("repeated trigger changed the status to %q", record.Status)
	}
	t.Logf("R1-A3 idempotent backup=%s rows=1 triggers=8 status=verified", file.BackupID)
}

// a3Blocked requires a real failed predecessor to block its successor, so the
// verified prefix never skips a gap, and recovery to resume in order.
func a3Blocked(t *testing.T, w *worker) {
	fresh(w)
	r := w.replicator("ftp-blocked", replication.NewFTPRemote(w.ftp), w.paths.key, replication.Policy{})
	files := w.produced(t, "r1-blocked")
	if len(files) != 3 {
		t.Fatalf("produced %d backups for the blocking scenario, want 3", len(files))
	}
	first, second, successor := files[0], files[1], files[2]
	enqueue(t, r, first.BackupID)
	enqueue(t, r, second.BackupID)
	enqueue(t, r, successor.BackupID)
	verify(t, w, r, first.BackupID)
	original := w.corruptLocal(t, second, "payload.bin")
	expectFailure(t, r, "the corrupt middle member was not refused")
	if status := w.replica(t, r, second.BackupID).Status; status != replication.StatusFailed {
		t.Fatalf("predecessor status = %q, want failed", status)
	}
	// The successor is never attempted while its predecessor is unverified.
	w.now = w.now.Add(72 * time.Hour)
	expectIdle(t, r, "a blocked successor was attempted")
	if status := w.replica(t, r, successor.BackupID).Status; status != replication.StatusPending {
		t.Fatalf("successor status = %q, want pending", status)
	}
	assertPoint(t, w, r, "r1-blocked", first.BackupID, "the remote point did not stop at the prefix")
	w.restoreLocal(t, second, "payload.bin", original)
	must(t, r.Retry(t.Context(), second.BackupID))
	verify(t, w, r, second.BackupID)
	verify(t, w, r, successor.BackupID)
	assertPoint(t, w, r, "r1-blocked", successor.BackupID, "the remote point did not follow the recovery")
	t.Logf("R1-A3 blocked predecessor=%s successor=%s successor_while_blocked=pending point_at_gap=%s final_point=%s",
		second.BackupID, successor.BackupID, first.BackupID, successor.BackupID)
}

// assertPoint requires the remote point of one source to be exactly one backup.
func assertPoint(t *testing.T, w *worker, r *replication.Replicator, sourceID, backupID, why string) {
	t.Helper()
	point, err := r.RemotePoint(t.Context(), sourceID)
	must(t, err)
	if point == nil || point.BackupID != backupID || point.At.IsZero() {
		t.Fatalf("%s: point=%+v want %s", why, point, backupID)
	}
}

// a3KillRecover SIGKILLs a separate replication process mid-upload, then a
// restarted process recovers on the real FTP and the item verifies.
func a3KillRecover(t *testing.T, w *worker) {
	fresh(w)
	file := w.produced(t, "r1-kill")[0]
	repositoryID := "ftp-kill"
	marker := filepath.Join(w.paths.work, "kill-child.written")
	must(t, os.RemoveAll(marker))
	if rows := w.partRows(t, repositoryID); rows != 0 {
		t.Fatalf("the kill repository already holds %d part rows", rows)
	}
	executable, err := os.Executable()
	must(t, err)
	child := childCommand(t, executable, "TestR1KillChildWorker")
	child.Stdout = os.Stderr
	child.Stderr = os.Stderr
	child.Env = append(child.Env,
		"INTERN_R1_KILL_REPOSITORY="+repositoryID,
		"INTERN_R1_KILL_BACKUP="+file.BackupID,
		"INTERN_R1_KILL_MARKER="+marker,
	)
	must(t, child.Start())
	waitMarker(t, marker)
	client := w.rawNormal(t)
	part := w.recordedPart(t, repositoryID, file.BackupID)
	if size := client.exists(t, part); size == 0 {
		t.Fatal("the killed worker never transferred real payload bytes")
	}
	killChild(t, child)
	// A restarted process reopens the committed database and cleans by the ledger.
	// CombinedOutput requires unset Stdout/Stderr; childCommand leaves them unset.
	restart := childCommand(t, executable, "TestR1RestartChildWorker")
	restart.Env = append(restart.Env,
		"INTERN_R1_KILL_REPOSITORY="+repositoryID,
		"INTERN_R1_KILL_BACKUP="+file.BackupID,
		"INTERN_R1_KILL_PART="+part,
	)
	output, err := restart.CombinedOutput()
	text := string(output)
	if err != nil || !strings.Contains(text, "R1-A3 restart recovery completed") ||
		!strings.Contains(text, "second_recover=noop") || strings.Contains(text, "SKIP") {
		t.Fatalf("the restarted worker did not recover: %v\n%s", err, text)
	}
	client.absent(t, part)
	if rows := w.partRows(t, repositoryID); rows != 0 {
		t.Fatalf("recovery kept %d part rows", rows)
	}
	if status := w.replicaFor(t, repositoryID, file.BackupID).Status; status != replication.StatusVerified {
		t.Fatalf("the recovered item is %q, want verified", status)
	}
	t.Logf("R1-A3 kill=SIGKILL part=%s part_bytes_before_kill=nonzero cleaned=real-ftp recovered=verified restart=separate-process repository=%s",
		part, repositoryID)
}

// recordedPart reads the durable temporary key of one killed member.
func (w *worker) recordedPart(t *testing.T, repositoryID, backupID string) string {
	t.Helper()
	var part string
	must(t, w.db.QueryRowContext(t.Context(),
		"SELECT part FROM replica_parts WHERE repository_id = $1 AND backup_id = $2", repositoryID, backupID).Scan(&part))
	if part == "" {
		t.Fatal("the killed worker left no recorded part")
	}
	return part
}

// a4Backoff drives repeated real quota failures with the injected clock and
// requires the exact delays, exhaustion, and the alert transition.
func a4Backoff(t *testing.T, w *worker) {
	fresh(w)
	w.resetQuota(t)
	file := w.produced(t, "r1-backoff")[0]
	policy := replication.Policy{BaseDelay: time.Minute, MaxDelay: 5 * time.Minute, AlertAfter: 90 * time.Second, MaxAttempts: 4}
	repositoryID := quotaRepo + "-backoff"
	r := w.replicator(repositoryID, replication.NewFTPRemote(w.quota), w.paths.key, policy)
	enqueue(t, r, file.BackupID)
	start := w.now
	delays := make([]time.Duration, 0, policy.MaxAttempts-1)
	for attempt := 1; attempt < policy.MaxAttempts; attempt++ {
		expectFailure(t, r, "attempt did not fail")
		want := backoff(policy, attempt)
		assertAttempt(t, w, r, file, attempt, want, start, policy)
		delays = append(delays, want)
		w.now = w.nextAttempt(t, repositoryID, file.BackupID)
	}
	expectFailure(t, r, "the last attempt did not fail")
	final := w.replica(t, r, file.BackupID)
	if final.Attempts != policy.MaxAttempts || final.Status != replication.StatusExhausted {
		t.Fatalf("final attempts=%d status=%q, want exhausted after %d", final.Attempts, final.Status, policy.MaxAttempts)
	}
	if w.alertSince(t, repositoryID, file.BackupID) == nil {
		t.Fatal("exhaustion did not keep the alert")
	}
	// An exhausted item is never attempted again, at any later time.
	w.now = w.now.Add(30 * 24 * time.Hour)
	expectIdle(t, r, "an exhausted item was attempted again")
	if after := w.replica(t, r, file.BackupID); after.Attempts != policy.MaxAttempts {
		t.Fatalf("attempts after exhaustion = %d, want %d", after.Attempts, policy.MaxAttempts)
	}
	t.Logf("R1-A4 backoff delays=%s cap=%s alert_after=%s exhausted_attempts=%d alert=set retried_after_exhausted=false",
		delays, policy.MaxDelay, policy.AlertAfter, policy.MaxAttempts)
}

// assertAttempt requires one failure attempt's count, schedule and alert state.
func assertAttempt(t *testing.T, w *worker, r *replication.Replicator, file producedFile, attempt int, want time.Duration, start time.Time, policy replication.Policy) {
	t.Helper()
	record := w.replica(t, r, file.BackupID)
	if record.Attempts != attempt || record.Status != replication.StatusFailed {
		t.Fatalf("attempt %d recorded as attempts=%d status=%q", attempt, record.Attempts, record.Status)
	}
	if got := w.nextAttempt(t, r.RepositoryID, file.BackupID).Sub(w.now); got != want {
		t.Fatalf("attempt %d delay = %s, want %s", attempt, got, want)
	}
	alerted := w.alertSince(t, r.RepositoryID, file.BackupID) != nil
	if want := w.now.Sub(start) >= policy.AlertAfter; alerted != want {
		t.Fatalf("attempt %d alert = %t, want %t", attempt, alerted, want)
	}
}

// a4ManualRetry resolves a real remote conflict through the real FTP protocol and
// requires the manual retry to verify and clear the alert while keeping history.
func a4ManualRetry(t *testing.T, w *worker) {
	fresh(w)
	file := w.produced(t, "r1-conflict")[0]
	repositoryID := "ftp-conflict"
	r := w.replicator(repositoryID, replication.NewFTPRemote(w.ftp), w.paths.key, replication.Policy{MaxAttempts: 2})
	key := file.ObjectPath + "/payload.bin"
	conflicting := []byte("a-different-object-at-the-final-key")
	client := w.rawNormal(t)
	client.mkdirs(t, file.ObjectPath)
	// The final key already holds a different object, so the real sink must refuse
	// to overwrite it.
	client.store(t, key, conflicting)
	enqueue(t, r, file.BackupID)
	expectFailure(t, r, "a conflicting final key was accepted")
	assertConflict(t, w, r, file, client, key, conflicting)
	// The operator removes it; only then can a manual retry publish.
	client.remove(t, key)
	client.absent(t, key)
	must(t, r.Retry(t.Context(), file.BackupID))
	assertRetried(t, w, r, file)
	verify(t, w, r, file.BackupID)
	if w.alertSince(t, repositoryID, file.BackupID) != nil {
		t.Fatal("verified did not clear the alert")
	}
	remote, _ := client.retrieve(t, key)
	local, _ := w.localDigest(t, file, "payload.bin")
	if remote != local {
		t.Fatal("the recovered member does not carry the local ciphertext")
	}
	t.Logf("R1-A4 manual_retry backup=%s code=%s attempts_before=1 attempts_after=0 history_kept=true final=verified alert=cleared",
		file.BackupID, replication.CodeRemoteConflict)
}

// assertConflict requires the non-retryable conflict state and the untouched key.
func assertConflict(t *testing.T, w *worker, r *replication.Replicator, file producedFile, client *raw, key string, conflicting []byte) {
	t.Helper()
	record := w.replica(t, r, file.BackupID)
	if record.Status != replication.StatusFailed || record.LastErrorCode != replication.CodeRemoteConflict {
		t.Fatalf("status=%q code=%q, want failed/remote_conflict", record.Status, record.LastErrorCode)
	}
	if record.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", record.Attempts)
	}
	if w.alertSince(t, r.RepositoryID, file.BackupID) == nil {
		t.Fatal("a remote conflict did not raise the alert")
	}
	if size := client.exists(t, key); size != int64(len(conflicting)) {
		t.Fatalf("conflicting object size = %d, want %d", size, len(conflicting))
	}
}

// assertRetried requires the manual retry's reset state and kept history.
func assertRetried(t *testing.T, w *worker, r *replication.Replicator, file producedFile) {
	t.Helper()
	retried := w.replica(t, r, file.BackupID)
	if retried.Status != replication.StatusPending || retried.Attempts != 0 {
		t.Fatalf("after manual retry status=%q attempts=%d", retried.Status, retried.Attempts)
	}
	if retried.NextAttemptAt == nil || retried.NextAttemptAt.After(w.now) {
		t.Fatal("manual retry did not schedule the item immediately")
	}
	if retried.LastErrorCode == "" {
		t.Fatal("manual retry discarded the failure history")
	}
}

// a4Points requires both protection points to follow only their own complete
// prefix: local completion and the remote verified prefix.
func a4Points(t *testing.T, w *worker) {
	fresh(w)
	files := w.produced(t, "r1-points")
	r := w.replicator("ftp-points", replication.NewFTPRemote(w.ftp), w.paths.key, replication.Policy{})
	last := files[len(files)-1]
	assertLocalPoint(t, w, "r1-points", last)
	// A real verified prefix, then a real gap: a failed predecessor never lets a
	// successor extend the remote point.
	enqueue(t, r, files[0].BackupID)
	verify(t, w, r, files[0].BackupID)
	assertPoint(t, w, r, "r1-points", files[0].BackupID, "the remote point is not the last verified replica")
	original := w.corruptLocal(t, files[1], "payload.bin")
	defer w.restoreLocal(t, files[1], "payload.bin", original)
	enqueue(t, r, files[1].BackupID)
	enqueue(t, r, files[2].BackupID)
	expectFailure(t, r, "the corrupt member was accepted")
	expectIdle(t, r, "a blocked successor was attempted")
	assertPoint(t, w, r, "r1-points", files[0].BackupID, "the remote point moved past an unverified predecessor")
	assertLocalPoint(t, w, "r1-points", last)
	point, err := r.RemotePoint(t.Context(), "r1-points")
	must(t, err)
	t.Logf("R1-A4 points local=%s remote=%s gap_local_unchanged=true gap_remote_stopped=true point_at=%s",
		last.BackupID, files[0].BackupID, point.At.UTC().Format(time.RFC3339))
}

// assertLocalPoint requires the local point to be the latest completed backup.
func assertLocalPoint(t *testing.T, w *worker, sourceID string, last producedFile) {
	t.Helper()
	local, err := replication.LocalPoint(t.Context(), w.store, sourceID)
	must(t, err)
	if local == nil || local.BackupID != last.BackupID || local.ChainID != last.ChainID || local.At.IsZero() {
		t.Fatalf("local point = %+v, want the latest completed backup", local)
	}
}

// a5Retention verifies a second real chain and exercises whole-chain local
// deletion together with every refusal.
func a5Retention(t *testing.T, w *worker) {
	fresh(w)
	r := w.replicator("ftp-ret", replication.NewFTPRemote(w.ftp), w.paths.key, replication.Policy{})
	files := w.produced(t, "r1-ret")
	must(t, r.EnqueueCompleted(t.Context(), "r1-ret"))
	drive(t, r)
	for _, file := range files {
		if status := w.replica(t, r, file.BackupID).Status; status != replication.StatusVerified {
			t.Fatalf("backup %s is %q, want verified", file.BackupID, status)
		}
	}
	olderID, newerID := files[0].ChainID, files[len(files)-1].ChainID
	if olderID == newerID {
		t.Fatal("the retention scenario needs two distinct chains")
	}
	older, newer := w.producedOfChain(t, "r1-ret", olderID), w.producedOfChain(t, "r1-ret", newerID)
	assertRetainedOlderChain(t, w, older)
	assertEligible(t, r, olderID, true, "")
	// Eligibility alone deletes nothing.
	for _, file := range files {
		if !w.published(t, file) || !w.backup(t, file.BackupID).LocalPresent {
			t.Fatal("eligibility removed a local payload")
		}
	}
	must(t, r.DeleteLocalChain(t.Context(), olderID))
	assertReclaimedChain(t, w, files, olderID)
	assertOlderChainReclaimedTogether(t, w, older, newer)
	// Refusals on the still-present chain: pinned, then latest.
	assertPinned(t, w, r, newerID)
	assertLatest(t, w, r, newerID)
	assertUnverifiedRefused(t, w, newerID)
	// A chain is deleted whole: repeating the completed deletion is idempotent
	// and never touches the kept chain, and recovery reclaims nothing extra.
	if err := r.DeleteLocalChain(t.Context(), olderID); err != nil {
		t.Fatalf("repeating a completed whole-chain delete failed: %v", err)
	}
	must(t, r.Recover(t.Context()))
	wholeChainOnly, middleReclaimed, keptUntouched, rowsRetained := assertOlderChainReclaimedTogether(t, w, older, newer)
	t.Logf("R1-A5 chains older=%s newer=%s deleted=%s kept=%s older_members=%d middle_link=%s middle_link_reclaimed=%t refused=pinned,latest,unverified whole_chain_only=%t rows_retained=%t kept_untouched=%t automatic=none",
		olderID, newerID, olderID, newerID, len(older), older[1].BackupID, middleReclaimed, wholeChainOnly, rowsRetained, keptUntouched)
}

// assertRetainedOlderChain requires the produced older chain to be a real
// full+inc+inc from chain.Run, so a middle link exists to reclaim with the rest.
func assertRetainedOlderChain(t *testing.T, w *worker, older []producedFile) {
	t.Helper()
	if len(older) != 3 {
		t.Fatalf("retained chain has %d members, want full+inc+inc", len(older))
	}
	if older[0].Seq != 1 || older[1].Seq != 2 || older[2].Seq != 3 {
		t.Fatalf("retained chain seq = %d/%d/%d, want 1/2/3", older[0].Seq, older[1].Seq, older[2].Seq)
	}
	kinds := []string{w.backup(t, older[0].BackupID).Kind, w.backup(t, older[1].BackupID).Kind, w.backup(t, older[2].BackupID).Kind}
	if kinds[0] != "physical-full" || kinds[1] != "physical-incremental" || kinds[2] != "physical-incremental" {
		t.Fatalf("retained chain kinds = %v, want physical-full, physical-incremental, physical-incremental", kinds)
	}
}

func memberGone(t *testing.T, w *worker, file producedFile) bool {
	t.Helper()
	return !w.published(t, file) && !w.backup(t, file.BackupID).LocalPresent
}

func memberPresent(t *testing.T, w *worker, file producedFile) bool {
	t.Helper()
	return w.published(t, file) && w.backup(t, file.BackupID).LocalPresent
}

func memberRowRetained(t *testing.T, w *worker, file producedFile) bool {
	t.Helper()
	backup := w.backup(t, file.BackupID)
	return backup.Status == "completed" && len(backup.CheckpointsRaw) != 0 && len(backup.InfoRaw) != 0
}

// assertOlderChainReclaimedTogether requires every older member, including the
// middle link, to lose local_present and its directory together, while the kept
// chain stays on disk. It returns the marker values derived from that state.
func assertOlderChainReclaimedTogether(t *testing.T, w *worker, older, newer []producedFile) (wholeChainOnly, middleReclaimed, keptUntouched, rowsRetained bool) {
	t.Helper()
	if len(older) != 3 {
		t.Fatalf("older chain has %d members, want 3", len(older))
	}
	gone := 0
	rowsRetained = true
	for _, file := range older {
		if memberGone(t, w, file) {
			gone++
		}
		if !memberRowRetained(t, w, file) {
			rowsRetained = false
		}
	}
	wholeChainOnly = gone == len(older)
	if !wholeChainOnly {
		t.Fatalf("older chain reclaim was partial: gone=%d, want all %d members removed together", gone, len(older))
	}
	middleReclaimed = memberGone(t, w, older[1])
	if !middleReclaimed {
		t.Fatal("middle link remained after whole-chain delete")
	}
	keptUntouched = true
	for _, file := range newer {
		if memberPresent(t, w, file) {
			continue
		}
		keptUntouched = false
		t.Fatalf("kept chain member %s was touched", file.BackupID)
	}
	if !rowsRetained {
		t.Fatal("whole-chain delete dropped rows, checkpoints or metadata")
	}
	return wholeChainOnly, middleReclaimed, keptUntouched, rowsRetained
}

// assertEligible requires one chain's eligibility and reason code.
func assertEligible(t *testing.T, r *replication.Replicator, chainID string, want bool, reason string) {
	t.Helper()
	eligible, got, err := r.Eligible(t.Context(), chainID)
	must(t, err)
	if eligible != want || got != reason {
		t.Fatalf("chain %s eligibility = %t/%q, want %t/%q", chainID, eligible, got, want, reason)
	}
}

// assertReclaimedChain requires the reclaimed chain to lose only its local
// payload, with its rows, checkpoints and metadata retained.
func assertReclaimedChain(t *testing.T, w *worker, files []producedFile, chainID string) {
	t.Helper()
	for _, file := range files {
		deleted := file.ChainID == chainID
		if w.published(t, file) == deleted {
			t.Fatalf("member %s local payload presence = %t, want %t", file.BackupID, !deleted, deleted)
		}
		backup := w.backup(t, file.BackupID)
		if backup.LocalPresent == deleted {
			t.Fatalf("member %s local_present = %t, want %t", file.BackupID, backup.LocalPresent, !deleted)
		}
		if backup.Status != "completed" || len(backup.CheckpointsRaw) == 0 || len(backup.InfoRaw) == 0 {
			t.Fatalf("member %s lost its row, checkpoints or metadata", file.BackupID)
		}
		if backup.PayloadSHA256 != file.PayloadSHA256 || backup.ChainID != file.ChainID {
			t.Fatalf("member %s row changed", file.BackupID)
		}
	}
}

// assertPinned requires a pinned chain to be refused and kept.
func assertPinned(t *testing.T, w *worker, r *replication.Replicator, chainID string) {
	t.Helper()
	must(t, w.store.SetPinned(t.Context(), chainID, true))
	defer func() { must(t, w.store.SetPinned(t.Context(), chainID, false)) }()
	assertEligible(t, r, chainID, false, replication.RetentionPinned)
	if err := r.DeleteLocalChain(t.Context(), chainID); err == nil {
		t.Fatal("a pinned chain was deleted")
	}
	if !w.published(t, w.producedOfChain(t, "r1-ret", chainID)[0]) {
		t.Fatal("a refused pinned delete removed the chain payload")
	}
}

// assertLatest requires the source's latest chain to be refused and kept.
func assertLatest(t *testing.T, w *worker, r *replication.Replicator, chainID string) {
	t.Helper()
	assertEligible(t, r, chainID, false, replication.RetentionLatest)
	if err := r.DeleteLocalChain(t.Context(), chainID); err == nil {
		t.Fatal("the source's latest chain was deleted")
	}
	if !w.published(t, w.producedOfChain(t, "r1-ret", chainID)[0]) {
		t.Fatal("a refused latest delete removed the chain payload")
	}
}

// assertUnverifiedRefused requires a repository with nothing verified to refuse
// the chain without changing anything.
func assertUnverifiedRefused(t *testing.T, w *worker, chainID string) {
	t.Helper()
	unverified := w.replicator("ftp-ret-unverified", replication.NewFTPRemote(w.ftp), w.paths.key, replication.Policy{})
	must(t, unverified.EnqueueCompleted(t.Context(), "r1-ret"))
	members := w.producedOfChain(t, "r1-ret", chainID)
	if status := w.replica(t, unverified, members[0].BackupID).Status; status != replication.StatusPending {
		t.Fatalf("the fresh repository already reports %q", status)
	}
	assertEligible(t, unverified, chainID, false, replication.RetentionUnverified)
	if err := unverified.DeleteLocalChain(t.Context(), chainID); err == nil {
		t.Fatal("an unverified chain was deleted")
	}
	if !w.published(t, members[0]) {
		t.Fatal("a refused unverified delete removed the chain payload")
	}
}

// a6Handoff records what stage D needs to rebuild a verified copy from the remote
// repository alone.
func a6Handoff(t *testing.T, w *worker) {
	fresh(w)
	client := w.rawNormal(t)
	lines := 0
	// Two replicated chains: the continuous one from A1 and the retention source.
	handoff := []struct{ sourceID, repositoryID string }{
		{sourceID: "r1", repositoryID: primaryRepo},
		{sourceID: "r1-ret", repositoryID: "ftp-ret"},
	}
	for _, entry := range handoff {
		sourceID := entry.sourceID
		for _, file := range w.produced(t, sourceID) {
			record := w.replicaFor(t, entry.repositoryID, file.BackupID)
			if record.Status != replication.StatusVerified || record.VerifiedAt == nil {
				continue
			}
			digests := make([]string, 0, len(members))
			for _, member := range members {
				digest, size := client.retrieve(t, file.ObjectPath+"/"+member)
				digests = append(digests, fmt.Sprintf("%s:%s:%d", member, digest, size))
			}
			lines++
			t.Logf("R1-A6 handoff source=%s chain=%s backup=%s seq=%d keys=%s members=%s snapshot_sha256=%s mysql=%s pxb=%s point=%s",
				sourceID, file.ChainID, file.BackupID, file.Seq, strings.Join(remoteKeys(file), ","),
				strings.Join(digests, ","), file.SnapshotSHA256, w.manifest.ServerVersion, w.manifest.ToolVersion,
				record.VerifiedAt.UTC().Format(time.RFC3339))
		}
	}
	if lines == 0 {
		t.Fatal("no verified copy is available for the stage D handoff")
	}
}
