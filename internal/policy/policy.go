// Package policy provides stateless validation, never admission or accounting.
package policy

import (
	"fmt"
	"hoorific/internal/core"
	"math/big"
)

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

// EstimateCost returns ceil((input*inputRate + output*outputRate)/unit).
// Rates and result are integer USD nanodollars; rates are nanodollars per
// million tokens when unit is 1,000,000. big.Int prevents intermediate
// multiplication overflow, and a result outside int64 is rejected.
func EstimateCost(input, output, inputRate, outputRate, unit int64) (int64, error) {
	if input < 0 || output < 0 || inputRate < 0 || outputRate < 0 || unit <= 0 {
		return 0, fmt.Errorf("invalid cost operands")
	}
	a := new(big.Int).Mul(big.NewInt(input), big.NewInt(inputRate))
	b := new(big.Int).Mul(big.NewInt(output), big.NewInt(outputRate))
	a.Add(a, b)
	a.Add(a, big.NewInt(unit-1))
	a.Quo(a, big.NewInt(unit))
	if !a.IsInt64() {
		return 0, fmt.Errorf("cost exceeds int64")
	}
	return a.Int64(), nil
}
