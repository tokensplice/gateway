package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
)

// Provider knowledge for BYOK: which upstream answers a model name, what a
// credential for that upstream looks like, and how to prove one is live
// without spending the customer's money.

// byokProbeTimeout bounds a key validation round trip. The probe runs on an
// interactive API request, so it must fail fast rather than hold a worker.
const byokProbeTimeout = 15 * time.Second

// ErrByokProbeUnsupported is returned when a provider has no fixed upstream
// endpoint to validate against, which is the case for a customer-defined
// "custom" key.
var ErrByokProbeUnsupported = errors.New("byok: this provider cannot be validated automatically")

// byokProbeEndpoints are the credential-checking endpoints per provider. Each
// one lists models, so a probe authenticates the key without generating any
// billable upstream usage on the customer's account.
var byokProbeEndpoints = map[string]string{
	constant.ByokProviderOpenAI:    "https://api.openai.com/v1/models",
	constant.ByokProviderAnthropic: "https://api.anthropic.com/v1/models",
	constant.ByokProviderGoogle:    "https://generativelanguage.googleapis.com/v1beta/models",
	constant.ByokProviderMistral:   "https://api.mistral.ai/v1/models",
}

// byokModelPrefixProviders maps a leading model-name token to the vendor whose
// own API serves it. It is the fallback used only when no channel in this
// deployment claims the model, so a loose match here cannot override an
// authoritative channel mapping.
var byokModelPrefixProviders = []struct {
	prefixes []string
	provider string
}{
	{
		prefixes: []string{"claude", "anthropic"},
		provider: constant.ByokProviderAnthropic,
	},
	{
		prefixes: []string{"gemini", "gemma", "paligemma", "google"},
		provider: constant.ByokProviderGoogle,
	},
	{
		prefixes: []string{
			"mistral", "ministral", "magistral", "mixtral", "codestral", "pixtral",
			"voxtral", "devstral", "open-mistral", "open-mixtral", "open-codestral",
		},
		provider: constant.ByokProviderMistral,
	},
	{
		prefixes: []string{
			"gpt-", "gpt4", "gpt-oss", "chatgpt-4o", "o1", "o3", "o4",
			"dall-e", "dalle", "whisper", "tts-", "text-embedding-", "computer-use-preview",
		},
		provider: constant.ByokProviderOpenAI,
	},
}

// DetectByokProviders returns the upstream providers that could serve
// modelName, best guess first, or nil when the model has no BYOK route.
//
// The configured channel mapping is authoritative: when at least one enabled
// channel claims the model, only the providers behind those channels are
// returned. A model served exclusively by a reseller or an incompatible
// transport (Azure, Bedrock, OpenRouter, ...) therefore yields nothing even if
// its name looks like an OpenAI model, because the customer's OpenAI key would
// not be accepted there. Name-based detection runs only when no channel claims
// the model at all.
func DetectByokProviders(modelName string) []string {
	normalizedName := strings.TrimSpace(modelName)
	if normalizedName == "" {
		return nil
	}

	channelTypes, err := model.GetEnabledChannelTypesForModel(normalizedName)
	if err != nil {
		common.SysError(fmt.Sprintf("byok: failed to resolve channels for model %q: %s", normalizedName, err.Error()))
		channelTypes = nil
	}
	if len(channelTypes) > 0 {
		var providers []string
		for _, channelType := range channelTypes {
			provider := constant.ByokProviderForChannelType(channelType)
			if provider != "" && !slices.Contains(providers, provider) {
				providers = append(providers, provider)
			}
		}
		return providers
	}

	if provider := detectByokProviderFromModelName(normalizedName); provider != "" {
		return []string{provider}
	}
	return nil
}

// detectByokProviderFromModelName matches the last path segment of a model
// name, so router-style names such as "openai/gpt-4o" resolve on "gpt-4o".
func detectByokProviderFromModelName(modelName string) string {
	name := strings.ToLower(strings.TrimSpace(modelName))
	if _, lastSegment, found := strings.Cut(name, "/"); found {
		name = lastSegment
	}
	if name == "" {
		return ""
	}
	for _, entry := range byokModelPrefixProviders {
		for _, prefix := range entry.prefixes {
			if strings.HasPrefix(name, prefix) {
				return entry.provider
			}
		}
	}
	return ""
}

