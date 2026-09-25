package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"

	"github.com/gin-gonic/gin"
)

// registerByokRoutes mounts the bring-your-own-key management API. Every
// self-service route is user-scoped: the owner comes from the authenticated
// session inside each handler, never from the request payload.
//
// DisableCache applies to the whole group so a credential-bearing response is
// never stored by a browser or intermediary cache. Mutating routes additionally
// get the critical rate limits used by the other self-service credential
// endpoints (see the access-token routes in api-router.go), which slows down
// guessing and bulk probing of key ids, plus ByokKeyAudit for the credential
// lifecycle trail.
func registerByokRoutes(apiRouter *gin.RouterGroup) {
	byokRoute := apiRouter.Group("/byok")
	byokRoute.Use(middleware.UserAuth())
	byokRoute.Use(middleware.DisableCache())

	byokRoute.GET("/usage", controller.GetByokUsage)

	byokKeysRoute := byokRoute.Group("/keys")
	byokKeysRoute.Use(middleware.ByokKeyAudit())
	byokKeysRoute.Use(middleware.CriticalRateLimit(), middleware.UserCriticalRateLimit("byok"))
	{
		byokKeysRoute.GET("", controller.GetByokKeys)
		byokKeysRoute.POST("", controller.AddByokKey)
		byokKeysRoute.PUT("/:id", controller.UpdateByokKey)
		byokKeysRoute.DELETE("/:id", controller.DeleteByokKey)
		byokKeysRoute.POST("/:id/test", controller.TestByokKey)
	}

	// Admin-only: the per-user override of the BYOK platform fee. Registered on
	// apiRouter rather than under byokRoute so it does not inherit UserAuth;
	// AdminAuth is the stricter check and already audits admin writes.
	byokAdminRoute := apiRouter.Group("/byok/admin")
	byokAdminRoute.Use(middleware.AdminAuth())
	byokAdminRoute.Use(middleware.DisableCache())
	{
		byokAdminRoute.PUT("/users/:id/fee", controller.AdminSetUserByokFee)
	}
}
