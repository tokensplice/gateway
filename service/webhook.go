package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/system_setting"
	hosttypes "github.com/QuantumNous/new-api/types"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
)

// WebhookPayload webhook 通知的负载数据
type WebhookPayload struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Values    []any  `json:"values,omitempty"`
	Timestamp int64  `json:"timestamp"`
}

// generateSignature 生成 webhook 签名
func generateSignature(secret string, payload []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}

// SendWebhookNotify 发送 webhook 通知
func SendWebhookNotify(webhookURL string, secret string, data dto.Notify) error {
	// 处理占位符
	content := data.Content
	for _, value := range data.Values {
		content = fmt.Sprintf(content, value)
	}

	// 构建 webhook 负载
	payload := WebhookPayload{
		Type:      data.Type,
		Title:     data.Title,
		Content:   content,
		Values:    data.Values,
		Timestamp: time.Now().Unix(),
	}

	// 序列化负载
	payloadBytes, err := common.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal webhook payload: %v", err)
	}

	// 创建 HTTP 请求
	var req *http.Request
	var resp *http.Response

	if system_setting.EnableWorker() {
		// 构建worker请求数据
		workerReq := &WorkerRequest{
			URL:    webhookURL,
			Key:    system_setting.WorkerValidKey,
			Method: http.MethodPost,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body: payloadBytes,
		}

		// 如果有secret，添加签名到headers
		if secret != "" {
			signature := generateSignature(secret, payloadBytes)
			workerReq.Headers["X-Webhook-Signature"] = signature
			workerReq.Headers["Authorization"] = "Bearer " + secret
		}

		resp, err = DoWorkerRequest(workerReq)
		if err != nil {
			return fmt.Errorf("failed to send webhook request through worker: %v", err)
		}
		defer resp.Body.Close()

		// 检查响应状态
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("webhook request failed with status code: %d", resp.StatusCode)
		}
	} else {
		// SSRF防护：验证Webhook URL（非Worker模式）
		if err := ValidateSSRFProtectedFetchURL(webhookURL); err != nil {
			return fmt.Errorf("request reject: %v", err)
		}

		req, err = http.NewRequest(http.MethodPost, webhookURL, bytes.NewBuffer(payloadBytes))
		if err != nil {
			return fmt.Errorf("failed to create webhook request: %v", err)
		}

		// 设置请求头
		req.Header.Set("Content-Type", "application/json")

		// 如果有 secret，生成签名
		if secret != "" {
			signature := generateSignature(secret, payloadBytes)
			req.Header.Set("X-Webhook-Signature", signature)
		}

		// 发送请求
		client := GetSSRFProtectedHTTPClient()
		resp, err = client.Do(req)
		if err != nil {
			return fmt.Errorf("failed to send webhook request: %v", err)
		}
		defer resp.Body.Close()

		// 检查响应状态
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("webhook request failed with status code: %d", resp.StatusCode)
		}
	}

	return nil
}

// Outbound event webhooks.
//
// SendWebhookNotify above is the notification transport configured through a
// user's settings: one URL that receives human-readable notices. What follows
// is the integration surface — a customer registers endpoints, subscribes each
// one to machine-readable event types, and the gateway POSTs a signed JSON
// envelope with a delivery log and retries behind it.
//
// Delivery never runs on a request path. DispatchEvent records the delivery
// rows and hands their ids to a buffered queue drained by a fixed worker pool;
// when the queue is full the row keeps a scheduled retry and the sweep
// re-queues it, so saturation delays an event instead of dropping it or
// blocking the caller.

const (
	webhookUserAgent = "TokenSplice-Gateway-Webhook/1.0"

	// HeaderWebhookEvent names the event type, so a receiver can route without
	// parsing the body.
	HeaderWebhookEvent = "X-TokenSplice-Event"
	// HeaderWebhookDelivery names the delivery row, which is what a receiver
	// deduplicates on and what a support request quotes.
	HeaderWebhookDelivery = "X-TokenSplice-Delivery"
	// HeaderWebhookSignature carries "sha256=<hex hmac>" over the exact request
	// body, computed with the webhook's signing secret.
	HeaderWebhookSignature = "X-TokenSplice-Signature"

	webhookSignaturePrefix = "sha256="

	// maxWebhookURLLength matches the url column width. Checking it before the
	// write keeps MySQL, which rejects an over-long value in strict mode, from
	// turning a bad registration into a 500.
	maxWebhookURLLength = 500
	// maxWebhookDescriptionLength matches the description column width.
	maxWebhookDescriptionLength = 200
	// webhookSecretBytes is the signing secret's entropy: 32 random bytes,
	// hex-encoded to 64 characters, returned exactly once at creation.
	webhookSecretBytes = 32

	// webhookRetrySweepInterval is how often the sweeper looks for retries a
	// process lost between a failed attempt and its own timer. The in-process
	// timer carries the normal case, so this only has to be often enough to
	// notice a restart.
	webhookRetrySweepInterval = time.Minute
	// webhookRetrySweepBatch bounds one sweep so a long outage does not turn
	// into an unbounded re-queue.
	webhookRetrySweepBatch = 100
	// webhookRequeueDelay is how far ahead a delivery is rescheduled when the
	// worker queue is full. It is a congestion signal, not a retry, so it does
	// not consume an attempt.
	webhookRequeueDelay = 10 * time.Second
	// webhookMaintenanceTokenLookback is how far back the expiring-token sweep
	// checks its own delivery log before re-announcing a token. It matches the
	// warning window, so a token is announced at most once per window.
	webhookMaintenanceTokenLookback = model.ExpirationWarningDays
)

