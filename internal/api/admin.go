package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hchw/slogan/internal/apperr"
	"github.com/hchw/slogan/internal/audit"
	"github.com/hchw/slogan/internal/httpx"
	"github.com/hchw/slogan/internal/model"
	"github.com/hchw/slogan/internal/policy"
	"github.com/hchw/slogan/internal/provider"
)

// ---- administrator sessions ----

func (a *API) adminLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := httpx.DecodeJSON(r, &in, 1<<16); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	admin, err := a.auth.AuthenticateAdmin(r.Context(), in.Username, in.Password)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	token, expires, err := a.auth.CreateAdminSession(r.Context(), admin.ID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{
		"token": token, "expiresIn": int(time.Until(expires).Seconds()),
		"admin": map[string]any{"id": admin.ID, "username": admin.Username, "displayName": admin.DisplayName},
	})
}

func (a *API) adminLogout(w http.ResponseWriter, r *http.Request) {
	if err := a.auth.RevokeSession(r.Context(), bearerToken(r)); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{})
}

func (a *API) adminMe(w http.ResponseWriter, r *http.Request) {
	admin := currentAdmin(r)
	if admin == nil {
		httpx.WriteError(w, r, apperr.Unauthenticated("authentication required"))
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{
		"id": admin.ID, "username": admin.Username, "displayName": admin.DisplayName,
		"permissions": admin.Permissions,
	})
}

// ---- providers ----

func (a *API) providerCreate(w http.ResponseWriter, r *http.Request) {
	var in provider.Input
	if err := httpx.DecodeJSON(r, &in, 1<<16); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, err := a.provider.Create(r.Context(), in, actorFrom(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, p)
}

func (a *API) providerList(w http.ResponseWriter, r *http.Request) {
	page, pageSize := httpx.ParsePagination(r)
	list, total, err := a.provider.List(r.Context(), r.URL.Query().Get("status"), r.URL.Query().Get("keyword"), pageSize, (page-1)*pageSize)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, httpx.Page{List: list, Page: page, PageSize: pageSize, Total: total})
}

func (a *API) providerGet(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, err := a.provider.Get(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, p)
}

func (a *API) providerUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var in provider.Input
	if err := httpx.DecodeJSON(r, &in, 1<<16); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, err := a.provider.Update(r.Context(), id, in, actorFrom(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, p)
}

func (a *API) providerDelete(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := a.provider.Delete(r.Context(), id, actorFrom(r)); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{})
}

func (a *API) providerTest(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, err := a.provider.Get(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	secret, err := a.provider.Secret(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	client := provider.NewClient(p.BaseURL, secret, p.AuthType, 20*time.Second)
	res, err := client.Test(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, res)
}

func (a *API) providerEnable(w http.ResponseWriter, r *http.Request) {
	a.setProviderStatus(w, r, "enabled")
}

func (a *API) providerDisable(w http.ResponseWriter, r *http.Request) {
	a.setProviderStatus(w, r, "disabled")
}

func (a *API) setProviderStatus(w http.ResponseWriter, r *http.Request, status string) {
	id, err := parseID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := a.provider.SetStatus(r.Context(), id, status, actorFrom(r)); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{})
}

func (a *API) providerDiscover(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	res, err := a.models.Discover(r.Context(), id, actorFrom(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, res)
}

// ---- models ----

func (a *API) modelCreate(w http.ResponseWriter, r *http.Request) {
	var in model.Input
	if err := httpx.DecodeJSON(r, &in, 1<<16); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	m, err := a.models.Create(r.Context(), in, actorFrom(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, m)
}

func (a *API) modelList(w http.ResponseWriter, r *http.Request) {
	page, pageSize := httpx.ParsePagination(r)
	var providerID int64
	if v := r.URL.Query().Get("providerId"); v != "" {
		providerID, _ = strconv.ParseInt(v, 10, 64)
	}
	list, total, err := a.models.List(r.Context(), providerID, r.URL.Query().Get("status"), r.URL.Query().Get("keyword"), pageSize, (page-1)*pageSize)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, httpx.Page{List: list, Page: page, PageSize: pageSize, Total: total})
}

func (a *API) modelGet(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	m, err := a.models.Get(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, m)
}

func (a *API) modelUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var in model.Input
	if err := httpx.DecodeJSON(r, &in, 1<<16); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	m, err := a.models.Update(r.Context(), id, in, actorFrom(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, m)
}

func (a *API) modelDelete(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := a.models.Delete(r.Context(), id, actorFrom(r)); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{})
}

func (a *API) modelEnable(w http.ResponseWriter, r *http.Request) {
	a.setModelStatus(w, r, model.StatusAvailable)
}
func (a *API) modelDisable(w http.ResponseWriter, r *http.Request) {
	a.setModelStatus(w, r, model.StatusDisabled)
}

func (a *API) setModelStatus(w http.ResponseWriter, r *http.Request, status string) {
	id, err := parseID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := a.models.SetStatus(r.Context(), id, status, actorFrom(r)); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{})
}

