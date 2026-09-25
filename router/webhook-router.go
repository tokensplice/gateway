package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"

	"github.com/gin-gonic/gin"
)

// registerWebhookRoutes mounts the outbound event webhook API.
//
// Every self-service route is user-scoped: the owner comes from the
// authenticated session inside each handler, never from the request payload.
// DisableCache applies to both groups so the one response that carries a
// signing secret is never stored by a browser or an intermediary cache.
//
// Routes with a literal path segment are registered before the `:id` wildcard
// of the same method, the same ordering the OAuth routes in api-router.go
// rely on, so `/webhook/admin/all` and `/webhook/deliveries/:id/retry` are
// matched literally instead of being read as an id.
//
// The routes that make the deployment dial a user-supplied URL — create,
// update, test, manual retry — additionally carry the critical rate limits
// used by the other self-service credential endpoints, which slows down
// registration-as-port-scan and bulk probing of webhook and delivery ids.
func registerWebhookRoutes(apiRouter *gin.RouterGroup) {
	webhookAdminRoute := apiRouter.Group("/webhook/admin")
	webhookAdminRoute.Use(middleware.AdminAuth())
	webhookAdminRoute.Use(middleware.DisableCache())
	{
		webhookAdminRoute.GET("/all", controller.AdminGetAllWebhooks)
	}

	webhookRoute := apiRouter.Group("/webhook")
	webhookRoute.Use(middleware.UserAuth())
	webhookRoute.Use(middleware.DisableCache())
	{
		webhookRoute.GET("", controller.GetUserWebhooks)
		webhookRoute.POST("", middleware.CriticalRateLimit(), middleware.UserCriticalRateLimit("webhook"), controller.AddWebhook)
		webhookRoute.POST("/deliveries/:id/retry", middleware.CriticalRateLimit(), middleware.UserCriticalRateLimit("webhook"), controller.RetryWebhookDelivery)
		webhookRoute.PUT("/:id", middleware.CriticalRateLimit(), middleware.UserCriticalRateLimit("webhook"), controller.UpdateWebhook)
		webhookRoute.DELETE("/:id", middleware.CriticalRateLimit(), middleware.UserCriticalRateLimit("webhook"), controller.DeleteWebhook)
		webhookRoute.GET("/:id/deliveries", controller.GetWebhookDeliveries)
		webhookRoute.POST("/:id/test", middleware.CriticalRateLimit(), middleware.UserCriticalRateLimit("webhook"), controller.TestWebhook)
	}
}
