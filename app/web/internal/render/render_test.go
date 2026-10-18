package render

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/api"
)

func assertError(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", contentType)
	}
	var response ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response, json.RejectUnknownMembers(true)); err != nil {
		t.Fatal(err)
	}
	if response.Error.Code != code || response.Error.Message == "" || response.Error.RequestID != "unit-request" {
		t.Fatalf("error response: %+v", response)
	}
	doc, err := openapi3.NewLoader().LoadFromData(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	var body any
	if err = json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if err = doc.Components.Schemas["ErrorResponse"].Value.VisitJSON(body); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeJSON(t *testing.T) {
	cases := []struct {
		name, contentType, body, code string
		status                        int
	}{
		{"valid", "application/json; charset=utf-8", `{"name":"test"}`, "", 0},
		{"unknown", "application/json", `{"extra":true}`, InvalidJSON, 400},
		{"malformed", "application/json", `{"name":`, InvalidJSON, 400},
		{"empty", "application/json", "", InvalidJSON, 400},
		{"duplicate", "application/json", `{"name":"a","name":"b"}`, InvalidJSON, 400},
		{"trailing", "application/json", `{"name":"a"} {}`, InvalidJSON, 400},
		{"wrong media", "text/plain", `{}`, UnsupportedMediaType, 415},
		{"missing media", "", `{}`, UnsupportedMediaType, 415},
		{"invalid media", "application/json;=", `{}`, UnsupportedMediaType, 415},
		{"oversize chunked", "application/json", `{"name":"` + strings.Repeat("x", 2048) + `"}`, PayloadTooLarge, 413},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/test", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", tc.contentType)
			c.Request.ContentLength = -1
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1024)
			c.Header(RequestIDHeader, "unit-request")
			var dst struct {
				Name string `json:"name"`
			}
			ok := DecodeJSON(c, &dst)
			if tc.code == "" {
				if !ok || dst.Name != "test" {
					t.Fatalf("decoded %q, success = %v", dst.Name, ok)
				}
				return
			}
			if ok || !c.IsAborted() {
				t.Fatal("decode error did not abort")
			}
			assertError(t, recorder, tc.status, tc.code)
		})
	}
}

func TestJSONAndError(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	JSON(c, http.StatusCreated, struct {
		Value string `json:"value"`
	}{Value: "<html>"})
	if recorder.Code != http.StatusCreated || recorder.Body.String() != `{"value":"<html>"}` {
		t.Fatalf("jsonv2 output: %d %s", recorder.Code, recorder.Body)
	}
	for _, code := range []string{BadRequest, NotFound, MethodNotAllowed, Internal, Unavailable} {
		t.Run(code, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Header(RequestIDHeader, "unit-request")
			Error(c, http.StatusBadRequest, code, "Readable error")
			assertError(t, recorder, http.StatusBadRequest, code)
		})
	}
}

func TestMarshalFailureIsInternalError(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Header(RequestIDHeader, "unit-request")
	JSON(c, http.StatusOK, make(chan int))
	assertError(t, recorder, http.StatusInternalServerError, Internal)
	if strings.Contains(recorder.Body.String(), "chan") {
		t.Fatal("marshal error exposed internal implementation")
	}
}