// ---- nodes ----

func (a *API) nodeList(w http.ResponseWriter, r *http.Request) {
	if a.eval == nil {
		httpx.WriteError(w, r, apperr.Internal("evaluation service is not configured"))
		return
	}
	nodes, err := a.eval.Nodes(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	active, err := a.eval.ActiveVersion(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{"list": nodes, "activeScoreVersionId": active})
}

// ---- evaluation ----

func (a *API) refreshStart(w http.ResponseWriter, r *http.Request) {
	task, err := a.eval.StartRefresh(r.Context(), actorFrom(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, task)
}

func (a *API) refreshStatus(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	task, err := a.eval.TaskStatus(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	items, err := a.eval.TaskItems(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{"task": task, "items": items})
}

func (a *API) refreshList(w http.ResponseWriter, r *http.Request) {
	list, err := a.eval.LatestTasks(r.Context(), 20)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{"list": list})
}

func (a *API) refreshRetry(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	retried, err := a.eval.RetryFailedItems(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{"retried": retried})
}

func (a *API) versionList(w http.ResponseWriter, r *http.Request) {
	list, err := a.eval.ListVersions(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{"list": list})
}

func (a *API) versionRollback(w http.ResponseWriter, r *http.Request) {
	a.publish(w, r, true)
}

func (a *API) versionPublish(w http.ResponseWriter, r *http.Request) {
	a.publish(w, r, false)
}

func (a *API) publish(w http.ResponseWriter, r *http.Request, rollback bool) {
	id, err := parseID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	_ = httpx.DecodeJSON(r, &in, 1<<16)
	var status string
	if rollback {
		status, err = a.eval.Rollback(r.Context(), id, in.Reason, actorFrom(r))
	} else {
		status, err = a.eval.Publish(r.Context(), id, in.Reason, actorFrom(r))
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{"status": status})
}

// ---- users ----

func (a *API) userList(w http.ResponseWriter, r *http.Request) {
	page, pageSize := httpx.ParsePagination(r)
	list, total, err := a.auth.ListUsers(r.Context(), r.URL.Query().Get("keyword"), r.URL.Query().Get("status"), pageSize, (page-1)*pageSize)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, httpx.Page{List: list, Page: page, PageSize: pageSize, Total: total})
}

func (a *API) userDetail(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	u, err := a.auth.UserByID(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	acct, err := a.quota.Account(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{
		"id": u.ID, "email": u.Email, "nickname": u.Nickname, "status": u.Status,
		"balanceMicro": acct.BalanceMicro, "reservedMicro": acct.ReservedMicro, "availableMicro": acct.AvailableMicro,
	})
}

func (a *API) userDisable(w http.ResponseWriter, r *http.Request) {
	a.setUserStatus(w, r, "disabled")
}
func (a *API) userEnable(w http.ResponseWriter, r *http.Request) {
	a.setUserStatus(w, r, "active")
}

func (a *API) setUserStatus(w http.ResponseWriter, r *http.Request, status string) {
	id, err := parseID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := a.auth.SetUserStatus(r.Context(), id, status, actorFrom(r)); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{})
}

func (a *API) userAdjust(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var in struct {
		AmountMicro    int64  `json:"amountMicro"`
		Remark         string `json:"remark"`
		IdempotencyKey string `json:"idempotencyKey"`
	}
	if err := httpx.DecodeJSON(r, &in, 1<<16); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	balance, err := a.quota.Adjust(r.Context(), actorFrom(r), id, in.AmountMicro, in.Remark, in.IdempotencyKey)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{"balanceMicro": balance})
}

// ---- usage ----

func (a *API) usageModels(w http.ResponseWriter, r *http.Request) {
	from, to, err := parseRange(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var providerID, modelID int64
	if v := r.URL.Query().Get("providerId"); v != "" {
		providerID, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := r.URL.Query().Get("modelId"); v != "" {
		modelID, _ = strconv.ParseInt(v, 10, 64)
	}
	list, err := a.usg.ModelUsage(r.Context(), from, to, providerID, modelID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{"list": list})
}

func (a *API) usageUsers(w http.ResponseWriter, r *http.Request) {
	from, to, err := parseRange(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var uid int64
	if v := r.URL.Query().Get("userId"); v != "" {
		uid, _ = strconv.ParseInt(v, 10, 64)
	}
	list, err := a.usg.UserUsage(r.Context(), from, to, uid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{"list": list})
}

// ---- packages and codes ----

func (a *API) packageCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name           string `json:"name"`
		FaceValueMicro int64  `json:"faceValueMicro"`
	}
	if err := httpx.DecodeJSON(r, &in, 1<<16); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, err := a.quota.CreatePackage(r.Context(), actorFrom(r), in.Name, in.FaceValueMicro)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, p)
}

