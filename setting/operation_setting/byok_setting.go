package operation_setting

import (
	"fmt"
	"math"
	"strconv"
)

// BYOK platform fee configuration.
//
// When a request is served by the customer's own upstream key the gateway does
// not pay for the tokens, so it charges a platform fee instead: a percentage
// of the quota the same request would have cost through a managed channel (the
// "notional cost"). The fee is stored in quota, the same internal unit as every
// other charge, where common.QuotaPerUnit quota equals one dollar.
const (
	DefaultByokFeePercent = 5.0
	MinByokFeePercent     = 0.0
	MaxByokFeePercent     = 20.0
	// ByokFeeNoOverride is the sentinel stored in users.byok_fee_override
	// when the user inherits ByokFeePercent instead of an admin-set value.
	ByokFeeNoOverride = -1.0
)

// ByokFeePercent is the global default. A user whose byok_fee_override is not
// -1 uses that override instead; see ResolveByokFeePercent in
// service/byok_billing.go.
var ByokFeePercent = DefaultByokFeePercent

// ByokFeePercentOptionKey is the OptionMap / options-table key.
const ByokFeePercentOptionKey = "ByokFeePercent"

// ValidateByokFeePercent rejects a persisted value outside the supported band.
// It runs before the option is written, so an out-of-range fee can never reach
// the in-memory variable that billing reads.
func ValidateByokFeePercent(value string) error {
	number, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fmt.Errorf("%s must be a number", ByokFeePercentOptionKey)
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return fmt.Errorf("%s must be a finite number", ByokFeePercentOptionKey)
	}
	if number < MinByokFeePercent || number > MaxByokFeePercent {
		return fmt.Errorf("%s must be between %g and %g", ByokFeePercentOptionKey, MinByokFeePercent, MaxByokFeePercent)
	}
	return nil
}

// ClampByokFeePercent bounds a runtime percentage to the supported band. A
// value that is NaN, negative, or absurd cannot become a credit or an
// unbounded charge; it falls back to the nearest allowed bound and, for a NaN,
// to the default.
func ClampByokFeePercent(percent float64) float64 {
	if math.IsNaN(percent) {
		return DefaultByokFeePercent
	}
	if math.IsInf(percent, 1) || percent > MaxByokFeePercent {
		return MaxByokFeePercent
	}
	if math.IsInf(percent, -1) || percent < MinByokFeePercent {
		return MinByokFeePercent
	}
	return percent
}
