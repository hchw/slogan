package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hchw/slogan/internal/audit"
	"github.com/hchw/slogan/internal/auth"
	"github.com/hchw/slogan/internal/config"
	"github.com/hchw/slogan/internal/db"
	"github.com/hchw/slogan/internal/evaluation"
	"github.com/hchw/slogan/internal/model"
	"github.com/hchw/slogan/internal/policy"
	"github.com/hchw/slogan/internal/provider"
	"github.com/hchw/slogan/internal/quota"
	"github.com/hchw/slogan/internal/redisx"
	"github.com/hchw/slogan/internal/requestrec"
	"github.com/hchw/slogan/internal/secure"
	"github.com/hchw/slogan/internal/usage"
)

func TestAdminNodeHealthAndEvaluationPermissions(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	redisAddr := os.Getenv("TEST_REDIS_ADDR")
	if url == "" || redisAddr == "" {
		t.Skip("TEST_DATABASE_URL and TEST_REDIS_ADDR required")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	lock, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_lock(742991884)`); err != nil {
		lock.Release()
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = lock.Exec(context.Background(), `SELECT pg_advisory_unlock(742991884)`); lock.Release() })
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatal(err)
	}
	rdb, err := redisx.Open(ctx, redisAddr, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	_ = rdb.FlushDB(ctx).Err()

	aud := audit.New(pool)
	authSvc := auth.New(pool, 24*time.Hour)
	if err := authSvc.EnsureSeedRoles(ctx); err != nil {
		t.Fatal(err)
	}
	root, err := authSvc.Bootstrap(ctx, "root", "AdminSecurePass123!", "Root")
	if err != nil {
		t.Fatal(err)
	}
	rootToken, _, err := authSvc.CreateAdminSession(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	newAdmin := func(username, roleCode string) string {
		t.Helper()
		hash, err := secure.HashPassword("AuditorSecurePass123!")
		if err != nil {
			t.Fatal(err)
		}
		var id, roleID int64
		if err := pool.QueryRow(ctx, `INSERT INTO admin_user (username,display_name,password_hash) VALUES ($1,$1,$2) RETURNING id`, username, hash).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT id FROM role WHERE code=$1`, roleCode).Scan(&roleID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO admin_user_role(admin_user_id,role_id) VALUES ($1,$2)`, id, roleID); err != nil {
			t.Fatal(err)
		}
		token, _, err := authSvc.CreateAdminSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	auditorToken := newAdmin("audit-reader", "auditor")
	modelAdminToken := newAdmin("model-admin", "model_admin")

	cfg := &config.Config{ProviderSecretKey: make([]byte, 32), SessionTTL: 24 * time.Hour}
	prov := provider.New(pool, cfg.ProviderSecretKey, aud)
	models := model.New(pool, prov, aud)
	pol := policy.New(pool)
	if err := pol.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	evalSvc := evaluation.New(pool, rdb, models, prov, pol, aud, cfg.Evaluation)
	handler := New(pool, cfg, authSvc, prov, models, quota.New(pool, aud), requestrec.NewService(pool), usage.New(pool), pol, evalSvc, aud).Handler()

	// A healthy node and a node that serves traffic with an unready classifier.
	if err := rdb.Heartbeat(ctx, "gw-1", 12, true, false); err != nil {
		t.Fatal(err)
	}
	if err := rdb.HeartbeatClassifier(ctx, "gw-1", redisx.ClassifierState{Ready: true, Version: "0.3.20"}); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Heartbeat(ctx, "gw-2", 11, true, false); err != nil {
		t.Fatal(err)
	}
	if err := rdb.HeartbeatClassifier(ctx, "gw-2", redisx.ClassifierState{Ready: false, Reason: "checkpoint_not_ready"}); err != nil {
		t.Fatal(err)
	}

	get := func(token, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	post := func(token, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}

	if w := get(auditorToken, "/admin/api/v1/nodes"); w.Code != http.StatusForbidden {
		t.Fatalf("auditor node list=%d body=%s", w.Code, w.Body.String())
	}
	nodesResponse := get(rootToken, "/admin/api/v1/nodes")
	if nodesResponse.Code != http.StatusOK {
		t.Fatalf("root node list=%d body=%s", nodesResponse.Code, nodesResponse.Body.String())
	}
	var payload struct {
		Code int `json:"code"`
		Data struct {
			ActiveScoreVersionID int64 `json:"activeScoreVersionId"`
			List                 []struct {
				ID               string `json:"id"`
				ScoreVersionID   int64  `json:"scoreVersionId"`
				Ready            bool   `json:"ready"`
				ClassifierReady  bool   `json:"classifierReady"`
				Degraded         bool   `json:"degraded"`
				ClassifierReason string `json:"classifierReason"`
			} `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(nodesResponse.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data.List) != 2 {
		t.Fatalf("node list=%+v", payload.Data)
	}
	byID := map[string]bool{}
	reasons := map[string]string{}
	for _, n := range payload.Data.List {
		byID[n.ID] = n.Degraded
		reasons[n.ID] = n.ClassifierReason
	}
	if byID["gw-1"] {
		t.Fatal("healthy classifier reported degraded")
	}
	if !byID["gw-2"] || reasons["gw-2"] != "checkpoint_not_ready" {
		t.Fatalf("classifier-degraded node not reported: %+v", payload.Data.List)
	}

	// Usage and audit stay within their own permissions.
	if w := get(auditorToken, "/admin/api/v1/usage/models"); w.Code != http.StatusOK {
		t.Fatalf("auditor usage=%d body=%s", w.Code, w.Body.String())
	}
	if w := get(modelAdminToken, "/admin/api/v1/usage/models"); w.Code != http.StatusForbidden {
		t.Fatalf("model admin usage=%d body=%s", w.Code, w.Body.String())
	}
	if w := get(auditorToken, "/admin/api/v1/providers"); w.Code != http.StatusForbidden {
		t.Fatalf("auditor providers=%d body=%s", w.Code, w.Body.String())
	}

	// Evaluation publish and refresh retry require their own permissions.
	if w := post(modelAdminToken, "/admin/api/v1/evaluation/versions/1/publish", `{"reason":"x"}`); w.Code != http.StatusForbidden {
		t.Fatalf("model admin publish=%d body=%s", w.Code, w.Body.String())
	}
	// The auditor may read evaluation state but must not re-arm work.
	if w := post(auditorToken, "/admin/api/v1/evaluation/refresh/1/retry", `{}`); w.Code != http.StatusForbidden {
		t.Fatalf("auditor retry=%d body=%s", w.Code, w.Body.String())
	}
	if w := get(modelAdminToken, "/admin/api/v1/evaluation/refresh"); w.Code != http.StatusOK {
		t.Fatalf("model admin refresh list=%d body=%s", w.Code, w.Body.String())
	}
}
