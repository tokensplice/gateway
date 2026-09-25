package metrics

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/constant"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Enabled reports whether the gateway publishes metrics at all. When it is
// false the scrape endpoint answers 404, the request middleware is a
// pass-through, and the recorders drop their samples, so disabling observability
// also removes its per-request cost and stops label series from accumulating.
func Enabled() bool {
	return constant.MetricsEnabled
}

// Middleware instruments every HTTP request the gateway answers, relay and
// management API alike. It reports how many requests are in flight and how long
// each one took, labelled by the route pattern rather than the raw URL.
//
// The route pattern is what keeps cardinality bounded: a registered pattern is
// drawn from the router table, while a raw URL is attacker-chosen. Requests no
// route matched are all reported under UnmatchedPath for the same reason.
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !Enabled() {
			c.Next()
			return
		}
		start := time.Now()
		ActiveConnections.Inc()
		// Deferred rather than placed after c.Next so an aborted or panicking
		// handler cannot leave the in-flight count permanently too high.
		defer ActiveConnections.Dec()

		c.Next()

		path := c.FullPath()
		if path == "" {
			path = UnmatchedPath
		}
		statusCode := strconv.Itoa(c.Writer.Status())
		HttpRequestsTotal.WithLabelValues(c.Request.Method, path, statusCode).Inc()
		HttpRequestDurationSeconds.WithLabelValues(c.Request.Method, path, statusCode).Observe(time.Since(start).Seconds())
	}
}

// Handler serves the Prometheus text exposition format.
//
// It is mounted outside every authentication group because a scraper presents
// no user session, so the endpoint carries its own credential check instead:
// when METRICS_TOKEN is set the caller must present it as an HTTP bearer token,
// and when it is empty the endpoint is open, which is only safe for a scrape
// that never leaves the cluster. The metric values themselves contain no
// credential, key material, or user identity — only ids, names, and counts — so
// an unauthorised reader learns operational shape, not secrets.
//
// The bearer comparison is constant-time (OWASP ASVS v4.0.3 V2.8.7, and the
// Authentication Cheat Sheet on verifying credentials without leaking them
// through timing), and the presented token is never logged or echoed back: both
// a missing and a wrong credential produce the same bare 401.
//
// A disabled gateway answers 404 rather than leaving the path to the frontend
// catch-all, which would otherwise reply 200 with the SPA shell and leave a
// scraper reporting a parse error instead of a missing target.
func Handler() gin.HandlerFunc {
	exposition := promhttp.Handler()
	return func(c *gin.Context) {
		if !Enabled() {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		if expected := constant.MetricsToken; expected != "" &&
			!bearerTokenMatches(c.GetHeader("Authorization"), expected) {
			c.Header("WWW-Authenticate", `Bearer realm="metrics", error="invalid_token"`)
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		exposition.ServeHTTP(c.Writer, c.Request)
	}
}

// bearerTokenMatches reports whether an Authorization header carries the
// expected bearer credential. The scheme comparison is case-insensitive as
// RFC 9110 requires; the credential comparison is not, and is constant-time.
func bearerTokenMatches(header string, expected string) bool {
	scheme, presented, found := strings.Cut(strings.TrimSpace(header), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(presented)), []byte(expected)) == 1
}
