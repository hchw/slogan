// Package laya is the Go adapter for the local Python Laya typed-decision
// classification sidecar. It only classifies intent; it never selects a
// provider model. Failures degrade to general-intent routing.
package laya

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hchw/slogan/internal/config"
	"github.com/hchw/slogan/internal/metrics"
)

// Intent labels understood by the router.
var AllowedIntents = map[string]bool{
	"general": true, "coding": true, "reasoning": true, "writing": true,
	"translation": true, "summarization": true, "casual_chat": true, "emotional_support": true,
}

// Fallback reasons.
const (
	ReasonDisabled      = "disabled"
	ReasonUnavailable   = "unavailable"
	ReasonTimeout       = "timeout"
	ReasonBadResponse   = "bad_response"
	ReasonUnknownLabel  = "unknown_label"
	ReasonLowConfidence = "low_confidence"
	ReasonCircuitOpen   = "circuit_open"
)

// Result is the normalized classification outcome.
type Result struct {
	Intent            string
	Confidence        float64
	ClassifierVersion string
	Truncated         bool
	FallbackReason    string
	DurationMs        int
}

// Fallback reports whether the result is a general-intent fallback.
func (r Result) Fallback() bool { return r.FallbackReason != "" }

// Config configures the client.
type Config struct {
	BaseURL       string
	APIKey        string
	Timeout       time.Duration
	MinConfidence float64
	Enabled       bool
	MaxInputRunes int
}

// Client classifies intent via the local sidecar.
type Client struct {
	cfg Config
	hc  *http.Client

	mu        sync.Mutex
	failures  int
	openUntil time.Time

	counters *metrics.Counters
}

// SetMetrics installs the counter set used for classify outcome and latency
// metrics. Latency is exposed as a sum/count pair so p95 can be derived by the
// scrape backend without a high-cardinality histogram here.
func (c *Client) SetMetrics(counters *metrics.Counters) {
	c.counters = counters
	if counters == nil {
		return
	}
	counters.Help("slogan_laya_classify_total", "Laya classifier calls by normalized outcome.")
	counters.Help("slogan_laya_classify_duration_ms_sum", "Total Laya classifier latency in milliseconds.")
	counters.Help("slogan_laya_classify_duration_ms_count", "Number of Laya classifier calls with a recorded latency.")
	counters.Help("slogan_laya_fallback_total", "Laya classifier fallbacks by reason.")
}

// observe records one classification outcome.
func (c *Client) observe(result Result) {
	if c.counters == nil {
		return
	}
	outcome := "classified"
	if result.Fallback() {
		outcome = "fallback"
		c.counters.Inc("slogan_laya_fallback_total", map[string]string{"reason": result.FallbackReason})
	}
	c.counters.Inc("slogan_laya_classify_total", map[string]string{"outcome": outcome})
	c.counters.Add("slogan_laya_classify_duration_ms_sum", nil, float64(result.DurationMs))
	c.counters.Inc("slogan_laya_classify_duration_ms_count", nil)
}

// New builds a client from configuration.
func New(cfg Config) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 500 * time.Millisecond
	}
	if cfg.MinConfidence <= 0 {
		cfg.MinConfidence = 0.60
	}
	return &Client{cfg: cfg, hc: &http.Client{Timeout: cfg.Timeout}}
}

type classifyRequest struct {
	Input    string `json:"input"`
	Question string `json:"question"`
}

type classifyResponse struct {
	Choice     string   `json:"choice"`
	Label      string   `json:"label"`
	Intent     string   `json:"intent"`
	Score      *float64 `json:"score"`
	Confidence *float64 `json:"confidence"`
	Version    string   `json:"version"`
}

