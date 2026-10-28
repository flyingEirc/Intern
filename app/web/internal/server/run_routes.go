package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/app/web/internal/dispatch"
	"github.com/flyingEirc/Intern/app/web/internal/middleware"
	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// SSE streaming constants frozen by the W4 contract: the per-subscriber buffer
// holds 256 events, on overflow the subscriber is dropped and the run keeps
// going; a heartbeat comment fires every 15s and rechecks the session; each
// event's id is its run_logs.seq.
const (
	sseBufferCapacity  = 256
	sseHeartbeat       = 15 * time.Second
	sseWriteDeadline   = 30 * time.Second
	sseRecheckLogLimit = 1000
)

// runHandlers serves the run, chain, replica and protection APIs plus the
// authenticated SSE event stream.
type runHandlers struct {
	database       *store.Store
	authentication *auth.Service
	logger         logger
	dispatcher     DispatcherControl
	heartbeat      time.Duration
}

type logger interface {
	ErrorContext(ctx context.Context, msg string, args ...any)
	InfoContext(ctx context.Context, msg string, args ...any)
}

// registerRuns wires every W4 API onto the engine.
func registerRuns(engine *gin.Engine, database *store.Store, dispatcher DispatcherControl,
	authentication *auth.Service, permissions middleware.Authorization, lg logger,
	heartbeat time.Duration,
) {
	if heartbeat <= 0 {
		heartbeat = sseHeartbeat
	}
	h := runHandlers{
		database: database, authentication: authentication, logger: lg, dispatcher: dispatcher,
		heartbeat: heartbeat,
	}

	runs := engine.Group("/api/v1/runs", permissions.RequirePermission(auth.RunsRead))
	runs.GET("", h.runs)
	runs.GET("/:id", h.run)
	runs.GET("/:id/logs", h.runLogs)
	runs.POST("/:id/cancel", permissions.RequirePermission(auth.RunsCancel), h.cancelRun)

	// The events route bypasses the buffered session writer (Session skips the
	// buffer for middleware.IsSSEPath paths). This group performs the same scs
	// load, user/disabled/epoch checks and runs:read permission BEFORE writing
	// headers, through the shared authenticate path; failures are normal JSON
	// 401/403/404.
	engine.GET("/api/v1/runs/:id/events",
		sseSession(&middleware.Authorization{Service: authentication}, auth.RunsRead),
		h.runEvents)

	policies := engine.Group("/api/v1/policies")
	policies.POST("/:id/runs", permissions.RequirePermission(auth.RunsTrigger), h.triggerRun)

	engine.GET("/api/v1/sources/:id/chains", permissions.RequirePermission(auth.ResourcesRead), h.sourceChains)
	engine.GET("/api/v1/backups/:id", permissions.RequirePermission(auth.ResourcesRead), h.backup)
	engine.GET("/api/v1/sources/:id/protection", permissions.RequirePermission(auth.ResourcesRead), h.protection)
	engine.GET("/api/v1/replicas", permissions.RequirePermission(auth.ResourcesRead), h.replicas)
	engine.POST("/api/v1/backups/:id/replicas/:repository_id/retry",
		permissions.RequirePermission(auth.ReplicasRetry), h.retryReplica)
}

// DispatcherControl is the dispatch surface the HTTP layer may call. The
// server package never touches the dispatcher's loops directly.
type DispatcherControl interface {
	RequestCancel(ctx context.Context, runID string) error
	ReplicaRetry(ctx context.Context, backupID, repositoryID string) error
	Protection(ctx context.Context, sourceID string) (dispatch.Protection, error)
	Replicas(ctx context.Context, filter dispatch.ReplicaFilter) ([]dispatch.ReplicaView, error)
	BackupView(ctx context.Context, backupID string) (*dispatch.BackupView, error)
	ChainViews(ctx context.Context, sourceID string) ([]dispatch.ChainView, error)
	SourceExists(ctx context.Context, sourceID string) (bool, error)
	TriggerRun(ctx context.Context, policyID, kind, key, requestHash, actorID string) (dispatch.TriggerOutcome, any)
}

// runTriggerRequest is the manual trigger body.
type runTriggerRequest struct {
	Kind string `json:"kind"`
}

// runsPage is the list wire shape, matching the resource cursor contract.
type runsPage struct {
	Items      []store.Run `json:"items"`
	NextBefore *string     `json:"next_before"`
}

// runLogsPage is the log page wire shape.
type runLogsPage struct {
	Items     []store.RunLog `json:"items"`
	NextAfter *int64         `json:"next_after"`
	Truncated bool           `json:"truncated"`
}

func (h runHandlers) runs(c *gin.Context) {
	before, limit, ok := resourcePagination(c, "run")
	if !ok {
		return
	}
	filter := store.RunsFilter{Before: before, Limit: limit}
	for name, dest := range map[string]*string{"policy_id": &filter.PolicyID, "source_id": &filter.SourceID} {
		value, exists := c.GetQuery(name)
		if !exists {
			continue
		}
		if _, err := parseUUID(value); err != nil {
			validationError(c, errors.New(name+" must be a resource ID"))
			return
		}
		*dest = value
	}
	if value, exists := c.GetQuery("status"); exists {
		if !validRunStatus(value) {
			validationError(c, errors.New("status must be one of queued, running, completed, failed, canceled"))
			return
		}
		filter.Status = value
	}
	// One extra row decides whether another page exists, exactly like the
	// resource lists; without it next_before can never be set.
	filter.Limit = limit + 1
	items, err := h.database.Runs(c.Request.Context(), filter)
	if err != nil {
		internalError(c)
		return
	}
	page := runsPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1].ID
		page.NextBefore = &last
	}
	render.JSON(c, http.StatusOK, page)
}

