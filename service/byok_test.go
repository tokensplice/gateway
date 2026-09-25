package service

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	hosttypes "github.com/QuantumNous/new-api/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestCalculateByokFee pins the BYOK platform fee arithmetic. The invariants
// that matter are the billing ones from .agents/rules/billing.md: a fee can
// never be negative, an out-of-band percentage is clamped rather than trusted,
// rounding is half-away-from-zero, and an absurd notional cost saturates at
// the single-request int32 boundary instead of wrapping.
func TestCalculateByokFee(t *testing.T) {
	for _, tc := range []struct {
		name      string
		notional  int64
		percent   float64
		wantQuota int64
	}{
		{name: "five percent of one dollar of quota", notional: 500000, percent: 5, wantQuota: 25000},
		{name: "zero notional charges nothing", notional: 0, percent: 5, wantQuota: 0},
		{name: "negative notional never becomes a credit", notional: -500000, percent: 5, wantQuota: 0},
		{name: "zero percent is free", notional: 500000, percent: 0, wantQuota: 0},
		{name: "negative percent is clamped to zero", notional: 500000, percent: -3, wantQuota: 0},
		{name: "percent above the band is clamped to 20", notional: 500000, percent: 95, wantQuota: 100000},
		{name: "NaN percent falls back to the default", notional: 500000, percent: math.NaN(), wantQuota: 25000},
		{name: "positive infinity is clamped to 20", notional: 500000, percent: math.Inf(1), wantQuota: 100000},
		{name: "half a quota rounds away from zero", notional: 50, percent: 5, wantQuota: 3},
		{name: "below half a quota rounds to zero", notional: 9, percent: 5, wantQuota: 0},
		{name: "fractional percent", notional: 1000000, percent: 2.5, wantQuota: 25000},
		{name: "oversized notional saturates instead of wrapping", notional: math.MaxInt64, percent: 20, wantQuota: int64(common.MaxQuota)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantQuota, CalculateByokFee(tc.notional, tc.percent))
		})
	}

	t.Run("saturation is reported to the caller", func(t *testing.T) {
		fee, clamp := CalculateByokFeeChecked(math.MaxInt64, 20)
		assert.Equal(t, int64(common.MaxQuota), fee)
		require.NotNil(t, clamp)
		assert.Equal(t, common.QuotaClampOverflow, clamp.Kind)
	})

	t.Run("in-range fee reports no clamp", func(t *testing.T) {
		fee, clamp := CalculateByokFeeChecked(500000, 5)
		assert.Equal(t, int64(25000), fee)
		assert.Nil(t, clamp)
	})
}

// TestResolveByokFeePercent covers the per-user override precedence: an
// explicit override wins, -1 inherits the global option, and a stored value
// outside the band is clamped instead of trusted.
func TestResolveByokFeePercent(t *testing.T) {
	truncateByok(t)
	original := operation_setting.ByokFeePercent
	operation_setting.ByokFeePercent = 5
	t.Cleanup(func() { operation_setting.ByokFeePercent = original })

	seedUser(t, 4242, 0)
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", 4242).
		Update("byok_fee_override", operation_setting.ByokFeeNoOverride).Error)

	inherited, err := ResolveByokFeePercent(4242)
	require.NoError(t, err)
	assert.Equal(t, 5.0, inherited)

	require.NoError(t, model.UpdateUserByokFeeOverride(4242, 12.5))
	overridden, err := ResolveByokFeePercent(4242)
	require.NoError(t, err)
	assert.Equal(t, 12.5, overridden)

	// An explicit zero override must survive; Updates(struct) would have
	// dropped it as a zero value.
	require.NoError(t, model.UpdateUserByokFeeOverride(4242, 0))
	free, err := ResolveByokFeePercent(4242)
	require.NoError(t, err)
	assert.Equal(t, 0.0, free)

	require.NoError(t, model.UpdateUserByokFeeOverride(4242, operation_setting.ByokFeeNoOverride))
	backToGlobal, err := ResolveByokFeePercent(4242)
	require.NoError(t, err)
	assert.Equal(t, 5.0, backToGlobal)

	assert.Error(t, model.UpdateUserByokFeeOverride(4242, 55), "percent above the band must be rejected")
	assert.Error(t, model.UpdateUserByokFeeOverride(4242, -7), "a negative percent other than the sentinel must be rejected")
	_, err = model.GetUserByokFeeOverride(999999)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	assert.Error(t, func() error { _, err := ResolveByokFeePercent(0); return err }(), "a non-positive user id must be rejected")
}

