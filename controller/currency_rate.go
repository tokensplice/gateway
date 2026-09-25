package controller

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

// GetCurrencyRates lists every stored exchange rate pair (admin only).
func GetCurrencyRates(c *gin.Context) {
	rates, err := model.GetAllCurrencyRates()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, rates)
}

type upsertCurrencyRateRequest struct {
	BaseCurrency   string  `json:"base_currency"`
	TargetCurrency string  `json:"target_currency"`
	Rate           float64 `json:"rate"`
}

// UpsertCurrencyRate adds or updates one pair manually (admin only). Manual
// pairs are authoritative: the periodic auto-refresh never overwrites them.
func UpsertCurrencyRate(c *gin.Context) {
	var request upsertCurrencyRateRequest
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		common.ApiErrorMsg(c, "invalid request body")
		return
	}
	base := common.NormalizeCurrencyCode(request.BaseCurrency)
	if base == "" {
		base = "USD"
	}
	target := common.NormalizeCurrencyCode(request.TargetCurrency)
	if target == "" || !common.IsSupportedCurrency(target) {
		common.ApiErrorMsg(c, "target_currency must be one of: "+strings.Join(common.SupportedCurrencies(), ", "))
		return
	}
	if err := model.UpsertCurrencyRate(base, target, request.Rate, model.CurrencyRateSourceManual, true); err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, true)
}

// DeleteCurrencyRate removes one pair by id (admin only).
func DeleteCurrencyRate(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		common.ApiErrorMsg(c, "invalid rate id")
		return
	}
	if err := model.DeleteCurrencyRateById(id); err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, true)
}

// RefreshCurrencyRatesNow forces an immediate fetch from the exchange rate
// API (admin only), mirroring what the scheduled task does.
func RefreshCurrencyRatesNow(c *gin.Context) {
	updated, err := service.RefreshCurrencyRates(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "currency rate refresh failed: " + err.Error(),
			"data":    gin.H{"updated": updated},
		})
		return
	}
	common.ApiSuccess(c, gin.H{"updated": updated})
}

// GetSupportedCurrencies lists the selectable display currencies for the
// frontend dropdown (public).
func GetSupportedCurrencies(c *gin.Context) {
	supported := make([]gin.H, 0, len(common.SupportedCurrencies()))
	for _, code := range common.SupportedCurrencies() {
		supported = append(supported, gin.H{
			"code":   code,
			"symbol": common.CurrencySymbol(code),
		})
	}
	common.ApiSuccess(c, gin.H{
		"currencies":               supported,
		"default_display_currency": common.DefaultDisplayCurrency,
	})
}
