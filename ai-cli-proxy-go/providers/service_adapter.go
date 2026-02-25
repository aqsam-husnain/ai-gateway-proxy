// Package providers defines the ServiceAdapter interface that every AI
// backend adapter must satisfy, a StreamChunk type for streaming responses,
// a thread-safe global ServiceAdapterRegistry, and the GetOrCreateAdapter
// factory function that is the primary entry-point for obtaining adapters.
//
// Concrete adapter implementations live in the sibling *_adapter.go files
// (gemini_adapter.go, openai_adapter.go, claude_adapter.go,
// antigravity_adapter.go).
package providers

import (
	"fmt"
	"sync"

	cfg "ai-cli-proxy-go/config"
)

// ---------------------------------------------------------------------------
// StreamChunk
// ---------------------------------------------------------------------------

// StreamChunk is a single unit of data emitted by a streaming adapter.
// When Done is true the stream has ended; Error may be set to indicate that
// the stream terminated abnormally.  Exactly one of Data, Done, or Error
// is meaningful per chunk.
type StreamChunk struct {
	// Data holds the raw bytes of a single SSE data line (without the
	// "data: " prefix).  It may be nil when Done is true.
	Data []byte

	// Done indicates that the stream has been fully consumed.  No further
	// chunks will be sent after a chunk with Done=true.
	Done bool

	// Error holds a non-nil error if the stream was terminated by an error
	// condition rather than a normal end-of-stream.
	Error error
}

// ---------------------------------------------------------------------------
// ServiceAdapter interface
// ---------------------------------------------------------------------------

// ServiceAdapter is the interface that every provider adapter must satisfy.
// Implementations are required to be safe for concurrent use.
type ServiceAdapter interface {
	// GenerateContent performs a non-streaming content generation request.
	// model is the provider-native model identifier.
	// requestBody is the provider-native request payload as a generic map.
	// Returns the provider-native response payload, or an error.
	GenerateContent(model string, requestBody map[string]interface{}) (map[string]interface{}, error)

	// GenerateContentStream performs a streaming content generation request.
	// It returns a read-only channel over which StreamChunks are sent.
	// The channel is closed (with Done=true on the final chunk) when the
	// stream is complete or an error occurs.
	GenerateContentStream(model string, requestBody map[string]interface{}) (<-chan StreamChunk, error)

	// ListModels returns the list of models available through this adapter.
	// The return type is interface{} to accommodate provider-specific formats
	// (OpenAI ModelList, Gemini GeminiModelList, etc.).
	ListModels() (interface{}, error)

	// RefreshToken refreshes any short-lived authentication credential held by
	// the adapter.  For adapters that use static API keys this is a no-op.
	RefreshToken() error

	// GetProviderType returns the canonical provider-type string for this
	// adapter (e.g. ProviderGeminiCLI, ProviderOpenAI).
	GetProviderType() string
}

// ---------------------------------------------------------------------------
// AdapterFactory
// ---------------------------------------------------------------------------

// AdapterFactory is a constructor function signature for creating a
// ServiceAdapter.  Implementations may register themselves via
// RegisterAdapterFactory for use in plugin-style extension scenarios.
//
// Parameters:
//   - providerCfg: the pool-entry configuration for this provider instance.
//   - globalCfg:   the full application-level configuration.
type AdapterFactory func(providerCfg *cfg.ProviderConfig, globalCfg *cfg.Config) (ServiceAdapter, error)

// ---------------------------------------------------------------------------
// Optional factory registry (for plugin / extension scenarios)
// ---------------------------------------------------------------------------

var (
	factoryMu        sync.RWMutex
	adapterFactories = make(map[string]AdapterFactory) // keyed by provider type
)

// RegisterAdapterFactory registers a factory function for a given provider
// type.  Calling this in an init() function allows new provider types to be
// added without modifying the core switch in GetOrCreateAdapter.
// Registering the same providerType twice overwrites the previous entry.
func RegisterAdapterFactory(providerType string, factory AdapterFactory) {
	factoryMu.Lock()
	defer factoryMu.Unlock()
	adapterFactories[providerType] = factory
}

// ---------------------------------------------------------------------------
// ServiceAdapterRegistry
// ---------------------------------------------------------------------------

// ServiceAdapterRegistry is a thread-safe cache of live ServiceAdapter
// instances.  The key is "<providerType>:<uuid>" — ensuring that each
// physical provider account gets exactly one adapter instance that is reused
// across requests (avoiding repeated credential file loads and token
// refreshes).
type ServiceAdapterRegistry struct {
	mu       sync.RWMutex
	adapters map[string]ServiceAdapter
}

