package service

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/metrics"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	hosttypes "github.com/QuantumNous/new-api/types"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
)

// BYOK relay routing.
//
// A request whose model the customer has bound their own upstream key to is
// served through an ephemeral in-memory channel — a "shadow channel" — that is
// injected ahead of managed channel selection. Everything downstream of channel
// selection keys off channel.Type and the context values written by
// middleware.SetupContextForSelectedChannel, so synthesising that one object
// reuses the existing adaptors, request conversion, streaming, error mapping,
// and settlement unchanged.
//
// Two properties make this safe to put in front of the managed path:
//
//   - The shadow channel reports a negative id (constant.ByokShadowChannelId)
//     and AutoBan 0, so the accounting and health writes on the relay path can
//     never reach a real channel row because of a customer key.
//   - A BYOK attempt is made at most once per request and only before any byte
//     of a response has been written, so a failure falls through to managed
//     channels invisibly instead of corrupting a stream that already started.
//
// Billing: the notional cost is computed by the ordinary managed pricing path
// and the customer is charged CalculateByokFee(notional, feePercent) instead.
// See .agents/rules/billing.md.

// ByokRoute is one in-flight attempt served by a customer's own upstream key.
//
// The plaintext credential lives only in Channel.Key, which the relay reads
// once to build the upstream request, and in secret, which exists so an
// upstream error that echoes the credential back can be redacted before it
// reaches a log or the client. Neither is persisted; ClearSecret drops both.
type ByokRoute struct {
	KeyId      int64
	UserId     int64
	Provider   string
	ModelName  string
	FeePercent float64
	Channel    *model.Channel
	StartedAt  time.Time

	secret string
}

// byokRelayFormats are the client protocols a shadow channel may serve. They
// are the formats whose adaptors are exercised against the four vendor-native
// channel types BYOK maps to. Responses, realtime, embeddings, rerank, audio,
// image, and task protocols stay on managed channels.
var byokRelayFormats = []types.RelayFormat{
	types.RelayFormatOpenAI,
	types.RelayFormatClaude,
	types.RelayFormatGemini,
}

// BeginByokAttempt resolves the BYOK route for the attempt about to run and
// records it in the request context. It returns nil when the attempt must use a
// managed channel, which is the common case and must stay cheap.
//
// Every attempt starts by clearing the previous route, so a BYOK failure that
// falls through to a managed channel can never settle at the platform fee.
func BeginByokAttempt(c *gin.Context, info *relaycommon.RelayInfo) *ByokRoute {
	if c == nil {
		return nil
	}
	c.Set(string(constant.ContextKeyByokRoute), nil)
	if info == nil {
		return nil
	}
	// One BYOK attempt per request. Marking it here, before the lookup, is what
	// keeps the managed-channel fallback from re-entering this path.
	if common.GetContextKeyBool(c, constant.ContextKeyByokAttempted) {
		return nil
	}
	c.Set(string(constant.ContextKeyByokAttempted), true)

	route := resolveByokRoute(c, info)
	if route == nil {
		return nil
	}
	common.SetContextKey(c, constant.ContextKeyByokRoute, route)
	return route
}

// CurrentByokRoute returns the route of the attempt in flight, or nil when a
// managed channel is serving it.
func CurrentByokRoute(c *gin.Context) *ByokRoute {
	if c == nil {
		return nil
	}
	route, _ := common.GetContextKeyType[*ByokRoute](c, constant.ContextKeyByokRoute)
	return route
}

// ClearByokRoute abandons a resolved route before it served anything, so the
// attempt is billed and logged as an ordinary managed-channel request.
func ClearByokRoute(c *gin.Context) {
	if c == nil {
		return
	}
	if route := CurrentByokRoute(c); route != nil {
		route.ClearSecret()
	}
	c.Set(string(constant.ContextKeyByokRoute), nil)
}

