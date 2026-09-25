// Package api exposes the admin (/admin/api/v1) and user (/user/api/v1) HTTP
// APIs. Every protected route is authorized on the server; hidden UI controls
// are not a security boundary.
package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/hchw/slogan/internal/apperr"
	"github.com/hchw/slogan/internal/audit"
	"github.com/hchw/slogan/internal/auth"
	"github.com/hchw/slogan/internal/config"
	"github.com/hchw/slogan/internal/db"
	"github.com/hchw/slogan/internal/evaluation"
	"github.com/hchw/slogan/internal/httpx"
	"github.com/hchw/slogan/internal/model"
	"github.com/hchw/slogan/internal/policy"
	"github.com/hchw/slogan/internal/provider"
	"github.com/hchw/slogan/internal/quota"
	"github.com/hchw/slogan/internal/requestrec"
	"github.com/hchw/slogan/internal/usage"
)

// API bundles the admin and user API dependencies.
type API struct {
	pool     *db.Pool
	cfg      *config.Config
	auth     *auth.Service
	provider *provider.Service
	models   *model.Service
	quota    *quota.Service
	reqs     *requestrec.Service
	usg      *usage.Service
	policy   *policy.Service
	eval     *evaluation.Service
	audit    *audit.Service
}

// New builds the API.
func New(pool *db.Pool, cfg *config.Config, authSvc *auth.Service, prov *provider.Service, models *model.Service,
	q *quota.Service, reqs *requestrec.Service, usg *usage.Service, pol *policy.Service,
	ev *evaluation.Service, aud *audit.Service) *API {
	return &API{pool: pool, cfg: cfg, auth: authSvc, provider: prov, models: models, quota: q, reqs: reqs,
		usg: usg, policy: pol, eval: ev, audit: aud}
}

type ctxKey int

const (
	userIDKey ctxKey = iota
	adminKey
	requestIDKey
)

