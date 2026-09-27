package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hchw/slogan/internal/apperr"
)

// Client is an OpenAI-compatible upstream HTTP client.
type Client struct {
	baseURL  string
	apiKey   string
	authType string
	hc       *http.Client
}

// NewClient builds a provider client. The secret is only held in memory and is
// never logged.
func NewClient(baseURL, apiKey, authType string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		apiKey:   apiKey,
		authType: authType,
		hc:       &http.Client{Timeout: timeout},
	}
}

// RemoteModel is a model reported by the provider.
type RemoteModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

type modelList struct {
	Data []RemoteModel `json:"data"`
}

// ListModels calls GET {base}/models.
func (c *Client) ListModels(ctx context.Context) ([]RemoteModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	c.applyAuth(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("provider returned status %d", resp.StatusCode)
	}
	var ml modelList
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ml); err != nil {
		return nil, fmt.Errorf("invalid model list response: %w", err)
	}
	return ml.Data, nil
}

// ConnectionTest verifies credentials and address.
type ConnectionTest struct {
	Reachable      bool `json:"reachable"`
	LatencyMs      int  `json:"latencyMs"`
	ModelsDetected int  `json:"modelsDetected"`
}

// Test performs a bounded connectivity check.
func (c *Client) Test(ctx context.Context) (*ConnectionTest, error) {
	start := time.Now()
	models, err := c.ListModels(ctx)
	if err != nil {
		return nil, apperr.New(502, "50201", "provider_error", "", "provider connectivity test failed")
	}
	return &ConnectionTest{Reachable: true, LatencyMs: int(time.Since(start).Milliseconds()), ModelsDetected: len(models)}, nil
}

// Chat sends a chat completion request and returns the raw response for
// streaming or buffered relay. authHeader=false omits the Authorization header
// (used when replaying a client-supplied header set).
func (c *Client) Chat(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.applyAuth(req)
	return c.hc.Do(req)
}

func (c *Client) applyAuth(req *http.Request) {
	switch c.authType {
	case "", "bearer":
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
	default:
		if c.apiKey != "" {
			req.Header.Set("Authorization", c.apiKey)
		}
	}
}
