package model

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/common/idgen"
	"github.com/QuantumNous/new-api/constant"

	"gorm.io/gorm"
)

// Outbound event webhooks. A user registers one or more HTTPS endpoints, picks
// the events each endpoint cares about, and the gateway POSTs a signed JSON
// envelope to it whenever one of those events happens.
//
// This is deliberately separate from the notification webhook in
// relaykit/dto.UserSetting (a single URL that receives human-readable quota and
// channel notices). That one is a message transport chosen per user; this one is
// an integration surface with a machine-readable contract, per-event
// subscriptions, a delivery log, and retries.
//
// Security notes that apply to every function in this file:
//   - Secret is `json:"-"`. It signs deliveries and is returned exactly once,
//     in the create response; no read path ever exposes it again.
//   - Every user-facing lookup is scoped by owner id, taken from the
//     authenticated session rather than from a request payload.
//   - Url is user-controlled and is dialed by a background worker, so it is
//     validated for SSRF at registration and again at dial time (see
//     service.ValidateWebhookURL and the webhook delivery clients).
//
// Identifiers use common/idgen snowflake values, assigned after the struct
// literal because a zero primary key inside the literal makes GORM fall back to
// the driver default.

// ErrWebhookNotFound is returned both for an id that does not exist and for an
// id owned by another user, so webhook ids cannot be probed across accounts.
var ErrWebhookNotFound = errors.New("webhook not found")

// ErrWebhookDeliveryNotFound is the delivery-log counterpart of the above.
var ErrWebhookDeliveryNotFound = errors.New("webhook delivery not found")

// Webhook is one registered notification endpoint owned by one user.
//
// Status carries no GORM default on purpose: an explicit "disabled" is the zero
// value, and a column default would make GORM drop it from the INSERT and store
// an active endpoint instead. InsertWebhook is the single place that fills the
// active state in.
type Webhook struct {
	BaseModel
	UserId      int64     `json:"user_id,string" gorm:"index;not null"`
	Url         string    `json:"url" gorm:"size:500;not null"`
	Events      string    `json:"events" gorm:"size:500;not null"` // comma-separated event types
	Secret      string    `json:"-" gorm:"size:100;not null"`      // HMAC-SHA256 signing secret, never exposed
	Status      int       `json:"status"`                          // 1=active, 0=disabled
	Description string    `json:"description" gorm:"size:200"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// WebhookDelivery is one send attempt series for one event on one webhook. A
// row is created before the first attempt, so an event that could not even be
// queued still shows up in the history with its reason.
//
// NextRetry doubles as the claim token for the retry sweep: a worker clears it
// with a compare-and-set before resending, so two instances cannot both pick up
// the same row.
type WebhookDelivery struct {
	BaseModel
	WebhookId   int64      `json:"webhook_id,string" gorm:"index;not null"`
	UserId      int64      `json:"user_id,string" gorm:"index;not null"`
	EventType   string     `json:"event_type" gorm:"size:50;index;not null"`
	Payload     string     `json:"payload" gorm:"type:text"`
	StatusCode  int        `json:"status_code"`
	Error       string     `json:"error" gorm:"size:500"`
	Attempts    int        `json:"attempts"`
	NextRetry   *time.Time `json:"next_retry" gorm:"index"`
	DeliveredAt *time.Time `json:"delivered_at"`
	CreatedAt   time.Time  `json:"created_at" gorm:"index"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// IsActive reports whether the webhook is eligible to receive events.
func (w *Webhook) IsActive() bool {
	return w.Status == constant.WebhookStatusActive
}

// EventList parses the stored comma-separated subscription.
func (w *Webhook) EventList() []string {
	if w.Events == "" {
		return nil
	}
	events := make([]string, 0, 4)
	for event := range strings.SplitSeq(w.Events, ",") {
		if trimmed := strings.TrimSpace(event); trimmed != "" {
			events = append(events, trimmed)
		}
	}
	return events
}

// Subscribes reports whether the webhook asked for eventType.
func (w *Webhook) Subscribes(eventType string) bool {
	return slices.Contains(w.EventList(), eventType)
}

// NormalizeWebhookEvents validates a requested subscription and renders it in
// the stored form: lower-cased, de-duplicated, sorted, comma-separated. Sorting
// makes two registrations that differ only in order compare equal, and the
// column width bounds the list to the eight known events.
//
// allowAdminEvents reflects the caller's role; the infrastructure events are
// reserved for administrators because they disclose platform routing.
func NormalizeWebhookEvents(events []string, allowAdminEvents bool) (string, error) {
	unique := make([]string, 0, len(events))
	for _, raw := range events {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" || slices.Contains(unique, name) {
			continue
		}
		if !constant.IsWebhookEvent(name) {
			return "", fmt.Errorf("unsupported webhook event %q, expected one of: %s", raw, strings.Join(constant.WebhookEvents, ", "))
		}
		if constant.IsAdminWebhookEvent(name) && !allowAdminEvents {
			return "", fmt.Errorf("webhook event %q is reserved for administrators", name)
		}
		unique = append(unique, name)
	}
	if len(unique) == 0 {
		return "", errors.New("at least one webhook event is required")
	}
	slices.Sort(unique)
	return strings.Join(unique, ","), nil
}

