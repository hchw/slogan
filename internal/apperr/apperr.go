// Package apperr defines transport-agnostic application errors that carry an
// HTTP status plus either a business code (admin/user APIs) or an
// OpenAI-compatible error code (gateway API).
package apperr

import "fmt"

// Error is an application error with a stable code and HTTP status.
type Error struct {
	HTTP       int
	Code       string // business code ("40001") or OpenAI code ("invalid_api_key")
	Type       string // OpenAI error type, or business category
	Param      string
	Message    string
	RetryAfter int
	cause      error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap exposes the underlying cause.
func (e *Error) Unwrap() error { return e.cause }

// WithCause attaches an underlying cause without changing exposed fields.
func (e *Error) WithCause(err error) *Error {
	c := *e
	c.cause = err
	return &c
}

// New builds an error.
func New(httpStatus int, code, typ, param, message string) *Error {
	return &Error{HTTP: httpStatus, Code: code, Type: typ, Param: param, Message: message}
}

// Is reports whether err (or an error it wraps) is an *Error.
func Is(err error) (*Error, bool) {
	for err != nil {
		if e, ok := err.(*Error); ok {
			return e, true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return nil, false
		}
		err = u.Unwrap()
	}
	return nil, false
}

// Common business errors (admin/user APIs).
func InvalidParam(msg string) *Error {
	return New(400, "40001", "invalid_request", "", msg)
}
func MissingParam(msg string) *Error {
	return New(400, "40002", "invalid_request", "", msg)
}
func Unauthenticated(msg string) *Error {
	return New(401, "40101", "authentication", "", msg)
}
func TokenExpired() *Error { return New(401, "40102", "authentication", "", "token expired") }
func Forbidden(msg string) *Error {
	return New(403, "40301", "permission", "", msg)
}
func RoleDenied() *Error { return New(403, "40302", "permission", "", "insufficient role") }
func NotFound(msg string) *Error {
	return New(404, "40401", "not_found", "", msg)
}
func StateConflict(msg string) *Error {
	return New(409, "40901", "conflict", "", msg)
}
func Duplicate(msg string) *Error {
	return New(409, "40902", "conflict", "", msg)
}
func Internal(msg string) *Error {
	return New(500, "50001", "internal", "", msg)
}
func Unavailable(msg string) *Error {
	return New(503, "50301", "unavailable", "", msg)
}

// OpenAI-compatible gateway errors.
func OpenAI(httpStatus int, code, typ, param, msg string) *Error {
	return New(httpStatus, code, typ, param, msg)
}
func InvalidAPIKey() *Error {
	return OpenAI(401, "invalid_api_key", "authentication_error", "", "The provided API key is invalid, disabled or revoked.")
}
func InvalidRequest(param, msg string) *Error {
	return OpenAI(400, "invalid_request_error", "invalid_request_error", param, msg)
}
func ModelNotFound() *Error {
	return OpenAI(404, "model_not_found", "invalid_request_error", "model", "The requested model does not exist or is not permitted.")
}
func ContextLengthExceeded() *Error {
	return OpenAI(400, "context_length_exceeded", "invalid_request_error", "messages", "The request exceeds the model context window.")
}
func InsufficientQuota() *Error {
	return OpenAI(429, "insufficient_quota", "insufficient_quota", "", "You exceeded your current quota.")
}
func RateLimited() *Error {
	return OpenAI(429, "rate_limit_exceeded", "rate_limit_error", "", "Rate limit exceeded.")
}
func RateLimitedAfter(seconds int) *Error {
	e := RateLimited()
	if seconds < 1 {
		seconds = 1
	}
	e.RetryAfter = seconds
	return e
}
func RateLimitStoreUnavailable() *Error {
	return OpenAI(503, "service_unavailable", "api_error", "", "Rate limit storage is unavailable.")
}
func ModelUnavailable() *Error {
	return OpenAI(503, "model_unavailable", "api_error", "", "No eligible model is available for this request.")
}
func UpstreamError(msg string) *Error {
	return OpenAI(502, "upstream_error", "api_error", "", msg)
}
func UpstreamTimeout() *Error {
	return OpenAI(504, "upstream_timeout", "api_error", "", "The upstream provider timed out.")
}
func StreamError() *Error {
	return OpenAI(500, "stream_error", "api_error", "", "The stream was interrupted.")
}
func RequestInProgress(requestID string) *Error {
	e := OpenAI(409, "request_in_progress", "invalid_request_error", "", "An identical request is still in progress.")
	return e
}
func IdempotencyConflict() *Error {
	return OpenAI(409, "idempotency_conflict", "invalid_request_error", "Idempotency-Key", "This idempotency key is bound to a different request.")
}
func IdempotencyReplay() *Error {
	return OpenAI(409, "idempotency_replay", "invalid_request_error", "Idempotency-Key", "This idempotency key was already processed.")
}
