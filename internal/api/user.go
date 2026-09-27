package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/hchw/slogan/internal/apperr"
	"github.com/hchw/slogan/internal/httpx"
)

func parseID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, apperr.InvalidParam("invalid id")
	}
	return id, nil
}

func parseRange(r *http.Request) (time.Time, time.Time, error) {
	now := time.Now().UTC()
	from := now.AddDate(0, 0, -29).Truncate(24 * time.Hour)
	to := now
	if v := r.URL.Query().Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			if t2, err2 := time.Parse("2006-01-02", v); err2 == nil {
				t = t2
			} else {
				return from, to, apperr.InvalidParam("invalid from")
			}
		}
		from = t
	}
	if v := r.URL.Query().Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			if t2, err2 := time.Parse("2006-01-02", v); err2 == nil {
				t = t2
			} else {
				return from, to, apperr.InvalidParam("invalid to")
			}
		}
		to = t
	}
	return from, to, nil
}

func (a *API) userRegister(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Nickname string `json:"nickname"`
	}
	if err := httpx.DecodeJSON(r, &in, 1<<16); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	u, err := a.auth.RegisterUser(r.Context(), in.Email, in.Password, in.Nickname)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	token, expires, err := a.auth.CreateUserSession(r.Context(), u.ID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{
		"token": token, "expiresIn": int(time.Until(expires).Seconds()),
		"user": map[string]any{"id": u.ID, "email": u.Email, "nickname": u.Nickname},
	})
}

func (a *API) userLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := httpx.DecodeJSON(r, &in, 1<<16); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	u, err := a.auth.AuthenticateUser(r.Context(), in.Email, in.Password)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	token, expires, err := a.auth.CreateUserSession(r.Context(), u.ID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{
		"token": token, "expiresIn": int(time.Until(expires).Seconds()),
		"user": map[string]any{"id": u.ID, "email": u.Email, "nickname": u.Nickname},
	})
}

func (a *API) userLogout(w http.ResponseWriter, r *http.Request) {
	if err := a.auth.RevokeSession(r.Context(), bearerToken(r)); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{})
}

func (a *API) userMe(w http.ResponseWriter, r *http.Request) {
	u, err := a.auth.UserByID(r.Context(), userID(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{
		"id": u.ID, "email": u.Email, "nickname": u.Nickname, "status": u.Status,
	})
}

func (a *API) userCreateKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name            string   `json:"name"`
		ExpiresAt       string   `json:"expiresAt"`
		RateLimitPerMin int      `json:"rateLimitPerMin"`
		AllowedModels   []string `json:"allowedModels"`
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
	view, secret, err := a.auth.CreateAPIKey(r.Context(), userID(r), in.Name, expires, in.RateLimitPerMin, in.AllowedModels)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{
		"id": view.ID, "name": view.Name, "key": secret, "keyPrefix": view.KeyPrefix,
		"status": view.Status, "createdAt": view.CreatedAt,
	})
}

func (a *API) userListKeys(w http.ResponseWriter, r *http.Request) {
	page, pageSize := httpx.ParsePagination(r)
	keys, total, err := a.auth.ListAPIKeys(r.Context(), userID(r), pageSize, (page-1)*pageSize)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, httpx.Page{List: keys, Page: page, PageSize: pageSize, Total: total})
}

func (a *API) userDisableKey(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := a.auth.SetAPIKeyStatus(r.Context(), userID(r), id, "disabled"); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{})
}

func (a *API) userEnableKey(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := a.auth.SetAPIKeyStatus(r.Context(), userID(r), id, "active"); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{})
}

func (a *API) userRevokeKey(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := a.auth.RevokeAPIKey(r.Context(), userID(r), id); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{})
}

func (a *API) userQuota(w http.ResponseWriter, r *http.Request) {
	acct, err := a.quota.Account(r.Context(), userID(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{
		"balanceMicro": acct.BalanceMicro, "reservedMicro": acct.ReservedMicro, "availableMicro": acct.AvailableMicro,
	})
}

func (a *API) userLedger(w http.ResponseWriter, r *http.Request) {
	page, pageSize := httpx.ParsePagination(r)
	from, to, err := parseRange(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	entries, total, err := a.quota.Ledger(r.Context(), userID(r), &from, &to, pageSize, (page-1)*pageSize)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, httpx.Page{List: entries, Page: page, PageSize: pageSize, Total: total})
}

func (a *API) userRedeem(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
	}
	if err := httpx.DecodeJSON(r, &in, 1<<16); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if in.Code == "" {
		httpx.WriteError(w, r, apperr.MissingParam("code is required"))
		return
	}
	granted, balance, err := a.quota.Redeem(r.Context(), userID(r), in.Code)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, map[string]any{"grantedMicro": granted, "balanceMicro": balance})
}

func (a *API) userUsage(w http.ResponseWriter, r *http.Request) {
	from, to, err := parseRange(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	sum, err := a.usg.UserSummary(r.Context(), userID(r), from, to)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteData(w, r, http.StatusOK, sum)
}

func (a *API) userRequests(w http.ResponseWriter, r *http.Request) {
	page, pageSize := httpx.ParsePagination(r)
	var modelID int64
	if v := r.URL.Query().Get("modelId"); v != "" {
		modelID, _ = strconv.ParseInt(v, 10, 64)
	}
	recs, total, err := a.reqs.ListByUser(r.Context(), userID(r), modelID, pageSize, (page-1)*pageSize)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	list := make([]map[string]any, 0, len(recs))
	for _, rec := range recs {
		actualModel := ""
		if rec.ModelID != 0 {
			if m, err := a.models.Get(r.Context(), rec.ModelID); err == nil {
				actualModel = m.Name
			}
		}
		list = append(list, map[string]any{
			"requestId": rec.RequestID, "requestedModel": rec.RequestedModel, "model": actualModel,
			"status": rec.Status, "tokens": rec.InputTokens + rec.OutputTokens,
			"inputTokens": rec.InputTokens, "outputTokens": rec.OutputTokens,
			"chargeMicro": rec.ChargeMicro, "latencyMs": rec.LatencyMs, "createdAt": rec.CreatedAt,
		})
	}
	httpx.WriteData(w, r, http.StatusOK, httpx.Page{List: list, Page: page, PageSize: pageSize, Total: total})
}