// InsertWebhook persists a new endpoint with an application-generated
// identifier.
func InsertWebhook(webhook *Webhook) error {
	if webhook.UserId <= 0 {
		return errors.New("webhook must belong to a user")
	}
	if webhook.Url == "" {
		return errors.New("webhook url is required")
	}
	if webhook.Secret == "" {
		return errors.New("webhook is missing its signing secret")
	}
	if webhook.Events == "" {
		return errors.New("webhook must subscribe to at least one event")
	}
	if webhook.Status != constant.WebhookStatusActive && webhook.Status != constant.WebhookStatusDisabled {
		return errors.New("webhook status must be 1 (active) or 0 (disabled)")
	}
	webhook.ID = idgen.Next()
	return DB.Create(webhook).Error
}

// GetWebhooksByUserId lists every webhook owned by a user, newest first.
func GetWebhooksByUserId(userId int64) ([]*Webhook, error) {
	var webhooks []*Webhook
	err := DB.Where("user_id = ?", userId).Order("created_at DESC, id DESC").Find(&webhooks).Error
	return webhooks, err
}

// GetWebhookByIdAndUser loads one webhook, scoping the lookup by owner so an id
// belonging to another user is reported as missing.
func GetWebhookByIdAndUser(id int64, userId int64) (*Webhook, error) {
	var webhook Webhook
	err := DB.Where("id = ? AND user_id = ?", id, userId).First(&webhook).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrWebhookNotFound
	}
	if err != nil {
		return nil, err
	}
	return &webhook, nil
}

// GetWebhookById loads one webhook regardless of owner. Only background
// delivery and the admin listing use it; user-facing handlers must go through
// GetWebhookByIdAndUser.
func GetWebhookById(id int64) (*Webhook, error) {
	var webhook Webhook
	err := DB.Where("id = ?", id).First(&webhook).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrWebhookNotFound
	}
	if err != nil {
		return nil, err
	}
	return &webhook, nil
}

// UpdateWebhook applies a set of column updates to one owned webhook and
// returns the stored result.
func UpdateWebhook(id int64, userId int64, updates map[string]any) (*Webhook, error) {
	if len(updates) == 0 {
		return GetWebhookByIdAndUser(id, userId)
	}
	result := DB.Model(&Webhook{}).Where("id = ? AND user_id = ?", id, userId).Updates(updates)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, ErrWebhookNotFound
	}
	return GetWebhookByIdAndUser(id, userId)
}

// DeleteWebhookByIdAndUser removes a webhook together with its delivery
// history, so a deleted endpoint leaves no signed payloads behind.
func DeleteWebhookByIdAndUser(id int64, userId int64) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Where("id = ? AND user_id = ?", id, userId).Delete(&Webhook{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrWebhookNotFound
		}
		return tx.Where("webhook_id = ?", id).Delete(&WebhookDelivery{}).Error
	})
}

// CountWebhooksByUserId backs the per-user registration limit.
func CountWebhooksByUserId(userId int64) (int64, error) {
	var count int64
	err := DB.Model(&Webhook{}).Where("user_id = ?", userId).Count(&count).Error
	return count, err
}

// GetAllWebhooks pages the whole table for the administrator listing and
// returns the total row count beside the page.
func GetAllWebhooks(limit int, offset int) ([]*Webhook, int64, error) {
	var webhooks []*Webhook
	var total int64
	query := DB.Model(&Webhook{})
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := query.Order("created_at DESC, id DESC").Limit(limit).Offset(offset).Find(&webhooks).Error
	return webhooks, total, err
}

