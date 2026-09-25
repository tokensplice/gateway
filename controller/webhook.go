package controller

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	hosttypes "github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// Outbound event webhook management API.
//
// Every user-facing handler is mounted behind middleware.UserAuth() and derives
// the owner from the authenticated session (c.GetInt("id")); an owner id is
// never read from the request body or a query parameter, and every model call
// is scoped by that owner so one user cannot list, edit, test, or delete
// another user's endpoint or replay another user's delivery.
//
// The signing secret exists in exactly two places: the stored row, whose Secret
// field is `json:"-"`, and the create response. No read path returns it again,
// so a leaked list response cannot be used to forge signatures.

const (
	webhookDeliveryDefaultLimit = 20
	webhookDeliveryMaxLimit     = 100
	webhookAdminDefaultLimit    = 50
	webhookAdminMaxLimit        = 200
)

// webhookRequest is the create/update payload. Url, Description, and Status are
// pointers on update so an absent field is distinguishable from an explicitly
// empty one: a PUT that omits "url" must leave the stored endpoint untouched.
type webhookRequest struct {
	Url         *string  `json:"url"`
	Events      []string `json:"events"`
	Description *string  `json:"description"`
	Status      *int     `json:"status"`
}

// webhookView is the API representation of a stored endpoint. It is built field
// by field on purpose: returning the model struct directly would put the
// signing secret one forgotten json tag away from the response.
type webhookView struct {
	Id          hosttypes.FlexInt64 `json:"id"`
	UserId      int64               `json:"user_id,omitempty"`
	Url         string              `json:"url"`
	Events      []string            `json:"events"`
	Status      int                 `json:"status"`
	Description string              `json:"description"`
	CreatedAt   time.Time           `json:"created_at"`
	UpdatedAt   time.Time           `json:"updated_at"`
}

func newWebhookView(webhook *model.Webhook, includeOwner bool) webhookView {
	events := webhook.EventList()
	if events == nil {
		events = []string{}
	}
	view := webhookView{
		Id:          hosttypes.NewFlexInt64(webhook.ID),
		Url:         webhook.Url,
		Events:      events,
		Status:      webhook.Status,
		Description: webhook.Description,
		CreatedAt:   webhook.CreatedAt,
		UpdatedAt:   webhook.UpdatedAt,
	}
	if includeOwner {
		view.UserId = webhook.UserId
	}
	return view
}

// webhookCreateView adds the signing secret to the create response. This is the
// only response that carries it.
type webhookCreateView struct {
	webhookView
	Secret string `json:"secret"`
}

type webhookDeliveryView struct {
	Id          hosttypes.FlexInt64 `json:"id"`
	WebhookId   hosttypes.FlexInt64 `json:"webhook_id"`
	EventType   string              `json:"event_type"`
	Payload     string              `json:"payload"`
	StatusCode  int                 `json:"status_code"`
	Error       string              `json:"error,omitempty"`
	Attempts    int                 `json:"attempts"`
	NextRetry   *time.Time          `json:"next_retry"`
	DeliveredAt *time.Time          `json:"delivered_at"`
	CreatedAt   time.Time           `json:"created_at"`
}

func newWebhookDeliveryView(delivery *model.WebhookDelivery) webhookDeliveryView {
	return webhookDeliveryView{
		Id:          hosttypes.NewFlexInt64(delivery.ID),
		WebhookId:   hosttypes.NewFlexInt64(delivery.WebhookId),
		EventType:   delivery.EventType,
		Payload:     delivery.Payload,
		StatusCode:  delivery.StatusCode,
		Error:       delivery.Error,
		Attempts:    delivery.Attempts,
		NextRetry:   delivery.NextRetry,
		DeliveredAt: delivery.DeliveredAt,
		CreatedAt:   delivery.CreatedAt,
	}
}

// webhookDeliveryResult is the shared shape of the two endpoints that send a
// delivery inline. A receiver that answers 500 is a normal result there, not an
// API failure, so the outcome is reported through the delivery itself.
func webhookDeliveryResult(delivery *model.WebhookDelivery) gin.H {
	return gin.H{
		"delivery":    newWebhookDeliveryView(delivery),
		"delivered":   delivery.DeliveredAt != nil,
		"status_code": delivery.StatusCode,
		"attempts":    delivery.Attempts,
		"error":       delivery.Error,
	}
}