// FinishByokAttempt closes the current iteration's BYOK attempt and reports
// whether there was one, which is what tells the relay loop that a failure has
// to fall through to managed channels.
//
// A nil apiErr means the request was served: settlement has already priced the
// platform fee and written the usage row, so all that is left is releasing the
// credential. A non-nil one books the failed attempt and redacts the credential
// from the error before it can reach a log row or the client.
func FinishByokAttempt(c *gin.Context, apiErr *types.NewAPIError) bool {
	route := CurrentByokRoute(c)
	if route == nil {
		return false
	}
	if apiErr != nil {
		route.RecordFailure(c, apiErr)
	}
	ClearByokRoute(c)
	return true
}

// resolveByokRoute answers "should this request go out on the customer's own
// key?". Every rejection path returns nil so the caller falls through to
// managed channels; a BYOK lookup that breaks must never fail a request that
// the platform could otherwise serve.
func resolveByokRoute(c *gin.Context, info *relaycommon.RelayInfo) *ByokRoute {
	if info.UserId <= 0 || info.IsChannelTest || info.OriginModelName == "" {
		return nil
	}
	if !slices.Contains(byokRelayFormats, info.RelayFormat) {
		return nil
	}
	// A pinned channel is an explicit operator or session decision; a customer
	// key must not quietly override it.
	if _, pinned, _ := GetChannelConstraints(c).ResolvedPin(); pinned {
		return nil
	}

	userId := int64(info.UserId)
	// The gate before the gate: provider detection and key selection cost
	// several queries, and almost every user holds no key at all.
	hasKey, err := model.HasActiveByokKey(userId)
	if err != nil {
		logger.LogWarn(c, "byok: key lookup failed for user %d, using managed channels: %s", userId, err.Error())
		return nil
	}
	if !hasKey {
		return nil
	}

	key, err := FindMatchingByokKey(userId, info.OriginModelName)
	if err != nil {
		logger.LogWarn(c, "byok: no route resolved for user %d model %s, using managed channels: %s", userId, info.OriginModelName, err.Error())
		return nil
	}
	if key == nil {
		return nil
	}
	if constant.ByokChannelTypeForProvider(key.Provider) == 0 {
		logger.LogDebug(c, "byok: provider %s has no relay channel type, using managed channels", key.Provider)
		return nil
	}

	secret, err := key.Secret()
	if err != nil || strings.TrimSpace(secret) == "" {
		// ByokDecryptSecret is deliberately generic; say which key failed and
		// never why, because the reason can distinguish a wrong owner from a
		// wrong master secret.
		logger.LogError(c, fmt.Sprintf("byok: key %d of user %d cannot be decrypted, using managed channels", key.ID, userId))
		secret = ""
		return nil
	}

	feePercent, err := ResolveByokFeePercent(userId)
	if err != nil {
		// Without a fee the request cannot be billed correctly, so it must not
		// be served on the customer key at all.
		secret = ""
		logger.LogError(c, fmt.Sprintf("byok: cannot resolve the platform fee for user %d, using managed channels: %s", userId, err.Error()))
		return nil
	}

	return &ByokRoute{
		KeyId:      key.ID,
		UserId:     userId,
		Provider:   key.Provider,
		ModelName:  info.OriginModelName,
		FeePercent: feePercent,
		Channel:    buildByokShadowChannel(key, secret, info.UsingGroup, info.OriginModelName),
		StartedAt:  time.Now(),
		secret:     secret,
	}
}

