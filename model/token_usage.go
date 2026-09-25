package model

import (
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// TokenConsumeRecorder observes every consumption fact written to the log
// table. The service layer installs it at startup so per-token rate limits and
// spending caps can update their counters without model importing service.
// It must be fast and non-blocking; implementations are expected to update
// counters asynchronously.
var TokenConsumeRecorder func(tokenId int, quota int, promptTokens int, completionTokens int)

// TokenUsageTotals aggregates consumption for one token over a time range.
type TokenUsageTotals struct {
	Requests         int64 `json:"requests"`
	Quota            int64 `json:"quota"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// TokenUsageDay is one UTC day of a token's consumption. DayStart is the UTC
// midnight timestamp; Date is the same day formatted for API responses.
type TokenUsageDay struct {
	TokenUsageTotals
	DayStart int64  `json:"day_start"`
	Date     string `json:"date"`
}

// TokenUsageModel is one model's contribution to a token's consumption.
type TokenUsageModel struct {
	TokenUsageTotals
	ModelName string `json:"model_name"`
}

// tokenUsageSelectColumns lists the aggregate expressions shared by the
// per-token usage queries so totals, daily buckets, and model breakdowns stay
// consistent.
const tokenUsageSelectColumns = "count(*) AS requests, " +
	"COALESCE(sum(quota), 0) AS quota, " +
	"COALESCE(sum(prompt_tokens), 0) AS prompt_tokens, " +
	"COALESCE(sum(completion_tokens), 0) AS completion_tokens, " +
	"COALESCE(sum(prompt_tokens), 0) + COALESCE(sum(completion_tokens), 0) AS total_tokens"

// tokenUsageDayBucketExpr buckets created_at into UTC days. Integer division is
// exact on SQLite and PostgreSQL; MySQL and ClickHouse divide into decimals, so
// they need an explicit FLOOR.
func tokenUsageDayBucketExpr() string {
	if common.UsingLogDatabase(common.DatabaseTypeMySQL) || common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		return "FLOOR(created_at / 86400) * 86400"
	}
	return "(created_at / 86400) * 86400"
}

func tokenUsageBaseQuery(tokenId int, userId int, startTimestamp int64, endTimestamp int64) *gorm.DB {
	return LOG_DB.Table("logs").
		Where("token_id = ? AND user_id = ? AND type = ?", tokenId, userId, LogTypeConsume).
		Where("created_at >= ? AND created_at <= ?", startTimestamp, endTimestamp)
}

// SumTokenConsumeQuotaSince returns the quota a token consumed since the given
// timestamp. Spending cap enforcement reads this on a cache miss, so it stays a
// single indexed aggregate instead of reusing the richer analytics queries.
func SumTokenConsumeQuotaSince(tokenId int, since int64) (int64, error) {
	var total int64
	err := LOG_DB.Table("logs").
		Select("COALESCE(sum(quota), 0)").
		Where("token_id = ? AND type = ? AND created_at >= ?", tokenId, LogTypeConsume, since).
		Scan(&total).Error
	if err != nil {
		return 0, fmt.Errorf("sum token consume quota: %w", err)
	}
	return total, nil
}

// GetTokenUsageTotals aggregates a token's consumption over a time range.
func GetTokenUsageTotals(tokenId int, userId int, startTimestamp int64, endTimestamp int64) (TokenUsageTotals, error) {
	var totals TokenUsageTotals
	err := tokenUsageBaseQuery(tokenId, userId, startTimestamp, endTimestamp).
		Select(tokenUsageSelectColumns).
		Scan(&totals).Error
	if err != nil {
		return totals, fmt.Errorf("query token usage totals: %w", err)
	}
	return totals, nil
}

// GetTokenUsageDaily breaks a token's consumption into UTC days, oldest first.
// Days without traffic are absent; callers that need a dense series fill gaps.
func GetTokenUsageDaily(tokenId int, userId int, startTimestamp int64, endTimestamp int64) ([]TokenUsageDay, error) {
	bucketExpr := tokenUsageDayBucketExpr()
	days := make([]TokenUsageDay, 0)
	err := tokenUsageBaseQuery(tokenId, userId, startTimestamp, endTimestamp).
		Select(fmt.Sprintf("%s, %s AS day_start", tokenUsageSelectColumns, bucketExpr)).
		Group(bucketExpr).
		Order("day_start ASC").
		Find(&days).Error
	if err != nil {
		return nil, fmt.Errorf("query token usage daily: %w", err)
	}
	for i := range days {
		days[i].Date = time.Unix(days[i].DayStart, 0).UTC().Format("2006-01-02")
	}
	return days, nil
}

// GetTokenTopModels ranks the models a token used by consumed quota.
func GetTokenTopModels(tokenId int, userId int, startTimestamp int64, endTimestamp int64, limit int) ([]TokenUsageModel, error) {
	if limit <= 0 {
		limit = 10
	}
	models := make([]TokenUsageModel, 0)
	err := tokenUsageBaseQuery(tokenId, userId, startTimestamp, endTimestamp).
		Select(fmt.Sprintf("%s, model_name", tokenUsageSelectColumns)).
		Where("model_name <> ''").
		Group("model_name").
		Order("quota DESC").
		Limit(limit).
		Find(&models).Error
	if err != nil {
		return nil, fmt.Errorf("query token top models: %w", err)
	}
	return models, nil
}
