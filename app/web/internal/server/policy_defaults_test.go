package server

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

func policyTuning(policy store.Policy) store.PolicyTuning {
	return store.PolicyTuning{
		MaxChainLength: policy.MaxChainLength,
		Parallel:       policy.Parallel,
		Throttle:       policy.Throttle,
		ZstdLevel:      policy.ZstdLevel,
		PageTracking:   policy.PageTracking,
	}
}

func TestPolicyBusinessCreateUsesServerDefaults(t *testing.T) {
	f := newAuthFixtureWithRuns(t)
	f.seed(t, "policy-operator", "operator")
	cookie := f.login(t, "policy-operator", testPassword)
	source := f.createSourceRow(t, t.Context(), "business-source", f.sealer)
	local := f.createRepositoryRow(t, t.Context(), "business-local", newRepositoryDir(t), f.sealer)
	body := policyBody("business-policy", source, local)
	created := decodeResponse[store.Policy](t, f.request(t, http.MethodPost, "/api/v1/policies", body, cookie, http.StatusCreated))
	defaults := store.DefaultPolicyTuning()
	if got := policyTuning(created); !reflect.DeepEqual(got, defaults) {
		t.Fatalf("API policy tuning=%+v, server defaults=%+v", got, defaults)
	}
	persisted, err := f.database.Policy(t.Context(), created.ID)
	if err != nil || !reflect.DeepEqual(policyTuning(persisted), defaults) {
		t.Fatalf("persisted new policy=%+v err=%v", persisted, err)
	}
}

func TestPolicyBusinessEditPreservesHistoricalTuning(t *testing.T) {
	f := newAuthFixtureWithRuns(t)
	f.seed(t, "policy-editor", "operator")
	cookie := f.login(t, "policy-editor", testPassword)
	source := f.createSourceRow(t, t.Context(), "history-source", f.sealer)
	local := f.createRepositoryRow(t, t.Context(), "history-local", newRepositoryDir(t), f.sealer)
	created := decodeResponse[store.Policy](t, f.request(t, http.MethodPost, "/api/v1/policies",
		policyBody("history-policy", source, local), cookie, http.StatusCreated))
	// Model a policy migrated from the former tuning UI. The ordinary write
	// API has no way to set or reset these diagnostic fields.
	historical := store.PolicyTuning{MaxChainLength: 19, Parallel: 3, Throttle: 2048, ZstdLevel: 9, PageTracking: true}
	_, err := f.database.DB().ExecContext(t.Context(), `UPDATE policies SET
		max_chain_length = $1, parallel = $2, throttle = $3, zstd_level = $4, page_tracking = $5
		WHERE id = $6`, historical.MaxChainLength, historical.Parallel, historical.Throttle, historical.ZstdLevel, 1, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	moved := f.createRepositoryRow(t, t.Context(), "history-moved", newRepositoryDir(t), f.sealer)
	updatedBody := policyBody("history-renamed", source, moved)
	updatedBody = strings.Replace(updatedBody, `"full_cron":"0 2 * * *"`, `"full_cron":"15 3 * * 1"`, 1)
	updatedBody = strings.Replace(updatedBody, `"incremental_cron":"0 * * * *"`, `"incremental_cron":"*/30 * * * *"`, 1)
	updatedBody = strings.Replace(updatedBody, `"timezone":"UTC"`, `"timezone":"Asia/Shanghai"`, 1)
	updated := decodeResponse[store.Policy](t, f.request(t, http.MethodPut, "/api/v1/policies/"+created.ID,
		updatedBody, cookie, http.StatusOK))
	assertHistoricalPolicyEdit(t, updated, historical, moved)
	persisted, err := f.database.Policy(t.Context(), created.ID)
	if err != nil || !reflect.DeepEqual(policyTuning(persisted), historical) {
		t.Fatalf("persisted historical tuning=%+v err=%v", persisted, err)
	}
	preview := decodeResponse[schedulePreviewResponse](t, f.request(t, http.MethodGet,
		"/api/v1/policies/"+created.ID+"/schedule?count=4", "", cookie, http.StatusOK))
	if preview.Timezone != updated.Timezone || len(preview.Fires) != 4 {
		t.Fatalf("business schedule preview=%+v", preview)
	}
	expectAuditCount(t, f.dsn, "policies.create", "success", 1)
	expectAuditCount(t, f.dsn, "policies.update", "success", 1)
}

func assertHistoricalPolicyEdit(t *testing.T, updated store.Policy, historical store.PolicyTuning, moved string) {
	t.Helper()
	if got := policyTuning(updated); !reflect.DeepEqual(got, historical) {
		t.Fatalf("business edit reset historical tuning: got=%+v want=%+v", got, historical)
	}
	if updated.Name != "history-renamed" || updated.LocalRepositoryID != moved || updated.Timezone != "Asia/Shanghai" ||
		updated.FullCron != "15 3 * * 1" || updated.IncrementalCron != "*/30 * * * *" {
		t.Fatalf("business edit was not persisted: %+v", updated)
	}
}

func TestPolicyWriteRejectsEveryInternalTuningField(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "policy-operator", "operator")
	cookie := f.login(t, "policy-operator", testPassword)
	source := f.createSourceRow(t, t.Context(), "internal-source", f.sealer)
	local := f.createRepositoryRow(t, t.Context(), "internal-local", newRepositoryDir(t), f.sealer)
	body := policyBody("internal-policy", source, local)
	created := decodeResponse[store.Policy](t, f.request(t, http.MethodPost, "/api/v1/policies", body, cookie, http.StatusCreated))
	for _, field := range []string{
		`"max_chain_length":7`, `"parallel":1`, `"throttle":0`, `"zstd_level":3`, `"page_tracking":false`,
	} {
		for _, target := range []struct{ method, path string }{
			{http.MethodPost, "/api/v1/policies"},
			{http.MethodPut, "/api/v1/policies/" + created.ID},
		} {
			request := strings.TrimSuffix(body, "}") + "," + field + "}"
			response := f.invalidRequest(t, target.method, target.path, request, cookie, http.StatusBadRequest)
			assertSingleError(t, response, render.InvalidJSON)
		}
	}
	persisted, err := f.database.Policy(t.Context(), created.ID)
	if err != nil || !reflect.DeepEqual(policyTuning(persisted), store.DefaultPolicyTuning()) {
		t.Fatalf("rejected tuning write mutated policy=%+v err=%v", persisted, err)
	}
	expectAuditCount(t, f.dsn, "policies.create", "success", 1)
	expectAuditCount(t, f.dsn, "policies.update", "success", 0)
}
