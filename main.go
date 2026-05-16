package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// version is set at build time via -ldflags="-X main.version=x.y.z".
var version = "dev"

// app holds all dependencies. Config is read once in main(); handlers are methods on *app.
// js is nil in tests — any NATS publish call is skipped when js is nil.
type app struct {
	js        jetstream.JetStream // nil in unit tests
	threshold float64

	// prevRunning tracks the last published state for edge detection.
	// Only transitions (idle→running, running→idle) are published to NATS.
	mu          sync.Mutex
	prevRunning bool

	// metrics
	wattsGauge      prometheus.Gauge
	runningGauge    prometheus.Gauge
	runsTotal       prometheus.Counter
	requestsTotal   *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
}

// shellyPayload is the JSON body sent by the Shelly PM Mini Gen3 webhook.
// Configure the Shelly Action outbound body template as: {"apower":${apower}}
type shellyPayload struct {
	APower float64 `json:"apower"`
}

// statusResponseWriter wraps http.ResponseWriter to capture the written status code.
type statusResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *statusResponseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

var metricsSkipPaths = map[string]struct{}{
	"/healthz":     {},
	"/favicon.ico": {},
}

var metricsKnownPaths = map[string]struct{}{
	"/webhook": {},
}

func (a *app) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.requestsTotal == nil || a.requestDuration == nil {
			next.ServeHTTP(w, r)
			return
		}
		if _, skip := metricsSkipPaths[r.URL.Path]; skip {
			next.ServeHTTP(w, r)
			return
		}
		rw := &statusResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		pathLabel := r.URL.Path
		if _, ok := metricsKnownPaths[pathLabel]; !ok {
			pathLabel = "unknown"
		}
		start := time.Now()
		next.ServeHTTP(rw, r)
		a.requestsTotal.WithLabelValues(r.Method, pathLabel, strconv.Itoa(rw.statusCode)).Inc()
		a.requestDuration.WithLabelValues(r.Method, pathLabel).Observe(time.Since(start).Seconds())
	})
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","version":%q}`, version) //nolint:errcheck
}

func notFoundHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("404 %s %s %q", r.Method, r.URL.Path, r.Header.Get("User-Agent"))
	w.WriteHeader(http.StatusNotFound)
}

func writeJSONError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]string{"error": msg})
	w.Write(b) //nolint:errcheck
}

// processWatts applies the wattage reading: updates metrics and publishes a NATS event on
// state transitions (idle→running or running→idle). Safe to call from multiple goroutines.
func (a *app) processWatts(ctx context.Context, watts float64) {
	if watts < 0 {
		watts = 0
	}
	running := watts >= a.threshold

	if a.wattsGauge != nil {
		a.wattsGauge.Set(watts)
	}
	if a.runningGauge != nil {
		if running {
			a.runningGauge.Set(1)
		} else {
			a.runningGauge.Set(0)
		}
	}

	a.mu.Lock()
	changed := running != a.prevRunning
	if changed {
		a.prevRunning = running
	}
	a.mu.Unlock()

	if changed {
		subject := "home.appliance.sump-pump.idle"
		if running {
			subject = "home.appliance.sump-pump.running"
			if a.runsTotal != nil {
				a.runsTotal.Inc()
			}
		}
		if a.js != nil {
			payload, _ := json.Marshal(map[string]float64{"watts": watts})
			pubCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if _, err := a.js.Publish(pubCtx, subject, payload); err != nil {
				log.Printf("nats publish %s: %v", subject, err)
			} else {
				log.Printf("published %s (%.1fW)", subject, watts)
			}
		}
	}
}

func (a *app) webhookHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeJSONError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Shelly Gen3 sends GET with ?apower= query params.
	// Manual testing can use POST with {"apower":<watts>} JSON body or ?apower= query param.
	var watts float64
	var parsed bool

	var p shellyPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&p); err == nil {
		watts = p.APower
		parsed = true
	} else if q := r.URL.Query().Get("apower"); q != "" {
		v, err := strconv.ParseFloat(q, 64)
		if err != nil {
			writeJSONError(w, "apower must be a number", http.StatusBadRequest)
			return
		}
		watts = v
		parsed = true
	}

	if !parsed {
		writeJSONError(w, `provide {"apower":<watts>} in body or ?apower= query param`, http.StatusBadRequest)
		return
	}

	a.processWatts(r.Context(), watts)

	running := watts >= a.threshold
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"watts":%.1f,"running":%t}`, watts, running) //nolint:errcheck
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	metricsPort := os.Getenv("METRICS_PORT")
	if metricsPort == "" {
		metricsPort = "9090"
	}
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = "nats://localhost:4222"
	}
	threshold := 300.0
	if t := os.Getenv("WATTS_THRESHOLD"); t != "" {
		if v, err := strconv.ParseFloat(t, 64); err == nil && v > 0 {
			threshold = v
		}
	}

	reg := prometheus.NewRegistry()
	wattsGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sump_pump_watts",
		Help: "Current power draw of the sump pump in watts.",
	})
	runningGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sump_pump_running",
		Help: "1 if the sump pump is currently running, 0 if idle.",
	})
	runsTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "sump_pump_runs_total",
		Help: "Total number of sump pump run cycles since last restart.",
	})
	requestsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total HTTP requests by method, path, and status code.",
	}, []string{"method", "path", "status_code"})
	requestDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request latency in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})
	reg.MustRegister(wattsGauge, runningGauge, runsTotal, requestsTotal, requestDuration)

	nc, err := nats.Connect(natsURL)
	if err != nil {
		log.Fatalf("nats connect %s: %v", natsURL, err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		log.Fatalf("jetstream init: %v", err)
	}

	a := &app{
		js:              js,
		threshold:       threshold,
		wattsGauge:      wattsGauge,
		runningGauge:    runningGauge,
		runsTotal:       runsTotal,
		requestsTotal:   requestsTotal,
		requestDuration: requestDuration,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/webhook", a.webhookHandler)
	mux.HandleFunc("/", notFoundHandler)

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	metricsSrv := &http.Server{
		Addr:              ":" + metricsPort,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		log.Printf("metrics listening on :%s", metricsPort)
		if err := metricsSrv.ListenAndServe(); err != nil {
			log.Fatal(err)
		}
	}()

	log.Printf("sump-pump-bridge %s listening on :%s (threshold=%.0fW)", version, port, threshold)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           a.metricsMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