// Classify minimizes and classifies the given text. It never returns an error;
// on any failure it returns a general-intent fallback result.
func (c *Client) Classify(ctx context.Context, text string) Result {
	start := time.Now()
	var observed *Result
	defer func() {
		if observed != nil {
			c.observe(*observed)
		}
	}()
	record := func(r Result) Result { observed = &r; return r }
	if !c.cfg.Enabled || c.cfg.BaseURL == "" {
		return record(Result{Intent: "general", FallbackReason: ReasonDisabled, DurationMs: ms(start)})
	}
	if c.circuitOpen() {
		return record(Result{Intent: "general", FallbackReason: ReasonCircuitOpen, DurationMs: ms(start)})
	}

	truncated := false
	if c.cfg.MaxInputRunes > 0 {
		if r := []rune(text); len(r) > c.cfg.MaxInputRunes {
			text = string(r[:c.cfg.MaxInputRunes])
			truncated = true
		}
	}

	body, _ := json.Marshal(classifyRequest{Input: text, Question: "intent"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.BaseURL, "/")+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		c.recordFailure()
		return record(Result{Intent: "general", FallbackReason: ReasonUnavailable, Truncated: truncated, DurationMs: ms(start)})
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		c.recordFailure()
		reason := ReasonUnavailable
		var netErr net.Error
		if ctx.Err() == context.DeadlineExceeded || (errors.As(err, &netErr) && netErr.Timeout()) {
			reason = ReasonTimeout
		}
		return record(Result{Intent: "general", FallbackReason: reason, Truncated: truncated, DurationMs: ms(start)})
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		c.recordFailure()
		return record(Result{Intent: "general", FallbackReason: ReasonUnavailable, Truncated: truncated, DurationMs: ms(start)})
	}

	var out classifyResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		c.recordFailure()
		return record(Result{Intent: "general", FallbackReason: ReasonBadResponse, Truncated: truncated, DurationMs: ms(start)})
	}
	c.recordSuccess()

	label := firstNonEmpty(out.Choice, out.Label, out.Intent)
	if !AllowedIntents[label] {
		return record(Result{Intent: "general", FallbackReason: ReasonUnknownLabel, ClassifierVersion: out.Version, Truncated: truncated, DurationMs: ms(start)})
	}
	conf := 1.0
	if out.Confidence != nil {
		conf = *out.Confidence
	} else if out.Score != nil {
		conf = *out.Score
	}
	if conf < c.cfg.MinConfidence {
		return record(Result{Intent: "general", Confidence: conf, FallbackReason: ReasonLowConfidence, ClassifierVersion: out.Version, Truncated: truncated, DurationMs: ms(start)})
	}
	return record(Result{Intent: label, Confidence: conf, ClassifierVersion: out.Version, Truncated: truncated, DurationMs: ms(start)})
}

// ReadyState is the sidecar's liveness plus checkpoint load state. A live
// process that has not loaded its checkpoint must not report classifier-ready.
type ReadyState struct {
	Alive           bool
	ClassifierReady bool
	Version         string
	Reason          string
}

// Ready probes the sidecar health endpoint and reports whether the classifier
// can actually classify. Liveness alone is never reported as readiness.
func (c *Client) Ready(ctx context.Context) ReadyState {
	if c.cfg.BaseURL == "" {
		return ReadyState{Reason: "not_configured"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.cfg.BaseURL, "/")+"/health", nil)
	if err != nil {
		return ReadyState{Reason: ReasonUnavailable}
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return ReadyState{Reason: ReasonUnavailable}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return ReadyState{Reason: ReasonUnavailable}
	}
	state := ReadyState{Alive: true, ClassifierReady: true}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil || len(bytes.TrimSpace(body)) == 0 {
		return state
	}
	var health struct {
		Status          string `json:"status"`
		ModelLoaded     *bool  `json:"model_loaded"`
		ClassifierReady *bool  `json:"classifier_ready"`
		Version         string `json:"version"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		// Non-JSON health body: the deployment only exposes liveness.
		return state
	}
	state.Version = health.Version
	if health.ClassifierReady != nil {
		state.ClassifierReady = *health.ClassifierReady
	} else if health.ModelLoaded != nil {
		state.ClassifierReady = *health.ModelLoaded
	}
	if health.Status != "" && !strings.EqualFold(health.Status, "ok") && !strings.EqualFold(health.Status, "ready") && !strings.EqualFold(health.Status, "healthy") {
		state.ClassifierReady = false
	}
	if !state.ClassifierReady {
		state.Reason = "checkpoint_not_ready"
	}
	return state
}

func (c *Client) circuitOpen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Before(c.openUntil)
}

func (c *Client) recordFailure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures++
	if c.failures >= 5 {
		c.openUntil = time.Now().Add(10 * time.Second)
		c.failures = 0
	}
}

func (c *Client) recordSuccess() {
	c.mu.Lock()
	c.failures = 0
	c.mu.Unlock()
}

func ms(start time.Time) int { return int(time.Since(start).Milliseconds()) }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Healthy reports whether the sidecar is live and able to classify.
func (c *Client) Healthy(ctx context.Context) bool { return c.Ready(ctx).ClassifierReady }

// Enabled reports whether semantic classification is switched on for this
// process. A disabled classifier is not degraded.
func (c *Client) Enabled() bool { return c.cfg.Enabled }

// FromConfig builds a client from the application config.
func FromConfig(cfg config.LayaConfig) *Client {
	return New(Config{
		BaseURL: cfg.BaseURL, APIKey: cfg.APIKey, Timeout: cfg.Timeout,
		MinConfidence: cfg.Confidence, Enabled: cfg.Enabled, MaxInputRunes: cfg.MaxInputRune,
	})
}
