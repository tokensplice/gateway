package service

import (
	"context"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tokenLimitNow is a fixed instant inside one UTC minute, day, and month so the
// per-token window and cap assertions stay deterministic.
var tokenLimitNow = time.Date(2026, 9, 25, 12, 30, 15, 0, time.UTC)

func seedTokenConsumeLog(t *testing.T, tokenId int, userId int, createdAt int64, quota int, promptTokens int, completionTokens int, modelName string) {
	t.Helper()
	require.NoError(t, model.DB.Create(&model.Log{
		UserId:           userId,
		TokenId:          tokenId,
		Type:             model.LogTypeConsume,
		CreatedAt:        createdAt,
		Quota:            quota,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		ModelName:        modelName,
	}).Error)
}

func TestCheckTokenRateLimitEnforcesPerTokenMinuteWindows(t *testing.T) {
	ctx := context.Background()

	t.Run("unlimited token is never throttled", func(t *testing.T) {
		token := &model.Token{Id: 910001}
		for range 5 {
			assert.True(t, checkTokenRateLimit(ctx, token, tokenLimitNow).Allowed)
		}
		assert.Zero(t, tokenWindows.current(tokenWindowName(tokenRateRPMKeyPrefix, token.Id), tokenLimitNow.Unix()/tokenRateWindowSeconds))
	})

	t.Run("token rpm limit rejects the request above the limit", func(t *testing.T) {
		token := &model.Token{Id: 910002, RateLimitRPM: 2}
		require.True(t, checkTokenRateLimit(ctx, token, tokenLimitNow).Allowed)
		require.True(t, checkTokenRateLimit(ctx, token, tokenLimitNow).Allowed)

		decision := checkTokenRateLimit(ctx, token, tokenLimitNow)
		require.False(t, decision.Allowed)
		assert.Equal(t, TokenRateLimitRPM, decision.Kind)
		assert.Equal(t, 2, decision.Limit)
		assert.EqualValues(t, 3, decision.Current)
		assert.EqualValues(t, 45, decision.RetryAfterSeconds, "retry after the current minute bucket ends")

		// The next minute bucket starts a fresh window.
		nextWindow := tokenLimitNow.Add(time.Minute)
		require.True(t, checkTokenRateLimit(ctx, token, nextWindow).Allowed)
	})

	t.Run("deployment default applies when the token has no rpm limit", func(t *testing.T) {
		constant.TokenDefaultRateLimitRPM = 1
		t.Cleanup(func() { constant.TokenDefaultRateLimitRPM = 0 })

		token := &model.Token{Id: 910003}
		require.True(t, checkTokenRateLimit(ctx, token, tokenLimitNow).Allowed)

		decision := checkTokenRateLimit(ctx, token, tokenLimitNow)
		require.False(t, decision.Allowed)
		assert.Equal(t, TokenRateLimitRPM, decision.Kind)
		assert.Equal(t, 1, decision.Limit)
	})

	t.Run("token tpm limit rejects once recorded usage reaches it", func(t *testing.T) {
		token := &model.Token{Id: 910004, RateLimitTPM: 100}
		require.True(t, checkTokenRateLimit(ctx, token, tokenLimitNow).Allowed)

		recordTokenTokens(ctx, token.Id, 60, tokenLimitNow)
		require.True(t, checkTokenRateLimit(ctx, token, tokenLimitNow).Allowed)

		recordTokenTokens(ctx, token.Id, 40, tokenLimitNow)
		decision := checkTokenRateLimit(ctx, token, tokenLimitNow)
		require.False(t, decision.Allowed)
		assert.Equal(t, TokenRateLimitTPM, decision.Kind)
		assert.Equal(t, 100, decision.Limit)
		assert.EqualValues(t, 100, decision.Current)
		assert.EqualValues(t, 45, decision.RetryAfterSeconds)

		// A new minute window forgets the previous window's tokens.
		assert.True(t, checkTokenRateLimit(ctx, token, tokenLimitNow.Add(time.Minute)).Allowed)
	})
}

func TestCheckTokenSpendingCapsEnforcesDailyAndMonthlyQuota(t *testing.T) {
	ctx := context.Background()
	dailyStart, dailyReset := dailySpendingPeriod(tokenLimitNow)
	monthlyStart, monthlyReset := monthlySpendingPeriod(tokenLimitNow)

	require.Equal(t, time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC).Unix(), dailyStart)
	require.Equal(t, time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC).Unix(), dailyReset)
	require.Equal(t, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Unix(), monthlyStart)
	require.Equal(t, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Unix(), monthlyReset)

	t.Run("caps compare against consume logs of their own period", func(t *testing.T) {
		truncate(t)
		const userId = 4242
		const tokenId = 920001
		seedTokenConsumeLog(t, tokenId, userId, dailyStart+9*3600, 300, 10, 5, "gpt-a")
		seedTokenConsumeLog(t, tokenId, userId, dailyStart+11*3600, 200, 10, 5, "gpt-a")
		// Earlier the same month: counts monthly, not daily.
		seedTokenConsumeLog(t, tokenId, userId, dailyStart-5*24*3600, 100, 10, 5, "gpt-a")

		tests := []struct {
			name       string
			dailyCap   int64
			monthlyCap int64
			wantKind   SpendingCapKind
			wantUsed   int64
			wantReset  int64
		}{
			{name: "no caps", dailyCap: 0, monthlyCap: 0, wantKind: ""},
			{name: "daily below cap", dailyCap: 501, monthlyCap: 0, wantKind: ""},
			{name: "daily at cap", dailyCap: 500, monthlyCap: 0, wantKind: SpendingCapDaily, wantUsed: 500, wantReset: dailyReset},
			{name: "monthly below cap", dailyCap: 0, monthlyCap: 601, wantKind: ""},
			{name: "monthly at cap", dailyCap: 0, monthlyCap: 600, wantKind: SpendingCapMonthly, wantUsed: 600, wantReset: monthlyReset},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				token := &model.Token{Id: tokenId, DailySpendingCap: test.dailyCap, MonthlySpendingCap: test.monthlyCap}
				exceeded := checkTokenSpendingCaps(ctx, token, tokenLimitNow)
				if test.wantKind == "" {
					require.Nil(t, exceeded)
					return
				}
				require.NotNil(t, exceeded)
				assert.Equal(t, test.wantKind, exceeded.Kind)
				assert.EqualValues(t, test.wantUsed, exceeded.Used)
				assert.EqualValues(t, test.wantReset, exceeded.ResetAt)
			})
		}
	})

	t.Run("freshly logged consumption updates a warm counter", func(t *testing.T) {
		truncate(t)
		const userId = 4243
		const tokenId = 920002
		seedTokenConsumeLog(t, tokenId, userId, dailyStart+9*3600, 500, 10, 5, "gpt-a")

		token := &model.Token{Id: tokenId, DailySpendingCap: 1000, MonthlySpendingCap: 1000}
		require.Nil(t, checkTokenSpendingCaps(ctx, token, tokenLimitNow))

		addTokenSpending(ctx, tokenId, 500, tokenLimitNow)

		exceeded := checkTokenSpendingCaps(ctx, &model.Token{Id: tokenId, DailySpendingCap: 1000}, tokenLimitNow)
		require.NotNil(t, exceeded)
		assert.Equal(t, SpendingCapDaily, exceeded.Kind)
		assert.EqualValues(t, 1000, exceeded.Used)

		exceeded = checkTokenSpendingCaps(ctx, &model.Token{Id: tokenId, MonthlySpendingCap: 1000}, tokenLimitNow)
		require.NotNil(t, exceeded)
		assert.Equal(t, SpendingCapMonthly, exceeded.Kind)
		assert.EqualValues(t, 1000, exceeded.Used)
	})

	t.Run("counter is rebuilt from the logs after the cache ttl", func(t *testing.T) {
		truncate(t)
		const userId = 4244
		const tokenId = 920003
		seedTokenConsumeLog(t, tokenId, userId, dailyStart+9*3600, 500, 10, 5, "gpt-a")

		token := &model.Token{Id: tokenId, DailySpendingCap: 570}
		require.Nil(t, checkTokenSpendingCaps(ctx, token, tokenLimitNow), "500 of 570 is still under the cap")

		// Logged while the cached total was still fresh: the warm counter picks it
		// up immediately, and a later rebuild from the logs must not double count.
		seedTokenConsumeLog(t, tokenId, userId, dailyStart+9*3600, 70, 10, 5, "gpt-a")
		addTokenSpending(ctx, tokenId, 70, tokenLimitNow)
		exceeded := checkTokenSpendingCaps(ctx, token, tokenLimitNow)
		require.NotNil(t, exceeded)
		assert.EqualValues(t, 570, exceeded.Used)

		afterTTL := tokenLimitNow.Add(time.Duration(constant.TokenSpendingCacheSeconds+1) * time.Second)
		exceeded = checkTokenSpendingCaps(ctx, token, afterTTL)
		require.NotNil(t, exceeded)
		assert.EqualValues(t, 570, exceeded.Used, "rebuilt from the consume logs, not added to the stale cache")
	})

	t.Run("only consume logs of the token itself count", func(t *testing.T) {
		truncate(t)
		seedTokenConsumeLog(t, 920004, 4245, dailyStart+9*3600, 900, 10, 5, "gpt-a")
		seedTokenConsumeLog(t, 920005, 4246, dailyStart+9*3600, 900, 10, 5, "gpt-a")
		// Non-consume log types never count as spending.
		require.NoError(t, model.DB.Create(&model.Log{
			UserId: 4245, TokenId: 920004, Type: model.LogTypeTopup, CreatedAt: dailyStart + 9*3600, Quota: 5000,
		}).Error)

		token := &model.Token{Id: 920004, DailySpendingCap: 900}
		exceeded := checkTokenSpendingCaps(ctx, token, tokenLimitNow)
		require.NotNil(t, exceeded)
		assert.EqualValues(t, 900, exceeded.Used)
	})
}

