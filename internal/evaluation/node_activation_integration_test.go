package evaluation

import (
	"context"
	"testing"
	"time"

	"github.com/hchw/slogan/internal/auth"
	"github.com/hchw/slogan/internal/config"
	"github.com/hchw/slogan/internal/gateway"
	"github.com/hchw/slogan/internal/laya"
	"github.com/hchw/slogan/internal/quota"
	"github.com/hchw/slogan/internal/requestrec"
	"github.com/hchw/slogan/internal/routing"
	"github.com/hchw/slogan/internal/usage"
)

func TestNewGatewayNodeLoadsPublishedSnapshotBeforeReadiness(t *testing.T) {
	f := setupEvaluation(t, "model-1")
	task, err := f.service.StartRefresh(f.ctx, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	runAllItems(t, f, task)
	status, err := f.service.Publish(f.ctx, task.ScoreVersionID, "initial", f.actor)
	if err != nil || status != "published" {
		t.Fatalf("publish=%s err=%v", status, err)
	}

	cfg := &config.Config{ProviderSecretKey: make([]byte, 32), SessionTTL: 24 * time.Hour, RequestMaxOutputTokens: 100}
	authSvc := auth.New(f.pool, 24*time.Hour)
	q := quota.New(f.pool, f.service.audit)
	reqs := requestrec.NewService(f.pool)
	usg := usage.New(f.pool)
	router := routing.NewRouter(f.service.models, f.service.policy, f.service, f.pool)
	gw := gateway.New(f.pool, cfg, authSvc, f.service.models, f.service.provider, q, reqs, usg, router, laya.New(laya.Config{Enabled: false}), f.rdb, f.service.policy)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go gw.RunNode(ctx, "gw-new")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		nodes, err := f.rdb.Nodes(context.Background())
		if err == nil {
			if node, ok := nodes["gw-new"]; ok && node.Ready && !node.Drain && node.Version == task.ScoreVersionID {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	nodes, _ := f.rdb.Nodes(context.Background())
	t.Fatalf("new node did not load active score before readiness: %+v", nodes)
}
