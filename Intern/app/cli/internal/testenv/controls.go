//go:build integration

package testenv

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	networktypes "github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	tc "github.com/testcontainers/testcontainers-go"

	"github.com/flyingEirc/IotProject/internal/fsutil"
)

func (c *Case) LastLog(db *Database) string {
	c.T.Helper()
	rows, err := db.SQL.QueryContext(c.Ctx, "SHOW BINARY LOGS")
	c.Must(c.SafeError(err))
	defer func() { c.Must(rows.Close()) }()
	var latest string
	for rows.Next() {
		var name, encrypted string
		var size int64
		c.Must(rows.Scan(&name, &size, &encrypted))
		latest = name
	}
	c.Must(rows.Err())
	c.Assert("binlog-observed", latest != "")
	return latest
}
func (c *Case) StartCLI(command string) int {
	c.T.Helper()
	// Callers supply fixed test commands/config paths; no credentials or SQL are shell arguments.
	script := "/usr/local/bin/intern " + command + " > /work/state/process.log 2>&1 & echo $!"
	output, code, err := execContainer(c.Ctx, c.Runner, "10001", []string{"/bin/sh", "-c", script})
	c.Must(c.SafeError(err))
	c.Assert("process-started", code == 0)
	pid, err := strconv.Atoi(strings.TrimSpace(output))
	c.Must(err)
	c.Assert("process-identifier", pid > 1)
	return pid
}
func (c *Case) Signal(pid int, signal string) {
	c.T.Helper()
	if signal != "TERM" && signal != "KILL" {
		c.T.Fatal("unsupported fixture signal")
	}
	c.Root("/bin/sh", "-c", "kill -"+signal+" "+strconv.Itoa(pid))
	c.Await("CLI process exits", func() (bool, error) {
		_, code, err := execContainer(c.Ctx, c.Runner, "0", []string{"/bin/sh", "-c", "kill -0 " + strconv.Itoa(pid)})
		return code != 0, err
	})
}
func (c *Case) ProcessLog() string { c.T.Helper(); return string(c.Read("/work/state/process.log")) }
func (c *Case) Disconnect(db *Database) {
	c.T.Helper()
	c.Must(changeNetwork(c.Ctx, c.Network.Name, db.Container.GetContainerID(), false, db.Alias))
}
func (c *Case) Reconnect(db *Database) {
	c.T.Helper()
	c.Must(changeNetwork(c.Ctx, c.Network.Name, db.Container.GetContainerID(), true, db.Alias))
	c.Must(db.SQL.Close())
	c.Must(c.connectDatabase(db))
	c.Must(c.waitDatabase(db))
}
func changeNetwork(ctx context.Context, network, id string, connect bool, alias string) (retErr error) {
	docker, err := tc.NewDockerClientWithOpts(ctx)
	if err != nil {
		return err
	}
	defer fsutil.Close(&retErr, docker)
	if !connect {
		_, err = docker.NetworkDisconnect(ctx, network, client.NetworkDisconnectOptions{Container: id, Force: true})
		return err
	}
	_, err = docker.NetworkConnect(ctx, network, client.NetworkConnectOptions{Container: id, EndpointConfig: &networktypes.EndpointSettings{Aliases: []string{alias}}})
	return err
}
func (c *Case) HoldWrites(db *Database) func() {
	c.T.Helper()
	conn, err := db.SQL.Conn(c.Ctx)
	c.Must(err)
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, unlockErr := conn.ExecContext(ctx, "UNLOCK TABLES")
		c.Must(errors.Join(unlockErr, conn.Close()))
	}
	c.T.Cleanup(release)
	_, err = conn.ExecContext(c.Ctx, "LOCK TABLES app.records WRITE")
	c.Must(c.SafeError(err))
	return release
}
func (c *Case) WaitForDump() {
	c.T.Helper()
	c.Await("mysqldump reaches real server and staging", func() (bool, error) {
		_, code, err := execContainer(c.Ctx, c.Runner, "0", []string{"/bin/sh", "-c", "for file in /work/state/work-*/dump.sql; do test -f \"$file\" && exit 0; done; exit 1"})
		if err != nil || code != 0 {
			return false, err
		}
		var count int
		err = c.Source.SQL.QueryRowContext(c.Ctx, "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE USER='intern_backup' AND COMMAND <> 'Sleep'").Scan(&count)
		return count > 0, c.SafeError(err)
	})
}
func (c *Case) WaitStable(duration time.Duration, check func() bool) {
	c.T.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !check() {
			c.T.Fatal("observable state changed after process termination")
		}
		select {
		case <-timer.C:
			return
		case <-c.Ctx.Done():
			c.T.Fatal(c.Ctx.Err())
		case <-ticker.C:
		}
	}
}
