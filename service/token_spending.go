package service

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"

	"github.com/bytedance/gopkg/util/gopool"
)

// tokenSpendingReadScript returns {exists, value} so a cached zero total stays
// distinguishable from a cold key without importing the Redis client's Nil error.
const tokenSpendingReadScript = `
local current = redis.call('GET', KEYS[1])
if current == false then
  return {0, 0}
end
return {1, tonumber(current)}
`

// tokenSpendingInitScript publishes a total recomputed from the consume logs
// only while the key is still cold, so it cannot overwrite increments recorded
// between the aggregate query and this write. It returns the effective value.
const tokenSpendingInitScript = `
if redis.call('EXISTS', KEYS[1]) == 1 then
  return tonumber(redis.call('GET', KEYS[1]))
end
redis.call('SET', KEYS[1], ARGV[1])
redis.call('EXPIRE', KEYS[1], ARGV[2])
return tonumber(ARGV[1])
`

// tokenSpendingDeltaScript adds to a warm total and reports a cold key as a nil
// reply, because a missing key must be rebuilt from the logs instead of starting
// at zero and silently understating the token's spending.
const tokenSpendingDeltaScript = `
if redis.call('EXISTS', KEYS[1]) ~= 1 then
  return false
end
return redis.call('INCRBY', KEYS[1], ARGV[1])
`

// SpendingCapKind identifies which per-token spending cap rejected a request.
type SpendingCapKind string

const (
	SpendingCapDaily   SpendingCapKind = "daily"
	SpendingCapMonthly SpendingCapKind = "monthly"
)

// SpendingCapExceeded describes a request rejected by a per-token spending cap.
// ResetAt is the UTC timestamp when the capped period starts over.
type SpendingCapExceeded struct {
	Kind    SpendingCapKind
	Cap     int64
	Used    int64
	ResetAt int64
}

func init() {
	model.TokenConsumeRecorder = recordTokenConsumption
}

// recordTokenConsumption keeps the per-token counters warm from the consumption
// facts the log layer reports. It runs off the request path: both counters are
// advisory caches that are rebuilt from the consume logs whenever they go cold.
func recordTokenConsumption(tokenId int, quota int, promptTokens int, completionTokens int) {
	if tokenId <= 0 {
		return
	}
	gopool.Go(func() {
		ctx := context.Background()
		now := time.Now()
		if quota != 0 {
			addTokenSpending(ctx, tokenId, int64(quota), now)
		}
		recordTokenTokens(ctx, tokenId, promptTokens+completionTokens, now)
	})
}

// CheckTokenSpendingCaps returns the first cap the token has already reached, or
// nil when the request may proceed.
func CheckTokenSpendingCaps(ctx context.Context, token *model.Token) *SpendingCapExceeded {
	return checkTokenSpendingCaps(ctx, token, time.Now().UTC())
}

// checkTokenSpendingCaps enforces the token's daily and monthly caps. Caps count
// quota recorded in the consume logs, so in-flight requests are not counted yet
// and a burst of concurrent requests can overshoot a cap by at most its own
// pre-consumed quota. A counter failure admits the request: a cache or log
// outage must not block traffic that is already within the token's quota.
func checkTokenSpendingCaps(ctx context.Context, token *model.Token, now time.Time) *SpendingCapExceeded {
	if token == nil || (token.DailySpendingCap <= 0 && token.MonthlySpendingCap <= 0) {
		return nil
	}

	if token.DailySpendingCap > 0 {
		periodStart, periodReset := dailySpendingPeriod(now)
		used, err := tokenSpendingSince(ctx, token.Id, periodStart, periodReset, now)
		if err != nil {
			logger.LogError(ctx, fmt.Sprintf("token %d daily spending lookup failed, admitting request: %v", token.Id, err))
		} else if used >= token.DailySpendingCap {
			return &SpendingCapExceeded{
				Kind:    SpendingCapDaily,
				Cap:     token.DailySpendingCap,
				Used:    used,
				ResetAt: periodReset,
			}
		}
	}

	if token.MonthlySpendingCap > 0 {
		periodStart, periodReset := monthlySpendingPeriod(now)
		used, err := tokenSpendingSince(ctx, token.Id, periodStart, periodReset, now)
		if err != nil {
			logger.LogError(ctx, fmt.Sprintf("token %d monthly spending lookup failed, admitting request: %v", token.Id, err))
		} else if used >= token.MonthlySpendingCap {
			return &SpendingCapExceeded{
				Kind:    SpendingCapMonthly,
				Cap:     token.MonthlySpendingCap,
				Used:    used,
				ResetAt: periodReset,
			}
		}
	}
	return nil
}

// dailySpendingPeriod returns the UTC day boundaries around now: midnight that
// started the day and midnight that starts the next one.
func dailySpendingPeriod(now time.Time) (start int64, reset int64) {
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return dayStart.Unix(), dayStart.AddDate(0, 0, 1).Unix()
}

// monthlySpendingPeriod returns the UTC month boundaries around now: midnight on
// the first day of the month and midnight on the first day of the next one.
func monthlySpendingPeriod(now time.Time) (start int64, reset int64) {
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return monthStart.Unix(), monthStart.AddDate(0, 1, 0).Unix()
}

