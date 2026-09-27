package laya

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClassifySuccessSendsOnlyClassificationCopy(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer local-secret" {
			t.Errorf("missing sidecar auth")
		}
		var body struct {
			Input string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = body.Input
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choice":"coding","confidence":0.91,"version":"pinned-test"}`))
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, APIKey: "local-secret", Enabled: true, Timeout: time.Second, MinConfidence: .6, MaxInputRunes: 8})
	res := c.Classify(context.Background(), "123456789-secret")
	if res.Intent != "coding" || res.Fallback() || res.ClassifierVersion != "pinned-test" || !res.Truncated {
		t.Fatalf("unexpected result: %+v", res)
	}
	if got != "12345678" || strings.Contains(got, "secret") {
		t.Fatalf("input was not truncated/minimized: %q", got)
	}
}

func TestClassifyFallsBackOnLowConfidenceAndBadLabel(t *testing.T) {
	for _, response := range []string{`{"choice":"coding","confidence":0.2}`, `{"choice":"unknown_intent","confidence":0.99}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(response)) }))
		c := New(Config{BaseURL: srv.URL, Enabled: true, Timeout: time.Second, MinConfidence: .6})
		res := c.Classify(context.Background(), "hello")
		if res.Intent != "general" || !res.Fallback() {
			t.Fatalf("expected safe fallback for %s, got %+v", response, res)
		}
		srv.Close()
	}
}

func TestClassifyFallsBackOnMalformedAndHTTPError(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusOK, "{bad json", ReasonBadResponse},
		{http.StatusServiceUnavailable, `{"error":"down"}`, ReasonUnavailable},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		res := New(Config{BaseURL: srv.URL, Enabled: true, Timeout: time.Second}).Classify(context.Background(), "hello")
		srv.Close()
		if res.Intent != "general" || res.FallbackReason != tc.want {
			t.Fatalf("status=%d result=%+v", tc.status, res)
		}
	}
}

func TestClassifyDoesNotRetryAndUsesConfiguredTimeout(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); time.Sleep(80 * time.Millisecond) }))
	defer srv.Close()
	res := New(Config{BaseURL: srv.URL, Enabled: true, Timeout: 10 * time.Millisecond}).Classify(context.Background(), "hello")
	if res.FallbackReason != ReasonTimeout {
		t.Fatalf("expected timeout, got %+v", res)
	}
	if calls.Load() != 1 {
		t.Fatalf("classifier automatically retried %d times", calls.Load())
	}
}

func TestReadyRequiresLoadedCheckpointNotJustLiveness(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		ready  bool
	}{
		{"live process without checkpoint", http.StatusOK, `{"status":"loading","model_loaded":false,"version":"0.3.20"}`, false},
		{"checkpoint ready", http.StatusOK, `{"status":"ready","model_loaded":true,"version":"0.3.20"}`, true},
		{"explicit classifier flag", http.StatusOK, `{"classifier_ready":true}`, true},
		{"sidecar down", http.StatusServiceUnavailable, `{}`, false},
		{"legacy liveness-only body", http.StatusOK, `ok`, true},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/health" {
				t.Errorf("unexpected path %s", r.URL.Path)
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		state := New(Config{BaseURL: srv.URL, Enabled: true}).Ready(context.Background())
		srv.Close()
		if state.ClassifierReady != tc.ready {
			t.Fatalf("%s: classifierReady=%v want %v (state=%+v)", tc.name, state.ClassifierReady, tc.ready, state)
		}
		if tc.status == http.StatusOK && !state.Alive {
			t.Fatalf("%s: live process must report alive", tc.name)
		}
	}
}

func TestCircuitBreakerOpensThenRecovers(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Enabled: true, Timeout: time.Second})
	for i := 0; i < 5; i++ {
		if res := c.Classify(context.Background(), "hello"); res.FallbackReason == ReasonCircuitOpen {
			t.Fatalf("circuit opened early at call %d", i+1)
		}
	}
	before := calls.Load()
	res := c.Classify(context.Background(), "hello")
	if res.FallbackReason != ReasonCircuitOpen {
		t.Fatalf("expected open circuit, got %+v", res)
	}
	if calls.Load() != before {
		t.Fatal("open circuit still called the sidecar")
	}
}

func TestClassifyFallsBackWhenDisabled(t *testing.T) {
	c := New(Config{Enabled: false})
	res := c.Classify(context.Background(), "sensitive text")
	if res.Intent != "general" || res.FallbackReason != ReasonDisabled {
		t.Fatalf("unexpected: %+v", res)
	}
}

func TestClassifyTimeoutFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(100 * time.Millisecond) }))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Enabled: true, Timeout: 10 * time.Millisecond})
	res := c.Classify(context.Background(), "hello")
	if res.Intent != "general" || res.FallbackReason != ReasonTimeout {
		t.Fatalf("expected timeout fallback, got %+v", res)
	}
}
