package router

import (
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/pkg/metrics"

	"github.com/gin-gonic/gin"
)

// SetMetricsRouter mounts the Prometheus scrape endpoint on the main HTTP
// server, on the same port as the API.
//
// It is registered before every other route group and outside all of them: a
// scraper presents no user session or API token, so the endpoint must not
// inherit TokenAuth, UserAuth, or the API group's gzip and body limits. Its own
// protection is the optional METRICS_TOKEN bearer check inside metrics.Handler,
// plus the web rate limit, which bounds how often an unauthenticated caller on
// an open endpoint can force a gather.
//
// The route is mounted even when METRICS_ENABLED is false, and answers 404 in
// that case. Leaving the path unregistered would hand it to the frontend
// catch-all, which replies 200 with the SPA shell and leaves a scraper reporting
// a parse error instead of a target that is simply not there.
func SetMetricsRouter(router *gin.Engine) {
	metricsRoute := router.Group("/metrics")
	metricsRoute.Use(middleware.RouteTag("metrics"))
	metricsRoute.Use(middleware.GlobalWebRateLimit())
	metricsRoute.GET("", metrics.Handler())
}