func (h runHandlers) run(c *gin.Context) {
	run, err := h.database.Run(c.Request.Context(), c.Param("id"))
	if err != nil {
		h.resourceError(c, err, "Run")
		return
	}
	render.JSON(c, http.StatusOK, run)
}

func (h runHandlers) runLogs(c *gin.Context) {
	runID := c.Param("id")
	if _, err := h.database.Run(c.Request.Context(), runID); err != nil {
		h.resourceError(c, err, "Run")
		return
	}
	limit, after, ok := logWindow(c)
	if !ok {
		return
	}
	items, err := h.database.RunLogsAfter(c.Request.Context(), runID, after, limit)
	if err != nil {
		internalError(c)
		return
	}
	page := runLogsPage{Items: items}
	if len(items) > 0 {
		last := items[len(items)-1].Seq
		page.NextAfter = &last
		page.Truncated = h.logTruncated(c, runID)
	}
	render.JSON(c, http.StatusOK, page)
}

// logWindow parses the log paging parameters: limit 1..1000 (default 100) and
// a non-negative after_seq. It writes the validation error and reports false.
func logWindow(c *gin.Context) (limit, after int64, ok bool) {
	limit = 100
	if value, exists := c.GetQuery("limit"); exists {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 1 || parsed > sseRecheckLogLimit {
			validationError(c, fmt.Errorf("limit must be between 1 and %d", sseRecheckLogLimit))
			return 0, 0, false
		}
		limit = parsed
	}
	if value, exists := c.GetQuery("after_seq"); exists {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			validationError(c, errors.New("after_seq must be a non-negative sequence"))
			return 0, 0, false
		}
		after = parsed
	}
	return limit, after, true
}

// logTruncated reports whether the newest stored row is the truncated marker.
// A read failure is reported as false: the page itself already succeeded.
func (h runHandlers) logTruncated(c *gin.Context, runID string) bool {
	maxSeq, err := h.database.MaxRunLogSeq(c.Request.Context(), runID)
	if err != nil || maxSeq <= 0 {
		return false
	}
	lastRow, err := h.database.RunLog(c.Request.Context(), runID, maxSeq)
	return err == nil && lastRow.Event == store.TruncatedEvent
}

func (h runHandlers) cancelRun(c *gin.Context) {
	id := c.Param("id")
	middleware.AuditResource(c, "run", id, store.AuditDetail{})
	// 202 even for a terminal run is wrong: a terminal run answers 409 before
	// the dispatcher is involved.
	if _, err := h.database.Run(c.Request.Context(), id); err != nil {
		h.resourceError(c, err, "Run")
		return
	}
	if err := h.dispatcher.RequestCancel(c.Request.Context(), id); err != nil {
		if errors.Is(err, store.ErrConflict) {
			render.Error(c, http.StatusConflict, render.Conflict, "Run is already terminal")
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			render.Error(c, http.StatusNotFound, render.NotFound, "Run not found")
			return
		}
		internalError(c)
		return
	}
	render.JSON(c, http.StatusAccepted, acceptedResponse{RunID: id})
}

type acceptedResponse struct {
	RunID string `json:"run_id"`
}

// triggerRun creates a manual run behind the Idempotency-Key contract.
func (h runHandlers) triggerRun(c *gin.Context) {
	policyID := c.Param("id")
	key := c.GetHeader("Idempotency-Key")
	if key == "" || len(key) > 128 {
		validationError(c, errors.New("Idempotency-Key header of 1 to 128 characters is required"))
		return
	}
	var request runTriggerRequest
	if !render.DecodeJSON(c, &request) {
		return
	}
	if request.Kind != store.RunKindFull && request.Kind != store.RunKindIncremental {
		validationError(c, errors.New("kind must be full or incremental"))
		return
	}
	requestHash := dispatch.MarshalTriggerBody(request.Kind)
	middleware.AuditResource(c, "policy", policyID, store.AuditDetail{})
	status, payload := h.dispatcher.TriggerRun(c.Request.Context(), policyID, request.Kind, key,
		requestHash, middleware.CurrentUser(c).ID)
	h.renderTriggerOutcome(c, status, payload)
}

// renderTriggerOutcome maps the frozen manual trigger outcomes onto the wire.
func (h runHandlers) renderTriggerOutcome(c *gin.Context, status dispatch.TriggerOutcome, payload any) {
	switch status {
	case dispatch.TriggerCreated, dispatch.TriggerReplayed:
		render.JSON(c, http.StatusAccepted, payload)
	case dispatch.TriggerConflict:
		render.Error(c, http.StatusConflict, render.Conflict, "Idempotency key was already used with a different body or the policy is disabled")
	case dispatch.TriggerNotFound:
		render.Error(c, http.StatusNotFound, render.NotFound, "Policy not found")
	case dispatch.TriggerUnavailable:
		unavailableDependency(c)
	default:
		internalError(c)
	}
}

// validRunStatus reports whether value is one of the closed run statuses.
func validRunStatus(value string) bool {
	switch value {
	case store.RunStatusQueued, store.RunStatusRunning, store.RunStatusCompleted,
		store.RunStatusFailed, store.RunStatusCanceled:
		return true
	default:
		return false
	}
}

func (h runHandlers) resourceError(c *gin.Context, err error, name string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		render.Error(c, http.StatusNotFound, render.NotFound, name+" not found")
	case errors.Is(err, store.ErrConflict):
		render.Error(c, http.StatusConflict, render.Conflict, name+" name already exists")
	default:
		internalError(c)
	}
}

var _ = io.EOF
