package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"

	"github.com/flyingEirc/Intern/app/web/internal/store"
)

type SessionConfig struct {
	IdleTimeout time.Duration `mapstructure:"idle_timeout"`
	Lifetime    time.Duration `mapstructure:"lifetime"`
}

func DefaultSessionConfig() SessionConfig {
	return SessionConfig{IdleTimeout: 30 * time.Minute, Lifetime: 12 * time.Hour}
}

func (c SessionConfig) Validate() error {
	if c.IdleTimeout <= 0 || c.Lifetime <= 0 || c.IdleTimeout > c.Lifetime {
		return errors.New("session.idle_timeout and session.lifetime must be positive, with idle_timeout <= lifetime")
	}
	return nil
}

// PasswordHasher hashes and verifies passwords. Implementations must perform
// exactly one dummy verification when the account does not exist. HashAnonymous
// (setup) and Verify (login) share the anonymous slot budget. Hash and
// VerifyAuthenticated serve accepted sessions and preserve that reserved capacity.
type PasswordHasher interface {
	Hash(password string) (string, error)
	HashAnonymous(password string) (string, error)
	Verify(password, encoded string) (bool, error)
	VerifyAuthenticated(password, encoded string) (bool, error)
	VerifyDummy(password string) error
}

type Service struct {
	Database  *store.Store
	Passwords PasswordHasher
	Sessions  *scs.SessionManager
}

func New(database *store.Store, config SessionConfig, params PasswordParams) (*Service, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	passwords, err := NewPasswords(params)
	if err != nil {
		return nil, err
	}
	database.SetSessionLifetime(config.Lifetime)
	manager := scs.New()
	manager.Store = database.Sessions()
	// scs 2.9.0 starts an idle default memstore cleanup goroutine in New.
	// Its StopCleanup races with initialization, so do not call it. The accepted
	// contract permits this one upstream goroutine per manager; our SQL cleanup
	// below is separately owned, cancelled, and joined before the database closes.
	manager.IdleTimeout, manager.Lifetime = config.IdleTimeout, config.Lifetime
	manager.Cookie.Name = "intern_session"
	manager.Cookie.HttpOnly, manager.Cookie.Secure = true, true
	manager.Cookie.SameSite, manager.Cookie.Path = http.SameSiteLaxMode, "/"
	return &Service{Database: database, Passwords: passwords, Sessions: manager}, nil
}

// StartCleanup returns a cancellation-and-join function, including when the
// owner's context is cancelled. The database remains open until this returns.
func (s *Service) StartCleanup(ctx context.Context, interval time.Duration, logger *slog.Logger) func() {
	cleanupCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Database.Sessions().Cleanup(cleanupCtx, interval, logger)
	}()
	return func() { cancel(); <-done }
}

const (
	SessionSelf        = "session:self"
	UsersManage        = "users:manage"
	AuditRead          = "audit:read"
	RepositoriesManage = "repositories:manage"
	SourcesManage      = "sources:manage"
	PoliciesManage     = "policies:manage"
	ResourcesRead      = "resources:read"
	// W4 additions frozen in the contract: viewers read runs, operators and
	// admins trigger, cancel and retry replicas.
	RunsRead      = "runs:read"
	RunsTrigger   = "runs:trigger"
	RunsCancel    = "runs:cancel"
	ReplicasRetry = "replicas:retry"
	// W5 additions frozen in the contract: viewers read restores, operators
	// plan them, and only administrators execute or cancel them.
	RestoresRead    = "restores:read"
	RestoresPlan    = "restores:plan"
	RestoresExecute = "restores:execute"
)

// Permissions is the frozen Stage C+D matrix. Administrator is a superset for
// the source and policy resources: both roles may manage them. Only an
// administrator may write repositories or execute/cancel restores, matching the
// architectural role split (operator owns sources, policies and restore plans;
// admin owns repositories and restore execution). Viewer is read-only and
// anonymous holds nothing.
func Permissions(role string) []string {
	switch role {
	case "admin":
		return []string{
			SessionSelf, UsersManage, AuditRead, RepositoriesManage, SourcesManage, PoliciesManage, ResourcesRead,
			RunsRead, RunsTrigger, RunsCancel, ReplicasRetry,
			RestoresRead, RestoresPlan, RestoresExecute,
		}
	case "operator":
		return []string{
			SessionSelf, SourcesManage, PoliciesManage, ResourcesRead, RunsRead, RunsTrigger, RunsCancel, ReplicasRetry,
			RestoresRead, RestoresPlan,
		}
	case "viewer":
		return []string{SessionSelf, ResourcesRead, RunsRead, RestoresRead}
	default:
		return []string{}
	}
}

func HasPermission(role, permission string) bool {
	for _, allowed := range Permissions(role) {
		if allowed == permission {
			return true
		}
	}
	return false
}
