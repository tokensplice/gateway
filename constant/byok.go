package constant

// BYOK (bring your own key) providers. A provider names the upstream vendor
// whose own API a customer key authenticates against, which is narrower than a
// channel type: an OpenAI-compatible reseller channel does not accept an
// OpenAI key, so resellers are deliberately absent from the mapping below.
const (
	ByokProviderOpenAI    = "openai"
	ByokProviderAnthropic = "anthropic"
	ByokProviderGoogle    = "google"
	ByokProviderMistral   = "mistral"
	// ByokProviderCustom is an operator-supplied upstream. It has no fixed key
	// format and no fixed base URL in rc.2, so it can be stored and routed but
	// not validated against a live endpoint.
	ByokProviderCustom = "custom"
)

// BYOK key lifecycle states stored in byok_keys.status.
const (
	ByokKeyStatusInvalid  = -1 // rejected by the upstream provider
	ByokKeyStatusDisabled = 0  // turned off by the user, kept for re-enabling
	ByokKeyStatusActive   = 1  // eligible for routing
)

// BYOK shadow channels.
//
// A BYOK request is served by an ephemeral in-memory *model.Channel that is
// never persisted. It reports a negative id so it can never collide with a
// real channel row (ids are positive), which keeps the accounting writes on
// the relay path — used-quota updates, auto-disable, channel affinity — from
// ever touching a managed channel because of a customer key.
const (
	// ByokShadowChannelIdBase is the most negative id a shadow channel can
	// report. Real ids stay above zero, so the whole band below the base is
	// reserved.
	ByokShadowChannelIdBase = -1_000_000_000
	// byokShadowChannelIdSpread bounds the per-key offset inside the band. A
	// key id is a snowflake, so only its low digits are folded in; the id is a
	// logging and metrics handle, not a lookup key.
	byokShadowChannelIdSpread = 1_000_000
)

// ByokProviders lists every accepted provider value, ordered for stable
// validation messages.
var ByokProviders = []string{
	ByokProviderOpenAI,
	ByokProviderAnthropic,
	ByokProviderGoogle,
	ByokProviderMistral,
	ByokProviderCustom,
}

// byokChannelTypeProviders maps a channel type to the BYOK provider whose
// customer key that upstream accepts.
var byokChannelTypeProviders = map[int]string{
	ChannelTypeOpenAI:    ByokProviderOpenAI,
	ChannelTypeCodex:     ByokProviderOpenAI,
	ChannelTypeAnthropic: ByokProviderAnthropic,
	ChannelTypeGemini:    ByokProviderGoogle,
	ChannelTypeMistral:   ByokProviderMistral,
}

// byokProviderChannelTypes is the reverse of byokChannelTypeProviders, pinned
// to the vendor's own channel type so a shadow channel selects the adaptor
// that speaks that vendor's native protocol. ByokProviderCustom is absent on
// purpose: an operator-supplied upstream has no fixed base URL, so it cannot be
// relayed until one is stored beside the key.
var byokProviderChannelTypes = map[string]int{
	ByokProviderOpenAI:    ChannelTypeOpenAI,
	ByokProviderAnthropic: ChannelTypeAnthropic,
	ByokProviderGoogle:    ChannelTypeGemini,
	ByokProviderMistral:   ChannelTypeMistral,
}

// IsByokProvider reports whether name is a supported BYOK provider.
func IsByokProvider(name string) bool {
	switch name {
	case ByokProviderOpenAI, ByokProviderAnthropic, ByokProviderGoogle, ByokProviderMistral, ByokProviderCustom:
		return true
	default:
		return false
	}
}

// ByokProviderForChannelType resolves the BYOK provider a channel type serves,
// returning "" when the channel is a reseller or an incompatible transport.
func ByokProviderForChannelType(channelType int) string {
	return byokChannelTypeProviders[channelType]
}

// ByokChannelTypeForProvider resolves the channel type whose adaptor speaks a
// BYOK provider's own API, returning 0 when the provider cannot be relayed. A
// zero result means the provider's key can be stored but not routed.
func ByokChannelTypeForProvider(provider string) int {
	return byokProviderChannelTypes[provider]
}

// ByokShadowChannelId derives the stable negative channel id a BYOK attempt
// reports. The same key always maps to the same id, so retries, logs, and
// metrics can be correlated without ever looking like a managed channel.
func ByokShadowChannelId(keyId int64) int {
	if keyId < 0 {
		keyId = -keyId
	}
	return ByokShadowChannelIdBase - int(keyId%byokShadowChannelIdSpread)
}

// IsByokShadowChannelId reports whether a channel id belongs to a BYOK shadow
// channel rather than to a persisted managed channel.
func IsByokShadowChannelId(channelId int) bool {
	return channelId <= ByokShadowChannelIdBase &&
		channelId > ByokShadowChannelIdBase-byokShadowChannelIdSpread
}
