// Package httpx contains shared HTTP helpers: JSON envelopes, request IDs,
// pagination and error mapping for the admin/user APIs and the gateway.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/hchw/slogan/internal/apperr"
	"github.com/hchw/slogan/internal/secure"
)

type ctxKey int

const requestIDKey ctxKey = 1

// RequestIDFrom returns the request ID stored in the context, if any.
func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// WithRequestID stores a request ID in the context.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID middleware assigns a request ID and echoes it in the response.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if id == "" {
			generated, err := secure.RandomToken(12)
			if err != nil {
				generated = "req_unknown"
			}
			id = "req_" + generated
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}

// Envelope is the admin/user API response wrapper.
type Envelope struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	Data      any    `json:"data,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

// Page is a paginated payload.
type Page struct {
	List     any   `json:"list"`
	Page     int   `json:"page"`
	PageSize int   `json:"pageSize"`
	Total    int64 `json:"total"`
}

// WriteData writes a success envelope.
func WriteData(w http.ResponseWriter, r *http.Request, status int, data any) {
	writeJSON(w, status, Envelope{Code: 0, Message: "ok", Data: data, RequestID: RequestIDFrom(r.Context())})
}

// WriteError writes a business error envelope derived from err.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	code := 50001
	message := "internal error"
	if e, ok := apperr.Is(err); ok {
		status = e.HTTP
		if n, convErr := strconv.Atoi(e.Code); convErr == nil {
			code = n
		}
		message = e.Message
	}
	writeJSON(w, status, Envelope{Code: code, Message: message, RequestID: RequestIDFrom(r.Context())})
}

// openAIError is the OpenAI-compatible error payload.
type openAIError struct {
	Error openAIErrorBody `json:"error"`
}

type openAIErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code"`
	// RequestID is included for support correlation.
	RequestID string `json:"request_id,omitempty"`
}

// WriteOpenAIError writes an OpenAI-compatible error response.
func WriteOpenAIError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	body := openAIErrorBody{Message: "internal error", Type: "api_error", Code: "internal_error"}
	if e, ok := apperr.Is(err); ok {
		status = e.HTTP
		body = openAIErrorBody{Message: e.Message, Type: e.Type, Param: e.Param, Code: e.Code}
		if e.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
		}
	}
	body.RequestID = RequestIDFrom(r.Context())
	writeJSON(w, status, openAIError{Error: body})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(normalizeTimes(reflect.ValueOf(v)).Interface())
}

var timeType = reflect.TypeOf(time.Time{})

// normalizeTimes ensures all time.Time values in API DTOs serialize in UTC,
// including nested maps, slices, pointers and structs.
func normalizeTimes(v reflect.Value) reflect.Value {
	if !v.IsValid() {
		return v
	}
	if v.Type() == timeType {
		return reflect.ValueOf(v.Interface().(time.Time).UTC())
	}
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		n := normalizeTimes(v.Elem())
		if n.IsValid() && n.Type().AssignableTo(v.Type()) {
			out.Set(n)
		} else if n.IsValid() && n.Type().Implements(v.Type()) {
			out.Set(n)
		} else {
			out.Set(v)
		}
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(normalizeTimes(v.Elem()))
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := 0; i < v.NumField(); i++ {
			if out.Field(i).CanSet() && v.Type().Field(i).IsExported() {
				n := normalizeTimes(v.Field(i))
				if n.IsValid() && n.Type().AssignableTo(out.Field(i).Type()) {
					out.Field(i).Set(n)
				}
			}
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(normalizeTimes(v.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(normalizeTimes(v.Index(i)))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			out.SetMapIndex(iter.Key(), normalizeTimes(iter.Value()))
		}
		return out
	default:
		return v
	}
}

// DecodeJSON decodes a JSON body with a size cap, rejecting unknown fields.
func DecodeJSON(r *http.Request, dst any, maxBytes int64) error {
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return apperr.InvalidParam("invalid JSON body: " + err.Error())
	}
	return nil
}

// ParsePagination extracts page/pageSize with sane bounds.
func ParsePagination(r *http.Request) (page, pageSize int) {
	page = atoiDefault(r.URL.Query().Get("page"), 1)
	pageSize = atoiDefault(r.URL.Query().Get("pageSize"), 20)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 200 {
		pageSize = 200
	}
	return page, pageSize
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// IsNotFound reports whether err is a not-found style application error.
func IsNotFound(err error) bool {
	e, ok := apperr.Is(err)
	return ok && e.HTTP == http.StatusNotFound
}

// ErrUnsupportedMediaType is returned by decoders for non-JSON bodies.
var ErrUnsupportedMediaType = errors.New("unsupported media type")
