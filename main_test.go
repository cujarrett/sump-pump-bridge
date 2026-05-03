package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newTestApp creates an app with metrics wired but no NATS connection (js=nil).
// This means webhookHandler will update metrics but skip any NATS publish calls.
func newTestApp(t *testing.T, threshold float64) *app {
	t.Helper()
	wattsGauge := prometheus.NewGauge(prometheus.GaugeOpts{Name: "t_sump_pump_watts_" + t.Name(), Help: "test"})
	runningGauge := prometheus.NewGauge(prometheus.GaugeOpts{Name: "t_sump_pump_running_" + t.Name(), Help: "test"})
	runsTotal := prometheus.NewCounter(prometheus.CounterOpts{Name: "t_sump_pump_runs_total_" + t.Name(), Help: "test"})
	requestsTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "t_http_requests_total_" + t.Name(), Help: "test"},
		[]string{"method", "path", "status_code"},
	)
	requestDuration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "t_http_request_duration_seconds_" + t.Name(), Help: "test", Buckets: prometheus.DefBuckets},
		[]string{"method", "path"},
	)
	return &app{
		threshold:       threshold,
		wattsGauge:      wattsGauge,
		runningGauge:    runningGauge,
		runsTotal:       runsTotal,
		requestsTotal:   requestsTotal,
		requestDuration: requestDuration,
	}
}

func TestHealthHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	healthHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Fatalf("expected status=ok, got %q", body["status"])
	}
}

func TestNotFoundHandler(t *testing.T) {
	for _, path := range []string{"/", "/.env", "/.git/config", "/wp-login.php"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		notFoundHandler(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("path %q: expected 404, got %d", path, w.Code)
		}
		if w.Body.Len() != 0 {
			t.Errorf("path %q: expected empty body", path)
		}
	}
}

func TestWebhookHandlerJSONBody(t *testing.T) {
	a := newTestApp(t, 50)
	body, _ := json.Marshal(map[string]float64{"apower": 85.3})
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.webhookHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp["running"] != true {
		t.Errorf("expected running=true for 85.3W, got %v", resp["running"])
	}
}

func TestWebhookHandlerQueryParam(t *testing.T) {
	a := newTestApp(t, 50)
	req := httptest.NewRequest(http.MethodPost, "/webhook?apower=10", nil)
	w := httptest.NewRecorder()
	a.webhookHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp["running"] != false {
		t.Errorf("expected running=false for 10W, got %v", resp["running"])
	}
}

// TestWebhookHandlerGETQueryParam verifies Shelly Gen3 GET webhook format works.
func TestWebhookHandlerGETQueryParam(t *testing.T) {
	a := newTestApp(t, 50)
	req := httptest.NewRequest(http.MethodGet, "/webhook?apower=350", nil)
	w := httptest.NewRecorder()
	a.webhookHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp["running"] != true {
		t.Errorf("expected running=true for 350W, got %v", resp["running"])
	}
}

func TestWebhookHandlerNoBody(t *testing.T) {
	a := newTestApp(t, 50)
	req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
	w := httptest.NewRecorder()
	a.webhookHandler(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestWebhookHandlerWrongMethod(t *testing.T) {
	a := newTestApp(t, 50)
	req := httptest.NewRequest(http.MethodPut, "/webhook", nil)
	w := httptest.NewRecorder()
	a.webhookHandler(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestWebhookHandlerInvalidQueryParam(t *testing.T) {
	a := newTestApp(t, 50)
	req := httptest.NewRequest(http.MethodPost, "/webhook?apower=notanumber", nil)
	w := httptest.NewRecorder()
	a.webhookHandler(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// TestWebhookHandlerStateTransitions verifies edge detection:
// runsTotal only increments on idle→running transitions, not on each webhook call.
func TestWebhookHandlerStateTransitions(t *testing.T) {
	a := newTestApp(t, 50)

	postWatts := func(w float64) {
		t.Helper()
		body, _ := json.Marshal(map[string]float64{"apower": w})
		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		a.webhookHandler(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %.0fW: expected 200, got %d", w, rec.Code)
		}
	}

	// 85W → running (first transition, runsTotal=1)
	postWatts(85)
	if got := testutil.ToFloat64(a.runsTotal); got != 1 {
		t.Errorf("after first run: expected runsTotal=1, got %v", got)
	}
	if got := testutil.ToFloat64(a.runningGauge); got != 1 {
		t.Errorf("after first run: expected runningGauge=1, got %v", got)
	}

	// 90W → still running (no transition, runsTotal stays 1)
	postWatts(90)
	if got := testutil.ToFloat64(a.runsTotal); got != 1 {
		t.Errorf("still running: expected runsTotal=1, got %v", got)
	}

	// 5W → idle (transition, runsTotal unchanged)
	postWatts(5)
	if got := testutil.ToFloat64(a.runningGauge); got != 0 {
		t.Errorf("after idle: expected runningGauge=0, got %v", got)
	}
	if got := testutil.ToFloat64(a.runsTotal); got != 1 {
		t.Errorf("after idle: expected runsTotal=1 (idle doesn't count), got %v", got)
	}

	// 70W → running again (second cycle, runsTotal=2)
	postWatts(70)
	if got := testutil.ToFloat64(a.runsTotal); got != 2 {
		t.Errorf("second run: expected runsTotal=2, got %v", got)
	}
}

func TestMetricsMiddlewareNormalizesUnknownPaths(t *testing.T) {
	a := newTestApp(t, 50)
	handler := a.metricsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	for _, path := range []string{"/.env", "/wp-login.php"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
	}

	if got := testutil.ToFloat64(a.requestsTotal.WithLabelValues("GET", "unknown", "404")); got != 2 {
		t.Fatalf("expected counter=2 for unknown paths, got %v", got)
	}
}

func TestMetricsMiddlewareSkipsHealthPath(t *testing.T) {
	a := newTestApp(t, 50)
	handler := a.metricsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// /health is skipped — counter for any label combination should be 0
	if got := testutil.ToFloat64(a.requestsTotal.WithLabelValues("GET", "/health", "200")); got != 0 {
		t.Fatalf("expected /health to be skipped, got counter=%v", got)
	}
}
