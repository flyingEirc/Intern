//go:build integration

package acceptance

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"

	"github.com/flyingEirc/Intern/app/web/internal/dispatch"
	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/physical"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

// TestW5Worker is the single W5 native entry point. It runs inside the pinned
// restore image (MySQL 8.4.11 plus Percona XtraBackup 8.4.0-7 and xbstream) as
// the source's own mysql uid, against the real MySQL socket, the real Pure-FTPd
// fixture, a real export volume the harness can also mount into a brand-new
// server, and the real internal/restore engine behind the HTTP API.
//
// Every locked W5 fact is read back through the API, SQLite, the local
// repository, the FTP namespace, the export directory or a rehearsal's own
// result: no marker value is a constant. The restore inputs are opt-in
// (INTERN_W5_EXPORT_ROOT, INTERN_W5_XBSTREAM, INTERN_W5_MYSQLD); a run without
// them refuses instead of reporting refusals it never exercised.
func TestW5Worker(t *testing.T) {
	requireW5Restore(t)
	f := newFixture(t)
	f.topology(t)
	f.startDispatch(t)
	w5 = &w5State{}
	t.Logf("W5 worker tool_sha256=%s xbstream_sha256=%s mysqld=%s export=%s instance=%s uid=%d gid=%d",
		toolSHA256(), fileSHA256Or(f.settings.XbstreamPath), f.settings.MysqldPath, f.layout.export,
		f.instanceID, os.Getuid(), os.Getgid())
	steps := []struct {
		name string
		run  func(*testing.T, *fixture)
	}{
		{"a2-plan", w5A2Plan},
		{"a6-e2e", w5A6E2E},
		{"a3-execute", w5A3Execute},
		{"a4-exclusion", w5A4Exclusion},
		{"a5-state", w5A5State},
	}
	// The cases share one chain: a2 builds it and every later case judges the
	// state an earlier case really produced. A failed case therefore ends the run
	// at once instead of letting the next one dereference state that was never
	// written and panic the whole test binary.
	for _, step := range steps {
		if !t.Run(step.name, func(t *testing.T) {
			f.t = t
			t.Cleanup(func() { f.dumpDiagnostics(t) })
			step.run(t, f)
		}) {
			t.Fatalf("W5 case %s failed: the shared chain state it was to produce is unknown, so no later case runs", step.name)
		}
	}
}

// requireW5Restore refuses a partially configured restore deployment.
func requireW5Restore(t *testing.T) {
	t.Helper()
	requireNative(t)
	for _, entry := range []struct{ name, path string }{
		{"INTERN_W5_EXPORT_ROOT", os.Getenv("INTERN_W5_EXPORT_ROOT")},
		{"INTERN_W5_XBSTREAM", os.Getenv("INTERN_W5_XBSTREAM")},
		{"INTERN_W5_MYSQLD", os.Getenv("INTERN_W5_MYSQLD")},
	} {
		if entry.path == "" {
			t.Fatalf("%s is required: the W5 worker restores with the real engine", entry.name)
		}
		info, err := os.Stat(entry.path)
		if err != nil {
			t.Fatalf("%s (%s): %v", entry.name, entry.path, err)
		}
		if entry.name == "INTERN_W5_EXPORT_ROOT" && !info.IsDir() {
			t.Fatalf("%s is not a directory", entry.name)
		}
	}
}

// w5Snapshot is the source state recorded at one restore point: the same digest
// the harness computes inside a brand-new server started on that restore's
// export, plus the row counts the rehearsal must answer.
type w5Snapshot struct {
	digest  string
	records int64
	lsnRows int64
}

// w5State carries what the chain scenario learns, so a later case judges the
// state an earlier case really produced.
type w5State struct {
	full, inc1, inc2 store.Run
	members          []dispatch.BackupView
	snapFull         w5Snapshot
	snap1, snap2     w5Snapshot
	planFull         store.RestorePlan
	plan1, plan2     store.RestorePlan
	exported         []string
	// created holds every restore id the scenario created, oldest first, so the
	// list case can judge "newest first" against real creation order.
	created []string
}

// w5 is the one shared chain state of this worker process.
var w5 = &w5State{}

