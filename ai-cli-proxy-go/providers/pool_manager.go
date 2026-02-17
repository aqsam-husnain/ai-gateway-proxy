// Package providers contains the ProviderPoolManager which manages pools of
// AI API provider configurations, handling health tracking, round-robin
// selection, fallback routing, and automatic health checks.
package providers

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"
	"time"

	cfg "ai-cli-proxy-go/config"
)

// ---------------------------------------------------------------------------
// Default health-check models per provider type.
// These are used when a pool entry does not specify its own CheckModel.
// ---------------------------------------------------------------------------

var defaultHealthCheckModels = map[string]string{
	ProviderGeminiCLI:   "gemini-2.5-flash",
	ProviderAntigravity: "gemini-2.5-flash",
	ProviderOpenAI:      "gpt-3.5-turbo",
	ProviderClaude:      "claude-3-7-sonnet-20250219",
	ProviderOpenAIResp:  "gpt-4o-mini",
	ProviderClaudeCode:  "haiku",
}

// ---------------------------------------------------------------------------
// Internal per-provider runtime state
// (fields not serialised back to JSON)
// ---------------------------------------------------------------------------

// providerState wraps a ProviderConfig with runtime-only fields.
type providerState struct {
	config *cfg.ProviderConfig

	// Auto health check runtime state (mirrors JS version).
	lastErrorStatusCode       int    // HTTP status code of the last error (0 = none)
	quickRetryCount           int    // how many quick retries have been attempted
	quickRetryPhaseStartTime  *time.Time
	lastQuickRetryTime        *time.Time
	healthCheckScheduleType   string // "quick_retry" | "rate_limit" | "standard" | ""

	// Scheduled health-check timer.
	healthCheckTimer *time.Timer
}

// ---------------------------------------------------------------------------
// ProviderPoolManager
// ---------------------------------------------------------------------------

// ProviderPoolManager manages a pool of API service providers, handling their
// health and selection.  It is safe for concurrent use.
type ProviderPoolManager struct {
	// ProviderPools holds the raw pool slices keyed by provider type.
	ProviderPools map[string][]cfg.ProviderConfig
	// GlobalConfig is a reference to the full application configuration.
	GlobalConfig *cfg.Config

	mu sync.RWMutex

	// Internal runtime state keyed by provider type.
	pools map[string][]*providerState

	roundRobinIndex     map[string]int
	maxErrorCount       int
	healthCheckInterval time.Duration
	fallbackChain       map[string][]string

	// Auto health check settings.
	quickRetryInterval           time.Duration
	quickRetryMaxCount           int
	rateLimitHealthCheckInterval time.Duration
	standardHealthCheckInterval  time.Duration
	autoHealthCheckEnabled       bool

	// Debounced save.
	saveDebounce time.Duration
	saveTimer    *time.Timer
	pendingSaves map[string]struct{}

	// Lifecycle channel.
	stopChan chan struct{}
}

// NewProviderPoolManager constructs a ProviderPoolManager from the supplied
// pool map and global configuration.  It calls InitializeProviderStatus
// immediately, so the manager is ready to use after construction.
func NewProviderPoolManager(pools map[string][]cfg.ProviderConfig, globalCfg *cfg.Config) *ProviderPoolManager {
	m := &ProviderPoolManager{
		ProviderPools: pools,
		GlobalConfig:  globalCfg,

		pools:           make(map[string][]*providerState),
		roundRobinIndex: make(map[string]int),
		fallbackChain:   globalCfg.ProviderFallbackChain,
		pendingSaves:    make(map[string]struct{}),
		stopChan:        make(chan struct{}),

		maxErrorCount:       globalCfg.MaxErrorCount,
		healthCheckInterval: time.Duration(float64(time.Hour) * globalCfg.HealthCheckIntervalHours),

		quickRetryInterval:           time.Duration(globalCfg.QuickRetryIntervalSeconds) * time.Second,
		quickRetryMaxCount:           globalCfg.QuickRetryMaxCount,
		rateLimitHealthCheckInterval: time.Duration(float64(time.Hour) * globalCfg.RateLimitCheckIntervalHours),
		standardHealthCheckInterval:  time.Duration(float64(time.Hour) * globalCfg.StandardCheckIntervalHours),
		autoHealthCheckEnabled:       globalCfg.AutoHealthCheckEnabled,

		saveDebounce: time.Second,
	}

	if m.maxErrorCount <= 0 {
		m.maxErrorCount = 3
	}
	if m.healthCheckInterval <= 0 {
		m.healthCheckInterval = time.Hour
	}
	if m.quickRetryInterval <= 0 {
		m.quickRetryInterval = 10 * time.Second
	}
	if m.quickRetryMaxCount <= 0 {
		m.quickRetryMaxCount = 3
	}
	if m.rateLimitHealthCheckInterval <= 0 {
		m.rateLimitHealthCheckInterval = 3 * time.Hour
	}
	if m.standardHealthCheckInterval <= 0 {
		m.standardHealthCheckInterval = 3 * time.Hour
	}

	m.InitializeProviderStatus()
	return m
}

