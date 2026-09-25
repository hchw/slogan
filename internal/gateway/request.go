// Package gateway implements the OpenAI-compatible data plane: request
// validation, authentication, rate limiting, routing, quota reservation,
// provider relay, streaming, settlement and idempotency.
package gateway

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/hchw/slogan/internal/apperr"
	"github.com/hchw/slogan/internal/secure"
	"github.com/hchw/slogan/internal/tokenest"
)

// allowedTopLevel is the set of supported standard request fields. Provider
// extensions are only forwarded when explicitly allow-listed.
var allowedTopLevel = map[string]bool{
	"model": true, "messages": true, "stream": true, "stream_options": true,
	"max_tokens": true, "max_completion_tokens": true, "temperature": true,
	"top_p": true, "stop": true, "n": true, "presence_penalty": true,
	"frequency_penalty": true, "seed": true, "response_format": true,
	"tools": true, "tool_choice": true, "parallel_tool_calls": true,
	"user": true, "logprobs": true, "top_logprobs": true, "service_tier": true,
	"logit_bias": true,
}

// Messages is the parsed chat request.
type Messages struct {
	Model       string
	Stream      bool
	MaxTokens   int
	HasMaxTok   bool
	ToolNames   int
	NeedsVision bool
	Extensions  []string
	Body        map[string]json.RawMessage
	Messages    []Message
}

// Message is a single chat message.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Name    string          `json:"name,omitempty"`
}

// ParseRequest validates the request field set and extracts routing inputs.
// Unsupported standard fields and non-allowlisted extensions are rejected.
func ParseRequest(raw []byte) (*Messages, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, apperr.InvalidRequest("", "invalid JSON body: "+err.Error())
	}
	var extensions []string
	for k := range body {
		if !allowedTopLevel[k] {
			extensions = append(extensions, k)
		}
	}
	out := &Messages{Body: body, Extensions: extensions}

	if v, ok := body["model"]; ok {
		if err := json.Unmarshal(v, &out.Model); err != nil {
			return nil, apperr.InvalidRequest("model", "model must be a string")
		}
	}
	if out.Model == "" {
		return nil, apperr.MissingParam("model is required")
	}
	if v, ok := body["stream"]; ok {
		if err := json.Unmarshal(v, &out.Stream); err != nil {
			return nil, apperr.InvalidRequest("stream", "stream must be a boolean")
		}
	}
	if v, ok := body["max_tokens"]; ok {
		if err := json.Unmarshal(v, &out.MaxTokens); err != nil {
			return nil, apperr.InvalidRequest("max_tokens", "max_tokens must be an integer")
		}
		out.HasMaxTok = true
	} else if v, ok := body["max_completion_tokens"]; ok {
		if err := json.Unmarshal(v, &out.MaxTokens); err != nil {
			return nil, apperr.InvalidRequest("max_completion_tokens", "max_completion_tokens must be an integer")
		}
		out.HasMaxTok = true
	}
	if out.MaxTokens < 0 {
		return nil, apperr.InvalidRequest("max_tokens", "max_tokens must be non-negative")
	}

	msgsRaw, ok := body["messages"]
	if !ok {
		return nil, apperr.MissingParam("messages is required")
	}
	if err := json.Unmarshal(msgsRaw, &out.Messages); err != nil {
		return nil, apperr.InvalidRequest("messages", "messages must be an array")
	}
	if len(out.Messages) == 0 {
		return nil, apperr.InvalidRequest("messages", "messages must not be empty")
	}
	for _, m := range out.Messages {
		if m.Role == "" {
			return nil, apperr.InvalidRequest("messages", "each message requires a role")
		}
		if contentNeedsVision(m.Content) {
			out.NeedsVision = true
		}
	}

	if v, ok := body["tools"]; ok {
		var tools []json.RawMessage
		if err := json.Unmarshal(v, &tools); err != nil {
			return nil, apperr.InvalidRequest("tools", "tools must be an array")
		}
		out.ToolNames = len(tools)
	}
	return out, nil
}

func contentNeedsVision(content json.RawMessage) bool {
	if len(content) == 0 {
		return false
	}
	// multipart content containing an image_url or image type
	s := string(content)
	return strings.Contains(s, "image_url") || strings.Contains(s, `"type":"image"`) || strings.Contains(s, `"type": "image"`)
}

