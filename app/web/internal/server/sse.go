package server

import (
	"context"
	"encoding/json/v2"
	"errors"
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

// runEvents streams one run's lifecycle as SSE over the unbuffered writer.
//
// Contract: replay run_logs with seq > Last-Event-ID (or ?after_seq), then
// stream live events, then send a final `end` event when the run is terminal
// and close. A heartbeat comment every 15s rechecks session revocation; each
// event's id is its seq; the per-subscriber buffer holds 256 events and an
// overflow drops the subscriber without blocking the run.
func (h runHandlers) runEvents(c *gin.Context) {
	runID := c.Param("id")
	run, err := h.database.Run(c.Request.Context(), runID)
	if err != nil {
		h.resourceError(c, err, "Run")
		return
	}
	after := int64(0)
	if value := c.GetHeader("Last-Event-ID"); value != "" {
		if after, err = strconv.ParseInt(value, 10, 64); err != nil || after < 0 {
			validationError(c, errors.New("Last-Event-ID must be a non-negative sequence"))
			return
		}
	} else if value, exists := c.GetQuery("after_seq"); exists {
		if after, err = strconv.ParseInt(value, 10, 64); err != nil || after < 0 {
			validationError(c, errors.New("after_seq must be a non-negative sequence"))
			return
		}
	}
	w := c.Writer
	header := w.Header()
	header.Set("Content-Type", sseMediaType)
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	header.Set("Vary", "Cookie")
	// The session cookie state rides the headers before the first event byte;
	// after that, errors only close the stream and no JSON body is possible.
	commitStreamSession(h.authentication.Sessions, c)
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		return
	}
	flusher.Flush()

	events := make(chan store.RunLog, sseBufferCapacity)
	dropped := make(chan struct{}, 1)
	go h.streamRun(c.Request.Context(), run, after, events, dropped)

	h.pumpEvents(c, w, flusher, runID, events, dropped)
}