// ---------------------------------------------------------------------------
// Initialisation
// ---------------------------------------------------------------------------

// InitializeProviderStatus sets default runtime state for every provider in
// every pool and populates the internal pools map.
func (m *ProviderPoolManager) InitializeProviderStatus() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for providerType, providerSlice := range m.ProviderPools {
		m.roundRobinIndex[providerType] = 0
		states := make([]*providerState, 0, len(providerSlice))

		for i := range providerSlice {
			pc := &m.ProviderPools[providerType][i]

			// Apply defaults for fields that may not be set in JSON.
			if !pc.IsHealthy && pc.ErrorCount == 0 {
				// A brand-new entry with no explicit isHealthy=false is healthy.
				pc.IsHealthy = true
			}
			// Always default to healthy when ErrorCount is 0 and IsHealthy unset.
			// (JSON bool zero-value is false, so we do this heuristically.)
			if pc.ErrorCount == 0 && pc.LastErrorTime == nil {
				pc.IsHealthy = true
			}

			ps := &providerState{config: pc}
			states = append(states, ps)
		}

		m.pools[providerType] = states
	}

	log.Printf("[ProviderPoolManager] Initialized provider statuses (maxErrorCount=%d)", m.maxErrorCount)
}

// ---------------------------------------------------------------------------
// Provider selection
// ---------------------------------------------------------------------------

// SelectProvider returns a healthy, non-disabled provider for the given type.
// It uses a Least-Recently-Used (LRU) strategy: it picks the provider with the
// oldest lastUsed timestamp so that usage is spread evenly.
// If requestedModel is non-empty, providers whose notSupportedModels list
// includes that model are excluded.
// Returns nil when no suitable provider is found.
func (m *ProviderPoolManager) SelectProvider(providerType string, requestedModel string) *cfg.ProviderConfig {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.selectLocked(providerType, requestedModel, nil)
}

// selectLocked is the internal, already-locked implementation of SelectProvider.
// excludeUUIDs is an optional set of UUIDs to skip (for retry logic).
func (m *ProviderPoolManager) selectLocked(providerType, requestedModel string, excludeUUIDs map[string]struct{}) *cfg.ProviderConfig {
	states := m.pools[providerType]
	if len(states) == 0 {
		log.Printf("[ProviderPoolManager] No providers configured for type: %s", providerType)
		return nil
	}

	var candidates []*providerState
	for _, ps := range states {
		if !ps.config.IsHealthy || ps.config.IsDisabled {
			continue
		}
		if excludeUUIDs != nil {
			if _, excluded := excludeUUIDs[ps.config.UUID]; excluded {
				continue
			}
		}
		if requestedModel != "" && len(ps.config.NotSupportedModels) > 0 {
			if sliceContains(ps.config.NotSupportedModels, requestedModel) {
				continue
			}
		}
		candidates = append(candidates, ps)
	}

	if len(candidates) == 0 {
		log.Printf("[ProviderPoolManager] No healthy providers for type=%s model=%s", providerType, requestedModel)
		return nil
	}

	// LRU: sort by lastUsed ascending (nil lastUsed = never used = oldest).
	sort.SliceStable(candidates, func(i, j int) bool {
		ti := parseTimePtr(candidates[i].config.LastUsed)
		tj := parseTimePtr(candidates[j].config.LastUsed)
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return candidates[i].config.UsageCount < candidates[j].config.UsageCount
	})

	selected := candidates[0]

	// Update usage stats.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	selected.config.LastUsed = &now
	selected.config.UsageCount++

	m.debouncedSaveLocked(providerType)

	log.Printf("[ProviderPoolManager] Selected provider uuid=%s for type=%s", selected.config.UUID, providerType)
	return selected.config
}

