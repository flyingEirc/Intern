// Package storetest contains test-only control-plane fixtures. It is never
// imported by production code and does not provide a bootstrap bypass.
package storetest

import (
	"errors"
	"testing"

	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// SeedAdmin creates an administrator and closes one-time setup in the same
// store transaction. It uses SetupAdmin for an empty database, then the normal
// audited user creation path for additional fixture users.
func SeedAdmin(t testing.TB, database *store.Store, username, hash string) store.User {
	t.Helper()
	user, err := database.SetupAdmin(t.Context(), username, hash,
		store.SystemAudit("auth.setup", store.AuditDetail{Role: "admin"}))
	if errors.Is(err, store.ErrSetupCompleted) {
		user, err = database.CreateUser(t.Context(), username, hash, "admin",
			store.SystemAudit("users.create", store.AuditDetail{Role: "admin"}))
	}
	if err != nil {
		t.Fatal(err)
	}
	return user
}
