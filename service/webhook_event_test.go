package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Outbound event webhook regressions.
//
// These cover the four contracts a receiver and an operator depend on: which
// URLs may be registered (SSRF), what a delivery looks like on the wire
// (body, headers, signature), which endpoints an event reaches (subscription
// and role scoping), and how a failing delivery is booked (attempt budget and
// the claim that keeps two instances from double-sending).
//
// The delivery worker pool is deliberately not started here. A delivery row is
// therefore closed out as undeliverable by the dispatcher-is-not-running path,
// which is asserted where it matters instead of being hidden behind a sleep.

const (
	webhookTestCommonUserId = int64(990001)
	webhookTestAdminUserId  = int64(990002)
)

func seedWebhookFixtures(t *testing.T) {
	t.Helper()
	cleanupWebhookFixtures(t)
	require.NoError(t, model.DB.Create(&model.User{
		Id:       int(webhookTestCommonUserId),
		Username: "webhook-common",
		Password: "not-a-real-password",
		AffCode:  "whcommon",
		Role:     common.RoleCommonUser,
		Status:   common.UserStatusEnabled,
	}).Error)
	require.NoError(t, model.DB.Create(&model.User{
		Id:       int(webhookTestAdminUserId),
		Username: "webhook-admin",
		Password: "not-a-real-password",
		AffCode:  "whadmin",
		Role:     common.RoleAdminUser,
		Status:   common.UserStatusEnabled,
	}).Error)
	t.Cleanup(func() { cleanupWebhookFixtures(t) })
}

func cleanupWebhookFixtures(t *testing.T) {
	t.Helper()
	// Hard delete: users soft-delete, and a soft-deleted row would keep the
	// username unique index occupied for the next test that seeds the same id.
	require.NoError(t, model.DB.Unscoped().Where("username IN ?", []string{"webhook-common", "webhook-admin"}).Delete(&model.User{}).Error)
	require.NoError(t, model.DB.Where("user_id IN ?", []int64{webhookTestCommonUserId, webhookTestAdminUserId}).Delete(&model.WebhookDelivery{}).Error)
	require.NoError(t, model.DB.Where("user_id IN ?", []int64{webhookTestCommonUserId, webhookTestAdminUserId}).Delete(&model.Webhook{}).Error)
}

func seedWebhook(t *testing.T, userId int64, url string, events string, status int) *model.Webhook {
	t.Helper()
	webhook := &model.Webhook{
		UserId:      userId,
		Url:         url,
		Events:      events,
		Secret:      "seed-secret",
		Status:      status,
		Description: "fixture",
	}
	require.NoError(t, model.InsertWebhook(webhook))
	return webhook
}

func TestValidateWebhookURLRegistrationPolicy(t *testing.T) {
	for _, tc := range []struct {
		name         string
		url          string
		allowPrivate bool
		accepted     bool
	}{
		{name: "public https endpoint", url: "https://example.com/hooks/tokensplice", accepted: true},
		{name: "public http endpoint on a non-standard port", url: "http://93.184.216.34:9000/hook", accepted: true},
		{name: "loopback is refused", url: "http://127.0.0.1/hook", accepted: false},
		{name: "loopback with a port is refused", url: "http://127.0.0.1:8080/hook", accepted: false},
		{name: "rfc1918 10/8 is refused", url: "http://10.1.2.3/hook", accepted: false},
		{name: "rfc1918 172.16/12 is refused", url: "http://172.16.5.4/hook", accepted: false},
		{name: "rfc1918 192.168/16 is refused", url: "http://192.168.0.10/hook", accepted: false},
		{name: "cloud metadata address is refused", url: "http://169.254.169.254/latest/meta-data", accepted: false},
		{name: "carrier-grade nat is refused", url: "http://100.64.0.1/hook", accepted: false},
		{name: "ipv6 loopback is refused", url: "http://[::1]/hook", accepted: false},
		{name: "unspecified address is refused", url: "http://0.0.0.0/hook", accepted: false},
		{name: "non-http scheme is refused", url: "ftp://example.com/hook", accepted: false},
		{name: "file scheme is refused", url: "file:///etc/passwd", accepted: false},
		{name: "missing host is refused", url: "https:///hook", accepted: false},
		{name: "empty url is refused", url: "   ", accepted: false},
		{name: "over-long url is refused", url: "https://example.com/" + strings.Repeat("a", maxWebhookURLLength), accepted: false},
		// The administrator exemption lifts the private-network block only; the
		// deployment's own port policy still applies to their endpoints.
		{name: "administrator may target loopback on an allowed port", url: "http://127.0.0.1/hook", allowPrivate: true, accepted: true},
		{name: "administrator may target an internal collector", url: "http://10.0.0.5:8080/collect", allowPrivate: true, accepted: true},
		{name: "administrator is still bound by the configured port allow-list", url: "http://127.0.0.1:9999/hook", allowPrivate: true, accepted: false},
		{name: "administrator may not use a non-http scheme either", url: "ftp://127.0.0.1/hook", allowPrivate: true, accepted: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateWebhookURL(tc.url, tc.allowPrivate)
			if tc.accepted {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			// The caller-facing verdict must not disclose what the host resolved
			// to, or registration becomes an internal-DNS oracle for any user.
			assert.NotContains(t, err.Error(), "resolves to")
		})
	}
}