// webhookRetryBackoff is the delay before retry 1, 2, 3, ... A retry beyond the
// schedule reuses the last step, so raising WEBHOOK_MAX_RETRIES keeps a bounded
// cadence instead of running off the end of the table.
var webhookRetryBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
	2 * time.Minute,
	10 * time.Minute,
}

// WebhookEventEnvelope is the JSON body of every event delivery. Timestamp is
// an RFC 3339 UTC instant; data is the event-specific struct below.
type WebhookEventEnvelope struct {
	Event     string `json:"event"`
	Timestamp string `json:"timestamp"`
	Data      any    `json:"data"`
}

// QuotaLowEventData is the quota.low body. RemainingQuota and Threshold are
// quota units, the internal currency; a receiver converts them with the same
// QuotaPerUnit the dashboard uses.
type QuotaLowEventData struct {
	UserId         int64 `json:"user_id"`
	RemainingQuota int64 `json:"remaining_quota"`
	Threshold      int64 `json:"threshold"`
}

// QuotaExhaustedEventData is the quota.exhausted body.
type QuotaExhaustedEventData struct {
	UserId         int64 `json:"user_id"`
	RemainingQuota int64 `json:"remaining_quota"`
}

// TokenExpiringEventData is the token.expiring body.
type TokenExpiringEventData struct {
	TokenId   int    `json:"token_id"`
	TokenName string `json:"token_name"`
	ExpiresAt string `json:"expires_at"`
}

