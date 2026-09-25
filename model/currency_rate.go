package model

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/common/idgen"

	"gorm.io/gorm/clause"
)

const (
	CurrencyRateSourceManual = "manual"
	CurrencyRateSourceAPI    = "api"

	currencyBaseUSD      = "USD"
	currencyRateCacheTTL = time.Minute
)

// CurrencyRate stores one display exchange rate pair. Rate semantics are
// "1 BaseCurrency = Rate TargetCurrency" (e.g. USD->HKD 7.8). Rates are used
// for DISPLAY conversion only; internal billing always stays in quota.
//
// RateOK=false marks a fallback/unreliable rate (e.g. the exchange rate API
// was unreachable during the last refresh); the value is still shown to users
// but flagged so the frontend can render a warning.
type CurrencyRate struct {
	BaseModel
	BaseCurrency   string    `json:"base_currency" gorm:"size:10;default:USD;index:idx_currency_pair,unique"`
	TargetCurrency string    `json:"target_currency" gorm:"size:10;index:idx_currency_pair,unique"`
	Rate           float64   `json:"rate" gorm:"type:decimal(18,8)"`
	Source         string    `json:"source" gorm:"size:20;default:manual"` // manual, api
	RateOK         bool      `json:"rate_ok"`
	UpdatedAt      time.Time `json:"updated_at"`
	CreatedAt      time.Time `json:"created_at"`
}

// defaultCurrencyRates seeds the table on first boot. Rows are seeded with
// source=api so the periodic auto-update replaces them; an admin edit flips a
// pair to source=manual, which auto-update never overwrites.
var defaultCurrencyRates = map[string]float64{
	"HKD": 7.8,
	"CNY": 7.3,
	"EUR": 0.92,
	"GBP": 0.79,
	"JPY": 157,
	"KRW": 1380,
	"SGD": 1.35,
	"TWD": 32.5,
}

// SeedDefaultCurrencyRates inserts the default USD pairs when the table is
// empty. It is idempotent: the (base, target) unique index plus OnConflict
// DoNothing keep concurrent seeds and restarts safe.
func SeedDefaultCurrencyRates() error {
	var count int64
	if err := DB.Model(&CurrencyRate{}).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	now := time.Now()
	for _, target := range common.SupportedCurrencies() {
		rate, ok := defaultCurrencyRates[target]
		if !ok {
			continue
		}
		entry := CurrencyRate{
			BaseCurrency:   currencyBaseUSD,
			TargetCurrency: target,
			Rate:           rate,
			Source:         CurrencyRateSourceAPI,
			RateOK:         true,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		entry.ID = idgen.Next()
		err := DB.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "base_currency"}, {Name: "target_currency"}},
			DoNothing: true,
		}).Create(&entry).Error
		if err != nil {
			return err
		}
	}
	InvalidateCurrencyRateCache()
	return nil
}

func GetAllCurrencyRates() ([]CurrencyRate, error) {
	var rates []CurrencyRate
	err := DB.Order("base_currency asc, target_currency asc").Find(&rates).Error
	return rates, err
}

// UpsertCurrencyRate inserts or updates one pair keyed by (base, target).
// rate must be a positive finite number; rateOK reflects whether the value is
// a trustworthy rate or a fallback.
func UpsertCurrencyRate(baseCurrency, targetCurrency string, rate float64, source string, rateOK bool) error {
	base := common.NormalizeCurrencyCode(baseCurrency)
	target := common.NormalizeCurrencyCode(targetCurrency)
	if base == "" || target == "" {
		return fmt.Errorf("invalid currency pair: %s -> %s", baseCurrency, targetCurrency)
	}
	if base == target {
		return fmt.Errorf("base and target currency must differ: %s", base)
	}
	if !(rate > 0) || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return fmt.Errorf("rate must be a positive finite number, got %v", rate)
	}
	if source != CurrencyRateSourceManual && source != CurrencyRateSourceAPI {
		return fmt.Errorf("unknown currency rate source: %s", source)
	}

	now := time.Now()
	entry := CurrencyRate{
		BaseCurrency:   base,
		TargetCurrency: target,
		Rate:           rate,
		Source:         source,
		RateOK:         rateOK,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	entry.ID = idgen.Next()
	err := DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "base_currency"}, {Name: "target_currency"}},
		DoUpdates: clause.Assignments(map[string]any{
			"rate":       rate,
			"source":     source,
			"rate_ok":    rateOK,
			"updated_at": now,
		}),
	}).Create(&entry).Error
	InvalidateCurrencyRateCache()
	return err
}