func TestNormalizeWebhookEvents(t *testing.T) {
	for _, tc := range []struct {
		name           string
		events         []string
		allowAdminOnly bool
		expected       string
		expectedErr    string
	}{
		{
			name:     "duplicates are collapsed and the list is sorted",
			events:   []string{"quota.low", " quota.low ", "request.error"},
			expected: "quota.low,request.error",
		},
		{
			name:     "case is normalized",
			events:   []string{"Quota.Exhausted"},
			expected: "quota.exhausted",
		},
		{
			name:        "an unknown event is rejected",
			events:      []string{"quota.low", "billing.surprise"},
			expectedErr: "unsupported webhook event",
		},
		{
			name:        "an empty subscription is rejected",
			events:      []string{"  "},
			expectedErr: "at least one webhook event is required",
		},
		{
			name:        "channel health is reserved for administrators",
			events:      []string{"channel.down"},
			expectedErr: "reserved for administrators",
		},
		{
			name:           "an administrator may subscribe to channel health",
			events:         []string{"channel.recovered", "channel.down"},
			allowAdminOnly: true,
			expected:       "channel.down,channel.recovered",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored, err := model.NormalizeWebhookEvents(tc.events, tc.allowAdminOnly)
			if tc.expectedErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.expectedErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.expected, stored)

			// The stored form must round-trip through the matcher the
			// dispatcher uses, so a subscription survives a write and a read.
			webhook := &model.Webhook{Events: stored}
			assert.Equal(t, strings.Split(tc.expected, ","), webhook.EventList())
			for _, event := range strings.Split(tc.expected, ",") {
				assert.True(t, webhook.Subscribes(event))
			}
			assert.False(t, webhook.Subscribes("billing.surprise"))
		})
	}
}

func TestPostWebhookDeliverySignsTheExactBody(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	body := `{"event":"quota.low","timestamp":"2026-09-25T12:00:00Z","data":{"user_id":7,"remaining_quota":100,"threshold":500}}`
	webhook := &model.Webhook{Secret: secret}
	webhook.ID = 4242
	delivery := &model.WebhookDelivery{WebhookId: webhook.ID, EventType: constant.WebhookEventQuotaLow, Payload: body}
	delivery.ID = 909090

	var received *http.Request
	var receivedBody []byte
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Clone(r.Context())
		receivedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()
	webhook.Url = receiver.URL

	client := &http.Client{Timeout: 5 * time.Second}

	t.Run("a 2xx receiver gets a signed request and is reported delivered", func(t *testing.T) {
		statusCode, err := postWebhookDelivery(client, webhook, delivery)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, statusCode)

		require.NotNil(t, received)
		assert.Equal(t, http.MethodPost, received.Method)
		assert.Equal(t, "application/json", received.Header.Get("Content-Type"))
		assert.Equal(t, constant.WebhookEventQuotaLow, received.Header.Get(HeaderWebhookEvent))
		assert.Equal(t, "909090", received.Header.Get(HeaderWebhookDelivery))

		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(body))
		assert.Equal(t, "sha256="+hex.EncodeToString(mac.Sum(nil)), received.Header.Get(HeaderWebhookSignature))

		// The bytes on the wire are the stored payload verbatim: re-encoding
		// would invalidate the signature a receiver computed on the first
		// attempt once the row is retried.
		assert.Equal(t, body, string(receivedBody))
	})

	t.Run("a non-2xx receiver is reported with its status code", func(t *testing.T) {
		failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer failing.Close()
		webhook.Url = failing.URL

		statusCode, err := postWebhookDelivery(client, webhook, delivery)
		require.Error(t, err)
		assert.Equal(t, http.StatusServiceUnavailable, statusCode)
		assert.Contains(t, err.Error(), "503")
	})
}

