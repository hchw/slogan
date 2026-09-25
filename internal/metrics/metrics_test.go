package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRenderPrometheusFormatAndLabelEscaping(t *testing.T) {
	samples := []Sample{
		{Name: "slogan_gateway_requests_total", Help: "Gateway requests by terminal status.", Type: "counter", Labels: map[string]string{"status": "success"}, Value: 7},
		{Name: "slogan_gateway_requests_total", Type: "counter", Labels: map[string]string{"status": "stream_broken"}, Value: 1},
		{Name: "slogan_laya_fallback_total", Help: "Fallbacks by reason.", Type: "counter", Labels: map[string]string{"reason": `bad"quote`}, Value: 2},
	}
	out := Render(samples)
	for _, want := range []string{
		"# TYPE slogan_gateway_requests_total counter",
		`slogan_gateway_requests_total{status="success"} 7`,
		`slogan_gateway_requests_total{status="stream_broken"} 1`,
		`reason="bad\"quote"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Count(out, "# HELP slogan_gateway_requests_total") != 1 {
		t.Fatalf("help must be emitted once:\n%s", out)
	}
}

func TestCountersAccumulateByLabel(t *testing.T) {
	counters := NewCounters()
	counters.Help("slogan_gateway_requests_total", "Gateway requests by terminal status.")
	counters.Inc("slogan_gateway_requests_total", map[string]string{"status": "success"})
	counters.Inc("slogan_gateway_requests_total", map[string]string{"status": "success"})
	counters.Inc("slogan_gateway_requests_total", map[string]string{"status": "upstream_error"})
	snapshot := counters.Snapshot()
	if len(snapshot) != 2 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	values := map[string]float64{}
	for _, s := range snapshot {
		values[s.Labels["status"]] = s.Value
	}
	if values["success"] != 2 || values["upstream_error"] != 1 {
		t.Fatalf("values=%v", values)
	}
}

func TestMetricsEndpointFailsClosedWithoutTokenAndSkipsBrokenCollectors(t *testing.T) {
	counters := NewCounters()
	counters.Inc("slogan_gateway_requests_total", map[string]string{"status": "success"})
	registry := NewRegistry(counters,
		FuncCollector(func(context.Context) ([]Sample, error) { return nil, context.DeadlineExceeded }),
		FuncCollector(func(context.Context) ([]Sample, error) {
			return []Sample{{Name: "slogan_gateway_nodes_serving", Type: "gauge", Value: 2}}, nil
		}),
	)

	unauthorized := httptest.NewRecorder()
	registry.TokenHandler("scrape-token")(unauthorized, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status=%d", unauthorized.Code)
	}

	unconfigured := httptest.NewRecorder()
	registry.TokenHandler("")(unconfigured, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if unconfigured.Code != http.StatusUnauthorized {
		t.Fatalf("unconfigured token must not serve metrics: %d", unconfigured.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer scrape-token")
	ok := httptest.NewRecorder()
	registry.TokenHandler("scrape-token")(ok, req)
	if ok.Code != http.StatusOK {
		t.Fatalf("authorized status=%d", ok.Code)
	}
	body := ok.Body.String()
	if !strings.Contains(body, "slogan_gateway_nodes_serving 2") {
		t.Fatalf("healthy collector missing:\n%s", body)
	}
	if !strings.Contains(body, `slogan_gateway_requests_total{status="success"} 1`) {
		t.Fatalf("in-process counters missing:\n%s", body)
	}
	if !strings.Contains(ok.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("content type=%s", ok.Header().Get("Content-Type"))
	}
}
