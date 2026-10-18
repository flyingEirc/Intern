package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/app/web/internal/middleware"
	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

func (h authHandlers) users(c *gin.Context) {
	users, err := h.service.Database.Users(c.Request.Context())
	if err != nil {
		internalError(c)
		return
	}
	render.JSON(c, http.StatusOK, struct {
		Items []store.User `json:"items"`
	}{Items: users})
}

func (h authHandlers) user(c *gin.Context) {
	user, err := h.service.Database.User(c.Request.Context(), c.Param("id"))
	if err != nil {
		resourceError(c, err)
		return
	}
	render.JSON(c, http.StatusOK, user)
}

func (h authHandlers) createUser(c *gin.Context) {
	var request struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if !render.DecodeJSON(c, &request) {
		return
	}
	if err := auth.ValidateUsername(request.Username); err != nil {
		validationError(c, err)
		return
	}
	if !auth.ValidRole(request.Role) {
		validationError(c, errors.New("role must be admin, operator, or viewer"))
		return
	}
	hash, err := h.service.Passwords.Hash(request.Password)
	if errors.Is(err, auth.ErrUnavailable) {
		unavailable(c)
		return
	}
	if err != nil {
		validationError(c, err)
		return
	}
	user, err := h.service.Database.CreateUser(c.Request.Context(), request.Username, hash, request.Role)
	if err != nil {
		resourceError(c, err)
		return
	}
	middleware.AuditResource(c, "user", user.ID, store.AuditDetail{Role: user.Role})
	render.JSON(c, http.StatusCreated, user)
}

type updateUserRequest struct {
	Role     *string `json:"role,omitempty"`
	Disabled *bool   `json:"disabled,omitempty"`
	Password *string `json:"password,omitempty"`
}

func (r updateUserRequest) validate() error {
	if r.Role == nil && r.Disabled == nil && r.Password == nil {
		return errors.New("at least one of role, disabled, or password is required")
	}
	if r.Role != nil && !auth.ValidRole(*r.Role) {
		return errors.New("role must be admin, operator, or viewer")
	}
	if r.Password != nil {
		return auth.ValidatePassword(*r.Password)
	}
	return nil
}

func (h authHandlers) updateUser(c *gin.Context) {
	var request updateUserRequest
	if !render.DecodeJSON(c, &request) {
		return
	}
	if err := request.validate(); err != nil {
		validationError(c, err)
		return
	}
	changes := store.UserChanges{Role: request.Role, Disabled: request.Disabled}
	if request.Password != nil {
		hash, err := h.service.Passwords.Hash(*request.Password)
		if errors.Is(err, auth.ErrUnavailable) {
			unavailable(c)
			return
		}
		if err != nil {
			internalError(c)
			return
		}
		changes.PasswordHash = &hash
	}
	user, err := h.service.Database.UpdateUser(c.Request.Context(), c.Param("id"), changes)
	if err != nil {
		resourceError(c, err)
		return
	}
	middleware.AuditResource(c, "user", user.ID, store.AuditDetail{
		Role: roleDetail(request.Role), Disabled: request.Disabled, PasswordChanged: request.Password != nil,
	})
	render.JSON(c, http.StatusOK, user)
}

func roleDetail(role *string) string {
	if role != nil {
		return *role
	}
	return ""
}

func (h authHandlers) revokeSessions(c *gin.Context) {
	id := c.Param("id")
	middleware.AuditResource(c, "user", id, store.AuditDetail{})
	if err := h.service.Database.RevokeSessions(c.Request.Context(), id); err != nil {
		resourceError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

type auditPage struct {
	Items      []store.AuditLog `json:"items"`
	NextBefore *int64           `json:"next_before"`
}

func (h authHandlers) auditLogs(c *gin.Context) {
	limit, before, err := auditPagination(c)
	if err != nil {
		validationError(c, err)
		return
	}
	entries, err := h.service.Database.AuditLogs(c.Request.Context(), before, limit+1)
	if err != nil {
		internalError(c)
		return
	}
	page := auditPage{Items: entries}
	if len(entries) > limit {
		page.Items = entries[:limit]
		id := page.Items[limit-1].ID
		page.NextBefore = &id
	}
	render.JSON(c, http.StatusOK, page)
}

func auditPagination(c *gin.Context) (int, int64, error) {
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "100"))
	if err != nil || limit < 1 || limit > 200 {
		return 0, 0, errors.New("limit must be between 1 and 200")
	}
	before := int64(0)
	if value, exists := c.GetQuery("before"); exists {
		before, err = strconv.ParseInt(value, 10, 64)
		if err != nil || before < 1 {
			return 0, 0, errors.New("before must be a positive audit ID")
		}
	}
	return limit, before, nil
}