// TestByokSecretEncryptionIsBoundToOwner is the credential-isolation regression:
// a ciphertext must only decrypt for the user it was sealed for, must fail
// closed on tampering, and must never carry the plaintext in the clear.
func TestByokSecretEncryptionIsBoundToOwner(t *testing.T) {
	original := common.CryptoSecret
	common.CryptoSecret = "byok-test-master-secret"
	t.Cleanup(func() { common.CryptoSecret = original })

	const (
		ownerID   int64 = 7
		otherID   int64 = 8
		plaintext       = "sk-proj-ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	)

	sealed, err := common.ByokEncryptSecret(ownerID, plaintext)
	require.NoError(t, err)
	assert.NotContains(t, sealed, plaintext)
	assert.NotContains(t, sealed, "sk-proj-ABCDEF")

	roundTripped, err := common.ByokDecryptSecret(ownerID, sealed)
	require.NoError(t, err)
	assert.Equal(t, plaintext, roundTripped)

	// The same plaintext sealed twice must differ, proving a fresh nonce.
	second, err := common.ByokEncryptSecret(ownerID, plaintext)
	require.NoError(t, err)
	assert.NotEqual(t, sealed, second)

	_, err = common.ByokDecryptSecret(otherID, sealed)
	assert.Error(t, err, "another user's derived key must not open this ciphertext")

	tampered := sealed[:len(sealed)-4] + "AAA="
	_, err = common.ByokDecryptSecret(ownerID, tampered)
	assert.Error(t, err, "a tampered ciphertext must fail GCM authentication")

	_, err = common.ByokEncryptSecret(0, plaintext)
	assert.Error(t, err, "a non-positive user id must be rejected")
	_, err = common.ByokEncryptSecret(ownerID, "   ")
	assert.Error(t, err, "a blank secret must be rejected")
	_, err = common.ByokEncryptSecret(ownerID, strings.Repeat("a", common.ByokMaxSecretLen+1))
	assert.Error(t, err, "an oversized secret must be rejected")

	hint, err := common.ByokKeyHint(plaintext)
	require.NoError(t, err)
	assert.Equal(t, "6789", hint)

	shortHint, err := common.ByokKeyHint("abc12345")
	require.NoError(t, err)
	assert.Equal(t, "********", shortHint, "a short secret must be fully masked")
}

// TestValidateByokKeyFormat covers the per-provider shape check that runs
// before a credential is stored.
func TestValidateByokKeyFormat(t *testing.T) {
	for _, tc := range []struct {
		provider string
		key      string
		wantErr  bool
	}{
		{provider: constant.ByokProviderOpenAI, key: "sk-proj-abc123"},
		{provider: constant.ByokProviderOpenAI, key: "sk-abc123"},
		{provider: constant.ByokProviderOpenAI, key: "abc123", wantErr: true},
		{provider: constant.ByokProviderAnthropic, key: "sk-ant-api03-abc123"},
		{provider: constant.ByokProviderAnthropic, key: "sk-abc123", wantErr: true},
		{provider: constant.ByokProviderGoogle, key: "AIzaSyA-abcdefghij"},
		{provider: constant.ByokProviderGoogle, key: "ya29.abcdefghij", wantErr: true},
		{provider: constant.ByokProviderMistral, key: "abcdefghij0123456789"},
		{provider: constant.ByokProviderMistral, key: "tooshort", wantErr: true},
		{provider: constant.ByokProviderCustom, key: "anything-goes"},
		{provider: constant.ByokProviderCustom, key: "tinykey", wantErr: true},
		{provider: "azure", key: "sk-abc123", wantErr: true},
		{provider: constant.ByokProviderOpenAI, key: "", wantErr: true},
		{provider: constant.ByokProviderOpenAI, key: "sk-line\nbreak", wantErr: true},
	} {
		t.Run(tc.provider+"/"+tc.key, func(t *testing.T) {
			err := ValidateByokKeyFormat(tc.provider, tc.key)
			if tc.wantErr {
				require.Error(t, err)
				// The message must never echo the submitted credential, because
				// request errors can end up in logs.
				if tc.key != "" {
					assert.NotContains(t, err.Error(), tc.key)
				}
				return
			}
			assert.NoError(t, err)
		})
	}
}

// TestDetectByokProviderFromModelName covers the fallback guesser used only
// when no configured channel claims the model.
func TestDetectByokProviderFromModelName(t *testing.T) {
	for _, tc := range []struct {
		modelName string
		want      string
	}{
		{modelName: "gpt-4o", want: constant.ByokProviderOpenAI},
		{modelName: "openai/gpt-4o-mini", want: constant.ByokProviderOpenAI},
		{modelName: "o3-mini", want: constant.ByokProviderOpenAI},
		{modelName: "text-embedding-3-large", want: constant.ByokProviderOpenAI},
		{modelName: "claude-sonnet-4-5", want: constant.ByokProviderAnthropic},
		{modelName: "anthropic/claude-3-haiku", want: constant.ByokProviderAnthropic},
		{modelName: "gemini-2.5-pro", want: constant.ByokProviderGoogle},
		{modelName: "google/gemini-2.0-flash", want: constant.ByokProviderGoogle},
		{modelName: "mistral-large-latest", want: constant.ByokProviderMistral},
		{modelName: "open-mixtral-8x7b", want: constant.ByokProviderMistral},
		{modelName: "deepseek-chat"},
		{modelName: "qwen-max"},
		{modelName: "  "},
	} {
		t.Run(tc.modelName, func(t *testing.T) {
			assert.Equal(t, tc.want, detectByokProviderFromModelName(tc.modelName))
		})
	}
}

// TestFindMatchingByokKey covers the routing decision that rc.4 will call from
// the relay loop: the configured channel mapping is authoritative, only the
// requesting user's own active keys are eligible, and several keys for one
// provider share the traffic.
func TestFindMatchingByokKey(t *testing.T) {
	truncateByok(t)
	original := common.CryptoSecret
	common.CryptoSecret = "byok-test-master-secret"
	t.Cleanup(func() { common.CryptoSecret = original })

	const (
		ownerID int64 = 11
		otherID int64 = 12
	)

	seedByokChannel(t, 900, constant.ChannelTypeOpenAI, "gpt-4o")
	// A model served only by Azure must not be matched to an OpenAI key even
	// though its name looks like one: an Azure deployment does not accept the
	// customer's OpenAI credential.
	seedByokChannel(t, 901, constant.ChannelTypeAzure, "gpt-4o-azure")

	ownerKey := newByokKeyFixture(ownerID, constant.ByokProviderOpenAI, "sk-owner-key-one", constant.ByokKeyStatusActive)
	require.NoError(t, model.InsertByokKey(ownerKey))
	require.NoError(t, model.InsertByokKey(newByokKeyFixture(otherID, constant.ByokProviderOpenAI, "sk-other-key-one", constant.ByokKeyStatusActive)))

	matched, err := FindMatchingByokKey(ownerID, "gpt-4o")
	require.NoError(t, err)
	require.NotNil(t, matched, "the owner's active OpenAI key must be selected")
	assert.Equal(t, ownerID, matched.UserId)
	assert.Equal(t, constant.ByokProviderOpenAI, matched.Provider)

	secret, err := matched.Secret()
	require.NoError(t, err)
	assert.Equal(t, "sk-owner-key-one", secret, "the selected key must decrypt for its owner")

	azureRoute, err := FindMatchingByokKey(ownerID, "gpt-4o-azure")
	require.NoError(t, err)
	assert.Nil(t, azureRoute, "a reseller-only model must fall through to managed channels")

	otherUserRoute, err := FindMatchingByokKey(13, "gpt-4o")
	require.NoError(t, err)
	assert.Nil(t, otherUserRoute, "a user with no key of their own must not borrow someone else's")

	unknownRoute, err := FindMatchingByokKey(ownerID, "deepseek-chat")
	require.NoError(t, err)
	assert.Nil(t, unknownRoute)

	// A disabled or rejected key is not eligible, and re-enabling it restores
	// the route.
	require.NoError(t, model.UpdateByokKeyStatus(ownerKey.ID, ownerID, constant.ByokKeyStatusDisabled, "turned off"))
	disabledRoute, err := FindMatchingByokKey(ownerID, "gpt-4o")
	require.NoError(t, err)
	assert.Nil(t, disabledRoute)
	require.NoError(t, model.UpdateByokKeyStatus(ownerKey.ID, ownerID, constant.ByokKeyStatusActive, ""))

	// A second key for the same provider joins the rotation rather than being
	// shadowed by the first.
	require.NoError(t, model.InsertByokKey(newByokKeyFixture(ownerID, constant.ByokProviderOpenAI, "sk-owner-key-two", constant.ByokKeyStatusActive)))
	seen := map[string]bool{}
	for range 8 {
		picked, err := FindMatchingByokKey(ownerID, "gpt-4o")
		require.NoError(t, err)
		require.NotNil(t, picked)
		decrypted, err := picked.Secret()
		require.NoError(t, err)
		seen[decrypted] = true
	}
	assert.Len(t, seen, 2, "both keys for one provider must share the traffic")

	// A key belonging to another provider must not be returned for this model.
	require.NoError(t, model.InsertByokKey(newByokKeyFixture(ownerID, constant.ByokProviderAnthropic, "sk-ant-owner-key", constant.ByokKeyStatusActive)))
	stillOpenAI, err := FindMatchingByokKey(ownerID, "gpt-4o")
	require.NoError(t, err)
	require.NotNil(t, stillOpenAI)
	assert.Equal(t, constant.ByokProviderOpenAI, stillOpenAI.Provider)
}

// TestByokUsageAggregation covers the reporting query behind GET
// /api/byok/usage: per-group sums, the date cutoff, and owner scoping.
func TestByokUsageAggregation(t *testing.T) {
	truncateByok(t)

	const ownerID int64 = 21
	now := time.Now()

	seedByokUsage(t, ownerID, 1, "gpt-4o", 100, 50, 2500, 50000, true, now.Add(-1*time.Hour))
	seedByokUsage(t, ownerID, 1, "gpt-4o", 200, 80, 4000, 80000, false, now.Add(-2*time.Hour))
	seedByokUsage(t, ownerID, 2, "gpt-4o-mini", 10, 5, 100, 2000, true, now.Add(-3*time.Hour))
	// Outside the 7-day window.
	seedByokUsage(t, ownerID, 1, "gpt-4o", 999, 999, 99999, 999999, true, now.Add(-30*24*time.Hour))
	// Another user's traffic must never appear.
	seedByokUsage(t, 22, 3, "gpt-4o", 7, 7, 700, 7000, true, now.Add(-1*time.Hour))

	since := now.Add(-7 * 24 * time.Hour)

	byModel, err := model.GetByokUsageAggregates(ownerID, since, model.ByokUsageGroupByModel)
	require.NoError(t, err)
	require.Len(t, byModel, 2)

	gpt4o := byModel[0]
	assert.Equal(t, "gpt-4o", gpt4o.ModelName)
	assert.Zero(t, gpt4o.ByokKeyId, "the non-grouped key id must not leak into a model report")
	assert.EqualValues(t, 2, gpt4o.RequestCount)
	assert.EqualValues(t, 1, gpt4o.SuccessCount)
	assert.EqualValues(t, 300, gpt4o.PromptTokens)
	assert.EqualValues(t, 130, gpt4o.CompletionTokens)
	assert.EqualValues(t, 6500, gpt4o.PlatformFee)
	assert.EqualValues(t, 130000, gpt4o.NotionalCost)

	mini := byModel[1]
	assert.Equal(t, "gpt-4o-mini", mini.ModelName)
	assert.EqualValues(t, 1, mini.RequestCount)
	assert.EqualValues(t, 1, mini.SuccessCount)

	byKey, err := model.GetByokUsageAggregates(ownerID, since, model.ByokUsageGroupByKey)
	require.NoError(t, err)
	require.Len(t, byKey, 2)
	assert.EqualValues(t, 1, byKey[0].ByokKeyId)
	assert.Empty(t, byKey[0].ModelName, "the non-grouped model must not leak into a key report")
	assert.EqualValues(t, 2, byKey[0].RequestCount)
	assert.EqualValues(t, 2, byKey[1].ByokKeyId)
	assert.EqualValues(t, 1, byKey[1].RequestCount)

	empty, err := model.GetByokUsageAggregates(404, since, model.ByokUsageGroupByModel)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// TestFlexInt64JSON pins the wire contract for snowflake identifiers: they are
// accepted as a JSON number or string and always emitted as a string, so a
// JavaScript client cannot round a value past 2^53.
func TestFlexInt64JSON(t *testing.T) {
	const large int64 = 7300123456789012345

	encoded, err := common.Marshal(hosttypes.NewFlexInt64(large))
	require.NoError(t, err)
	assert.Equal(t, `"7300123456789012345"`, string(encoded))

	for _, payload := range []string{`"7300123456789012345"`, `7300123456789012345`} {
		var decoded hosttypes.FlexInt64
		require.NoError(t, common.Unmarshal([]byte(payload), &decoded))
		assert.Equal(t, large, decoded.Int64())
	}

	var nullValue hosttypes.FlexInt64 = 5
	require.NoError(t, common.Unmarshal([]byte(`null`), &nullValue))
	assert.EqualValues(t, 0, nullValue)

	var fractional hosttypes.FlexInt64
	assert.Error(t, common.Unmarshal([]byte(`1.5`), &fractional), "a rounded identifier would point at another row")

	var overflow hosttypes.FlexInt64
	assert.Error(t, common.Unmarshal([]byte(`99999999999999999999`), &overflow))

	var view struct {
		Id hosttypes.FlexInt64 `json:"id"`
	}
	encodedView, err := common.Marshal(struct {
		Id hosttypes.FlexInt64 `json:"id"`
	}{Id: hosttypes.NewFlexInt64(large)})
	require.NoError(t, err)
	assert.JSONEq(t, `{"id":"7300123456789012345"}`, string(encodedView))
	require.NoError(t, common.Unmarshal(encodedView, &view))
	assert.Equal(t, large, view.Id.Int64())
}

// TestByokKeyIDIsSnowflake guards the primary-key contract: ids are assigned by
// the application, are unique across rows, and survive a database round trip.
func TestByokKeyIDIsSnowflake(t *testing.T) {
	truncateByok(t)
	original := common.CryptoSecret
	common.CryptoSecret = "byok-test-master-secret"
	t.Cleanup(func() { common.CryptoSecret = original })

	first := newByokKeyFixture(31, constant.ByokProviderOpenAI, "sk-first-key-value", constant.ByokKeyStatusActive)
	second := newByokKeyFixture(31, constant.ByokProviderAnthropic, "sk-ant-second-key", constant.ByokKeyStatusActive)
	require.NoError(t, model.InsertByokKey(first))
	require.NoError(t, model.InsertByokKey(second))

	assert.NotZero(t, first.ID)
	assert.NotEqual(t, first.ID, second.ID)

	reloaded, err := model.GetByokKeyByIdAndUser(first.ID, 31)
	require.NoError(t, err)
	assert.Equal(t, first.ID, reloaded.ID)
	assert.Equal(t, "...alue", reloaded.MaskedHint())

	_, err = model.GetByokKeyByIdAndUser(first.ID, 32)
	assert.ErrorIs(t, err, model.ErrByokKeyNotFound, "another user must not be able to load this key")

	require.NoError(t, model.DeleteByokKeyByIdAndUser(first.ID, 31))
	_, err = model.GetByokKeyByIdAndUser(first.ID, 31)
	assert.ErrorIs(t, err, model.ErrByokKeyNotFound, "deletion must be a hard delete")
	assert.ErrorIs(t, model.DeleteByokKeyByIdAndUser(first.ID, 31), model.ErrByokKeyNotFound)
}

// ---------------------------------------------------------------------------
// BYOK fixtures
// ---------------------------------------------------------------------------

// truncateByok clears only the tables these tests own, so it can run alongside
// the shared truncate helper without disturbing other suites.
func truncateByok(t *testing.T) {
	t.Helper()
	for _, table := range []string{"byok_keys", "byok_usage", "abilities", "channels"} {
		require.NoError(t, model.DB.Exec("DELETE FROM "+table).Error)
	}
	t.Cleanup(func() {
		for _, table := range []string{"byok_keys", "byok_usage", "abilities", "channels"} {
			model.DB.Exec("DELETE FROM " + table)
		}
	})
}

func newByokKeyFixture(userId int64, provider string, secret string, status int) *model.ByokKey {
	key := &model.ByokKey{UserId: userId, Provider: provider, Label: provider + " key", Status: status}
	if err := key.SetSecret(secret); err != nil {
		panic(err)
	}
	return key
}

func seedByokChannel(t *testing.T, channelId int, channelType int, modelName string) {
	t.Helper()
	channel := &model.Channel{
		Id:     channelId,
		Type:   channelType,
		Key:    "channel-key",
		Name:   "channel-" + modelName,
		Status: common.ChannelStatusEnabled,
		Group:  "default",
		Models: modelName,
	}
	require.NoError(t, model.DB.Create(channel).Error)

	enabled := true
	ability := &model.Ability{Group: "default", Model: modelName, ChannelId: channelId, Enabled: enabled}
	require.NoError(t, model.DB.Create(ability).Error)
}

func seedByokUsage(t *testing.T, userId int64, keyId int64, modelName string, promptTokens int, completionTokens int, platformFee int64, notionalCost int64, success bool, createdAt time.Time) {
	t.Helper()
	usage := &model.ByokUsage{
		ByokKeyId:        keyId,
		UserId:           userId,
		ModelName:        modelName,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		PlatformFee:      platformFee,
		NotionalCost:     notionalCost,
		LatencyMs:        120,
		Success:          success,
		CreatedAt:        createdAt,
	}
	require.NoError(t, model.InsertByokUsage(usage))
}
