package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
)

// Per-token throttling counts against fixed one-minute buckets. The bucket is
// part of the Redis key, so a window rolls over by itself and the key TTL
// doubles as the Retry-After hint. Without Redis the same buckets live in
// process memory and are shared by every token that process serves.
const (
	tokenRateWindowSeconds = 60
	tokenRateRPMKeyPrefix  = "token_rate"
	tokenRateTPMKeyPrefix  = "token_tpm"
)

// TokenRateLimitKind identifies which per-token limit rejected a request.
type TokenRateLimitKind string

const (
	TokenRateLimitRPM TokenRateLimitKind = "rpm"
	TokenRateLimitTPM TokenRateLimitKind = "tpm"
)

// TokenRateLimitDecision is the outcome of a per-token throttling check.
// RetryAfterSeconds, Limit, and Current only carry meaning when Allowed is false.
type TokenRateLimitDecision struct {
	Allowed           bool
	Kind              TokenRateLimitKind
	Limit             int
	Current           int64
	RetryAfterSeconds int64
}

// tokenWindowIncrScript adds a delta to a window counter and arms its TTL only
// while the key has none, so later traffic can never extend a window past its
// own minute bucket.
const tokenWindowIncrScript = `
local count = redis.call('INCRBY', KEYS[1], ARGV[1])
if redis.call('TTL', KEYS[1]) < 0 then
  redis.call('EXPIRE', KEYS[1], ARGV[2])
end
return count
`

// tokenWindowReadScript reads a window counter without creating the key, so a
// tokens-per-minute pre-check stays free while the window is still empty.
const tokenWindowReadScript = `
local current = redis.call('GET', KEYS[1])
if current == false then
  return 0
end
return tonumber(current)
`

// tokenRedisCountersEnabled reports whether per-token counters are shared
// through Redis instead of kept in process memory.
func tokenRedisCountersEnabled() bool {
	return common.RedisEnabled && common.RDB != nil
}

// CheckTokenRateLimit admits or rejects one request against the token's current
// minute windows.
func CheckTokenRateLimit(ctx context.Context, token *model.Token) TokenRateLimitDecision {
	return checkTokenRateLimit(ctx, token, time.Now())
}

// checkTokenRateLimit admits or rejects one request for the token. An admitted
// request consumes one slot of the current requests-per-minute window; the
// tokens-per-minute window is only read here and filled by recordTokenTokens
// once a request's usage is known. A token without its own request limit falls
// back to TOKEN_DEFAULT_RATE_LIMIT_RPM, and zero on both sides means unthrottled.
//
// Counter failures (for example a Redis outage) admit the request: throttling
// must not turn a cache outage into a full relay outage.
func checkTokenRateLimit(ctx context.Context, token *model.Token, now time.Time) TokenRateLimitDecision {
	admitted := TokenRateLimitDecision{Allowed: true}
	if token == nil {
		return admitted
	}

	requestLimit := token.RateLimitRPM
	if requestLimit == 0 {
		requestLimit = constant.TokenDefaultRateLimitRPM
	}
	if requestLimit <= 0 && token.RateLimitTPM <= 0 {
		return admitted
	}

	window := now.Unix() / tokenRateWindowSeconds
	retryAfter := tokenRateWindowSeconds - now.Unix()%tokenRateWindowSeconds

	if requestLimit > 0 {
		requests, err := addTokenWindowCount(ctx, tokenRateRPMKeyPrefix, token.Id, window, 1, retryAfter+1)
		if err != nil {
			logger.LogError(ctx, fmt.Sprintf("token %d rpm counter failed, admitting request: %v", token.Id, err))
		} else if requests > int64(requestLimit) {
			return TokenRateLimitDecision{
				Allowed:           false,
				Kind:              TokenRateLimitRPM,
				Limit:             requestLimit,
				Current:           requests,
				RetryAfterSeconds: retryAfter,
			}
		}
	}

	if token.RateLimitTPM > 0 {
		tokens, err := readTokenWindowCount(ctx, tokenRateTPMKeyPrefix, token.Id, window)
		if err != nil {
			logger.LogError(ctx, fmt.Sprintf("token %d tpm counter failed, admitting request: %v", token.Id, err))
			return admitted
		}
		if tokens >= int64(token.RateLimitTPM) {
			return TokenRateLimitDecision{
				Allowed:           false,
				Kind:              TokenRateLimitTPM,
				Limit:             token.RateLimitTPM,
				Current:           tokens,
				RetryAfterSeconds: retryAfter,
			}
		}
	}
	return admitted
}

