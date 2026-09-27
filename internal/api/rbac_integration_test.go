package api

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/hchw/slogan/internal/model"
	"github.com/hchw/slogan/internal/policy"
	"github.com/hchw/slogan/internal/provider"
	"github.com/hchw/slogan/internal/quota"
	"github.com/hchw/slogan/internal/requestrec"
	"github.com/hchw/slogan/internal/secure"
	"github.com/hchw/slogan/internal/usage"
)

func TestRBACDenialHasNoSideEffect(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping API RBAC integration test")
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

	var auditorID, auditorRoleID int64
	passwordHash, _ := secure.HashPassword("AuditorSecurePass123!")
	if err := pool.QueryRow(ctx, `INSERT INTO admin_user (username,display_name,password_hash) VALUES ('audit-reader','Audit Reader',$1) RETURNING id`, passwordHash).Scan(&auditorID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM role WHERE code='auditor'`).Scan(&auditorRoleID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO admin_user_role(admin_user_id,role_id) VALUES ($1,$2)`, auditorID, auditorRoleID); err != nil {
		t.Fatal(err)
	}
	auditorToken, _, err := authSvc.CreateAdminSession(ctx, auditorID)
	if err != nil {
		t.Fatal(err)
	}
	user, err := authSvc.RegisterUser(ctx, "reader@example.test", "UserSecurePass123!", "")
	if err != nil {
		t.Fatal(err)
	}
	userToken, _, err := authSvc.CreateUserSession(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{ProviderSecretKey: make([]byte, 32), SessionTTL: 24 * time.Hour}
	prov := provider.New(pool, cfg.ProviderSecretKey, aud)
	models := model.New(pool, prov, aud)
	apiHandler := New(pool, cfg, authSvc, prov, models, quota.New(pool, aud), requestrec.NewService(pool), usage.New(pool), policy.New(pool), nil, aud).Handler()

	request := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/admin/api/v1/providers", strings.NewReader(`{"name":"nope","baseUrl":"http://x","secret":"s"}`))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		apiHandler.ServeHTTP(w, r)
		return w
	}
	if w := request(auditorToken); w.Code != http.StatusForbidden {
		t.Fatalf("auditor status=%d body=%s", w.Code, w.Body.String())
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider WHERE name='nope'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("unauthorized admin write had side effect: %d", count)
	}
	if w := request(userToken); w.Code != http.StatusUnauthorized {
		t.Fatalf("user token status=%d body=%s", w.Code, w.Body.String())
	}
	rootResponse := request(rootToken)
	if rootResponse.Code != http.StatusOK {
		t.Fatalf("root status=%d body=%s", rootResponse.Code, rootResponse.Body.String())
	}
	var created struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rootResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	providerID := created.Data.ID
	if providerID == 0 {
		t.Fatalf("provider create response missing ID: %s", rootResponse.Body.String())
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+rootToken)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		apiHandler.ServeHTTP(w, req)
		return w
	}
	if w := call(http.MethodGet, fmt.Sprintf("/admin/api/v1/providers/%d", providerID), ""); w.Code != http.StatusOK {
		t.Fatalf("provider get status=%d body=%s", w.Code, w.Body.String())
	}
	if w := call(http.MethodPut, fmt.Sprintf("/admin/api/v1/providers/%d", providerID), `{"name":"updated","baseUrl":"http://updated","region":"test"}`); w.Code != http.StatusOK {
		t.Fatalf("provider update status=%d body=%s", w.Code, w.Body.String())
	}
	modelBody := fmt.Sprintf(`{"providerId":%d,"name":"API model","modelKey":"api-model","inputPriceMicro":10,"outputPriceMicro":20,"chargeInputMicro":30,"chargeOutputMicro":40,"supportsStream":true}`, providerID)
	modelResponse := call(http.MethodPost, "/admin/api/v1/models", modelBody)
	if modelResponse.Code != http.StatusOK {
		t.Fatalf("model create status=%d body=%s", modelResponse.Code, modelResponse.Body.String())
	}
	var createdModel struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(modelResponse.Body.Bytes(), &createdModel); err != nil {
		t.Fatal(err)
	}
	modelID := createdModel.Data.ID
	if w := call(http.MethodGet, fmt.Sprintf("/admin/api/v1/models/%d", modelID), ""); w.Code != http.StatusOK {
		t.Fatalf("model get status=%d body=%s", w.Code, w.Body.String())
	}
	if w := call(http.MethodPut, fmt.Sprintf("/admin/api/v1/models/%d", modelID), `{"name":"API model updated","inputPriceMicro":15}`); w.Code != http.StatusOK {
		t.Fatalf("model update status=%d body=%s", w.Code, w.Body.String())
	}
	if w := call(http.MethodDelete, fmt.Sprintf("/admin/api/v1/providers/%d", providerID), ""); w.Code != http.StatusConflict {
		t.Fatalf("provider with model delete status=%d body=%s", w.Code, w.Body.String())
	}
	if w := call(http.MethodDelete, fmt.Sprintf("/admin/api/v1/models/%d", modelID), ""); w.Code != http.StatusOK {
		t.Fatalf("model delete status=%d body=%s", w.Code, w.Body.String())
	}
	if w := call(http.MethodDelete, fmt.Sprintf("/admin/api/v1/providers/%d", providerID), ""); w.Code != http.StatusOK {
		t.Fatalf("provider delete status=%d body=%s", w.Code, w.Body.String())
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider WHERE name='updated' AND deleted_at IS NOT NULL`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("provider soft-delete should preserve one history row, count=%d", count)
	}

	otherUser, err := authSvc.RegisterUser(ctx, "other@example.test", "OtherSecurePass123!", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE quota_account SET balance_micro=999 WHERE user_id=$1`, otherUser.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO request_record(request_id,user_id,requested_model,status,input_tokens,output_tokens,charge_micro)
        VALUES ('reader-request',$1,'auto','success',10,5,100)`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO request_record(request_id,user_id,requested_model,status,input_tokens,output_tokens,charge_micro)
        VALUES ('other-request',$1,'fake-chat','success',99,99,999)`, otherUser.ID); err != nil {
		t.Fatal(err)
	}
	userReq := httptest.NewRequest(http.MethodGet, "/user/api/v1/requests", nil)
	userReq.Header.Set("Authorization", "Bearer "+userToken)
	userW := httptest.NewRecorder()
	apiHandler.ServeHTTP(userW, userReq)
	if userW.Code != http.StatusOK {
		t.Fatalf("user requests status=%d body=%s", userW.Code, userW.Body.String())
	}
	var requestPage struct {
		Data struct {
			List []map[string]any `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(userW.Body.Bytes(), &requestPage); err != nil {
		t.Fatal(err)
	}
	if len(requestPage.Data.List) != 1 || requestPage.Data.List[0]["requestId"] != "reader-request" {
		t.Fatalf("cross-user request data leaked: %+v", requestPage.Data.List)
	}
	statDate := time.Now().UTC().Format("2006-01-02")
	for _, row := range []struct{ uid, in, out, charge int64 }{{user.ID, 10, 5, 100}, {otherUser.ID, 99, 99, 999}} {
		if _, err := pool.Exec(ctx, `INSERT INTO usage_record(user_id,model_id,provider_id,request_count,input_tokens,output_tokens,cost_micro,charge_micro,stat_date,stat_hour)
            VALUES($1,$2,$3,1,$4,$5,0,$6,$7::date,0)`, row.uid, modelID, providerID, row.in, row.out, row.charge, statDate); err != nil {
			t.Fatal(err)
		}
	}
	usageReq := httptest.NewRequest(http.MethodGet, "/user/api/v1/usage", nil)
	usageReq.Header.Set("Authorization", "Bearer "+userToken)
	usageW := httptest.NewRecorder()
	apiHandler.ServeHTTP(usageW, usageReq)
	var usageBody struct {
		Data struct {
			Requests int64 `json:"requests"`
			Input    int64 `json:"inputTokens"`
			Output   int64 `json:"outputTokens"`
			Charge   int64 `json:"chargeMicro"`
		} `json:"data"`
	}
	if err := json.Unmarshal(usageW.Body.Bytes(), &usageBody); err != nil {
		t.Fatal(err)
	}
	if usageBody.Data.Requests != 1 || usageBody.Data.Input != 10 || usageBody.Data.Output != 5 || usageBody.Data.Charge != 100 {
		t.Fatalf("usage isolation totals: %+v", usageBody.Data)
	}
	var packageID, batchID int64
	if err := pool.QueryRow(ctx, `INSERT INTO quota_package(name,face_value_micro) VALUES('api-redeem',500) RETURNING id`).Scan(&packageID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO redemption_code_batch(package_id,quantity) VALUES($1,1) RETURNING id`, packageID).Scan(&batchID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO redemption_code(batch_id,package_id,code_hash,code_prefix,expires_at) VALUES($1,$2,$3,'API-REDEEM',now()+interval '1 day')`, batchID, packageID, secure.HashToken("API-REDEEM-ONCE")); err != nil {
		t.Fatal(err)
	}
	redeemReq := httptest.NewRequest(http.MethodPost, "/user/api/v1/redemption/redeem", strings.NewReader(`{"code":"API-REDEEM-ONCE"}`))
	redeemReq.Header.Set("Authorization", "Bearer "+userToken)
	redeemReq.Header.Set("Content-Type", "application/json")
	redeemW := httptest.NewRecorder()
	apiHandler.ServeHTTP(redeemW, redeemReq)
	if redeemW.Code != http.StatusOK {
		t.Fatalf("redeem status=%d body=%s", redeemW.Code, redeemW.Body.String())
	}
	duplicateReq := httptest.NewRequest(http.MethodPost, "/user/api/v1/redemption/redeem", strings.NewReader(`{"code":"API-REDEEM-ONCE"}`))
	duplicateReq.Header.Set("Authorization", "Bearer "+userToken)
	duplicateReq.Header.Set("Content-Type", "application/json")
	duplicateW := httptest.NewRecorder()
	apiHandler.ServeHTTP(duplicateW, duplicateReq)
	if duplicateW.Code != http.StatusConflict {
		t.Fatalf("duplicate redeem status=%d body=%s", duplicateW.Code, duplicateW.Body.String())
	}
	quotaReq := httptest.NewRequest(http.MethodGet, "/user/api/v1/quota", nil)
	quotaReq.Header.Set("Authorization", "Bearer "+userToken)
	quotaW := httptest.NewRecorder()
	apiHandler.ServeHTTP(quotaW, quotaReq)
	var quotaBody struct {
		Data struct {
			Balance int64 `json:"balanceMicro"`
		} `json:"data"`
	}
	if err := json.Unmarshal(quotaW.Body.Bytes(), &quotaBody); err != nil {
		t.Fatal(err)
	}
	if quotaBody.Data.Balance != 500 {
		t.Fatalf("user quota should show only its own 500 micro grant, got %d", quotaBody.Data.Balance)
	}
}
