package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/app/web/internal/middleware"
	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/resources"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// resourceHandlers serves the Stage C repository, source and policy resources.
type resourceHandlers struct {
	database *store.Store
	service  *resources.Service
	logger   *slog.Logger
}

func registerResources(engine *gin.Engine, database *store.Store, service *resources.Service, permissions middleware.Authorization, logger *slog.Logger) {
	h := resourceHandlers{database: database, service: service, logger: logger}
	read := permissions.RequirePermission(auth.ResourcesRead)
	writeRepositories := permissions.RequirePermission(auth.RepositoriesManage)
	writeSources := permissions.RequirePermission(auth.SourcesManage)
	writePolicies := permissions.RequirePermission(auth.PoliciesManage)

	repositories := engine.Group("/api/v1/repositories", read)
	repositories.GET("", h.repositories)
	repositories.POST("", writeRepositories, h.createRepository)
	repositories.GET("/:id", h.repository)
	repositories.PUT("/:id", writeRepositories, h.updateRepository)
	repositories.DELETE("/:id", writeRepositories, h.deleteRepository)
	repositories.POST("/:id/probe", writeRepositories, h.probeRepository)

	sources := engine.Group("/api/v1/sources", read)
	sources.GET("", h.sources)
	sources.POST("", writeSources, h.createSource)
	sources.GET("/:id", h.source)
	sources.PUT("/:id", writeSources, h.updateSource)
	sources.DELETE("/:id", writeSources, h.deleteSource)
	sources.POST("/:id/preflight", writeSources, h.preflightSource)

	policies := engine.Group("/api/v1/policies", read)
	policies.GET("", h.policies)
	policies.POST("", writePolicies, h.createPolicy)
	policies.GET("/:id", h.policy)
	policies.PUT("/:id", writePolicies, h.updatePolicy)
	policies.DELETE("/:id", writePolicies, h.deletePolicy)
	policies.POST("/:id/enable", writePolicies, h.enablePolicy)
	policies.POST("/:id/disable", writePolicies, h.disablePolicy)
}

type repositoriesPage struct {
	Items      []store.Repository `json:"items"`
	NextBefore *string            `json:"next_before"`
}

type sourcesPage struct {
	Items      []store.Source `json:"items"`
	NextBefore *string        `json:"next_before"`
}

type policiesPage struct {
	Items      []store.Policy `json:"items"`
	NextBefore *string        `json:"next_before"`
}

func (h resourceHandlers) repositories(c *gin.Context) {
	before, limit, ok := resourcePagination(c, "repository")
	if !ok {
		return
	}
	items, err := h.database.Repositories(c.Request.Context(), before, limit+1)
	if err != nil {
		internalError(c)
		return
	}
	page := repositoriesPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1].ID
		page.NextBefore = &last
	}
	render.JSON(c, http.StatusOK, page)
}

func (h resourceHandlers) sources(c *gin.Context) {
	before, limit, ok := resourcePagination(c, "source")
	if !ok {
		return
	}
	items, err := h.database.Sources(c.Request.Context(), before, limit+1)
	if err != nil {
		internalError(c)
		return
	}
	page := sourcesPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1].ID
		page.NextBefore = &last
	}
	render.JSON(c, http.StatusOK, page)
}

func (h resourceHandlers) policies(c *gin.Context) {
	before, limit, ok := resourcePagination(c, "policy")
	if !ok {
		return
	}
	items, err := h.database.Policies(c.Request.Context(), before, limit+1)
	if err != nil {
		internalError(c)
		return
	}
	page := policiesPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1].ID
		page.NextBefore = &last
	}
	render.JSON(c, http.StatusOK, page)
}

// resourcePagination applies the audit-log cursor contract to resource lists:
// a descending UUIDv7 id order with limit 1-200 (default 100).
func resourcePagination(c *gin.Context, kind string) (string, int, bool) {
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "100"))
	if err != nil || limit < 1 || limit > 200 {
		validationError(c, errors.New("limit must be between 1 and 200"))
		return "", 0, false
	}
	before := ""
	if value, exists := c.GetQuery("before"); exists {
		before = value
		if _, err = parseUUID(value); err != nil {
			validationError(c, errors.New("before must be a resource ID"))
			return "", 0, false
		}
	}
	return before, limit, true
}

func (h resourceHandlers) repository(c *gin.Context) {
	repository, err := h.database.Repository(c.Request.Context(), c.Param("id"))
	if err != nil {
		h.resourceError(c, err, "Repository")
		return
	}
	render.JSON(c, http.StatusOK, repository)
}

func (h resourceHandlers) source(c *gin.Context) {
	source, err := h.database.Source(c.Request.Context(), c.Param("id"))
	if err != nil {
		h.resourceError(c, err, "Source")
		return
	}
	render.JSON(c, http.StatusOK, source)
}

func (h resourceHandlers) policy(c *gin.Context) {
	policy, err := h.database.Policy(c.Request.Context(), c.Param("id"))
	if err != nil {
		h.resourceError(c, err, "Policy")
		return
	}
	render.JSON(c, http.StatusOK, policy)
}

func parseUUID(value string) (string, error) {
	parsed, err := uuid.Parse(value)
	if err != nil {
		return "", err
	}
	return parsed.String(), nil
}

// resourceError maps a store failure to the resource wire error.
func (h resourceHandlers) resourceError(c *gin.Context, err error, name string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		render.Error(c, http.StatusNotFound, render.NotFound, name+" not found")
	case errors.Is(err, store.ErrConflict):
		render.Error(c, http.StatusConflict, render.Conflict, name+" name already exists")
	case errors.Is(err, store.ErrReferenced):
		render.Error(c, http.StatusConflict, render.Conflict, name+" is still referenced")
	default:
		internalError(c)
	}
}
