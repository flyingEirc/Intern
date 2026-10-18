package middleware

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/render"
)

func RequestLog(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		logger.InfoContext(c.Request.Context(), "request complete",
			"request_id", c.Writer.Header().Get(render.RequestIDHeader),
			"method", c.Request.Method, "route", route,
			"status", c.Writer.Status(), "latency", time.Since(start),
			"client_ip", c.ClientIP())
	}
}
