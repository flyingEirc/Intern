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
)

type ErrorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
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
	c.Abort()
	JSON(c, status, ErrorResponse{Error: ErrorDetail{
		Code: code, Message: message, RequestID: c.Writer.Header().Get(RequestIDHeader),
	}})
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