// ValidateExtensions rejects top-level provider-specific fields that are not
// explicitly allow-listed by the selected provider.
func ValidateExtensions(m *Messages, allowlist []string) error {
	allowed := make(map[string]bool, len(allowlist))
	for _, name := range allowlist {
		allowed[name] = true
	}
	for _, name := range m.Extensions {
		if !allowed[name] {
			return apperr.InvalidRequest(name, fmt.Sprintf("unsupported field: %s", name))
		}
	}
	return nil
}

// CanonicalDigest returns a stable digest of the request for idempotency.
func CanonicalDigest(body map[string]json.RawMessage) string {
	normalized, err := json.Marshal(body)
	if err != nil {
		return ""
	}
	return secure.Digest(normalized)
}

// LastUserText returns the latest user message text for classification.
func (m *Messages) LastUserText() string {
	for i := len(m.Messages) - 1; i >= 0; i-- {
		if m.Messages[i].Role != "user" {
			continue
		}
		var s string
		if err := json.Unmarshal(m.Messages[i].Content, &s); err == nil {
			return s
		}
		// multipart: concatenate text parts
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(m.Messages[i].Content, &parts); err == nil {
			var b strings.Builder
			for _, p := range parts {
				if p.Text != "" {
					b.WriteString(p.Text)
					b.WriteString("\n")
				}
			}
			return b.String()
		}
	}
	return ""
}

// EstimateInputTokens is a conservative fallback estimator used only for
// reservations, never for final billing.
func EstimateInputTokens(m *Messages) int {
	tokens, err := EstimateInputTokensForModel(m, "")
	if err != nil {
		return 0
	}
	return tokens
}

// EstimateInputTokensForModel uses a registered provider/model tokenizer when
// available, otherwise the bounded conservative estimator. The result is used
// only to reserve quota and check context, never for final billing.
func ComputeReservation(inputTokens, maxOutput int, inputRate, outputRate int64) (int64, error) {
	if inputTokens < 0 || maxOutput < 0 || inputRate < 0 || outputRate < 0 {
		return 0, fmt.Errorf("reservation inputs must be non-negative")
	}
	in, out := int64(inputTokens), int64(maxOutput)
	if inputRate != 0 && in > math.MaxInt64/inputRate {
		return 0, fmt.Errorf("reservation overflow")
	}
	if outputRate != 0 && out > math.MaxInt64/outputRate {
		return 0, fmt.Errorf("reservation overflow")
	}
	inputCost, outputCost := in*inputRate, out*outputRate
	if inputCost > math.MaxInt64-outputCost {
		return 0, fmt.Errorf("reservation overflow")
	}
	return inputCost + outputCost, nil
}

func EstimateInputTokensForModel(m *Messages, modelKey string) (int, error) {
	parts := make([]string, 0, len(m.Messages)*2)
	for _, msg := range m.Messages {
		parts = append(parts, msg.Role)
		if len(msg.Content) > 0 {
			parts = append(parts, string(msg.Content))
		}
	}
	tokens, _, err := tokenest.Default.Estimate(modelKey, tokenest.JoinMessages(parts...))
	if err != nil {
		return 0, err
	}
	return tokens + len(m.Messages)*4, nil
}

// Usage is validated provider-reported token usage.
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
}

// Valid reports whether usage looks trustworthy.
func (u Usage) Valid() bool {
	return u.PromptTokens >= 0 && u.CompletionTokens >= 0 && (u.PromptTokens+u.CompletionTokens) > 0
}

// ExtractUsage parses usage from a response body, returning ok=false when it is
// missing or malformed.
func ExtractUsage(body []byte) (Usage, bool) {
	var env struct {
		Usage *struct {
			PromptTokens     *int64 `json:"prompt_tokens"`
			CompletionTokens *int64 `json:"completion_tokens"`
			TotalTokens      *int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Usage == nil {
		return Usage{}, false
	}
	if env.Usage.PromptTokens == nil || env.Usage.CompletionTokens == nil {
		return Usage{}, false
	}
	u := Usage{PromptTokens: *env.Usage.PromptTokens, CompletionTokens: *env.Usage.CompletionTokens}
	if !u.Valid() {
		return Usage{}, false
	}
	return u, true
}