// globalRegistry is the process-wide singleton registry.
var globalRegistry = &ServiceAdapterRegistry{
	adapters: make(map[string]ServiceAdapter),
}

// adapterRegistryKey builds the composite map key used inside the registry.
func adapterRegistryKey(providerType, uuid string) string {
	return providerType + ":" + uuid
}

// Get returns an existing adapter for the given (providerType, uuid) pair, or
// nil if none has been created yet.
func (r *ServiceAdapterRegistry) Get(providerType, uuid string) ServiceAdapter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.adapters[adapterRegistryKey(providerType, uuid)]
}

// Set stores an adapter in the registry under the given (providerType, uuid)
// key.  Existing entries are overwritten.
func (r *ServiceAdapterRegistry) Set(providerType, uuid string, adapter ServiceAdapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[adapterRegistryKey(providerType, uuid)] = adapter
}

// Delete removes an adapter from the registry.
func (r *ServiceAdapterRegistry) Delete(providerType, uuid string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.adapters, adapterRegistryKey(providerType, uuid))
}

// GlobalRegistry returns the process-wide ServiceAdapterRegistry singleton.
func GlobalRegistry() *ServiceAdapterRegistry {
	return globalRegistry
}

// ---------------------------------------------------------------------------
// GetOrCreateAdapter — primary entry point
// ---------------------------------------------------------------------------

// GetOrCreateAdapter returns a ServiceAdapter for the given (providerType,
// uuid) pair.  If an adapter already exists in the global registry it is
// returned immediately (fast path).  Otherwise a new adapter is constructed
// via the built-in switch-statement factory, cached in the registry, and
// returned.
//
// Parameters:
//   - providerType: one of the ProviderXxx constants.
//   - uuid:         unique identifier of the pool entry.
//   - globalCfg:    application-level Config used for credential fallbacks.
//   - providerCfg:  pool-entry ProviderConfig.  When nil a minimal config is
//     synthesised from globalCfg to support single-provider (non-pooled) setups.
func GetOrCreateAdapter(
	providerType string,
	uuid string,
	globalCfg *cfg.Config,
	providerCfg *cfg.ProviderConfig,
) (ServiceAdapter, error) {
	// Fast path: adapter already cached.
	if existing := globalRegistry.Get(providerType, uuid); existing != nil {
		return existing, nil
	}

	// Build a minimal ProviderConfig when none is supplied.
	if providerCfg == nil {
		providerCfg = buildProviderConfigFromGlobal(providerType, uuid, globalCfg)
	}

	// Check plugin-registered factories first so that external providers can
	// override or extend the built-in ones.
	factoryMu.RLock()
	factory, hasFactory := adapterFactories[providerType]
	factoryMu.RUnlock()

	var adapter ServiceAdapter
	var err error

	if hasFactory {
		adapter, err = factory(providerCfg, globalCfg)
		if err != nil {
			return nil, fmt.Errorf("GetOrCreateAdapter: factory for %q (uuid=%s): %w", providerType, uuid, err)
		}
	} else {
		// Built-in switch-statement factory.
		adapter, err = createBuiltinAdapter(providerType, globalCfg, providerCfg)
		if err != nil {
			return nil, err
		}
	}

	globalRegistry.Set(providerType, uuid, adapter)
	return adapter, nil
}

