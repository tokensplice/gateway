package metrics

import (
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/constant"
)

// The recorders in this file are the only API the rest of the gateway uses to
// publish operational facts. Three properties hold for every one of them:
//
//   - They never return an error and never panic. A dropped sample must not
//     become a failed request, so a non-positive duration, an empty label, or a
//     negative quota is normalised or ignored rather than propagated.
//   - They are no-ops when METRICS_ENABLED is false, so disabling observability
//     also stops the label series from accumulating in memory.
//   - They only ever publish values from a closed set. Anything a client or an
//     upstream could choose — a URL, a model name it invents, a provider's error
//     string — is either a route pattern, a configured value, or normalised by
//     the caller before it reaches a label.

// RecordRelayRequest publishes one finished relay request. duration is the
// whole request including channel retries, so it matches what the client
// experienced rather than what a single upstream took.
func RecordRelayRequest(model, provider, route string, duration time.Duration, success bool, promptTokens, completionTokens int) {
	if !Enabled() {
		return
	}
	status := StatusError
	if success {
		status = StatusSuccess
	}
	RelayRequestsTotal.WithLabelValues(label(model), label(provider), status, label(route)).Inc()
	RelayDurationSeconds.WithLabelValues(label(model), label(provider), label(route)).Observe(duration.Seconds())
	RecordTokensProcessed(model, promptTokens, completionTokens)
}

// RecordFirstToken publishes the streaming time to first token. Callers must
// only report it for a response that actually streamed, since a non-streaming
// request has no first-token boundary and its value would be the full latency.
func RecordFirstToken(model, provider string, duration time.Duration) {
	if !Enabled() || duration <= 0 {
		return
	}
	RelayFirstTokenSeconds.WithLabelValues(label(model), label(provider)).Observe(duration.Seconds())
}

// RecordByokRequest publishes one attempt on a customer's own upstream key.
// feeQuota is the platform fee actually charged, so a failed attempt reports 0
// and never inflates revenue. The request's own duration is not recorded here:
// the relay boundary already reports it, on the byok route.
func RecordByokRequest(model, provider string, success bool, feeQuota int64, duration time.Duration) {
	if !Enabled() {
		return
	}
	status := StatusError
	if success {
		status = StatusSuccess
	}
	ByokRequestsTotal.WithLabelValues(label(model), label(provider), status).Inc()
	ByokFeeQuotaTotal.Add(quotaUnits(feeQuota))
	ByokAttemptDurationSeconds.WithLabelValues(label(provider)).Observe(duration.Seconds())
}

// RecordByokKeyFailure publishes why a customer key could not serve a request.
// errorType must come from a closed set the caller derives from the upstream
// status code; passing the provider's own error string through would let an
// upstream create unbounded series by changing its wording.
func RecordByokKeyFailure(provider, errorType string) {
	if !Enabled() {
		return
	}
	ByokKeyFailuresTotal.WithLabelValues(label(provider), label(errorType)).Inc()
}

// RecordChannelRequest publishes one attempt against a persisted managed
// channel. A BYOK shadow channel is skipped: it has a synthetic negative id and
// is already accounted for by RecordByokRequest.
//
// A failure deliberately says nothing about health — one bad request does not
// disable a channel, and only DisableChannel, EnableChannel, and the periodic
// database snapshot lower channel_up. A success does raise it, because a channel
// that just served a request is enabled by definition, which keeps the gauge
// current between snapshots.
func RecordChannelRequest(channelId int, channelName string, success bool, duration time.Duration) {
	if !Enabled() || channelId <= 0 {
		return
	}
	status := StatusError
	if success {
		status = StatusSuccess
		SetChannelUp(channelId, channelName, true)
	}
	id := strconv.Itoa(channelId)
	ChannelRequestsTotal.WithLabelValues(id, status).Inc()
	ChannelDurationSeconds.WithLabelValues(id).Observe(duration.Seconds())
}

// SetChannelUp publishes the health of a managed channel. The gauge is keyed by
// id and name so a dashboard can show a readable row without a join; a renamed
// channel therefore appears as a new series until the next database snapshot.
func SetChannelUp(channelId int, channelName string, up bool) {
	if !Enabled() || channelId <= 0 {
		return
	}
	value := float64(0)
	if up {
		value = 1
	}
	ChannelUp.WithLabelValues(strconv.Itoa(channelId), label(channelName)).Set(value)
}

// ResetChannelHealth drops every channel_up series. The periodic snapshot in
// service.StartMetricsGaugeSync calls it before republishing the whole channel
// table, which is what removes the series of a deleted or renamed channel
// instead of leaving it pinned at its last value forever.
func ResetChannelHealth() {
	ChannelUp.Reset()
}

// SetByokKeysActive publishes how many customer keys are currently routable.
func SetByokKeysActive(count int) {
	if !Enabled() {
		return
	}
	ByokKeysActive.Set(float64(max(count, 0)))
}

// RecordTokensProcessed publishes the settled token counts of a request. A
// request that produced no billable usage reports nothing rather than two zeros.
func RecordTokensProcessed(model string, promptTokens, completionTokens int) {
	if !Enabled() {
		return
	}
	if promptTokens > 0 {
		TokensProcessedTotal.WithLabelValues(label(model), TokenTypePrompt).Add(float64(promptTokens))
	}
	if completionTokens > 0 {
		TokensProcessedTotal.WithLabelValues(label(model), TokenTypeCompletion).Add(float64(completionTokens))
	}
}

// RecordQuotaConsumed publishes the quota charged for one settled request,
// whichever route served it.
func RecordQuotaConsumed(quota int) {
	if !Enabled() {
		return
	}
	QuotaConsumedTotal.Add(quotaUnits(int64(quota)))
}

// RecordUserRegistration publishes one completed sign-up. It is aggregate only,
// so a metrics reader learns the rate and nothing about who registered.
func RecordUserRegistration() {
	if !Enabled() {
		return
	}
	UserRegistrationsTotal.Inc()
}

// RouteForChannelId names the route a channel id belongs to, which is how the
// relay path tells a BYOK shadow channel from a persisted managed channel.
func RouteForChannelId(channelId int) string {
	if constant.IsByokShadowChannelId(channelId) {
		return RouteByok
	}
	return RouteManaged
}

// quotaUnits keeps a negative quota, which only a settlement bug could produce,
// from ever moving a counter backwards and panicking the scrape.
func quotaUnits(quota int64) float64 {
	return float64(max(quota, 0))
}
