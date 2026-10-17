//go:build integration

package integration

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"github.com/flyingEirc/Intern/app/cli/internal/testenv"
	"github.com/flyingEirc/Intern/internal/backup"
)

func scheduler(c *testenv.Case) {
	seedBasic(c)
	due := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	if time.Until(due) < 10*time.Second {
		due = due.Add(time.Minute)
	}
	command := fmt.Sprintf("schedule run --config /run/secrets/source.yaml --full-at %s --timezone UTC --incremental-every 2s", due.Format("15:04"))
	pid := c.StartCLI(command)
	c.Await("scheduler bootstrap and first increment", func() (bool, error) {
		events := scheduleEvents(c)
		return countKind(events, "logical-full") == 1 && countKind(events, "binlog") > 0, nil
	})
	c.Refuse("scheduler-directory-lock", "backup", "full", "--config", "/run/secrets/source.yaml")
	release := c.HoldWrites(c.Source)
	c.WaitForDump()
	before := len(scheduleEvents(c))
	c.WaitStable(2200*time.Millisecond, func() bool { return len(scheduleEvents(c)) == before })
	release()
	c.Await("daily full followed by an increment", func() (bool, error) {
		events := scheduleEvents(c)
		return countKind(events, "logical-full") >= 2 && events[len(events)-1].Kind == "binlog", nil
	})
	c.Signal(pid, "TERM")
	completed := len(c.List())
	c.WaitStable(2200*time.Millisecond, func() bool { return len(c.List()) == completed })
	c.Assert("daily-interval-and-stop", countKind(scheduleEvents(c), "logical-full") == 2)
	schedulerRebaseline(c, command)
}
func schedulerRebaseline(c *testenv.Case, command string) {
	c.SQL(c.Source, "USE fixture; ALTER TABLE app.records ADD COLUMN marker INT NOT NULL DEFAULT 17")
	pid := c.StartCLI(command)
	c.Await("broken-chain new full baseline", func() (bool, error) { return countKind(scheduleEvents(c), "logical-full") > 0, nil })
	c.Signal(pid, "TERM")
	var full backup.Result
	for _, result := range c.List() {
		if result.Kind == "logical-full" {
			full = result
			break
		}
	}
	c.Restore(full.ID, true)
	c.Assert("rebaseline-recovers-ddl", c.Scalar(c.Target, "SELECT marker FROM restored.records WHERE id=1") == "17")
}
func scheduleEvents(c *testenv.Case) []backup.Result {
	events := []backup.Result{}
	for _, line := range strings.Split(c.ProcessLog(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event backup.Result
		if err := json.Unmarshal([]byte(line), &event); err == nil && event.ID != "" {
			events = append(events, event)
		}
	}
	return events
}
func countKind(events []backup.Result, kind string) int {
	count := 0
	for _, event := range events {
		if event.Kind == kind {
			count++
		}
	}
	return count
}
func lifecycle(c *testenv.Case) {
	seedBasic(c)
	full := c.Backup("full")
	c.Restore(full.ID, true)
	uuid := c.Target.UUID
	c.StopDatabase(c.Target)
	c.RestartDatabase(c.Target)
	c.Assert("database-restart-persists-data", rows(c, c.Target, "restored") == "1:baseline|2:old|3:delete")
	c.Assert("restart-keeps-identity", c.Scalar(c.Target, "SELECT @@server_uuid") == uuid)
	c.Assert("independent-instances", c.Source.UUID != c.Target.UUID)
	c.FailedStartup()
}