// GetUserWebhooks lists the authenticated user's endpoints.
func GetUserWebhooks(c *gin.Context) {
	userId, ok := webhookOwnerId(c)
	if !ok {
		return
	}
	webhooks, err := model.GetWebhooksByUserId(userId)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	views := make([]webhookView, 0, len(webhooks))
	for _, webhook := range webhooks {
		views = append(views, newWebhookView(webhook, false))
	}
	common.ApiSuccess(c, views)
}

// AddWebhook registers a new endpoint for the authenticated user and returns
// its signing secret once.
func AddWebhook(c *gin.Context) {
	userId, ok := webhookOwnerId(c)
	if !ok {
		return
	}
	if !constant.WebhookEnabled {
		common.ApiErrorMsg(c, "webhook events are disabled on this deployment")
		return
	}

	count, err := model.CountWebhooksByUserId(userId)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if count >= constant.MaxWebhooksPerUser {
		common.ApiErrorMsg(c, "webhook limit reached: a user may register at most "+strconv.Itoa(constant.MaxWebhooksPerUser)+" endpoints")
		return
	}

	var request webhookRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		common.ApiError(c, err)
		return
	}
	urlValue := ""
	if request.Url != nil {
		urlValue = *request.Url
	}
	// An administrator may point an endpoint at an internal collector and may
	// subscribe to the channel-health events; every other user is restricted to
	// public addresses and tenant-scoped events, because the gateway dials these
	// URLs from inside the deployment's own network.
	isAdmin := webhookCallerIsAdmin(c)
	if err := service.ValidateWebhookURL(urlValue, isAdmin); err != nil {
		common.ApiError(c, err)
		return
	}
	events, err := model.NormalizeWebhookEvents(request.Events, isAdmin)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	description, err := webhookDescription(request.Description)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	secret, err := service.GenerateWebhookSecret()
	if err != nil {
		common.ApiError(c, err)
		return
	}

	webhook := &model.Webhook{
		UserId:      userId,
		Url:         strings.TrimSpace(urlValue),
		Events:      events,
		Secret:      secret,
		Status:      constant.WebhookStatusActive,
		Description: description,
	}
	if request.Status != nil && *request.Status == constant.WebhookStatusDisabled {
		webhook.Status = constant.WebhookStatusDisabled
	}
	if err := model.InsertWebhook(webhook); err != nil {
		common.ApiError(c, err)
		return
	}

	common.ApiSuccess(c, webhookCreateView{
		webhookView: newWebhookView(webhook, false),
		Secret:      secret,
	})
}

// UpdateWebhook changes an endpoint's URL, subscription, description, or
// enabled state. The signing secret is never part of an update: rotating it
// means deleting the endpoint and registering a new one, so a secret can never
// be replaced by a value an attacker chose.
func UpdateWebhook(c *gin.Context) {
	userId, ok := webhookOwnerId(c)
	if !ok {
		return
	}
	webhookId, err := parseWebhookId(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	var request webhookRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		common.ApiError(c, err)
		return
	}
	if request.Url == nil && request.Events == nil && request.Description == nil && request.Status == nil {
		common.ApiErrorMsg(c, "nothing to update: provide \"url\", \"events\", \"description\", or \"status\"")
		return
	}

	isAdmin := webhookCallerIsAdmin(c)
	updates := map[string]any{}
	if request.Url != nil {
		if err := service.ValidateWebhookURL(*request.Url, isAdmin); err != nil {
			common.ApiError(c, err)
			return
		}
		updates["url"] = strings.TrimSpace(*request.Url)
	}
	if request.Events != nil {
		events, err := model.NormalizeWebhookEvents(request.Events, isAdmin)
		if err != nil {
			common.ApiError(c, err)
			return
		}
		updates["events"] = events
	}
	if request.Description != nil {
		description, err := webhookDescription(request.Description)
		if err != nil {
			common.ApiError(c, err)
			return
		}
		updates["description"] = description
	}
	if request.Status != nil {
		if *request.Status != constant.WebhookStatusActive && *request.Status != constant.WebhookStatusDisabled {
			common.ApiErrorMsg(c, "status must be 1 (active) or 0 (disabled)")
			return
		}
		updates["status"] = *request.Status
	}

	updated, err := model.UpdateWebhook(webhookId, userId, updates)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, newWebhookView(updated, false))
}

