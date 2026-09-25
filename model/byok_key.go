package model

import (
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/common/idgen"
	"github.com/QuantumNous/new-api/constant"

	"gorm.io/gorm"
)

// BYOK (bring your own key) lets a customer bind their own upstream API key and
// have matching requests routed through it. The gateway then charges a platform
// fee instead of the full managed-channel price.
//
// Security notes that apply to every function in this file:
//   - EncryptedKey is `json:"-"`; the plaintext credential is never persisted,
//     never logged, and never leaves the process except inside an upstream
//     request. Decrypt with common.ByokDecryptSecret only at the point of use.
//   - Every read and write is scoped by user id. A caller must never pass a
//     user id taken from the request body; it comes from the authenticated
//     session (see controller/byok.go).
//   - Deletion is a hard delete, which crypto-shreds the key: the ciphertext
//     row is gone and the per-user HKDF derivation makes the remaining
//     ciphertexts of other users useless.
//
// Identifiers use common/idgen snowflake values. They MUST be assigned after
// the struct literal, because a zero primary key in the literal makes GORM
// treat the field as unset and fall back to the driver default.

// ErrByokKeyNotFound is returned both for an id that does not exist and for an
// id that belongs to another user. The two cases are deliberately
// indistinguishable at the API boundary so key ids cannot be probed.
var ErrByokKeyNotFound = errors.New("byok key not found")

// ByokKey stores a user's encrypted API key for a provider.
type ByokKey struct {
	BaseModel
	UserId       int64      `json:"user_id,string" gorm:"index;not null"`
	Provider     string     `json:"provider" gorm:"size:50;index;not null"` // openai, anthropic, google, mistral, custom
	Label        string     `json:"label" gorm:"size:100"`
	EncryptedKey string     `json:"-" gorm:"type:text;not null"` // AES-256-GCM ciphertext, base64
	KeyHint      string     `json:"key_hint" gorm:"size:10"`     // last 4 chars for display
	Status       int        `json:"status" gorm:"default:1"`     // 1=active, 0=disabled, -1=invalid
	LastUsedAt   *time.Time `json:"last_used_at"`
	LastError    string     `json:"last_error" gorm:"size:500"`
	UsageCount   int64      `json:"usage_count" gorm:"default:0"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// ByokUsage tracks per-request usage for BYOK routing.
type ByokUsage struct {
	BaseModel
	ByokKeyId        int64     `json:"byok_key_id,string" gorm:"index;not null"`
	UserId           int64     `json:"user_id,string" gorm:"index;not null"`
	ModelName        string    `json:"model_name" gorm:"size:100;index"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	PlatformFee      int64     `json:"platform_fee"`  // quota charged (5% of notional cost)
	NotionalCost     int64     `json:"notional_cost"` // what it would have cost via managed
	LatencyMs        int64     `json:"latency_ms"`
	Success          bool      `json:"success"`
	CreatedAt        time.Time `json:"created_at" gorm:"index"`
}

// TableName pins the usage table to the singular name used by the BYOK
// reporting queries and dashboards; the key table keeps GORM's plural default.
func (ByokUsage) TableName() string {
	return "byok_usage"
}

// IsActive reports whether the key is eligible to serve a request.
func (k *ByokKey) IsActive() bool {
	return k.Status == constant.ByokKeyStatusActive
}

// MaskedHint renders the stored trailing characters for display. A record
// created before a hint was captured renders as an empty mask rather than
// exposing anything derived from the ciphertext.
func (k *ByokKey) MaskedHint() string {
	if k.KeyHint == "" {
		return ""
	}
	return "..." + k.KeyHint
}

// SetSecret encrypts an upstream credential for this key's owner and refreshes
// the display hint. It must be called after UserId is populated, because the
// derived encryption key and the GCM additional data are both bound to it.
func (k *ByokKey) SetSecret(secret string) error {
	if k.UserId <= 0 {
		return errors.New("byok key must have an owner before a secret is set")
	}
	encrypted, err := common.ByokEncryptSecret(k.UserId, secret)
	if err != nil {
		return err
	}
	hint, err := common.ByokKeyHint(secret)
	if err != nil {
		return err
	}
	k.EncryptedKey = encrypted
	k.KeyHint = hint
	return nil
}

