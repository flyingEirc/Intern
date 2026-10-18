package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/render"
)

const MaxRequestBodyBytes int64 = 1 << 20

func BodyLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.ContentLength > MaxRequestBodyBytes {
			render.Error(c, http.StatusRequestEntityTooLarge, render.PayloadTooLarge, "Request body exceeds the size limit")
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxRequestBodyBytes)
		c.Next()
	}
}
