package proxy

import (
	"math"
	"sync"
	"time"
)

const (
	// TrafficBucketSize is the resolution exposed by TrafficTracker.
	TrafficBucketSize = time.Minute
	// DefaultTrafficRetention balances dashboard history with a small memory footprint.
	DefaultTrafficRetention = 7 * 24 * time.Hour
	// MaxTrafficRetention prevents accidental oversized allocations.
	MaxTrafficRetention = 31 * 24 * time.Hour
)

// TrafficObservation is one completed request. Negative token values are treated as zero.
type TrafficObservation struct {
	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
}

// TrafficCounters contains the values shared by a bucket and its aggregate total.
type TrafficCounters struct {
	Requests            uint64 `json:"requests"`
	InputTokens         uint64 `json:"inputTokens"`
	OutputTokens        uint64 `json:"outputTokens"`
	CacheCreationTokens uint64 `json:"cacheCreationTokens"`
	CacheReadTokens     uint64 `json:"cacheReadTokens"`
}

// TrafficBucket represents one UTC minute. Timestamp is the bucket's Unix start time.
type TrafficBucket struct {
	Timestamp int64 `json:"timestamp"`
	TrafficCounters
}

// TrafficSnapshot is ready for direct Admin API JSON serialization. EndTime is exclusive.
type TrafficSnapshot struct {
	StartTime     int64           `json:"startTime"`
	EndTime       int64           `json:"endTime"`
	BucketSeconds int64           `json:"bucketSeconds"`
	Totals        TrafficCounters `json:"totals"`
	Buckets       []TrafficBucket `json:"buckets"`
}

type trafficSlot struct {
	occupied bool
	minute   int64
	counters TrafficCounters
}

// TrafficTracker keeps a bounded, process-local ring of minute buckets.
type TrafficTracker struct {
	mu      sync.RWMutex
	slots   []trafficSlot
	nowFunc func() time.Time
}

// NewTrafficTracker creates a tracker with the requested retention. Non-positive values use
// DefaultTrafficRetention; values above MaxTrafficRetention are capped.
func NewTrafficTracker(retention time.Duration) *TrafficTracker {
	return newTrafficTracker(retention, time.Now)
}

func newTrafficTracker(retention time.Duration, nowFunc func() time.Time) *TrafficTracker {
	if retention <= 0 {
		retention = DefaultTrafficRetention
	}
	if retention > MaxTrafficRetention {
		retention = MaxTrafficRetention
	}
	bucketCount := int((retention + TrafficBucketSize - 1) / TrafficBucketSize)
	if bucketCount < 1 {
		bucketCount = 1
	}
	if nowFunc == nil {
		nowFunc = time.Now
	}
	return &TrafficTracker{
		slots:   make([]trafficSlot, bucketCount),
		nowFunc: nowFunc,
	}
}

// Retention returns the effective in-memory retention window.
func (t *TrafficTracker) Retention() time.Duration {
	if t == nil {
		return 0
	}
	return time.Duration(len(t.slots)) * TrafficBucketSize
}

// Record adds one completed request to the current minute bucket.
func (t *TrafficTracker) Record(observation TrafficObservation) {
	if t == nil || len(t.slots) == 0 {
		return
	}
	minute := t.nowFunc().UTC().Truncate(TrafficBucketSize).Unix() / int64(time.Minute/time.Second)
	index := ringIndex(minute, len(t.slots))

	t.mu.Lock()
	slot := &t.slots[index]
	if !slot.occupied || slot.minute != minute {
		*slot = trafficSlot{occupied: true, minute: minute}
	}
	addSaturating(&slot.counters.Requests, 1)
	addSaturating(&slot.counters.InputTokens, nonNegativeUint64(observation.InputTokens))
	addSaturating(&slot.counters.OutputTokens, nonNegativeUint64(observation.OutputTokens))
	addSaturating(&slot.counters.CacheCreationTokens, nonNegativeUint64(observation.CacheCreationTokens))
	addSaturating(&slot.counters.CacheReadTokens, nonNegativeUint64(observation.CacheReadTokens))
	t.mu.Unlock()
}