// tokenSpendingSince returns the quota a token consumed since periodStart,
// served from the shared counter cache and rebuilt from the consume logs when
// the cache entry is cold.
func tokenSpendingSince(ctx context.Context, tokenId int, periodStart int64, periodReset int64, now time.Time) (int64, error) {
	if !tokenRedisCountersEnabled() {
		key := tokenSpendingCacheKey{tokenId: tokenId, periodStart: periodStart}
		if total, ok := tokenSpendingTotals.cached(key, now); ok {
			return total, nil
		}
		total, err := model.SumTokenConsumeQuotaSince(tokenId, periodStart)
		if err != nil {
			return 0, err
		}
		tokenSpendingTotals.store(key, total, now, tokenSpendingCacheTTL(periodReset, now))
		return total, nil
	}

	cacheKey := tokenSpendingRedisKey(tokenId, periodStart)
	reply, err := common.RDB.Eval(ctx, tokenSpendingReadScript, []string{cacheKey}).Slice()
	if err != nil {
		return 0, err
	}
	if len(reply) != 2 {
		return 0, fmt.Errorf("unexpected token spending reply length %d", len(reply))
	}
	cached, err := redisCounterInt64(reply[0])
	if err != nil {
		return 0, err
	}
	if cached == 1 {
		return redisCounterInt64(reply[1])
	}

	total, err := model.SumTokenConsumeQuotaSince(tokenId, periodStart)
	if err != nil {
		return 0, err
	}
	ttlSeconds := int64(tokenSpendingCacheTTL(periodReset, now).Seconds())
	effective, err := common.RDB.Eval(ctx, tokenSpendingInitScript, []string{cacheKey}, total, ttlSeconds).Int64()
	if err != nil {
		// A failed cache write only costs the next request another aggregate query.
		logger.LogError(ctx, fmt.Sprintf("token %d spending cache write failed: %v", tokenId, err))
		return total, nil
	}
	return effective, nil
}

// addTokenSpending folds a freshly logged consumption into the warm counter for
// every period the token may cap. Cold counters are left alone: the next check
// rebuilds them from the consume logs, which already contain this record.
func addTokenSpending(ctx context.Context, tokenId int, quota int64, now time.Time) {
	now = now.UTC()
	dailyStart, _ := dailySpendingPeriod(now)
	monthlyStart, _ := monthlySpendingPeriod(now)
	for _, periodStart := range []int64{dailyStart, monthlyStart} {
		if tokenRedisCountersEnabled() {
			cacheKey := tokenSpendingRedisKey(tokenId, periodStart)
			if _, err := common.RDB.Eval(ctx, tokenSpendingDeltaScript, []string{cacheKey}, quota).Int64(); err != nil {
				logger.LogDebug(ctx, "token %d spending counter for period %d is cold: %v", tokenId, periodStart, err)
			}
			continue
		}
		tokenSpendingTotals.add(tokenSpendingCacheKey{tokenId: tokenId, periodStart: periodStart}, quota, now)
	}
}

func tokenSpendingRedisKey(tokenId int, periodStart int64) string {
	return fmt.Sprintf("token_spend:%d:%d", tokenId, periodStart)
}

// tokenSpendingCacheTTL bounds a cached total by both the configured refresh
// interval and the remaining period, so a counter never outlives its own cap.
func tokenSpendingCacheTTL(periodReset int64, now time.Time) time.Duration {
	ttl := time.Duration(constant.TokenSpendingCacheSeconds) * time.Second
	if remaining := time.Unix(periodReset, 0).Sub(now); remaining > 0 && remaining < ttl {
		ttl = remaining
	}
	return ttl
}

// redisCounterInt64 coerces the integer replies of the counter scripts, which
// Redis may deliver as int64, string, or bytes.
func redisCounterInt64(value any) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case string:
		return strconv.ParseInt(typed, 10, 64)
	case []byte:
		return strconv.ParseInt(string(typed), 10, 64)
	default:
		return 0, fmt.Errorf("unexpected Redis integer reply type %T", value)
	}
}

// tokenSpendingCacheKey identifies one cached period total.
type tokenSpendingCacheKey struct {
	tokenId     int
	periodStart int64
}

type tokenSpendingEntry struct {
	total     int64
	expiresAt time.Time
}

// tokenSpendingStore is the in-memory fallback for the shared spending counters.
// Expired entries are swept at most once a minute so a long-running process does
// not retain a total for every period a token ever touched.
type tokenSpendingStore struct {
	mutex     sync.Mutex
	entries   map[tokenSpendingCacheKey]tokenSpendingEntry
	lastSweep time.Time
}

var tokenSpendingTotals = &tokenSpendingStore{entries: make(map[tokenSpendingCacheKey]tokenSpendingEntry)}

func (s *tokenSpendingStore) cached(key tokenSpendingCacheKey, now time.Time) (int64, bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	entry, ok := s.entries[key]
	if !ok || !now.Before(entry.expiresAt) {
		return 0, false
	}
	return entry.total, true
}

func (s *tokenSpendingStore) store(key tokenSpendingCacheKey, total int64, now time.Time, ttl time.Duration) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.sweepLocked(now)
	s.entries[key] = tokenSpendingEntry{total: total, expiresAt: now.Add(ttl)}
}

func (s *tokenSpendingStore) add(key tokenSpendingCacheKey, delta int64, now time.Time) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	entry, ok := s.entries[key]
	if !ok || !now.Before(entry.expiresAt) {
		return
	}
	entry.total += delta
	s.entries[key] = entry
}

func (s *tokenSpendingStore) sweepLocked(now time.Time) {
	if now.Sub(s.lastSweep) < time.Minute {
		return
	}
	s.lastSweep = now
	for key, entry := range s.entries {
		if !now.Before(entry.expiresAt) {
			delete(s.entries, key)
		}
	}
}