// streamRun replays the stored log then tails it until the run is terminal.
// Every read failure or terminal state closes the channel. The goroutine never
// blocks the backup worker: events land in a bounded channel and an overflow
// marks the subscriber dropped.
func (h runHandlers) streamRun(ctx context.Context, run store.Run, after int64, events chan<- store.RunLog, dropped chan<- struct{}) {
	defer close(events)
	cursor := after
	for {
		rows, err := h.database.RunLogsAfter(ctx, run.ID, cursor, sseRecheckLogLimit)
		if err != nil {
			return
		}
		next, delivered := deliverAll(rows, events, cursor, dropped)
		if !delivered {
			return
		}
		cursor = next
		terminal, err := h.terminal(ctx, run.ID)
		if err != nil {
			return
		}
		if terminal != nil {
			// The terminal log row commits with the status. Drain it before
			// closing so a caught-up subscriber sees completed/failed/canceled
			// /interrupted before the final end event.
			rows, err = h.database.RunLogsAfter(ctx, run.ID, cursor, sseRecheckLogLimit)
			if err != nil {
				return
			}
			_, _ = deliverAll(rows, events, cursor, dropped)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// deliverAll forwards every row into the bounded subscriber buffer and returns
// the advanced cursor. A full buffer drops the subscriber and reports false; the
// run itself keeps going.
func deliverAll(rows []store.RunLog, events chan<- store.RunLog, cursor int64, dropped chan<- struct{}) (int64, bool) {
	for _, row := range rows {
		select {
		case events <- row:
			cursor = row.Seq
		default:
			select {
			case dropped <- struct{}{}:
			default:
			}
			return cursor, false
		}
	}
	return cursor, true
}

// terminal returns the run row when it reached a terminal status.
func (h runHandlers) terminal(ctx context.Context, runID string) (*store.Run, error) {
	run, err := h.database.Run(ctx, runID)
	if err != nil {
		return nil, err
	}
	switch run.Status {
	case store.RunStatusCompleted, store.RunStatusFailed, store.RunStatusCanceled:
		return &run, nil
	default:
		return nil, nil
	}
}

// pumpEvents writes replayed and live events, heartbeats every 15s and ends
// the stream exactly once with the terminal status. Every write extends the
// response write deadline through http.NewResponseController, so the server's
// 60s WriteTimeout never cuts a long-lived stream. The end event is written
// only at the single exit that observed the channel close, so an
// already-terminal run at connect answers replay + end without a duplicate.
func (h runHandlers) sseHeartbeatInterval() time.Duration {
	if h.heartbeat > 0 {
		return h.heartbeat
	}
	return sseHeartbeat
}

func (h runHandlers) pumpEvents(c *gin.Context, w gin.ResponseWriter, flusher http.Flusher, runID string, events <-chan store.RunLog, dropped <-chan struct{}) {
	controller := http.NewResponseController(w)
	recheck := time.NewTicker(h.sseHeartbeatInterval())
	defer recheck.Stop()

	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-dropped:
			// Slow consumer: stop the stream without touching the run.
			return
		case <-recheck.C:
			if !h.sessionAlive(c) {
				// Session revocation (logout, epoch change, user disabled):
				// send the revoked event and close.
				sseWrite(controller, w, flusher, func() string { return "event: revoked\n\n" })
				return
			}
			sseWrite(controller, w, flusher, func() string { return ": ping\n\n" })
		case row, open := <-events:
			if !open {
				// The tail goroutine finished: it saw the terminal run or a
				// read failure. Confirm the terminal status once and send the
				// single final end event.
				terminal, err := h.terminal(c.Request.Context(), runID)
				if err != nil || terminal == nil {
					return
				}
				sseWrite(controller, w, flusher, func() string { return endFrame(terminal.Status) })
				return
			}
			sseWrite(controller, w, flusher, func() string {
				return "event: message\nid: " + strconv.FormatInt(row.Seq, 10) +
					"\ndata: " + sseEventJSON(row) + "\n\n"
			})
		}
	}
}

// endFrame is the single final event of every stream: the run's terminal
// status as the data of an end event.
func endFrame(status string) string {
	return "event: end\ndata: " + status + "\n\n"
}

// sseWrite renders the frame, extends the write deadline for this write and
// flushes. Write failures close the stream by returning false; the caller's
// next select observes the request context or channel and exits. Errors after
// the first byte never produce a JSON body.
func sseWrite(controller *http.ResponseController, w gin.ResponseWriter, flusher http.Flusher, frame func() string) bool {
	// A connection type that does not support deadlines reports an error here;
	// the server's own WriteTimeout then still bounds the stream and writes
	// continue. isDeadlineUnsupported is the one error this deliberately ignores.
	if err := controller.SetWriteDeadline(time.Now().Add(sseWriteDeadline)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return false
	}
	text := frame()
	if _, err := w.WriteString(text); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

// sessionAlive rechecks the session at every heartbeat. It repeats the frozen
// acceptance checks — user present, enabled, epoch unchanged, runs:read still
// granted — without modifying the session, so a revoked, disabled or demoted
// session ends the stream with the revoked event. The checks mirror the
// shared authenticate path; the stream cannot call that path itself because
// it destroys the session and writes JSON, which is impossible after the
// first event byte.
func (h runHandlers) sessionAlive(c *gin.Context) bool {
	ctx := c.Request.Context()
	token := h.authentication.Sessions.Token(ctx)
	if token == "" {
		return false
	}
	_, found, err := h.authentication.Database.Sessions().FindCtx(ctx, token)
	if err != nil || !found {
		return false
	}
	id := h.authentication.Sessions.GetString(ctx, "user_id")
	if id == "" {
		return false
	}
	user, err := h.authentication.Database.User(ctx, id)
	if err != nil || user.Disabled || user.SessionEpoch != h.authentication.Sessions.GetInt64(ctx, "epoch") {
		return false
	}
	return auth.HasPermission(user.Role, auth.RunsRead)
}

// sseEventJSON is the fixed wire shape of one event: seq, ts, level, event,
// message and data.
func sseEventJSON(row store.RunLog) string {
	payload := struct {
		Seq     int64  `json:"seq"`
		TS      string `json:"ts"`
		Level   string `json:"level"`
		Event   string `json:"event"`
		Message string `json:"message"`
		Data    any    `json:"data"`
	}{Seq: row.Seq, TS: row.TS, Level: row.Level, Event: row.Event, Message: row.Message, Data: row.Data}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// sourceChains serves one source's chains with their members.
func (h runHandlers) sourceChains(c *gin.Context) {
	sourceID := c.Param("id")
	exists, err := h.dispatcher.SourceExists(c.Request.Context(), sourceID)
	if err != nil {
		internalError(c)
		return
	}
	if !exists {
		render.Error(c, http.StatusNotFound, render.NotFound, "Source not found")
		return
	}
	chains, err := h.dispatcher.ChainViews(c.Request.Context(), sourceID)
	if err != nil {
		internalError(c)
		return
	}
	render.JSON(c, http.StatusOK, chainsPage{Items: chains})
}

type chainsPage struct {
	Items []dispatch.ChainView `json:"items"`
}

// backup serves one backup with its per-repository replicas.
func (h runHandlers) backup(c *gin.Context) {
	view, err := h.dispatcher.BackupView(c.Request.Context(), c.Param("id"))
	if err != nil {
		h.resourceError(c, err, "Backup")
		return
	}
	render.JSON(c, http.StatusOK, view)
}

// protection serves the split local/remote protection points.
func (h runHandlers) protection(c *gin.Context) {
	sourceID := c.Param("id")
	exists, err := h.dispatcher.SourceExists(c.Request.Context(), sourceID)
	if err != nil {
		internalError(c)
		return
	}
	if !exists {
		render.Error(c, http.StatusNotFound, render.NotFound, "Source not found")
		return
	}
	protection, err := h.dispatcher.Protection(c.Request.Context(), sourceID)
	if err != nil {
		internalError(c)
		return
	}
	render.JSON(c, http.StatusOK, protection)
}

// replicas lists replica rows with the bounded filters.
func (h runHandlers) replicas(c *gin.Context) {
	limit, ok := replicaLimit(c)
	if !ok {
		return
	}
	filter := dispatch.ReplicaFilter{Limit: limit}
	if value, exists := c.GetQuery("source_id"); exists {
		if _, err := parseUUID(value); err != nil {
			validationError(c, errors.New("source_id must be a resource ID"))
			return
		}
		filter.SourceID = value
	}
	if value, exists := c.GetQuery("repository_id"); exists {
		if _, err := parseUUID(value); err != nil {
			validationError(c, errors.New("repository_id must be a resource ID"))
			return
		}
		filter.RepositoryID = value
	}
	if value, exists := c.GetQuery("status"); exists {
		switch value {
		case "pending", "uploading", "uploaded", "verified", "failed", "exhausted":
			filter.Status = value
		default:
			validationError(c, errors.New("status is not a replica status"))
			return
		}
	}
	items, err := h.dispatcher.Replicas(c.Request.Context(), filter)
	if err != nil {
		internalError(c)
		return
	}
	render.JSON(c, http.StatusOK, replicasPage{Items: items})
}

func replicaLimit(c *gin.Context) (int, bool) {
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "100"))
	if err != nil || limit < 1 || limit > 200 {
		validationError(c, errors.New("limit must be between 1 and 200"))
		return 0, false
	}
	return limit, true
}

type replicasPage struct {
	Items []dispatch.ReplicaView `json:"items"`
}

// retryReplica resets one replica for an immediate manual attempt.
func (h runHandlers) retryReplica(c *gin.Context) {
	backupID, repositoryID := c.Param("id"), c.Param("repository_id")
	middleware.AuditResource(c, "replica", backupID+"/"+repositoryID, store.AuditDetail{})
	if err := h.dispatcher.ReplicaRetry(c.Request.Context(), backupID, repositoryID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			render.Error(c, http.StatusNotFound, render.NotFound, "Replica not found")
			return
		}
		internalError(c)
		return
	}
	render.JSON(c, http.StatusAccepted, acceptedResponse{RunID: backupID})
}
