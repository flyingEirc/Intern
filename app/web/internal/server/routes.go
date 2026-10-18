// Package server assembles the Web routes and HTTP server lifecycle.
package server

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/middleware"
	"github.com/flyingEirc/Intern/app/web/internal/render"
)

type Readiness interface {
	Ready(context.Context) error
}

type StatusResponse struct {
	Status string `json:"status"`
}

func NewEngine(database Readiness, logger *slog.Logger, trustedProxies []string) (*gin.Engine, error) {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.HandleMethodNotAllowed = true
	engine.RedirectTrailingSlash = false
	engine.RedirectFixedPath = false
	if err := engine.SetTrustedProxies(trustedProxies); err != nil {
		return nil, err
	}
	engine.Use(middleware.RequestID(), middleware.RequestLog(logger), middleware.Recovery(logger), middleware.BodyLimit())
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
	return engine, nil
}
