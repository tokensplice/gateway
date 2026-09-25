package common

import (
	"math"
	"slices"
	"strconv"
	"strings"
)

// DefaultDisplayCurrency is the fallback display currency for users without a
// preferred_currency setting. Initialized from the DEFAULT_DISPLAY_CURRENCY
// environment variable in InitEnv.
var DefaultDisplayCurrency = "USD"

// currencySymbols maps the supported ISO 4217 display codes to their symbols.
// The key set defines which currencies users may pick as display currency.
var currencySymbols = map[string]string{
	"USD": "$",
	"HKD": "HK$",
	"CNY": "¥",
	"EUR": "€",
	"GBP": "£",
	"JPY": "¥",
	"KRW": "₩",
	"SGD": "S$",
	"TWD": "NT$",
}

// supportedCurrencyOrder keeps the dropdown ordering stable.
var supportedCurrencyOrder = []string{"USD", "HKD", "CNY", "EUR", "GBP", "JPY", "KRW", "SGD", "TWD"}

// SupportedCurrencies returns the display currency codes in dropdown order.
func SupportedCurrencies() []string {
	return slices.Clone(supportedCurrencyOrder)
}

// IsSupportedCurrency reports whether code is a selectable display currency.
func IsSupportedCurrency(code string) bool {
	_, ok := currencySymbols[code]
	return ok
}

// CurrencySymbol returns the display symbol for a currency code, falling back
// to the code itself for unknown currencies.
func CurrencySymbol(code string) string {
	if symbol, ok := currencySymbols[code]; ok {
		return symbol
	}
	return code
}

// NormalizeCurrencyCode trims and upper-cases a currency code and returns ""
// when it is not a 3-letter ISO 4217 style code.
func NormalizeCurrencyCode(code string) string {
	normalized := strings.ToUpper(strings.TrimSpace(code))
	if len(normalized) != 3 {
		return ""
	}
	for _, r := range normalized {
		if r < 'A' || r > 'Z' {
			return ""
		}
	}
	return normalized
}

// CurrencyRateLookup resolves the exchange rate for 1 base = X target.
// It returns (1, false) when no reliable rate is available so display code can
// fall back to the USD amount while flagging the value as unreliable.
type CurrencyRateLookup func(baseCurrency, targetCurrency string) (rate float64, rateOK bool)

var currencyRateLookup CurrencyRateLookup

// SetCurrencyRateLookup installs the database-backed rate resolver (registered
// by the model package). Kept as a hook so common stays import-cycle free.
func SetCurrencyRateLookup(lookup CurrencyRateLookup) {
	currencyRateLookup = lookup
}

// ConvertQuotaToCurrency converts an internal quota amount to the target
// display currency. This is DISPLAY ONLY: billing always stays in quota.
// Returns (amount, currencyCode, rateOK); rateOK is false when the rate is
// unavailable and the amount fell back to the raw USD value (rate 1.0).
func ConvertQuotaToCurrency(quota int64, targetCurrency string) (float64, string, bool) {
	return ConvertUSDToCurrency(float64(quota)/QuotaPerUnit, targetCurrency)
}

// ConvertUSDToCurrency converts a USD amount to the target display currency
// with the same fallback semantics as ConvertQuotaToCurrency.
func ConvertUSDToCurrency(amountUSD float64, targetCurrency string) (float64, string, bool) {
	target := NormalizeCurrencyCode(targetCurrency)
	if target == "" {
		target = DefaultDisplayCurrency
	}
	if target == "USD" {
		return amountUSD, "USD", true
	}
	rate, rateOK := 1.0, false
	if currencyRateLookup != nil {
		if lookupRate, ok := currencyRateLookup("USD", target); ok &&
			lookupRate > 0 && !math.IsNaN(lookupRate) && !math.IsInf(lookupRate, 0) {
			rate, rateOK = lookupRate, true
		}
	}
	return amountUSD * rate, target, rateOK
}

// FormatCurrency formats an amount with its currency symbol, e.g. "$1.250000".
func FormatCurrency(amount float64, currency string) string {
	code := NormalizeCurrencyCode(currency)
	if code == "" {
		code = DefaultDisplayCurrency
	}
	return CurrencySymbol(code) + strconv.FormatFloat(amount, 'f', 6, 64)
}
