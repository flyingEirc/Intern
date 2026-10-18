// Package middleware contains the Web protocol middleware.
package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/flyingEirc/Intern/app/web/internal/render"
)

func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !validRequestIDChar(c) {
			return false
		}
	}
	return true
}

func validRequestIDChar(c rune) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
}

func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(render.RequestIDHeader)
		if !validRequestID(id) {
			generated, err := uuid.NewV7()
			if err != nil {
				render.Error(c, http.StatusInternalServerError, render.Internal, "Cannot generate request identity")
				return
			}
			id = generated.String()
		}
		c.Header(render.RequestIDHeader, id)
		c.Next()
	}
}
