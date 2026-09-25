package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
)

const (
	// currencyRateAPIURL is a free key-less exchange rate feed that updates
	// daily. Rates are USD-based: "1 USD = X TARGET".
	currencyRateAPIURL       = "https://open.er-api.com/v6/latest/USD"
	currencyRateFetchTimeout = 20 * time.Second

	defaultCurrencyUpdateIntervalHours = 24
)

// CurrencyAutoUpdateEnabled reports whether the periodic exchange rate refresh
// scheduled task is enabled (CURRENCY_AUTO_UPDATE, default true).
func CurrencyAutoUpdateEnabled() bool {
	return common.GetEnvOrDefaultBool("CURRENCY_AUTO_UPDATE", true)
}

// CurrencyUpdateInterval is the auto-refresh cadence
// (CURRENCY_UPDATE_INTERVAL_HOURS, default 24, minimum 1).
func CurrencyUpdateInterval() time.Duration {
	hours := common.GetEnvOrDefault("CURRENCY_UPDATE_INTERVAL_HOURS", defaultCurrencyUpdateIntervalHours)
	if hours < 1 {
		hours = defaultCurrencyUpdateIntervalHours
	}
	return time.Duration(hours) * time.Hour
}

type openExchangeRateResponse struct {
	Result string             `json:"result"`
	Rates  map[string]float64 `json:"rates"`
}

// RefreshCurrencyRates fetches the latest USD-based rates and updates every
// non-manual pair (auto-updated rows plus any missing default pair). Pairs an
// admin set manually (source=manual) are never overwritten. On fetch failure
// the existing rates are kept but auto rows are flagged rate_ok=false so
// consumers can show them as stale. Returns the number of updated pairs.
func RefreshCurrencyRates(ctx context.Context) (int, error) {
	fetched, err := fetchOpenExchangeRates(ctx)
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("currency rate refresh failed, keeping existing rates: %v", err))
		if _, markErr := model.MarkAutoCurrencyRatesUnreliable(); markErr != nil {
			logger.LogWarn(ctx, fmt.Sprintf("failed to mark currency rates unreliable: %v", markErr))
		}
		return 0, err
	}

	// Targets to refresh: every non-manual USD pair already in the table, plus
	// any built-in supported pair that has no row yet (so deleted defaults come
	// back). Pairs an admin set manually are excluded from both passes.
	targets := map[string]bool{}
	manualTargets := map[string]bool{}
	rates, err := model.GetAllCurrencyRates()
	if err != nil {
		return 0, err
	}
	for _, rate := range rates {
		if rate.BaseCurrency != "USD" {
			continue
		}
		if rate.Source == model.CurrencyRateSourceManual {
			manualTargets[rate.TargetCurrency] = true
			continue
		}
		targets[rate.TargetCurrency] = true
	}
	for _, code := range common.SupportedCurrencies() {
		if code != "USD" && !manualTargets[code] {
			targets[code] = true
		}
	}

	updated := 0
	for target := range targets {
		value, ok := fetched[target]
		if !ok || value <= 0 {
			continue
		}
		if err := model.UpsertCurrencyRate("USD", target, value, model.CurrencyRateSourceAPI, true); err != nil {
			logger.LogWarn(ctx, fmt.Sprintf("failed to update currency rate USD->%s: %v", target, err))
			continue
		}
		updated++
	}
	return updated, nil
}

func fetchOpenExchangeRates(ctx context.Context) (map[string]float64, error) {
	requestCtx, cancel := context.WithTimeout(ctx, currencyRateFetchTimeout)
	defer cancel()

	client := GetHttpClient()
	if client == nil {
		client = &http.Client{Timeout: currencyRateFetchTimeout}
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, currencyRateAPIURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exchange rate API returned status %d", response.StatusCode)
	}
	var payload openExchangeRateResponse
	if err := common.DecodeJson(response.Body, &payload); err != nil {
		return nil, err
	}
	if payload.Result != "success" || len(payload.Rates) == 0 {
		return nil, errors.New("exchange rate API returned no usable rates")
	}
	return payload.Rates, nil
}
