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