// Secret decrypts the stored credential. The result is sensitive: use it only
// to build the upstream request, and never log or return it.
func (k *ByokKey) Secret() (string, error) {
	return common.ByokDecryptSecret(k.UserId, k.EncryptedKey)
}

// Insert persists a new BYOK key with an application-generated identifier.
func InsertByokKey(key *ByokKey) error {
	if key.UserId <= 0 {
		return errors.New("byok key must belong to a user")
	}
	if !constant.IsByokProvider(key.Provider) {
		return fmt.Errorf("unsupported byok provider: %s", key.Provider)
	}
	if key.EncryptedKey == "" {
		return errors.New("byok key is missing its encrypted secret")
	}
	if key.Status == 0 {
		key.Status = constant.ByokKeyStatusActive
	}
	key.ID = idgen.Next()
	return DB.Create(key).Error
}

// GetByokKeysByUserId lists every key owned by a user, newest first.
func GetByokKeysByUserId(userId int64) ([]*ByokKey, error) {
	var keys []*ByokKey
	err := DB.Where("user_id = ?", userId).Order("created_at DESC, id DESC").Find(&keys).Error
	return keys, err
}

// GetByokKeyByIdAndUser loads one key, scoping the lookup by owner so an id
// belonging to another user is reported as missing.
func GetByokKeyByIdAndUser(id int64, userId int64) (*ByokKey, error) {
	var key ByokKey
	err := DB.Where("id = ? AND user_id = ?", id, userId).First(&key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrByokKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	return &key, nil
}

// UpdateByokKeySecret replaces the stored credential and clears the last
// failure, because the previous error described a key that no longer exists.
func UpdateByokKeySecret(id int64, userId int64, secret string) (*ByokKey, error) {
	key, err := GetByokKeyByIdAndUser(id, userId)
	if err != nil {
		return nil, err
	}
	if err := key.SetSecret(secret); err != nil {
		return nil, err
	}
	updates := map[string]any{
		"encrypted_key": key.EncryptedKey,
		"key_hint":      key.KeyHint,
		"last_error":    "",
	}
	// A key the upstream rejected is re-armed on rotation; a key the user
	// disabled stays disabled, because that was an explicit choice.
	if key.Status == constant.ByokKeyStatusInvalid {
		updates["status"] = constant.ByokKeyStatusActive
	}
	if err := DB.Model(&ByokKey{}).Where("id = ? AND user_id = ?", id, userId).Updates(updates).Error; err != nil {
		return nil, err
	}
	return GetByokKeyByIdAndUser(id, userId)
}

// UpdateByokKeyLabel renames a key without touching its credential.
func UpdateByokKeyLabel(id int64, userId int64, label string) (*ByokKey, error) {
	err := DB.Model(&ByokKey{}).
		Where("id = ? AND user_id = ?", id, userId).
		Update("label", label).Error
	if err != nil {
		return nil, err
	}
	return GetByokKeyByIdAndUser(id, userId)
}

// UpdateByokKeyStatus records the outcome of an upstream validation. The
// error text is truncated to the column width so a verbose upstream response
// cannot fail the write.
func UpdateByokKeyStatus(id int64, userId int64, status int, lastError string) error {
	if len(lastError) > 500 {
		lastError = lastError[:500]
	}
	return DB.Model(&ByokKey{}).
		Where("id = ? AND user_id = ?", id, userId).
		Updates(map[string]any{"status": status, "last_error": lastError}).Error
}

// DeleteByokKeyByIdAndUser hard-deletes a key so its ciphertext is destroyed
// rather than merely orphaned.
func DeleteByokKeyByIdAndUser(id int64, userId int64) error {
	result := DB.Where("id = ? AND user_id = ?", id, userId).Delete(&ByokKey{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrByokKeyNotFound
	}
	return nil
}

// GetActiveByokKeysByUserProvider returns every routable key a user holds for
// a provider. service.FindMatchingByokKey picks one of these.
func GetActiveByokKeysByUserProvider(userId int64, provider string) ([]*ByokKey, error) {
	var keys []*ByokKey
	err := DB.
		Where("user_id = ? AND provider = ? AND status = ?", userId, provider, constant.ByokKeyStatusActive).
		Order("id ASC").
		Find(&keys).Error
	return keys, err
}

// HasActiveByokKey is the cheap relay-path gate in front of BYOK routing.
// Provider detection and key selection cost several queries, so the relay asks
// this first and skips BYOK entirely for the overwhelming majority of users,
// who hold no key at all. It reads one indexed row and stops.
func HasActiveByokKey(userId int64) (bool, error) {
	if userId <= 0 {
		return false, nil
	}
	var ids []int64
	err := DB.Model(&ByokKey{}).
		Where("user_id = ? AND status = ?", userId, constant.ByokKeyStatusActive).
		Limit(1).
		Pluck("id", &ids).Error
	if err != nil {
		return false, err
	}
	return len(ids) > 0, nil
}

// TouchByokKeyUsage marks a key as used. It is called from BYOK settlement
// (service.ByokRoute.RecordSuccess), not from key selection, so a routed but
// failed request does not advance the rotation.
func TouchByokKeyUsage(id int64, usedAt time.Time) error {
	return DB.Model(&ByokKey{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"last_used_at": usedAt,
			"usage_count":  gorm.Expr("usage_count + 1"),
		}).Error
}

// InsertByokUsage records one routed request.
func InsertByokUsage(usage *ByokUsage) error {
	if usage.UserId <= 0 || usage.ByokKeyId == 0 {
		return errors.New("byok usage must reference a key and its owner")
	}
	usage.ID = idgen.Next()
	return DB.Create(usage).Error
}

// ByokUsageGroup is one row of an aggregated BYOK usage report.
type ByokUsageGroup struct {
	ModelName        string `json:"model_name" gorm:"column:model_name"`
	ByokKeyId        int64  `json:"byok_key_id,string" gorm:"column:byok_key_id"`
	RequestCount     int64  `json:"request_count" gorm:"column:request_count"`
	SuccessCount     int64  `json:"success_count" gorm:"column:success_count"`
	PromptTokens     int64  `json:"prompt_tokens" gorm:"column:prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens" gorm:"column:completion_tokens"`
	PlatformFee      int64  `json:"platform_fee" gorm:"column:platform_fee"`
	NotionalCost     int64  `json:"notional_cost" gorm:"column:notional_cost"`
}

// byokUsageGroupColumn maps an allowed group_by value to the column it
// aggregates on. Only columns that exist in every supported dialect and need
// no date arithmetic are offered; per-day bucketing would require a
// dialect-specific date cast and is intentionally not exposed here.
const (
	ByokUsageGroupByModel = "model"
	ByokUsageGroupByKey   = "key"
)

// GetByokUsageAggregates sums a user's BYOK usage since the given cutoff,
// grouped by model name or by key.
//
// Only the grouped column appears in the select list beside the aggregates.
// MySQL runs with ONLY_FULL_GROUP_BY by default since 5.7.5 and rejects a
// select list carrying a non-aggregated column that is absent from GROUP BY;
// the field that is not grouped on therefore stays zero in the returned rows.
// COALESCE keeps empty groups at zero so the aggregates scan into int64 on
// every supported engine, and the success count uses a standard CASE
// expression because SUM(boolean) is not valid in PostgreSQL.
func GetByokUsageAggregates(userId int64, since time.Time, groupBy string) ([]ByokUsageGroup, error) {
	groupColumn := "model_name"
	if groupBy == ByokUsageGroupByKey {
		groupColumn = "byok_key_id"
	}

	var groups []ByokUsageGroup
	err := DB.Model(&ByokUsage{}).
		Select(
			groupColumn+", "+
				"COUNT(*) AS request_count, "+
				"COALESCE(SUM(CASE WHEN success THEN 1 ELSE 0 END), 0) AS success_count, "+
				"COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens, "+
				"COALESCE(SUM(completion_tokens), 0) AS completion_tokens, "+
				"COALESCE(SUM(platform_fee), 0) AS platform_fee, "+
				"COALESCE(SUM(notional_cost), 0) AS notional_cost",
		).
		Where("user_id = ? AND created_at >= ?", userId, since).
		Group(groupColumn).
		Order(groupColumn + " ASC").
		Scan(&groups).Error
	return groups, err
}
