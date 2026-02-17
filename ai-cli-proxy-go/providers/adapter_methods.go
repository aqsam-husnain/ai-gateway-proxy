// This file adds GetProviderType() to the adapter types that don't yet
// implement it, ensuring they satisfy the ServiceAdapter interface.
// OpenAIAdapter already declares GetProviderType and NewOpenAIResponsesAdapter
// in openai_adapter.go, so those are omitted here.
package providers

// GetProviderType returns the canonical provider type for a GeminiAdapter.
func (a *GeminiAdapter) GetProviderType() string { return ProviderGeminiCLI }

// GetProviderType returns the canonical provider type for an AntigravityAdapter.
func (a *AntigravityAdapter) GetProviderType() string { return ProviderAntigravity }

// GetProviderType returns the canonical provider type for a ClaudeAdapter.
func (a *ClaudeAdapter) GetProviderType() string { return ProviderClaude }