func TestGetAllUserTokensFiltersByGroupName(t *testing.T) {
	truncate(t)
	require.NoError(t, model.DB.Create(&model.Token{Id: 930001, UserId: 51, Key: "group-a", Name: "a", GroupName: "production"}).Error)
	require.NoError(t, model.DB.Create(&model.Token{Id: 930002, UserId: 51, Key: "group-b", Name: "b", GroupName: "production"}).Error)
	require.NoError(t, model.DB.Create(&model.Token{Id: 930003, UserId: 51, Key: "group-c", Name: "c", GroupName: "staging"}).Error)
	require.NoError(t, model.DB.Create(&model.Token{Id: 930004, UserId: 51, Key: "group-d", Name: "d"}).Error)
	require.NoError(t, model.DB.Create(&model.Token{Id: 930005, UserId: 52, Key: "group-e", Name: "e", GroupName: "production"}).Error)
	require.NoError(t, model.DB.Create(&model.Token{Id: 930006, UserId: 51, Key: "group-f", Name: "f", GroupName: "production"}).Error)
	require.NoError(t, model.DB.Delete(&model.Token{Id: 930006}).Error)

	tokens, total, err := model.GetAllUserTokens(51, "", 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 4, total)
	assert.Len(t, tokens, 4, "the soft-deleted token stays out of the list")

	tokens, total, err = model.GetAllUserTokens(51, "production", 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 2, total)
	require.Len(t, tokens, 2)
	for _, token := range tokens {
		assert.Equal(t, "production", token.GroupName)
	}

	tokens, total, err = model.GetAllUserTokens(51, "staging", 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	require.Len(t, tokens, 1)
	assert.Equal(t, 930003, tokens[0].Id)

	tokens, total, err = model.GetAllUserTokens(51, "client-a", 0, 10)
	require.NoError(t, err)
	assert.Zero(t, total)
	assert.Empty(t, tokens)

	groups, err := model.GetUserTokenGroups(51)
	require.NoError(t, err)
	assert.Equal(t, []string{"production", "staging"}, groups)

	groups, err = model.GetUserTokenGroups(53)
	require.NoError(t, err)
	assert.Empty(t, groups)
}

func TestTokenUsageQueriesAggregateConsumeLogs(t *testing.T) {
	truncate(t)
	const userId = 61
	const tokenId = 940001
	dayOne := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC).Unix()
	dayTwo := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC).Unix()

	seedTokenConsumeLog(t, tokenId, userId, dayOne+3600, 120, 100, 20, "gpt-a")
	seedTokenConsumeLog(t, tokenId, userId, dayOne+7200, 80, 40, 10, "gpt-b")
	seedTokenConsumeLog(t, tokenId, userId, dayTwo+3600, 300, 200, 50, "gpt-a")
	// Another token's traffic and a log outside the queried range stay out.
	seedTokenConsumeLog(t, 940002, userId, dayTwo+3600, 999, 999, 999, "gpt-a")
	seedTokenConsumeLog(t, tokenId, userId, dayOne-10*24*3600, 999, 999, 999, "gpt-a")

	start := dayOne
	end := dayTwo + 86399

	totals, err := model.GetTokenUsageTotals(tokenId, userId, start, end)
	require.NoError(t, err)
	assert.EqualValues(t, 3, totals.Requests)
	assert.EqualValues(t, 500, totals.Quota)
	assert.EqualValues(t, 340, totals.PromptTokens)
	assert.EqualValues(t, 80, totals.CompletionTokens)
	assert.EqualValues(t, 420, totals.TotalTokens)

	daily, err := model.GetTokenUsageDaily(tokenId, userId, start, end)
	require.NoError(t, err)
	require.Len(t, daily, 2)
	assert.Equal(t, dayOne, daily[0].DayStart)
	assert.Equal(t, "2026-09-24", daily[0].Date)
	assert.EqualValues(t, 2, daily[0].Requests)
	assert.EqualValues(t, 200, daily[0].Quota)
	assert.Equal(t, dayTwo, daily[1].DayStart)
	assert.Equal(t, "2026-09-25", daily[1].Date)
	assert.EqualValues(t, 1, daily[1].Requests)
	assert.EqualValues(t, 300, daily[1].Quota)

	topModels, err := model.GetTokenTopModels(tokenId, userId, start, end, 10)
	require.NoError(t, err)
	require.Len(t, topModels, 2)
	assert.Equal(t, "gpt-a", topModels[0].ModelName)
	assert.EqualValues(t, 420, topModels[0].Quota)
	assert.EqualValues(t, 2, topModels[0].Requests)
	assert.Equal(t, "gpt-b", topModels[1].ModelName)
	assert.EqualValues(t, 80, topModels[1].Quota)

	limited, err := model.GetTokenTopModels(tokenId, userId, start, end, 1)
	require.NoError(t, err)
	require.Len(t, limited, 1)
	assert.Equal(t, "gpt-a", limited[0].ModelName)

	sum, err := model.SumTokenConsumeQuotaSince(tokenId, dayTwo)
	require.NoError(t, err)
	assert.EqualValues(t, 300, sum)
}

func TestTokenExpirationWarningCoversSevenDayThreshold(t *testing.T) {
	now := tokenLimitNow.Unix()

	tests := []struct {
		name        string
		expiredTime int64
		wantDays    int64
	}{
		{name: "never expires", expiredTime: -1, wantDays: 0},
		{name: "already expired", expiredTime: now - 60, wantDays: 0},
		{name: "twelve hours out rounds up to one day", expiredTime: now + 12*3600, wantDays: 1},
		{name: "three days out", expiredTime: now + 3*24*3600, wantDays: 3},
		{name: "exactly seven days out", expiredTime: now + 7*24*3600, wantDays: 7},
		{name: "just beyond the warning window", expiredTime: now + 7*24*3600 + 1, wantDays: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			token := &model.Token{Id: 1, ExpiredTime: test.expiredTime}
			assert.EqualValues(t, test.wantDays, token.ExpirationWarning(now))
		})
	}
}
