package service

import (
	"fmt"
	"sync/atomic"

	"github.com/QuantumNous/new-api/model"
)

// byokRotationCursor advances the round-robin position across the candidate
// keys of one provider. It is process-local: with several gateway replicas
// each rotates independently, which still spreads load without needing shared
// state on a path that runs before every request.
var byokRotationCursor atomic.Uint64

// FindMatchingByokKey returns the customer key that should serve modelName for
// userId, or (nil, nil) when the request must fall through to a managed
// channel and standard billing.
//
// A nil key with a nil error is the normal "no BYOK route" answer, not a
// failure, so the caller can treat it as a cheap predicate. An error means the
// lookup itself broke and the caller should decide whether to fall through or
// fail the request.
//
// Selection order:
//  1. DetectByokProviders resolves which upstreams could serve the model, most
//     authoritative first (see byok_provider.go).
//  2. For each candidate provider, the user's active keys are loaded in a
//     stable id order and one is chosen round-robin, so several keys for the
//     same provider share the traffic instead of one key absorbing all of it.
//  3. The first provider that has a key wins; a provider the user has not
//     bound a key for is skipped.
//
// The returned key still holds its ciphertext. Decrypt with (*ByokKey).Secret
// only at the point where the upstream request is built, and never log it.
//
// rc.4 INTEGRATION POINT (routing enhancement). This function is deliberately
// not called from the relay loop yet. Wiring it up means:
//   - controller/relay.go getChannel(): consult FindMatchingByokKey(info.UserId,
//     info.OriginModelName) before service.CacheGetRandomSatisfiedChannel, and
//     build a synthetic channel (base URL + decrypted key) when it returns a
//     key, so the existing adaptors, retries, and stream handling are reused
//     unchanged.
//   - On settlement, charge CalculateByokFee(notionalCost, feePercent) instead
//     of the full price, then persist a model.ByokUsage row and call
//     model.TouchByokKeyUsage. Keeping the touch at settlement rather than
//     here means a routed-but-failed request does not advance the rotation.
//   - On an upstream 401/403 from a BYOK channel, mark the key invalid through
//     model.UpdateByokKeyStatus and fall through to a managed channel, so one
//     revoked customer key cannot take down the route.
func FindMatchingByokKey(userId int64, modelName string) (*model.ByokKey, error) {
	if userId <= 0 {
		return nil, nil
	}
	for _, provider := range DetectByokProviders(modelName) {
		candidates, err := model.GetActiveByokKeysByUserProvider(userId, provider)
		if err != nil {
			return nil, fmt.Errorf("byok: failed to load %s keys for user %d: %w", provider, userId, err)
		}
		if len(candidates) == 0 {
			continue
		}
		cursor := byokRotationCursor.Add(1)
		return candidates[cursor%uint64(len(candidates))], nil
	}
	return nil, nil
}
