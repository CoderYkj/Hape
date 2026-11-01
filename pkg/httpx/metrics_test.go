package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gamebackend/pkg/httpx"
)

func scrape(t *testing.T, m *httpx.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	return rec.Body.String()
}

// TestDomainMetricsRenderInPrometheusFormat pins the naming contract from
// rules/95-observability.md: backend_<domain>_<unit>, counters end in _total.
func TestDomainMetricsRenderInPrometheusFormat(t *testing.T) {
	m := httpx.NewMetrics("platform")
	m.AddCounter("release_failed", 2)
	m.AddCounter("release_failed", 1)
	m.SetGauge("release_pending_sessions", 3)
	m.SetGauge("release_oldest_pending_seconds", 12.5)

	body := scrape(t, m)
	for _, want := range []string{
		"# TYPE backend_release_failed_total counter",
		`backend_release_failed_total{service="platform"} 3`,
		"# TYPE backend_release_pending_sessions gauge",
		`backend_release_pending_sessions{service="platform"} 3`,
		`backend_release_oldest_pending_seconds{service="platform"} 12.5`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output is missing %q:\n%s", want, body)
		}
	}
	// A whole-number gauge must not render as 3.000000: curl output is read by humans.
	if strings.Contains(body, "3.000000") {
		t.Fatalf("integer gauge rendered with float noise:\n%s", body)
	}
}

// TestGaugeIsASnapshotNotADelta: the sweep publishes the current state of the world, and
// a later tick must overwrite it - otherwise the number only ever grows.
func TestGaugeIsASnapshotNotADelta(t *testing.T) {
	m := httpx.NewMetrics("platform")
	m.SetGauge("release_pending_sessions", 5)
	m.SetGauge("release_pending_sessions", 0)
	body := scrape(t, m)
	if !strings.Contains(body, `backend_release_pending_sessions{service="platform"} 0`) {
		t.Fatalf("gauge did not take the later value:\n%s", body)
	}
	if strings.Contains(body, "release_pending_sessions{service=\"platform\"} 5") {
		t.Fatalf("the old gauge value is still published:\n%s", body)
	}
}

// TestRequestMetricsSurviveAlongsideDomainMetrics keeps the existing contract intact.
func TestRequestMetricsSurviveAlongsideDomainMetrics(t *testing.T) {
	m := httpx.NewMetrics("platform")
	m.Record("session.create", 200)
	m.SetGauge("release_pending_sessions", 1)
	body := scrape(t, m)
	if !strings.Contains(body, `backend_requests_total{service="platform",op="session.create",status="200"} 1`) {
		t.Fatalf("request counter missing:\n%s", body)
	}
	if !strings.Contains(body, "backend_start_time_seconds") {
		t.Fatalf("start time missing:\n%s", body)
	}
}