func TestDispatchEventReachesOnlySubscribedEndpoints(t *testing.T) {
	seedWebhookFixtures(t)

	subscribed := seedWebhook(t, webhookTestCommonUserId, "https://example.com/a", constant.WebhookEventQuotaLow, constant.WebhookStatusActive)
	otherEvent := seedWebhook(t, webhookTestCommonUserId, "https://example.com/b", constant.WebhookEventRequestError, constant.WebhookStatusActive)
	disabled := seedWebhook(t, webhookTestCommonUserId, "https://example.com/c", constant.WebhookEventQuotaLow, constant.WebhookStatusDisabled)
	otherUser := seedWebhook(t, webhookTestAdminUserId, "https://example.com/d", constant.WebhookEventQuotaLow, constant.WebhookStatusActive)
	adminChannel := seedWebhook(t, webhookTestAdminUserId, "https://example.com/e", constant.WebhookEventChannelDown, constant.WebhookStatusActive)

	t.Run("a tenant event reaches only that user's subscribed active endpoints", func(t *testing.T) {
		DispatchEvent(webhookTestCommonUserId, constant.WebhookEventQuotaLow, QuotaLowEventData{
			UserId:         webhookTestCommonUserId,
			RemainingQuota: 120,
			Threshold:      500,
		})

		deliveries := loadWebhookDeliveries(t, subscribed.ID)
		require.Len(t, deliveries, 1)
		assert.Equal(t, constant.WebhookEventQuotaLow, deliveries[0].EventType)

		var envelope WebhookEventEnvelope
		require.NoError(t, common.UnmarshalJsonStr(deliveries[0].Payload, &envelope))
		assert.Equal(t, constant.WebhookEventQuotaLow, envelope.Event)
		assert.NotEmpty(t, envelope.Timestamp)
		// The data survived the round trip through the stored JSON body.
		encoded, err := common.Marshal(envelope.Data)
		require.NoError(t, err)
		assert.JSONEq(t, `{"user_id":990001,"remaining_quota":120,"threshold":500}`, string(encoded))

		assert.Empty(t, loadWebhookDeliveries(t, otherEvent.ID), "an endpoint subscribed to another event must stay silent")
		assert.Empty(t, loadWebhookDeliveries(t, disabled.ID), "a disabled endpoint must stay silent")
		assert.Empty(t, loadWebhookDeliveries(t, otherUser.ID), "another user's endpoint must stay silent")
	})

	t.Run("an infrastructure event reaches only administrator endpoints", func(t *testing.T) {
		DispatchAdminEvent(constant.WebhookEventChannelDown, ChannelDownEventData{
			ChannelId:   31,
			ChannelName: "primary",
			Error:       "upstream returned 503",
		})

		deliveries := loadWebhookDeliveries(t, adminChannel.ID)
		require.Len(t, deliveries, 1)
		assert.Equal(t, constant.WebhookEventChannelDown, deliveries[0].EventType)
		assert.Contains(t, deliveries[0].Payload, `"channel_name":"primary"`)
		assert.Empty(t, loadWebhookDeliveries(t, otherUser.ID), "an endpoint that did not subscribe to channel health must stay silent")
	})

	t.Run("an unknown event type is dropped", func(t *testing.T) {
		DispatchEvent(webhookTestCommonUserId, "billing.surprise", nil)
		assert.Len(t, loadWebhookDeliveries(t, subscribed.ID), 1, "no new row for an event nobody can subscribe to")
	})

	t.Run("webhooks are disabled globally", func(t *testing.T) {
		constant.WebhookEnabled = false
		t.Cleanup(func() { constant.WebhookEnabled = true })

		DispatchEvent(webhookTestCommonUserId, constant.WebhookEventQuotaLow, QuotaLowEventData{UserId: webhookTestCommonUserId})
		assert.Len(t, loadWebhookDeliveries(t, subscribed.ID), 1, "WEBHOOK_ENABLED=false must stop event fan-out")
	})
}

