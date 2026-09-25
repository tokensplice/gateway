package service

import (
	"errors"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

// BYOK platform fee arithmetic.
//
// A request served by the customer's own upstream key costs the gateway
// nothing in tokens, so the charge is a percentage of the "notional cost": the
// quota the same request would have consumed through a managed channel. Both
// figures are plain quota, the internal unit where common.QuotaPerUnit quota
// equals one dollar.
//
// The notional cost itself is produced by the ordinary managed pricing path and
// is only available once a request has been routed and its usage measured, so
// this file stays pure arithmetic. rc.4 supplies the notional cost from the
// relay settlement step described in service/byok_router.go.

// CalculateByokFee returns the quota to charge for a BYOK-routed request.
//
// It follows the billing safety invariants in .agents/rules/billing.md:
//   - A non-positive notional cost yields zero. A BYOK fee can never become a
//     credit, whatever the caller passes in.
//   - The percentage is clamped to the supported band before use, so a
//     corrupted option value or a NaN cannot produce an unbounded charge.
//   - The float product is converted with common.QuotaRound, the shared
//     half-away-from-zero helper that saturates at the int32 single-request
//     boundary and logs any clamp. There is no bare int() cast here.
func CalculateByokFee(notionalQuotaCost int64, feePercent float64) int64 {
	fee, _ := CalculateByokFeeChecked(notionalQuotaCost, feePercent)
	return fee
}

// CalculateByokFeeChecked is CalculateByokFee but also returns a non-nil
// *common.QuotaClamp when the computed fee had to be saturated, so the calling
// billing path can attach it to the consume log the same way every other
// settlement does (see attachQuotaSaturation in service/log_info_generate.go).
func CalculateByokFeeChecked(notionalQuotaCost int64, feePercent float64) (int64, *common.QuotaClamp) {
	if notionalQuotaCost <= 0 {
		return 0, nil
	}
	percent := operation_setting.ClampByokFeePercent(feePercent)
	if percent <= 0 {
		return 0, nil
	}
	fee, clamp := common.QuotaRoundChecked(float64(notionalQuotaCost) * percent / 100)
	if fee <= 0 {
		// A sub-half-quota fee rounds to zero; never let the conversion emit a
		// negative charge.
		return 0, clamp
	}
	return int64(fee), clamp
}

// ResolveByokFeePercent returns the fee percentage that applies to one user:
// their admin-set override when there is one, otherwise the global
// ByokFeePercent option. The result is always inside the supported band.
//
// The override lives in users.byok_fee_override and is not part of the UserBase
// cache yet, so this reads through to the database. rc.4 must add the field to
// UserBase and bump cacheSchema before this is called on the relay hot path.
func ResolveByokFeePercent(userId int64) (float64, error) {
	globalPercent := operation_setting.ClampByokFeePercent(operation_setting.ByokFeePercent)
	if userId <= 0 {
		return 0, errors.New("byok: user id must be positive")
	}
	override, err := model.GetUserByokFeeOverride(userId)
	if err != nil {
		return 0, err
	}
	if override == operation_setting.ByokFeeNoOverride {
		return globalPercent, nil
	}
	return operation_setting.ClampByokFeePercent(override), nil
}
