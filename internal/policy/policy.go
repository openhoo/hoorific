// Package policy provides stateless validation, never admission or accounting.
package policy

import (
	"errors"
	"fmt"
	"hoorific/internal/core"
	"math/big"
)

var ErrUnknownCost = errors.New("cost is unknown")

const maxInt64 = int64(^uint64(0) >> 1)

func CheckedAdd(a, b int64) (int64, error) {
	if a < 0 || b < 0 || a > maxInt64-b {
		return 0, fmt.Errorf("integer addition overflow")
	}
	return a + b, nil
}

func ValidateAllowance(a core.Allowance) error {
	if a.ScopeKind == "" || a.ScopeID == "" || a.WindowID == "" || a.Kind == "" {
		return fmt.Errorf("allowance scope, window and kind are required")
	}
	if a.Maximum < 0 || a.Reserve < 0 || a.Reserve > a.Maximum {
		return fmt.Errorf("allowance requires 0 <= reserve <= maximum")
	}
	return nil
}

// ValidateAttempt checks representation only. The admission store must still
// authenticate revisions, resolve authoritative limits and reserve atomically.
func ValidateAttempt(p core.AttemptPlan) error {
	if p.TenantID == "" || p.RequestID == "" || p.AttemptID == "" || p.KeyID == "" || p.ConnectionID == "" {
		return fmt.Errorf("attempt identity is incomplete")
	}
	if p.KeyRevision < 0 || p.ConfigRevision < 0 {
		return fmt.Errorf("negative revision")
	}
	if p.MaximumCost != nil && *p.MaximumCost < 0 {
		return fmt.Errorf("negative maximum cost")
	}
	if p.Deadline.IsZero() {
		return fmt.Errorf("attempt deadline required")
	}
	seen := make(map[[4]string]bool)
	for _, a := range p.Allowances {
		if err := ValidateAllowance(a); err != nil {
			return err
		}
		key := [4]string{a.ScopeKind, a.ScopeID, a.WindowID, a.Kind}
		if seen[key] {
			return fmt.Errorf("duplicate allowance")
		}
		seen[key] = true
	}
	return nil
}

// ValidateTokens rejects negative values and checks known bounds. A nil limit is
// unknown, not an invented provider/model-specific default.
func ValidateTokens(input, output int64, contextLimit, outputLimit *int64) error {
	if input < 0 || output < 0 {
		return fmt.Errorf("negative token count")
	}
	if contextLimit != nil {
		if *contextLimit < 0 || input > *contextLimit || output > *contextLimit-input {
			return fmt.Errorf("context token limit exceeded")
		}
	}
	if outputLimit != nil && (*outputLimit < 0 || output > *outputLimit) {
		return fmt.Errorf("output token limit exceeded")
	}
	return nil
}

type costTerm struct {
	units int64
	rate  int64
}

func estimateTerms(terms []costTerm, unit int64) (int64, error) {
	if unit <= 0 {
		return 0, fmt.Errorf("invalid cost unit")
	}
	total := new(big.Int)
	for _, term := range terms {
		if term.units < 0 || term.rate < 0 {
			return 0, fmt.Errorf("invalid cost operands")
		}
		if term.units == 0 || term.rate == 0 {
			continue
		}
		total.Add(total, new(big.Int).Mul(big.NewInt(term.units), big.NewInt(term.rate)))
	}
	total.Add(total, big.NewInt(unit-1))
	total.Quo(total, big.NewInt(unit))
	if !total.IsInt64() {
		return 0, fmt.Errorf("cost exceeds int64")
	}
	return total.Int64(), nil
}

// EstimateCost returns ceil((input*inputRate + output*outputRate)/unit).
// Rates and result are integer USD nanodollars; rates are nanodollars per
// million tokens when unit is 1,000,000. big.Int prevents intermediate
// multiplication overflow, and a result outside int64 is rejected.
func EstimateCost(input, output, inputRate, outputRate, unit int64) (int64, error) {
	if input < 0 || output < 0 || inputRate < 0 || outputRate < 0 || unit <= 0 {
		return 0, fmt.Errorf("invalid cost operands")
	}
	return estimateTerms([]costTerm{{input, inputRate}, {output, outputRate}}, unit)
}

func priceRate(price *int64) (int64, bool, error) {
	if price == nil {
		return 0, false, nil
	}
	if *price < 0 {
		return 0, false, fmt.Errorf("negative price rate")
	}
	return *price, true, nil
}

// EstimateBound computes a conservative token cost bound. Input tokens are
// inclusive of ordinary, cache-read and cache-write tokens, so the highest
// configured input rate is used for every input token. Nil required rates
// produce ErrUnknownCost rather than a fabricated zero or base-rate estimate.
func EstimateBound(input, output int64, price *core.PriceSchedule) (int64, error) {
	if input < 0 || output < 0 {
		return 0, fmt.Errorf("invalid cost operands")
	}
	if price == nil {
		return 0, ErrUnknownCost
	}
	var inputRate int64
	haveInputRate := false
	for _, candidate := range []*int64{
		price.InputPerMillion,
		price.CachedInputPerMillion,
		price.CacheWriteInputPerMillion,
		price.CacheWrite5mPerMillion,
		price.CacheWrite1hPerMillion,
	} {
		rate, present, err := priceRate(candidate)
		if err != nil {
			return 0, err
		}
		if present && (!haveInputRate || rate > inputRate) {
			inputRate, haveInputRate = rate, true
		}
	}
	if input > 0 && !haveInputRate {
		return 0, ErrUnknownCost
	}
	outputRate, haveOutputRate, err := priceRate(price.OutputPerMillion)
	if err != nil {
		return 0, err
	}
	if output > 0 && !haveOutputRate {
		return 0, ErrUnknownCost
	}
	return estimateTerms([]costTerm{{input, inputRate}, {output, outputRate}}, 1000000)
}