// SelectProviderWithFallback attempts to select a provider for the primary
// type.  When no healthy provider is found it walks the configured fallback
// chain.  It returns (config, actualProviderType, isFallback).
// Returns (nil, "", false) when no healthy provider is found anywhere.
func (m *ProviderPoolManager) SelectProviderWithFallback(providerType string, requestedModel string) (*cfg.ProviderConfig, string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	typesToTry := []string{providerType}
	if fallbacks, ok := m.fallbackChain[providerType]; ok {
		typesToTry = append(typesToTry, fallbacks...)
	}

	tried := make(map[string]struct{})

	for _, currentType := range typesToTry {
		if _, already := tried[currentType]; already {
			continue
		}
		tried[currentType] = struct{}{}

		if len(m.pools[currentType]) == 0 {
			continue
		}

		// For fallback types, enforce protocol compatibility.
		if currentType != providerType && requestedModel != "" {
			primaryProto := GetProtocolPrefix(providerType)
			fallbackProto := GetProtocolPrefix(currentType)
			if primaryProto != fallbackProto {
				log.Printf("[ProviderPoolManager] Skipping fallback %s: protocol mismatch (%s vs %s)",
					currentType, primaryProto, fallbackProto)
				continue
			}
			supported := GetProviderModels(currentType)
			if len(supported) > 0 && !sliceContains(supported, requestedModel) {
				log.Printf("[ProviderPoolManager] Skipping fallback %s: model %s not supported",
					currentType, requestedModel)
				continue
			}
		}

		if pc := m.selectLocked(currentType, requestedModel, nil); pc != nil {
			isFallback := currentType != providerType
			if isFallback {
				log.Printf("[ProviderPoolManager] Fallback activated: %s -> %s (uuid=%s)",
					providerType, currentType, pc.UUID)
			}
			return pc, currentType, isFallback
		}
	}

	log.Printf("[ProviderPoolManager] No provider available for type=%s or any fallback", providerType)
	return nil, "", false
}

// ---------------------------------------------------------------------------
// Health management
// ---------------------------------------------------------------------------

// MarkProviderUnhealthy increments the error count for the identified provider
// and marks it unhealthy once maxErrorCount is reached.
func (m *ProviderPoolManager) MarkProviderUnhealthy(providerType, uuid, errMsg string, isRateLimit bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ps := m.findStateLocked(providerType, uuid)
	if ps == nil {
		log.Printf("[ProviderPoolManager] MarkProviderUnhealthy: uuid=%s not found in type=%s", uuid, providerType)
		return
	}

	ps.config.ErrorCount++
	now := time.Now().UTC().Format(time.RFC3339Nano)
	ps.config.LastErrorTime = &now
	// Advance lastUsed so LRU does not keep re-selecting this failed node.
	ps.config.LastUsed = &now

	if errMsg != "" {
		ps.config.LastErrorMessage = errMsg
	}
	if isRateLimit {
		ps.lastErrorStatusCode = 429
	}

	if ps.config.ErrorCount >= m.maxErrorCount {
		ps.config.IsHealthy = false
		log.Printf("[ProviderPoolManager] Marked UNHEALTHY: uuid=%s type=%s errors=%d/%d msg=%q",
			uuid, providerType, ps.config.ErrorCount, m.maxErrorCount, errMsg)

		if m.autoHealthCheckEnabled {
			m.scheduleHealthCheckLocked(providerType, ps)
		}
	} else {
		log.Printf("[ProviderPoolManager] Provider uuid=%s type=%s error %d/%d, still healthy",
			uuid, providerType, ps.config.ErrorCount, m.maxErrorCount)
	}

	m.debouncedSaveLocked(providerType)
}