// buildByokShadowChannel synthesises the channel object the relay selects.
//
// BaseURL is the vendor's own endpoint from constant.ChannelBaseURLs, which is
// exactly the upstream a customer key authenticates against. It is set
// explicitly rather than left nil because Channel.GetBaseURL only falls back to
// the built-in default for a non-nil empty string. AutoBan is 0 so a rejected
// customer credential can never disable a platform channel, and the negative id
// keeps every id-keyed write off the managed channel rows.
func buildByokShadowChannel(key *model.ByokKey, secret string, groupName string, modelName string) *model.Channel {
	zero := uint(0)
	priority := int64(0)
	autoBan := 0
	channelType := constant.ByokChannelTypeForProvider(key.Provider)
	return &model.Channel{
		Id:          constant.ByokShadowChannelId(key.ID),
		Type:        channelType,
		Name:        fmt.Sprintf("BYOK %s #%d", key.Provider, key.ID),
		Key:         secret,
		Status:      common.ChannelStatusEnabled,
		Weight:      &zero,
		Priority:    &priority,
		AutoBan:     &autoBan,
		BaseURL:     common.GetPointer(constant.GetChannelBaseURL(channelType)),
		CreatedTime: key.CreatedAt.Unix(),
		Models:      modelName,
		Group:       groupName,
	}
}

// ClearSecret drops the plaintext credential this route holds. Go strings are
// immutable so the bytes cannot be wiped, but the request-scoped references the
// BYOK path owns are released as soon as the attempt is over. The credential is
// never written to a log, an audit record, a usage row, or a response.
func (r *ByokRoute) ClearSecret() {
	if r == nil {
		return
	}
	r.secret = ""
	if r.Channel != nil {
		r.Channel.Key = ""
	}
}

// byokSecretPatterns match the credential shapes the supported vendors issue.
// They are the second line of defence behind the exact-match redaction: some
// providers echo the received Authorization header back inside an error body,
// and that body can end up in a log row.
var byokSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{8,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9_\-]{8,}`),
	regexp.MustCompile(`AIza[0-9A-Za-z_\-]{20,}`),
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-]+`),
}

// RedactSecret removes the customer's credential from an error before that
// error can reach a log row, an audit record, or the client. It only rewrites
// the message when something actually matched, so the error's wrapping — which
// the retry decision reads — is preserved for ordinary failures.
func (r *ByokRoute) RedactSecret(apiErr *types.NewAPIError) {
	if r == nil || apiErr == nil {
		return
	}
	message := apiErr.Error()
	if message == "" {
		return
	}
	redacted := message
	if r.secret != "" {
		redacted = strings.ReplaceAll(redacted, r.secret, "***")
	}
	for _, pattern := range byokSecretPatterns {
		redacted = pattern.ReplaceAllString(redacted, "***")
	}
	if redacted == message {
		return
	}
	apiErr.SetMessage(redacted)
}

// RecordSuccess books a served BYOK request: the byok_usage row and the key
// touch. The touch happens here rather than at selection time so a routed but
// failed request does not advance the round-robin.
func (r *ByokRoute) RecordSuccess(ctx *gin.Context, promptTokens int, completionTokens int, notionalQuota int64, feeQuota int64) {
	if r == nil {
		return
	}
	r.recordUsage(ctx, promptTokens, completionTokens, notionalQuota, feeQuota, true)
	if err := model.TouchByokKeyUsage(r.KeyId, time.Now()); err != nil {
		logger.LogError(ctx, fmt.Sprintf("byok: failed to record the usage of key %d: %s", r.KeyId, err.Error()))
	}
}

