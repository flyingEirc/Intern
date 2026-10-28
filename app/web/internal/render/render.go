// Package render provides the Web JSON wire format without Gin's JSON encoder.
package render

import (
	"encoding/json/v2"
	"errors"
	"mime"
	"net/http"

	"github.com/gin-gonic/gin"
)

const RequestIDHeader = "X-Request-ID"

const (
	BadRequest           = "bad_request"
	InvalidJSON          = "invalid_json"
	UnsupportedMediaType = "unsupported_media_type"
	PayloadTooLarge      = "payload_too_large"
	NotFound             = "not_found"
	MethodNotAllowed     = "method_not_allowed"
	Internal             = "internal"
	Unavailable          = "unavailable"
	Unauthorized         = "unauthorized"
	Forbidden            = "forbidden"
	CrossOriginRejected  = "cross_origin_rejected"
	RateLimited          = "rate_limited"
	Conflict             = "conflict"
	ValidationFailed     = "validation_failed"
	PreflightFailed      = "preflight_failed"
	// W5 restore codes. restore_refused carries the restore engine's fixed
	// machine code in error.detail.code; plan_changed is a digest mismatch;
	// confirmation_failed is a failed password re-entry.
	RestoreRefused     = "restore_refused"
	PlanChanged        = "plan_changed"
	ConfirmationFailed = "confirmation_failed"
)

// ErrorRefusal is the machine-readable cause of a refused operation. It is
// rendered as error.detail and never carries free-form engine text.
type ErrorRefusal struct {
	Code string `json:"code"`
}

type ErrorDetail struct {
	Code      string        `json:"code"`
	Message   string        `json:"message"`
	RequestID string        `json:"request_id"`
	Preflight *Preflight    `json:"preflight,omitempty"`
	Detail    *ErrorRefusal `json:"detail,omitempty"`
}

// Preflight is the structured, secret-safe outcome of one preflight run. It is
// both the 200 body of a standalone preflight and the optional detail of a
// preflight_failed error.
type Preflight struct {
	OK       bool               `json:"ok"`
	Checks   []string           `json:"checks"`
	Warnings []PreflightWarning `json:"warnings"`
	Error    *PreflightError    `json:"error,omitempty"`
}

// PreflightWarning is a stable code with an actionable explanation.
type PreflightWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// PreflightError names the failed check. Missing holds known privilege names
// only; it never carries SQL, paths, replies or credentials.
type PreflightError struct {
	Check   string   `json:"check"`
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Missing []string `json:"missing,omitempty"`
}

type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

func JSON(c *gin.Context, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		Error(c, http.StatusInternalServerError, Internal, "An internal error occurred")
		return
	}
	c.Data(status, "application/json; charset=utf-8", data)
}

func Error(c *gin.Context, status int, code, message string) {
	errorWith(c, status, ErrorDetail{Code: code, Message: message})
}

// ErrorPreflight renders a preflight_failed error with its structured outcome.
func ErrorPreflight(c *gin.Context, status int, message string, preflight Preflight) {
	errorWith(c, status, ErrorDetail{Code: PreflightFailed, Message: message, Preflight: &preflight})
}

// ErrorRefused renders a refusal with its machine-readable detail code.
func ErrorRefused(c *gin.Context, status int, code, message, refusal string) {
	errorWith(c, status, ErrorDetail{Code: code, Message: message, Detail: &ErrorRefusal{Code: refusal}})
}

func errorWith(c *gin.Context, status int, detail ErrorDetail) {
	c.Abort()
	detail.RequestID = c.Writer.Header().Get(RequestIDHeader)
	JSON(c, status, ErrorResponse{Error: detail})
}

// HTTPError preserves the same wire format for errors outside the Gin engine.
func HTTPError(w http.ResponseWriter, status int, code, message string) error {
	data, err := json.Marshal(ErrorResponse{Error: ErrorDetail{
		Code: code, Message: message, RequestID: w.Header().Get(RequestIDHeader),
	}})
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, err = w.Write(data)
	return err
}

// DecodeJSON rejects unknown fields, trailing JSON values, and unsupported media.
// The request-body middleware supplies the limit, including for chunked requests.
func DecodeJSON(c *gin.Context, dst any) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		Error(c, http.StatusUnsupportedMediaType, UnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	err = json.UnmarshalRead(c.Request.Body, dst, json.RejectUnknownMembers(true))
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		Error(c, http.StatusRequestEntityTooLarge, PayloadTooLarge, "Request body exceeds the size limit")
		return false
	}
	Error(c, http.StatusBadRequest, InvalidJSON, "Request body must be valid JSON with no unknown fields")
	return false
}
