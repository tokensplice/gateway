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

// temporary variable for sora patch, will be removed in future
var TaskPricePatches []string

// TrustedRedirectDomains is a list of trusted domains for redirect URL validation.
// Domains support subdomain matching (e.g., "example.com" matches "sub.example.com").
var TrustedRedirectDomains []string
