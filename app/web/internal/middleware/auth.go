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

func (a Authorization) RequirePermission(permission string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !a.authenticate(c) {
			return
		}
		if !auth.HasPermission(CurrentUser(c).Role, permission) {
			render.Error(c, http.StatusForbidden, render.Forbidden, "Permission denied")
			return
		}
		c.Next()
	}
}

func (a Authorization) authenticate(c *gin.Context) bool {
	ctx := c.Request.Context()
	id := a.Service.Sessions.GetString(ctx, "user_id")
	if id == "" {
		render.Error(c, http.StatusUnauthorized, render.Unauthorized, "Authentication required")
		return false
	}
	user, err := a.Service.Database.User(ctx, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		render.Error(c, http.StatusInternalServerError, render.Internal, "Cannot load session user")
		return false
	}
	if errors.Is(err, store.ErrNotFound) || user.Disabled || user.SessionEpoch != a.Service.Sessions.GetInt64(ctx, "epoch") {
		if err = a.Service.Sessions.Destroy(ctx); err != nil {
			render.Error(c, http.StatusInternalServerError, render.Internal, "Cannot destroy session")
			return false
		}
		render.Error(c, http.StatusUnauthorized, render.Unauthorized, "Authentication required")
		return false
	}
	SetCurrentUser(c, user)
	return true
}
