// Package server assembles the Web routes and HTTP server lifecycle.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/app/web/internal/middleware"
	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/resources"
)

type Readiness interface {
	Ready(context.Context) error
}

type StatusResponse struct {
	Status string `json:"status"`
}

// DispatcherControl is the dispatch surface the HTTP layer may call. The
// server package never touches the dispatcher's loops directly.

// NewEngine serves every route with the deployment capabilities authentication
// already carries. A deployment without a KEK or preflight settings still serves:
// only the operations that need them answer 503. The runs routes and the
// authenticated SSE stream appear only when runs is non-nil.
func NewEngine(database Readiness, logger *slog.Logger, trustedProxies []string, authentication *auth.Service, opts ...EngineOption) (*gin.Engine, error) {
	return NewEngineWithResources(database, logger, trustedProxies, authentication,
		&resources.Service{Store: authentication.Database}, nil, opts...)
}

// EngineOption configures optional engine behaviour such as the SSE heartbeat.
type EngineOption func(*engineOptions)

type engineOptions struct {
	sseHeartbeat time.Duration
	restores     RestoreControl
}

// WithSSEHeartbeat sets the SSE comment interval. Production leaves this unset
// so the contract default of 15s applies. Tests pass a short interval so
// heartbeat and revocation cases stay inside the default test timeout.
func WithSSEHeartbeat(d time.Duration) EngineOption {
	return func(o *engineOptions) {
		if d > 0 {
			o.sseHeartbeat = d
		}
	}
}

// WithRestores attaches the restore API. Production serve always attaches the
// dispatcher; an unconfigured deployment then answers 503 for plan and execute
// while the read routes keep serving stored rows. Tests that do not exercise
// restores omit the option and the routes stay absent.
func WithRestores(control RestoreControl) EngineOption {
	return func(o *engineOptions) { o.restores = control }
}

// NewEngineWithResources is NewEngine with explicit preflight and credential
// settings. Callers outside the server package supply them from configuration.
// runs may be nil: the runs APIs are then absent (unit tests of the resource
// surface rely on this; production serve always passes the dispatcher).
func NewEngineWithResources(database Readiness, logger *slog.Logger, trustedProxies []string, authentication *auth.Service, service *resources.Service, runs DispatcherControl, opts ...EngineOption) (*gin.Engine, error) {
	options := engineOptions{sseHeartbeat: sseHeartbeat}
	for _, opt := range opts {
		opt(&options)
	}
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.HandleMethodNotAllowed = true
	engine.RedirectTrailingSlash = false
	engine.RedirectFixedPath = false
	if err := engine.SetTrustedProxies(trustedProxies); err != nil {
		return nil, err
	}
	// The inner Recovery turns panics in the session buffer and handlers into one
	// 500 before Audit reads the status. The outer Recovery covers panics in
	// cross-origin rejection and audit recording. RequestLog stays outside both
	// so the access log sees that final status.
	engine.Use(middleware.RequestID(), middleware.RequestLog(logger),
		middleware.Recovery(logger),
		middleware.CrossOriginRejection(authentication.Database, logger),
		middleware.Audit(authentication.Database, logger),
		middleware.Recovery(logger),
		middleware.Session(authentication.Sessions, logger),
		middleware.BodyLimit())
	engine.NoRoute(func(c *gin.Context) {
		render.Error(c, http.StatusNotFound, render.NotFound, "Route not found")
	})
	engine.NoMethod(func(c *gin.Context) {
		render.Error(c, http.StatusMethodNotAllowed, render.MethodNotAllowed, "Method not allowed")
	})
	engine.GET("/healthz", func(c *gin.Context) {
		render.JSON(c, http.StatusOK, StatusResponse{Status: "ok"})
	})
	engine.GET("/readyz", func(c *gin.Context) {
		if err := database.Ready(c.Request.Context()); err != nil {
			render.Error(c, http.StatusServiceUnavailable, render.Unavailable, "Metadata database is not ready")
			return
		}
		render.JSON(c, http.StatusOK, StatusResponse{Status: "ready"})
	})
	registerAuth(engine, authentication, logger)
	registerResources(engine, authentication.Database, service, middleware.Authorization{Service: authentication}, logger)
	if runs != nil {
		registerRuns(engine, authentication.Database, runs, authentication,
			middleware.Authorization{Service: authentication}, logger, options.sseHeartbeat)
	}
	if options.restores != nil {
		registerRestores(engine, authentication.Database, options.restores, authentication,
			middleware.Authorization{Service: authentication}, logger)
	}
	return engine, nil
}