// ChannelDownEventData is the channel.down body. Error is upstream-derived
// text and is masked the same way the channel-health notification masks it.
type ChannelDownEventData struct {
	ChannelId   int    `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	Error       string `json:"error"`
}

// ChannelRecoveredEventData is the channel.recovered body.
type ChannelRecoveredEventData struct {
	ChannelId   int    `json:"channel_id"`
	ChannelName string `json:"channel_name"`
}

// RequestErrorEventData is the request.error body, raised only after every
// retry for that request has been exhausted.
type RequestErrorEventData struct {
	Model      string `json:"model"`
	ErrorCode  string `json:"error_code"`
	StatusCode int    `json:"status_code"`
	RequestId  string `json:"request_id"`
}

// ByokKeyInvalidEventData is the byok.key_invalid body. ByokKeyId is a
// FlexInt64 because a snowflake exceeds the JavaScript integer boundary.
type ByokKeyInvalidEventData struct {
	ByokKeyId hosttypes.FlexInt64 `json:"byok_key_id"`
	Provider  string              `json:"provider"`
	Error     string              `json:"error"`
}

// WebhookTestData is the webhook.test body, produced only by the test endpoint.
type WebhookTestData struct {
	WebhookId   hosttypes.FlexInt64 `json:"webhook_id"`
	Description string              `json:"description"`
}

// GenerateWebhookSecret returns a fresh signing secret. It is the only source
// of webhook secrets, and the value it returns is shown to the owner exactly
// once, in the create response.
func GenerateWebhookSecret() (string, error) {
	raw := make([]byte, webhookSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("failed to generate webhook secret: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// ValidateWebhookURL checks a registration URL before it is stored.
//
// allowPrivateTarget lifts the loopback/private-network block for an endpoint
// an administrator registers, which is how a deployment points webhooks at an
// internal collector. A non-administrator's endpoint must resolve to a public
// address: the URL is dialed by a background worker inside the deployment's
// network, so without this check any user could scan internal services or read
// a cloud metadata endpoint through the gateway.
func ValidateWebhookURL(rawUrl string, allowPrivateTarget bool) error {
	trimmed := strings.TrimSpace(rawUrl)
	if trimmed == "" {
		return errors.New("webhook url is required")
	}
	if len(trimmed) > maxWebhookURLLength {
		return fmt.Errorf("webhook url must be at most %d characters", maxWebhookURLLength)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("invalid webhook url: %v", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("unsupported webhook url scheme %q, only http and https are allowed", parsed.Scheme)
	}
	if parsed.Host == "" {
		return errors.New("webhook url must include a host")
	}

	protection, enabled, err := webhookTargetProtection(allowPrivateTarget)
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	validationErr := protection.ValidateURL(parsed.String())
	if validationErr == nil {
		return nil
	}
	if allowPrivateTarget {
		// An administrator is trusted with the reason, which names the address
		// the host resolved to.
		return validationErr
	}
	// The same detail would hand any registered user an internal-DNS oracle, so
	// the operator gets it in the log and the caller gets a generic verdict.
	// Only the host is logged: a URL may carry credentials in its userinfo.
	common.SysLog(fmt.Sprintf("webhook: refused to register %s: %s", parsed.Host, validationErr.Error()))
	return errWebhookURLNotPublic
}

// errWebhookURLNotPublic is the caller-facing verdict for a registration that
// failed the SSRF check. It deliberately carries no detail about why.
var errWebhookURLNotPublic = errors.New("webhook url must point at a publicly reachable http or https address")

// ValidateWebhookDescription enforces the description column width.
func ValidateWebhookDescription(description string) (string, error) {
	trimmed := strings.TrimSpace(description)
	if len([]rune(trimmed)) > maxWebhookDescriptionLength {
		return "", fmt.Errorf("webhook description must be at most %d characters", maxWebhookDescriptionLength)
	}
	return trimmed, nil
}

// webhookPublicOnlyProtection blocks loopback, link-local, and private-network
// targets while leaving every public host and port reachable. It is enforced at
// dial time as well as at registration, so a DNS name that starts resolving to
// an internal address after it was accepted still cannot be reached.
var webhookPublicOnlyProtection = &common.SSRFProtection{
	ApplyIPFilterForDomain: true,
}

// webhookTargetProtection resolves the SSRF rule for one endpoint. A regular
// user's endpoint is held to public addresses only. An administrator's endpoint
// keeps every domain, IP, and port filter the deployment configured but is
// allowed to reach a private address, matching how the relay already treats
// operator-managed upstream base URLs.
func webhookTargetProtection(allowPrivateTarget bool) (*common.SSRFProtection, bool, error) {
	if !allowPrivateTarget {
		return webhookPublicOnlyProtection, true, nil
	}
	fetchSetting := system_setting.GetFetchSetting()
	if !fetchSetting.EnableSSRFProtection {
		return nil, false, nil
	}
	protection, err := common.NewSSRFProtectionFromFetchSetting(
		true,
		fetchSetting.DomainFilterMode,
		fetchSetting.IpFilterMode,
		fetchSetting.DomainList,
		fetchSetting.IpList,
		fetchSetting.AllowedPorts,
		fetchSetting.ApplyIPFilterForDomain,
	)
	if err != nil {
		return nil, true, err
	}
	return protection, true, nil
}

var (
	webhookClientOnce     sync.Once
	webhookPublicClient   *http.Client
	webhookInternalClient *http.Client
)

// webhookHTTPClient returns the delivery client for one attempt. An
// administrator's endpoint travels through a client that may reach internal
// addresses; every other endpoint travels through one that refuses private
// networks at dial time.
func webhookHTTPClient(allowPrivateTarget bool) *http.Client {
	webhookClientOnce.Do(func() {
		webhookPublicClient = newWebhookHTTPClient(false)
		webhookInternalClient = newWebhookHTTPClient(true)
	})
	if allowPrivateTarget {
		return webhookInternalClient
	}
	return webhookPublicClient
}

// newWebhookHTTPClient builds a delivery client that enforces one endpoint's
// SSRF rule at dial time.
//
// It deliberately does not reuse the shared fetch client: that client's round
// tripper re-validates every request against the deployment-wide fetch setting,
// which would overrule the per-endpoint rule resolved here — and would either
// let a non-administrator reach internal addresses when fetch protection is
// switched off, or block an administrator's internal collector when it is on.
// The shared dialer guard is reused, so a hostname is checked against the rule
// again for every address it resolves to.
func newWebhookHTTPClient(allowPrivateTarget bool) *http.Client {
	netDialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        common.RelayMaxIdleConns,
		MaxIdleConnsPerHost: common.RelayMaxIdleConnsPerHost,
		IdleConnTimeout:     time.Duration(common.RelayIdleConnTimeout) * time.Second,
		DialContext: (&protectedFetchDialer{
			resolver:    net.DefaultResolver,
			dialContext: netDialer.DialContext,
			getProtection: func() (*common.SSRFProtection, bool, error) {
				return webhookTargetProtection(allowPrivateTarget)
			},
		}).DialContext,
	}
	if common.TLSInsecureSkipVerify {
		transport.TLSClientConfig = common.InsecureTLSConfig
	}
	return &http.Client{
		Transport: transport,
		Timeout:   webhookDeliveryTimeout(),
		// A redirect is a new user-controlled target, so it is held to the same
		// rule as the registered URL.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return ValidateWebhookURL(req.URL.String(), allowPrivateTarget)
		},
	}
}

func webhookDeliveryTimeout() time.Duration {
	return time.Duration(constant.WebhookTimeoutSeconds) * time.Second
}

var (
	webhookQueueOnce  sync.Once
	webhookQueueMutex sync.RWMutex
	webhookQueue      chan int64
)

// webhookDeliveryQueue returns the worker queue, or nil when the dispatcher was
// never started. The read is guarded because dispatchers run on request and
// background goroutines that have no ordering against startup.
func webhookDeliveryQueue() chan int64 {
	webhookQueueMutex.RLock()
	defer webhookQueueMutex.RUnlock()
	return webhookQueue
}

// StartWebhookDispatcher starts the delivery worker pool and the retry
// sweeper. Call it once during startup, before any event can be dispatched.
func StartWebhookDispatcher() {
	if !constant.WebhookEnabled {
		common.SysLog("webhook event dispatcher is disabled")
		return
	}
	webhookQueueOnce.Do(func() {
		workers := max(constant.WebhookMaxWorkers, 1)
		queue := make(chan int64, workers*8)
		webhookQueueMutex.Lock()
		webhookQueue = queue
		webhookQueueMutex.Unlock()
		for range workers {
			gopool.Go(func() { runWebhookWorker(queue) })
		}
		gopool.Go(func() { runWebhookRetrySweeper(queue) })
		common.SysLog(fmt.Sprintf("webhook event dispatcher started: workers=%d timeout=%ds max_retries=%d",
			workers, constant.WebhookTimeoutSeconds, constant.WebhookMaxRetries))
	})
}

// DispatchEvent records and queues one event for every active endpoint its user
// owns that subscribes to eventType. It is safe to call from any goroutine and
// returns as soon as the delivery rows exist; the HTTP work happens in the
// worker pool.
func DispatchEvent(userId int64, eventType string, data any) {
	if !constant.WebhookEnabled || userId <= 0 || !constant.IsWebhookEvent(eventType) {
		return
	}
	webhooks, err := model.GetActiveWebhooksForEvent(userId, eventType)
	if err != nil {
		common.SysError(fmt.Sprintf("webhook: failed to load endpoints for user %d event %s: %s", userId, eventType, err.Error()))
		return
	}
	dispatchToWebhooks(webhooks, eventType, data)
}

// DispatchAdminEvent fans an infrastructure event out to every active endpoint
// subscribed to it whose owner currently holds an administrator role. Channel
// health describes the platform rather than one tenant, so it is delivered to
// administrators only.
func DispatchAdminEvent(eventType string, data any) {
	if !constant.WebhookEnabled || !constant.IsWebhookEvent(eventType) {
		return
	}
	webhooks, err := model.GetActiveAdminWebhooksForEvent(eventType)
	if err != nil {
		common.SysError(fmt.Sprintf("webhook: failed to load admin endpoints for event %s: %s", eventType, err.Error()))
		return
	}
	dispatchToWebhooks(webhooks, eventType, data)
}

// SendWebhookTestEvent records and immediately delivers a webhook.test event
// to one endpoint the caller already owns, returning the delivery with its
// outcome so the API can show the receiver's status code.
//
// It bypasses the subscription filter on purpose: the point of the test action
// is to prove the endpoint and its signature work even before the customer has
// settled on an event list. Unlike a production event it is sent inline, which
// is bounded by the delivery timeout and sits behind a critical rate limit.
func SendWebhookTestEvent(webhook *model.Webhook) (*model.WebhookDelivery, error) {
	if !constant.WebhookEnabled {
		return nil, errors.New("webhook events are disabled on this deployment")
	}
	body, err := marshalWebhookEnvelope(constant.WebhookEventTest, WebhookTestData{
		WebhookId:   hosttypes.NewFlexInt64(webhook.ID),
		Description: "Test event sent from the TokenSplice Gateway webhook API",
	})
	if err != nil {
		return nil, err
	}
	delivery := &model.WebhookDelivery{
		WebhookId: webhook.ID,
		UserId:    webhook.UserId,
		EventType: constant.WebhookEventTest,
		Payload:   string(body),
	}
	if err := model.InsertWebhookDelivery(delivery); err != nil {
		return nil, err
	}
	// The send error is reported through the delivery row, which is what the
	// caller inspects; returning it as well would make a failing test look like
	// a failed API call.
	_ = SendWebhook(delivery, webhook)
	return delivery, nil
}

func dispatchToWebhooks(webhooks []*model.Webhook, eventType string, data any) {
	if len(webhooks) == 0 {
		return
	}
	body, err := marshalWebhookEnvelope(eventType, data)
	if err != nil {
		common.SysError(fmt.Sprintf("webhook: failed to encode event %s: %s", eventType, err.Error()))
		return
	}
	for _, webhook := range webhooks {
		if _, err := createWebhookDelivery(webhook, eventType, body); err != nil {
			common.SysError(fmt.Sprintf("webhook: failed to record event %s for webhook %d: %s", eventType, webhook.ID, err.Error()))
		}
	}
}

func marshalWebhookEnvelope(eventType string, data any) ([]byte, error) {
	return common.Marshal(WebhookEventEnvelope{
		Event:     eventType,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data:      data,
	})
}

// createWebhookDelivery persists one delivery row and queues it. The row is
// written before the send so a receiver that answers and then vanishes still
// leaves an auditable attempt behind.
func createWebhookDelivery(webhook *model.Webhook, eventType string, body []byte) (*model.WebhookDelivery, error) {
	delivery := &model.WebhookDelivery{
		WebhookId: webhook.ID,
		UserId:    webhook.UserId,
		EventType: eventType,
		Payload:   string(body),
	}
	if err := model.InsertWebhookDelivery(delivery); err != nil {
		return nil, err
	}
	enqueueWebhookDelivery(delivery.ID)
	return delivery, nil
}

// enqueueWebhookDelivery hands a delivery to the worker pool without ever
// blocking the caller. A saturated pool reschedules the row instead, so the
// event survives and is picked up again shortly; a pool that was never started
// closes the row out, because nothing will ever drain it.
func enqueueWebhookDelivery(deliveryId int64) {
	queue := webhookDeliveryQueue()
	if queue == nil {
		closeUndeliverableWebhook(deliveryId, "webhook dispatcher is not running")
		return
	}
	select {
	case queue <- deliveryId:
	default:
		rescheduleWebhookDelivery(deliveryId, "webhook delivery queue is full")
	}
}

// closeUndeliverableWebhook records a delivery that can never be sent. It marks
// the row permanently failed rather than rescheduling it, so a missing
// dispatcher cannot re-arm a timer forever.
func closeUndeliverableWebhook(deliveryId int64, reason string) {
	if err := model.MarkWebhookDeliveryFailed(deliveryId, 0, 0, reason, nil); err != nil {
		common.SysError(fmt.Sprintf("webhook: failed to close delivery %d: %s", deliveryId, err.Error()))
	}
}

func rescheduleWebhookDelivery(deliveryId int64, reason string) {
	retryAt := time.Now().Add(webhookRequeueDelay)
	if err := model.RescheduleWebhookDelivery(deliveryId, reason, retryAt); err != nil {
		common.SysError(fmt.Sprintf("webhook: failed to reschedule delivery %d: %s", deliveryId, err.Error()))
		return
	}
	time.AfterFunc(webhookRequeueDelay, func() { requeueWebhookDelivery(deliveryId) })
}

func runWebhookWorker(queue chan int64) {
	for deliveryId := range queue {
		deliverWebhookById(deliveryId)
	}
}

// deliverWebhookById loads one queued delivery and sends it. A row carrying a
// scheduled retry is claimed with a compare-and-set first, so an in-process
// timer and the sweeper on another instance cannot both send it.
func deliverWebhookById(deliveryId int64) {
	// One malformed row or one panicking transport must not take a worker out of
	// the pool: it would shrink the delivery capacity for the life of the
	// process and nothing would ever restart it.
	defer func() {
		if recovered := recover(); recovered != nil {
			common.SysError(fmt.Sprintf("webhook: panic while delivering %d recovered: %v", deliveryId, recovered))
		}
	}()

	delivery, err := model.GetWebhookDeliveryById(deliveryId)
	if err != nil {
		if !errors.Is(err, model.ErrWebhookDeliveryNotFound) {
			common.SysError(fmt.Sprintf("webhook: failed to load delivery %d: %s", deliveryId, err.Error()))
		}
		return
	}
	if delivery.DeliveredAt != nil {
		return
	}
	if delivery.NextRetry != nil {
		claimed, claimErr := model.ClaimWebhookDeliveryRetry(deliveryId, time.Now())
		if claimErr != nil {
			common.SysError(fmt.Sprintf("webhook: failed to claim delivery %d: %s", deliveryId, claimErr.Error()))
			return
		}
		if !claimed {
			return
		}
	}

	webhook, err := model.GetWebhookById(delivery.WebhookId)
	if err != nil {
		// The endpoint was deleted or is unreadable; the delivery can never
		// succeed, so it is closed out instead of retried.
		if markErr := model.MarkWebhookDeliveryFailed(deliveryId, 0, delivery.Attempts+1, "webhook is no longer available", nil); markErr != nil {
			common.SysError(fmt.Sprintf("webhook: failed to close delivery %d: %s", deliveryId, markErr.Error()))
		}
		return
	}
	if !webhook.IsActive() {
		if markErr := model.MarkWebhookDeliveryFailed(deliveryId, 0, delivery.Attempts+1, "webhook is disabled", nil); markErr != nil {
			common.SysError(fmt.Sprintf("webhook: failed to close delivery %d: %s", deliveryId, markErr.Error()))
		}
		return
	}
	if err := SendWebhook(delivery, webhook); err != nil {
		logger.LogWarn(context.Background(), "webhook: delivery %d of event %s failed: %s", deliveryId, delivery.EventType, err.Error())
	}
}

// requeueWebhookDelivery claims a due retry and puts it back on the queue. It
// reports whether this caller won the claim.
func requeueWebhookDelivery(deliveryId int64) bool {
	claimed, err := model.ClaimWebhookDeliveryRetry(deliveryId, time.Now())
	if err != nil {
		common.SysError(fmt.Sprintf("webhook: failed to claim delivery %d: %s", deliveryId, err.Error()))
		return false
	}
	if !claimed {
		return false
	}
	queue := webhookDeliveryQueue()
	if queue == nil {
		closeUndeliverableWebhook(deliveryId, "webhook dispatcher is not running")
		return false
	}
	select {
	case queue <- deliveryId:
		return true
	default:
		rescheduleWebhookDelivery(deliveryId, "webhook delivery queue is full")
		return false
	}
}

func runWebhookRetrySweeper(queue chan int64) {
	ticker := time.NewTicker(webhookRetrySweepInterval)
	defer ticker.Stop()
	for range ticker.C {
		if queued := sweepWebhookRetries(queue, time.Now()); queued > 0 {
			logger.LogInfo(context.Background(), fmt.Sprintf("webhook: re-queued %d due deliveries", queued))
		}
	}
}

// sweepWebhookRetries re-queues every delivery whose scheduled retry has come
// due and returns how many this caller claimed. It is the recovery path for
// retries a restart orphaned; the normal case is carried by an in-process timer
// armed when the attempt failed.
func sweepWebhookRetries(queue chan int64, now time.Time) int {
	due, err := model.GetDueWebhookRetries(now, webhookRetrySweepBatch)
	if err != nil {
		common.SysError(fmt.Sprintf("webhook: retry sweep failed: %s", err.Error()))
		return 0
	}
	queued := 0
	for _, delivery := range due {
		claimed, claimErr := model.ClaimWebhookDeliveryRetry(delivery.ID, now)
		if claimErr != nil {
			common.SysError(fmt.Sprintf("webhook: failed to claim delivery %d: %s", delivery.ID, claimErr.Error()))
			continue
		}
		if !claimed {
			continue
		}
		select {
		case queue <- delivery.ID:
			queued++
		default:
			// The claim already cleared next_retry, so hand the row back to the
			// congestion path rather than losing it.
			rescheduleWebhookDelivery(delivery.ID, "webhook delivery queue is full")
		}
	}
	return queued
}

// SendWebhook performs one delivery attempt and records its outcome: a 2xx
// marks the delivery delivered, anything else consumes an attempt and either
// schedules the next retry or closes the delivery as permanently failed.
//
// The body sent is exactly the bytes stored on the delivery row, so the
// signature a receiver verifies matches the payload it logged on the first
// attempt even after several retries.
func SendWebhook(delivery *model.WebhookDelivery, webhook *model.Webhook) error {
	// The private-network exemption is resolved per attempt from the owner's
	// current role rather than persisted at registration, so demoting an
	// administrator immediately withdraws their endpoints' ability to reach
	// internal addresses. A failed lookup fails closed.
	statusCode, err := postWebhookDelivery(webhookHTTPClient(model.IsAdmin(int(webhook.UserId))), webhook, delivery)
	return recordWebhookAttempt(delivery, statusCode, err)
}

// recordWebhookAttempt persists the outcome of one attempt on the delivery row
// and in memory: a 2xx closes the delivery as delivered, anything else consumes
// an attempt and either arms the next retry or, once the budget is spent,
// closes the delivery as permanently failed.
//
// The retry timer is only armed while the dispatcher is running. Without a
// worker pool there is nothing to hand the job to, and the row keeps its
// scheduled retry for the next deployment to sweep.
func recordWebhookAttempt(delivery *model.WebhookDelivery, statusCode int, attemptErr error) error {
	attempts := delivery.Attempts + 1
	delivery.Attempts = attempts
	delivery.StatusCode = statusCode

	if attemptErr == nil {
		deliveredAt := time.Now()
		delivery.Error = ""
		delivery.NextRetry = nil
		delivery.DeliveredAt = &deliveredAt
		if err := model.MarkWebhookDeliverySucceeded(delivery.ID, statusCode, attempts, deliveredAt); err != nil {
			common.SysError(fmt.Sprintf("webhook: failed to record the success of delivery %d: %s", delivery.ID, err.Error()))
		}
		return nil
	}

	delivery.Error = attemptErr.Error()
	if attempts-1 >= constant.WebhookMaxRetries {
		delivery.NextRetry = nil
		delivery.Error = fmt.Sprintf("%s (retries exhausted)", attemptErr.Error())
		if err := model.MarkWebhookDeliveryFailed(delivery.ID, statusCode, attempts, delivery.Error, nil); err != nil {
			common.SysError(fmt.Sprintf("webhook: failed to record the permanent failure of delivery %d: %s", delivery.ID, err.Error()))
		}
		return attemptErr
	}

	delay := webhookRetryDelay(attempts)
	retryAt := time.Now().Add(delay)
	delivery.NextRetry = &retryAt
	if err := model.MarkWebhookDeliveryFailed(delivery.ID, statusCode, attempts, attemptErr.Error(), &retryAt); err != nil {
		common.SysError(fmt.Sprintf("webhook: failed to record the failure of delivery %d: %s", delivery.ID, err.Error()))
		return attemptErr
	}
	if webhookDeliveryQueue() != nil {
		// The timer carries the normal retry; the sweeper only has to catch
		// what a restart drops.
		time.AfterFunc(delay, func() { requeueWebhookDelivery(delivery.ID) })
	}
	return attemptErr
}

// postWebhookDelivery performs the HTTP attempt itself and reports the
// receiver's status code. It touches no database state, so the transport
// contract — body, headers, signature — is asserted against it directly.
func postWebhookDelivery(client *http.Client, webhook *model.Webhook, delivery *model.WebhookDelivery) (int, error) {
	body := []byte(delivery.Payload)
	request, err := http.NewRequest(http.MethodPost, webhook.Url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("failed to build webhook request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", webhookUserAgent)
	request.Header.Set(HeaderWebhookEvent, delivery.EventType)
	request.Header.Set(HeaderWebhookDelivery, strconv.FormatInt(delivery.ID, 10))
	request.Header.Set(HeaderWebhookSignature, webhookSignaturePrefix+generateSignature(webhook.Secret, body))

	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	// Drain and discard a bounded prefix so the connection returns to the pool
	// instead of being torn down; a receiver's error body is never logged, since
	// it is remote-controlled text.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, fmt.Errorf("webhook receiver returned status %d", response.StatusCode)
	}
	return response.StatusCode, nil
}

// webhookRetryDelay is the backoff before retry number retryIndex, counted from
// one. A manual retry of a permanently failed delivery restarts the schedule.
func webhookRetryDelay(retryIndex int) time.Duration {
	if retryIndex <= 0 {
		return webhookRetryBackoff[0]
	}
	return webhookRetryBackoff[min(retryIndex, len(webhookRetryBackoff))-1]
}

// RetryWebhookDelivery re-sends a delivery the caller owns, resetting its
// attempt counter so a repaired receiver gets the full retry budget. The send
// outcome is recorded on the delivery and reported through it: a receiver that
// answers 500 is a normal result of a manual retry, so only a precondition
// failure — a deleted or disabled endpoint, a database error — is returned.
func RetryWebhookDelivery(delivery *model.WebhookDelivery) error {
	webhook, err := model.GetWebhookById(delivery.WebhookId)
	if err != nil {
		return errors.New("webhook is no longer available")
	}
	if !webhook.IsActive() {
		return errors.New("webhook is disabled")
	}
	if err := model.ResetWebhookDelivery(delivery.ID); err != nil {
		return err
	}
	delivery.Attempts = 0
	delivery.StatusCode = 0
	delivery.Error = ""
	delivery.NextRetry = nil
	delivery.DeliveredAt = nil
	_ = SendWebhook(delivery, webhook)
	return nil
}

// DispatchRequestErrorEvent raises request.error for a relay request that failed
// after every retry. It reads the gin context synchronously and dispatches from
// a pooled goroutine, because the context is not safe to touch once the handler
// returns and the webhook lookup must not extend the request path.
func DispatchRequestErrorEvent(c *gin.Context, apiErr *types.NewAPIError, requestId string) {
	if !constant.WebhookEnabled || apiErr == nil || c == nil {
		return
	}
	userId := int64(c.GetInt("id"))
	if userId <= 0 {
		return
	}
	modelName := c.GetString("original_model")
	if modelName == "" {
		modelName = c.GetString("model")
	}
	data := RequestErrorEventData{
		Model:      modelName,
		ErrorCode:  string(apiErr.GetErrorCode()),
		StatusCode: apiErr.StatusCode,
		RequestId:  requestId,
	}
	gopool.Go(func() {
		DispatchEvent(userId, constant.WebhookEventRequestError, data)
	})
}

// WebhookMaintenanceResult reports one housekeeping pass.
type WebhookMaintenanceResult struct {
	TokenEventsDispatched int `json:"token_events_dispatched"`
	DeliveriesPurged      int `json:"deliveries_purged"`
}

// RunWebhookMaintenance is the periodic webhook housekeeping pass: it raises
// token.expiring for tokens that enter the warning window and purges delivery
// rows past the retention horizon.
func RunWebhookMaintenance(ctx context.Context) (WebhookMaintenanceResult, error) {
	result := WebhookMaintenanceResult{}

	dispatched, err := dispatchExpiringTokenEvents(ctx)
	result.TokenEventsDispatched = dispatched
	if err != nil {
		logger.LogWarn(ctx, "webhook: expiring-token sweep failed: %s", err.Error())
	}

	cutoff := time.Now().AddDate(0, 0, -constant.WebhookDeliveryRetentionDays)
	purged, err := model.DeleteWebhookDeliveriesBefore(cutoff)
	result.DeliveriesPurged = int(purged)
	if err != nil {
		return result, fmt.Errorf("failed to purge webhook deliveries older than %s: %w", cutoff.Format(time.RFC3339), err)
	}
	return result, nil
}

// dispatchExpiringTokenEvents raises token.expiring for every token that is
// inside the warning window and has not already been announced to that owner
// during this window. The dedupe reads the delivery log rather than adding
// state to the token table, so a token that is renewed and later re-enters the
// window is announced again.
func dispatchExpiringTokenEvents(ctx context.Context) (int, error) {
	owners, err := model.GetActiveWebhookOwnersForEvent(constant.WebhookEventTokenExpiring)
	if err != nil {
		return 0, err
	}
	if len(owners) == 0 {
		return 0, nil
	}

	now := time.Now()
	windowEnd := now.AddDate(0, 0, model.ExpirationWarningDays)
	announcedSince := now.AddDate(0, 0, -webhookMaintenanceTokenLookback)
	dispatched := 0

	for _, owner := range owners {
		tokens, tokenErr := model.GetTokensExpiringForUser(int(owner), now.Unix(), windowEnd.Unix())
		if tokenErr != nil {
			logger.LogWarn(ctx, "webhook: failed to load expiring tokens of user %d: %s", owner, tokenErr.Error())
			continue
		}
		if len(tokens) == 0 {
			continue
		}
		alreadyAnnounced := announcedTokenIds(owner, announcedSince)
		for _, token := range tokens {
			if alreadyAnnounced[token.Id] {
				continue
			}
			DispatchEvent(owner, constant.WebhookEventTokenExpiring, TokenExpiringEventData{
				TokenId:   token.Id,
				TokenName: token.Name,
				ExpiresAt: time.Unix(token.ExpiredTime, 0).UTC().Format(time.RFC3339),
			})
			dispatched++
		}
	}
	return dispatched, nil
}

func announcedTokenIds(userId int64, since time.Time) map[int]bool {
	announced := make(map[int]bool)
	deliveries, err := model.GetRecentWebhookEventDeliveries(userId, constant.WebhookEventTokenExpiring, since)
	if err != nil {
		common.SysError(fmt.Sprintf("webhook: failed to load recent token.expiring deliveries of user %d: %s", userId, err.Error()))
		return announced
	}
	for _, delivery := range deliveries {
		var event TokenExpiringEventData
		if err := common.UnmarshalJsonStr(delivery.Payload, &event); err != nil {
			continue
		}
		announced[event.TokenId] = true
	}
	return announced
}
