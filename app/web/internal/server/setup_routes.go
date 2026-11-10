package server

import (
	"context"
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

// setupGateTimeout bounds how long one setup request waits for another setup
// request in this process. It does not bound the PostgreSQL row lock: that lock
// is the final authority across processes and remains inside the write
// transaction. A timed-out request is refused with Retry-After instead of
// queueing unbounded password hashes.
const setupGateTimeout = 10 * time.Second

type setupHandlers struct {
	*authHandlers
	gate chan struct{}
}

type setupStatusResponse struct {
	Required bool `json:"required"`
}

func newSetupHandlers(h *authHandlers) *setupHandlers {
	return &setupHandlers{authHandlers: h, gate: make(chan struct{}, 1)}
}

// status is the anonymous, read-only setup probe. It does not expose a username,
// a credential, or any hint about the number of existing users.
func (h *setupHandlers) status(c *gin.Context) {
	required, err := h.service.Database.SetupRequired(c.Request.Context())
	if err != nil {
		internalError(c)
		return
	}
	render.JSON(c, http.StatusOK, setupStatusResponse{Required: required})
}

// create closes setup exactly once. Existing rate limits and the per-process
// argon2id cap remain active. The per-process gate keeps ten simultaneous
// requests from all entering the hash pool before the first transaction commits;
// those that follow a successful commit see the persistent marker and answer
// setup_completed without hashing. SetupAdmin repeats the marker and user check
// under SELECT ... FOR UPDATE, so a second process cannot rely on this gate.
func (h *setupHandlers) create(c *gin.Context) {
	var request loginRequest
	if !render.DecodeJSON(c, &request) {
		return
	}
	middleware.LoginAttempt(c, request.Username)
	if !h.allowSetupAttempt(c, request.Username) || !h.acquireSetupGate(c) {
		return
	}
	defer func() { <-h.gate }()

	required, err := h.service.Database.SetupRequired(c.Request.Context())
	if err != nil {
		internalError(c)
		return
	}
	if !required {
		setupCompletedError(c)
		return
	}
	if !validSetupRequest(c, request) {
		return
	}
	hash, ok := h.hashAnonymous(c, request.Password)
	if !ok {
		return
	}
	user, ok := h.setupAdmin(c, request.Username, hash)
	if !ok {
		return
	}
	h.finishSetup(c, user)
}

func (h *setupHandlers) allowSetupAttempt(c *gin.Context, username string) bool {
	retry := h.limiter.Allow(c.ClientIP(), username, time.Now())
	if retry == 0 {
		return true
	}
	c.Header("Retry-After", strconv.Itoa(retry))
	render.Error(c, http.StatusTooManyRequests, render.RateLimited, "Too many setup attempts")
	return false
}

func (h *setupHandlers) acquireSetupGate(c *gin.Context) bool {
	gateCtx, cancel := context.WithTimeout(c.Request.Context(), setupGateTimeout)
	defer cancel()
	select {
	case h.gate <- struct{}{}:
		return true
	case <-gateCtx.Done():
		setupUnavailable(c)
		return false
	}
}

func validSetupRequest(c *gin.Context, request loginRequest) bool {
	if err := auth.ValidateUsername(request.Username); err != nil {
		validationError(c, err)
		return false
	}
	if err := auth.ValidatePassword(request.Password); err != nil {
		validationError(c, err)
		return false
	}
	return true
}

func (h *setupHandlers) hashAnonymous(c *gin.Context, password string) (string, bool) {
	hash, err := h.service.Passwords.HashAnonymous(password)
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

func (h *setupHandlers) setupAdmin(c *gin.Context, username, hash string) (store.User, bool) {
	entry := middleware.SuccessAudit(c, "auth.setup", http.StatusCreated, "user", "",
		store.AuditDetail{Role: "admin"})
	user, err := h.service.Database.SetupAdmin(c.Request.Context(), username, hash, entry)
	if errors.Is(err, store.ErrSetupCompleted) {
		setupCompletedError(c)
		return store.User{}, false
	}
	if err != nil {
		resourceError(c, err)
		return store.User{}, false
	}
	return user, true
}

func (h *setupHandlers) finishSetup(c *gin.Context, user store.User) {
	ctx := c.Request.Context()
	if err := h.service.Sessions.RenewToken(ctx); err != nil {
		h.logger.ErrorContext(ctx, "setup committed but session creation failed; log in again",
			"request_id", c.Writer.Header().Get(render.RequestIDHeader), "error", err)
		render.Error(c, http.StatusInternalServerError, render.Internal,
			"Initialization is complete; log in with the new administrator account")
		middleware.MarkAuditRecorded(c)
		return
	}
	h.service.Sessions.Put(ctx, "user_id", user.ID)
	h.service.Sessions.Put(ctx, "epoch", user.SessionEpoch)
	middleware.SetCurrentUser(c, user)
	middleware.AuditResource(c, "user", user.ID, store.AuditDetail{Role: "admin"})
	render.JSON(c, http.StatusCreated, userResponse{User: user})
	if c.Writer.Status() < 500 {
		middleware.MarkAuditRecorded(c)
	}
}

func setupCompletedError(c *gin.Context) {
	render.Error(c, http.StatusConflict, render.SetupCompleted, "Initial administrator setup is already complete")
}

func setupUnavailable(c *gin.Context) {
	c.Header("Retry-After", strconv.Itoa(auth.UnavailableRetryAfter))
	render.Error(c, http.StatusServiceUnavailable, render.Unavailable, "Initialization is temporarily unavailable")
}