// RecordFailure books a BYOK attempt that produced no response, redacts the
// credential from the error, and marks the key invalid when the vendor's own
// API authoritatively rejected it.
//
// Only 401 and 403 change the stored status. A timeout, a rate limit, or a 5xx
// says nothing about the credential, and disabling a working customer key
// during an upstream incident would be worse than one failed attempt.
func (r *ByokRoute) RecordFailure(ctx *gin.Context, apiErr *types.NewAPIError) {
	if r == nil {
		return
	}
	r.RedactSecret(apiErr)

	statusCode := 0
	reason := "byok attempt failed"
	if apiErr != nil {
		statusCode = apiErr.StatusCode
		if masked := apiErr.MaskSensitiveErrorWithStatusCode(); masked != "" {
			reason = masked
		}
	}

	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		if err := model.UpdateByokKeyStatus(r.KeyId, r.UserId, constant.ByokKeyStatusInvalid, reason); err != nil {
			logger.LogError(ctx, fmt.Sprintf("byok: failed to mark key %d invalid: %s", r.KeyId, err.Error()))
		} else {
			logger.LogWarn(ctx, "byok: key %d of user %d was rejected by %s (HTTP %d) and is now marked invalid",
				r.KeyId, r.UserId, r.Provider, statusCode)
			// The customer's own credential was rejected, so this is a
			// tenant-scoped event that goes only to that owner's endpoints. It
			// is dispatched off the request path because a BYOK failure still
			// has to fall through to a managed channel promptly, and the reason
			// is already masked above.
			gopool.Go(func() {
				DispatchEvent(r.UserId, constant.WebhookEventByokKeyInvalid, ByokKeyInvalidEventData{
					ByokKeyId: hosttypes.NewFlexInt64(r.KeyId),
					Provider:  r.Provider,
					Error:     reason,
				})
			})
		}
	}

	// The failure reason is published as a closed set derived from the upstream
	// status code. The provider's own error string must not become a label: it
	// is upstream-controlled text, so it would let a vendor multiply the series
	// count by changing its wording.
	errorType := "unreachable"
	switch {
	case statusCode == http.StatusUnauthorized, statusCode == http.StatusForbidden:
		errorType = "invalid_credential"
	case statusCode == http.StatusTooManyRequests:
		errorType = "rate_limited"
	case statusCode >= 500:
		errorType = "upstream_error"
	case statusCode >= 400:
		errorType = "request_rejected"
	}
	metrics.RecordByokKeyFailure(r.Provider, errorType)

	r.recordUsage(ctx, 0, 0, 0, 0, false)
}

// recordUsage writes the one byok_usage row every BYOK attempt produces,
// successful or not. A failed attempt has no measured usage and is charged
// nothing, so its token and quota columns stay zero.
func (r *ByokRoute) recordUsage(ctx *gin.Context, promptTokens int, completionTokens int, notionalQuota int64, feeQuota int64, success bool) {
	if r == nil {
		return
	}
	latency := time.Since(r.StartedAt)
	// Every attempt is booked here, served or not, which is exactly the
	// population the BYOK counters need: a failed attempt is invisible to the
	// customer because the request falls through to a managed channel, so the
	// counter is the only signal that their key is breaking. feeQuota is 0 on a
	// failure, so revenue is never overstated. Token counts are not published
	// here — the relay boundary reports them for both routes from the same
	// settled figures.
	metrics.RecordByokRequest(r.ModelName, r.Provider, success, feeQuota, latency)
	usage := &model.ByokUsage{
		ByokKeyId:        r.KeyId,
		UserId:           r.UserId,
		ModelName:        r.ModelName,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		PlatformFee:      feeQuota,
		NotionalCost:     notionalQuota,
		LatencyMs:        latency.Milliseconds(),
		Success:          success,
	}
	if err := model.InsertByokUsage(usage); err != nil {
		logger.LogError(ctx, fmt.Sprintf("byok: failed to record usage for key %d: %s", r.KeyId, err.Error()))
	}
}

// AppendByokLogInfo stamps the consume log with the BYOK facts a customer needs
// to reconcile their own upstream bill against the platform fee. The key id is
// a FlexInt64 because a snowflake exceeds the JavaScript integer boundary.
func (r *ByokRoute) AppendByokLogInfo(other *model.LogOther, notionalQuota int64, feeQuota int64) {
	if r == nil || other == nil {
		return
	}
	other.SetPublic("byok", true)
	other.SetPublic("byok_provider", r.Provider)
	other.SetPublic("byok_key_id", hosttypes.NewFlexInt64(r.KeyId))
	other.SetPublic("byok_fee_percent", r.FeePercent)
	other.SetPublic("byok_notional_quota", notionalQuota)
	other.SetPublic("byok_fee_quota", feeQuota)
}
