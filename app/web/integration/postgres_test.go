//go:build integration

package integration

import (
	"context"
	"net/url"
	"regexp"
	"testing"
	"time"

	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/flyingEirc/Intern/internal/pgtest"
)

// workerPostgres keeps the real control plane on the worker's private network.
// Workers receive only an administrative URL; pgtest.New creates and cleans up
// an isolated database for each fixture without accessing Docker from a worker.
func workerPostgres(t *testing.T, ctx context.Context, nw *tc.DockerNetwork, env map[string]string) map[string]string {
	t.Helper()
	password := randomSecret(t)
	alias := "intern-pg-" + password[:12]
	server, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{
		Image:    pgtest.Image,
		Networks: []string{nw.Name}, NetworkAliases: map[string][]string{nw.Name: {alias}},
		Env:        map[string]string{"POSTGRES_USER": "intern", "POSTGRES_PASSWORD": password, "POSTGRES_DB": "postgres"},
		WaitingFor: wait.ForExec([]string{"pg_isready", "-U", "intern", "-d", "postgres"}).WithStartupTimeout(2 * time.Minute),
	}, Started: true})
	if server != nil {
		tc.CleanupContainer(t, server, tc.StopTimeout(5*time.Second))
	}
	require(t, err)
	if env == nil {
		env = make(map[string]string)
	}
	dsn := &url.URL{Scheme: "postgres", User: url.UserPassword("intern", password), Host: alias + ":5432", Path: "/postgres", RawQuery: "sslmode=disable"}
	env["INTERN_TEST_POSTGRES_DSN"] = dsn.String()
	identity, err := server.Inspect(ctx)
	require(t, err)
	t.Logf("PG control_plane image=%s container_image=%s database_isolation=random cleanup=drop-force", pgtest.Image, identity.Image)
	return env
}

func postgresURLPresent(text string) bool {
	return postgresURL.MatchString(text)
}

var postgresURL = regexp.MustCompile(`(?i)postgres(?:ql)?://[^\s]+`)
