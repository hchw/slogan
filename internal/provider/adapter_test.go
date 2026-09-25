package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestListModelsUsesBearerAndParsesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("auth header missing")
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a","object":"model","owned_by":"vendor"}]}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL+"/v1", "secret", "bearer", time.Second)
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "model-a" {
		t.Fatalf("models=%+v", models)
	}
}

func TestChatForwardsBodyAndCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("auth header missing")
		}
		b := make([]byte, 1024)
		n, _ := r.Body.Read(b)
		if !strings.Contains(string(b[:n]), `"model":"m"`) {
			t.Errorf("body=%s", b[:n])
		}
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL+"/v1", "secret", "bearer", time.Second)
	resp, err := c.Chat(context.Background(), []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestConnectionTestSanitizesProviderErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "secret leaked by upstream", http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "dont-log-this", "bearer", time.Second)
	if _, err := c.Test(context.Background()); err == nil || strings.Contains(err.Error(), "secret leaked") || strings.Contains(err.Error(), "dont-log-this") {
		t.Fatalf("provider error was not sanitized: %v", err)
	}
}

func TestConnectionTestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "", "bearer", 10*time.Millisecond)
	if _, err := c.Test(context.Background()); err == nil {
		t.Fatal("expected timeout")
	}
}
