package middleware

import (
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/gin-gonic/gin"
)

// CurrencyQuotaHeaders adds display-currency response headers for the
// authenticated user's wallet balance (rc.3 multi-currency display):
//
//	X-Quota-USD      remaining quota converted to USD
//	X-Quota-Display  remaining quota converted to the user's preferred currency
//	X-Display-Currency   the ISO code used for X-Quota-Display
//	X-Quota-Display-Reliable  "false" when the rate is a stale/unreliable fallback
//
// The headers are purely informational; billing stays in quota. Must run
// after the auth middleware so the user quota/setting context keys are set.
func CurrencyQuotaHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		quota, hasQuota := common.GetContextKeyType[int](c, constant.ContextKeyUserQuota)
		if hasQuota {
			c.Header("X-Quota-USD", strconv.FormatFloat(float64(quota)/common.QuotaPerUnit, 'f', 6, 64))

			currency := common.DefaultDisplayCurrency
			if userSetting, ok := common.GetContextKeyType[dto.UserSetting](c, constant.ContextKeyUserSetting); ok && userSetting.PreferredCurrency != "" {
				currency = userSetting.PreferredCurrency
			}
			displayAmount, displayCurrency, rateOK := common.ConvertQuotaToCurrency(int64(quota), currency)
			c.Header("X-Quota-Display", strconv.FormatFloat(displayAmount, 'f', 6, 64))
			c.Header("X-Display-Currency", displayCurrency)
			c.Header("X-Quota-Display-Reliable", strconv.FormatBool(rateOK))
		}
		c.Next()
	}
}
