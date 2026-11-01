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
}

type metricKey struct {
	op     string
	status int
}

// NewMetrics creates a registry for one service.
func NewMetrics(service string) *Metrics {
	return &Metrics{
		service:  service,
		started:  time.Now(),
		counts:   map[string]int64{},
		counters: map[string]int64{},
		gauges:   map[string]float64{},
	}
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