// GetActiveWebhooksForEvent returns the endpoints of one user that are active
// and subscribed to eventType. The subscription filter runs in Go rather than
// in SQL: matching inside a comma-separated column would need a LIKE pattern
// per event and a dialect-specific FIND_IN_SET, while a user can register at
// most MaxWebhooksPerUser rows.
func GetActiveWebhooksForEvent(userId int64, eventType string) ([]*Webhook, error) {
	var webhooks []*Webhook
	err := DB.Where("user_id = ? AND status = ?", userId, constant.WebhookStatusActive).
		Order("id ASC").
		Find(&webhooks).Error
	if err != nil {
		return nil, err
	}
	return filterWebhooksByEvent(webhooks, eventType), nil
}

// GetActiveAdminWebhooksForEvent returns the active endpoints subscribed to an
// infrastructure event, restricted to owners who currently hold an
// administrator role. The role is re-checked here on every dispatch instead of
// being trusted from registration time, so demoting an administrator stops
// their channel-health webhooks immediately.
func GetActiveAdminWebhooksForEvent(eventType string) ([]*Webhook, error) {
	adminIds := DB.Model(&User{}).Select("id").Where("role >= ?", common.RoleAdminUser)
	var webhooks []*Webhook
	err := DB.Where("status = ? AND user_id IN (?)", constant.WebhookStatusActive, adminIds).
		Order("id ASC").
		Find(&webhooks).Error
	if err != nil {
		return nil, err
	}
	return filterWebhooksByEvent(webhooks, eventType), nil
}

// GetActiveWebhookOwnersForEvent lists the distinct users who have at least one
// active endpoint subscribed to eventType, newest registration first. The
// expiring-token sweep uses it to visit each interested account once.
func GetActiveWebhookOwnersForEvent(eventType string) ([]int64, error) {
	var webhooks []*Webhook
	err := DB.Select("user_id").
		Where("status = ?", constant.WebhookStatusActive).
		Order("user_id ASC, id ASC").
		Find(&webhooks).Error
	if err != nil {
		return nil, err
	}
	owners := make([]int64, 0, len(webhooks))
	for _, webhook := range webhooks {
		if !webhook.Subscribes(eventType) || slices.Contains(owners, webhook.UserId) {
			continue
		}
		owners = append(owners, webhook.UserId)
	}
	return owners, nil
}

func filterWebhooksByEvent(webhooks []*Webhook, eventType string) []*Webhook {
	matched := make([]*Webhook, 0, len(webhooks))
	for _, webhook := range webhooks {
		if webhook.Subscribes(eventType) {
			matched = append(matched, webhook)
		}
	}
	return matched
}

// InsertWebhookDelivery records one queued event before it is sent.
func InsertWebhookDelivery(delivery *WebhookDelivery) error {
	if delivery.WebhookId <= 0 {
		return errors.New("webhook delivery must reference a webhook")
	}
	if delivery.EventType == "" {
		return errors.New("webhook delivery must carry an event type")
	}
	delivery.ID = idgen.Next()
	return DB.Create(delivery).Error
}

// GetWebhookDeliveries returns a webhook's delivery history, newest first.
func GetWebhookDeliveries(webhookId int64, limit int) ([]*WebhookDelivery, error) {
	var deliveries []*WebhookDelivery
	err := DB.Where("webhook_id = ?", webhookId).
		Order("created_at DESC, id DESC").
		Limit(limit).
		Find(&deliveries).Error
	return deliveries, err
}

// GetWebhookDeliveryById loads one delivery row regardless of owner. It is the
// background worker's entry point; user-facing handlers must go through
// GetWebhookDeliveryByIdAndUser.
func GetWebhookDeliveryById(id int64) (*WebhookDelivery, error) {
	var delivery WebhookDelivery
	err := DB.Where("id = ?", id).First(&delivery).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrWebhookDeliveryNotFound
	}
	if err != nil {
		return nil, err
	}
	return &delivery, nil
}

// GetWebhookDeliveryByIdAndUser loads one delivery row scoped by the owning
// webhook's owner, which is what the manual-retry endpoint needs to prove the
// caller may re-send it.
func GetWebhookDeliveryByIdAndUser(id int64, userId int64) (*WebhookDelivery, error) {
	webhookIds := DB.Model(&Webhook{}).Select("id").Where("user_id = ?", userId)
	var delivery WebhookDelivery
	err := DB.Where("id = ? AND webhook_id IN (?)", id, webhookIds).First(&delivery).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrWebhookDeliveryNotFound
	}
	if err != nil {
		return nil, err
	}
	return &delivery, nil
}

