package config

import (
	"fmt"
	"math"
)

const (
	MinTokenBillingInflationPercent = 0.0
	MaxTokenBillingInflationPercent = 99999.0
)

// ValidateTokenBillingInflationPercent accepts a finite percentage in the
// supported inclusive range. The value is a percentage, so 100 means +100%.
func ValidateTokenBillingInflationPercent(percent float64) error {
	if math.IsNaN(percent) || math.IsInf(percent, 0) || percent < MinTokenBillingInflationPercent || percent > MaxTokenBillingInflationPercent {
		return fmt.Errorf("tokenBillingInflationPercent must be a finite number between %.0f and %.0f", MinTokenBillingInflationPercent, MaxTokenBillingInflationPercent)
	}
	return nil
}

// GetTokenBillingInflationPercent returns the configured billing percentage.
// Existing configuration files that omit the field use zero inflation.
func GetTokenBillingInflationPercent() float64 {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return 0
	}
	return cfg.TokenBillingInflationPercent
}

// UpdateTokenBillingInflationPercent validates and persists the percentage.
// A failed save leaves the in-memory value unchanged.
func UpdateTokenBillingInflationPercent(percent float64) error {
	if err := ValidateTokenBillingInflationPercent(percent); err != nil {
		return err
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return fmt.Errorf("config is not initialized")
	}
	previous := cfg.TokenBillingInflationPercent
	cfg.TokenBillingInflationPercent = percent
	if err := saveLocked(); err != nil {
		cfg.TokenBillingInflationPercent = previous
		return err
	}
	return nil
}
