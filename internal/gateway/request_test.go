package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProviderExtensionsRequireAllowlist(t *testing.T) {
	req, err := ParseRequest([]byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`))
	if err != nil {
		t.Fatalf("parse extension for provider validation: %v", err)
	}
	if err := ValidateExtensions(req, nil); err == nil {
		t.Fatal("non-allowlisted provider extension accepted")
	}
	if err := ValidateExtensions(req, []string{"reasoning_effort"}); err != nil {
		t.Fatalf("allowlisted extension rejected: %v", err)
	}
}

func TestParseRequestValid(t *testing.T) {
	req, err := ParseRequest([]byte(`{"model":"auto","stream":true,"max_tokens":100,
        "messages":[{"role":"system","content":"be nice"},{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !req.Stream || req.MaxTokens != 100 || !req.HasMaxTok {
		t.Fatalf("unexpected parse: %+v", req)
	}
	if req.LastUserText() != "hello" {
		t.Fatalf("last user text = %q", req.LastUserText())
	}
}

func TestParseRequestDetectsVisionAndTools(t *testing.T) {
	req, err := ParseRequest([]byte(`{"model":"auto",
        "messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":"x"}}]}],
        "tools":[{"type":"function","function":{"name":"f"}}]}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !req.NeedsVision || req.ToolNames != 1 {
		t.Fatalf("unexpected: vision=%v tools=%d", req.NeedsVision, req.ToolNames)
	}
}

func TestCanonicalDigestStable(t *testing.T) {
	a, err := ParseRequest([]byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ParseRequest([]byte(`{"messages":[{"role":"user","content":"hi"}],"model":"auto"}`))
	if CanonicalDigest(a.Body) != CanonicalDigest(b.Body) {
		t.Fatal("digest should be stable regardless of key order")
	}
	c, _ := ParseRequest([]byte(`{"model":"auto","messages":[{"role":"user","content":"bye"}]}`))
	if CanonicalDigest(a.Body) == CanonicalDigest(c.Body) {
		t.Fatal("digest should differ for different content")
	}
}

func TestEstimateInputTokensGrows(t *testing.T) {
	small, _ := ParseRequest([]byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	large, _ := ParseRequest([]byte(`{"model":"auto","messages":[{"role":"user","content":"` +
		"this is a much longer message with many many words to estimate" + `"}]}`))
	if EstimateInputTokens(large) <= EstimateInputTokens(small) {
		t.Fatal("expected larger message to estimate more tokens")
	}
}

func TestComputeReservationChecksOverflow(t *testing.T) {
	if amount, err := ComputeReservation(10, 5, 100, 200); err != nil || amount != 2000 {
		t.Fatalf("reservation=%d err=%v", amount, err)
	}
	if _, err := ComputeReservation(int(^uint(0)>>1), 1, int64(^uint64(0)>>1), 1); err == nil {
		t.Fatal("expected overflow to be rejected")
	}
}

func TestEstimateInputTokensForModelIsBounded(t *testing.T) {
	large := &Messages{Messages: []Message{{Role: "user", Content: json.RawMessage(`"` + strings.Repeat("x", 4<<20) + `"`)}}}
	if _, err := EstimateInputTokensForModel(large, "no-model-tokenizer"); err == nil {
		t.Fatal("expected estimator byte bound")
	}
}

func TestExtractUsage(t *testing.T) {
	u, ok := ExtractUsage([]byte(`{"usage":{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46}}`))
	if !ok || u.PromptTokens != 12 || u.CompletionTokens != 34 {
		t.Fatalf("got %+v ok=%v", u, ok)
	}
	if _, ok := ExtractUsage([]byte(`{"choices":[]}`)); ok {
		t.Fatal("missing usage should not be trusted")
	}
	if _, ok := ExtractUsage([]byte(`{"usage":{"prompt_tokens":0,"completion_tokens":0}}`)); ok {
		t.Fatal("zero usage should be invalid")
	}
}

func TestRewriteModel(t *testing.T) {
	out := rewriteModel([]byte(`{"model":"upstream-key","choices":[]}`), "display-name")
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "display-name" {
		t.Fatalf("expected rewritten model, got %v", got["model"])
	}
}
