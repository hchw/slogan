package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hchw/slogan/internal/audit"
	"github.com/hchw/slogan/internal/auth"
	"github.com/hchw/slogan/internal/config"
	"github.com/hchw/slogan/internal/db"
	"github.com/hchw/slogan/internal/evaluation"
	"github.com/hchw/slogan/internal/laya"
	"github.com/hchw/slogan/internal/model"
	"github.com/hchw/slogan/internal/policy"
	"github.com/hchw/slogan/internal/provider"
	"github.com/hchw/slogan/internal/quota"
	"github.com/hchw/slogan/internal/redisx"
	"github.com/hchw/slogan/internal/requestrec"
	"github.com/hchw/slogan/internal/routing"
	"github.com/hchw/slogan/internal/usage"
)

func TestGatewayDirectStreamingIdempotencyAndSettlement(t *testing.T) {
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

	var providerCalls atomic.Int64
	var extensionSeen atomic.Bool
	var upstreamCancelled atomic.Bool
	var modelID atomic.Int64
	holdReached := make(chan struct{})
	releaseHold := make(chan struct{})
	var holdOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"fake-model","object":"model"}]}`))
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		providerCalls.Add(1)
		var body struct {
			Stream          bool   `json:"stream"`
			ReasoningEffort string `json:"reasoning_effort"`
			Messages        []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.ReasoningEffort == "high" {
			extensionSeen.Store(true)
		}
		text := ""
		if len(body.Messages) > 0 {
			text = body.Messages[len(body.Messages)-1].Content
		}
		if strings.Contains(text, "change price") && modelID.Load() != 0 {
			_, _ = pool.Exec(r.Context(), `UPDATE model SET input_price_micro=9000, output_price_micro=9000, charge_input_micro=90000, charge_output_micro=90000 WHERE id=$1`, modelID.Load())
		}
		if strings.Contains(text, "hold for pin") {
			holdOnce.Do(func() { close(holdReached) })
			<-releaseHold
		}
		if strings.Contains(text, "upstream error") {
			http.Error(w, "secret provider detail", http.StatusBadGateway)
			return
		}
		if body.Stream && strings.Contains(text, "client disconnect") {
			w.Header().Set("Content-Type", "text/event-stream")
			if f, ok := w.(http.Flusher); ok {
				_, _ = fmt.Fprint(w, "data: {\"id\":\"disconnect\",\"object\":\"chat.completion.chunk\",\"model\":\"fake-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
				f.Flush()
			}
			<-r.Context().Done()
			upstreamCancelled.Store(true)
			return
		}
		if body.Stream && strings.Contains(text, "stream break") {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "hijacking unavailable", 500)
				return
			}
			conn, rw, err := hijacker.Hijack()
			if err != nil {
				return
			}
			_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n")
			_, _ = rw.WriteString("data: {\"id\":\"broken\",\"object\":\"chat.completion.chunk\",\"model\":\"fake-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
			_, _ = rw.WriteString("data: {\"id\":\"broken\",\"object\":\"chat.completion.chunk\",\"model\":\"fake-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n")
			_ = rw.Flush()
			_ = conn.Close()
			return
		}
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"id\":\"s\",\"object\":\"chat.completion.chunk\",\"model\":\"fake-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
			if strings.Contains(text, "no usage") {
				_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
				return
			}
			_, _ = fmt.Fprint(w, "data: {\"id\":\"s\",\"object\":\"chat.completion.chunk\",\"model\":\"fake-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n")
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		response := map[string]any{"id": "n", "object": "chat.completion", "model": "fake-model", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}}}
		if !strings.Contains(text, "no usage") {
			response["usage"] = map[string]int{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer upstream.Close()

	aud := audit.New(pool)
	cfg := &config.Config{ProviderSecretKey: make([]byte, 32), SessionTTL: 24 * time.Hour, RequestMaxOutputTokens: 100, RateLimits: config.RateLimitConfig{}}
	authSvc := auth.New(pool, cfg.SessionTTL)
	prov := provider.New(pool, cfg.ProviderSecretKey, aud)
	models := model.New(pool, prov, aud)
	q := quota.New(pool, aud)
	reqs := requestrec.NewService(pool)
	usg := usage.New(pool)
	pol := policy.New(pool)
	if err := pol.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	eval := evaluation.New(pool, rdb, models, prov, pol, aud, cfg.Evaluation)
	u, err := authSvc.RegisterUser(ctx, "gateway@example.test", "GatewaySecurePass123!", "")
	if err != nil {
		t.Fatal(err)
	}
	keyView, apiKey, err := authSvc.CreateAPIKey(ctx, u.ID, "integration", nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Adjust(ctx, audit.Actor{Type: "system", RequestID: "seed"}, u.ID, 1_000_000, "test quota grant", ""); err != nil {
		t.Fatal(err)
	}
	p, err := prov.Create(ctx, provider.Input{Name: "mock", BaseURL: upstream.URL + "/v1", AuthType: "bearer", Secret: "integration-secret", ExtensionAllowlist: []string{"reasoning_effort"}}, audit.Actor{Type: "system"})
	if err != nil {
		t.Fatal(err)
	}
	costIn, costOut, chargeIn, chargeOut := int64(100), int64(200), int64(1000), int64(2000)
	m, err := models.Create(ctx, model.Input{ProviderID: p.ID, Name: "Displayed Model", ModelKey: "fake-model", ContextLength: 10000, InputModalities: []string{"text"}, SupportsStream: true, InputPriceMicro: &costIn, OutputPriceMicro: &costOut, ChargeInputMicro: &chargeIn, ChargeOutputMicro: &chargeOut}, audit.Actor{Type: "system"})
	if err != nil {
		t.Fatal(err)
	}
	modelID.Store(m.ID)
	router := routing.NewRouter(models, pol, eval, pool)
	svc := New(pool, cfg, authSvc, models, prov, q, reqs, usg, router, laya.New(laya.Config{Enabled: false}), rdb, pol)
	handler := svc.Handler()

	modelsRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsRequest.Header.Set("Authorization", "Bearer "+apiKey)
	modelsResponse := httptest.NewRecorder()
	handler.ServeHTTP(modelsResponse, modelsRequest)
	var modelList struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if modelsResponse.Code != http.StatusOK || json.Unmarshal(modelsResponse.Body.Bytes(), &modelList) != nil || modelList.Object != "list" || len(modelList.Data) != 2 || modelList.Data[0].ID != "auto" {
		t.Fatalf("OpenAI models contract: status=%d body=%s", modelsResponse.Code, modelsResponse.Body.String())
	}

	do := func(payload, idem string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload))
		r.Header.Set("Authorization", "Bearer "+apiKey)
		r.Header.Set("Content-Type", "application/json")
		if idem != "" {
			r.Header.Set("Idempotency-Key", idem)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	autoCallsBefore := providerCalls.Load()
	unsupportedExtension := do(`{"model":"fake-model","messages":[{"role":"user","content":"hello"}],"unlisted_field":true}`, "")
	if unsupportedExtension.Code != http.StatusBadRequest {
		t.Fatalf("nonallowlisted extension=%d %s", unsupportedExtension.Code, unsupportedExtension.Body.String())
	}
	auto := do(`{"model":"auto","messages":[{"role":"user","content":"general request"}],"max_tokens":10}`, "")
	if auto.Code != http.StatusServiceUnavailable || !strings.Contains(auto.Body.String(), "model_unavailable") {
		t.Fatalf("unscored auto route=%d %s", auto.Code, auto.Body.String())
	}
	if providerCalls.Load() != autoCallsBefore {
		t.Fatal("auto route without a published score called provider")
	}

	// Auto routing needs an authoritative ability matrix from a published version.
	publishVersion := func(score float64) int64 {
		var versionID int64
		if err := pool.QueryRow(ctx, `INSERT INTO score_version (status,total_models,valid_models,success_ratio)
            VALUES ('building',1,1,1) RETURNING id`).Scan(&versionID); err != nil {
			t.Fatal(err)
		}
		for _, dim := range evaluation.Dimensions {
			if _, err := pool.Exec(ctx, `INSERT INTO model_capability
                (score_version_id,model_id,dimension,score,self_score,peer_score,confidence)
                VALUES ($1,$2,$3,$4,$4,$4,$4)`, versionID, m.ID, dim, score); err != nil {
				t.Fatal(err)
			}
		}
		published, err := eval.Publish(ctx, versionID, "integration publish", audit.Actor{Type: "system", RequestID: "gateway-test"})
		if err != nil || published != "published" {
			t.Fatalf("publish=%s err=%v", published, err)
		}
		return versionID
	}
	firstVersion := publishVersion(0.95)
	autoRouted := do(`{"model":"auto","messages":[{"role":"user","content":"route me"}],"max_tokens":10}`, "")
	if autoRouted.Code != http.StatusOK {
		t.Fatalf("auto route with published score=%d %s", autoRouted.Code, autoRouted.Body.String())
	}
	var pinnedVersion int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(score_version_id,0) FROM request_record WHERE request_id=$1`, autoRouted.Header().Get("X-Request-Id")).Scan(&pinnedVersion); err != nil {
		t.Fatal(err)
	}
	if pinnedVersion != firstVersion {
		t.Fatalf("request score version pin=%d want %d", pinnedVersion, firstVersion)
	}

	// An in-flight request must complete on the version it started with, even if a
	// new version is published while upstream is still working.
	inFlight := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		inFlight <- do(`{"model":"auto","messages":[{"role":"user","content":"hold for pin"}],"max_tokens":10}`, "")
	}()
	select {
	case <-holdReached:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request never reached the provider")
	}
	secondVersion := publishVersion(0.10)
	close(releaseHold)
	inFlightResponse := <-inFlight
	if inFlightResponse.Code != http.StatusOK {
		t.Fatalf("in-flight request=%d %s", inFlightResponse.Code, inFlightResponse.Body.String())
	}
	var inFlightVersion int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(score_version_id,0) FROM request_record WHERE request_id=$1`, inFlightResponse.Header().Get("X-Request-Id")).Scan(&inFlightVersion); err != nil {
		t.Fatal(err)
	}
	if inFlightVersion != firstVersion {
		t.Fatalf("in-flight request moved to version %d, want %d", inFlightVersion, firstVersion)
	}
	newRequest := do(`{"model":"auto","messages":[{"role":"user","content":"route me"}],"max_tokens":10}`, "")
	if newRequest.Code != http.StatusOK {
		t.Fatalf("post-publish auto route=%d %s", newRequest.Code, newRequest.Body.String())
	}
	var newPin int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(score_version_id,0) FROM request_record WHERE request_id=$1`, newRequest.Header().Get("X-Request-Id")).Scan(&newPin); err != nil {
		t.Fatal(err)
	}
	if newPin != secondVersion {
		t.Fatalf("new request score version=%d want %d", newPin, secondVersion)
	}
	first := do(`{"model":"fake-model","messages":[{"role":"user","content":"hello"}],"max_tokens":10,"reasoning_effort":"high"}`, "idem-one")
	if first.Code != http.StatusOK {
		t.Fatalf("direct call status=%d body=%s", first.Code, first.Body.String())
	}
	if !extensionSeen.Load() {
		t.Fatal("allowlisted provider extension was not forwarded")
	}
	var response struct {
		Model string `json:"model"`
		Usage struct {
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Model != "Displayed Model" || response.Usage.Prompt != 3 || response.Usage.Completion != 2 {
		t.Fatalf("response=%+v", response)
	}
	callsAfterFirst := providerCalls.Load()
	replay := do(`{"model":"fake-model","messages":[{"role":"user","content":"hello"}],"max_tokens":10,"reasoning_effort":"high"}`, "idem-one")
	if replay.Code != http.StatusConflict || !strings.Contains(replay.Body.String(), "idempotency_replay") {
		t.Fatalf("replay=%d %s", replay.Code, replay.Body.String())
	}
	if providerCalls.Load() != callsAfterFirst {
		t.Fatal("idempotency replay called provider again")
	}
	conflict := do(`{"model":"fake-model","messages":[{"role":"user","content":"different"}],"max_tokens":10}`, "idem-one")
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "idempotency_conflict") {
		t.Fatalf("conflict=%d %s", conflict.Code, conflict.Body.String())
	}
	tooMany := do(`{"model":"fake-model","messages":[{"role":"user","content":"too many"}],"max_tokens":101}`, "")
	if tooMany.Code != http.StatusBadRequest {
		t.Fatalf("platform max_tokens limit status=%d body=%s", tooMany.Code, tooMany.Body.String())
	}
	priceChange := do(`{"model":"fake-model","messages":[{"role":"user","content":"change price"}],"max_tokens":10}`, "")
	if priceChange.Code != http.StatusOK {
		t.Fatalf("price change call=%d %s", priceChange.Code, priceChange.Body.String())
	}
	priceRequestID := priceChange.Header().Get("X-Request-Id")
	var snapCostIn, snapChargeIn, actualCost, actualCharge int64
	if err := pool.QueryRow(ctx, `SELECT cost_input_micro,charge_input_micro,cost_micro,charge_micro FROM request_record WHERE request_id=$1`, priceRequestID).Scan(&snapCostIn, &snapChargeIn, &actualCost, &actualCharge); err != nil {
		t.Fatal(err)
	}
	if snapCostIn != 100 || snapChargeIn != 1000 || actualCost != 700 || actualCharge != 7000 {
		t.Fatalf("price snapshot changed mid-request: %d %d %d %d", snapCostIn, snapChargeIn, actualCost, actualCharge)
	}
	if _, err := pool.Exec(ctx, `UPDATE model SET input_price_micro=$2,output_price_micro=$3,charge_input_micro=$4,charge_output_micro=$5 WHERE id=$1`, m.ID, costIn, costOut, chargeIn, chargeOut); err != nil {
		t.Fatal(err)
	}

	stream := do(`{"model":"fake-model","stream":true,"messages":[{"role":"user","content":"stream me"}],"max_tokens":10}`, "")
	if stream.Code != http.StatusOK || !strings.Contains(stream.Body.String(), "data: [DONE]") || !strings.Contains(stream.Body.String(), "Displayed Model") {
		t.Fatalf("stream=%d %s", stream.Code, stream.Body.String())
	}
	broken := do(`{"model":"fake-model","stream":true,"messages":[{"role":"user","content":"stream break"}],"max_tokens":10}`, "")
	if broken.Code != http.StatusOK || !strings.Contains(broken.Body.String(), "stream_error") || !strings.Contains(broken.Body.String(), "data: [DONE]") {
		t.Fatalf("broken stream=%d %s", broken.Code, broken.Body.String())
	}
	brokenID := broken.Header().Get("X-Request-Id")
	var brokenStatus string
	var brokenCharge int64
	if err := pool.QueryRow(ctx, `SELECT status,charge_micro FROM request_record WHERE request_id=$1`, brokenID).Scan(&brokenStatus, &brokenCharge); err != nil {
		t.Fatal(err)
	}
	if brokenStatus != "stream_broken" || brokenCharge != 7000 {
		t.Fatalf("partial usage settlement status=%s charge=%d", brokenStatus, brokenCharge)
	}

	gatewayServer := httptest.NewServer(handler)
	defer gatewayServer.Close()
	disconnectCtx, disconnectCancel := context.WithCancel(ctx)
	disconnectReq, err := http.NewRequestWithContext(disconnectCtx, http.MethodPost, gatewayServer.URL+"/v1/chat/completions", strings.NewReader(`{"model":"fake-model","stream":true,"messages":[{"role":"user","content":"client disconnect"}],"max_tokens":10}`))
	if err != nil {
		t.Fatal(err)
	}
	disconnectReq.Header.Set("Authorization", "Bearer "+apiKey)
	disconnectReq.Header.Set("Content-Type", "application/json")
	disconnectResp, err := http.DefaultClient.Do(disconnectReq)
	if err != nil {
		t.Fatal(err)
	}
	disconnectID := disconnectResp.Header.Get("X-Request-Id")
	_, _ = bufio.NewReader(disconnectResp.Body).ReadString('\n')
	disconnectCancel()
	_ = disconnectResp.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for !upstreamCancelled.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !upstreamCancelled.Load() {
		t.Fatal("client disconnect did not cancel upstream context")
	}
	var disconnectStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM request_record WHERE request_id=$1`, disconnectID).Scan(&disconnectStatus); err != nil {
		t.Fatal(err)
	}
	if disconnectStatus != "client_disconnected" {
		t.Fatalf("disconnect status=%s", disconnectStatus)
	}

	noUsage := do(`{"model":"fake-model","messages":[{"role":"user","content":"no usage please"}],"max_tokens":10}`, "")
	if noUsage.Code != http.StatusOK {
		t.Fatalf("no-usage response=%d %s", noUsage.Code, noUsage.Body.String())
	}
	upstreamErr := do(`{"model":"fake-model","messages":[{"role":"user","content":"upstream error"}],"max_tokens":10}`, "")
	if upstreamErr.Code != http.StatusBadGateway {
		t.Fatalf("upstream error=%d %s", upstreamErr.Code, upstreamErr.Body.String())
	}

	acct, err := q.Account(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Seven billable calls at 7,000 micro each: direct calls plus auto-routed calls.
	// Missing usage, disconnect without usage, and provider error release
	// reservations without estimated-token charges.
	if acct.BalanceMicro != 951_000 || acct.ReservedMicro != 0 || acct.AvailableMicro != 951_000 {
		t.Fatalf("quota after calls=%+v", acct)
	}
	var events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM usage_event WHERE user_id=$1`, u.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 7 {
		t.Fatalf("usage event count=%d", events)
	}
	var records int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM request_record WHERE user_id=$1 AND model_id=$2 AND status='success'`, u.ID, m.ID).Scan(&records); err != nil {
		t.Fatal(err)
	}
	if records != 7 {
		t.Fatalf("successful metadata request count=%d", records)
	}
	_ = keyView
}
