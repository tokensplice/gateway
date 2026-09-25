package common

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withCurrencyRateLookup installs a test rate lookup and restores the previous
// one after the test.
func withCurrencyRateLookup(t *testing.T, lookup CurrencyRateLookup) {
	t.Helper()
	previous := currencyRateLookup
	SetCurrencyRateLookup(lookup)
	t.Cleanup(func() { SetCurrencyRateLookup(previous) })
}

func TestNormalizeCurrencyCode(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"usd", "USD"},
		{"  hkd ", "HKD"},
		{"CNY", "CNY"},
		{"US", ""},
		{"USDD", ""},
		{"U1D", ""},
		{"", ""},
	}
	for _, test := range tests {
		assert.Equal(t, test.expected, NormalizeCurrencyCode(test.input), "input=%q", test.input)
	}
}

func TestConvertQuotaToCurrency(t *testing.T) {
	// 500000 quota is exactly 1 USD at the default QuotaPerUnit.
	require.InDelta(t, 500000.0, QuotaPerUnit, 1e-9)

	fixedRates := map[string]float64{"HKD": 7.8, "JPY": 157}
	withCurrencyRateLookup(t, func(base, target string) (float64, bool) {
		require.Equal(t, "USD", base)
		rate, ok := fixedRates[target]
		return rate, ok
	})

	tests := []struct {
		name           string
		quota          int64
		target         string
		expectedAmount float64
		expectedCode   string
		expectedRateOK bool
	}{
		{"usd passthrough", 500000, "USD", 1.0, "USD", true},
		{"hkd conversion", 500000, "hkd", 7.8, "HKD", true},
		{"jpy conversion", 250000, "JPY", 78.5, "JPY", true},
		{"unknown pair falls back to usd amount", 500000, "THB", 1.0, "THB", false},
		{"empty target uses default display currency", 500000, "", 1.0, "USD", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			amount, code, rateOK := ConvertQuotaToCurrency(test.quota, test.target)
			assert.InDelta(t, test.expectedAmount, amount, 1e-9)
			assert.Equal(t, test.expectedCode, code)
			assert.Equal(t, test.expectedRateOK, rateOK)
		})
	}
}

func TestConvertQuotaToCurrencyWithoutLookup(t *testing.T) {
	withCurrencyRateLookup(t, nil)

	amount, code, rateOK := ConvertQuotaToCurrency(500000, "CNY")
	assert.InDelta(t, 1.0, amount, 1e-9)
	assert.Equal(t, "CNY", code)
	assert.False(t, rateOK)

	amount, code, rateOK = ConvertQuotaToCurrency(500000, "USD")
	assert.InDelta(t, 1.0, amount, 1e-9)
	assert.Equal(t, "USD", code)
	assert.True(t, rateOK)
}

func TestConvertUSDToCurrencyRejectsInvalidLookupRates(t *testing.T) {
	for name, badRate := range map[string]float64{
		"nan":      math.NaN(),
		"inf":      math.Inf(1),
		"negative": -7.8,
	} {
		t.Run(name, func(t *testing.T) {
			withCurrencyRateLookup(t, func(string, string) (float64, bool) { return badRate, true })

			amount, code, rateOK := ConvertUSDToCurrency(2, "HKD")
			assert.InDelta(t, 2.0, amount, 1e-9)
			assert.Equal(t, "HKD", code)
			assert.False(t, rateOK)
		})
	}
}

func TestFormatCurrency(t *testing.T) {
	tests := []struct {
		amount   float64
		currency string
		expected string
	}{
		{1.5, "USD", "$1.500000"},
		{7.8, "hkd", "HK$7.800000"},
		{0.92, "EUR", "€0.920000"},
		{1380, "KRW", "₩1380.000000"},
		{2.5, "XYZ", "XYZ2.500000"}, // unknown but well-formed code falls back to the code
		{2.5, "", "$2.500000"},      // empty falls back to the default display currency
	}
	for _, test := range tests {
		assert.Equal(t, test.expected, FormatCurrency(test.amount, test.currency), "currency=%q", test.currency)
	}
}

func TestSupportedCurrenciesContract(t *testing.T) {
	supported := SupportedCurrencies()
	require.Contains(t, supported, "USD")
	for _, code := range supported {
		assert.True(t, IsSupportedCurrency(code), "code=%s", code)
		assert.NotEmpty(t, CurrencySymbol(code), "code=%s", code)
	}
	assert.False(t, IsSupportedCurrency("THB"))
	// Mutating the returned slice must not affect the package state.
	supported[0] = "XXX"
	assert.Equal(t, "USD", SupportedCurrencies()[0])
}