func loadWebhookDeliveries(t *testing.T, webhookId int64) []*model.WebhookDelivery {
	t.Helper()
	deliveries, err := model.GetWebhookDeliveries(webhookId, 50)
	require.NoError(t, err)
	return deliveries
}

func TestRecordWebhookAttemptRetryBudget(t *testing.T) {
	seedWebhookFixtures(t)
	webhook := seedWebhook(t, webhookTestCommonUserId, "https://example.com/retry", constant.WebhookEventQuotaLow, constant.WebhookStatusActive)

	constant.WebhookMaxRetries = 2
	t.Cleanup(func() { constant.WebhookMaxRetries = 5 })

	delivery := &model.WebhookDelivery{
		WebhookId: webhook.ID,
		UserId:    webhook.UserId,
		EventType: constant.WebhookEventQuotaLow,
		Payload:   `{"event":"quota.low"}`,
	}
	require.NoError(t, model.InsertWebhookDelivery(delivery))
	t.Cleanup(func() {
		require.NoError(t, model.DB.Where("webhook_id = ?", webhook.ID).Delete(&model.WebhookDelivery{}).Error)
	})

	failure := errors.New("webhook receiver returned status 503")

	// Attempt 1 fails: retry 1 is armed one second out.
	first := time.Now()
	require.Error(t, recordWebhookAttempt(delivery, http.StatusServiceUnavailable, failure))
	assert.Equal(t, 1, delivery.Attempts)
	require.NotNil(t, delivery.NextRetry)
	assert.WithinDuration(t, first.Add(time.Second), *delivery.NextRetry, 2*time.Second)
	assert.Nil(t, delivery.DeliveredAt)

	stored, err := model.GetWebhookDeliveryById(delivery.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, stored.Attempts)
	assert.Equal(t, http.StatusServiceUnavailable, stored.StatusCode)
	require.NotNil(t, stored.NextRetry, "the retry must be persisted, not only held in memory")

	// Attempt 2 fails: retry 2 moves out to the next backoff step.
	require.Error(t, recordWebhookAttempt(delivery, http.StatusServiceUnavailable, failure))
	assert.Equal(t, 2, delivery.Attempts)
	require.NotNil(t, delivery.NextRetry)
	assert.WithinDuration(t, time.Now().Add(5*time.Second), *delivery.NextRetry, 2*time.Second)

	// Attempt 3 spends the budget: the delivery becomes permanently failed.
	require.Error(t, recordWebhookAttempt(delivery, http.StatusServiceUnavailable, failure))
	assert.Equal(t, 3, delivery.Attempts)
	assert.Nil(t, delivery.NextRetry)
	assert.Contains(t, delivery.Error, "retries exhausted")

	stored, err = model.GetWebhookDeliveryById(delivery.ID)
	require.NoError(t, err)
	assert.Nil(t, stored.NextRetry)
	assert.Contains(t, stored.Error, "retries exhausted")

	// A later success clears the failure state.
	require.NoError(t, recordWebhookAttempt(delivery, http.StatusOK, nil))
	assert.Equal(t, 4, delivery.Attempts)
	require.NotNil(t, delivery.DeliveredAt)
	assert.Empty(t, delivery.Error)

	stored, err = model.GetWebhookDeliveryById(delivery.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.DeliveredAt)
	assert.Empty(t, stored.Error)
}

