package router

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"

	"github.com/gin-gonic/gin"
)

func SetRouter(router *gin.Engine, assets WebAssets) {
	// The Prometheus scrape endpoint goes first: it sits outside every
	// authentication group, and registering it before the web router keeps the
	// frontend's catch-all from ever shadowing it.
	SetMetricsRouter(router)
	SetApiRouter(router)
	SetDashboardRouter(router)
	SetRelayRouter(router)
	SetTaskPluginProtocolRouter(router)
	SetVideoRouter(router)
	SetTaskRouter(router)
	pluginDispatcher := SetPluginRouter(router)
	frontendBaseUrl := os.Getenv("FRONTEND_BASE_URL")
	if common.IsMasterNode && frontendBaseUrl != "" {
		frontendBaseUrl = ""
		common.SysLog("FRONTEND_BASE_URL is ignored on master node")
	}
	if frontendBaseUrl == "" {
		SetWebRouter(router, assets, pluginDispatcher)
	} else {
		frontendBaseUrl = strings.TrimSuffix(frontendBaseUrl, "/")
		router.NoRoute(
			pluginDispatcher,
			middleware.RouteTag("web"),
			middleware.AccessTokenAudit(),
			func(c *gin.Context) {
				c.Redirect(http.StatusMovedPermanently, fmt.Sprintf("%s%s", frontendBaseUrl, c.Request.RequestURI))
			},
		)
	}
}