// DeleteWebhook removes an endpoint together with its delivery history.
func DeleteWebhook(c *gin.Context) {
	userId, ok := webhookOwnerId(c)
	if !ok {
		return
	}
	webhookId, err := parseWebhookId(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if err := model.DeleteWebhookByIdAndUser(webhookId, userId); err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}

// GetWebhookDeliveries pages one endpoint's delivery history, newest first.
func GetWebhookDeliveries(c *gin.Context) {
	userId, ok := webhookOwnerId(c)
	if !ok {
		return
	}
	webhookId, err := parseWebhookId(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	// Proving ownership before reading the log is what stops a delivery id
	// guess from disclosing another user's payload.
	if _, err := model.GetWebhookByIdAndUser(webhookId, userId); err != nil {
		common.ApiError(c, err)
		return
	}

	limit := webhookDeliveryDefaultLimit
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 1 {
			common.ApiErrorMsg(c, "limit must be a positive integer")
			return
		}
		limit = min(parsed, webhookDeliveryMaxLimit)
	}

	deliveries, err := model.GetWebhookDeliveries(webhookId, limit)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	views := make([]webhookDeliveryView, 0, len(deliveries))
	for _, delivery := range deliveries {
		views = append(views, newWebhookDeliveryView(delivery))
	}
	common.ApiSuccess(c, views)
}

// TestWebhook sends a webhook.test event to one of the caller's endpoints and
// reports the receiver's answer inline.
func TestWebhook(c *gin.Context) {
	userId, ok := webhookOwnerId(c)
	if !ok {
		return
	}
	webhookId, err := parseWebhookId(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	webhook, err := model.GetWebhookByIdAndUser(webhookId, userId)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	delivery, err := service.SendWebhookTestEvent(webhook)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, webhookDeliveryResult(delivery))
}

// RetryWebhookDelivery re-sends one of the caller's deliveries. The ownership
// check runs through the webhook that owns the row, so a delivery id from
// another account is reported as missing rather than forbidden.
func RetryWebhookDelivery(c *gin.Context) {
	userId, ok := webhookOwnerId(c)
	if !ok {
		return
	}
	deliveryId, err := parseWebhookId(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	delivery, err := model.GetWebhookDeliveryByIdAndUser(deliveryId, userId)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if err := service.RetryWebhookDelivery(delivery); err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, webhookDeliveryResult(delivery))
}

// AdminGetAllWebhooks lists every registered endpoint across all users. It
// never exposes a signing secret, so an administrator can audit which events
// leave the deployment and where they go without being able to forge them.
func AdminGetAllWebhooks(c *gin.Context) {
	limit := webhookAdminDefaultLimit
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			common.ApiErrorMsg(c, "limit must be a positive integer")
			return
		}
		limit = min(parsed, webhookAdminMaxLimit)
	}
	offset := 0
	if raw := strings.TrimSpace(c.Query("offset")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			common.ApiErrorMsg(c, "offset must be a non-negative integer")
			return
		}
		offset = parsed
	}

	webhooks, total, err := model.GetAllWebhooks(limit, offset)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	views := make([]webhookView, 0, len(webhooks))
	for _, webhook := range webhooks {
		views = append(views, newWebhookView(webhook, true))
	}
	common.ApiSuccess(c, gin.H{
		"items":  views,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// webhookOwnerId reads the authenticated owner and rejects an unauthenticated
// context. The id always comes from the session set by the auth middleware.
func webhookOwnerId(c *gin.Context) (int64, bool) {
	userId := int64(c.GetInt("id"))
	if userId <= 0 {
		common.ApiErrorMsg(c, "unauthenticated")
		return 0, false
	}
	return userId, true
}

func webhookCallerIsAdmin(c *gin.Context) bool {
	return c.GetInt("role") >= common.RoleAdminUser
}

func webhookDescription(description *string) (string, error) {
	if description == nil {
		return "", nil
	}
	return service.ValidateWebhookDescription(*description)
}

// parseWebhookId reads the :id path parameter. Only a positive decimal integer
// is accepted, matching the string form the API emits for snowflake ids.
func parseWebhookId(c *gin.Context) (int64, error) {
	raw := strings.TrimSpace(c.Param("id"))
	if raw == "" {
		return 0, errors.New("missing id")
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}