var (
	w5DigestPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	w5RestoreIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

const (
	// w5Poll is the API sampling period of the status waits.
	w5Poll = 250 * time.Millisecond
	// w5PhasePoll is the sampling period of the phase waits: a phase is only a
	// window (the scenario cancels or kills inside it), so it is sampled faster
	// and still far shorter than any real engine phase.
	w5PhasePoll = 100 * time.Millisecond
	// w5QueueSamples is how many consecutive samples must show a blocked row for
	// the mutual-exclusion claim to count as observed rather than inferred.
	w5QueueSamples = 6
	// w5TokenWait is one password-limiter refill: the bucket holds five tokens and
	// adds one every twelve seconds.
	w5TokenWait = 13 * time.Second
	// w5FullBucket is one complete limiter window: after this long of no
	// password work the five-token bucket is certainly full again.
	w5FullBucket = 65 * time.Second
	// w5WrongPassword is unrelated to every generated secret, so a leak of the
	// confirmation body would not be hidden by the real credential's substring.
	w5WrongPassword = "definitely-not-the-admin-password"
)

// w5Chain builds full -> DML -> incremental -> DML -> incremental through the W4
// API, waits for every replica to verify, and records the source snapshot at
// each restore point.
func w5Chain(t *testing.T, f *fixture) {
	t.Helper()
	// The source must already carry substantive data when the full backup is
	// taken, or the "restore of a full backup" proves nothing: the fixture is
	// created with one records row and an empty lsn_pressure table, and the
	// snapshot below requires both to be non-empty. Each round writes one
	// records row and the 1 MiB lsn_pressure page that moves the LSN forward, and
	// every snapshot invariant stays exactly as it was.
	for round := range 5 {
		f.dml(t, round)
	}
	fullID := f.triggerRun(t, f.e2ePolicy, store.RunKindFull, "w5-full")
	w5.full = f.waitRun(t, fullID, store.RunStatusCompleted)
	if w5.full.BackupID == nil {
		t.Fatal("the full run published no backup")
	}
	f.waitReplica(t, *w5.full.BackupID, "verified")
	w5.snapFull = f.w5Snapshot(t)
	f.dml(t, 1)
	w5.inc1 = f.waitRun(t, f.triggerRun(t, f.e2ePolicy, store.RunKindIncremental, "w5-inc-1"), store.RunStatusCompleted)
	if w5.inc1.BackupID == nil {
		t.Fatal("the first incremental published no backup")
	}
	f.waitReplica(t, *w5.inc1.BackupID, "verified")
	w5.snap1 = f.w5Snapshot(t)
	f.dml(t, 2)
	w5.inc2 = f.waitRun(t, f.triggerRun(t, f.e2ePolicy, store.RunKindIncremental, "w5-inc-2"), store.RunStatusCompleted)
	if w5.inc2.BackupID == nil {
		t.Fatal("the second incremental published no backup")
	}
	f.waitReplica(t, *w5.inc2.BackupID, "verified")
	w5.snap2 = f.w5Snapshot(t)
	w5.members = e2eMembers(t, f)
	if w5.members[0].ID != *w5.full.BackupID || w5.members[1].ID != *w5.inc1.BackupID || w5.members[2].ID != *w5.inc2.BackupID {
		t.Fatalf("the chain members are not the backups this scenario published: %+v", w5.members)
	}
}

// w5RequireChain refuses a case whose shared chain is incomplete. The cases run
// in one process and judge what an earlier case produced, so every backup id,
// member and snapshot must really exist before a later case dereferences it;
// without this a half-built chain is a nil pointer, not a finding.
func w5RequireChain(t *testing.T) {
	t.Helper()
	for _, member := range []struct {
		name string
		id   *string
	}{{"full", w5.full.BackupID}, {"inc1", w5.inc1.BackupID}, {"inc2", w5.inc2.BackupID}} {
		if member.id == nil || *member.id == "" {
			t.Fatalf("the shared chain has no %s backup id: the case that publishes it never completed", member.name)
		}
	}
	if len(w5.members) != 3 {
		t.Fatalf("the shared chain has %d members, want the three this scenario published", len(w5.members))
	}
	for _, point := range []w5Snapshot{w5.snapFull, w5.snap1, w5.snap2} {
		if !w5DigestPattern.MatchString(point.digest) {
			t.Fatal("the shared chain is missing a source snapshot")
		}
	}
}

// w5RequirePlans refuses a case whose confirmed plans were never built: a plan
// digest is the execute request's whole precondition.
func w5RequirePlans(t *testing.T, plans ...store.RestorePlan) {
	t.Helper()
	w5RequireChain(t)
	for _, plan := range plans {
		if !w5DigestPattern.MatchString(plan.Digest) || len(plan.Links) == 0 {
			t.Fatalf("no plan was built for %s: the planning case never completed", plan.BackupID)
		}
	}
}

// w5Snapshot digests the live source exactly as the harness digests a restored
// server, and counts the rows the rehearsal queries answer.
func (f *fixture) w5Snapshot(t *testing.T) w5Snapshot {
	t.Helper()
	db := f.w5MySQL(t)
	defer func() { must(t, db.Close()) }()
	hash := sha256.New()
	for _, query := range []string{
		"SELECT id, CAST(value AS CHAR) FROM fixture.records ORDER BY id",
		"SELECT id, SHA2(payload,256) FROM fixture.lsn_pressure ORDER BY id",
	} {
		rows, err := db.QueryContext(t.Context(), query)
		must(t, err)
		for rows.Next() {
			var id int64
			var value string
			must(t, rows.Scan(&id, &value))
			_, err = fmt.Fprintf(hash, "%d:%s\n", id, value)
			must(t, err)
		}
		must(t, rows.Err())
		must(t, rows.Close())
		_, err = hash.Write([]byte{0})
		must(t, err)
	}
	out := w5Snapshot{digest: hex.EncodeToString(hash.Sum(nil))}
	must(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM fixture.records").Scan(&out.records))
	must(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM fixture.lsn_pressure WHERE id > 0").Scan(&out.lsnRows))
	if out.records <= 0 || out.lsnRows <= 0 {
		t.Fatalf("the source carries no substantive data: records=%d lsn_pressure=%d", out.records, out.lsnRows)
	}
	return out
}

// w5MySQL opens the real source over its mounted socket.
func (f *fixture) w5MySQL(t *testing.T) *sql.DB {
	t.Helper()
	cfg := mysqldriver.NewConfig()
	cfg.User = backupUser
	cfg.Passwd = f.secret.source
	cfg.Net = "unix"
	cfg.Addr = socketPath
	cfg.DBName = "fixture"
	db, err := sql.Open("mysql", cfg.FormatDSN())
	must(t, err)
	if pingErr := db.PingContext(t.Context()); pingErr != nil {
		_ = db.Close()
		must(t, pingErr)
	}
	return db
}

// w5DropLocalPayloads removes every chain member's published local object, so a
// plan must fall back to the verified FTP replica for every link.
func (f *fixture) w5DropLocalPayloads(t *testing.T) {
	t.Helper()
	repo, err := local.Open(f.layout.local)
	must(t, err)
	defer func() { must(t, repo.Close()) }()
	for _, member := range w5.members {
		must(t, repo.ReclaimPublished(member.ObjectPath))
	}
	if keys := f.listLocalObjects(t); len(keys) != 0 {
		t.Fatalf("local objects survived the payload removal: %s", strings.Join(keys, ","))
	}
}

// w5FTPNamespace digests the whole remote namespace (every key with its exact
// size). Planning must never change it.
func (f *fixture) w5FTPNamespace(t *testing.T) string {
	t.Helper()
	remote := f.openFTP(t)
	keys, err := remote.List(t.Context(), "")
	must(t, err)
	sort.Strings(keys)
	hash := sha256.New()
	for _, key := range keys {
		size, sizeErr := remote.Size(t.Context(), key)
		must(t, sizeErr)
		_, err = fmt.Fprintf(hash, "%s=%d\n", key, size)
		must(t, err)
	}
	return hex.EncodeToString(hash.Sum(nil))[:16]
}

// w5DropFTPPayloads removes one member's remote payload object. With its local
// object already reclaimed, that member has no readable source left.
func (f *fixture) w5DropFTPPayloads(t *testing.T, member dispatch.BackupView) {
	t.Helper()
	remote := f.openFTP(t)
	must(t, remote.Delete(t.Context(), member.ObjectPath+"/"+objectPayload))
}

// w5ExportEntries lists the configured export root.
func (f *fixture) w5ExportEntries(t *testing.T) []string {
	t.Helper()
	return f.w5ExportListing(t, f.layout.export)
}

// w5ExportListing lists any export root by path, so a check can still read the
// volume after the fixture has been deliberately reconfigured away from it.
func (f *fixture) w5ExportListing(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	must(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func (f *fixture) w5RestoreCount(t *testing.T) int {
	t.Helper()
	return f.sqliteCount(t, `SELECT COUNT(*) FROM restores`)
}

// w5ExportState reads one published export: its mode, its total size and a
// digest of its data-directory listing.
func (f *fixture) w5ExportState(t *testing.T, id string) (mode string, bytes int64, digest string) {
	t.Helper()
	root := filepath.Join(f.layout.export, id)
	info, err := os.Stat(root)
	must(t, err)
	mode = fmt.Sprintf("%04o", info.Mode().Perm())
	data := filepath.Join(root, "data")
	listing, err := os.Stat(data)
	must(t, err)
	if !listing.IsDir() {
		t.Fatalf("export %s has no data directory", id)
	}
	entries, err := os.ReadDir(data)
	must(t, err)
	hash := sha256.New()
	for _, entry := range entries {
		item, infoErr := entry.Info()
		must(t, infoErr)
		bytes += item.Size()
		_, err = fmt.Fprintf(hash, "%s:%d\n", entry.Name(), item.Size())
		must(t, err)
	}
	return mode, bytes, hex.EncodeToString(hash.Sum(nil))[:16]
}

// w5PathExists reports whether one restore-owned path is present.
func w5PathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func (f *fixture) w5WorkDirLive(t *testing.T, id string) bool {
	t.Helper()
	return w5PathExists(filepath.Join(f.layout.work, "restore-"+id))
}

func (f *fixture) w5PendingDirLive(t *testing.T, id string) bool {
	t.Helper()
	return w5PathExists(filepath.Join(f.layout.export, ".pending-"+id))
}

func (f *fixture) w5ExportDirLive(t *testing.T, id string) bool {
	t.Helper()
	return w5PathExists(filepath.Join(f.layout.export, id))
}

// w5Plan posts a plan request and decodes an accepted plan.
func (f *fixture) w5Plan(t *testing.T, backupID string, want int) (store.RestorePlan, []byte) {
	t.Helper()
	body := fmt.Sprintf(`{"backup_id":%q}`, backupID)
	raw := f.admin.request(t, http.MethodPost, "/api/v1/restores/plan", body, want)
	if want != http.StatusOK {
		return store.RestorePlan{}, raw
	}
	var plan store.RestorePlan
	must(t, json.Unmarshal(raw, &plan))
	return plan, raw
}

// w5PlanRefusal requires one refused plan carrying its machine code.
func (f *fixture) w5PlanRefusal(t *testing.T, backupID string, want int, code string) {
	t.Helper()
	_, raw := f.w5Plan(t, backupID, want)
	if !bytesContainCode(raw, code) {
		t.Fatalf("plan refusal for %s did not carry code %q: %s", backupID, code, raw)
	}
}

// bytesContainCode requires one error body to name a fixed machine code.
func bytesContainCode(raw []byte, code string) bool {
	return strings.Contains(string(raw), fmt.Sprintf(`"code":"%s"`, code))
}

type createdRestore struct {
	RestoreID string `json:"restore_id"`
}

// w5Execute posts an execute request and returns the accepted id or the refused
// body. key may be empty.
func (f *fixture) w5Execute(t *testing.T, body, key string, want int) (string, []byte) {
	t.Helper()
	raw := f.admin.requestWithKey(t, http.MethodPost, "/api/v1/restores", body, key, want)
	if want != http.StatusAccepted {
		return "", raw
	}
	var created createdRestore
	must(t, json.Unmarshal(raw, &created))
	if !w5RestoreIDPattern.MatchString(created.RestoreID) {
		t.Fatalf("restore_id %q is not 32 lowercase hex: %s", created.RestoreID, raw)
	}
	w5.recordCreated(created.RestoreID)
	return created.RestoreID, raw
}

// recordCreated remembers the order the scenario created restores in, so the
// list case can judge "newest first" against real creation order instead of
// against the id sort the query already guarantees. An idempotent replay answers
// an id that is already recorded, so it is not a new creation.
func (s *w5State) recordCreated(id string) {
	for _, existing := range s.created {
		if existing == id {
			return
		}
	}
	s.created = append(s.created, id)
}

// w5ExecuteBody builds one confirmation body. verifySQL may be nil.
func w5ExecuteBody(backupID, digest, password string, rehearse bool, verifySQL []string) string {
	return fmt.Sprintf(
		`{"backup_id":%q,"plan_digest":%q,"password":%q,"rehearse":%t,"verify_sql":%s}`,
		backupID, digest, password, rehearse, w5JSONStrings(verifySQL))
}

func w5JSONStrings(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, fmt.Sprintf("%q", value))
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

func (f *fixture) w5Get(t *testing.T, id string) store.Restore {
	t.Helper()
	var restore store.Restore
	must(t, json.Unmarshal(f.operator.request(t, http.MethodGet, "/api/v1/restores/"+id, "", http.StatusOK), &restore))
	return restore
}

func (f *fixture) w5List(t *testing.T, query string) []store.Restore {
	t.Helper()
	path := "/api/v1/restores"
	if query != "" {
		path += "?" + query
	}
	var page w5RestoresPage
	must(t, json.Unmarshal(f.operator.request(t, http.MethodGet, path, "", http.StatusOK), &page))
	return page.Items
}

type w5RestoresPage struct {
	Items []store.Restore `json:"items"`
}

func (f *fixture) w5Cancel(t *testing.T, id string, want int) {
	t.Helper()
	raw := f.admin.request(t, http.MethodPost, "/api/v1/restores/"+id+"/cancel", "", want)
	if want != http.StatusAccepted {
		return
	}
	var accepted createdRestore
	must(t, json.Unmarshal(raw, &accepted))
	if accepted.RestoreID != id {
		t.Fatalf("cancel answered %s for %s", accepted.RestoreID, id)
	}
}

// w5Logs reads the whole paged restore log, ascending.
func (f *fixture) w5Logs(t *testing.T, id string) []store.RestoreLog {
	t.Helper()
	var all []store.RestoreLog
	after := int64(0)
	for {
		var page w5LogsPage
		must(t, json.Unmarshal(f.operator.request(t, http.MethodGet,
			fmt.Sprintf("/api/v1/restores/%s/logs?after=%d&limit=200", id, after), "", http.StatusOK), &page))
		all = append(all, page.Items...)
		if page.NextAfter == nil || *page.NextAfter <= after {
			return all
		}
		after = *page.NextAfter
	}
}

type w5LogsPage struct {
	Items     []store.RestoreLog `json:"items"`
	NextAfter *int64             `json:"next_after"`
	Truncated bool               `json:"truncated"`
}

// w5Wait polls the API until the restore reaches one of the wanted statuses.
func (f *fixture) w5Wait(t *testing.T, id string, statuses ...string) store.Restore {
	t.Helper()
	deadline := time.Now().Add(25 * time.Minute)
	for time.Now().Before(deadline) {
		restore := f.w5Get(t, id)
		if w5HasStatus(statuses, restore.Status) {
			return restore
		}
		time.Sleep(w5Poll)
	}
	t.Fatalf("restore %s never reached %s", id, strings.Join(statuses, "|"))
	return store.Restore{}
}

// w5WaitPhase polls until the running restore reports one of the wanted engine
// phases, so a fault lands while the engine is really inside that stage.
func (f *fixture) w5WaitPhase(t *testing.T, id string, phases ...string) store.Restore {
	t.Helper()
	deadline := time.Now().Add(20 * time.Minute)
	for time.Now().Before(deadline) {
		restore := f.w5Get(t, id)
		if restore.Status != store.RestoreStatusRunning && restore.Status != store.RestoreStatusQueued {
			t.Fatalf("restore %s is %s before reaching phase %s", id, restore.Status, strings.Join(phases, "|"))
		}
		if restore.Phase != nil && w5HasStatus(phases, *restore.Phase) {
			return restore
		}
		// The phase window is the one thing here that can close on its own, so it
		// is sampled several times faster than a status wait.
		time.Sleep(w5PhasePoll)
	}
	t.Fatalf("restore %s never reported phase %s", id, strings.Join(phases, "|"))
	return store.Restore{}
}

func w5HasStatus(wanted []string, value string) bool {
	for _, item := range wanted {
		if item == value {
			return true
		}
	}
	return false
}

// w5PhaseSeen lists the engine phases one restore's log recorded, in order.
func (f *fixture) w5PhaseSeen(t *testing.T, id string) []string {
	t.Helper()
	return w5LogPhases(f.w5Logs(t, id))
}

// w5LastPhase is the last engine phase one restore's log recorded. The row's own
// phase column is NULL once the restore is terminal, so the log is the only
// record of how far the engine really got.
func (f *fixture) w5LastPhase(t *testing.T, id string) string {
	t.Helper()
	phases := f.w5PhaseSeen(t, id)
	if len(phases) == 0 {
		t.Fatalf("restore %s recorded no engine phase", id)
	}
	return phases[len(phases)-1]
}

// w5LogPhases reads the phase sequence straight out of the fixed-format restore
// log messages, so the recorded engine phases are the marker's only source. The
// engine reports fetching and authenticating once per chain link and preparing
// once per prepare step, so the list is the distinct stages in first-occurrence
// order: a repeat is the same stage seen again, not a new stage.
func w5LogPhases(logs []store.RestoreLog) []string {
	const prefix = "restore phase "
	var seen []string
	for _, row := range logs {
		if !strings.HasPrefix(row.Message, prefix) {
			continue
		}
		phase := strings.TrimPrefix(row.Message, prefix)
		if !w5HasStatus(seen, phase) {
			seen = append(seen, phase)
		}
	}
	return seen
}

// w5SQLiteRestore reads the stored outcome of one restore.
func (f *fixture) w5SQLiteRestore(t *testing.T, id string) (status, class, code, effects, started string) {
	t.Helper()
	var classValue, codeValue, effectsValue, startedValue sql.NullString
	var statusValue string
	must(t, f.view(t).QueryRowContext(t.Context(),
		`SELECT status, error_class, error_code, side_effects_json, COALESCE(started_at, '') FROM restores WHERE id = ?`, id,
	).Scan(&statusValue, &classValue, &codeValue, &effectsValue, &startedValue))
	return statusValue, classValue.String, codeValue.String, effectsValue.String, startedValue.String
}

// w5AuditCount counts audit rows for one action and result.
func (f *fixture) w5AuditCount(t *testing.T, action, result string) int {
	t.Helper()
	return f.sqliteCount(t, `SELECT COUNT(*) FROM audit_logs WHERE action = ? AND result = ?`, action, result)
}

// w5AuditFor counts the audit rows one action recorded for one resource id.
func (f *fixture) w5AuditFor(t *testing.T, action, result, resourceID string) int {
	t.Helper()
	return f.sqliteCount(t, `SELECT COUNT(*) FROM audit_logs WHERE action = ? AND result = ? AND resource_id = ?`,
		action, result, resourceID)
}

// w5CrossOrigin posts one mutating request with a foreign Origin.
func (f *fixture) w5CrossOrigin(t *testing.T, path, body string, want int) []byte {
	t.Helper()
	cross := *f.admin
	cross.origin = "https://not-the-console.example"
	return cross.request(t, http.MethodPost, path, body, want)
}

// w5Headed posts one request and returns the response headers with the body, for
// the facts that live only in a header.
func (f *fixture) w5Headed(t *testing.T, body, key string, want int) (http.Header, []byte) {
	t.Helper()
	c := f.admin
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, c.base+"/api/v1/restores", strings.NewReader(body))
	must(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", c.origin)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.http.Do(req)
	must(t, err)
	defer func() { must(t, resp.Body.Close()) }()
	raw, err := io.ReadAll(resp.Body)
	must(t, err)
	if resp.StatusCode != want {
		t.Fatalf("POST /api/v1/restores: status %d want %d body %s", resp.StatusCode, want, raw)
	}
	return resp.Header, raw
}

// fileSHA256Or digests one tool binary, reporting unknown when it cannot be read.
func fileSHA256Or(path string) string {
	sum, err := fileSHA256(path)
	if err != nil {
		return "unknown"
	}
	return sum
}

// w5Append returns a new list with one more name, sorted like w5ExportEntries.
func w5Append(list []string, name string) []string {
	out := append([]string(nil), list...)
	out = append(out, name)
	sort.Strings(out)
	return out
}

// w5EqualLists compares two sorted name lists.
func w5EqualLists(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// w5A2Plan proves the plan API is read-only and complete, that every link falls
// back to the verified FTP replica once no local payload exists, and that the
// refusal codes reach the frozen statuses on the real server.
func w5A2Plan(t *testing.T, f *fixture) {
	w5Chain(t, f)
	f.w5DropLocalPayloads(t)
	namespace := f.w5FTPNamespace(t)
	exportBefore := f.w5ExportEntries(t)
	restoresBefore := f.w5RestoreCount(t)

	w5.planFull = f.w5RequireFTPPlan(t, *w5.full.BackupID, "full", 1, w5.snapFull)
	w5.plan1 = f.w5RequireFTPPlan(t, *w5.inc1.BackupID, "inc1", 2, w5.snap1)
	w5.plan2 = f.w5RequireFTPPlan(t, *w5.inc2.BackupID, "inc2", 3, w5.snap2)

	if got := f.w5FTPNamespace(t); got != namespace {
		t.Fatalf("planning wrote the FTP namespace: before=%s after=%s", namespace, got)
	}
	if entries := f.w5ExportEntries(t); !w5EqualLists(entries, exportBefore) {
		t.Fatalf("planning wrote the export root: before=%s after=%s", exportBefore, entries)
	}
	if count := f.w5RestoreCount(t); count != restoresBefore {
		t.Fatalf("planning wrote restore rows: before=%d after=%d", restoresBefore, count)
	}
	if left := f.pendingDirs(); len(left) != 0 {
		t.Fatalf("planning left staging directories: %s", left)
	}
	f.assertNoSecret(t, nil)
	f.w5PlanRefusal(t, "00000000-0000-7000-8000-0000000000ff", http.StatusNotFound, dispatch.RestoreCodeBackupNotFound)
	f.w5UnconfiguredRefusal(t)

	t.Logf("W5-A2 plan target=inc2 links=ftp:3 digest=%s lsn=%d required_export=%d available_export=%d export_path=%s rehearsal=true risks=%s",
		w5.plan2.Digest, w5.plan2.TargetToLSN, w5.plan2.RequiredExportBytes, w5.plan2.AvailableExportBytes,
		w5.plan2.ExportPath, strings.Join(w5.plan2.Risks, ","))
	t.Logf("W5-A2 plan_readonly restores=0 export_dirs=%d ftp_namespace=%s ftp_writes=0 local_payloads=removed",
		len(exportBefore), namespace)
	t.Logf("W5-A2 refused backup_not_found=404 unconfigured=503")
}

// w5RequireFTPPlan requires every plan fact for one target: the chain links,
// the identity and rehearsal view, and the space estimate against the real
// filesystem. Each facet is judged by its own requirement so a failure names the
// fact that broke rather than the whole plan.
func (f *fixture) w5RequireFTPPlan(t *testing.T, backupID, label string, wantLinks int,
	point w5Snapshot,
) store.RestorePlan {
	t.Helper()
	// The plan reports live free space, so the readings that judge it bracket the
	// request itself: the volumes are shared with other writers and free space
	// moves between the engine's Statfs and any single later reading.
	before := f.w5SpaceNow(t)
	plan, _ := f.w5Plan(t, backupID, http.StatusOK)
	window := w5SpaceWindow{before: before, after: f.w5SpaceNow(t)}
	f.w5RequirePlanLinks(t, label, plan, wantLinks, backupID)
	f.w5RequirePlanIdentity(t, label, plan)
	f.w5RequirePlanSpace(t, label, plan, window)
	if point.digest == "" || !w5DigestPattern.MatchString(point.digest) {
		t.Fatalf("no source snapshot was recorded before planning %s", label)
	}
	return plan
}

// w5RequirePlanLinks requires the exact member count, every link served by the
// verified FTP replica, and LSN continuity up to the target recorded in SQLite.
func (f *fixture) w5RequirePlanLinks(t *testing.T, label string, plan store.RestorePlan,
	wantLinks int, backupID string,
) {
	t.Helper()
	if plan.BackupID != backupID {
		t.Fatalf("plan %s target = %s", label, plan.BackupID)
	}
	if len(plan.Links) != wantLinks {
		t.Fatalf("plan %s links = %d, want %d", label, len(plan.Links), wantLinks)
	}
	for index, link := range plan.Links {
		f.w5RequirePlanLink(t, label, plan, index, link)
	}
	last := plan.Links[wantLinks-1]
	if plan.TargetToLSN != last.ToLSN || plan.TargetToLSN == 0 {
		t.Fatalf("plan %s target_to_lsn = %d, last link %d", label, plan.TargetToLSN, last.ToLSN)
	}
	if stored := f.w5Int64(t, `SELECT to_lsn FROM backups WHERE id = ?`, backupID); uint64(stored) != plan.TargetToLSN {
		t.Fatalf("plan %s target_to_lsn = %d, SQLite says %d", label, plan.TargetToLSN, stored)
	}
}

// w5RequirePlanLink requires one chain member: its source, its replica, its
// position, the authenticated member sizes and the LSN hand-off from its parent.
func (f *fixture) w5RequirePlanLink(t *testing.T, label string, plan store.RestorePlan,
	index int, link store.RestorePlanLink,
) {
	t.Helper()
	if link.Source != "ftp" {
		t.Fatalf("plan %s link %d source = %s, want ftp", label, index, link.Source)
	}
	if link.RepositoryID != f.ftpID {
		t.Fatalf("plan %s link %d repository = %s, want %s", label, index, link.RepositoryID, f.ftpID)
	}
	if link.Seq != int64(index+1) {
		t.Fatalf("plan %s link %d seq = %d", label, index, link.Seq)
	}
	if link.PayloadSize <= 0 || link.MetadataSize <= 0 {
		t.Fatalf("plan %s link %d sizes = payload %d metadata %d", label, index, link.PayloadSize, link.MetadataSize)
	}
	if index > 0 && link.FromLSN != plan.Links[index-1].ToLSN {
		t.Fatalf("plan %s is LSN-discontinuous at link %d", label, index)
	}
}

// w5RequirePlanIdentity requires the plan to name the stored chain and source,
// the real candidate versions, the rehearsal this deployment can run, the fixed
// advisory risks and the export location built from the restore id.
func (f *fixture) w5RequirePlanIdentity(t *testing.T, label string, plan store.RestorePlan) {
	t.Helper()
	if !w5DigestPattern.MatchString(plan.Digest) {
		t.Fatalf("plan %s digest = %q", label, plan.Digest)
	}
	if plan.ChainID != f.w5String(t, `SELECT chain_id FROM backups WHERE id = ?`, plan.BackupID) {
		t.Fatalf("plan %s chain = %s, the stored backup says otherwise", label, plan.ChainID)
	}
	if plan.SourceID != f.sourceID {
		t.Fatalf("plan %s source = %s, want %s", label, plan.SourceID, f.sourceID)
	}
	if plan.ServerVersion != "8.4.11" || !strings.HasPrefix(plan.ToolVersion, "8.4.0-7") {
		t.Fatalf("plan %s versions = %s/%s", label, plan.ServerVersion, plan.ToolVersion)
	}
	if !plan.RehearsalAvailable {
		t.Fatalf("plan %s reports no rehearsal although mysqld is configured", label)
	}
	w5RequirePlanRisks(t, label, strings.Join(plan.Risks, ","))
	if want := filepath.Join(f.layout.export, "{restore_id}"); plan.ExportPath != want {
		t.Fatalf("plan %s export_path = %s, want %s", label, plan.ExportPath, want)
	}
}

// w5RequirePlanRisks requires the fixed advisory codes every plan carries, and
// forbids the rehearsal warning while mysqld is configured.
func w5RequirePlanRisks(t *testing.T, label, risks string) {
	t.Helper()
	for _, risk := range w5FixedRisks {
		if !strings.Contains(risks, risk) {
			t.Fatalf("plan %s omits risk %s: %s", label, risk, risks)
		}
	}
	if strings.Contains(risks, "rehearsal_unavailable") {
		t.Fatalf("plan %s reports the rehearsal unavailable while mysqld is configured", label)
	}
}

// w5SpacePair is one live observation of the two volumes a plan reports on.
type w5SpacePair struct{ work, export int64 }

// w5SpaceWindow is the pair of observations that brackets one plan request.
type w5SpaceWindow struct {
	before, after w5SpacePair
}

// w5SpaceSlack is the drift one accepts between the engine's Statfs and the
// test's own readings of the same volume, in bytes. It is a fixed bound and not
// a proportion: another writer on the shared CI volume shifted free space by
// 12 KiB inside one plan request, while 1% of a 140 GiB runner would be 1.4 GiB
// and would accept a figure borrowed from a much larger volume.
const w5SpaceSlack = int64(16 << 20)

// w5SpaceNow reads the real free space of both restore volumes.
func (f *fixture) w5SpaceNow(t *testing.T) w5SpacePair {
	t.Helper()
	work, err := physical.AvailableBytes(f.layout.work)
	must(t, err)
	export, err := physical.AvailableBytes(f.layout.export)
	must(t, err)
	return w5SpacePair{work: work, export: export}
}

// w5RequirePlanSpace requires the estimate to be the recorded member sizes times
// this deployment's expansion factor, and both reported free-space figures to be
// real observations of these volumes: positive, fitting the requirement, and
// inside the Statfs window the request ran in.
func (f *fixture) w5RequirePlanSpace(t *testing.T, label string, plan store.RestorePlan, window w5SpaceWindow) {
	t.Helper()
	if plan.RequiredWorkBytes <= 0 || plan.RequiredExportBytes <= 0 {
		t.Fatalf("plan %s estimate = work %d export %d", label, plan.RequiredWorkBytes, plan.RequiredExportBytes)
	}
	if plan.AvailableWorkBytes < plan.RequiredWorkBytes || plan.AvailableExportBytes < plan.RequiredExportBytes {
		t.Fatalf("plan %s does not fit: required %d/%d available %d/%d", label,
			plan.RequiredWorkBytes, plan.RequiredExportBytes, plan.AvailableWorkBytes, plan.AvailableExportBytes)
	}
	w5RequirePlanCapacity(t, label, "work", plan.AvailableWorkBytes, window.before.work, window.after.work)
	w5RequirePlanCapacity(t, label, "export", plan.AvailableExportBytes, window.before.export, window.after.export)
	total := int64(0)
	for _, link := range plan.Links {
		total += link.PayloadSize
	}
	if plan.RequiredExportBytes != total*int64(f.settings.RestoreExpansionFactor) {
		t.Fatalf("plan %s required_export = %d, want %d x %d", label,
			plan.RequiredExportBytes, total, f.settings.RestoreExpansionFactor)
	}
}

// w5RequirePlanCapacity requires one reported free-space figure to be an
// observation of this volume, not a constant: it must be positive and lie within
// the bracketed readings widened by the fixed drift tolerance. A zero, a
// negative, a fabricated or a borrowed value is still refused in both directions.
func w5RequirePlanCapacity(t *testing.T, label, kind string, planned, before, after int64) {
	t.Helper()
	low, high := before, after
	if low > high {
		low, high = high, low
	}
	if planned <= 0 || planned < low-w5SpaceSlack || planned > high+w5SpaceSlack {
		t.Fatalf("plan %s available_%s = %d, outside the live Statfs window %d..%d (slack %d)",
			label, kind, planned, low, high, w5SpaceSlack)
	}
}

// w5FixedRisks are the advisory risk codes every plan must carry when the
// deployment can rehearse.
var w5FixedRisks = []string{"no_auto_replace_production", "server_uuid_duplicate", "pitr_unavailable"}

func (f *fixture) w5Int64(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var value int64
	must(t, f.view(t).QueryRowContext(t.Context(), query, args...).Scan(&value))
	return value
}

func (f *fixture) w5String(t *testing.T, query string, args ...any) string {
	t.Helper()
	var value sql.NullString
	must(t, f.view(t).QueryRowContext(t.Context(), query, args...).Scan(&value))
	return value.String
}

// w5UnconfiguredRefusal restarts the dispatcher without a restore engine, which
// is exactly a deployment missing a restore input, and requires both write routes
// to answer 503 while the read routes keep serving.
func (f *fixture) w5UnconfiguredRefusal(t *testing.T) {
	t.Helper()
	xbstream := f.settings.XbstreamPath
	f.settings.XbstreamPath = ""
	f.startDispatch(t)
	f.w5PlanRefusal(t, *w5.inc2.BackupID, http.StatusServiceUnavailable, render.Unavailable)
	body := w5ExecuteBody(*w5.inc2.BackupID, w5.plan2.Digest, f.secret.admin, false, nil)
	refused := f.admin.requestWithKey(t, http.MethodPost, "/api/v1/restores", body, "w5-unconfigured",
		http.StatusServiceUnavailable)
	if !bytesContainCode(refused, render.Unavailable) {
		t.Fatalf("unconfigured execute body = %s", refused)
	}
	if items := f.w5List(t, "status=completed"); len(items) != 0 {
		t.Fatalf("an unconfigured deployment must still answer restores reads, got %d rows", len(items))
	}
	f.settings.XbstreamPath = xbstream
	f.startDispatch(t)
}

// w5A6E2E restores both incremental points over the HTTP API with password
// re-entry, and reads every result back from the API, SQLite, the export volume,
// the restore log and the rehearsal's own answers.
func w5A6E2E(t *testing.T, f *fixture) {
	w5RequirePlans(t, w5.plan1, w5.plan2)
	inc1 := f.w5RunRestore(t, *w5.inc1.BackupID, w5.plan1, "w5-e2e-inc1", true,
		[]string{"SELECT COUNT(*) FROM fixture.records", "SELECT COUNT(*) FROM fixture.lsn_pressure WHERE id > 0"})
	// The confirmation answers 202 while the row is still queued: the rehearsal
	// facts are only written by the terminal transition, so the completed status
	// is awaited before any result assertion judges them.
	f.w5Wait(t, inc1, store.RestoreStatusCompleted)
	f.w5RequireRehearsal(t, inc1, w5.snap1)
	exported1 := f.w5Completed(t, inc1, w5.plan1, 2, w5.exported)
	w5.exported = w5Append(w5.exported, exported1)

	inc2 := f.w5RunRestore(t, *w5.inc2.BackupID, w5.plan2, "w5-e2e-inc2", false, nil)
	exported2 := f.w5Completed(t, inc2, w5.plan2, 3, w5.exported)
	w5.exported = w5Append(w5.exported, exported2)
	if got := f.w5Get(t, inc2).VerifyStatus; got != store.RestoreVerifyNotRequested {
		t.Fatalf("a restore without a rehearsal reports verify_status = %s", got)
	}

	logs := f.w5Logs(t, inc1)
	if rows := f.sqliteCount(t, `SELECT COUNT(*) FROM restore_logs WHERE restore_id = ?`, inc1); rows != len(logs) {
		t.Fatalf("restore logs: API returned %d, SQLite stores %d", len(logs), rows)
	}
	f.assertNoSecret(t, w5RawJSON(t, logs))
	t.Logf("W5-A6 e2e chain=seq:1,2,3 inc1=%s inc2=%s replicas_verified=3 local_payloads=removed logs_agree=sqlite log_rows=%d phase_seen=%s",
		*w5.inc1.BackupID, *w5.inc2.BackupID, len(logs), strings.Join(w5LogPhases(logs), ","))
	t.Logf("W5-A6 restore=inc1 source=ftp export=%s expected_snapshot=%s verify_status=passed", inc1, w5.snap1.digest)
	t.Logf("W5-A6 restore=inc2 source=ftp export=%s expected_snapshot=%s verify_status=not_requested", inc2, w5.snap2.digest)
}

// w5RunRestore confirms one restore with the administrator password.
func (f *fixture) w5RunRestore(t *testing.T, backupID string, plan store.RestorePlan,
	key string, rehearse bool, verifySQL []string,
) string {
	t.Helper()
	// Every confirmation spends one password-limiter token, so the restores this
	// scenario starts are spaced by one refill: the limiter can then never be the
	// thing that fails an unrelated assertion.
	time.Sleep(w5TokenWait)
	return f.w5RunRestoreNow(t, backupID, plan, key, rehearse, verifySQL)
}

// w5RunRestoreNow confirms one restore without waiting for a limiter token: the
// caller already did that. It exists for the case whose request must land inside
// a live window (a running backup holds the execution slot), where waiting here
// would spend the window instead of the token.
func (f *fixture) w5RunRestoreNow(t *testing.T, backupID string, plan store.RestorePlan,
	key string, rehearse bool, verifySQL []string,
) string {
	t.Helper()
	id, _ := f.w5Execute(t, w5ExecuteBody(backupID, plan.Digest, f.secret.admin, rehearse, verifySQL), key,
		http.StatusAccepted)
	return id
}

// w5Completed requires every persisted fact of a completed restore and returns
// the export directory name it published.
func (f *fixture) w5Completed(t *testing.T, id string, plan store.RestorePlan, links int,
	published []string,
) string {
	t.Helper()
	restore := f.w5Wait(t, id, store.RestoreStatusCompleted)
	f.w5RequireCompletedRow(t, id, restore, plan, links)
	mode, bytes, digest := f.w5RequireCompletedExport(t, id, restore, published, links)
	f.w5RequireCompletedStored(t, id, restore)
	t.Logf("W5-A6 export=%s bytes=%d data_digest=%s phase=%s links=ftp:%d mode=%s",
		id, bytes, digest, f.w5LastPhase(t, id), links, mode)
	return id
}

// w5RequireCompletedRow requires the API row to name the confirmed plan, the
// target, the requester, the work directory it used and no failure of its own.
func (f *fixture) w5RequireCompletedRow(t *testing.T, id string, restore store.Restore,
	plan store.RestorePlan, links int,
) {
	t.Helper()
	if restore.ExportPath == nil || *restore.ExportPath != filepath.Join(f.layout.export, id) {
		t.Fatalf("export_path = %v, want %s", restore.ExportPath, filepath.Join(f.layout.export, id))
	}
	if restore.ExportBytes == nil || *restore.ExportBytes <= 0 {
		t.Fatalf("export_bytes = %v", restore.ExportBytes)
	}
	if restore.WorkDir == nil || *restore.WorkDir != filepath.Join(f.layout.work, "restore-"+id) {
		t.Fatalf("work_dir = %v", restore.WorkDir)
	}
	f.w5RequireStoredPlanLinks(t, restore, links)
	w5RequireConfirmedTarget(t, restore, plan)
}

// w5RequireConfirmedTarget requires the stored row to be exactly the plan the
// administrator confirmed, requested by a named user, and to carry no failure of
// its own.
func w5RequireConfirmedTarget(t *testing.T, restore store.Restore, plan store.RestorePlan) {
	t.Helper()
	if restore.PlanDigest != plan.Digest || restore.Plan.Digest != plan.Digest {
		t.Fatalf("the persisted plan digest drifted: row %s confirmed %s", restore.PlanDigest, plan.Digest)
	}
	if restore.BackupID != plan.BackupID {
		t.Fatalf("restore target = %s, want %s", restore.BackupID, plan.BackupID)
	}
	if restore.RequestedBy == "" {
		t.Fatal("the restore row records no requester")
	}
	if restore.ErrorClass != nil || restore.ErrorCode != nil {
		t.Fatalf("a completed restore carries error_class=%v error_code=%v", restore.ErrorClass, restore.ErrorCode)
	}
}

// w5RequireStoredPlanLinks requires the persisted plan snapshot to carry exactly
// the confirmed chain, every link read from the verified FTP replica.
func (f *fixture) w5RequireStoredPlanLinks(t *testing.T, restore store.Restore, links int) {
	t.Helper()
	if len(restore.Plan.Links) != links {
		t.Fatalf("the stored plan has %d links, want %d", len(restore.Plan.Links), links)
	}
	for _, link := range restore.Plan.Links {
		if link.Source != "ftp" {
			t.Fatalf("the stored plan link seq %d came from %s", link.Seq, link.Source)
		}
	}
}

// w5RequireCompletedExport requires the published directory itself: its mode, the
// exact set of exports the root holds, and the engine phases the log recorded.
func (f *fixture) w5RequireCompletedExport(t *testing.T, id string, restore store.Restore,
	published []string, links int,
) (mode string, bytes int64, digest string) {
	t.Helper()
	if f.w5WorkDirLive(t, id) {
		t.Fatal("the work directory of a completed restore still exists")
	}
	if f.w5PendingDirLive(t, id) {
		t.Fatal("the pending export of a completed restore still exists")
	}
	mode, _, digest = f.w5ExportState(t, id)
	if mode != "0750" {
		t.Fatalf("export directory mode = %s, want 0750", mode)
	}
	want := w5Append(published, id)
	if entries := f.w5ExportEntries(t); !w5EqualLists(entries, want) {
		t.Fatalf("export root = %s, want %s", entries, want)
	}
	f.w5RequireCompletedPhases(t, id, restore)
	if restore.ExportBytes == nil {
		t.Fatal("a completed restore reports no export size")
	}
	return mode, *restore.ExportBytes, digest
}

// w5RequireCompletedPhases requires the completed restore's engine phases. The
// row carries no live phase once it is terminal (every terminal write nulls the
// column), so the sequence and the stage the engine finished in are read from
// the restore log: a rehearsed restore ends at rehearsing, one without a
// rehearsal ends at exporting.
func (f *fixture) w5RequireCompletedPhases(t *testing.T, id string, restore store.Restore) {
	t.Helper()
	seen := f.w5PhaseSeen(t, id)
	for _, phase := range w5CompletedPhases {
		if !w5HasStatus(seen, phase) {
			t.Fatalf("restore %s never recorded phase %s: %s", id, phase, strings.Join(seen, ","))
		}
	}
	if restore.Phase != nil {
		t.Fatalf("a completed restore still reports a live phase %q", *restore.Phase)
	}
	wantLast := "exporting"
	if restore.Rehearse {
		wantLast = "rehearsing"
	}
	if last := f.w5LastPhase(t, id); last != wantLast {
		t.Fatalf("the completed restore's log ends at phase %s, want %s", last, wantLast)
	}
}

// w5RequireCompletedStored requires the persisted row and its explicit side
// effects to agree with the API view.
func (f *fixture) w5RequireCompletedStored(t *testing.T, id string, restore store.Restore) {
	t.Helper()
	if restore.SideEffects == nil || !restore.SideEffects.WorkDirRemoved ||
		!restore.SideEffects.PendingExportRemoved || !restore.SideEffects.ExportPublished {
		t.Fatalf("side_effects = %+v", restore.SideEffects)
	}
	stored, class, code, effects, started := f.w5SQLiteRestore(t, id)
	if stored != store.RestoreStatusCompleted || class != "" || code != "" {
		t.Fatalf("SQLite row = %s class=%q code=%q", stored, class, code)
	}
	if !strings.Contains(effects, `"export_published":true`) {
		t.Fatalf("SQLite side_effects = %s", effects)
	}
	if started == "" {
		t.Fatal("a completed restore has no started_at")
	}
}

// w5CompletedPhases are the engine stages every completed restore must have
// recorded, in order, from the API's own restore log.
var w5CompletedPhases = []string{"fetching", "authenticating", "extracting", "preparing", "exporting"}

// w5RequireRehearsal requires the rehearsal result the API reports: the instance
// really started, the user schema is counted, every CHECK TABLE is clean, and the
// requested read-only queries answered with the source's own counts.
func (f *fixture) w5RequireRehearsal(t *testing.T, id string, point w5Snapshot) {
	t.Helper()
	restore := f.w5Get(t, id)
	if restore.VerifyStatus != store.RestoreVerifyPassed {
		t.Fatalf("verify_status = %s, want passed", restore.VerifyStatus)
	}
	if restore.VerifyResult == nil {
		t.Fatal("a passed rehearsal stored no result")
	}
	raw := w5RawJSON(t, restore.VerifyResult)
	var verify w5Verify
	must(t, json.Unmarshal(raw, &verify))
	if verify.Status != "passed" || !verify.Started || !verify.SystemTablesReadable {
		t.Fatalf("rehearsal = %+v", verify)
	}
	f.w5RequireRehearsalSchema(t, verify, point)
	f.w5RequireRehearsalQueries(t, verify, point)
	if strings.Contains(string(raw), f.secret.source) || strings.Contains(string(raw), f.secret.admin) {
		t.Fatal("the rehearsal result carried a credential")
	}
}

// w5RequireRehearsalSchema requires the restored instance to expose the user
// schema with its tables and rows, and every InnoDB table to pass CHECK TABLE.
func (f *fixture) w5RequireRehearsalSchema(t *testing.T, verify w5Verify, point w5Snapshot) {
	t.Helper()
	tables := 0
	for _, schema := range verify.Schemas {
		if schema.Name != "fixture" {
			continue
		}
		tables = schema.Tables
		// The export is an immutable snapshot, so the count is exact: the user
		// schema holds the records rows plus the lsn_pressure rows the snapshot
		// recorded at that restore point, nothing more and nothing less.
		if want := point.records + point.lsnRows; schema.Rows != want {
			t.Fatalf("the rehearsal counted %d rows, want %d (%d records + %d lsn_pressure)",
				schema.Rows, want, point.records, point.lsnRows)
		}
	}
	if tables != 2 {
		t.Fatalf("the rehearsal saw %d tables in the user schema, want 2", tables)
	}
	if len(verify.CheckTable) < 2 {
		t.Fatalf("CHECK TABLE covered %d tables", len(verify.CheckTable))
	}
	for _, check := range verify.CheckTable {
		if !strings.EqualFold(check.Status, "OK") {
			t.Fatalf("CHECK TABLE %s = %s", check.Table, check.Status)
		}
	}
}

// w5RequireRehearsalQueries requires each requested read-only statement to be
// answered exactly as the source looked at that restore point.
func (f *fixture) w5RequireRehearsalQueries(t *testing.T, verify w5Verify, point w5Snapshot) {
	t.Helper()
	if len(verify.Queries) != 2 {
		t.Fatalf("the rehearsal answered %d queries, want 2", len(verify.Queries))
	}
	want := []string{fmt.Sprintf("%d", point.records), fmt.Sprintf("%d", point.lsnRows)}
	for index, query := range verify.Queries {
		if query.ErrorCode != "" {
			t.Fatalf("rehearsal query %d failed: %s", index, query.SQL)
		}
		if got := w5FirstCell(query); got != want[index] {
			t.Fatalf("restored query %d = %s, the source had %s at that point", index, got, want[index])
		}
	}
}

type w5Verify struct {
	Status               string `json:"status"`
	Started              bool   `json:"started"`
	SystemTablesReadable bool   `json:"system_tables_readable"`
	Schemas              []struct {
		Name   string `json:"name"`
		Tables int    `json:"tables"`
		Rows   int64  `json:"rows"`
	} `json:"schemas"`
	CheckTable []struct {
		Table  string `json:"table"`
		Status string `json:"status"`
	} `json:"check_table"`
	Queries []struct {
		SQL       string     `json:"sql"`
		Columns   []string   `json:"columns"`
		Rows      [][]string `json:"rows"`
		ErrorCode string     `json:"error_code"`
	} `json:"queries"`
}

func w5FirstCell(query struct {
	SQL       string     `json:"sql"`
	Columns   []string   `json:"columns"`
	Rows      [][]string `json:"rows"`
	ErrorCode string     `json:"error_code"`
},
) string {
	if len(query.Rows) != 1 || len(query.Rows[0]) != 1 {
		return ""
	}
	return query.Rows[0][0]
}

func w5RawJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	must(t, err)
	return raw
}

// w5A3Execute proves the execute gate on the real server: the permission split,
// the CSRF rejection, password re-entry with its failure audit and limiter, the
// stale-digest refusal, the idempotency contract, and the atomic commit of the
// row with its success audit. It then restores the full backup through the same
// route so the export is compared in a brand-new server.
func w5A3Execute(t *testing.T, f *fixture) {
	w5RequirePlans(t, w5.planFull)
	adminBody := w5ExecuteBody(*w5.full.BackupID, w5.planFull.Digest, f.secret.admin, false, nil)
	// The operator's own credential is correct for its account: this request can
	// only fail because restores:execute belongs to administrators.
	operatorBody := w5ExecuteBody(*w5.full.BackupID, w5.planFull.Digest, f.secret.operator, false, nil)
	restoresBefore := f.w5RestoreCount(t)

	// An operator may plan but never execute: the permission middleware answers
	// before any password work, so no limiter token is spent.
	forbidden := f.operator.requestWithKey(t, http.MethodPost, "/api/v1/restores", operatorBody, "w5-a3-operator",
		http.StatusForbidden)
	if !bytesContainCode(forbidden, render.Forbidden) {
		t.Fatalf("operator execute = %s", forbidden)
	}
	cross := f.w5CrossOrigin(t, "/api/v1/restores", adminBody, http.StatusForbidden)
	if !bytesContainCode(cross, render.CrossOriginRejected) {
		t.Fatalf("cross-origin execute = %s", cross)
	}

	denied, after := f.w5RequireConfirmationLimiter(adminBody)

	// A stale or borrowed digest is refused before any row exists.
	time.Sleep(w5TokenWait)
	f.w5RequirePlanChanged(t, restoresBefore)

	// The confirmed create commits the row and its success audit together.
	time.Sleep(w5TokenWait)
	key := "w5-a3-create"
	created, _ := f.w5Execute(t, adminBody, key, http.StatusAccepted)
	restores := f.w5RestoreCount(t)
	if restores != restoresBefore+1 {
		t.Fatalf("the created restore did not store exactly one row: before=%d after=%d", restoresBefore, restores)
	}
	if audit := f.w5AuditFor(t, "restores.execute", "success", created); audit != 1 {
		t.Fatalf("the created restore has %d success audit rows named by id, want 1", audit)
	}
	if status, _, _, _, started := f.w5SQLiteRestore(t, created); status == "" || (status == store.RestoreStatusRunning && started == "") {
		t.Fatalf("the created restore row is %q with started_at %q", status, started)
	}
	// Replay answers the stored id; a different request with the same key is a
	// conflict. Both still cost a confirmation token.
	conflict := f.w5RequireIdempotency(adminBody, key, created, restores)

	// The confirmed restore publishes its export; the harness starts a brand-new
	// server on it and compares the data.
	completed := f.w5Completed(t, created, w5.planFull, 1, w5.exported)
	w5.exported = w5Append(w5.exported, completed)
	f.assertNoSecret(t, conflict)
	t.Logf("W5-A3 execute admin_only=true cross_origin=%s confirm=403,%s no_row=true audit_denied=%d limiter=429 retry_after=%s digest_changed=409,%s replay_same_id=true conflict=409 audit_atomic=true",
		render.CrossOriginRejected, render.ConfirmationFailed, denied, after, render.PlanChanged)
	t.Logf("W5-A3 restore=full source=ftp export=%s expected_snapshot=%s verify_status=not_requested", created, w5.snapFull.digest)
}

// w5RequireIdempotency replays the stored request and requires the same id with
// no extra row, then refuses a different request carrying the same key. It
// returns the conflict body for the caller's secret scan.
func (f *fixture) w5RequireIdempotency(adminBody, key, created string, restores int) []byte {
	t := f.t
	t.Helper()
	time.Sleep(w5TokenWait)
	replay, _ := f.w5Execute(t, adminBody, key, http.StatusAccepted)
	if replay != created {
		t.Fatalf("replay answered %s, want the stored %s", replay, created)
	}
	if count := f.w5RestoreCount(t); count != restores {
		t.Fatalf("a replay created another row: %d -> %d", restores, count)
	}
	time.Sleep(w5TokenWait)
	conflictBody := w5ExecuteBody(*w5.full.BackupID, w5.planFull.Digest, f.secret.admin, true, nil)
	conflict := f.admin.requestWithKey(t, http.MethodPost, "/api/v1/restores", conflictBody, key, http.StatusConflict)
	if !bytesContainCode(conflict, render.Conflict) {
		t.Fatalf("idempotency conflict = %s", conflict)
	}
	return conflict
}

// w5RequirePlanChanged requires a borrowed plan digest to be refused with the
// fixed code and to leave no restore row behind.
func (f *fixture) w5RequirePlanChanged(t *testing.T, restoresBefore int) {
	t.Helper()
	stale := w5ExecuteBody(*w5.inc1.BackupID, w5.plan2.Digest, f.secret.admin, false, nil)
	changed := f.admin.requestWithKey(t, http.MethodPost, "/api/v1/restores", stale, "w5-a3-stale", http.StatusConflict)
	if !bytesContainCode(changed, render.PlanChanged) {
		t.Fatalf("stale digest = %s", changed)
	}
	if count := f.w5RestoreCount(t); count != restoresBefore {
		t.Fatalf("a plan_changed request created rows: before=%d after=%d", restoresBefore, count)
	}
}

// w5RequireConfirmationLimiter proves the password re-entry gate on the real
// server: five attempts per user per minute, every confirmation spending a token
// whether or not the password matched, then a refused 429 carrying Retry-After,
// with a failed audit for each denial and no restore row created at all. It
// returns the observed denial count and the Retry-After the server chose.
func (f *fixture) w5RequireConfirmationLimiter(validBody string) (denied int, retryAfter string) {
	f.t.Helper()
	restoresBefore := f.w5RestoreCount(f.t)
	deniedBefore := f.w5AuditCount(f.t, "restores.execute", "denied")
	// Waiting a full window first makes the five-then-refused count exact rather
	// than a function of what earlier scenarios spent.
	time.Sleep(w5FullBucket)
	wrong := strings.Replace(validBody, fmt.Sprintf("%q", f.secret.admin), fmt.Sprintf("%q", w5WrongPassword), 1)
	for attempt := 1; attempt <= 5; attempt++ {
		refused := f.admin.requestWithKey(f.t, http.MethodPost, "/api/v1/restores", wrong, "", http.StatusForbidden)
		if !bytesContainCode(refused, render.ConfirmationFailed) {
			f.t.Fatalf("wrong password attempt %d = %s", attempt, refused)
		}
	}
	if count := f.w5RestoreCount(f.t); count != restoresBefore {
		f.t.Fatalf("failed confirmations created %d restore rows", count-restoresBefore)
	}
	limits, limited := f.w5Headed(f.t, wrong, "", http.StatusTooManyRequests)
	retryAfter = limits.Get("Retry-After")
	if retryAfter == "" || retryAfter == "0" {
		f.t.Fatalf("429 carried Retry-After %q", retryAfter)
	}
	if !bytesContainCode(limited, render.RateLimited) {
		f.t.Fatalf("limiter body = %s", limited)
	}
	denied = f.w5AuditCount(f.t, "restores.execute", "denied") - deniedBefore
	if denied < 6 {
		f.t.Fatalf("the failed confirmations recorded %d denied audit rows, want at least 6", denied)
	}
	return denied, retryAfter
}

// w5A4Exclusion proves the two queues share one execution slot in both
// directions: a queued backup run is not claimed while a restore is running, and
// a queued restore is not claimed while a backup run is running. running_max is
// sampled from the API and cross-checked against the stored intervals.
func w5A4Exclusion(t *testing.T, f *fixture) {
	w5RequirePlans(t, w5.planFull)
	policy := f.createPolicy(t, "w5-exclusion", 8)
	f.enable(t, policy)

	// Direction one: a running restore holds the slot, so a queued backup run is
	// not claimed until the restore releases it.
	blocker := f.w5RunRestore(t, *w5.full.BackupID, w5.planFull, "w5-a4-restore", false, nil)
	// The restore must really hold the slot before the run is queued, so the
	// observation cannot pass because the run happened to be claimed first.
	f.w5Wait(t, blocker, store.RestoreStatusRunning)
	runID := f.triggerRun(t, policy, store.RunKindFull, "w5-a4-run")
	blockedRun := f.w5Observe(t, "run", runID, "restore", blocker)
	if blockedRun.violated {
		t.Fatal("a backup run was claimed while a restore was running")
	}
	f.w5Cancel(t, blocker, http.StatusAccepted)
	f.w5Wait(t, blocker, store.RestoreStatusCanceled)
	released := f.waitRun(t, runID, store.RunStatusCompleted)
	if ptrString(released.StartedAt) == "" {
		t.Fatal("the run was never claimed after the restore released the slot")
	}

	// Direction two: a running backup run holds the slot, so a queued restore is
	// not claimed. The run is proven to be really executing by the live
	// xtrabackup process of its own work directory.
	// The confirmation spends one limiter token and every spend is spaced by one
	// refill. That spacing happens BEFORE the run starts: waiting after it would
	// spend the contention window instead, and the execute request could land
	// once the run had already released the slot.
	time.Sleep(w5TokenWait)
	second := f.triggerRun(t, policy, store.RunKindFull, "w5-a4-run-2")
	backup := f.waitXtrabackup(t, second)
	f.requireBackupAlive(t, backup)
	queued := f.w5RunRestoreNow(t, *w5.full.BackupID, w5.planFull, "w5-a4-queued", false, nil)
	blockedRestore := f.w5Observe(t, "restore", queued, "run", second)
	if blockedRestore.violated {
		t.Fatal("a restore was claimed while a backup run was running")
	}
	f.w5Cancel(t, queued, http.StatusAccepted)
	f.w5Wait(t, queued, store.RestoreStatusCanceled)
	f.waitRun(t, second, store.RunStatusCompleted)

	runningMax := blockedRun.maxRunning
	if blockedRestore.maxRunning > runningMax {
		runningMax = blockedRestore.maxRunning
	}
	if runningMax != 1 {
		t.Fatalf("running_max=%d across both queues while they contended", runningMax)
	}
	if concurrent := f.requireDisjointUnion(t); concurrent != 1 {
		t.Fatalf("the stored intervals show %d concurrent executions", concurrent)
	}
	if _, _, _, _, started := f.w5SQLiteRestore(t, queued); started != "" {
		t.Fatal("a restore canceled while queued was started")
	}
	f.assertNoSecret(t, nil)
	t.Logf("W5-A4 exclusion run_blocked_by_restore=true restore_blocked_by_run=true running_max=%d queued_samples=%d sqlite_intervals=disjoint",
		runningMax, blockedRun.samples+blockedRestore.samples)
}

// w5Blocked is one contention observation: how many samples showed the blocked
// row queued while its peer held the slot, the largest number of rows ever seen
// running across both queues at one instant, and whether the blocked row was
// claimed while the peer still ran.
type w5Blocked struct {
	samples    int
	maxRunning int
	violated   bool
}

// w5Observe samples both queues through the API while one row must stay queued
// because its peer holds the single execution slot. kind selects whose API a row
// is read from, so a run and a restore are observed on their own routes.
func (f *fixture) w5Observe(t *testing.T, blockedKind, blockedID, peerKind, peerID string) w5Blocked {
	t.Helper()
	var out w5Blocked
	deadline := time.Now().Add(12 * time.Minute)
	for time.Now().Before(deadline) {
		blocked := f.w5StatusOf(t, blockedKind, blockedID)
		peer := f.w5StatusOf(t, peerKind, peerID)
		if running := len(f.w5List(t, "status=running")) + len(f.listRuns(t, "status=running")); running > out.maxRunning {
			out.maxRunning = running
		}
		if out.record(t, blocked, peer, blockedID, peerID) {
			return out
		}
		time.Sleep(runningSampleInterval)
	}
	t.Fatalf("observing %s while %s held the execution slot timed out", blockedID, peerID)
	return out
}

// record classifies one sample and reports the observation as finished: either
// the blocked row has stayed queued through enough samples while its peer held
// the slot, or its peer has left the slot again.
func (out *w5Blocked) record(t *testing.T, blocked, peer, blockedID, peerID string) bool {
	t.Helper()
	switch {
	case peer == store.RestoreStatusRunning && blocked == store.RestoreStatusQueued:
		out.samples++
		return out.samples >= w5QueueSamples*4
	case peer == store.RestoreStatusRunning && blocked == store.RestoreStatusRunning:
		out.violated = true
	case blocked == store.RestoreStatusFailed:
		t.Fatalf("the blocked row %s failed while %s was %s", blockedID, peerID, peer)
	case peer != store.RestoreStatusRunning:
		if out.samples < w5QueueSamples {
			t.Fatalf("%s was seen queued only %d times before %s left running as %s",
				blockedID, out.samples, peerID, peer)
		}
		return true
	}
	return false
}

// w5StatusOf reads one row's status from the API of its own queue.
func (f *fixture) w5StatusOf(t *testing.T, kind, id string) string {
	t.Helper()
	if kind == "run" {
		return f.getRun(t, id).Status
	}
	return f.w5Get(t, id).Status
}

// w5UnionInterval is one stored [started_at, finished_at) window of either queue.
type w5UnionInterval struct {
	id         string
	kind       string
	start, end time.Time
}

// requireDisjointUnion reads every stored run and restore interval and requires
// that no two overlap: the two queues share one global execution slot. It
// returns the highest concurrency the stored windows show.
func (f *fixture) requireDisjointUnion(t *testing.T) int {
	t.Helper()
	all := append(f.w5RunIntervals(t), f.restoreIntervals(t)...)
	if len(all) == 0 {
		t.Fatal("no execution intervals were stored")
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].start.Equal(all[j].start) {
			return all[i].id < all[j].id
		}
		return all[i].start.Before(all[j].start)
	})
	latest := time.Time{}
	for _, item := range all {
		if item.start.Before(latest) {
			t.Fatalf("%s %s starts at %s before the previous execution finished at %s", item.kind, item.id, item.start, latest)
		}
		if item.end.After(latest) {
			latest = item.end
		}
	}
	return 1
}

// restoreIntervals reads every started restore's window from SQLite.
func (f *fixture) restoreIntervals(t *testing.T) []w5UnionInterval {
	t.Helper()
	rows, err := f.view(t).QueryContext(t.Context(), `SELECT id, started_at, finished_at FROM restores`)
	must(t, err)
	defer func() { must(t, rows.Close()) }()
	var all []w5UnionInterval
	for rows.Next() {
		var id string
		var start, end sql.NullString
		must(t, rows.Scan(&id, &start, &end))
		if !start.Valid {
			continue
		}
		if !end.Valid {
			t.Fatalf("restore %s has no finished_at", id)
		}
		all = append(all, w5UnionInterval{
			id: id, kind: "restore",
			start: parseStamp(t, start.String), end: parseStamp(t, end.String),
		})
	}
	must(t, rows.Err())
	return all
}

// w5RunIntervals adapts the stored run windows into the union shape.
func (f *fixture) w5RunIntervals(t *testing.T) []w5UnionInterval {
	t.Helper()
	items := f.runIntervals(t)
	all := make([]w5UnionInterval, 0, len(items))
	for _, item := range items {
		all = append(all, w5UnionInterval{id: item.id, kind: "run", start: item.start, end: item.end})
	}
	return all
}

// w5A5State proves the persisted state and interruption behaviour: phase and
// per-link source visibility, a queued cancel, a running cancel with its exact
// cleanup, a SIGKILL whose restart records failed/interrupted with explicit side
// effects, a published export that nothing ever deletes, and the refusal when no
// source is left.
func w5A5State(t *testing.T, f *fixture) {
	w5RequirePlans(t, w5.plan2, w5.planFull)
	// Both confirmations spend one password-limiter token, and a token only comes
	// back after one refill. Waiting for the full bucket BEFORE the first restore
	// starts is what keeps the window open: a restore lives roughly ten to fifteen
	// seconds, so a wait spent after it started would outlast it and the second
	// row would never be observed queued.
	time.Sleep(w5FullBucket)
	// One restore holds the slot so a second can be observed genuinely queued.
	running := f.w5RunRestoreNow(t, *w5.inc2.BackupID, w5.plan2, "w5-a5-running", false, nil)
	f.w5WaitPhase(t, running, "fetching", "authenticating", "extracting")
	queued := f.w5RunRestoreNow(t, *w5.full.BackupID, w5.planFull, "w5-a5-queued", false, nil)
	if got := f.w5Get(t, queued); got.Status != store.RestoreStatusQueued {
		t.Fatalf("the second restore is %s while the first is running", got.Status)
	}
	publishedBefore := f.w5ExportEntries(t)
	f.w5RequireQueuedCancel(t, queued)

	// The preparing phase alone is a 100 ms poll on a stage that is short, so the
	// cancel is armed on the live prepare tool of this restore's work directory
	// instead: the phase is already reported, and the tool is still running.
	prepare := f.waitRestorePrepare(t, running)
	requirePrepareAlive(t, prepare)
	phaseSeen := f.w5PhaseSeen(t, running)
	canceled := f.w5RequireRunningCancel(t, running, publishedBefore)
	// The queued cancel is terminal: the freed slot must not resurrect it.
	if again := f.w5Get(t, queued); again.Status != store.RestoreStatusCanceled {
		t.Fatalf("the freed slot re-ran a canceled restore: %s", again.Status)
	}
	logs := f.w5Logs(t, running)
	if rows := f.sqliteCount(t, `SELECT COUNT(*) FROM restore_logs WHERE restore_id = ?`, running); rows != len(logs) {
		t.Fatalf("restore logs: API %d SQLite %d", len(logs), rows)
	}
	f.assertNoSecret(t, w5RawJSON(t, logs))
	completed := f.w5List(t, "status=completed&limit=200")
	// The documented list order is judged against creation order and stored
	// created_at, not against the id sort the query already guarantees.
	f.w5RequireNewestFirst(t, f.w5List(t, "limit=200"))
	t.Logf("W5-A5 state phase_seen=%s sources=ftp completed_restores=%d export_bytes=%d verify_status=%s side_effects=explicit canceled_queued=true canceled_running=true logs_agree=sqlite",
		strings.Join(phaseSeen, ","), len(completed), w5ExportedBytes(t, completed), canceled.VerifyStatus)
	w5Sigkill(t, f, publishedBefore)
}

// w5ExportedBytes sums the export bytes of the completed restores, read back
// from the API rather than remembered.
func w5ExportedBytes(t *testing.T, completed []store.Restore) int64 {
	t.Helper()
	total := int64(0)
	if len(completed) == 0 {
		t.Fatal("no completed restore to read export bytes from")
	}
	for _, item := range completed {
		if item.ExportBytes == nil || *item.ExportBytes <= 0 {
			t.Fatalf("completed restore %s reports export_bytes %v", item.ID, item.ExportBytes)
		}
		total += *item.ExportBytes
	}
	return total
}

// w5RequireNewestFirst requires the restores page to be newest first in the two
// ways the contract claims: the rows the scenario created come back in the exact
// reverse of the order they were created in, and the stored created_at never
// becomes newer further down the page. A list that merely descends by id would
// pass the query's own ORDER BY and prove nothing, so both facts are checked.
func (f *fixture) w5RequireNewestFirst(t *testing.T, items []store.Restore) {
	t.Helper()
	if len(w5.created) < 3 {
		t.Fatalf("the scenario created %d restores, want at least 3 to order", len(w5.created))
	}
	position := make(map[string]int, len(items))
	for index, item := range items {
		position[item.ID] = index
	}
	w5RequireCreationOrder(t, position)
	w5RequireCreatedAtOrder(t, items)
}

// w5RequireCreationOrder requires every restore the scenario created to be on
// the page, and each later-created one to be listed strictly before the one
// created before it. That is the newest-first claim itself: the query's own
// ORDER BY id DESC could not fail it, so creation order is what is judged.
func w5RequireCreationOrder(t *testing.T, position map[string]int) {
	t.Helper()
	for index, id := range w5.created {
		at, listed := position[id]
		if !listed {
			t.Fatalf("the restore this scenario created (%s) is missing from the page", id)
		}
		if index == 0 {
			continue
		}
		if previous := position[w5.created[index-1]]; at >= previous {
			t.Fatalf("not newest first: %s (created later than %s) is listed at %d, not before %d",
				id, w5.created[index-1], at, previous)
		}
	}
}

// w5RequireCreatedAtOrder requires the stored created_at to go down the page from
// newest to oldest, with the id as the tie-break the query documents.
func w5RequireCreatedAtOrder(t *testing.T, items []store.Restore) {
	t.Helper()
	for index := 1; index < len(items); index++ {
		first, second := items[index-1], items[index]
		newer, older := parseStamp(t, first.CreatedAt), parseStamp(t, second.CreatedAt)
		if older.After(newer) {
			t.Fatalf("created_at goes backwards in the page: %s (%s) precedes %s (%s)",
				first.ID, first.CreatedAt, second.ID, second.CreatedAt)
		}
		if older.Equal(newer) && first.ID < second.ID {
			t.Fatalf("restores created in the same instant are not id-descending: %s precedes %s",
				first.ID, second.ID)
		}
	}
}

// w5Sigkill kills the serving process while the engine is inside prepare, then
// proves the restart records the interruption exactly: failed/interrupted with
// explicit side effects, this restore's two targets reclaimed, nothing published
// for it, and every earlier published export byte-identical.
func w5Sigkill(t *testing.T, f *fixture, publishedBefore []string) {
	t.Helper()
	w5RequirePlans(t, w5.plan1, w5.plan2)
	keep := w5ProtectedExports(t, f, publishedBefore)
	f.startServeChild(t)
	victim := f.w5RunRestore(t, *w5.inc1.BackupID, w5.plan1, "w5-a5-sigkill", false, nil)
	victimPrepare := f.waitRestorePrepare(t, victim)
	requirePrepareAlive(t, victimPrepare)
	if live := f.w5WorkDirLive(t, victim); !live {
		t.Fatal("the victim restore never created its work directory")
	}
	f.signalChild(t, syscall.SIGKILL)
	f.waitChild(t, 30*time.Second)
	killed, ok := f.child.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !killed.Signaled() || killed.Signal() != syscall.SIGKILL {
		t.Fatal("the serve child was not SIGKILL")
	}
	f.startServeChild(t)
	f.w5RequireInterrupted(t, victim, publishedBefore, keep)
	f.assertNoSecret(t, nil)

	// With no source left for the chain, the plan must refuse: the local payloads
	// were removed by the scenario and the remote payload is now gone too.
	restoresBefore := f.w5RestoreCount(t)
	f.w5DropFTPPayloads(t, w5.members[0])
	f.w5PlanRefusal(t, *w5.full.BackupID, http.StatusConflict, w5CodeSourceUnavailable)
	_, refused := f.w5Headed(t, w5ExecuteBody(*w5.full.BackupID, w5.planFull.Digest, f.secret.admin, false, nil),
		"w5-a5-drift", http.StatusConflict)
	if !bytesContainCode(refused, w5CodeSourceUnavailable) {
		t.Fatalf("no-source execute refusal = %s", refused)
	}
	if count := f.w5RestoreCount(t); count != restoresBefore {
		t.Fatalf("the refused plan and execute stored %d extra restore rows", count-restoresBefore)
	}
	if entries := f.w5ExportListing(t, f.layout.export); !w5EqualLists(entries, publishedBefore) {
		t.Fatalf("the refused requests wrote to the export root: %s", entries)
	}
	f.assertNoSecret(t, refused)
	f.w5UnconfiguredChildRefusal(t, victim, publishedBefore)
	t.Logf("W5-A5 sigkill=failed/interrupted side_effects=explicit cleanup=exact published_intact=true refused=%s:%d recovery_repeated=true",
		w5CodeSourceUnavailable, http.StatusConflict)
}

// w5RequireQueuedCancel cancels a restore that never held the slot and requires
// the terminal row to say so: no start, the explicit all-absent side effects, and
// not one path created for its id.
func (f *fixture) w5RequireQueuedCancel(t *testing.T, id string) {
	t.Helper()
	f.w5Cancel(t, id, http.StatusAccepted)
	canceled := f.w5Wait(t, id, store.RestoreStatusCanceled)
	if canceled.StartedAt != nil {
		t.Fatalf("a queued cancel recorded started_at %s", *canceled.StartedAt)
	}
	if canceled.SideEffects == nil || *canceled.SideEffects != (store.RestoreSideEffects{}) {
		t.Fatalf("queued cancel side_effects = %+v", canceled.SideEffects)
	}
	if f.w5WorkDirLive(t, id) || f.w5PendingDirLive(t, id) || f.w5ExportDirLive(t, id) {
		t.Fatal("a restore that never ran created a path for its id")
	}
}

// w5RequireRunningCancel cancels a restore that is inside the engine's prepare
// stage and requires the canceled row, its cleanup observation and the untouched
// export root. It returns the stored row for the caller's marker.
func (f *fixture) w5RequireRunningCancel(t *testing.T, id string, published []string) store.Restore {
	t.Helper()
	f.w5Cancel(t, id, http.StatusAccepted)
	canceled := f.w5Wait(t, id, store.RestoreStatusCanceled)
	// The cancel must have landed inside prepare, which is the stage the scenario
	// waited for. The canceled row carries no live phase (every terminal write
	// nulls it), so the log's last recorded engine phase is the record of where
	// the engine was when it stopped.
	if canceled.Phase != nil {
		t.Fatalf("a canceled restore still reports a live phase %q", *canceled.Phase)
	}
	if last := f.w5LastPhase(t, id); last != "preparing" {
		t.Fatalf("the cancel landed outside prepare: the log ends at phase %s", last)
	}
	if canceled.ErrorClass == nil || *canceled.ErrorClass != store.RestoreErrorCanceled {
		t.Fatalf("canceled error_class = %v", canceled.ErrorClass)
	}
	if canceled.ErrorCode == nil || *canceled.ErrorCode != "canceled" {
		t.Fatalf("canceled error_code = %v", canceled.ErrorCode)
	}
	f.w5RequireCancelEffects(t, canceled, id)
	if entries := f.w5ExportEntries(t); !w5EqualLists(entries, published) {
		t.Fatalf("the cancel changed the export root: before=%s after=%s", published, entries)
	}
	return canceled
}

// w5ProtectedExports digests every already-published export, so a later fault can
// prove the data an earlier restore published was never touched.
func w5ProtectedExports(t *testing.T, f *fixture, published []string) map[string]string {
	t.Helper()
	keep := map[string]string{}
	for _, id := range published {
		if !w5RestoreIDPattern.MatchString(id) {
			continue
		}
		_, _, digest := f.w5ExportState(t, id)
		keep[id] = digest
	}
	if len(keep) < 3 {
		t.Fatalf("only %d published exports to protect, want at least 3", len(keep))
	}
	return keep
}

// w5RequireInterrupted requires the restart to have failed the interrupted restore
// with explicit side effects, reclaimed exactly its two targets, published
// nothing for it, and left every earlier export and stored row untouched.
func (f *fixture) w5RequireInterrupted(t *testing.T, id string, published []string, keep map[string]string) {
	t.Helper()
	interrupted := f.w5Wait(t, id, store.RestoreStatusFailed)
	w5RequireInterruptedView(t, interrupted)
	// The kill must have landed inside prepare: the log's last recorded engine
	// phase is the stage the process died in, and the interrupted write nulls the
	// row's own phase column, so the log is where that fact survives.
	if phases := f.w5PhaseSeen(t, id); len(phases) == 0 || phases[len(phases)-1] != "preparing" {
		t.Fatalf("the kill landed outside prepare: recorded phases = %s", strings.Join(phases, ","))
	}
	stored, class, code, effects, _ := f.w5SQLiteRestore(t, id)
	if stored != store.RestoreStatusFailed || class != store.RestoreErrorInterrupted {
		t.Fatalf("SQLite row = %s class=%q code=%q", stored, class, code)
	}
	if !strings.Contains(effects, `"work_dir_removed":`) {
		t.Fatalf("the interrupted row carries no explicit side effects: %s", effects)
	}
	if f.w5WorkDirLive(t, id) || f.w5PendingDirLive(t, id) || f.w5ExportDirLive(t, id) {
		t.Fatal("startup recovery did not reclaim exactly the interrupted restore's targets")
	}
	f.w5RequireExportsUntouched(t, published, keep)
	if others := f.w5List(t, "status=running"); len(others) != 0 {
		t.Fatalf("the restart left %d restores running", len(others))
	}
}

// w5RequireInterruptedView requires the API row of the interrupted restore to
// carry the recovery's class, code and an explicit cleanup observation that never
// claims a published export.
func w5RequireInterruptedView(t *testing.T, interrupted store.Restore) {
	t.Helper()
	if interrupted.ErrorClass == nil || *interrupted.ErrorClass != store.RestoreErrorInterrupted {
		t.Fatalf("interrupted error_class = %v", interrupted.ErrorClass)
	}
	if interrupted.ErrorCode == nil || *interrupted.ErrorCode != "interrupted" {
		t.Fatalf("interrupted error_code = %v", interrupted.ErrorCode)
	}
	// Every terminal write nulls the live phase, including the interrupt
	// recovery: the stage it died in is only in the log.
	if interrupted.Phase != nil {
		t.Fatalf("an interrupted restore still reports a live phase %q", *interrupted.Phase)
	}
	if interrupted.SideEffects == nil || interrupted.SideEffects.ExportPublished {
		t.Fatalf("interrupted side_effects = %+v", interrupted.SideEffects)
	}
}

// w5RequireCancelEffects requires the engine's cleanup observation for a canceled
// restore: both private targets gone, nothing published, and no path left behind.
func (f *fixture) w5RequireCancelEffects(t *testing.T, canceled store.Restore, id string) {
	t.Helper()
	if canceled.SideEffects == nil || !canceled.SideEffects.WorkDirRemoved || !canceled.SideEffects.PendingExportRemoved {
		t.Fatalf("canceled side_effects = %+v", canceled.SideEffects)
	}
	if canceled.SideEffects.ExportPublished {
		t.Fatal("a canceled restore reported a published export")
	}
	if f.w5WorkDirLive(t, id) || f.w5PendingDirLive(t, id) {
		t.Fatal("the canceled restore left its two targets behind")
	}
}

// w5RequireExportsUntouched requires the export root to hold exactly the exports
// published before the fault, each with its recorded data digest.
func (f *fixture) w5RequireExportsUntouched(t *testing.T, published []string, keep map[string]string) {
	t.Helper()
	if entries := f.w5ExportEntries(t); !w5EqualLists(entries, published) {
		t.Fatalf("the restart changed the export root: before=%s after=%s", published, entries)
	}
	for export, digest := range keep {
		_, _, now := f.w5ExportState(t, export)
		if now != digest {
			t.Fatalf("the restart changed an earlier published export %s: %s -> %s", export, digest, now)
		}
	}
}

// w5CodeSourceUnavailable is the engine's fixed refusal code the API carries in
// error.detail.code when neither the local repository nor a verified replica can
// serve a chain member.
const w5CodeSourceUnavailable = "source_unavailable"

// w5UnconfiguredChildRefusal restarts the serve child as a deployment with no
// restore input. The restart must leave the interrupted row exactly as the first
// recovery wrote it, answer 503 on the write routes, keep serving reads, and
// touch nothing on the export volume.
func (f *fixture) w5UnconfiguredChildRefusal(t *testing.T, victim string, published []string) {
	t.Helper()
	root := f.layout.export
	before := f.w5Get(t, victim)
	xbstream, mysqld := f.settings.XbstreamPath, f.settings.MysqldPath
	f.layout.export, f.settings.XbstreamPath, f.settings.MysqldPath = "", "", ""
	f.startServeChild(t)
	f.w5PlanRefusal(t, *w5.inc2.BackupID, http.StatusServiceUnavailable, render.Unavailable)
	refused := f.admin.requestWithKey(t, http.MethodPost, "/api/v1/restores",
		w5ExecuteBody(*w5.inc2.BackupID, w5.plan2.Digest, f.secret.admin, false, nil), "w5-a5-503",
		http.StatusServiceUnavailable)
	if !bytesContainCode(refused, render.Unavailable) {
		t.Fatalf("unconfigured execute = %s", refused)
	}
	after := f.w5Get(t, victim)
	if after.Status != before.Status || ptrString(after.ErrorClass) != ptrString(before.ErrorClass) ||
		ptrString(after.ErrorCode) != ptrString(before.ErrorCode) {
		t.Fatalf("a second recovery rewrote the interrupted row: %s/%v/%v was %s/%v/%v",
			after.Status, after.ErrorClass, after.ErrorCode, before.Status, before.ErrorClass, before.ErrorCode)
	}
	if items := f.w5List(t, "limit=200"); len(items) == 0 {
		t.Fatal("an unconfigured deployment stopped serving restore reads")
	}
	f.settings.XbstreamPath, f.settings.MysqldPath = xbstream, mysqld
	if entries := f.w5ExportListing(t, root); !w5EqualLists(entries, published) {
		t.Fatalf("the unconfigured child changed the export root: before=%s after=%s", published, entries)
	}
	f.assertNoSecret(t, refused)
}
