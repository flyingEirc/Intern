package dispatch

import (
	"context"

	"github.com/flyingEirc/Intern/app/web/internal/store"
)

const (
	eventQueued              = "queued"
	eventClaimed             = "claimed"
	eventPreflightOK         = "preflight_ok"
	eventPreflightFailed     = "preflight_failed"
	eventStarted             = "started"
	eventCompleted           = "completed"
	eventFailed              = "failed"
	eventCanceled            = "canceled"
	eventInterrupted         = "interrupted"
	eventReplicationEnqueued = "replication_enqueued"
	eventReplicationStep     = "replication_step"
)

var eventMessages = map[string]string{
	eventQueued:              "run queued",
	eventClaimed:             "run claimed",
	eventPreflightOK:         "preflight succeeded",
	eventPreflightFailed:     "preflight failed",
	eventStarted:             "backup started",
	eventCompleted:           "backup completed",
	eventFailed:              "run failed",
	eventCanceled:            "run canceled",
	eventInterrupted:         "run interrupted",
	eventReplicationEnqueued: "replication enqueued",
	eventReplicationStep:     "replication step",
}

func eventMessage(event string) string {
	if message, ok := eventMessages[event]; ok {
		return message
	}
	return event
}

func eventLevel(event string) string {
	switch event {
	case eventFailed, eventPreflightFailed, eventInterrupted:
		return "error"
	case eventCanceled:
		return "warn"
	default:
		return "info"
	}
}

func eventPayload(data any) any {
	if data == nil {
		return map[string]any{}
	}
	return data
}

func runLogWrite(event string, data any) *store.RunLogWrite {
	return &store.RunLogWrite{
		Level: eventLevel(event), Event: event, Message: eventMessage(event), Data: eventPayload(data),
	}
}

func (d *Dispatcher) logEvent(ctx context.Context, runID, event string, data any) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runLogCapDeadline)
	defer cancel()
	if err := d.config.Database.AppendRunLog(writeCtx, runID, eventLevel(event), event, eventMessage(event), eventPayload(data)); err != nil {
		d.logger.ErrorContext(ctx, "cannot append run log", "run_id", runID, "event", event, "error", err)
	}
}