func DeleteCurrencyRateById(id int64) error {
	result := DB.Delete(&CurrencyRate{}, id)
	InvalidateCurrencyRateCache()
	return result.Error
}

// MarkAutoCurrencyRatesUnreliable flags every non-manual pair as rate_ok=false
// after a failed exchange rate refresh. The stored rates are kept so displays
// keep working, but consumers can show them as stale. Manual admin rates stay
// authoritative.
func MarkAutoCurrencyRatesUnreliable() (int64, error) {
	result := DB.Model(&CurrencyRate{}).
		Where("source <> ?", CurrencyRateSourceManual).
		Update("rate_ok", false)
	InvalidateCurrencyRateCache()
	return result.RowsAffected, result.Error
}

// Rate lookups are cached in memory: display conversion runs on every
// dashboard/pricing response while rates change at most once per refresh.
var (
	currencyRateCacheMu  sync.RWMutex
	currencyRateCache    map[string]float64
	currencyRateCachedAt time.Time
)

func currencyRateCacheKey(base, target string) string {
	return base + "->" + target
}

// LookupCurrencyRate resolves 1 base = X target from the cached rate table.
// It returns (1, false) when the pair is missing, stale-flagged, or the cache
// cannot be loaded, matching the fallback contract of common.CurrencyRateLookup.
func LookupCurrencyRate(baseCurrency, targetCurrency string) (float64, bool) {
	base := common.NormalizeCurrencyCode(baseCurrency)
	target := common.NormalizeCurrencyCode(targetCurrency)
	if base == "" || target == "" {
		return 1, false
	}
	if base == target {
		return 1, true
	}

	currencyRateCacheMu.RLock()
	cache, cachedAt := currencyRateCache, currencyRateCachedAt
	currencyRateCacheMu.RUnlock()
	if cache == nil || time.Since(cachedAt) > currencyRateCacheTTL {
		var err error
		if cache, err = loadCurrencyRateCache(); err != nil {
			common.SysError("failed to load currency rate cache: " + err.Error())
			return 1, false
		}
	}

	rate, ok := cache[currencyRateCacheKey(base, target)]
	if !ok || rate <= 0 {
		return 1, false
	}
	return rate, true
}

func loadCurrencyRateCache() (map[string]float64, error) {
	rates, err := GetAllCurrencyRates()
	if err != nil {
		return nil, err
	}
	cache := make(map[string]float64, len(rates))
	for _, rate := range rates {
		if !rate.RateOK || rate.Rate <= 0 {
			continue
		}
		key := currencyRateCacheKey(
			strings.ToUpper(rate.BaseCurrency),
			strings.ToUpper(rate.TargetCurrency),
		)
		cache[key] = rate.Rate
	}
	currencyRateCacheMu.Lock()
	currencyRateCache = cache
	currencyRateCachedAt = time.Now()
	currencyRateCacheMu.Unlock()
	return cache, nil
}

// InvalidateCurrencyRateCache forces the next lookup to re-read the table.
func InvalidateCurrencyRateCache() {
	currencyRateCacheMu.Lock()
	currencyRateCache = nil
	currencyRateCachedAt = time.Time{}
	currencyRateCacheMu.Unlock()
}

func init() {
	common.SetCurrencyRateLookup(LookupCurrencyRate)
}
