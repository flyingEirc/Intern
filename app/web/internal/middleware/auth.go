package middleware

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

const currentUserKey = "intern.current_user"

// Authorization reloads the user on every protected request. Session data never
// caches roles, disabled state, or revocation decisions.
type Authorization struct{ Service *auth.Service }

func CurrentUser(c *gin.Context) store.User {
	user, ok := c.MustGet(currentUserKey).(store.User)
	if !ok {
		panic("invalid authenticated user context")
	}
	return user
}

func SetCurrentUser(c *gin.Context, user store.User) { c.Set(currentUserKey, user) }

// Authenticate is the exported form of the shared authenticated load for the
// SSE stream group, which cannot sit behind RequirePermission's buffered
// writer assumptions. It returns the loaded user, or false after writing the
// JSON response.
func (a Authorization) Authenticate(c *gin.Context) (store.User, bool) {
	return a.authenticate(c)
}

// authenticate is the shared authenticated-load path: it performs the same scs
// load, user, disabled and epoch checks for both the buffered JSON routes and
// the unbuffered SSE stream group, so the two cannot drift. It returns the
// loaded user, or false after writing the JSON response (401 on missing,
// destroyed or revoked sessions, 500 on store failures).
func (a Authorization) authenticate(c *gin.Context) (store.User, bool) {
	ctx := c.Request.Context()
	id := a.Service.Sessions.GetString(ctx, "user_id")
	if id == "" {
		render.Error(c, http.StatusUnauthorized, render.Unauthorized, "Authentication required")
		return store.User{}, false
	}
	user, err := a.Service.Database.User(ctx, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		render.Error(c, http.StatusInternalServerError, render.Internal, "Cannot load session user")
		return store.User{}, false
	}
	if errors.Is(err, store.ErrNotFound) || user.Disabled || user.SessionEpoch != a.Service.Sessions.GetInt64(ctx, "epoch") {
		if err = a.Service.Sessions.Destroy(ctx); err != nil {
			render.Error(c, http.StatusInternalServerError, render.Internal, "Cannot destroy session")
			return store.User{}, false
		}
		render.Error(c, http.StatusUnauthorized, render.Unauthorized, "Authentication required")
		return store.User{}, false
	}
	SetCurrentUser(c, user)
	return user, true
}

// RequirePermission authenticates with the shared load path and then checks one
// permission from the frozen Stage C matrix.
func (a Authorization) RequirePermission(permission string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, ok := a.authenticate(c); !ok {
			return
		}
		if !auth.HasPermission(CurrentUser(c).Role, permission) {
			render.Error(c, http.StatusForbidden, render.Forbidden, "Permission denied")
			return
		}
		c.Next()
	}
}
