package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	promdto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// metricValue reads the current value of a single counter or gauge sample.
//
// prometheus/client_golang ships a testutil package for this, but it depends on
// a module this project does not otherwise use, so the read goes through the
// collector's own Write contract instead of adding a dependency for a test.
func metricValue(t *testing.T, collector prometheus.Collector) float64 {
	t.Helper()
	samples := make(chan prometheus.Metric, 1)
	collector.Collect(samples)
	close(samples)
	sample, ok := <-samples
	require.True(t, ok, "collector produced no sample")
	written := &promdto.Metric{}
	require.NoError(t, sample.Write(written))
	if counter := written.GetCounter(); counter != nil {
		return counter.GetValue()
	}
	if gauge := written.GetGauge(); gauge != nil {
		return gauge.GetValue()
	}
	require.FailNow(t, "collector is neither a counter nor a gauge")
	return 0
}

// seriesCount reports how many label combinations a vector currently holds,
// which is what proves a recorder refused to publish a series.
func seriesCount(collector prometheus.Collector) int {
	samples := make(chan prometheus.Metric, 256)
	collector.Collect(samples)
	close(samples)
	count := 0
	for range samples {
		count++
	}
	return count
}

// The scrape endpoint is reachable without a user session, so its bearer check
// is the only access control it has. These cases pin the contract: an open
// endpoint when no token is configured, a uniform 401 that leaks nothing about
// the configured credential when one is, and a scheme comparison that follows
// RFC 9110 rather than an exact string match.
func TestMetricsEndpointBearerToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const configured = "s3cr3t-scrape-token"

	cases := []struct {
		name         string
		configuredTo string
		header       string
		wantStatus   int
		wantScrape   bool
	}{
		{name: "open endpoint without a header", configuredTo: "", header: "", wantStatus: http.StatusOK, wantScrape: true},
		{name: "open endpoint ignores a bogus header", configuredTo: "", header: "Bearer anything", wantStatus: http.StatusOK, wantScrape: true},
		{name: "protected endpoint rejects a missing header", configuredTo: configured, header: "", wantStatus: http.StatusUnauthorized},
		{name: "protected endpoint rejects a wrong token", configuredTo: configured, header: "Bearer not-the-token", wantStatus: http.StatusUnauthorized},
		{name: "protected endpoint rejects a bare token", configuredTo: configured, header: configured, wantStatus: http.StatusUnauthorized},
		{name: "protected endpoint rejects a non-bearer scheme", configuredTo: configured, header: "Basic " + configured, wantStatus: http.StatusUnauthorized},
		{name: "protected endpoint rejects a token prefix", configuredTo: configured, header: "Bearer s3cr3t", wantStatus: http.StatusUnauthorized},
		{name: "protected endpoint accepts the token", configuredTo: configured, header: "Bearer " + configured, wantStatus: http.StatusOK, wantScrape: true},
		{name: "scheme comparison is case-insensitive", configuredTo: configured, header: "bearer " + configured, wantStatus: http.StatusOK, wantScrape: true},
		{name: "surrounding whitespace is tolerated", configuredTo: configured, header: "Bearer  " + configured + " ", wantStatus: http.StatusOK, wantScrape: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			constant.MetricsToken = tc.configuredTo
			t.Cleanup(func() { constant.MetricsToken = "" })

			router := gin.New()
			router.GET("/metrics", Handler())

			request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			if tc.header != "" {
				request.Header.Set("Authorization", tc.header)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			require.Equal(t, tc.wantStatus, response.Code)
			body := response.Body.String()
			if tc.wantScrape {
				assert.Contains(t, body, "tokensplice_active_connections")
				assert.Contains(t, response.Header().Get("Content-Type"), "text/plain")
			} else {
				assert.Empty(t, body)
				assert.Equal(t, `Bearer realm="metrics", error="invalid_token"`, response.Header().Get("WWW-Authenticate"))
			}
			// Neither the configured credential nor the presented one may be
			// reflected back to the caller.
			assert.NotContains(t, body, configured)
			if tc.header != "" {
				assert.NotContains(t, body, tc.header)
			}
		})
	}
}

