//go:build integration

package testenv

import (
	"time"

	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

func (c *Case) CopyUntrustedCA(path string) {
	c.T.Helper()
	wrong, err := newAuthority()
	c.Must(err)
	c.Copy(path, wrong.PEM)
}

func (c *Case) FailedStartup() {
	c.T.Helper()
	volume := c.prefix + "-failed-data"
	ctr, err := mysql.Run(c.Ctx, c.Suite.Profile.Image, mysql.WithUsername("root"), mysql.WithPassword(c.rootPassword), tc.WithImagePlatform(c.Suite.Candidate.Platform), network.WithNetwork([]string{"failed-start"}, c.Network), tc.WithMounts(tc.VolumeMount(volume, "/var/lib/mysql")), tc.WithCmd("mysqld", "--intern-gate-invalid-option"), tc.WithWaitStrategy(wait.ForLog("port: 3306  MySQL Community Server").WithStartupTimeout(5*time.Second)))
	if ctr != nil {
		c.ownContainer(ctr.Container, "failed initialization", volume)
	}
	c.Assert("invalid-initialization-rejected", err != nil && ctr != nil)
	state, inspectErr := ctr.State(c.Ctx)
	c.Must(inspectErr)
	c.Assert("real-database-startup-failed", !state.Running && state.ExitCode != 0)
}
