package constant

import "slices"

// Webhook event types a customer can subscribe to. The names are part of the
// public API contract: they appear in the `events` list of a stored webhook, in
// the `X-TokenSplice-Event` request header, and in the `event` field of the
// delivered JSON body, so renaming one is a breaking change.
const (
	// WebhookEventQuotaLow fires when a user's remaining quota drops below the
	// configured warning threshold.
	WebhookEventQuotaLow = "quota.low"
	// WebhookEventQuotaExhausted fires when a user's remaining quota reaches
	// zero.
	WebhookEventQuotaExhausted = "quota.exhausted"
	// WebhookEventTokenExpiring fires when one of a user's API tokens expires
	// within model.ExpirationWarningDays.
	WebhookEventTokenExpiring = "token.expiring"
	// WebhookEventChannelDown fires when a managed channel is auto-disabled.
	WebhookEventChannelDown = "channel.down"
	// WebhookEventChannelRecovered fires when an auto-disabled managed channel
	// comes back online.
	WebhookEventChannelRecovered = "channel.recovered"
	// WebhookEventRequestError fires when one of a user's relay requests fails
	// after every retry. Subscribing to it is opt-in per webhook, so a customer
	// who does not want per-request traffic never receives it.
	WebhookEventRequestError = "request.error"
	// WebhookEventByokKeyInvalid fires when an upstream provider rejects one of
	// the user's own BYOK credentials with 401/403.
	WebhookEventByokKeyInvalid = "byok.key_invalid"
	// WebhookEventTest is only produced by the "send a test event" endpoint. It
	// is subscribable so a receiver can filter it, but no production path emits
	// it.
	WebhookEventTest = "webhook.test"
)

// Webhook lifecycle states stored in webhooks.status.
const (
	WebhookStatusDisabled = 0
	WebhookStatusActive   = 1
)

// MaxWebhooksPerUser bounds how many endpoints one user can register. It keeps
// a single account from turning every event into a fan-out storm.
const MaxWebhooksPerUser = 100

// WebhookEvents lists every subscribable event, ordered for stable validation
// messages.
var WebhookEvents = []string{
	WebhookEventQuotaLow,
	WebhookEventQuotaExhausted,
	WebhookEventTokenExpiring,
	WebhookEventChannelDown,
	WebhookEventChannelRecovered,
	WebhookEventRequestError,
	WebhookEventByokKeyInvalid,
	WebhookEventTest,
}

// webhookAdminEvents are the events that describe platform infrastructure
// rather than one customer's own resources. Only a webhook owned by an
// administrator may subscribe to them: a channel outage is not a regular
// user's business, and leaking channel ids and upstream error text to one
// would disclose other tenants' routing.
var webhookAdminEvents = []string{
	WebhookEventChannelDown,
	WebhookEventChannelRecovered,
}

// IsWebhookEvent reports whether name is a subscribable event type.
func IsWebhookEvent(name string) bool {
	return slices.Contains(WebhookEvents, name)
}

// IsAdminWebhookEvent reports whether only an administrator may subscribe to
// name.
func IsAdminWebhookEvent(name string) bool {
	return slices.Contains(webhookAdminEvents, name)
}
