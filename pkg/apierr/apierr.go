// Package apierr is the single error model shared by every service.
//
// Rules (.harness/rules/20-go-style.md):
//   - Handlers return *apierr.Error; the transport layer renders it.
//   - Client-visible messages never carry internal detail (SQL, stack, host).
//   - Unknown errors always become CodeInternal with a generic message.
package apierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Code is the stable, machine-readable error code. Clients switch on it.
type Code string

const (
	CodeInvalidArgument    Code = "invalid_argument"
	CodeUnauthenticated    Code = "unauthenticated"
	CodePermissionDenied   Code = "permission_denied"
	CodeNotFound           Code = "not_found"
	CodeConflict           Code = "conflict"
	CodeFailedPrecondition Code = "failed_precondition"
	CodeResourceExhausted  Code = "resource_exhausted"
	CodeUnavailable        Code = "unavailable"
	CodeInternal           Code = "internal"
)

// Error is the only error type allowed to cross a service boundary.
type Error struct {
	Code    Code
	Message string
	Details map[string]string

	cause error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

// WithDetail attaches a client-safe hint (field name, limit, ...).
func (e *Error) WithDetail(key, value string) *Error {
	if e.Details == nil {
		e.Details = map[string]string{}
	}
	e.Details[key] = value
	return e
}

// WithCause attaches internal context. It is logged, never serialised.
func (e *Error) WithCause(cause error) *Error {
	e.cause = cause
	return e
}

// HTTPStatus maps code onto wire status. One code, one status, forever.
func (e *Error) HTTPStatus() int {
	switch e.Code {
	case CodeInvalidArgument:
		return http.StatusBadRequest
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodePermissionDenied:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict:
		return http.StatusConflict
	case CodeFailedPrecondition:
		return http.StatusPreconditionFailed
	case CodeResourceExhausted:
		return http.StatusTooManyRequests
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func New(code Code, message string) *Error { return &Error{Code: code, Message: message} }

func Invalid(message string) *Error          { return New(CodeInvalidArgument, message) }
func Unauthenticated(message string) *Error  { return New(CodeUnauthenticated, message) }
func PermissionDenied(message string) *Error { return New(CodePermissionDenied, message) }
func NotFound(message string) *Error         { return New(CodeNotFound, message) }
func Conflict(message string) *Error         { return New(CodeConflict, message) }
func Precondition(message string) *Error     { return New(CodeFailedPrecondition, message) }
func Unavailable(message string) *Error      { return New(CodeUnavailable, message) }
func Internal(message string) *Error         { return New(CodeInternal, message) }

// As normalises any error into an *Error. Unknown values become internal so
// implementation detail never leaks.
func As(err error) *Error {
	if err == nil {
		return nil
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return Internal("internal error").WithCause(err)
}

// Payload is the wire form of an error.
type Payload struct {
	Code    Code              `json:"code" doc:"Stable machine-readable error code."`
	Message string            `json:"message" doc:"Human-readable explanation, safe to show to a player."`
	Details map[string]string `json:"details,omitempty" doc:"Optional client-safe hints, e.g. the offending field."`
}

type envelope struct {
	Error Payload `json:"error"`
}

// Write renders err using the documented error envelope.
func Write(w http.ResponseWriter, err error) {
	apiErr := As(err)
	body, marshalErr := json.Marshal(envelope{Error: Payload{
		Code:    apiErr.Code,
		Message: apiErr.Message,
		Details: apiErr.Details,
	}})
	if marshalErr != nil {
		http.Error(w, `{"error":{"code":"internal","message":"internal error"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Error-Code", string(apiErr.Code))
	w.WriteHeader(apiErr.HTTPStatus())
	_, _ = w.Write(body)
}
