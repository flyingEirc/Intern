package server

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/dispatch"
	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// TestManualTriggerIdempotencyOverHTTP covers the frozen Idempotency-Key
// contract at the API boundary over TLS: same key and body replay the same
// run_id with 202, the same key with a different body is 409, and a missing
// header is a validation failure.
func TestManualTriggerIdempotencyOverHTTP(t *testing.T) {
	h := newRunHarness(t)
	cookie := h.login(t, "trigger", "operator")
	policy := h.seedPolicyOnly(t, "http-idempotency")
	h.stopRuns()
	client := h.jsonClient(t)

	created := triggerAccepted(t, h, client, cookie, policy.ID, "incremental", "http-key")
	// The same key and body replays the same run.
	if replayed := triggerAccepted(t, h, client, cookie, policy.ID, "incremental", "http-key"); replayed != created {
		t.Fatalf("replay run_id = %s, want %s", replayed, created)
	}
	assertTriggerConflict(t, h, client, cookie, policy.ID, created)
	// A missing Idempotency-Key is a validation failure, not a created run.
	status, body := readJSONStatus(t, h.postTrigger(t, client, cookie, policy.ID, "full", ""))
	if status != http.StatusBadRequest || !strings.Contains(body, render.ValidationFailed) {
		t.Fatalf("missing key status=%d body=%s", status, body)
	}
}

// triggerAccepted posts one trigger and returns the accepted run id.
func triggerAccepted(t *testing.T, h *runHarness, client *http.Client, cookie *http.Cookie, policyID, kind, key string) string {
	t.Helper()
	status, body := readJSONStatus(t, h.postTrigger(t, client, cookie, policyID, kind, key))
	if status != http.StatusAccepted {
		t.Fatalf("trigger status=%d body=%s", status, body)
	}
	created := decodeBody[dispatch.CreatedRun](t, body)
	if created.RunID == "" {
		t.Fatalf("body = %s", body)
	}
	return created.RunID
}

// assertTriggerConflict requires a different body under the used key to be a
// 409 conflict and to leave the original run as the only row.
func assertTriggerConflict(t *testing.T, h *runHarness, client *http.Client, cookie *http.Cookie, policyID, wantRunID string) {
	t.Helper()
	status, body := readJSONStatus(t, h.postTrigger(t, client, cookie, policyID, "full", "http-key"))
	if status != http.StatusConflict {
		t.Fatalf("conflict status=%d body=%s", status, body)
	}
	if code := decodeBody[render.ErrorResponse](t, body).Error.Code; code != render.Conflict {
		t.Fatalf("conflict code = %q", code)
	}
	if runs := h.runsOf(t, policyID); len(runs) != 1 || runs[0].ID != wantRunID {
		t.Fatalf("runs = %+v, want only %s", runs, wantRunID)
	}
}

// TestManualTriggerReplaySurvivesRestart proves the idempotency key is durable:
// a new process answers the same key with the same run instead of a new one.
func TestManualTriggerReplaySurvivesRestart(t *testing.T) {
	h := newRunHarness(t)
	cookie := h.login(t, "restart", "operator")
	policy := h.seedPolicyOnly(t, "restart-idempotency")
	h.stopRuns()
	client := h.jsonClient(t)

	first := h.triggerOverHTTP(t, client, cookie, policy.ID, "full", "durable-key")

	// Restart the control-plane dispatcher against the same database: recovery
	// runs again, which is what a serve restart does.
	h.materializeDispatcher(t)
	h.stopRuns()
	second := h.triggerOverHTTP(t, client, cookie, policy.ID, "full", "durable-key")
	if second != first {
		t.Fatalf("restart replay returned %s, want %s", second, first)
	}
	if runs := h.runsOf(t, policy.ID); len(runs) != 1 {
		t.Fatalf("restart created %d runs, want 1", len(runs))
	}
}

