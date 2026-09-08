package policy

import (
	"errors"
	"testing"

	"hoorific/internal/core"
)

func TestEstimateUsageCostUsesInclusiveCacheDetailsAndTTLRates(t *testing.T) {
	input, output := int64(1_000_000), int64(1_000_000)
	cached, writes := int64(200_000), int64(300_000)
	write5m, write1h := int64(100_000), int64(100_000)
	usage := &core.Usage{
		Input:             &input,
		Output:            &output,
		CachedInput:       &cached,
		CacheWriteInput:   &writes,
		CacheWrite5mInput: &write5m,
		CacheWrite1hInput: &write1h,
	}
	price := &core.PriceSchedule{
		InputPerMillion:           new(int64(10)),
		OutputPerMillion:          new(int64(20)),
		CachedInputPerMillion:     new(int64(2)),
		CacheWriteInputPerMillion: new(int64(8)),
		CacheWrite5mPerMillion:    new(int64(3)),
		CacheWrite1hPerMillion:    new(int64(4)),
	}
	got, err := EstimateUsageCost(usage, price)
	if err != nil {
		t.Fatal(err)
	}
	// Ordinary 500k at 10, cached 200k at 2, writes 100k/100k/100k
	// at 3/4/8, and output 1m at 20: 26.9 nanodollars, rounded up.
	if got != 27 {
		t.Fatalf("cache-aware cost=%d, want 27", got)
	}
}

func TestEstimateUsageCostDoesNotGuessMissingCacheCounters(t *testing.T) {
	input := int64(1_000_000)
	output := int64(0)
	usage := &core.Usage{Input: &input, Output: &output}
	price := &core.PriceSchedule{
		InputPerMillion:       new(int64(10)),
		CachedInputPerMillion: new(int64(2)),
	}
	_, err := EstimateUsageCost(usage, price)
	if !errors.Is(err, ErrUnknownCost) {
		t.Fatalf("missing cache counter error=%v, want ErrUnknownCost", err)
	}
}

func TestEstimateUsageCostKeepsLegacyBaseOnlyPricing(t *testing.T) {
	input, output := int64(1_000_000), int64(0)
	got, err := EstimateUsageCost(&core.Usage{Input: &input, Output: &output}, &core.PriceSchedule{InputPerMillion: new(int64(10))})
	if err != nil {
		t.Fatal(err)
	}
	if got != 10 {
		t.Fatalf("legacy base-only cost=%d, want 10", got)
	}
}

func TestEstimateBoundUsesHighestConfiguredInputRate(t *testing.T) {
	got, err := EstimateBound(1_000_000, 1_000_000, &core.PriceSchedule{
		InputPerMillion:        new(int64(5)),
		OutputPerMillion:       new(int64(7)),
		CachedInputPerMillion:  new(int64(20)),
		CacheWrite5mPerMillion: new(int64(30)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != 37 {
		t.Fatalf("conservative bound=%d, want 37", got)
	}
}

func TestEstimateCostRejectsInt64Overflow(t *testing.T) {
	if _, err := EstimateCost(int64(^uint64(0)>>1), 0, int64(^uint64(0)>>1), 0, 1); err == nil {
		t.Fatal("overflowing cost was accepted")
	}
}
