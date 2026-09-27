package evaluation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hchw/slogan/internal/audit"
	"github.com/hchw/slogan/internal/auth"
	"github.com/hchw/slogan/internal/config"
	"github.com/hchw/slogan/internal/db"
	"github.com/hchw/slogan/internal/model"
	"github.com/hchw/slogan/internal/policy"
	"github.com/hchw/slogan/internal/provider"
	"github.com/hchw/slogan/internal/redisx"
)

type evaluationFixture struct {
	ctx       context.Context
	pool      *db.Pool
	rdb       *redisx.Client
	service   *Service
	modelsSvc *model.Service
	models    []*model.Model
	actor     audit.Actor
	failKey   *atomic.Value
	failMode  *atomic.Value
}

// fail makes the fake upstream reject or stall for one model key.
func (f *evaluationFixture) fail(key, mode string) {
	f.failKey.Store(key)
	f.failMode.Store(mode)
}

func (f *evaluationFixture) addModel(t *testing.T, key string) *model.Model {
	t.Helper()
	price := int64(5)
	m, err := f.modelsSvc.Create(f.ctx, model.Input{ProviderID: f.models[0].ProviderID, Name: key, ModelKey: key, ContextLength: 4096, InputModalities: []string{"text"}, SupportsStream: true, InputPriceMicro: &price, OutputPriceMicro: &price}, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	f.models = append(f.models, m)
	return m
}

func setupEvaluation(t *testing.T, modelKeys ...string) *evaluationFixture {
	t.Helper()
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

	failKey := &atomic.Value{}
	failMode := &atomic.Value{}
	failKey.Store("")
	failMode.Store("")
	validContent := func() string {
		caps := map[string]float64{}
		for _, d := range Dimensions {
			caps[d] = 0.8
		}
		b, _ := json.Marshal(caps)
		return `{"capabilities":` + string(b) + `,"confidence":0.9}`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mode, _ := failMode.Load().(string)
		key, _ := failKey.Load().(string)
		content := validContent()
		if key != "" && req.Model == key {
			switch mode {
			case "bad":
				content = "not valid json"
			case "slow":
				time.Sleep(3 * time.Second)
			case "status":
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}
		response := map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": content}}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 100}}
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(server.Close)

	key := make([]byte, 32)
	aud := audit.New(pool)
	authSvc := auth.New(pool, 24*time.Hour)
	if err := authSvc.EnsureSeedRoles(ctx); err != nil {
		t.Fatal(err)
	}
	admin, err := authSvc.Bootstrap(ctx, "eval-admin", "EvaluationSecurePass123!", "Evaluation Admin")
	if err != nil {
		t.Fatal(err)
	}
	prov := provider.New(pool, key, aud)
	modelsSvc := model.New(pool, prov, aud)
	pol := policy.New(pool)
	if err := pol.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	providerRecord, err := prov.Create(ctx, provider.Input{Name: "eval-provider", BaseURL: server.URL + "/v1", Secret: "eval-secret"}, audit.Actor{Type: "admin", ID: admin.ID})
	if err != nil {
		t.Fatal(err)
	}
	actor := audit.Actor{Type: "admin", ID: admin.ID, RequestID: "evaluation-test"}
	f := &evaluationFixture{ctx: ctx, pool: pool, rdb: rdb, modelsSvc: modelsSvc, actor: actor, failKey: failKey, failMode: failMode}
	for _, id := range modelKeys {
		price := int64(5)
		m, err := modelsSvc.Create(ctx, model.Input{ProviderID: providerRecord.ID, Name: id, ModelKey: id, ContextLength: 4096, InputModalities: []string{"text"}, SupportsStream: true, InputPriceMicro: &price, OutputPriceMicro: &price}, actor)
		if err != nil {
			t.Fatal(err)
		}
		f.models = append(f.models, m)
	}
	cfg := config.EvaluationConfig{MaxTokens: 2000, Concurrency: 5, Timeout: 2 * time.Second, MinPublishRatio: .8, MaxRefreshesPerDay: 10}
	ev := New(pool, rdb, modelsSvc, prov, pol, aud, cfg)
	ev.SetAckTimeout(150 * time.Millisecond)
	f.service = ev
	return f
}

type confirmRemovalMembership struct{}

func (confirmRemovalMembership) DrainAndConfirm(context.Context, string) (bool, error) {
	return true, nil
}

func runAllItems(t *testing.T, f *evaluationFixture, task *Task) {
	t.Helper()
	for _, m := range f.models {
		if err := f.service.ProcessItem(f.ctx, task.ID, m.ID); err != nil {
			t.Fatalf("process item %d: %v", m.ID, err)
		}
	}
}

func itemByModel(t *testing.T, f *evaluationFixture, taskID, modelID int64) TaskItem {
	t.Helper()
	items, err := f.service.TaskItems(f.ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.ModelID == modelID {
			return it
		}
	}
	t.Fatalf("no item for model %d in task %d", modelID, taskID)
	return TaskItem{}
}

func TestRefreshCandidatePublishAndRollback(t *testing.T) {
	f := setupEvaluation(t, "good-model", "bad-model")
	first, err := f.service.StartRefresh(f.ctx, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.ProcessItem(f.ctx, first.ID, f.models[0].ID); err != nil {
		t.Fatal(err)
	}
	// Duplicate stream delivery must not evaluate or aggregate the same item twice.
	if err := f.service.ProcessItem(f.ctx, first.ID, f.models[0].ID); err != nil {
		t.Fatal(err)
	}
	// A worker that died mid-item leaves a stale running item; recovery re-arms it.
	if _, err := f.pool.Exec(f.ctx, `UPDATE refresh_task_item SET status='running',started_at=now()-interval '3 minutes' WHERE task_id=$1 AND model_id=$2`, first.ID, f.models[1].ID); err != nil {
		t.Fatal(err)
	}
	f.service.recoverPending(f.ctx)
	if err := f.service.ProcessItem(f.ctx, first.ID, f.models[1].ID); err != nil {
		t.Fatal(err)
	}
	status, err := f.service.TaskStatus(f.ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != "completed" || status.Succeeded != 2 || status.Failed != 0 {
		t.Fatalf("task=%+v", status)
	}
	caps, err := f.service.Capabilities(f.ctx, first.ScoreVersionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(caps) != 2 || caps[f.models[0].ID]["coding"] < .79 {
		t.Fatalf("capability aggregate=%v", caps)
	}
	items, err := f.service.TaskItems(f.ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("task items=%+v", items)
	}
	// Each item runs a self assessment plus a peer assessment: 110 tokens each,
	// priced at 5 micro per token on both frozen directions.
	if items[0].OutputTokens != 200 || items[0].CostMicro != 2*110*5 || items[0].ModelKey == "" {
		t.Fatalf("item evidence=%+v", items[0])
	}
	statusCost, err := f.service.TaskStatus(f.ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if statusCost.CostMicro != 2*2*110*5 {
		t.Fatalf("task cost=%d", statusCost.CostMicro)
	}
	tasks, err := f.service.LatestTasks(f.ctx, 5)
	if err != nil || len(tasks) != 1 || tasks[0].ID != first.ID {
		t.Fatalf("latest tasks=%+v err=%v", tasks, err)
	}
	published, err := f.service.Publish(f.ctx, first.ScoreVersionID, "initial publish", f.actor)
	if err != nil || published != "published" {
		t.Fatalf("publish=%s err=%v", published, err)
	}
	// A fully successful task has nothing to re-arm.
	if retried, err := f.service.RetryFailedItems(f.ctx, first.ID); err != nil || retried != 0 {
		t.Fatalf("retry of a clean task: retried=%d err=%v", retried, err)
	}

	second, err := f.service.StartRefresh(f.ctx, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	runAllItems(t, f, second)
	published, err = f.service.Publish(f.ctx, second.ScoreVersionID, "new candidate", f.actor)
	if err != nil || published != "published" {
		t.Fatalf("second publish=%s err=%v", published, err)
	}
	rollback, err := f.service.Rollback(f.ctx, first.ScoreVersionID, "rollback due smoke test", f.actor)
	if err != nil || rollback != "published" {
		t.Fatalf("rollback=%s err=%v", rollback, err)
	}
	active, err := f.service.ActiveVersion(f.ctx)
	if err != nil || active != first.ScoreVersionID {
		t.Fatalf("active version=%d err=%v", active, err)
	}
	var actionCount int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM audit_log WHERE action IN ('evaluation.publish','evaluation.rollback')`).Scan(&actionCount); err != nil {
		t.Fatal(err)
	}
	if actionCount != 3 {
		t.Fatalf("publish/rollback audit count=%d", actionCount)
	}
}

func TestPartialFailureRecordsItemOutcomeAndBlocksPublish(t *testing.T) {
	f := setupEvaluation(t, "good-model", "bad-model")
	f.fail("bad-model", "bad")
	task, err := f.service.StartRefresh(f.ctx, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	runAllItems(t, f, task)
	status, err := f.service.TaskStatus(f.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Succeeded != 1 || status.Failed != 1 {
		t.Fatalf("partial outcomes=%+v", status)
	}
	bad := itemByModel(t, f, task.ID, f.models[1].ID)
	if bad.Status != "timeout" || bad.Error != "invalid_output" {
		t.Fatalf("item outcome evidence=%+v", bad)
	}
	good := itemByModel(t, f, task.ID, f.models[0].ID)
	if good.Status != "succeeded" || good.CostMicro == 0 {
		t.Fatalf("successful item evidence=%+v", good)
	}
	versions, err := f.service.ListVersions(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || versions[0].SuccessRatio < .49 || versions[0].SuccessRatio > .51 {
		t.Fatalf("candidate coverage=%+v", versions)
	}
	if _, err := f.service.Publish(f.ctx, task.ScoreVersionID, "should reject", f.actor); err == nil {
		t.Fatal("below-threshold candidate published")
	}
	active, err := f.service.ActiveVersion(f.ctx)
	if err != nil || active != 0 {
		t.Fatalf("published version changed: %d %v", active, err)
	}
	retried, err := f.service.RetryFailedItems(f.ctx, task.ID)
	if err != nil || retried != 1 {
		t.Fatalf("retry failed items: retried=%d err=%v", retried, err)
	}
	afterRetry, err := f.service.TaskStatus(f.ctx, task.ID)
	if err != nil || afterRetry.Status != "running" || afterRetry.Failed != 0 {
		t.Fatalf("retried task state=%+v err=%v", afterRetry, err)
	}
	if it := itemByModel(t, f, task.ID, f.models[1].ID); it.Status != "pending" {
		t.Fatalf("failed item was not re-armed: %+v", it)
	}
	if err := f.service.ProcessItem(f.ctx, task.ID, f.models[1].ID); err != nil {
		t.Fatal(err)
	}
	final, err := f.service.TaskStatus(f.ctx, task.ID)
	if err != nil || final.Failed != 1 || final.Succeeded != 1 {
		t.Fatalf("final state=%+v err=%v", final, err)
	}
	if _, err := f.service.RetryFailedItems(f.ctx, task.ID+1000); err == nil {
		t.Fatal("retry of unknown task was accepted")
	}
}

func TestTimeoutIsClassifiedAndNeverFabricatesScores(t *testing.T) {
	f := setupEvaluation(t, "good-model", "slow-model")
	f.fail("slow-model", "slow")
	task, err := f.service.StartRefresh(f.ctx, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.ProcessItem(f.ctx, task.ID, f.models[1].ID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.ProcessItem(f.ctx, task.ID, f.models[0].ID); err != nil {
		t.Fatal(err)
	}
	slow := itemByModel(t, f, task.ID, f.models[1].ID)
	if slow.Status != "timeout" || slow.Error != "timeout" || slow.CostMicro != 0 {
		t.Fatalf("timeout evidence=%+v", slow)
	}
	caps, err := f.service.NewScores(f.ctx, task.ScoreVersionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := caps[f.models[1].ID]; ok {
		t.Fatalf("timed-out model must not receive a score: %+v", caps)
	}
	if _, ok := caps[f.models[0].ID]; !ok {
		t.Fatalf("healthy model must still be scored: %+v", caps)
	}
}

func TestHistoricalFallbackKeepsExistingModelsRoutable(t *testing.T) {
	f := setupEvaluation(t, "model-1", "model-2", "model-3", "model-4", "model-5")
	first, err := f.service.StartRefresh(f.ctx, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	runAllItems(t, f, first)
	if _, err := f.service.Publish(f.ctx, first.ScoreVersionID, "baseline", f.actor); err != nil {
		t.Fatal(err)
	}
	baseline, err := f.service.NewScores(f.ctx, first.ScoreVersionID)
	if err != nil {
		t.Fatal(err)
	}

	f.fail("model-1", "bad")
	second, err := f.service.StartRefresh(f.ctx, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	runAllItems(t, f, second)
	status, err := f.service.TaskStatus(f.ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Succeeded != 4 || status.Failed != 1 {
		t.Fatalf("refresh coverage=%+v", status)
	}
	if published, err := f.service.Publish(f.ctx, second.ScoreVersionID, "coverage 80%", f.actor); err != nil || published != "published" {
		t.Fatalf("publish=%s err=%v", published, err)
	}

	// New scores exclude the failed participant; the effective matrix falls back.
	fresh, err := f.service.NewScores(f.ctx, second.ScoreVersionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fresh[f.models[0].ID]; ok {
		t.Fatalf("failed model received a new score: %+v", fresh)
	}
	effective, err := f.service.Capabilities(f.ctx, second.ScoreVersionID)
	if err != nil {
		t.Fatal(err)
	}
	served, ok := effective[f.models[0].ID]
	if !ok || served["coding"] != baseline[f.models[0].ID]["coding"] {
		t.Fatalf("historical fallback missing: %+v", effective[f.models[0].ID])
	}
	fallback, err := f.service.FallbackModels(f.ctx, second.ScoreVersionID)
	if err != nil || len(fallback) != 1 || fallback[0] != f.models[0].ID {
		t.Fatalf("fallback list=%v err=%v", fallback, err)
	}
	versions, err := f.service.ListVersions(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0].ValidModels != 4 || versions[0].TotalModels != 5 || versions[0].FallbackModels != 1 {
		t.Fatalf("version evidence=%+v", versions[0])
	}

	// A model with no historical score is never armed with a fabricated ability.
	late := f.addModel(t, "model-late")
	if _, ok := effective[late.ID]; ok {
		t.Fatalf("never-evaluated model appeared in the effective matrix")
	}
}

func TestAckBarrierRequiresAllLiveNodesOrConfirmedRemoval(t *testing.T) {
	f := setupEvaluation(t, "good-model", "bad-model")
	if err := f.rdb.Heartbeat(f.ctx, "gw-ack", 0, true, false); err != nil {
		t.Fatal(err)
	}
	version := int64(99)
	go func() {
		time.Sleep(40 * time.Millisecond)
		_ = f.rdb.Heartbeat(context.Background(), "gw-ack", version, false, true)
	}()
	ok, err := f.service.awaitActorAck(f.ctx, version)
	if err != nil || !ok {
		t.Fatalf("ack barrier ok=%v err=%v", ok, err)
	}

	if err := f.rdb.Heartbeat(f.ctx, "gw-unacked", 0, true, false); err != nil {
		t.Fatal(err)
	}
	f.service.SetMembershipAdapter(noMembershipAdapter{})
	ok, err = f.service.awaitActorAck(f.ctx, 100)
	if err != nil || ok {
		t.Fatalf("unconfirmed removal must remain pending: ok=%v err=%v", ok, err)
	}
	nodes, err := f.rdb.Nodes(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !nodes["gw-unacked"].Drain {
		t.Fatalf("unacknowledged node was not marked draining: %+v", nodes["gw-unacked"])
	}
	f.service.SetMembershipAdapter(confirmRemovalMembership{})
	ok, err = f.service.awaitActorAck(f.ctx, 101)
	if err != nil || !ok {
		t.Fatalf("confirmed removal should unblock barrier: ok=%v err=%v", ok, err)
	}
	remaining, err := f.rdb.Nodes(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("removed nodes still serve traffic: %+v", remaining)
	}
}
