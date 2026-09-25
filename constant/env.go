package constant

var StreamingTimeout int
var DifyDebug bool
var MaxFileDownloadMB int
var StreamScannerMaxBufferMB int
var ForceStreamOption bool
var CountToken bool
var GetMediaToken bool
var GetMediaTokenNotStream bool
var UpdateTask bool
var MaxRequestBodyMB int
var AnonymousRequestBodyLimitKB int
var AzureDefaultAPIVersion string
var NotifyLimitCount int
var NotificationLimitDurationMinute int
var GenerateDefaultToken bool
var ErrorLogEnabled bool
var TaskQueryLimit int
var TaskTimeoutMinutes int
var TaskPollMaxFailures = 20
var TaskPluginProtocolTimeoutSeconds int
var TaskPluginProtocolTickMilliseconds int
var TaskPluginProtocolTickJitterMilliseconds int
var TaskPluginProtocolHeartbeatSeconds int

// MetricsEnabled gates the Prometheus /metrics endpoint and the request
// metrics middleware. Default on: observability is expected in production.
var MetricsEnabled = true

// MetricsToken is the bearer credential a scraper must present to read
// /metrics. Empty means the endpoint is open, which is only acceptable when it
// is scraped inside a trusted network.
var MetricsToken string

// TokenDefaultRateLimitRPM is the deployment-wide fallback for a token's
// requests-per-minute limit, used when the token itself leaves RateLimitRPM at
// zero. Zero means tokens without an explicit limit are not request throttled.
var TokenDefaultRateLimitRPM int

// TokenSpendingCacheSeconds bounds how long a per-token daily/monthly spending
// total is served from cache before it is recomputed from the consume logs.
var TokenSpendingCacheSeconds = 60

// WebhookEnabled is the master switch for the outbound event webhook system.
// Default on: a deployment that never registers a webhook pays one indexed
// lookup per event and nothing else.
var WebhookEnabled = true

// WebhookMaxWorkers sizes the goroutine pool that delivers webhook events and
// the queue that feeds it. A full queue never blocks the caller: the delivery
// row keeps its scheduled retry and the sweep re-queues it later.
var WebhookMaxWorkers = 10

// WebhookTimeoutSeconds is the whole-request timeout of one delivery attempt,
// covering DNS, connect, TLS, write, and the receiver's response headers.
var WebhookTimeoutSeconds = 10

// WebhookMaxRetries is how many times a failed delivery is retried after the
// first attempt, so the default of 5 allows up to 6 sends spaced by the
// exponential backoff schedule. Zero disables retrying.
var WebhookMaxRetries = 5

// WebhookDeliveryRetentionDays is how long webhook_deliveries rows are kept
// before the maintenance task deletes them.
var WebhookDeliveryRetentionDays = 30

// temporary variable for sora patch, will be removed in future
var TaskPricePatches []string

// TrustedRedirectDomains is a list of trusted domains for redirect URL validation.
// Domains support subdomain matching (e.g., "example.com" matches "sub.example.com").
var TrustedRedirectDomains []string
