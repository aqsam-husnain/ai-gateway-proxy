package providers

import (
	"fmt"
	"log"

	cfg "ai-cli-proxy-go/config"
)

// ProviderConfig is a re-export (type alias) of cfg.ProviderConfig.
// It exists so that handler packages can reference providers.ProviderConfig
// without importing the config package directly, keeping import cycles minimal.
type ProviderConfig = cfg.ProviderConfig

// ForEachHealthyProvider iterates over every healthy, non-disabled provider
// across all pool types and calls fn for each one.  fn receives the provider
// type string and a pointer to the ProviderConfig of the healthy entry.
//
// The iteration order across provider types is non-deterministic (map
// iteration), but within a single provider type entries are visited in the
// order they appear in the pool slice.
func (m *ProviderPoolManager) ForEachHealthyProvider(fn func(providerType string, providerCfg *cfg.ProviderConfig)) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for providerType, states := range m.pools {
		for _, ps := range states {
			if !ps.config.IsHealthy || ps.config.IsDisabled {
				continue
			}
			fn(providerType, ps.config)
		}
	}
}

// GetAdapterForProvider returns a ServiceAdapter for the given provider type
// and config entry.  If an adapter already exists in the registry it is
// returned immediately; otherwise a new one is created and cached.
func (m *ProviderPoolManager) GetAdapterForProvider(providerType string, providerCfg *cfg.ProviderConfig) (ServiceAdapter, error) {
	if providerCfg == nil {
		return nil, fmt.Errorf("GetAdapterForProvider: providerCfg is nil for type %s", providerType)
	}
	adapter, err := GetOrCreateAdapter(providerType, providerCfg.UUID, m.GlobalConfig, providerCfg)
	if err != nil {
		return nil, fmt.Errorf("GetAdapterForProvider(%s, uuid=%s): %w", providerType, providerCfg.UUID, err)
	}
	return adapter, nil
}

// SelectAdapter selects a healthy provider for providerType (using the
// existing LRU strategy) and returns the corresponding ServiceAdapter.
// Returns an error when no healthy provider is available or when the
// adapter cannot be instantiated.
func (m *ProviderPoolManager) SelectAdapter(providerType string, model string) (ServiceAdapter, error) {
	pc := m.SelectProvider(providerType, model)
	if pc == nil {
		// Try fallback chain.
		fallbackPc, fallbackType, _ := m.SelectProviderWithFallback(providerType, model)
		if fallbackPc == nil {
			return nil, fmt.Errorf("no healthy provider available for type %s (model=%s)", providerType, model)
		}
		log.Printf("[SelectAdapter] Using fallback provider type=%s for requested=%s", fallbackType, providerType)
		pc = fallbackPc
		providerType = fallbackType
	}

	return m.GetAdapterForProvider(providerType, pc)
}

// GetProviderPools returns the current pool configuration snapshot.
// This satisfies the poolDataProvider interface consumed by the health handler.
func (m *ProviderPoolManager) GetProviderPools() map[string][]cfg.ProviderConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.buildPoolSnapshot()
}

// SetProviderPools replaces the pool data from an externally loaded map and
// re-initialises the provider status.  Used by the config-reload endpoint.
func (m *ProviderPoolManager) SetProviderPools(pools map[string][]cfg.ProviderConfig) {
	m.mu.Lock()
	// Rebuild the internal pools map from the supplied config slice.
	newPools := make(map[string][]*providerState, len(pools))
	for pt, configs := range pools {
		states := make([]*providerState, len(configs))
		for i := range configs {
			c := configs[i]
			if c.UUID == "" {
				continue
			}
			states[i] = &providerState{config: &c}
		}
		newPools[pt] = states
	}
	m.pools = newPools
	m.mu.Unlock()
	m.InitializeProviderStatus()
}
