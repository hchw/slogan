package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hchw/slogan/internal/api"
	"github.com/hchw/slogan/internal/audit"
	"github.com/hchw/slogan/internal/bootstrap"
	"github.com/hchw/slogan/internal/config"
	"github.com/hchw/slogan/internal/db"
	"github.com/hchw/slogan/internal/evaluation"
	"github.com/hchw/slogan/internal/gateway"
	"github.com/hchw/slogan/internal/laya"
	"github.com/hchw/slogan/internal/routing"
)

// actorAdmin is the audit identity used for control-plane actions in the flow.
func actorAdmin() audit.Actor {
	return audit.Actor{Type: "admin", ID: 1, RequestID: "e2e-flow"}
}

// TestFullFlowRegisterToRollback walks the complete first-release flow through
// the real HTTP surfaces: register -> key -> redeem -> direct call -> reserve/
// settle -> score publish with multi-node ACK -> auto routing -> rollback.
func TestFullFlowRegisterToRollback(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	redisAddr := os.Getenv("TEST_REDIS_ADDR")
	if dbURL == "" || redisAddr == "" {
		t.Skip("TEST_DATABASE_URL and TEST_REDIS_ADDR required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Upstream stand-in: returns trusted usage and evaluation-shaped content.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"e2e-model","object":"model"}]}`))
			return
		}
		caps := map[string]float64{}
		for _, dim := range evaluation.Dimensions {
			caps[dim] = 0.9
		}
		payload, _ := json.Marshal(caps)
		content := `{"capabilities":` + string(payload) + `,"confidence":0.9}`
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "e2e", "object": "chat.completion", "model": "e2e-model",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))
	defer upstream.Close()

	// Serialize schema resets with the other integration packages. The lock
	// connection stays checked out for the whole test so no other package can
	// reset the schema concurrently.
	pool, err := db.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_lock(742991884)`); err != nil {
		lock.Release()
		t.Fatal(err)
	}
	defer func() {
		_, _ = lock.Exec(context.Background(), `SELECT pg_advisory_unlock(742991884)`)
		lock.Release()
	}()
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Env: "test", DatabaseURL: dbURL, RedisAddr: redisAddr,
		ProviderSecretKey: bytes.Repeat([]byte{7}, 32), SessionTTL: 24 * time.Hour,
		RequestMaxOutputTokens: 256,
		Evaluation:             config.EvaluationConfig{MaxTokens: 2000, Concurrency: 5, Timeout: 5 * time.Second, MinPublishRatio: 0.8, MaxRefreshesPerDay: 10},
		Laya:                   config.LayaConfig{Enabled: false},
		MetricsToken:           "e2e-scrape-token",
	}
	bundle, err := bootstrap.Build(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bundle.Close)
	if err := bundle.Redis.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	admin, err := bundle.Auth.Bootstrap(ctx, "e2e-admin", "E2eAdminSecurePass123!", "E2E Admin")
	if err != nil {
		t.Fatal(err)
	}
	_ = admin

	apiHandler := api.New(bundle.DB, cfg, bundle.Auth, bundle.Provider, bundle.Model, bundle.Quota,
		bundle.Request, bundle.Usage, bundle.Policy, bundle.Eval, bundle.Audit).Handler()
	router := routing.NewRouter(bundle.Model, bundle.Policy, bundle.Eval, bundle.DB)
	gw := gateway.New(bundle.DB, cfg, bundle.Auth, bundle.Model, bundle.Provider, bundle.Quota,
		bundle.Request, bundle.Usage, router, laya.New(laya.Config{Enabled: false}), bundle.Redis, bundle.Policy)
	gw.SetMetrics(bundle.Metrics.Counters())
	gatewayHandler := gw.Handler()

	call := func(handler http.Handler, method, path, token, body string) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		var reader *bytes.Reader
		if body == "" {
			reader = bytes.NewReader(nil)
		} else {
			reader = bytes.NewReader([]byte(body))
		}
		req := httptest.NewRequest(method, path, reader)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var decoded map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
		return rec, decoded
	}
	data := func(t *testing.T, rec *httptest.ResponseRecorder, decoded map[string]any) map[string]any {
		t.Helper()
		payload, ok := decoded["data"].(map[string]any)
		if !ok {
			t.Fatalf("unexpected envelope (%d): %s", rec.Code, rec.Body.String())
		}
		return payload
	}

	// 1. admin session + provider + model + catalog prices
	rec, decoded := call(apiHandler, http.MethodPost, "/admin/api/v1/auth/login", "", `{"username":"e2e-admin","password":"E2eAdminSecurePass123!"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin login=%d %s", rec.Code, rec.Body.String())
	}
	adminToken := data(t, rec, decoded)["token"].(string)

	rec, decoded = call(apiHandler, http.MethodPost, "/admin/api/v1/providers", adminToken,
		fmt.Sprintf(`{"name":"e2e-provider","baseUrl":%q,"secret":"e2e-secret","type":"openai_compatible","authType":"bearer"}`, upstream.URL+"/v1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("provider create=%d %s", rec.Code, rec.Body.String())
	}
	providerID := int64(data(t, rec, decoded)["id"].(float64))
	rec, decoded = call(apiHandler, http.MethodPost, fmt.Sprintf("/admin/api/v1/providers/%d/test", providerID), adminToken, `{}`)
	if rec.Code != http.StatusOK || data(t, rec, decoded)["reachable"] != true {
		t.Fatalf("provider test=%d %s", rec.Code, rec.Body.String())
	}
	rec, decoded = call(apiHandler, http.MethodPost, "/admin/api/v1/models", adminToken,
		fmt.Sprintf(`{"providerId":%d,"name":"E2E Model","modelKey":"e2e-model","contextLength":8192,"supportsStream":true,"inputModalities":["text"],"inputPriceMicro":100,"outputPriceMicro":200,"chargeInputMicro":1000,"chargeOutputMicro":2000}`, providerID))
	if rec.Code != http.StatusOK {
		t.Fatalf("model create=%d %s", rec.Code, rec.Body.String())
	}
	modelID := int64(data(t, rec, decoded)["id"].(float64))

	// 2. user registers and receives a one-time key
	rec, decoded = call(apiHandler, http.MethodPost, "/user/api/v1/auth/register", "", `{"email":"flow@example.test","password":"FlowSecurePass123!","nickname":"Flow"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("register=%d %s", rec.Code, rec.Body.String())
	}
	userToken := data(t, rec, decoded)["token"].(string)
	rec, decoded = call(apiHandler, http.MethodPost, "/user/api/v1/api-keys", userToken, `{"name":"flow","rateLimitPerMin":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("key create=%d %s", rec.Code, rec.Body.String())
	}
	apiKey := data(t, rec, decoded)["key"].(string)
	rec, _ = call(apiHandler, http.MethodGet, "/user/api/v1/api-keys", userToken, "")
	if bytes.Contains(rec.Body.Bytes(), []byte(apiKey)) {
		t.Fatal("plaintext API key must never be listed again")
	}

	// 3. nothing is routable before a score version exists
	rec, decoded = call(gatewayHandler, http.MethodPost, "/v1/chat/completions", apiKey, `{"model":"auto","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("auto route before publish=%d %s", rec.Code, rec.Body.String())
	}
	if errPayload, ok := decoded["error"].(map[string]any); !ok || errPayload["code"] != "model_unavailable" {
		t.Fatalf("unexpected error payload: %s", rec.Body.String())
	}

	// 4. redeem a package code, then call a model directly
	rec, decoded = call(apiHandler, http.MethodPost, "/admin/api/v1/quota-packages", adminToken, `{"name":"e2e-package","faceValueMicro":5000000}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("package create=%d %s", rec.Code, rec.Body.String())
	}
	packageID := int64(data(t, rec, decoded)["id"].(float64))
	rec, decoded = call(apiHandler, http.MethodPost, fmt.Sprintf("/admin/api/v1/quota-packages/%d/codes", packageID), adminToken, `{"quantity":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code generate=%d %s", rec.Code, rec.Body.String())
	}
	code := data(t, rec, decoded)["codes"].([]any)[0].(string)
	rec, decoded = call(apiHandler, http.MethodPost, "/user/api/v1/redemption/redeem", userToken, fmt.Sprintf(`{"code":%q}`, code))
	if rec.Code != http.StatusOK || data(t, rec, decoded)["grantedMicro"].(float64) != 5_000_000 {
		t.Fatalf("redeem=%d %s", rec.Code, rec.Body.String())
	}
	// Redeeming the same code twice must be refused.
	rec, _ = call(apiHandler, http.MethodPost, "/user/api/v1/redemption/redeem", userToken, fmt.Sprintf(`{"code":%q}`, code))
	if rec.Code == http.StatusOK {
		t.Fatal("duplicate redemption succeeded")
	}

	rec, decoded = call(gatewayHandler, http.MethodPost, "/v1/chat/completions", apiKey, `{"model":"e2e-model","messages":[{"role":"user","content":"hello"}],"max_tokens":16}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("direct call=%d %s", rec.Code, rec.Body.String())
	}
	requestID := rec.Header().Get("X-Request-Id")
	// 10 prompt + 5 completion tokens: charge 10*1000 + 5*2000 = 20000.
	var status string
	var charge, reserved int64
	if err := bundle.DB.QueryRow(ctx, `SELECT status, charge_micro, reserved_micro FROM request_record WHERE request_id=$1`, requestID).Scan(&status, &charge, &reserved); err != nil {
		t.Fatal(err)
	}
	// `reserved_micro` on the record is the reservation snapshot taken at request
	// start; the live frozen balance must be released by settlement.
	if status != "success" || charge != 20_000 || reserved <= 0 {
		t.Fatalf("request settlement status=%s charge=%d reservation snapshot=%d", status, charge, reserved)
	}
	acct, err := bundle.Quota.Account(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if acct.BalanceMicro != 5_000_000-20_000 || acct.ReservedMicro != 0 {
		t.Fatalf("account after call=%+v", acct)
	}
	var events int
	if err := bundle.DB.QueryRow(ctx, `SELECT count(*) FROM usage_event WHERE request_id=$1`, requestID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("usage events=%d", events)
	}

	// 5. evaluation refresh -> candidate -> publish requires node ACK
	task, err := bundle.Eval.StartRefresh(ctx, actorAdmin())
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Eval.ProcessItem(ctx, task.ID, modelID); err != nil {
		t.Fatal(err)
	}
	rows, err := bundle.Eval.LatestTasks(ctx, 1)
	if err != nil || len(rows) != 1 || rows[0].Status != "completed" {
		t.Fatalf("refresh task state=%+v err=%v", rows, err)
	}

	// Two gateway nodes join and load the published snapshot before readiness.
	for _, nodeID := range []string{"e2e-node-a", "e2e-node-b"} {
		id := nodeID
		go gw.RunNode(ctx, id)
	}
	// The candidate is not yet published, so nodes report version 0; publish now
	// and wait for both to ACK the new version.
	if _, err := bundle.Eval.Publish(ctx, task.ScoreVersionID, "e2e publish", actorAdmin()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	acked := false
	for time.Now().Before(deadline) {
		nodes, err := bundle.Redis.Nodes(ctx)
		if err == nil {
			all := len(nodes) == 2
			for _, node := range nodes {
				if node.Version != task.ScoreVersionID || !node.Ready {
					all = false
				}
			}
			if all {
				acked = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !acked {
		nodes, _ := bundle.Redis.Nodes(ctx)
		t.Fatalf("nodes did not ACK the published score version: %+v", nodes)
	}
	active, err := bundle.Eval.ActiveVersion(ctx)
	if err != nil || active != task.ScoreVersionID {
		t.Fatalf("active version=%d err=%v", active, err)
	}

	// 6. auto routing now works and records the pinned score version
	rec, decoded = call(gatewayHandler, http.MethodPost, "/v1/chat/completions", apiKey, `{"model":"auto","messages":[{"role":"user","content":"帮我写一个排序函数"}],"max_tokens":16}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("auto route=%d %s", rec.Code, rec.Body.String())
	}
	autoID := rec.Header().Get("X-Request-Id")
	var pinned int64
	if err := bundle.DB.QueryRow(ctx, `SELECT COALESCE(score_version_id,0) FROM request_record WHERE request_id=$1`, autoID).Scan(&pinned); err != nil {
		t.Fatal(err)
	}
	if pinned != task.ScoreVersionID {
		t.Fatalf("auto request score version=%d want %d", pinned, task.ScoreVersionID)
	}
	rec, decoded = call(apiHandler, http.MethodGet, "/admin/api/v1/nodes", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("node health=%d %s", rec.Code, rec.Body.String())
	}
	nodePayload := data(t, rec, decoded)
	if nodePayload["activeScoreVersionId"].(float64) != float64(task.ScoreVersionID) {
		t.Fatalf("node health payload=%v", nodePayload)
	}
	if list, ok := nodePayload["list"].([]any); !ok || len(list) != 2 {
		t.Fatalf("node list=%v", nodePayload["list"])
	}

	// 7. user-visible records match the ledger
	rec, decoded = call(apiHandler, http.MethodGet, "/user/api/v1/requests", userToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("requests=%d %s", rec.Code, rec.Body.String())
	}
	history, ok := data(t, rec, decoded)["list"].([]any)
	if !ok {
		t.Fatalf("request history=%v", data(t, rec, decoded)["list"])
	}
	// The unroutable auto attempt is recorded too, with its terminal status.
	byStatus := map[string]int{}
	for _, row := range history {
		entry, _ := row.(map[string]any)
		statusValue, _ := entry["status"].(string)
		byStatus[statusValue]++
		// The console separates the requested model from the actual model.
		if statusValue == "success" && (entry["requestedModel"] == nil || entry["model"] == nil) {
			t.Fatalf("request row must separate requested and actual model: %v", entry)
		}
	}
	if byStatus["success"] != 2 || byStatus["model_unavailable"] != 1 {
		t.Fatalf("request history statuses=%v", byStatus)
	}
	rec, decoded = call(apiHandler, http.MethodGet, "/user/api/v1/usage", userToken, "")
	usagePayload := data(t, rec, decoded)
	if usagePayload["inputTokens"].(float64) != 20 || usagePayload["outputTokens"].(float64) != 10 {
		t.Fatalf("usage aggregation=%v", usagePayload)
	}
	if usagePayload["chargeMicro"].(float64) != 40_000 {
		t.Fatalf("usage charge=%v", usagePayload)
	}

	// 8. publish a second version, then roll back to the first
	second, err := bundle.Eval.StartRefresh(ctx, actorAdmin())
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Eval.ProcessItem(ctx, second.ID, modelID); err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.Eval.Publish(ctx, second.ScoreVersionID, "second", actorAdmin()); err != nil {
		t.Fatal(err)
	}
	rolled, err := bundle.Eval.Rollback(ctx, task.ScoreVersionID, "e2e rollback", actorAdmin())
	if err != nil || rolled != "published" {
		t.Fatalf("rollback=%s err=%v", rolled, err)
	}
	active, err = bundle.Eval.ActiveVersion(ctx)
	if err != nil || active != task.ScoreVersionID {
		t.Fatalf("active after rollback=%d err=%v", active, err)
	}
	rec, decoded = call(apiHandler, http.MethodGet, "/user/api/v1/quota", userToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("quota=%d %s", rec.Code, rec.Body.String())
	}
	if data(t, rec, decoded)["availableMicro"].(float64) != float64(5_000_000-40_000) {
		t.Fatalf("final available balance=%v", data(t, rec, decoded))
	}
	// 9. metrics wiring: internal scrape token required, request and node series present
	metricsReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsRec := httptest.NewRecorder()
	bundle.Metrics.TokenHandler(cfg.MetricsToken)(metricsRec, metricsReq)
	if metricsRec.Code != http.StatusUnauthorized {
		t.Fatalf("metrics without token=%d", metricsRec.Code)
	}
	metricsReq.Header.Set("Authorization", "Bearer "+cfg.MetricsToken)
	metricsRec = httptest.NewRecorder()
	bundle.Metrics.TokenHandler(cfg.MetricsToken)(metricsRec, metricsReq)
	if metricsRec.Code != http.StatusOK {
		t.Fatalf("metrics with token=%d", metricsRec.Code)
	}
	metricsBody := metricsRec.Body.String()
	for _, want := range []string{
		`slogan_gateway_requests_total{status="success"}`,
		"slogan_gateway_score_version_active",
		"slogan_gateway_nodes_serving",
		"slogan_gateway_settlements_total",
	} {
		if !strings.Contains(metricsBody, want) {
			t.Fatalf("metrics missing %s:\n%s", want, metricsBody)
		}
	}
	// No label may carry user identifiers or prompt content.
	if strings.Contains(metricsBody, "flow@example.test") || strings.Contains(metricsBody, "帮我写") {
		t.Fatal("metrics leaked identifying data")
	}

	// The rollback is audited with the operator-supplied reason.
	var reason string
	if err := bundle.DB.QueryRow(ctx, `SELECT reason FROM audit_log WHERE action='evaluation.rollback' ORDER BY id DESC LIMIT 1`).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "e2e rollback" {
		t.Fatalf("rollback audit reason=%q", reason)
	}
}
