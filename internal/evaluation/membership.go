package evaluation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MembershipAdapter removes a draining Gateway from traffic and confirms the
// removal. A nil/no-op implementation never confirms removal, so publication
// remains pending instead of silently excluding an unacknowledged node.
type MembershipAdapter interface {
	DrainAndConfirm(ctx context.Context, nodeID string) (bool, error)
}

type noMembershipAdapter struct{}

func (noMembershipAdapter) DrainAndConfirm(context.Context, string) (bool, error) { return false, nil }

// HTTPMembershipAdapter integrates with the deployment's load-balancer control
// plane. The control endpoint must return {"removed":true} only after its
// traffic membership no longer contains the node.
type HTTPMembershipAdapter struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewHTTPMembershipAdapter(baseURL, token string, timeout time.Duration) *HTTPMembershipAdapter {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &HTTPMembershipAdapter{baseURL: strings.TrimRight(baseURL, "/"), token: token, client: &http.Client{Timeout: timeout}}
}

func (a *HTTPMembershipAdapter) DrainAndConfirm(ctx context.Context, nodeID string) (bool, error) {
	if a == nil || a.baseURL == "" {
		return false, nil
	}
	body, _ := json.Marshal(map[string]string{"nodeId": nodeID})
	endpoint := a.baseURL + "/v1/nodes/" + url.PathEscape(nodeID) + "/drain"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return false, fmt.Errorf("membership drain returned status %d", resp.StatusCode)
	}
	var result struct {
		Removed bool `json:"removed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, err
	}
	return result.Removed, nil
}
