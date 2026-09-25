package controller

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	hosttypes "github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// BYOK key management API.
//
// Every handler here is mounted behind middleware.UserAuth() and derives the
// owner from the authenticated session (c.GetInt("id")). A user id is never
// read from the request body or a query parameter, and every model call is
// scoped by that owner so one user cannot read, rename, test, or delete
// another user's key.
//
// The plaintext credential exists only between ShouldBindJSON and
// ByokKey.SetSecret. It is never written to a log, an audit record, or a
// response; the API surface exposes at most the trailing characters captured
// in key_hint.

const (
	byokLabelMaxLength   = 100
	byokUsageDefaultDays = 7
	byokUsageMaxDays     = 365
)

// byokKeyRequest is the create/update payload. Key and Label are pointers on
// update so an absent field is distinguishable from an explicitly empty one:
// a PUT that omits "key" must leave the stored credential untouched.
type byokKeyRequest struct {
	Provider string  `json:"provider"`
	Key      *string `json:"key"`
	Label    *string `json:"label"`
}

// byokKeyView is the API representation of a stored key. It is built field by
// field on purpose: returning the model struct directly would put the
// ciphertext one forgotten json tag away from a customer's credential.
type byokKeyView struct {
	Id         hosttypes.FlexInt64 `json:"id"`
	Provider   string              `json:"provider"`
	Label      string              `json:"label"`
	KeyHint    string              `json:"key_hint"`
	Status     int                 `json:"status"`
	LastError  string              `json:"last_error,omitempty"`
	UsageCount int64               `json:"usage_count"`
	LastUsedAt *time.Time          `json:"last_used_at"`
	CreatedAt  time.Time           `json:"created_at"`
	UpdatedAt  time.Time           `json:"updated_at"`
}

func newByokKeyView(key *model.ByokKey) byokKeyView {
	return byokKeyView{
		Id:         hosttypes.NewFlexInt64(key.ID),
		Provider:   key.Provider,
		Label:      key.Label,
		KeyHint:    key.MaskedHint(),
		Status:     key.Status,
		LastError:  key.LastError,
		UsageCount: key.UsageCount,
		LastUsedAt: key.LastUsedAt,
		CreatedAt:  key.CreatedAt,
		UpdatedAt:  key.UpdatedAt,
	}
}

// byokUsageGroupView and byokUsageSummary shape the usage report. The key id is
// a FlexInt64 so a snowflake identifier survives the JavaScript number
// boundary exactly.
type byokUsageGroupView struct {
	ModelName        string              `json:"model_name"`
	ByokKeyId        hosttypes.FlexInt64 `json:"byok_key_id"`
	RequestCount     int64               `json:"request_count"`
	SuccessCount     int64               `json:"success_count"`
	PromptTokens     int64               `json:"prompt_tokens"`
	CompletionTokens int64               `json:"completion_tokens"`
	PlatformFee      int64               `json:"platform_fee"`
	NotionalCost     int64               `json:"notional_cost"`
}

