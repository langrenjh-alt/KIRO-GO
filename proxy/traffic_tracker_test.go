package proxy

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTrafficTrackerSnapshotOrdersAndZeroFillsBuckets(t *testing.T) {
	current := time.Date(2026, 7, 11, 10, 0, 15, 0, time.FixedZone("CST", 8*60*60))
	tracker := newTrafficTracker(10*time.Minute, func() time.Time { return current })

	tracker.Record(TrafficObservation{
		InputTokens:         100,
		OutputTokens:        20,
		CacheCreationTokens: 30,
		CacheReadTokens:     40,
	})
	current = current.Add(2 * time.Minute)
	tracker.Record(TrafficObservation{
		InputTokens:         50,
		OutputTokens:        -1,
		CacheCreationTokens: 10,
		CacheReadTokens:     70,
	})

	start := time.Date(2026, 7, 11, 2, 0, 1, 0, time.UTC)
	end := time.Date(2026, 7, 11, 2, 2, 30, 0, time.UTC)
	got := tracker.Snapshot(start, end)

	if len(got.Buckets) != 3 {
		t.Fatalf("bucket count = %d, want 3", len(got.Buckets))
	}
	wantTimes := []int64{
		time.Date(2026, 7, 11, 2, 0, 0, 0, time.UTC).Unix(),
		time.Date(2026, 7, 11, 2, 1, 0, 0, time.UTC).Unix(),
		time.Date(2026, 7, 11, 2, 2, 0, 0, time.UTC).Unix(),
	}
	for i, want := range wantTimes {
		if got.Buckets[i].Timestamp != want {
			t.Fatalf("bucket[%d].timestamp = %d, want %d", i, got.Buckets[i].Timestamp, want)
		}
	}
	if got.Buckets[1].TrafficCounters != (TrafficCounters{}) {
		t.Fatalf("middle bucket was not zero-filled: %+v", got.Buckets[1])
	}
	if got.Buckets[2].OutputTokens != 0 {
		t.Fatalf("negative output tokens were recorded: %d", got.Buckets[2].OutputTokens)
	}
	wantTotals := TrafficCounters{
		Requests:            2,
		InputTokens:         150,
		OutputTokens:        20,
		CacheCreationTokens: 40,
		CacheReadTokens:     110,
	}
	if got.Totals != wantTotals {
		t.Fatalf("totals = %+v, want %+v", got.Totals, wantTotals)
	}
}

func TestTrafficTrackerRingEvictsOldBucketsAndCapsQuery(t *testing.T) {
	current := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	tracker := newTrafficTracker(3*time.Minute, func() time.Time { return current })

	for i := int64(1); i <= 4; i++ {
		tracker.Record(TrafficObservation{InputTokens: i})
		current = current.Add(time.Minute)
	}

	got := tracker.Snapshot(
		time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 11, 0, 4, 0, 0, time.UTC),
	)
	if len(got.Buckets) != 3 {
		t.Fatalf("bucket count = %d, want retention-capped 3", len(got.Buckets))
	}
	if got.StartTime != time.Date(2026, 7, 11, 0, 1, 0, 0, time.UTC).Unix() {
		t.Fatalf("startTime = %d, want newest three-minute window", got.StartTime)
	}
	for i, want := range []uint64{2, 3, 4} {
		if got.Buckets[i].InputTokens != want {
			t.Fatalf("bucket[%d].inputTokens = %d, want %d", i, got.Buckets[i].InputTokens, want)
		}
	}
	if got.Totals.Requests != 3 || got.Totals.InputTokens != 9 {
		t.Fatalf("totals = %+v, want requests=3 input=9", got.Totals)
	}
}

