package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hchw/slogan/internal/apperr"
	"github.com/hchw/slogan/internal/httpx"
	"github.com/hchw/slogan/internal/provider"
	"github.com/hchw/slogan/internal/requestrec"
)

func (s *Service) dispatch(w http.ResponseWriter, r *http.Request, in dispatchInput) {
	ctx := r.Context()

	// Build the forwarded body, replacing only the model identifier.
	forward := make(map[string]json.RawMessage, len(in.req.Body))
	for k, v := range in.req.Body {
		forward[k] = v
	}
	modelKey, _ := json.Marshal(in.selected.ModelKey)
	forward["model"] = modelKey
	body, err := json.Marshal(forward)
	if err != nil {
		s.settleRecord(ctx, in.record, nil, requestrec.StatusInvalidRequest, "invalid_request_error", time.Since(in.start))
		httpx.WriteOpenAIError(w, r, apperr.Internal("failed to build upstream request"))
		return
	}

	prov, err := s.provider.Get(ctx, in.selected.ProviderID)
	if err != nil {
		s.settleRecord(ctx, in.record, nil, requestrec.StatusUpstreamError, "upstream_error", time.Since(in.start))
		httpx.WriteOpenAIError(w, r, apperr.UpstreamError("provider configuration unavailable"))
		return
	}
	secret, err := s.provider.Secret(ctx, in.selected.ProviderID)
	if err != nil {
		s.settleRecord(ctx, in.record, nil, requestrec.StatusUpstreamError, "upstream_error", time.Since(in.start))
		httpx.WriteOpenAIError(w, r, apperr.UpstreamError("provider credential unavailable"))
		return
	}
	client := provider.NewClient(prov.BaseURL, secret, prov.AuthType, s.upstreamTimeout())

	_ = s.pool.WithTx(ctx, func(tx pgx.Tx) error {
		return s.reqs.SetUpstreamPending(ctx, tx, in.record.RequestID)
	})

	if in.req.Stream {
		s.relayStream(w, r, client, body, in)
		return
	}
	s.relayBuffered(w, r, client, body, in)
}

func (s *Service) upstreamTimeout() time.Duration {
	// A generous default; providers that stream keep the connection open.
	return 5 * time.Minute
}

func (s *Service) relayBuffered(w http.ResponseWriter, r *http.Request, client *provider.Client, body []byte, in dispatchInput) {
	ctx := r.Context()
	resp, err := client.Chat(ctx, body)
	if err != nil {
		status := requestrec.StatusUpstreamError
		code := "upstream_error"
		if ctx.Err() == context.DeadlineExceeded {
			status, code = requestrec.StatusGatewayTimeout, "upstream_timeout"
		}
		s.settleRecord(ctx, in.record, nil, status, code, time.Since(in.start))
		httpx.WriteOpenAIError(w, r, apperr.UpstreamError("upstream request failed"))
		return
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		s.settleRecord(ctx, in.record, nil, requestrec.StatusUpstreamError, "upstream_error", time.Since(in.start))
		httpx.WriteOpenAIError(w, r, apperr.UpstreamError("failed to read upstream response"))
		return
	}
	if resp.StatusCode/100 != 2 {
		var usg *Usage
		if u, ok := ExtractUsage(data); ok {
			usg = &u
		}
		s.settleRecord(ctx, in.record, usg, requestrec.StatusUpstreamError, "upstream_error", time.Since(in.start))
		httpx.WriteOpenAIError(w, r, apperr.UpstreamError("upstream returned an error"))
		return
	}
	usg, ok := ExtractUsage(data)
	var usagePtr *Usage
	if ok {
		usagePtr = &usg
	}
	out := rewriteModel(data, in.selected.Name)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	s.settleRecord(ctx, in.record, usagePtr, requestrec.StatusSuccess, "", time.Since(in.start))
}

