package evaluation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPMembershipRequiresExplicitRemovalConfirmation(t *testing.T) {
	var seen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/nodes/gw-1/drain" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer lb-secret" {
			t.Errorf("missing auth")
		}
		seen = true
		_ = json.NewEncoder(w).Encode(map[string]bool{"removed": true})
	}))
	defer srv.Close()
	a := NewHTTPMembershipAdapter(srv.URL, "lb-secret", 0)
	removed, err := a.DrainAndConfirm(context.Background(), "gw-1")
	if err != nil || !removed || !seen {
		t.Fatalf("removed=%v seen=%v err=%v", removed, seen, err)
	}
}

func TestNoMembershipAdapterNeverConfirms(t *testing.T) {
	removed, err := (noMembershipAdapter{}).DrainAndConfirm(context.Background(), "gw")
	if err != nil || removed {
		t.Fatalf("no-op membership must not confirm removal: %v %v", removed, err)
	}
}