// MarkProviderHealthy resets the error state for the identified provider and
// marks it healthy.
func (m *ProviderPoolManager) MarkProviderHealthy(providerType, uuid string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ps := m.findStateLocked(providerType, uuid)
	if ps == nil {
		log.Printf("[ProviderPoolManager] MarkProviderHealthy: uuid=%s not found in type=%s", uuid, providerType)
		return
	}

	ps.config.IsHealthy = true
	ps.config.ErrorCount = 0
	ps.config.LastErrorTime = nil
	ps.config.LastErrorMessage = ""

	// Reset auto health check runtime state.
	ps.lastErrorStatusCode = 0
	ps.quickRetryCount = 0
	ps.quickRetryPhaseStartTime = nil
	ps.lastQuickRetryTime = nil
	ps.healthCheckScheduleType = ""

	// Cancel any pending timer.
	m.clearHealthCheckTimerLocked(ps)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	ps.config.LastHealthCheckTime = &now

	log.Printf("[ProviderPoolManager] Marked HEALTHY: uuid=%s type=%s", uuid, providerType)

	m.debouncedSaveLocked(providerType)
}

// ---------------------------------------------------------------------------
// Health checks
// ---------------------------------------------------------------------------

// PerformHealthChecks iterates over every provider in every pool and performs a
// health check.  When initial is true it will check all providers regardless
// of their current health status; otherwise it only re-checks unhealthy
// providers whose last error is older than the configured healthCheckInterval.
func (m *ProviderPoolManager) PerformHealthChecks(initial bool) error {
	m.mu.RLock()
	// Snapshot the list of providers to check so we can release the lock while
	// performing potentially slow HTTP calls.
	type checkTarget struct {
		providerType string
		ps           *providerState
	}
	var targets []checkTarget

	for providerType, states := range m.pools {
		for _, ps := range states {
			if !initial && ps.config.IsHealthy {
				continue
			}
			if !initial && ps.config.LastErrorTime != nil {
				t, err := time.Parse(time.RFC3339Nano, *ps.config.LastErrorTime)
				if err == nil && time.Since(t) < m.healthCheckInterval {
					log.Printf("[ProviderPoolManager] Skipping health check uuid=%s (last error too recent)", ps.config.UUID)
					continue
				}
			}
			targets = append(targets, checkTarget{providerType: providerType, ps: ps})
		}
	}
	m.mu.RUnlock()

	var firstErr error
	for _, t := range targets {
		if err := m.checkAndUpdateHealth(t.providerType, t.ps, true); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// checkAndUpdateHealth performs a single provider health check by sending a
// minimal "Hi" request and updating health state accordingly.
// forceCheck bypasses the checkHealth flag on the provider config.
func (m *ProviderPoolManager) checkAndUpdateHealth(providerType string, ps *providerState, forceCheck bool) error {
	pc := ps.config
	modelName := pc.CheckModel
	if modelName == "" {
		if def, ok := defaultHealthCheckModels[providerType]; ok {
			modelName = def
		}
	}

	if modelName == "" {
		log.Printf("[ProviderPoolManager] Health check skipped uuid=%s: no check model configured", pc.UUID)
		return nil
	}

	// Build request body.
	var requestBody map[string]interface{}
	switch {
	case GetProtocolPrefix(providerType) == ProtocolGemini:
		requestBody = map[string]interface{}{
			"contents": []map[string]interface{}{
				{
					"role":  "user",
					"parts": []map[string]interface{}{{"text": "Hi"}},
				},
			},
		}
	case providerType == ProviderOpenAIResp:
		requestBody = map[string]interface{}{
			"input": []map[string]interface{}{
				{"role": "user", "content": "Hi"},
			},
			"model": modelName,
		}
	default:
		requestBody = map[string]interface{}{
			"messages": []map[string]interface{}{
				{"role": "user", "content": "Hi"},
			},
			"model": modelName,
		}
	}

	// Obtain adapter from the global registry.
	adapter, err := GetOrCreateAdapter(providerType, pc.UUID, m.GlobalConfig, pc)
	if err != nil {
		log.Printf("[ProviderPoolManager] Health check uuid=%s: could not get adapter: %v", pc.UUID, err)
		m.MarkProviderUnhealthy(providerType, pc.UUID, err.Error(), false)
		return fmt.Errorf("health check adapter error for %s: %w", pc.UUID, err)
	}

	log.Printf("[ProviderPoolManager] Health check uuid=%s type=%s model=%s", pc.UUID, providerType, modelName)
	_, callErr := adapter.GenerateContent(modelName, requestBody)
	if callErr != nil {
		log.Printf("[ProviderPoolManager] Health check FAILED uuid=%s: %v", pc.UUID, callErr)
		m.MarkProviderUnhealthy(providerType, pc.UUID, callErr.Error(), false)
		return callErr
	}

	log.Printf("[ProviderPoolManager] Health check PASSED uuid=%s", pc.UUID)
	m.MarkProviderHealthy(providerType, pc.UUID)
	return nil
}

// ---------------------------------------------------------------------------
// Auto health checks (background goroutines)
// ---------------------------------------------------------------------------

// StartAutoHealthChecks schedules background health checks for all currently
// unhealthy providers.  It is idempotent — calling it multiple times is safe.
func (m *ProviderPoolManager) StartAutoHealthChecks() {
	if !m.autoHealthCheckEnabled {
		log.Printf("[ProviderPoolManager] Auto health checks disabled")
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	log.Printf("[ProviderPoolManager] Starting auto health check system")

	for providerType, states := range m.pools {
		for _, ps := range states {
			if !ps.config.IsHealthy && !ps.config.IsDisabled {
				log.Printf("[ProviderPoolManager] Scheduling initial check for unhealthy uuid=%s", ps.config.UUID)
				m.scheduleHealthCheckLocked(providerType, ps)
			}
		}
	}
}

// StopAutoHealthChecks cancels all pending health-check timers and stops the
// background goroutines.
func (m *ProviderPoolManager) StopAutoHealthChecks() {
	log.Printf("[ProviderPoolManager] Stopping auto health checks")

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, states := range m.pools {
		for _, ps := range states {
			m.clearHealthCheckTimerLocked(ps)
		}
	}

	// Signal the save goroutine to stop (non-blocking).
	select {
	case <-m.stopChan:
	default:
		close(m.stopChan)
	}
}

// scheduleHealthCheckLocked schedules a health check timer for ps based on the
// error type.  Must be called with m.mu held (write).
func (m *ProviderPoolManager) scheduleHealthCheckLocked(providerType string, ps *providerState) {
	m.clearHealthCheckTimerLocked(ps)

	isRateLimit := ps.lastErrorStatusCode == 429

	var delay time.Duration
	if isRateLimit {
		ps.healthCheckScheduleType = "rate_limit"
		ps.quickRetryCount = 0
		ps.quickRetryPhaseStartTime = nil
		delay = m.rateLimitHealthCheckInterval
		log.Printf("[ProviderPoolManager] [Auto] Rate-limit schedule uuid=%s delay=%v", ps.config.UUID, delay)
	} else if ps.quickRetryCount < m.quickRetryMaxCount {
		ps.healthCheckScheduleType = "quick_retry"
		if ps.quickRetryCount == 0 {
			now := time.Now()
			ps.quickRetryPhaseStartTime = &now
		}
		delay = m.quickRetryInterval
		log.Printf("[ProviderPoolManager] [Auto] Quick-retry %d/%d uuid=%s delay=%v",
			ps.quickRetryCount+1, m.quickRetryMaxCount, ps.config.UUID, delay)
	} else {
		ps.healthCheckScheduleType = "standard"
		delay = m.standardHealthCheckInterval
		log.Printf("[ProviderPoolManager] [Auto] Standard schedule uuid=%s delay=%v", ps.config.UUID, delay)
	}

	// Capture loop variables for the goroutine closure.
	capturedType := providerType
	capturedPS := ps

	ps.healthCheckTimer = time.AfterFunc(delay, func() {
		m.executeScheduledHealthCheck(capturedType, capturedPS)
	})
}

// clearHealthCheckTimerLocked stops the timer on ps if one is running.
// Must be called with m.mu held (write).
func (m *ProviderPoolManager) clearHealthCheckTimerLocked(ps *providerState) {
	if ps.healthCheckTimer != nil {
		ps.healthCheckTimer.Stop()
		ps.healthCheckTimer = nil
	}
}

// executeScheduledHealthCheck is called by a timer goroutine.  It runs a health
// check and reschedules if needed.
func (m *ProviderPoolManager) executeScheduledHealthCheck(providerType string, ps *providerState) {
	uuid := ps.config.UUID
	log.Printf("[ProviderPoolManager] [Auto] Executing scheduled check uuid=%s type=%s schedule=%s",
		uuid, providerType, ps.healthCheckScheduleType)

	err := m.checkAndUpdateHealth(providerType, ps, true)
	if err == nil {
		// Provider recovered — timer was already cleared inside MarkProviderHealthy.
		return
	}

	// Still unhealthy — update quick-retry counter and reschedule.
	m.mu.Lock()
	defer m.mu.Unlock()

	if ps.healthCheckScheduleType == "quick_retry" {
		ps.quickRetryCount++
		now := time.Now()
		ps.lastQuickRetryTime = &now
		if ps.quickRetryCount >= m.quickRetryMaxCount {
			log.Printf("[ProviderPoolManager] [Auto] uuid=%s exhausted %d quick retries — switching to standard schedule",
				uuid, m.quickRetryMaxCount)
		}
	}

	m.scheduleHealthCheckLocked(providerType, ps)
}

// ---------------------------------------------------------------------------
// Status reporting
// ---------------------------------------------------------------------------

// ProviderStatusEntry is a slim, serialisable view of a provider's state.
type ProviderStatusEntry struct {
	UUID                 string   `json:"uuid"`
	ProviderType         string   `json:"providerType"`
	CustomName           string   `json:"customName,omitempty"`
	IsHealthy            bool     `json:"isHealthy"`
	IsDisabled           bool     `json:"isDisabled"`
	UsageCount           int      `json:"usageCount"`
	ErrorCount           int      `json:"errorCount"`
	LastUsed             *string  `json:"lastUsed"`
	LastErrorTime        *string  `json:"lastErrorTime"`
	LastErrorMessage     string   `json:"lastErrorMessage,omitempty"`
	LastHealthCheckTime  *string  `json:"lastHealthCheckTime"`
	CheckModel           string   `json:"checkModel,omitempty"`
	ScheduleType         string   `json:"scheduleType,omitempty"`
	QuickRetryCount      int      `json:"quickRetryCount,omitempty"`
	NotSupportedModels   []string `json:"notSupportedModels,omitempty"`
}

// GetProviderStatus returns a filtered, slim status list.
//
//   - filterProvider: if non-empty, only entries with this providerType are returned.
//   - filterCustomName: if non-empty, only entries with this customName are returned.
//   - unhealthRatioThreshold: if > 0, only pool groups where the unhealthy ratio
//     exceeds the threshold are included.
func (m *ProviderPoolManager) GetProviderStatus(filterProvider, filterCustomName string, unhealthRatioThreshold float64) []ProviderStatusEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []ProviderStatusEntry

	for providerType, states := range m.pools {
		if filterProvider != "" && filterProvider != providerType {
			continue
		}

		// Compute unhealthy ratio for this pool group.
		if unhealthRatioThreshold > 0 {
			total := len(states)
			unhealthy := 0
			for _, ps := range states {
				if !ps.config.IsHealthy {
					unhealthy++
				}
			}
			if total > 0 {
				ratio := float64(unhealthy) / float64(total)
				if ratio < unhealthRatioThreshold {
					continue
				}
			}
		}

		for _, ps := range states {
			pc := ps.config
			if filterCustomName != "" && pc.CustomName != filterCustomName {
				continue
			}
			entry := ProviderStatusEntry{
				UUID:                pc.UUID,
				ProviderType:        providerType,
				CustomName:          pc.CustomName,
				IsHealthy:           pc.IsHealthy,
				IsDisabled:          pc.IsDisabled,
				UsageCount:          pc.UsageCount,
				ErrorCount:          pc.ErrorCount,
				LastUsed:            pc.LastUsed,
				LastErrorTime:       pc.LastErrorTime,
				LastErrorMessage:    pc.LastErrorMessage,
				LastHealthCheckTime: pc.LastHealthCheckTime,
				CheckModel:          pc.CheckModel,
				ScheduleType:        ps.healthCheckScheduleType,
				QuickRetryCount:     ps.quickRetryCount,
				NotSupportedModels:  pc.NotSupportedModels,
			}
			result = append(result, entry)
		}
	}

	return result
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

// SaveProviderPools serialises the current pool state back to the JSON file at
// filePath.  The write is debounced: rapid successive calls within the debounce
// window are coalesced into a single write.
func (m *ProviderPoolManager) SaveProviderPools(filePath string) error {
	m.mu.RLock()
	snapshot := m.buildPoolSnapshot()
	m.mu.RUnlock()

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("SaveProviderPools: marshal: %w", err)
	}
	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		return fmt.Errorf("SaveProviderPools: write %s: %w", filePath, err)
	}
	log.Printf("[ProviderPoolManager] Saved provider pools to %s", filePath)
	return nil
}

// buildPoolSnapshot creates a plain map[string][]ProviderConfig from the
// current runtime state.  Must be called with at least a read lock.
func (m *ProviderPoolManager) buildPoolSnapshot() map[string][]cfg.ProviderConfig {
	snapshot := make(map[string][]cfg.ProviderConfig, len(m.pools))
	for providerType, states := range m.pools {
		entries := make([]cfg.ProviderConfig, 0, len(states))
		for _, ps := range states {
			entries = append(entries, *ps.config) // copy
		}
		snapshot[providerType] = entries
	}
	return snapshot
}

// debouncedSaveLocked triggers a debounced save.  Must be called with m.mu
// held (write).
func (m *ProviderPoolManager) debouncedSaveLocked(providerType string) {
	m.pendingSaves[providerType] = struct{}{}

	if m.saveTimer != nil {
		m.saveTimer.Stop()
	}

	filePath := m.GlobalConfig.ProviderPoolsFilePath
	if filePath == "" {
		filePath = "configs/provider_pools.json"
	}

	m.saveTimer = time.AfterFunc(m.saveDebounce, func() {
		if err := m.SaveProviderPools(filePath); err != nil {
			log.Printf("[ProviderPoolManager] Debounced save error: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// findStateLocked looks up a providerState by (providerType, uuid).
// Must be called with m.mu held.
func (m *ProviderPoolManager) findStateLocked(providerType, uuid string) *providerState {
	for _, ps := range m.pools[providerType] {
		if ps.config.UUID == uuid {
			return ps
		}
	}
	return nil
}

// IsAllProvidersUnhealthy returns true if every provider in the pool for the
// given type is unhealthy or disabled.
func (m *ProviderPoolManager) IsAllProvidersUnhealthy(providerType string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	states := m.pools[providerType]
	if len(states) == 0 {
		return true
	}
	for _, ps := range states {
		if ps.config.IsHealthy && !ps.config.IsDisabled {
			return false
		}
	}
	return true
}

// GetProviderStats returns aggregate health statistics for a provider type.
func (m *ProviderPoolManager) GetProviderStats(providerType string) (total, healthy, unhealthy, disabled int) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, ps := range m.pools[providerType] {
		total++
		switch {
		case ps.config.IsDisabled:
			disabled++
		case ps.config.IsHealthy:
			healthy++
		default:
			unhealthy++
		}
	}
	return
}

// parseTimePtr parses an RFC3339Nano timestamp string pointer.
// Returns the zero time when the pointer is nil or the string is unparseable.
func parseTimePtr(s *string) time.Time {
	if s == nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, *s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// sliceContains returns true when needle is in haystack.
func sliceContains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