// A disabled gateway must not serve the endpoint at all, and neither the
// middleware nor the recorders may publish anything, so turning observability
// off also stops label series from accumulating in memory.
func TestMetricsDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	constant.MetricsEnabled = false
	t.Cleanup(func() { constant.MetricsEnabled = true })

	require.False(t, Enabled())

	router := gin.New()
	router.Use(Middleware())
	router.GET("/metrics", Handler())
	router.GET("/api/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })

	httpBefore := seriesCount(HttpRequestsTotal)
	relayBefore := seriesCount(RelayRequestsTotal)
	channelBefore := seriesCount(ChannelRequestsTotal)
	tokensBefore := seriesCount(TokensProcessedTotal)
	byokBefore := seriesCount(ByokRequestsTotal)
	quotaBefore := metricValue(t, QuotaConsumedTotal)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ping", nil))
	assert.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, "pong", response.Body.String())

	// The route stays mounted so a scraper sees a missing target rather than the
	// frontend catch-all answering 200 with the SPA shell.
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusNotFound, response.Code)
	assert.Empty(t, response.Body.String())

	RecordRelayRequest("gpt-4o", "OpenAI", RouteManaged, time.Second, true, 100, 50)
	RecordChannelRequest(7, "primary", true, time.Second)
	RecordByokRequest("claude-sonnet-4", "anthropic", true, 500, time.Second)
	RecordQuotaConsumed(500)
	SetByokKeysActive(3)

	assert.Equal(t, httpBefore, seriesCount(HttpRequestsTotal), "a disabled middleware must not publish samples")
	assert.Equal(t, relayBefore, seriesCount(RelayRequestsTotal))
	assert.Equal(t, channelBefore, seriesCount(ChannelRequestsTotal))
	assert.Equal(t, tokensBefore, seriesCount(TokensProcessedTotal))
	assert.Equal(t, byokBefore, seriesCount(ByokRequestsTotal))
	assert.InDelta(t, quotaBefore, metricValue(t, QuotaConsumedTotal), 1e-9)
	assert.Zero(t, metricValue(t, ActiveConnections))
}

