package proxy

import (
	"strings"
	"testing"
)

func TestEstimateApproxTokensDoesNotCopyLargeInput(t *testing.T) {
	input := strings.Repeat("ascii token estimation payload ", 1<<15)
	want := estimateApproxTokens(input)
	var got int
	allocations := testing.AllocsPerRun(10, func() {
		got = estimateApproxTokens(input)
	})
	if got != want {
		t.Fatalf("token estimate changed across runs: got %d want %d", got, want)
	}
	if allocations != 0 {
		t.Fatalf("large token estimation allocated %.1f objects per call", allocations)
	}
}

func TestTrailingRunesAvoidsFullStringCopy(t *testing.T) {
	input := strings.Repeat("前缀abc", 1<<14) + "结尾XYZ"
	if got := trailingRunes(input, 5); got != "结尾XYZ" {
		t.Fatalf("trailingRunes=%q, want %q", got, "结尾XYZ")
	}
	var got string
	allocations := testing.AllocsPerRun(10, func() {
		got = trailingRunes(input, 5)
	})
	if got != "结尾XYZ" || allocations != 0 {
		t.Fatalf("trailingRunes result=%q allocations=%.1f", got, allocations)
	}
}
