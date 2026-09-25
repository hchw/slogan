package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestConnectionTestTLSFailureDoesNotLeakSecret(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	secret := "provider-secret-value"
	c := NewClient(srv.URL, secret, "bearer", time.Second)
	if _, err := c.Test(context.Background()); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("expected sanitized TLS error, got %v", err)
	}
}