// The path label is the route pattern, never the raw URL: an unauthenticated
// caller must not be able to create unbounded series by requesting random paths.
func TestMiddlewareLabelsRequestsByRoutePattern(t *testing.T) {
	gin.SetMode(gin.TestMode)
	require.True(t, Enabled())

	router := gin.New()
	router.Use(Middleware())
	router.GET("/api/status", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	router.POST("/api/user/:id", func(c *gin.Context) { c.String(http.StatusTeapot, "ok") })

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	require.Equal(t, http.StatusOK, response.Code)

	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/user/99", nil))
	require.Equal(t, http.StatusTeapot, response.Code)

	// A path no route matches, carrying a value that must never become a label.
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/status/../../etc/passwd", nil))
	require.Equal(t, http.StatusNotFound, response.Code)

	assert.InDelta(t, 1.0, metricValue(t, HttpRequestsTotal.WithLabelValues(http.MethodGet, "/api/status", "200")), 1e-9)
	assert.InDelta(t, 1.0, metricValue(t, HttpRequestsTotal.WithLabelValues(http.MethodPost, "/api/user/:id", "418")), 1e-9)
	assert.InDelta(t, 1.0, metricValue(t, HttpRequestsTotal.WithLabelValues(http.MethodGet, UnmatchedPath, "404")), 1e-9)
	assert.Zero(t, metricValue(t, HttpRequestsTotal.WithLabelValues(http.MethodGet, "/api/status/../../etc/passwd", "404")))

	// In-flight requests are released even though the handler chain aborted.
	assert.Zero(t, metricValue(t, ActiveConnections))
}

// A BYOK shadow channel has a synthetic negative id. Publishing it as a channel
// would invent a health row for a credential that is not a channel at all, so
// the recorder drops it.
func TestChannelRecordersIgnoreShadowAndInvalidChannels(t *testing.T) {
	requestsBefore := seriesCount(ChannelRequestsTotal)
	healthBefore := seriesCount(ChannelUp)

	RecordChannelRequest(constant.ByokShadowChannelId(4242), "BYOK openai #4242", true, 10*time.Millisecond)
	RecordChannelRequest(0, "", false, time.Millisecond)
	SetChannelUp(constant.ByokShadowChannelId(4242), "BYOK openai #4242", true)

	assert.Equal(t, requestsBefore, seriesCount(ChannelRequestsTotal))
	assert.Equal(t, healthBefore, seriesCount(ChannelUp))

	RecordChannelRequest(7, "primary", true, 250*time.Millisecond)
	RecordChannelRequest(7, "primary", false, 50*time.Millisecond)

	assert.Equal(t, requestsBefore+2, seriesCount(ChannelRequestsTotal))
	assert.InDelta(t, 1.0, metricValue(t, ChannelRequestsTotal.WithLabelValues("7", StatusSuccess)), 1e-9)
	assert.InDelta(t, 1.0, metricValue(t, ChannelRequestsTotal.WithLabelValues("7", StatusError)), 1e-9)
	// A failed attempt does not by itself mark a channel down; only an explicit
	// disable or the database snapshot does.
	assert.InDelta(t, 1.0, metricValue(t, ChannelUp.WithLabelValues("7", "primary")), 1e-9)

	SetChannelUp(7, "primary", false)
	assert.Zero(t, metricValue(t, ChannelUp.WithLabelValues("7", "primary")))

	// The periodic snapshot replaces the whole set so a deleted channel loses
	// its series instead of being pinned at its last value.
	ResetChannelHealth()
	assert.Zero(t, seriesCount(ChannelUp))
}

// Route attribution and empty-label normalisation are what make the Grafana
// panels group correctly, so both are pinned here.
func TestRouteAttributionAndLabelNormalisation(t *testing.T) {
	assert.Equal(t, RouteByok, RouteForChannelId(constant.ByokShadowChannelId(1)))
	assert.Equal(t, RouteManaged, RouteForChannelId(1))
	assert.Equal(t, RouteManaged, RouteForChannelId(0))

	assert.Equal(t, unknownLabel, label(""))
	assert.Equal(t, "gpt-4o", label("gpt-4o"))

	RecordRelayRequest("", "", RouteManaged, 1500*time.Millisecond, false, 0, 0)
	assert.InDelta(t, 1.0, metricValue(t, RelayRequestsTotal.WithLabelValues(unknownLabel, unknownLabel, StatusError, RouteManaged)), 1e-9)

	// A request that settled nothing reports no token series rather than zeros.
	tokensBefore := seriesCount(TokensProcessedTotal)
	RecordTokensProcessed("gpt-4o", 0, 0)
	assert.Equal(t, tokensBefore, seriesCount(TokensProcessedTotal))

	RecordTokensProcessed("gpt-4o", 120, 45)
	assert.InDelta(t, 120.0, metricValue(t, TokensProcessedTotal.WithLabelValues("gpt-4o", TokenTypePrompt)), 1e-9)
	assert.InDelta(t, 45.0, metricValue(t, TokensProcessedTotal.WithLabelValues("gpt-4o", TokenTypeCompletion)), 1e-9)
}

// Quota and fee counters must never move backwards: a negative value would panic
// the Prometheus client and take the whole scrape down with it.
func TestQuotaCountersClampNonPositiveValues(t *testing.T) {
	feeBefore := metricValue(t, ByokFeeQuotaTotal)
	quotaBefore := metricValue(t, QuotaConsumedTotal)

	RecordByokRequest("claude-sonnet-4", "anthropic", false, -5, time.Second)
	RecordQuotaConsumed(-100)
	RecordQuotaConsumed(0)

	assert.InDelta(t, feeBefore, metricValue(t, ByokFeeQuotaTotal), 1e-9)
	assert.InDelta(t, quotaBefore, metricValue(t, QuotaConsumedTotal), 1e-9)

	RecordByokRequest("claude-sonnet-4", "anthropic", true, 250, time.Second)
	assert.InDelta(t, feeBefore+250, metricValue(t, ByokFeeQuotaTotal), 1e-9)
	assert.InDelta(t, 1.0, metricValue(t, ByokRequestsTotal.WithLabelValues("claude-sonnet-4", "anthropic", StatusSuccess)), 1e-9)

	RecordQuotaConsumed(900)
	assert.InDelta(t, quotaBefore+900, metricValue(t, QuotaConsumedTotal), 1e-9)
}

// A time to first token is only meaningful for a stream that actually started,
// so a non-positive duration publishes nothing instead of a bogus fast sample.
func TestFirstTokenRejectsNonPositiveDuration(t *testing.T) {
	before := seriesCount(RelayFirstTokenSeconds)

	RecordFirstToken("gpt-4o", "OpenAI", 0)
	RecordFirstToken("gpt-4o", "OpenAI", -time.Second)
	assert.Equal(t, before, seriesCount(RelayFirstTokenSeconds))

	RecordFirstToken("gpt-4o", "OpenAI", 350*time.Millisecond)
	assert.Equal(t, before+1, seriesCount(RelayFirstTokenSeconds))
}