// EstimateUsageCost applies the configured ordinary, cache-read and
// cache-write rates to provider-reported inclusive usage. Cache detail fields
// are subsets and are never added to Input a second time. A nonzero category
// with no applicable rate returns ErrUnknownCost; callers must retain a
// reconcilable hold rather than charging at a guessed rate.
func EstimateUsageCost(usage *core.Usage, price *core.PriceSchedule) (int64, error) {
	if usage == nil || usage.Input == nil || usage.Output == nil {
		return 0, ErrUnknownCost
	}
	for _, value := range []*int64{
		usage.Input, usage.Output, usage.CachedInput, usage.CacheWriteInput,
		usage.CacheWrite5mInput, usage.CacheWrite1hInput,
		usage.ReasoningOutput, usage.ToolInput,
	} {
		if value != nil && *value < 0 {
			return 0, fmt.Errorf("negative usage")
		}
	}
	input := *usage.Input
	output := *usage.Output
	cached := int64(0)
	if usage.CachedInput != nil {
		cached = *usage.CachedInput
	}
	writes := int64(0)
	if usage.CacheWriteInput != nil {
		writes = *usage.CacheWriteInput
	}
	write5m, write1h := int64(0), int64(0)
	if usage.CacheWrite5mInput != nil {
		write5m = *usage.CacheWrite5mInput
	}
	if usage.CacheWrite1hInput != nil {
		write1h = *usage.CacheWrite1hInput
	}
	if writes == 0 && (write5m != 0 || write1h != 0) {
		return 0, ErrUnknownCost
	}
	if write5m > writes || write1h > writes {
		return 0, fmt.Errorf("cache write detail exceeds aggregate")
	}
	writeDetails, err := CheckedAdd(write5m, write1h)
	if err != nil || writeDetails > writes {
		return 0, fmt.Errorf("cache write detail is not representable")
	}
	inputDetails, err := CheckedAdd(cached, writes)
	if err != nil || inputDetails > input {
		return 0, fmt.Errorf("cache input detail exceeds aggregate")
	}
	if price == nil {
		if input == 0 && output == 0 {
			return 0, nil
		}
		return 0, ErrUnknownCost
	}
	// A missing category counter is unknown unless the other inclusive
	// category is known to consume all input, which proves the missing
	// category is zero rather than guessing it.
	cachedKnown := usage.CachedInput != nil || usage.CacheWriteInput != nil && writes == input
	writesKnown := usage.CacheWriteInput != nil || usage.CachedInput != nil && cached == input
	if input > 0 && price.CachedInputPerMillion != nil && !cachedKnown {
		return 0, ErrUnknownCost
	}
	writeRateConfigured := price.CacheWriteInputPerMillion != nil || price.CacheWrite5mPerMillion != nil || price.CacheWrite1hPerMillion != nil
	if input > 0 && writeRateConfigured && !writesKnown {
		return 0, ErrUnknownCost
	}
	if writes > 0 {
		write5mKnown := usage.CacheWrite5mInput != nil || usage.CacheWrite1hInput != nil && write1h == writes
		write1hKnown := usage.CacheWrite1hInput != nil || usage.CacheWrite5mInput != nil && write5m == writes
		if !write5mKnown && price.CacheWrite5mPerMillion != nil &&
			(price.CacheWriteInputPerMillion == nil || *price.CacheWrite5mPerMillion != *price.CacheWriteInputPerMillion) {
			return 0, ErrUnknownCost
		}
		if !write1hKnown && price.CacheWrite1hPerMillion != nil &&
			(price.CacheWriteInputPerMillion == nil || *price.CacheWrite1hPerMillion != *price.CacheWriteInputPerMillion) {
			return 0, ErrUnknownCost
		}
	}
	ordinary := input - inputDetails
	terms := make([]costTerm, 0, 5)
	if ordinary > 0 {
		rate, present, err := priceRate(price.InputPerMillion)
		if err != nil {
			return 0, err
		}
		if !present {
			return 0, ErrUnknownCost
		}
		terms = append(terms, costTerm{ordinary, rate})
	}
	if cached > 0 {
		rate, present, err := priceRate(price.CachedInputPerMillion)
		if err != nil {
			return 0, err
		}
		if !present {
			return 0, ErrUnknownCost
		}
		terms = append(terms, costTerm{cached, rate})
	}
	aggregateWriteRate, aggregateWrite, err := priceRate(price.CacheWriteInputPerMillion)
	if err != nil {
		return 0, err
	}
	appendWrite := func(units int64, rate *int64) error {
		if units == 0 {
			return nil
		}
		selected, present, err := priceRate(rate)
		if err != nil {
			return err
		}
		if !present {
			if !aggregateWrite {
				return ErrUnknownCost
			}
			selected = aggregateWriteRate
		}
		terms = append(terms, costTerm{units, selected})
		return nil
	}
	if err := appendWrite(write5m, price.CacheWrite5mPerMillion); err != nil {
		return 0, err
	}
	if err := appendWrite(write1h, price.CacheWrite1hPerMillion); err != nil {
		return 0, err
	}
	remainingWrites := writes - writeDetails
	if remainingWrites > 0 {
		if !aggregateWrite {
			return 0, ErrUnknownCost
		}
		terms = append(terms, costTerm{remainingWrites, aggregateWriteRate})
	}
	if output > 0 {
		rate, present, err := priceRate(price.OutputPerMillion)
		if err != nil {
			return 0, err
		}
		if !present {
			return 0, ErrUnknownCost
		}
		terms = append(terms, costTerm{output, rate})
	}
	return estimateTerms(terms, 1000000)
}