// MarkWebhookDeliverySucceeded records a 2xx response.
func MarkWebhookDeliverySucceeded(id int64, statusCode int, attempts int, deliveredAt time.Time) error {
	return DB.Model(&WebhookDelivery{}).Where("id = ?", id).Updates(map[string]any{
		"status_code":  statusCode,
		"attempts":     attempts,
		"error":        "",
		"next_retry":   nil,
		"delivered_at": deliveredAt,
	}).Error
}

// MarkWebhookDeliveryFailed records an attempt that produced no 2xx. A nil
// nextRetry marks the delivery permanently failed; the message is truncated to
// the column width so a verbose transport error cannot fail the write.
func MarkWebhookDeliveryFailed(id int64, statusCode int, attempts int, message string, nextRetry *time.Time) error {
	return DB.Model(&WebhookDelivery{}).Where("id = ?", id).Updates(map[string]any{
		"status_code": statusCode,
		"attempts":    attempts,
		"error":       truncateWebhookError(message),
		"next_retry":  nextRetry,
	}).Error
}

func truncateWebhookError(message string) string {
	const maxWidth = 500
	if len(message) <= maxWidth {
		return message
	}
	return message[:maxWidth]
}

// RescheduleWebhookDelivery arms a row's next retry without consuming an
// attempt. It is used when the delivery could not be handed to the worker pool
// at all — a saturated queue, a dispatcher that has not started — which is
// local congestion rather than a rejection by the receiver.
func RescheduleWebhookDelivery(id int64, reason string, nextRetry time.Time) error {
	return DB.Model(&WebhookDelivery{}).Where("id = ?", id).Updates(map[string]any{
		"error":      truncateWebhookError(reason),
		"next_retry": nextRetry,
	}).Error
}

// ResetWebhookDelivery clears a delivery's attempt history ahead of a manual
// re-send, so a repaired receiver gets the full retry budget rather than
// inheriting the failure count of the outage that broke it.
func ResetWebhookDelivery(id int64) error {
	return DB.Model(&WebhookDelivery{}).Where("id = ?", id).Updates(map[string]any{
		"status_code":  0,
		"attempts":     0,
		"error":        "manual retry requested",
		"next_retry":   nil,
		"delivered_at": nil,
	}).Error
}

// GetDueWebhookRetries lists deliveries whose scheduled retry has come due. The
// sweep restarts deliveries a process lost between a failed attempt and its
// timer, so it is bounded and ordered by the earliest schedule first.
func GetDueWebhookRetries(now time.Time, limit int) ([]*WebhookDelivery, error) {
	var deliveries []*WebhookDelivery
	err := DB.Where("delivered_at IS NULL AND next_retry IS NOT NULL AND next_retry <= ?", now).
		Order("next_retry ASC, id ASC").
		Limit(limit).
		Find(&deliveries).Error
	return deliveries, err
}

// ClaimWebhookDeliveryRetry atomically clears a due row's next_retry and
// reports whether this caller won it. The compare-and-set on the primary key is
// what keeps two instances from sending the same retry twice, and it is plain
// SQL that behaves identically on SQLite, MySQL, and PostgreSQL.
func ClaimWebhookDeliveryRetry(id int64, now time.Time) (bool, error) {
	result := DB.Model(&WebhookDelivery{}).
		Where("id = ? AND delivered_at IS NULL AND next_retry IS NOT NULL AND next_retry <= ?", id, now).
		Update("next_retry", nil)
	return result.RowsAffected == 1, result.Error
}

// DeleteWebhookDeliveriesBefore drops delivery rows older than the retention
// cutoff and reports how many were removed.
func DeleteWebhookDeliveriesBefore(cutoff time.Time) (int64, error) {
	result := DB.Where("created_at < ?", cutoff).Delete(&WebhookDelivery{})
	return result.RowsAffected, result.Error
}

// GetRecentWebhookEventDeliveries returns the payloads a user's endpoints
// already received for one event since a cutoff. The expiring-token sweep uses
// it to avoid re-announcing the same token on every run.
func GetRecentWebhookEventDeliveries(userId int64, eventType string, since time.Time) ([]*WebhookDelivery, error) {
	var deliveries []*WebhookDelivery
	err := DB.Where("user_id = ? AND event_type = ? AND created_at >= ?", userId, eventType, since).
		Order("created_at DESC, id DESC").
		Limit(500).
		Find(&deliveries).Error
	return deliveries, err
}
