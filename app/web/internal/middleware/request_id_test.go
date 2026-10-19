package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/flyingEirc/Intern/app/web/internal/render"
)

func TestRequestIDValidationAndEcho(t *testing.T) {
	cases := []struct {
		id    string
		valid bool
	}{
		{"abc.ABC-123_", true},
		{strings.Repeat("a", 64), true},
		{"", false},
		{strings.Repeat("a", 65), false},
		{"spaces not allowed", false},
		{"中文", false},
		{"slash/id", false},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			if validRequestID(tc.id) != tc.valid {
				t.Fatal("validation mismatch")
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/healthz", nil)
			c.Request.Header.Set(render.RequestIDHeader, tc.id)
			RequestID()(c)
			got := recorder.Header().Get(render.RequestIDHeader)
			if tc.valid {
				if got != tc.id {
					t.Fatalf("ID not echoed: %q", got)
				}
				return
			}
			id, err := uuid.Parse(got)
			if err != nil || id.Version() != 7 {
				t.Fatalf("generated ID = %q, %v", got, err)
			}
		})
	}
}