// ValidateByokKeyFormat checks the shape of a submitted credential before it
// is encrypted. It is a fast typo guard, not proof of validity: a key that
// looks right is only confirmed by ProbeByokKey. The message never echoes the
// submitted value, because request bodies can end up in error logs.
func ValidateByokKeyFormat(provider string, key string) error {
	if !constant.IsByokProvider(provider) {
		return fmt.Errorf("unsupported provider %q, expected one of: %s", provider, strings.Join(constant.ByokProviders, ", "))
	}
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return errors.New("key is required")
	}
	if len(trimmed) > common.ByokMaxSecretLen {
		return fmt.Errorf("key exceeds %d characters", common.ByokMaxSecretLen)
	}
	if strings.ContainsAny(trimmed, "\r\n\t") {
		return errors.New("key must not contain line breaks or tabs")
	}

	switch provider {
	case constant.ByokProviderOpenAI:
		if !strings.HasPrefix(trimmed, "sk-") {
			return errors.New(`an OpenAI key starts with "sk-"`)
		}
	case constant.ByokProviderAnthropic:
		if !strings.HasPrefix(trimmed, "sk-ant-") {
			return errors.New(`an Anthropic key starts with "sk-ant-"`)
		}
	case constant.ByokProviderGoogle:
		if !strings.HasPrefix(trimmed, "AIza") {
			return errors.New(`a Google AI key starts with "AIza"`)
		}
	case constant.ByokProviderMistral:
		// Mistral issues opaque alphanumeric keys with no stable public prefix,
		// so only an obviously truncated value is rejected.
		if len(trimmed) < 16 {
			return errors.New("a Mistral key looks too short")
		}
	case constant.ByokProviderCustom:
		if len(trimmed) < 8 {
			return errors.New("a custom key looks too short")
		}
	}
	return nil
}

// ByokProbeOutcome classifies a validation attempt so the caller can decide
// whether a failure is authoritative.
type ByokProbeOutcome string

const (
	// ByokProbeValid means the upstream accepted the credential.
	ByokProbeValid ByokProbeOutcome = "valid"
	// ByokProbeInvalid means the upstream rejected the credential, so the key
	// can safely be marked unusable.
	ByokProbeInvalid ByokProbeOutcome = "invalid"
	// ByokProbeInconclusive means the check reached no verdict (network
	// failure, timeout, rate limit, upstream outage, unsupported provider).
	// The stored key status must not be changed on this outcome.
	ByokProbeInconclusive ByokProbeOutcome = "inconclusive"
)

// ByokProbeResult carries the outcome and a user-visible reason.
type ByokProbeResult struct {
	Outcome ByokProbeOutcome `json:"outcome"`
	Reason  string           `json:"reason"`
}

// ProbeByokKey authenticates a credential against the provider's model listing
// endpoint. The caller passes an already decrypted secret; it is never logged,
// and no upstream response body is surfaced beyond the status code.
func ProbeByokKey(ctx context.Context, provider string, secret string) ByokProbeResult {
	endpoint, ok := byokProbeEndpoints[provider]
	if !ok {
		return ByokProbeResult{Outcome: ByokProbeInconclusive, Reason: ErrByokProbeUnsupported.Error()}
	}

	probeCtx, cancel := context.WithTimeout(ctx, byokProbeTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ByokProbeResult{Outcome: ByokProbeInconclusive, Reason: "failed to build the validation request"}
	}
	applyByokProbeAuth(request, provider, secret)

	client := GetHttpClient()
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return ByokProbeResult{Outcome: ByokProbeInconclusive, Reason: "could not reach the provider"}
	}
	defer response.Body.Close()

	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		return ByokProbeResult{Outcome: ByokProbeValid, Reason: "the provider accepted this key"}
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return ByokProbeResult{
			Outcome: ByokProbeInvalid,
			Reason:  fmt.Sprintf("the provider rejected this key (HTTP %d)", response.StatusCode),
		}
	default:
		// 429 and 5xx say nothing about the credential itself; marking the key
		// invalid here would disable a working key during an upstream incident.
		return ByokProbeResult{
			Outcome: ByokProbeInconclusive,
			Reason:  fmt.Sprintf("the provider returned HTTP %d, so the key could not be validated", response.StatusCode),
		}
	}
}

// applyByokProbeAuth attaches the credential the way each provider expects it.
// Google's key is sent as a header rather than the documented ?key= query
// parameter, because a query string is routinely captured in proxy and access
// logs.
func applyByokProbeAuth(request *http.Request, provider string, secret string) {
	switch provider {
	case constant.ByokProviderOpenAI, constant.ByokProviderMistral:
		request.Header.Set("Authorization", "Bearer "+secret)
	case constant.ByokProviderAnthropic:
		request.Header.Set("x-api-key", secret)
		request.Header.Set("anthropic-version", "2023-06-01")
	case constant.ByokProviderGoogle:
		request.Header.Set("x-goog-api-key", secret)
	}
}
