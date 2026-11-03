package httpx

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Metrics is a dependency-free metric registry rendered in Prometheus text format.
// It is deliberately small: swap it for real OpenTelemetry when the team outgrows
// one process.
type Metrics struct {
	mu      sync.Mutex
	service string
	started time.Time
	counts  map[string]int64
	// counters and gauges are domain metrics published next to the request counters.
	// They exist so an operator can alert on the things that cost money, not only on
	// request latency.
	counters map[string]int64
	gauges   map[string]float64
	// histograms hold latency observations per key (op name). Buckets are fixed: the request
	// path is known, and a per-op dynamic bucket set would be a configurability problem
	// nobody asked for.
	histograms map[string]*histogram
}

// latencyBuckets are the upper bounds (seconds) of the request duration histogram. They
// bracket what a local backend actually does (single-digit ms) and still catch the tail
// (a second or more) that a p99 SLO is about.
var latencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

type histogram struct {
	counts []int64 // one per bucket, plus a final +Inf slot
	sum    float64
	count  uint64
}

type metricKey struct {
	op     string
	status int
}

// NewMetrics creates a registry for one service.
func NewMetrics(service string) *Metrics {
	return &Metrics{
		service:    service,
		started:    time.Now(),
		counts:     map[string]int64{},
		counters:   map[string]int64{},
		gauges:     map[string]float64{},
		histograms: map[string]*histogram{},
	}
}

// Observe records one latency observation (seconds) under key.
func (m *Metrics) Observe(key string, seconds float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.histograms[key]
	if !ok {
		h = &histogram{counts: make([]int64, len(latencyBuckets)+1)}
		m.histograms[key] = h
	}
	index := len(latencyBuckets) // +Inf slot by default
	for i, bound := range latencyBuckets {
		if seconds <= bound {
			index = i
			break
		}
	}
	h.counts[index]++
	h.sum += seconds
	h.count++
}

// Record counts one finished request.
func (m *Metrics) Record(op string, status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[op+"/"+itoa(status)]++
}

// AddCounter adds delta to a domain counter, rendered as backend_<name>_total.
//
// name must be snake_case without a suffix: the exporter adds the prefix and the unit
// suffix, so the same name is never spelled two ways.
func (m *Metrics) AddCounter(name string, delta int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// A zero-delta call still registers the series. Dashboards and rate() queries are
	// easier to write when a counter exists from the first sweep instead of appearing
	// some time later.
	m.counters[name] += delta
}

// SetGauge sets a domain gauge, rendered as backend_<name>.
//
// A gauge is a snapshot, not a delta: the caller publishes the current state of the
// world, and a later tick overwrites it.
func (m *Metrics) SetGauge(name string, value float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gauges[name] = value
}

// Handler serves the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		m.mu.Lock()
		keys := make([]string, 0, len(m.counts))
		for k := range m.counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		counts := make(map[string]int64, len(m.counts))
		for k, v := range m.counts {
			counts[k] = v
		}
		counterNames := make([]string, 0, len(m.counters))
		for k := range m.counters {
			counterNames = append(counterNames, k)
		}
		sort.Strings(counterNames)
		counters := make(map[string]int64, len(m.counters))
		for k, v := range m.counters {
			counters[k] = v
		}
		gaugeNames := make([]string, 0, len(m.gauges))
		for k := range m.gauges {
			gaugeNames = append(gaugeNames, k)
		}
		sort.Strings(gaugeNames)
		gauges := make(map[string]float64, len(m.gauges))
		for k, v := range m.gauges {
			gauges[k] = v
		}
		histogramKeys := make([]string, 0, len(m.histograms))
		for k := range m.histograms {
			histogramKeys = append(histogramKeys, k)
		}
		sort.Strings(histogramKeys)
		histograms := make(map[string]histogram, len(m.histograms))
		for k, v := range m.histograms {
			histograms[k] = histogram{
				counts: append([]int64(nil), v.counts...),
				sum:    v.sum,
				count:  v.count,
			}
		}
		started := m.started.Unix()
		service := m.service
		m.mu.Unlock()

		var b strings.Builder
		b.WriteString("# HELP backend_requests_total Requests served, by operation and status.\n")
		b.WriteString("# TYPE backend_requests_total counter\n")
		for _, k := range keys {
			op, status, _ := strings.Cut(k, "/")
			fmt.Fprintf(&b, "backend_requests_total{service=%q,op=%q,status=%q} %d\n", service, op, status, counts[k])
		}

		for _, name := range counterNames {
			fmt.Fprintf(&b, "# TYPE backend_%s_total counter\n", name)
			fmt.Fprintf(&b, "backend_%s_total{service=%q} %d\n", name, service, counters[name])
		}
		for _, name := range gaugeNames {
			fmt.Fprintf(&b, "# TYPE backend_%s gauge\n", name)
			fmt.Fprintf(&b, "backend_%s{service=%q} %s\n", name, service, formatFloat(gauges[name]))
		}

		// Latency histograms: `histogram_quantile(0.99, sum(rate(..._bucket[5m])) by (le))` is
		// the query the SLO in docs/slo.md is written against.
		for _, key := range histogramKeys {
			h := histograms[key]
			fmt.Fprintf(&b, "# TYPE backend_request_duration_seconds histogram\n")
			cumulative := int64(0)
			for i, bound := range latencyBuckets {
				cumulative += h.counts[i]
				fmt.Fprintf(&b, "backend_request_duration_seconds_bucket{service=%q,op=%q,le=%q} %d\n",
					service, key, formatFloat(bound), cumulative)
			}
			cumulative += h.counts[len(latencyBuckets)]
			fmt.Fprintf(&b, "backend_request_duration_seconds_bucket{service=%q,op=%q,le=\"+Inf\"} %d\n",
				service, key, cumulative)
			fmt.Fprintf(&b, "backend_request_duration_seconds_sum{service=%q,op=%q} %s\n",
				service, key, formatFloat(h.sum))
			fmt.Fprintf(&b, "backend_request_duration_seconds_count{service=%q,op=%q} %d\n",
				service, key, h.count)
		}

		b.WriteString("# HELP backend_start_time_seconds Unix start time of the process.\n")
		b.WriteString("# TYPE backend_start_time_seconds gauge\n")
		fmt.Fprintf(&b, "backend_start_time_seconds{service=%q} %d\n", service, started)

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
	})
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// formatFloat renders a gauge without a trailing ".000000": Prometheus accepts either,
// but a human reading curl output does not want the noise.
func formatFloat(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}
