package egress

import (
	"math"
	"sync"
	"time"

	"kiro-go/config"
)

// ProxyStats reports live application payload bytes, not TCP/TLS overhead.
// TotalBytes is this process's observed total; persisted totals are held in
// config. Use PendingTraffic alongside the persisted total for the list API.
type ProxyStats struct {
	CurrentConcurrency    int64 `json:"currentConcurrency"`
	CurrentBytesPerSecond int64 `json:"currentBytesPerSecond"`
	TotalBytes            int64 `json:"totalBytes"`
}

type byteBucket struct{ second, bytes int64 }
type proxyMetrics struct {
	mu          sync.Mutex
	concurrency int64
	total       int64
	pending     int64
	buckets     [5]byteBucket
}

var proxyMetricsStore sync.Map
var flushMu sync.Mutex

func metricsFor(id string) *proxyMetrics {
	if id == "" {
		return nil
	}
	value, _ := proxyMetricsStore.LoadOrStore(id, &proxyMetrics{})
	return value.(*proxyMetrics)
}

func (m *proxyMetrics) begin() { m.mu.Lock(); m.concurrency++; m.mu.Unlock() }
func (m *proxyMetrics) end()   { m.mu.Lock(); m.concurrency--; m.mu.Unlock() }
func (m *proxyMetrics) add(n int64) {
	if n <= 0 {
		return
	}
	second := time.Now().Unix()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.total += n
	m.pending += n
	bucket := &m.buckets[second%int64(len(m.buckets))]
	if bucket.second != second {
		*bucket = byteBucket{second: second}
	}
	bucket.bytes += n
}

func Stats(id string) ProxyStats {
	value, ok := proxyMetricsStore.Load(id)
	if !ok {
		return ProxyStats{}
	}
	m := value.(*proxyMetrics)
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot := ProxyStats{CurrentConcurrency: m.concurrency, TotalBytes: m.total}
	second := time.Now().Unix()
	for _, bucket := range m.buckets {
		if age := second - bucket.second; age >= 0 && age < 5 {
			snapshot.CurrentBytesPerSecond += bucket.bytes
		}
	}
	snapshot.CurrentBytesPerSecond /= 5
	return snapshot
}

func PendingTraffic(id string) int64 {
	value, ok := proxyMetricsStore.Load(id)
	if !ok {
		return 0
	}
	m := value.(*proxyMetrics)
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pending
}

// ProxyEntriesWithTraffic reads durable and unflushed bytes across a single
// flush barrier so list responses never lose or duplicate an in-flight delta.
func ProxyEntriesWithTraffic() []config.ProxyEntry {
	flushMu.Lock()
	defer flushMu.Unlock()
	entries := config.GetProxies()
	for i := range entries {
		pending := PendingTraffic(entries[i].ID)
		if entries[i].TotalBytes > math.MaxInt64-pending {
			entries[i].TotalBytes = math.MaxInt64
		} else {
			entries[i].TotalBytes += pending
		}
	}
	return entries
}

// FlushTraffic persists a coalesced delta. Failed saves keep the entire delta
// pending so the next flush retries it without losing or duplicating bytes.
func FlushTraffic() error {
	flushMu.Lock()
	defer flushMu.Unlock()
	deltas := make(map[string]int64)
	proxyMetricsStore.Range(func(key, value any) bool {
		m := value.(*proxyMetrics)
		m.mu.Lock()
		if m.pending > 0 {
			deltas[key.(string)] = m.pending
		}
		m.mu.Unlock()
		return true
	})
	if len(deltas) == 0 {
		return nil
	}
	if err := config.AddProxyTraffic(deltas); err != nil {
		return err
	}
	for id, n := range deltas {
		m := metricsFor(id)
		m.mu.Lock()
		m.pending -= n
		m.mu.Unlock()
	}
	return nil
}
