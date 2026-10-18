package middleware

import (
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/flyingEirc/Intern/app/web/internal/store"
)

func TestAuditWriteFailureLogsErrorWithoutSecrets(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = database.Close(); err != nil {
		t.Fatal(err)
	}
	var output lockedAuditLog
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	writeAudit(t.Context(), database, logger, store.AuditLog{
		ActorType: "anonymous", ActorName: "sentinel-secret-password", Action: "auth.login",
		Result: "failed", RequestID: "audit-req",
	})
	logged := output.String()
	if !strings.Contains(logged, `"msg":"audit write failed"`) || !strings.Contains(logged, `"error"`) {
		t.Fatalf("audit failure log = %s", logged)
	}
	if !strings.Contains(logged, `"request_id":"audit-req"`) || strings.Contains(logged, "sentinel-secret-password") {
		t.Fatalf("audit failure log leaked or dropped identity: %s", logged)
	}
}

type lockedAuditLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedAuditLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedAuditLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