func (s *Service) relayStream(w http.ResponseWriter, r *http.Request, client *provider.Client, body []byte, in dispatchInput) {
	ctx := r.Context()
	resp, err := client.Chat(ctx, body)
	if err != nil {
		status := requestrec.StatusUpstreamError
		code := "upstream_error"
		if ctx.Err() == context.DeadlineExceeded {
			status, code = requestrec.StatusGatewayTimeout, "upstream_timeout"
		}
		s.settleRecord(ctx, in.record, nil, status, code, time.Since(in.start))
		httpx.WriteOpenAIError(w, r, apperr.UpstreamError("upstream request failed"))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var usagePtr *Usage
		if u, ok := ExtractUsage(data); ok {
			usagePtr = &u
		}
		s.settleRecord(ctx, in.record, usagePtr, requestrec.StatusUpstreamError, "upstream_error", time.Since(in.start))
		httpx.WriteOpenAIError(w, r, apperr.UpstreamError("upstream returned an error"))
		return
	}

	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReaderSize(resp.Body, 64<<10)
	var (
		headersSent bool
		sawDone     bool
		usage       *Usage
	)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if !headersSent {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Connection", "keep-alive")
				w.WriteHeader(http.StatusOK)
				headersSent = true
			}
			trimmed := strings.TrimSpace(string(line))
			if strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")) == "[DONE]" {
				sawDone = true
			}
			if u := scrapeStreamUsage(line); u != nil {
				usage = u
			}
			if _, werr := w.Write(rewriteStreamModel(line, in.selected.Name)); werr != nil {
				s.settleRecord(ctx, in.record, usage, requestrec.StatusClientDisconnect, "client_disconnected", time.Since(in.start))
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if ctx.Err() != nil {
				s.settleRecord(ctx, in.record, usage, requestrec.StatusClientDisconnect, "client_disconnected", time.Since(in.start))
				return
			}
			if readErr == io.EOF && headersSent && sawDone {
				s.settleRecord(ctx, in.record, usage, requestrec.StatusSuccess, "", time.Since(in.start))
				return
			}
			if headersSent {
				writeSSEError(w, flusher, "The stream was interrupted.")
				s.settleRecord(ctx, in.record, usage, requestrec.StatusStreamBroken, "stream_error", time.Since(in.start))
				return
			}
			if readErr == io.EOF {
				s.settleRecord(ctx, in.record, nil, requestrec.StatusUpstreamError, "upstream_error", time.Since(in.start))
				httpx.WriteOpenAIError(w, r, apperr.UpstreamError("upstream returned an empty stream"))
				return
			}
			s.settleRecord(ctx, in.record, nil, requestrec.StatusUpstreamError, "upstream_error", time.Since(in.start))
			httpx.WriteOpenAIError(w, r, apperr.UpstreamError("upstream stream failed before the first event"))
			return
		}
	}
}

func writeSSEError(w io.Writer, flusher http.Flusher, msg string) {
	b, _ := json.Marshal(map[string]any{"error": map[string]string{"message": msg, "type": "api_error", "code": "stream_error"}})
	_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

// rewriteModel replaces the response model field with the actual model name.
func rewriteModel(body []byte, name string) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if _, ok := obj["model"]; !ok {
		return body
	}
	nb, _ := json.Marshal(name)
	obj["model"] = nb
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

func rewriteStreamModel(line []byte, name string) []byte {
	trimmed := strings.TrimRight(string(line), "\r\n")
	if !strings.HasPrefix(trimmed, "data:") {
		return line
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		return line
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		return line
	}
	if _, ok := obj["model"]; !ok {
		return line
	}
	nb, _ := json.Marshal(name)
	obj["model"] = nb
	out, err := json.Marshal(obj)
	if err != nil {
		return line
	}
	return []byte("data: " + string(out) + "\n\n")
}

// scrapeStreamUsage extracts usage from a streamed SSE chunk, if present.
func scrapeStreamUsage(line []byte) *Usage {
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "data:") {
		return nil
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" || !bytes.Contains([]byte(payload), []byte("usage")) {
		return nil
	}
	if u, ok := ExtractUsage([]byte(payload)); ok {
		return &u
	}
	return nil
}