func TestClaimWebhookDeliveryRetryIsExclusive(t *testing.T) {
	seedWebhookFixtures(t)
	webhook := seedWebhook(t, webhookTestCommonUserId, "https://example.com/claim", constant.WebhookEventQuotaLow, constant.WebhookStatusActive)

	retryAt := time.Now().Add(-time.Minute)
	delivery := &model.WebhookDelivery{
		WebhookId: webhook.ID,
		UserId:    webhook.UserId,
		EventType: constant.WebhookEventQuotaLow,
		Payload:   `{"event":"quota.low"}`,
		Attempts:  1,
		NextRetry: &retryAt,
	}
	require.NoError(t, model.InsertWebhookDelivery(delivery))
	t.Cleanup(func() {
		require.NoError(t, model.DB.Where("webhook_id = ?", webhook.ID).Delete(&model.WebhookDelivery{}).Error)
	})

	now := time.Now()
	due, err := model.GetDueWebhookRetries(now, 100)
	require.NoError(t, err)
	dueIds := make([]int64, 0, len(due))
	for _, row := range due {
		dueIds = append(dueIds, row.ID)
	}
	require.Contains(t, dueIds, delivery.ID, "a delivery whose retry came due must be swept")

	// Exactly one instance may win the row: the claim is what keeps a
	// multi-node deployment from sending the same retry twice.
	claimed, err := model.ClaimWebhookDeliveryRetry(delivery.ID, now)
	require.NoError(t, err)
	assert.True(t, claimed)

	claimedAgain, err := model.ClaimWebhookDeliveryRetry(delivery.ID, now)
	require.NoError(t, err)
	assert.False(t, claimedAgain)

	stored, err := model.GetWebhookDeliveryById(delivery.ID)
	require.NoError(t, err)
	assert.Nil(t, stored.NextRetry, "a claimed retry must be cleared so it is not swept again")
	assert.Equal(t, 1, stored.Attempts, "claiming must not consume an attempt")
}

// TestRunWebhookMaintenancePurgesOldDeliveries pins the retention contract: a
// delivery older than WEBHOOK_DELIVERY_RETENTION_DAYS is deleted and a recent
// one is kept.
func TestRunWebhookMaintenancePurgesOldDeliveries(t *testing.T) {
	seedWebhookFixtures(t)
	webhook := seedWebhook(t, webhookTestCommonUserId, "https://example.com/retention", constant.WebhookEventQuotaLow, constant.WebhookStatusActive)

	old := &model.WebhookDelivery{
		WebhookId: webhook.ID,
		UserId:    webhook.UserId,
		EventType: constant.WebhookEventQuotaLow,
		Payload:   `{"event":"quota.low"}`,
	}
	require.NoError(t, model.InsertWebhookDelivery(old))
	recent := &model.WebhookDelivery{
		WebhookId: webhook.ID,
		UserId:    webhook.UserId,
		EventType: constant.WebhookEventQuotaLow,
		Payload:   `{"event":"quota.low"}`,
	}
	require.NoError(t, model.InsertWebhookDelivery(recent))
	expiredAt := time.Now().AddDate(0, 0, -(constant.WebhookDeliveryRetentionDays + 1))
	require.NoError(t, model.DB.Model(&model.WebhookDelivery{}).Where("id = ?", old.ID).Update("created_at", expiredAt).Error)
	t.Cleanup(func() {
		require.NoError(t, model.DB.Where("webhook_id = ?", webhook.ID).Delete(&model.WebhookDelivery{}).Error)
	})

	_, err := RunWebhookMaintenance(context.Background())
	require.NoError(t, err)

	_, err = model.GetWebhookDeliveryById(old.ID)
	assert.ErrorIs(t, err, model.ErrWebhookDeliveryNotFound, "a delivery past the retention horizon must be purged")
	_, err = model.GetWebhookDeliveryById(recent.ID)
	assert.NoError(t, err, "a delivery inside the retention window must be kept")
}