// TestManualTriggerUnconfiguredRuntimeIs503 proves decision 9's manual path: an
// unconfigured deployment still serves, and the trigger answers 503 without
// creating a run.
func TestManualTriggerUnconfiguredRuntimeIs503(t *testing.T) {
	h := newUnconfiguredRunHarness(t)
	cookie := h.login(t, "unavailable", "operator")
	// The row is seeded with the fixture's sealer, which is how a deployment
	// that once had a KEK looks after the KEK is withdrawn.
	policy := h.seedPolicyOnly(t, "unavailable-policy")
	client := h.jsonClient(t)

	status, body := readJSONStatus(t, h.postTrigger(t, client, cookie, policy.ID, "full", "unavailable-key"))
	if status != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured trigger status=%d body=%s; want 503", status, body)
	}
	if code := decodeBody[render.ErrorResponse](t, body).Error.Code; code != render.Unavailable {
		t.Fatalf("503 code = %q", code)
	}
	// Nothing was skipped silently: no run row was created for the refusal.
	if runs := h.runsOf(t, policy.ID); len(runs) != 0 {
		t.Fatalf("503 created runs: %+v", runs)
	}
	// The cron path is the other half: the same deployment records the run and
	// the worker fails it visibly with error_class unavailable.
	cron, err := h.fixture.database.CreateRun(t.Context(), store.RunInput{
		PolicyID: policy.ID, SourceID: policy.SourceID, Kind: store.RunKindFull,
		Trigger: store.RunTriggerCron, IdempotencyKey: "unconfigured-cron",
		RequestHash: dispatch.MarshalTriggerBody(store.RunKindFull), ScheduledAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	failed := h.awaitTerminal(t, cron.ID)
	if failed.Status != store.RunStatusFailed || failed.ErrorClass == nil || *failed.ErrorClass != store.RunErrorUnavailable {
		t.Fatalf("cron run = %+v, want failed/unavailable", failed)
	}
}

// TestCancelQueuedRunOverHTTP proves the API cancel path on a queued run: 202 the
// first time, 409 once terminal, 404 for an unknown id, and the worker never
// claims the canceled row.
func TestCancelQueuedRunOverHTTP(t *testing.T) {
	h := newRunHarness(t)
	cookie := h.login(t, "cancel", "operator")
	policy, run := h.seedPolicyWithRun(t, "cancel-http")
	h.stopRuns()
	client := h.jsonClient(t)

	status, body := readJSONStatus(t, h.do(t, client, http.MethodPost, "/api/v1/runs/"+run.ID+"/cancel", "", cookie))
	if status != http.StatusAccepted {
		t.Fatalf("cancel status=%d body=%s", status, body)
	}
	assertCanceledWithoutRunning(t, h, run.ID)
	assertCancelRefusals(t, h, client, cookie, run.ID)
	if runs := h.runsOf(t, policy.ID); len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
}

// assertCanceledWithoutRunning checks the terminal state, its log and the fact
// that the worker can never pick the row up.
func assertCanceledWithoutRunning(t *testing.T, h *runHarness, runID string) {
	t.Helper()
	if final := h.run(t, runID); final.Status != store.RunStatusCanceled {
		t.Fatalf("status = %s, want canceled", final.Status)
	}
	logs, err := h.fixture.database.RunLogsAfter(t.Context(), runID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 || logs[0].Event != "queued" || logs[1].Event != "canceled" {
		t.Fatalf("run log = %+v, want queued then canceled", logs)
	}
	if _, ok, err := h.fixture.database.ClaimRun(t.Context()); err != nil || ok {
		t.Fatalf("canceled run was claimable: %v, %v", ok, err)
	}
}

// assertCancelRefusals covers the terminal and unknown-run cancel responses.
func assertCancelRefusals(t *testing.T, h *runHarness, client *http.Client, cookie *http.Cookie, runID string) {
	t.Helper()
	status, body := readJSONStatus(t, h.do(t, client, http.MethodPost, "/api/v1/runs/"+runID+"/cancel", "", cookie))
	if status != http.StatusConflict || !strings.Contains(body, render.Conflict) {
		t.Fatalf("second cancel status=%d body=%s", status, body)
	}
	unknown := "0191c9c4-0000-7000-8000-000000000000"
	status, body = readJSONStatus(t, h.do(t, client, http.MethodPost, "/api/v1/runs/"+unknown+"/cancel", "", cookie))
	if status != http.StatusNotFound || !strings.Contains(body, render.NotFound) {
		t.Fatalf("unknown cancel status=%d body=%s", status, body)
	}
}

// TestManualRunFailsVisiblyWithPreflightClass proves a manual run whose real
// preflight fails reaches failed/preflight through the worker, and the API then
// reports that terminal state with its error class.
func TestManualRunFailsVisiblyWithPreflightClass(t *testing.T) {
	h := newRunHarness(t)
	cookie := h.login(t, "preflight-class", "operator")
	policy := h.seedPolicyOnly(t, "preflight-class")
	client := h.jsonClient(t)

	// The live worker claims the accepted run and fails it on the unreachable
	// socket; no MySQL server is needed for this failure.
	runID := h.triggerOverHTTP(t, client, cookie, policy.ID, "full", "preflight-class-key")
	final := h.awaitTerminal(t, runID)
	if final.Status != store.RunStatusFailed || final.ErrorClass == nil || *final.ErrorClass != store.RunErrorPreflight {
		t.Fatalf("run = %+v, want failed/preflight", final)
	}
	response := h.do(t, client, http.MethodGet, "/api/v1/runs/"+runID, "", cookie)
	status, body := readJSONStatus(t, response)
	if status != http.StatusOK {
		t.Fatalf("get status=%d body=%s", status, body)
	}
	stored := decodeBody[store.Run](t, body)
	if stored.ErrorClass == nil || *stored.ErrorClass != store.RunErrorPreflight || stored.ErrorCode == nil {
		t.Fatalf("wire run = %s", body)
	}
}

// TestRunLifecycleWritesSystemAuditRows proves the dispatcher's audit rows do
// not depend on HTTP middleware: a claimed and failed run writes system rows for
// runs.claimed and runs.failed, with actor_type system.
func TestRunLifecycleWritesSystemAuditRows(t *testing.T) {
	h := newRunHarness(t)
	cookie := h.login(t, "audit", "operator")
	policy := h.seedPolicyOnly(t, "audit-rows")
	client := h.jsonClient(t)

	runID := h.triggerOverHTTP(t, client, cookie, policy.ID, "full", "audit-rows-key")
	final := h.awaitTerminal(t, runID)
	if final.Status != store.RunStatusFailed {
		t.Fatalf("run = %+v", final)
	}
	for _, action := range []string{"runs.claimed", "runs.failed"} {
		if got := h.systemAudit(t, action); got != 1 {
			t.Fatalf("%s system audits = %d, want 1", action, got)
		}
	}
	// The HTTP trigger itself is audited as a user action by the middleware,
	// which is a different row from the dispatcher's system rows.
	var userRows int
	query := `SELECT COUNT(*) FROM audit_logs WHERE actor_type = 'user' AND action = 'runs.trigger'`
	if err := h.fixture.database.DB().QueryRowContext(t.Context(), query).Scan(&userRows); err != nil {
		t.Fatal(err)
	}
	if userRows != 1 {
		t.Fatalf("runs.trigger user audits = %d, want 1", userRows)
	}
}

// TestRunListFiltersAndCursor exercises the bounded list filters over the wire.
func TestRunListFiltersAndCursor(t *testing.T) {
	h := newRunHarness(t)
	cookie := h.login(t, "list", "viewer")
	policy := h.seedPolicyOnly(t, "list-filters")
	h.stopRuns()
	client := h.jsonClient(t)
	// Three queued runs on one policy, so the cursor has a real second page.
	for _, key := range []string{"list-a", "list-b", "list-c"} {
		if _, err := h.fixture.database.CreateRun(t.Context(), store.RunInput{
			PolicyID: policy.ID, SourceID: policy.SourceID, Kind: store.RunKindIncremental,
			Trigger: store.RunTriggerCron, IdempotencyKey: key,
			RequestHash: dispatch.MarshalTriggerBody(store.RunKindIncremental), ScheduledAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	first := listRuns(t, h, client, cookie, "/api/v1/runs?policy_id="+policy.ID+"&status=queued&limit=2")
	if len(first.Items) != 2 || first.NextBefore == nil {
		t.Fatalf("first page = %+v", first)
	}
	second := listRuns(t, h, client, cookie,
		"/api/v1/runs?policy_id="+policy.ID+"&limit=2&before="+*first.NextBefore)
	if len(second.Items) != 1 {
		t.Fatalf("second page = %+v", second)
	}
	// The cursor is exclusive: the next page starts after the last item.
	if id := second.Items[0].ID; id >= *first.NextBefore {
		t.Fatalf("cursor not exclusive: %s >= %s", id, *first.NextBefore)
	}
	// An unknown status is a validation failure, not an empty page.
	status, body := readJSONStatus(t, h.do(t, client, http.MethodGet, "/api/v1/runs?status=bogus", "", cookie))
	if status != http.StatusBadRequest || !strings.Contains(body, render.ValidationFailed) {
		t.Fatalf("bad status=%d body=%s", status, body)
	}
}

// runPage is the list wire shape the cursor assertions read.
type runPage struct {
	Items      []store.Run `json:"items"`
	NextBefore *string     `json:"next_before"`
}

// listRuns fetches one page and requires 200.
func listRuns(t *testing.T, h *runHarness, client *http.Client, cookie *http.Cookie, path string) runPage {
	t.Helper()
	status, body := readJSONStatus(t, h.do(t, client, http.MethodGet, path, "", cookie))
	if status != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, status, body)
	}
	return decodeBody[runPage](t, body)
}

// Concurrent triggers over HTTP share one key: exactly one run may exist.
func TestConcurrentHTTPTriggersCreateOneRun(t *testing.T) {
	h := newRunHarness(t)
	cookie := h.login(t, "concurrent", "operator")
	policy := h.seedPolicyOnly(t, "concurrent-http")
	h.stopRuns()
	client := h.jsonClient(t)

	const callers = 8
	ids := make([]string, callers)
	statuses := make([]int, callers)
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for i := range callers {
		done.Add(1)
		go func(index int) {
			defer done.Done()
			start.Wait()
			response := h.postTrigger(t, client, cookie, policy.ID, "full", "concurrent-http-key")
			code, body := readJSONStatus(t, response)
			statuses[index] = code
			if code == http.StatusAccepted {
				ids[index] = decodeBody[dispatch.CreatedRun](t, body).RunID
			}
		}(i)
	}
	start.Done()
	done.Wait()

	first := ""
	created := 0
	for i, code := range statuses {
		if code != http.StatusAccepted {
			t.Fatalf("caller %d status=%d", i, code)
		}
		if first == "" {
			first = ids[i]
		}
		if ids[i] != first {
			t.Fatalf("concurrent trigger returned %s, want %s", ids[i], first)
		}
		if _, err := h.fixture.database.RunByIdempotencyKey(t.Context(), "concurrent-http-key"); err != nil {
			t.Fatal(err)
		}
		created++
	}
	if created != callers {
		t.Fatalf("accepted=%d, want %d", created, callers)
	}
	if runs := h.runsOf(t, policy.ID); len(runs) != 1 {
		t.Fatalf("stored runs = %d, want 1", len(runs))
	}
}

// decodeBody unmarshals one JSON body.
func decodeBody[T any](t *testing.T, body string) T {
	t.Helper()
	var value T
	if err := json.Unmarshal([]byte(body), &value); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return value
}
