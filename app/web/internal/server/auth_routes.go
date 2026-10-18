package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/app/web/internal/middleware"
	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

type authHandlers struct {
	service   *auth.Service
	limiter   *auth.LoginLimiter
	passwords *auth.PasswordLimiter
}

type userResponse struct {
	User store.User `json:"user"`
}
type sessionResponse struct {
	User        store.User `json:"user"`
	Permissions []string   `json:"permissions"`
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func registerAuth(engine *gin.Engine, service *auth.Service) {
	h := authHandlers{service: service, limiter: auth.NewLoginLimiter(), passwords: auth.NewPasswordLimiter()}
	permissions := middleware.Authorization{Service: service}
	engine.POST("/api/v1/auth/login", h.login)
	self := engine.Group("/api/v1/auth", permissions.RequirePermission(auth.SessionSelf))
	self.POST("/logout", h.logout)
	self.GET("/session", h.session)
	self.PUT("/password", h.password)
	users := engine.Group("/api/v1/users", permissions.RequirePermission(auth.UsersManage))
	users.GET("", h.users)
	users.POST("", h.createUser)
	users.GET("/:id", h.user)
	users.PATCH("/:id", h.updateUser)
	users.DELETE("/:id/sessions", h.revokeSessions)
	engine.GET("/api/v1/audit-logs", permissions.RequirePermission(auth.AuditRead), h.auditLogs)
}

func (h authHandlers) login(c *gin.Context) {
	var request loginRequest
	if !render.DecodeJSON(c, &request) {
		return
	}
	middleware.LoginAttempt(c, request.Username)
	if retry := h.limiter.Allow(c.ClientIP(), request.Username, time.Now()); retry > 0 {
		c.Header("Retry-After", strconv.Itoa(retry))
		render.Error(c, http.StatusTooManyRequests, render.RateLimited, "Too many login attempts")
		return
	}
	user, valid, err := h.verifyLogin(c, request)
	if errors.Is(err, auth.ErrUnavailable) {
		unavailable(c)
		return
	}
	if err != nil {
		internalError(c)
		return
	}
	if !valid {
		render.Error(c, http.StatusUnauthorized, render.Unauthorized, "Invalid username or password")
		return
	}
	ctx := c.Request.Context()
	if err = h.service.Sessions.RenewToken(ctx); err != nil {
		internalError(c)
		return
	}
	h.service.Sessions.Put(ctx, "user_id", user.ID)
	h.service.Sessions.Put(ctx, "epoch", user.SessionEpoch)
	middleware.SetCurrentUser(c, user)
	middleware.AuditResource(c, "user", user.ID, store.AuditDetail{})
	render.JSON(c, http.StatusOK, userResponse{User: user})
}

func (h authHandlers) verifyLogin(c *gin.Context, request loginRequest) (store.User, bool, error) {
	user, err := h.service.Database.UserByUsername(c.Request.Context(), request.Username)
	if errors.Is(err, store.ErrNotFound) {
		return user, false, h.service.Passwords.VerifyDummy(request.Password)
	}
	if err != nil {
		return user, false, err
	}
	valid, err := h.service.Passwords.Verify(request.Password, user.PasswordHash)
	return user, valid && !user.Disabled, err
}

func (h authHandlers) logout(c *gin.Context) {
	middleware.AuditResource(c, "user", middleware.CurrentUser(c).ID, store.AuditDetail{})
	if err := h.service.Sessions.Destroy(c.Request.Context()); err != nil {
		internalError(c)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h authHandlers) session(c *gin.Context) {
	user := middleware.CurrentUser(c)
	render.JSON(c, http.StatusOK, sessionResponse{User: user, Permissions: auth.Permissions(user.Role)})
}

func (h authHandlers) password(c *gin.Context) {
	var request struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !render.DecodeJSON(c, &request) {
		return
	}
	if err := auth.ValidatePassword(request.NewPassword); err != nil {
		validationError(c, err)
		return
	}
	user := middleware.CurrentUser(c)
	if !allowPasswordAttempt(c, h.passwords, user.ID) {
		return
	}
	valid, ok := verifiedPassword(c, h.service.Passwords, request.CurrentPassword, user.PasswordHash)
	if !ok {
		return
	}
	if !valid {
		validationError(c, errors.New("current password is incorrect"))
		return
	}
	hash, ok := hashedPassword(c, h.service.Passwords, request.NewPassword)
	if !ok {
		return
	}
	updated, err := h.service.Database.ChangePassword(c.Request.Context(), user, hash)
	if errors.Is(err, store.ErrNotFound) {
		render.Error(c, http.StatusUnauthorized, render.Unauthorized, "Authentication required")
		return
	}
	if err != nil {
		internalError(c)
		return
	}
	if err = h.service.Sessions.RenewToken(c.Request.Context()); err != nil {
		internalError(c)
		return
	}
	h.service.Sessions.Put(c.Request.Context(), "epoch", updated.SessionEpoch)
	middleware.AuditResource(c, "user", user.ID, store.AuditDetail{PasswordChanged: true})
	c.Status(http.StatusNoContent)
}

func allowPasswordAttempt(c *gin.Context, limiter *auth.PasswordLimiter, userID string) bool {
	retry := limiter.Allow(userID, time.Now())
	if retry == 0 {
		return true
	}
	c.Header("Retry-After", strconv.Itoa(retry))
	render.Error(c, http.StatusTooManyRequests, render.RateLimited, "Too many password attempts")
	return false
}

func verifiedPassword(c *gin.Context, hasher auth.PasswordHasher, password, encoded string) (bool, bool) {
	valid, err := hasher.Verify(password, encoded)
	if errors.Is(err, auth.ErrUnavailable) {
		unavailable(c)
		return false, false
	}
	if err != nil {
		internalError(c)
		return false, false
	}
	return valid, true
}

func hashedPassword(c *gin.Context, hasher auth.PasswordHasher, password string) (string, bool) {
	hash, err := hasher.Hash(password)
	if errors.Is(err, auth.ErrUnavailable) {
		unavailable(c)
		return "", false
	}
	if err != nil {
		internalError(c)
		return "", false
	}
	return hash, true
}

func validationError(c *gin.Context, err error) {
	render.Error(c, http.StatusBadRequest, render.ValidationFailed, err.Error())
}

func internalError(c *gin.Context) {
	render.Error(c, http.StatusInternalServerError, render.Internal, "An internal error occurred")
}

func unavailable(c *gin.Context) {
	c.Header("Retry-After", strconv.Itoa(auth.UnavailableRetryAfter))
	render.Error(c, http.StatusServiceUnavailable, render.Unavailable, "Password verification is temporarily unavailable")
}

func resourceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		render.Error(c, http.StatusNotFound, render.NotFound, "User not found")
	case errors.Is(err, store.ErrConflict):
		render.Error(c, http.StatusConflict, render.Conflict, "Username already exists")
	case errors.Is(err, store.ErrLastAdmin):
		render.Error(c, http.StatusConflict, render.Conflict, "Cannot disable or demote the last enabled administrator")
	default:
		internalError(c)
	}
}
