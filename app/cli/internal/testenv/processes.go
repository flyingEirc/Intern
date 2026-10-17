//go:build integration

package testenv

import (
	"context"
	"time"
)

func (c *Case) AwaitToolsStopped() {
	c.T.Helper()
	ctx, cancel := context.WithTimeout(c.Ctx, 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	// A disconnected MySQL server thread can remain blocked on a lock. Inspect
	// actual client processes instead of confusing that thread with an orphan tool.
	const probe = `for path in /proc/[0-9]*/comm; do
 read -r name < "$path" 2>/dev/null || continue
 case "$name" in mysql|mysqldump|mysqlbinlog) exit 1 ;; esac
 done; exit 0`
	for {
		_, code, err := execContainer(ctx, c.Runner, "0", []string{"/bin/sh", "-c", probe})
		c.Must(c.SafeError(err))
		if code == 0 {
			return
		}
		select {
		case <-ctx.Done():
			c.T.Fatal("official client process remained after CLI termination")
		case <-ticker.C:
		}
	}
}