// recordTokenTokens adds a finished request's prompt+completion tokens to the
// token's tokens-per-minute window.
func recordTokenTokens(ctx context.Context, tokenId int, tokens int, now time.Time) {
	if tokenId <= 0 || tokens <= 0 {
		return
	}
	window := now.Unix() / tokenRateWindowSeconds
	ttl := tokenRateWindowSeconds - now.Unix()%tokenRateWindowSeconds + 1
	if _, err := addTokenWindowCount(ctx, tokenRateTPMKeyPrefix, tokenId, window, int64(tokens), ttl); err != nil {
		logger.LogError(ctx, fmt.Sprintf("token %d tpm counter failed: %v", tokenId, err))
	}
}

func tokenWindowRedisKey(prefix string, tokenId int, window int64) string {
	return fmt.Sprintf("%s:%d:%d", prefix, tokenId, window)
}

func tokenWindowName(prefix string, tokenId int) string {
	return fmt.Sprintf("%s:%d", prefix, tokenId)
}

func addTokenWindowCount(ctx context.Context, prefix string, tokenId int, window int64, delta int64, ttlSeconds int64) (int64, error) {
	if !tokenRedisCountersEnabled() {
		return tokenWindows.add(tokenWindowName(prefix, tokenId), window, delta), nil
	}
	key := tokenWindowRedisKey(prefix, tokenId, window)
	return common.RDB.Eval(ctx, tokenWindowIncrScript, []string{key}, delta, ttlSeconds).Int64()
}

func readTokenWindowCount(ctx context.Context, prefix string, tokenId int, window int64) (int64, error) {
	if !tokenRedisCountersEnabled() {
		return tokenWindows.current(tokenWindowName(prefix, tokenId), window), nil
	}
	key := tokenWindowRedisKey(prefix, tokenId, window)
	return common.RDB.Eval(ctx, tokenWindowReadScript, []string{key}).Int64()
}

// tokenWindowBucket is one counter's value for one minute window.
type tokenWindowBucket struct {
	window int64
	value  int64
}

// tokenWindowStore keeps the in-memory fallback counters. A stale window is
// replaced on access, and previous windows are swept at most once per window so
// idle tokens do not accumulate entries in a long-running process. The zero
// lastSweptWindow makes the first write sweep as well.
type tokenWindowStore struct {
	mutex           sync.Mutex
	buckets         map[string]tokenWindowBucket
	lastSweptWindow int64
}

var tokenWindows = &tokenWindowStore{buckets: make(map[string]tokenWindowBucket)}

func (s *tokenWindowStore) add(name string, window int64, delta int64) int64 {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.sweepLocked(window)
	bucket := s.buckets[name]
	if bucket.window != window {
		bucket = tokenWindowBucket{window: window}
	}
	bucket.value += delta
	s.buckets[name] = bucket
	return bucket.value
}

func (s *tokenWindowStore) current(name string, window int64) int64 {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if bucket, ok := s.buckets[name]; ok && bucket.window == window {
		return bucket.value
	}
	return 0
}

func (s *tokenWindowStore) sweepLocked(window int64) {
	if window-s.lastSweptWindow < tokenRateWindowSeconds {
		return
	}
	s.lastSweptWindow = window
	for name, bucket := range s.buckets {
		if bucket.window < window {
			delete(s.buckets, name)
		}
	}
}