// Handler returns the combined admin+user mux.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	// ---- user API ----
	user := http.NewServeMux()
	user.HandleFunc("POST /auth/register", a.userRegister)
	user.HandleFunc("POST /auth/login", a.userLogin)
	user.HandleFunc("POST /auth/logout", a.userAuth(a.userLogout))
	user.HandleFunc("GET /me", a.userAuth(a.userMe))
	user.HandleFunc("GET /api-keys", a.userAuth(a.userListKeys))
	user.HandleFunc("POST /api-keys", a.userAuth(a.userCreateKey))
	user.HandleFunc("POST /api-keys/{id}/disable", a.userAuth(a.userDisableKey))
	user.HandleFunc("POST /api-keys/{id}/enable", a.userAuth(a.userEnableKey))
	user.HandleFunc("DELETE /api-keys/{id}", a.userAuth(a.userRevokeKey))
	user.HandleFunc("GET /quota", a.userAuth(a.userQuota))
	user.HandleFunc("GET /quota/ledger", a.userAuth(a.userLedger))
	user.HandleFunc("POST /redemption/redeem", a.userAuth(a.userRedeem))
	user.HandleFunc("GET /usage", a.userAuth(a.userUsage))
	user.HandleFunc("GET /requests", a.userAuth(a.userRequests))
	mux.Handle("/user/api/v1/", http.StripPrefix("/user/api/v1", user))

	// ---- admin API ----
	admin := http.NewServeMux()
	admin.HandleFunc("POST /auth/login", a.adminLogin)
	admin.HandleFunc("POST /auth/logout", a.admin("", a.adminLogout))
	admin.HandleFunc("GET /me", a.admin("", a.adminMe))
	admin.HandleFunc("GET /providers", a.admin("provider:read", a.providerList))
	admin.HandleFunc("POST /providers", a.admin("provider:create", a.providerCreate))
	admin.HandleFunc("GET /providers/{id}", a.admin("provider:read", a.providerGet))
	admin.HandleFunc("PUT /providers/{id}", a.admin("provider:update", a.providerUpdate))
	admin.HandleFunc("DELETE /providers/{id}", a.admin("provider:delete", a.providerDelete))
	admin.HandleFunc("POST /providers/{id}/test", a.admin("provider:read", a.providerTest))
	admin.HandleFunc("POST /providers/{id}/discover", a.admin("model:create", a.providerDiscover))
	admin.HandleFunc("POST /providers/{id}/enable", a.admin("provider:update", a.providerEnable))
	admin.HandleFunc("POST /providers/{id}/disable", a.admin("provider:update", a.providerDisable))

	admin.HandleFunc("GET /models", a.admin("model:read", a.modelList))
	admin.HandleFunc("POST /models", a.admin("model:create", a.modelCreate))
	admin.HandleFunc("GET /models/{id}", a.admin("model:read", a.modelGet))
	admin.HandleFunc("PUT /models/{id}", a.admin("model:update", a.modelUpdate))
	admin.HandleFunc("DELETE /models/{id}", a.admin("model:update", a.modelDelete))
	admin.HandleFunc("POST /models/{id}/enable", a.admin("model:update", a.modelEnable))
	admin.HandleFunc("POST /models/{id}/disable", a.admin("model:update", a.modelDisable))

	admin.HandleFunc("POST /evaluation/refresh", a.admin("evaluation:refresh", a.refreshStart))
	admin.HandleFunc("GET /evaluation/refresh", a.admin("evaluation:read", a.refreshList))
	admin.HandleFunc("GET /evaluation/refresh/{id}", a.admin("evaluation:read", a.refreshStatus))
	admin.HandleFunc("POST /evaluation/refresh/{id}/retry", a.admin("evaluation:refresh", a.refreshRetry))
	admin.HandleFunc("GET /nodes", a.admin("evaluation:read", a.nodeList))
	admin.HandleFunc("GET /evaluation/versions", a.admin("evaluation:read", a.versionList))
	admin.HandleFunc("POST /evaluation/versions/{id}/publish", a.admin("evaluation:publish", a.versionPublish))
	admin.HandleFunc("POST /evaluation/versions/{id}/rollback", a.admin("evaluation:publish", a.versionRollback))

	admin.HandleFunc("GET /users", a.admin("user:read", a.userList))
	admin.HandleFunc("GET /users/{id}", a.admin("user:read", a.userDetail))
	admin.HandleFunc("POST /users/{id}/disable", a.admin("user:disable", a.userDisable))
	admin.HandleFunc("POST /users/{id}/enable", a.admin("user:enable", a.userEnable))
	admin.HandleFunc("POST /users/{id}/quota/adjust", a.admin("quota:adjust", a.userAdjust))

	admin.HandleFunc("GET /usage/models", a.admin("usage:read", a.usageModels))
	admin.HandleFunc("GET /usage/users", a.admin("usage:read", a.usageUsers))

	admin.HandleFunc("GET /quota-packages", a.admin("package:read", a.packageList))
	admin.HandleFunc("POST /quota-packages", a.admin("package:create", a.packageCreate))
	admin.HandleFunc("POST /quota-packages/{id}/codes", a.admin("code:generate", a.codeGenerate))
	admin.HandleFunc("GET /redemption-codes", a.admin("code:read", a.codeList))

	admin.HandleFunc("GET /routing-policy", a.admin("policy:read", a.policyGet))
	admin.HandleFunc("PUT /routing-policy", a.admin("policy:update", a.policyUpdate))

	admin.HandleFunc("GET /audit-logs", a.admin("audit:read", a.auditList))
	mux.Handle("/admin/api/v1/", http.StripPrefix("/admin/api/v1", admin))

	return httpx.RequestID(mux)
}

// userAuth requires a valid user session and stores the user ID in context.
func (a *API) userAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := a.auth.ValidateSession(r.Context(), bearerToken(r))
		if err != nil || p.Type != "user" {
			httpx.WriteError(w, r, apperr.Unauthenticated("authentication required"))
			return
		}
		ctx := context.WithValue(r.Context(), userIDKey, p.ID)
		next(w, r.WithContext(ctx))
	}
}

// admin requires a valid admin session with a specific permission.
func (a *API) admin(perm string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := a.auth.ValidateSession(r.Context(), bearerToken(r))
		if err != nil || p.Type != "admin" {
			httpx.WriteError(w, r, apperr.Unauthenticated("authentication required"))
			return
		}
		admin, err := a.auth.AdminByID(r.Context(), p.ID)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		if perm != "" && !admin.Has(perm) {
			httpx.WriteError(w, r, apperr.RoleDenied())
			return
		}
		ctx := context.WithValue(r.Context(), adminKey, admin)
		next(w, r.WithContext(ctx))
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	return ""
}

func userID(r *http.Request) int64 {
	if v, ok := r.Context().Value(userIDKey).(int64); ok {
		return v
	}
	return 0
}

func currentAdmin(r *http.Request) *auth.Admin {
	if v, ok := r.Context().Value(adminKey).(*auth.Admin); ok {
		return v
	}
	return nil
}

func actorFrom(r *http.Request) audit.Actor {
	a := audit.Actor{RequestID: httpx.RequestIDFrom(r.Context()), IP: clientIP(r)}
	if ad := currentAdmin(r); ad != nil {
		a.Type, a.ID = "admin", ad.ID
	} else if uid := userID(r); uid != 0 {
		a.Type, a.ID = "user", uid
	}
	return a
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	return r.RemoteAddr
}