func TestTrafficTrackerQueryRoundsWindowAndIncludesCurrentMinute(t *testing.T) {
	current := time.Date(2026, 7, 11, 12, 34, 45, 0, time.UTC)
	tracker := newTrafficTracker(5*time.Minute, func() time.Time { return current })
	tracker.Record(TrafficObservation{OutputTokens: 9})

	got := tracker.Query(90 * time.Second)
	if len(got.Buckets) != 2 {
		t.Fatalf("bucket count = %d, want 2", len(got.Buckets))
	}
	last := got.Buckets[len(got.Buckets)-1]
	if last.Timestamp != current.Truncate(time.Minute).Unix() || last.OutputTokens != 9 {
		t.Fatalf("current bucket = %+v", last)
	}
	if got.EndTime != current.Truncate(time.Minute).Add(time.Minute).Unix() {
		t.Fatalf("endTime = %d, want end of current minute", got.EndTime)
	}
}

func TestTrafficTrackerRecordIsConcurrentSafe(t *testing.T) {
	current := time.Date(2026, 7, 11, 1, 2, 3, 0, time.UTC)
	tracker := newTrafficTracker(time.Hour, func() time.Time { return current })

	const goroutines = 20
	const observationsPerGoroutine = 500
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < observationsPerGoroutine; j++ {
				tracker.Record(TrafficObservation{
					InputTokens:         2,
					OutputTokens:        3,
					CacheCreationTokens: 5,
					CacheReadTokens:     7,
				})
			}
		}()
	}
	wg.Wait()

	got := tracker.Query(time.Minute).Totals
	wantRequests := uint64(goroutines * observationsPerGoroutine)
	if got.Requests != wantRequests ||
		got.InputTokens != wantRequests*2 ||
		got.OutputTokens != wantRequests*3 ||
		got.CacheCreationTokens != wantRequests*5 ||
		got.CacheReadTokens != wantRequests*7 {
		t.Fatalf("concurrent totals = %+v, want requests=%d", got, wantRequests)
	}
}

func TestTrafficSnapshotJSONShape(t *testing.T) {
	current := time.Date(2026, 7, 11, 1, 2, 3, 0, time.UTC)
	tracker := newTrafficTracker(time.Minute, func() time.Time { return current })
	tracker.Record(TrafficObservation{InputTokens: 1})

	encoded, err := json.Marshal(tracker.Query(time.Minute))
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	text := string(encoded)
	for _, field := range []string{
		`"startTime"`, `"endTime"`, `"bucketSeconds"`, `"totals"`, `"buckets"`,
		`"timestamp"`, `"requests"`, `"inputTokens"`, `"outputTokens"`,
		`"cacheCreationTokens"`, `"cacheReadTokens"`,
	} {
		if !strings.Contains(text, field) {
			t.Fatalf("snapshot JSON missing %s: %s", field, text)
		}
	}
}

func TestTrafficTrackerRecordsFailuresAndResetClearsAllBuckets(t *testing.T) {
	current := time.Date(2026, 7, 11, 1, 2, 3, 0, time.UTC)
	tracker := newTrafficTracker(time.Hour, func() time.Time { return current })

	// A failed request still counts as a request; its unavailable token fields stay zero.
	tracker.Record(TrafficObservation{})
	tracker.Record(TrafficObservation{InputTokens: 10, OutputTokens: 2})
	before := tracker.Query(time.Minute).Totals
	if before.Requests != 2 || before.InputTokens != 10 || before.OutputTokens != 2 {
		t.Fatalf("totals before reset = %+v", before)
	}

	tracker.Reset()
	after := tracker.Query(time.Minute)
	if after.Totals != (TrafficCounters{}) {
		t.Fatalf("totals after reset = %+v, want zero", after.Totals)
	}
	if len(after.Buckets) != 1 || after.Buckets[0].TrafficCounters != (TrafficCounters{}) {
		t.Fatalf("query after reset should remain zero-filled: %+v", after.Buckets)
	}
}

func TestNewTrafficTrackerNormalizesRetention(t *testing.T) {
	if got := newTrafficTracker(90*time.Second, time.Now).Retention(); got != 2*time.Minute {
		t.Fatalf("rounded retention = %v, want 2m", got)
	}
	if got := newTrafficTracker(MaxTrafficRetention+time.Hour, time.Now).Retention(); got != MaxTrafficRetention {
		t.Fatalf("capped retention = %v, want %v", got, MaxTrafficRetention)
	}
}
