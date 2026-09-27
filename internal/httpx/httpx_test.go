package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hchw/slogan/internal/apperr"
)

func TestRequestIDMiddlewareEchoesSuppliedID(t *testing.T) {
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := RequestIDFrom(r.Context()); got != "req-test" {
			t.Fatalf("context request ID=%q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Request-Id", "req-test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Header().Get("X-Request-Id") != "req-test" || w.Code != http.StatusNoContent {
		t.Fatalf("headers=%v status=%d", w.Header(), w.Code)
	}
}

func TestBusinessEnvelopeCarriesRequestIDAndIntegerCode(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(WithRequestID(r.Context(), "req-contract"))
	w := httptest.NewRecorder()
	WriteData(w, r, http.StatusOK, map[string]any{"amountMicro": int64(123)})
	var body Envelope
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != 0 || body.RequestID != "req-contract" || w.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("unexpected envelope: %+v headers=%v", body, w.Header())
	}
	if !strings.Contains(w.Body.String(), `"amountMicro":123`) {
		t.Fatalf("micro-unit amount was not encoded as an integer: %s", w.Body.String())
	}
}

func TestWriteDataNormalizesNestedTimesToUTC(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*60*60)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	WriteData(w, r, http.StatusOK, map[string]any{"createdAt": time.Date(2026, 1, 2, 8, 0, 0, 0, loc)})
	var body struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if got := body.Data["createdAt"]; !strings.HasSuffix(got, "Z") || got != "2026-01-02T00:00:00Z" {
		t.Fatalf("timestamp is not normalized to UTC: %q", got)
	}
}

func TestOpenAIErrorContract(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	WriteOpenAIError(w, r, apperr.InvalidRequest("model", "bad model"))
	var body struct {
		Error struct {
			Code  string `json:"code"`
			Param string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusBadRequest || body.Error.Code != "invalid_request_error" || body.Error.Param != "model" {
		t.Fatalf("status=%d payload=%+v", w.Code, body)
	}
}

func TestPaginationBounds(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/?page=0&pageSize=9999", nil)
	page, size := ParsePagination(r)
	if page != 1 || size != 200 {
		t.Fatalf("page=%d size=%d", page, size)
	}
}

func TestOpenAIRateLimitSetsRetryAfter(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	WriteOpenAIError(w, r, apperr.RateLimitedAfter(17))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "17" {
		t.Fatalf("status=%d headers=%v", w.Code, w.Header())
	}
}

func TestRequestIDMiddlewareGeneratesID(t *testing.T) {
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if RequestIDFrom(r.Context()) == "" {
			t.Fatal("request ID missing from context")
		}
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Header().Get("X-Request-Id") == "" {
		t.Fatal("expected generated request ID")
	}
}