// Query returns zero-filled buckets ending with the current minute. The requested window is
// rounded up to whole minutes and capped to the tracker's retention.
func (t *TrafficTracker) Query(window time.Duration) TrafficSnapshot {
	if t == nil || len(t.slots) == 0 || window <= 0 {
		return emptyTrafficSnapshot()
	}
	bucketCount := int((window + TrafficBucketSize - 1) / TrafficBucketSize)
	if bucketCount > len(t.slots) {
		bucketCount = len(t.slots)
	}
	end := t.nowFunc().UTC().Truncate(TrafficBucketSize).Add(TrafficBucketSize)
	start := end.Add(-time.Duration(bucketCount) * TrafficBucketSize)
	return t.Snapshot(start, end)
}

// Reset atomically clears all retained traffic buckets.
func (t *TrafficTracker) Reset() {
	if t == nil || len(t.slots) == 0 {
		return
	}
	t.mu.Lock()
	clear(t.slots)
	t.mu.Unlock()
}

// Snapshot returns zero-filled, ordered minute buckets overlapping [start, end). Start is
// rounded down and end is rounded up. Requests wider than retention keep the newest buckets.
func (t *TrafficTracker) Snapshot(start, end time.Time) TrafficSnapshot {
	if t == nil || len(t.slots) == 0 {
		return emptyTrafficSnapshot()
	}
	start, end, bucketCount := t.normalizeRange(start, end)
	result := TrafficSnapshot{
		StartTime:     start.Unix(),
		EndTime:       end.Unix(),
		BucketSeconds: int64(TrafficBucketSize / time.Second),
		Buckets:       make([]TrafficBucket, 0, bucketCount),
	}
	if bucketCount == 0 {
		return result
	}

	startMinute := start.Unix() / int64(time.Minute/time.Second)
	t.mu.RLock()
	for offset := 0; offset < bucketCount; offset++ {
		minute := startMinute + int64(offset)
		bucket := TrafficBucket{Timestamp: minute * int64(time.Minute/time.Second)}
		slot := t.slots[ringIndex(minute, len(t.slots))]
		if slot.occupied && slot.minute == minute {
			bucket.TrafficCounters = slot.counters
		}
		result.Totals.add(bucket.TrafficCounters)
		result.Buckets = append(result.Buckets, bucket)
	}
	t.mu.RUnlock()
	return result
}

func (t *TrafficTracker) normalizeRange(start, end time.Time) (time.Time, time.Time, int) {
	start = start.UTC().Truncate(TrafficBucketSize)
	end = end.UTC()
	endFloor := end.Truncate(TrafficBucketSize)
	if end.After(endFloor) {
		end = endFloor.Add(TrafficBucketSize)
	} else {
		end = endFloor
	}
	if !end.After(start) {
		return start, start, 0
	}

	bucketCount := int(end.Sub(start) / TrafficBucketSize)
	if bucketCount > len(t.slots) {
		bucketCount = len(t.slots)
		start = end.Add(-time.Duration(bucketCount) * TrafficBucketSize)
	}
	return start, end, bucketCount
}

func (c *TrafficCounters) add(other TrafficCounters) {
	addSaturating(&c.Requests, other.Requests)
	addSaturating(&c.InputTokens, other.InputTokens)
	addSaturating(&c.OutputTokens, other.OutputTokens)
	addSaturating(&c.CacheCreationTokens, other.CacheCreationTokens)
	addSaturating(&c.CacheReadTokens, other.CacheReadTokens)
}

func addSaturating(value *uint64, delta uint64) {
	if math.MaxUint64-*value < delta {
		*value = math.MaxUint64
		return
	}
	*value += delta
}

func nonNegativeUint64(value int64) uint64 {
	if value <= 0 {
		return 0
	}
	return uint64(value)
}

func ringIndex(minute int64, size int) int {
	index := minute % int64(size)
	if index < 0 {
		index += int64(size)
	}
	return int(index)
}

func emptyTrafficSnapshot() TrafficSnapshot {
	return TrafficSnapshot{
		BucketSeconds: int64(TrafficBucketSize / time.Second),
		Buckets:       []TrafficBucket{},
	}
}
