// Package metrics exposes the gateway's operational state as Prometheus
// collectors, scraped from GET /metrics.
//
// Everything here is observability only: a recorder must never influence
// routing, billing, or the response a client receives, and a label value must
// never be attacker-controlled free text. Label sets are therefore bounded by
// configured models, channel types, and channels, and any value that could
// come from a request body or an upstream error is normalised to a closed set
// before it reaches a collector.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// namespace prefixes every collector so the gateway's own series cannot
// collide with the Go runtime and process series the default registerer adds.
const namespace = "tokensplice"

// Label values shared by several collectors. They are a closed set on purpose:
// a Grafana panel groups by them, and an unbounded value would multiply the
// series count without adding information.
const (
	RouteByok    = "byok"
	RouteManaged = "managed"

	StatusSuccess = "success"
	StatusError   = "error"

	TokenTypePrompt     = "prompt"
	TokenTypeCompletion = "completion"

	// UnmatchedPath labels requests that no registered route handled. The raw
	// URL is deliberately not used: an unauthenticated client could otherwise
	// create an unbounded number of series by requesting random paths.
	UnmatchedPath = "unmatched"

	unknownLabel = "unknown"
)

// durationBuckets spans a local cache hit to a multi-minute video or reasoning
// generation, so a slow relay is distinguishable from a stalled one.
var durationBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// firstTokenBuckets covers only the wait before the first streamed chunk,
// which is the latency a chat client actually perceives.
var firstTokenBuckets = []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30}

var (
	// RelayRequestsTotal counts finished relay requests, one per request rather
	// than per channel attempt, split by the route that served it.
	RelayRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "relay_requests_total",
		Help:      "Relay requests served, by model, upstream provider, outcome, and route (byok or managed).",
	}, []string{"model", "provider", "status", "route"})

	// RelayDurationSeconds measures the whole relay request, including every
	// retried channel attempt.
	RelayDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "relay_duration_seconds",
		Help:      "End-to-end relay request duration in seconds, by model, upstream provider, and route.",
		Buckets:   durationBuckets,
	}, []string{"model", "provider", "route"})

	// RelayFirstTokenSeconds is the streaming time to first token: the moment
	// the first response byte was written minus the moment the request arrived.
	RelayFirstTokenSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "relay_first_token_seconds",
		Help:      "Time to the first streamed response token in seconds, by model and upstream provider.",
		Buckets:   firstTokenBuckets,
	}, []string{"model", "provider"})

	// TokensProcessedTotal counts the tokens the gateway settled, split into the
	// two directions that are priced differently.
	TokensProcessedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "tokens_processed_total",
		Help:      "Tokens accounted for at settlement, by model and direction (prompt or completion).",
	}, []string{"model", "type"})

	// ByokRequestsTotal counts attempts served on a customer's own upstream key.
	// A failed attempt is one the customer never sees, because the gateway falls
	// through to a managed channel; the counter is what makes that visible.
	ByokRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "byok_requests_total",
		Help:      "BYOK attempts, by model, customer provider, and outcome.",
	}, []string{"model", "provider", "status"})

	// ByokFeeQuotaTotal is the platform fee revenue collected on BYOK traffic,
	// in internal quota units.
	ByokFeeQuotaTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "byok_fee_quota_total",
		Help:      "Total BYOK platform fee collected, in internal quota units.",
	})

	// ByokAttemptDurationSeconds measures one attempt against a customer's own
	// upstream. It is separate from RelayDurationSeconds because a BYOK attempt
	// that fails is followed by a managed attempt inside the same request, so the
	// two durations answer different questions.
	ByokAttemptDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "byok_attempt_duration_seconds",
		Help:      "Duration of a single BYOK attempt in seconds, by customer provider.",
		Buckets:   durationBuckets,
	}, []string{"provider"})

	// ByokKeysActive is the number of customer keys currently eligible for
	// routing. It is refreshed from the database, so it survives a restart.
	ByokKeysActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "byok_keys_active",
		Help:      "Number of BYOK keys whose status is active.",
	})

	// ByokKeyFailuresTotal classifies why a customer key could not serve a
	// request. error_type is a closed set derived from the upstream status code,
	// never the provider's own error string.
	ByokKeyFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "byok_key_failures_total",
		Help:      "Failed BYOK attempts, by customer provider and normalised error type.",
	}, []string{"provider", "error_type"})

	// ChannelUp is the health of a managed channel: 1 enabled, 0 disabled or
	// auto-disabled. Refreshed from the database and corrected by the relay path.
	ChannelUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "channel_up",
		Help:      "Whether a managed channel is enabled (1) or not (0).",
	}, []string{"channel_id", "channel_name"})

	// ChannelRequestsTotal counts one managed-channel attempt, so a request that
	// failed over is attributed to every channel it touched.
	ChannelRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "channel_requests_total",
		Help:      "Managed channel attempts, by channel id and outcome.",
	}, []string{"channel_id", "status"})

	// ChannelDurationSeconds measures a single attempt against one channel,
	// which is what an operator needs to rank upstreams by speed.
	ChannelDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "channel_duration_seconds",
		Help:      "Duration of a single managed channel attempt in seconds, by channel id.",
		Buckets:   durationBuckets,
	}, []string{"channel_id"})

	// ActiveConnections is the number of HTTP requests currently being served,
	// including long-lived SSE and WebSocket relays.
	ActiveConnections = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "active_connections",
		Help:      "HTTP requests currently in flight.",
	})

	// QuotaConsumedTotal is the quota charged across every route, BYOK fees
	// included, in internal quota units.
	QuotaConsumedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "quota_consumed_total",
		Help:      "Total quota charged to users, in internal quota units.",
	})

	// UserRegistrationsTotal counts completed sign-ups.
	UserRegistrationsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "user_registrations_total",
		Help:      "Total user registrations.",
	})

	// HttpRequestsTotal counts every request the HTTP server answered, relay and
	// management API alike.
	HttpRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "http_requests_total",
		Help:      "HTTP requests served, by method, route pattern, and status code.",
	}, []string{"method", "path", "status_code"})

	// HttpRequestDurationSeconds is the same population as HttpRequestsTotal,
	// measured around the whole handler chain.
	HttpRequestDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "http_request_duration_seconds",
		Help:      "HTTP request duration in seconds, by method, route pattern, and status code.",
		Buckets:   durationBuckets,
	}, []string{"method", "path", "status_code"})
)

// collectors is the closed registration list. Adding a collector here is what
// publishes it; a collector that is only declared stays invisible to a scrape.
var collectors = []prometheus.Collector{
	RelayRequestsTotal,
	RelayDurationSeconds,
	RelayFirstTokenSeconds,
	TokensProcessedTotal,
	ByokRequestsTotal,
	ByokFeeQuotaTotal,
	ByokAttemptDurationSeconds,
	ByokKeysActive,
	ByokKeyFailuresTotal,
	ChannelUp,
	ChannelRequestsTotal,
	ChannelDurationSeconds,
	ActiveConnections,
	QuotaConsumedTotal,
	UserRegistrationsTotal,
	HttpRequestsTotal,
	HttpRequestDurationSeconds,
}

func init() {
	prometheus.MustRegister(collectors...)
}

// label normalises an empty label value so a series is never published with a
// blank dimension, which Grafana would render as an unnamed group.
func label(value string) string {
	if value == "" {
		return unknownLabel
	}
	return value
}