// createBuiltinAdapter instantiates the correct concrete adapter for a known
// provider type.  It mirrors the logic from the JS getServiceAdapter factory.
func createBuiltinAdapter(providerType string, globalCfg *cfg.Config, pc *cfg.ProviderConfig) (ServiceAdapter, error) {
	switch providerType {
	case ProviderGeminiCLI:
		// Prefer per-pool credentials; fall back to global config.
		credsFile := pc.GeminiOAuthCredsFilePath
		if credsFile == "" {
			credsFile = globalCfg.GeminiOAuthCredsFilePath
		}
		projectID := pc.ProjectID
		if projectID == "" {
			projectID = globalCfg.ProjectID
		}
		if globalCfg.GeminiOAuthCredsBase64 != "" {
			return NewGeminiAdapterFromBase64(projectID, globalCfg.GeminiOAuthCredsBase64)
		}
		if credsFile != "" {
			return NewGeminiAdapterFromFile(projectID, credsFile)
		}
		return nil, fmt.Errorf("gemini-cli-oauth: no OAuth credentials configured (uuid=%s)", pc.UUID)

	case ProviderAntigravity:
		credsFile := pc.AntigravityOAuthCredsFilePath
		baseURL := pc.AntigravityBaseURLDaily
		if baseURL == "" {
			baseURL = pc.AntigravityBaseURLAutopush
		}
		if credsFile == "" {
			return nil, fmt.Errorf("gemini-antigravity: ANTIGRAVITY_OAUTH_CREDS_FILE_PATH not set (uuid=%s)", pc.UUID)
		}
		if baseURL == "" {
			return nil, fmt.Errorf("gemini-antigravity: no base URL configured (uuid=%s)", pc.UUID)
		}
		return NewAntigravityAdapterFromFile(credsFile, baseURL)

	case ProviderOpenAI:
		apiKey := pc.OpenAIAPIKey
		if apiKey == "" {
			apiKey = globalCfg.OpenAIAPIKey
		}
		baseURL := pc.OpenAIBaseURL
		if baseURL == "" {
			baseURL = globalCfg.OpenAIBaseURL
		}
		return NewOpenAIAdapter(apiKey, baseURL), nil

	case ProviderOpenAIResp:
		apiKey := pc.OpenAIAPIKey
		if apiKey == "" {
			apiKey = globalCfg.OpenAIAPIKey
		}
		baseURL := pc.OpenAIBaseURL
		if baseURL == "" {
			baseURL = globalCfg.OpenAIBaseURL
		}
		return NewOpenAIResponsesAdapter(apiKey, baseURL), nil

	case ProviderClaude, ProviderClaudeCode:
		apiKey := pc.ClaudeAPIKey
		if apiKey == "" {
			apiKey = globalCfg.ClaudeAPIKey
		}
		baseURL := pc.ClaudeBaseURL
		if baseURL == "" {
			baseURL = globalCfg.ClaudeBaseURL
		}
		return NewClaudeAdapter(apiKey, baseURL), nil

	default:
		return nil, fmt.Errorf("GetOrCreateAdapter: unsupported provider type %q (uuid=%s)", providerType, pc.UUID)
	}
}

// buildProviderConfigFromGlobal synthesises a minimal ProviderConfig from the
// global Config when no pool entry is available.
func buildProviderConfigFromGlobal(providerType, uuid string, globalCfg *cfg.Config) *cfg.ProviderConfig {
	pc := &cfg.ProviderConfig{
		UUID:      uuid,
		IsHealthy: true,
	}
	switch providerType {
	case ProviderGeminiCLI:
		pc.GeminiOAuthCredsFilePath = globalCfg.GeminiOAuthCredsFilePath
		pc.ProjectID = globalCfg.ProjectID
		pc.GeminiBaseURL = globalCfg.GeminiBaseURL
	case ProviderAntigravity:
		pc.AntigravityOAuthCredsFilePath = globalCfg.GeminiOAuthCredsFilePath
		pc.ProjectID = globalCfg.ProjectID
	case ProviderOpenAI, ProviderOpenAIResp:
		pc.OpenAIAPIKey = globalCfg.OpenAIAPIKey
		pc.OpenAIBaseURL = globalCfg.OpenAIBaseURL
	case ProviderClaude, ProviderClaudeCode:
		pc.ClaudeAPIKey = globalCfg.ClaudeAPIKey
		pc.ClaudeBaseURL = globalCfg.ClaudeBaseURL
	}
	return pc
}

// ---------------------------------------------------------------------------
// BaseAdapter — optional embedding for concrete adapter implementations
// ---------------------------------------------------------------------------

// BaseAdapter provides default implementations of the less-commonly overridden
// ServiceAdapter methods.  Concrete adapters may embed this struct to satisfy
// the interface without boilerplate.
type BaseAdapter struct {
	providerType string
}

// NewBaseAdapter creates a BaseAdapter for the given provider type string.
func NewBaseAdapter(providerType string) BaseAdapter {
	return BaseAdapter{providerType: providerType}
}

// GetProviderType returns the provider type string supplied at construction.
func (b BaseAdapter) GetProviderType() string {
	return b.providerType
}

// RefreshToken is a no-op default.  Concrete adapters holding short-lived
// credentials should override this.
func (b BaseAdapter) RefreshToken() error {
	return nil
}

// ListModels returns an empty OpenAI-format ModelList by default.  Concrete
// adapters should override this to return real model data.
func (b BaseAdapter) ListModels() (interface{}, error) {
	return ModelList{Object: "list", Data: []ModelObject{}}, nil
}
