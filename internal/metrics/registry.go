package metrics

import (
	"context"
	"net/http"

	"github.com/hchw/slogan/internal/apperr"
	"github.com/hchw/slogan/internal/httpx"
)

// Registry combines on-demand collectors with in-process counters.
type Registry struct {
	collectors []Collector
	counters   *Counters
}

// NewRegistry builds a registry. The counters set is always included.
func NewRegistry(counters *Counters, collectors ...Collector) *Registry {
	if counters == nil {
		counters = NewCounters()
	}
	return &Registry{collectors: collectors, counters: counters}
}

// Counters exposes the in-process counter set for instrumentation.
func (r *Registry) Counters() *Counters { return r.counters }

// Gather collects every sample. A failing collector is skipped instead of
// failing the whole scrape, so one unavailable dependency cannot blind the
// remaining metrics.
func (r *Registry) Gather(ctx context.Context) []Sample {
	if r == nil {
		return nil
	}
	out := r.counters.Snapshot()
	for _, c := range r.collectors {
		samples, err := c.Collect(ctx)
		if err != nil {
			continue
		}
		out = append(out, samples...)
	}
	return out
}

// Handler serves the Prometheus text exposition format.
func (r *Registry) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(Render(r.Gather(req.Context()))))
	}
}

// TokenHandler serves metrics only to a caller presenting the configured
// bearer token. Without a configured token the handler refuses to serve, so a
// misconfigured deployment fails closed instead of publishing metrics.
func (r *Registry) TokenHandler(token string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if token == "" {
			httpx.WriteError(w, req, apperr.Unauthenticated("metrics endpoint is not configured"))
			return
		}
		if req.Header.Get("Authorization") != "Bearer "+token {
			httpx.WriteError(w, req, apperr.Unauthenticated("metrics endpoint requires the scrape token"))
			return
		}
		r.Handler()(w, req)
	}
}
