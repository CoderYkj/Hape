package httpx

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Metrics is a dependency-free counter registry rendered in Prometheus text
// format. It is deliberately small: swap it for real OpenTelemetry when the
// team outgrows one process.
type Metrics struct {
	mu      sync.Mutex
	service string
	started time.Time
	counts  map[string]int64
}

type metricKey struct {
	op     string
	status int
}

// NewMetrics creates a registry for one service.
func NewMetrics(service string) *Metrics {
	return &Metrics{service: service, started: time.Now(), counts: map[string]int64{}}
}

// Record counts one finished request.
func (m *Metrics) Record(op string, status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[op+"/"+itoa(status)]++
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
		started := m.started.Unix()
		m.mu.Unlock()

		var b strings.Builder
		b.WriteString("# HELP backend_requests_total Requests served, by operation and status.\n")
		b.WriteString("# TYPE backend_requests_total counter\n")
		for _, k := range keys {
			op, status, _ := strings.Cut(k, "/")
			fmt.Fprintf(&b, "backend_requests_total{service=%q,op=%q,status=%q} %d\n", m.service, op, status, counts[k])
		}
		b.WriteString("# HELP backend_start_time_seconds Unix start time of the process.\n")
		b.WriteString("# TYPE backend_start_time_seconds gauge\n")
		fmt.Fprintf(&b, "backend_start_time_seconds{service=%q} %d\n", m.service, started)

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