type byokUsageSummary struct {
	RequestCount     int64 `json:"request_count"`
	SuccessCount     int64 `json:"success_count"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	PlatformFee      int64 `json:"platform_fee"`
	NotionalCost     int64 `json:"notional_cost"`
}

type byokUsageResponse struct {
	Days    int                  `json:"days"`
	GroupBy string               `json:"group_by"`
	Summary byokUsageSummary     `json:"summary"`
	Groups  []byokUsageGroupView `json:"groups"`
}

// AddByokKey stores a new encrypted credential for the authenticated user.
func AddByokKey(c *gin.Context) {
	userId := int64(c.GetInt("id"))
	if userId <= 0 {
		common.ApiErrorMsg(c, "unauthenticated")
		return
	}

	var request byokKeyRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		common.ApiError(c, err)
		return
	}

	provider := strings.ToLower(strings.TrimSpace(request.Provider))
	if !constant.IsByokProvider(provider) {
		common.ApiErrorMsg(c, "unsupported provider, expected one of: "+strings.Join(constant.ByokProviders, ", "))
		return
	}
	secret := ""
	if request.Key != nil {
		secret = *request.Key
	}
	if err := service.ValidateByokKeyFormat(provider, secret); err != nil {
		common.ApiError(c, err)
		return
	}
	label, err := normalizeByokLabel(request.Label)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	key := model.ByokKey{
		UserId:   userId,
		Provider: provider,
		Label:    label,
		Status:   constant.ByokKeyStatusActive,
	}
	// The owner must be set before the secret, because the derived encryption
	// key and the GCM additional data are both bound to it.
	if err := key.SetSecret(secret); err != nil {
		common.ApiError(c, err)
		return
	}
	if err := model.InsertByokKey(&key); err != nil {
		common.ApiError(c, err)
		return
	}

	common.ApiSuccess(c, newByokKeyView(&key))
}

// GetByokKeys lists the authenticated user's keys.
func GetByokKeys(c *gin.Context) {
	userId := int64(c.GetInt("id"))
	if userId <= 0 {
		common.ApiErrorMsg(c, "unauthenticated")
		return
	}
	keys, err := model.GetByokKeysByUserId(userId)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	views := make([]byokKeyView, 0, len(keys))
	for _, key := range keys {
		views = append(views, newByokKeyView(key))
	}
	common.ApiSuccess(c, views)
}

// UpdateByokKey rotates the credential, renames the key, or both. Rotating a
// key the upstream had rejected re-arms it, since the previous failure
// described a credential that no longer exists.
func UpdateByokKey(c *gin.Context) {
	userId := int64(c.GetInt("id"))
	if userId <= 0 {
		common.ApiErrorMsg(c, "unauthenticated")
		return
	}
	keyId, err := parseByokKeyId(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	var request byokKeyRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		common.ApiError(c, err)
		return
	}
	if request.Key == nil && request.Label == nil {
		common.ApiErrorMsg(c, "nothing to update: provide \"key\" or \"label\"")
		return
	}

	existing, err := model.GetByokKeyByIdAndUser(keyId, userId)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	updated := existing
	if request.Key != nil {
		if err := service.ValidateByokKeyFormat(existing.Provider, *request.Key); err != nil {
			common.ApiError(c, err)
			return
		}
		updated, err = model.UpdateByokKeySecret(keyId, userId, *request.Key)
		if err != nil {
			common.ApiError(c, err)
			return
		}
	}
	if request.Label != nil {
		label, err := normalizeByokLabel(request.Label)
		if err != nil {
			common.ApiError(c, err)
			return
		}
		updated, err = model.UpdateByokKeyLabel(keyId, userId, label)
		if err != nil {
			common.ApiError(c, err)
			return
		}
	}

	common.ApiSuccess(c, newByokKeyView(updated))
}

// DeleteByokKey hard-deletes a key. The row holds the only copy of the
// ciphertext, so removing it crypto-shreds the credential.
func DeleteByokKey(c *gin.Context) {
	userId := int64(c.GetInt("id"))
	if userId <= 0 {
		common.ApiErrorMsg(c, "unauthenticated")
		return
	}
	keyId, err := parseByokKeyId(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if err := model.DeleteByokKeyByIdAndUser(keyId, userId); err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}

// GetByokUsage reports the authenticated user's BYOK activity, aggregated by
// model name or by key.
func GetByokUsage(c *gin.Context) {
	userId := int64(c.GetInt("id"))
	if userId <= 0 {
		common.ApiErrorMsg(c, "unauthenticated")
		return
	}

	days := byokUsageDefaultDays
	if raw := strings.TrimSpace(c.Query("days")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			common.ApiErrorMsg(c, "days must be a positive integer")
			return
		}
		days = min(parsed, byokUsageMaxDays)
	}

	groupBy := strings.ToLower(strings.TrimSpace(c.DefaultQuery("group_by", model.ByokUsageGroupByModel)))
	if groupBy != model.ByokUsageGroupByModel && groupBy != model.ByokUsageGroupByKey {
		common.ApiErrorMsg(c, "group_by must be \"model\" or \"key\"")
		return
	}

	since := time.Now().AddDate(0, 0, -days)
	aggregates, err := model.GetByokUsageAggregates(userId, since, groupBy)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	response := byokUsageResponse{
		Days:    days,
		GroupBy: groupBy,
		Groups:  make([]byokUsageGroupView, 0, len(aggregates)),
	}
	for _, aggregate := range aggregates {
		response.Groups = append(response.Groups, byokUsageGroupView{
			ModelName:        aggregate.ModelName,
			ByokKeyId:        hosttypes.NewFlexInt64(aggregate.ByokKeyId),
			RequestCount:     aggregate.RequestCount,
			SuccessCount:     aggregate.SuccessCount,
			PromptTokens:     aggregate.PromptTokens,
			CompletionTokens: aggregate.CompletionTokens,
			PlatformFee:      aggregate.PlatformFee,
			NotionalCost:     aggregate.NotionalCost,
		})
		response.Summary.RequestCount += aggregate.RequestCount
		response.Summary.SuccessCount += aggregate.SuccessCount
		response.Summary.PromptTokens += aggregate.PromptTokens
		response.Summary.CompletionTokens += aggregate.CompletionTokens
		response.Summary.PlatformFee += aggregate.PlatformFee
		response.Summary.NotionalCost += aggregate.NotionalCost
	}

	common.ApiSuccess(c, response)
}

// TestByokKey validates a stored credential against the provider and records
// the verdict on the key.
//
// Only an authoritative rejection (HTTP 401/403) marks the key invalid. A
// timeout, rate limit, or upstream outage is reported as inconclusive and
// leaves the status alone, so a provider incident cannot disable a working
// customer key.
func TestByokKey(c *gin.Context) {
	userId := int64(c.GetInt("id"))
	if userId <= 0 {
		common.ApiErrorMsg(c, "unauthenticated")
		return
	}
	keyId, err := parseByokKeyId(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	key, err := model.GetByokKeyByIdAndUser(keyId, userId)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	// The decrypted secret is held in a local only for the duration of the
	// probe. It is never logged and never returned; clearing the reference
	// afterwards just keeps it out of the rest of the handler's scope.
	secret, err := key.Secret()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	result := service.ProbeByokKey(c.Request.Context(), key.Provider, secret)
	secret = ""

	// Only an authoritative rejection changes the stored status. An
	// inconclusive probe leaves it exactly as it was.
	status := key.Status
	lastError := key.LastError
	switch result.Outcome {
	case service.ByokProbeValid:
		status, lastError = constant.ByokKeyStatusActive, ""
	case service.ByokProbeInvalid:
		status, lastError = constant.ByokKeyStatusInvalid, result.Reason
	case service.ByokProbeInconclusive:
		// Keep the previous status and error.
	default:
		common.ApiErrorMsg(c, "unexpected validation outcome")
		return
	}
	if status != key.Status || lastError != key.LastError {
		if err := model.UpdateByokKeyStatus(keyId, userId, status, lastError); err != nil {
			common.ApiError(c, err)
			return
		}
	}

	common.ApiSuccess(c, gin.H{
		"id":      hosttypes.NewFlexInt64(keyId),
		"outcome": result.Outcome,
		"status":  status,
		"message": result.Reason,
	})
}

// byokUserFeeRequest carries an admin's per-user fee override. The pointer
// field makes an explicit 0 (a fee-free user) distinguishable from an omitted
// one, which a value field could not.
type byokUserFeeRequest struct {
	ByokFeeOverride *float64 `json:"byok_fee_override"`
}

// AdminSetUserByokFee stores a per-user override of the BYOK platform fee.
// Sending -1 clears the override so the user inherits the global
// ByokFeePercent option again.
func AdminSetUserByokFee(c *gin.Context) {
	targetId, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || targetId <= 0 {
		common.ApiErrorMsg(c, "invalid user id")
		return
	}

	var request byokUserFeeRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		common.ApiError(c, err)
		return
	}
	if request.ByokFeeOverride == nil {
		common.ApiErrorMsg(c, "byok_fee_override is required, use -1 to inherit the global fee")
		return
	}

	override := *request.ByokFeeOverride
	if err := model.UpdateUserByokFeeOverride(targetId, override); err != nil {
		common.ApiError(c, err)
		return
	}

	effective, err := service.ResolveByokFeePercent(targetId)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{
		"user_id":               targetId,
		"byok_fee_override":     override,
		"effective_fee_percent": effective,
	})
}

// parseByokKeyId reads the :id path parameter. Only a decimal integer is
// accepted, matching the string form the API emits for snowflake ids.
func parseByokKeyId(c *gin.Context) (int64, error) {
	raw := strings.TrimSpace(c.Param("id"))
	if raw == "" {
		return 0, errors.New("missing key id")
	}
	keyId, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || keyId <= 0 {
		return 0, errors.New("invalid key id")
	}
	return keyId, nil
}

// normalizeByokLabel trims a display label and enforces the column width, so a
// too-long value is rejected here instead of failing the write on MySQL.
func normalizeByokLabel(label *string) (string, error) {
	if label == nil {
		return "", nil
	}
	trimmed := strings.TrimSpace(*label)
	if len([]rune(trimmed)) > byokLabelMaxLength {
		return "", errors.New("label must be at most 100 characters")
	}
	return trimmed, nil
}