func (a *API) packageList(w http.ResponseWriter, r *http.Request) {
	page, pageSize := httpx.ParsePagination(r)
	list, total, err := a.quota.ListPackages(r.Context(), pageSize, (page-1)*pageSize)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, httpx.Page{List: list, Page: page, PageSize: pageSize, Total: total})
}

func (a *API) codeGenerate(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var in struct {
		Quantity  int    `json:"quantity"`
		ExpiresAt string `json:"expiresAt"`
		Remark    string `json:"remark"`
	}
	if err := httpx.DecodeJSON(r, &in, 1<<16); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var expires *time.Time
	if in.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, in.ExpiresAt)
		if err != nil {
			httpx.WriteError(w, r, apperr.InvalidParam("expiresAt must be RFC3339"))
			return
		}
		expires = &t
	}
	batch, err := a.quota.GenerateCodes(r.Context(), actorFrom(r), id, in.Quantity, expires)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, batch)
}

func (a *API) codeList(w http.ResponseWriter, r *http.Request) {
	page, pageSize := httpx.ParsePagination(r)
	var batchID int64
	if v := r.URL.Query().Get("batchId"); v != "" {
		batchID, _ = strconv.ParseInt(v, 10, 64)
	}
	list, total, err := a.quota.ListCodes(r.Context(), batchID, r.URL.Query().Get("status"), pageSize, (page-1)*pageSize)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, httpx.Page{List: list, Page: page, PageSize: pageSize, Total: total})
}

// ---- routing policy ----

func (a *API) policyGet(w http.ResponseWriter, r *http.Request) {
	p, err := a.policy.Get(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, p)
}

func (a *API) policyUpdate(w http.ResponseWriter, r *http.Request) {
	var in policy.Policy
	if err := httpx.DecodeJSON(r, &in, 1<<16); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if in.LowCapabilityBias < 0 || in.LowCapabilityBias > 100 {
		httpx.WriteError(w, r, apperr.InvalidParam("lowCapabilityBias must be between 0 and 100"))
		return
	}
	if in.MinPublishRatio <= 0 || in.MinPublishRatio > 1 {
		httpx.WriteError(w, r, apperr.InvalidParam("minPublishRatio must be in (0,1]"))
		return
	}
	if in.Version == "" {
		in.Version = "v" + time.Now().UTC().Format("20060102150405")
	}
	actor := actorFrom(r)
	err := a.pool.WithTx(r.Context(), func(tx pgx.Tx) error {
		if err := a.policy.Update(r.Context(), tx, &in); err != nil {
			return err
		}
		return a.audit.WriteInTx(r.Context(), tx, audit.Entry{
			ActorType: actor.Type, ActorID: actor.ID, Action: "policy.update",
			TargetType: "routing_policy", RequestID: actor.RequestID, IP: actor.IP,
			Detail: map[string]any{"lowCapabilityBias": in.LowCapabilityBias, "minPublishRatio": in.MinPublishRatio},
		})
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, in)
}

// ---- audit ----

type auditRow struct {
	ID         int64     `json:"id"`
	ActorType  string    `json:"actorType"`
	ActorID    int64     `json:"actorId"`
	Action     string    `json:"action"`
	TargetType string    `json:"targetType"`
	TargetID   string    `json:"targetId"`
	Reason     string    `json:"reason"`
	Result     string    `json:"result"`
	RequestID  string    `json:"requestId"`
	CreatedAt  time.Time `json:"createdAt"`
}

func (a *API) auditList(w http.ResponseWriter, r *http.Request) {
	page, pageSize := httpx.ParsePagination(r)
	rows, err := a.pool.Query(r.Context(), `SELECT id, actor_type, COALESCE(actor_id,0), action,
        COALESCE(target_type,''), COALESCE(target_id,''), reason, result, COALESCE(request_id,''), created_at
        FROM audit_log WHERE ($1='' OR action=$1)
        ORDER BY id DESC LIMIT $2 OFFSET $3`,
		r.URL.Query().Get("action"), pageSize, (page-1)*pageSize)
	if err != nil {
		httpx.WriteError(w, r, apperr.Internal("failed to list audit logs"))
		return
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var e auditRow
		if err := rows.Scan(&e.ID, &e.ActorType, &e.ActorID, &e.Action, &e.TargetType, &e.TargetID, &e.Reason, &e.Result, &e.RequestID, &e.CreatedAt); err != nil {
			httpx.WriteError(w, r, apperr.Internal("failed to scan audit log"))
			return
		}
		e.CreatedAt = e.CreatedAt.UTC()
		out = append(out, e)
	}
	var total int64
	_ = a.pool.QueryRow(r.Context(), `SELECT count(*) FROM audit_log WHERE ($1='' OR action=$1)`, r.URL.Query().Get("action")).Scan(&total)
	httpx.WriteData(w, r, http.StatusOK, httpx.Page{List: out, Page: page, PageSize: pageSize, Total: total})
}
