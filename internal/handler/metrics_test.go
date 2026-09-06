package handler_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/handler"
	"github.com/calnode/calnode/internal/metrics"
)

const metricsToken = "metrics-token-for-tests"

func doMetrics(h *handler.Handler, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.Metrics(rec, req)
	return rec
}

// Unset METRICS_TOKEN ⇒ 404, byte-identical to the mux's own not-found. A 401 would
// confirm the endpoint exists on an instance whose operator never opted in to publishing
// its booking rate.
func TestMetrics_404WithoutAToken(t *testing.T) {
	h, _ := newTestHandlerDB(t)

	rec := doMetrics(h, "Bearer "+metricsToken)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 — %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "calnode_") {
		t.Error("body leaked metrics despite the endpoint being off")
	}
}

func TestMetrics_rejectsWrongOrMissingBearer(t *testing.T) {
	cases := map[string]string{
		"no header":              "",
		"empty bearer":           "Bearer ",
		"wrong token":            "Bearer nope",
		"right token, no scheme": metricsToken,
		"basic auth":             "Basic " + metricsToken,
		// A prefix of the real token must not pass: the comparison is over digests, so
		// length tells an attacker nothing either.
		"token prefix": "Bearer " + metricsToken[:10],
	}
	for name, auth := range cases {
		t.Run(name, func(t *testing.T) {
			h, _ := newTestHandlerDB(t)
			h.SetMetricsToken(metricsToken)

			rec := doMetrics(h, auth)

			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d; want 404 for %q", rec.Code, auth)
			}
		})
	}
}

func TestMetrics_servesExpositionWithTheToken(t *testing.T) {
	metrics.Reset()
	h, database := newTestHandlerDB(t)
	h.SetMetricsToken(metricsToken)

	// Two pending jobs and one failed one, so the gauges are read from the table rather
	// than reported as zero. The payload differs per row: jobs carries UNIQUE(type,
	// payload), which is what makes an enqueue idempotent.
	for _, row := range []struct{ id, status string }{
		{"job-1", "pending"}, {"job-2", "pending"}, {"job-3", "failed"}, {"job-4", "done"},
	} {
		if _, err := database.Exec(
			`INSERT INTO jobs (id, type, payload, run_at, status) VALUES (?, 'webhook.deliver', ?, '2026-01-01T00:00:00Z', ?)`,
			row.id, `{"webhook_delivery_id":"`+row.id+`"}`, row.status); err != nil {
			t.Fatalf("seed job %s: %v", row.id, err)
		}
	}

	rec := doMetrics(h, "Bearer "+metricsToken)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 — %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != metrics.ContentType {
		t.Errorf("Content-Type = %q; want %q", ct, metrics.ContentType)
	}
	// A cached scrape shows a frozen instance as a healthy one.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q; want no-store", cc)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"calnode_jobs_pending 2",
		"calnode_jobs_failed_total 1",
		"# TYPE calnode_build_info gauge",
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}
