// Package handlers provides HTTP handler functions for the AI CLI proxy.
package handlers

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"ai-cli-proxy-go/config"

	"github.com/gin-gonic/gin"
)

// HealthResponse is the JSON body returned by GET /health.
type HealthResponse struct {
	Status    string `json:"status"`
	Timestamp string `json:"timestamp"`
	Provider  string `json:"provider"`
}

// HealthHandler returns a Gin handler for GET /health.
//
// It always responds 200 OK with a small JSON body that includes the current
// UTC timestamp and the configured primary model provider.
func HealthHandler(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, HealthResponse{
			Status:    "healthy",
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Provider:  cfg.ModelProvider,
		})
	}
}

// ProviderHealthEntry is the per-provider section in the provider health response.
type ProviderHealthEntry struct {
	UUID             string  `json:"uuid"`
	Provider         string  `json:"provider"`
	CustomName       string  `json:"customName,omitempty"`
	IsHealthy        bool    `json:"isHealthy"`
	IsDisabled       bool    `json:"isDisabled"`
	UsageCount       int     `json:"usageCount"`
	ErrorCount       int     `json:"errorCount"`
	LastUsed         *string `json:"lastUsed"`
	LastErrorTime    *string `json:"lastErrorTime"`
	LastErrorMessage string  `json:"lastErrorMessage,omitempty"`
	CheckModel       string  `json:"checkModel,omitempty"`
}

// ProviderHealthResponse is the full JSON body returned by GET /provider_health.
type ProviderHealthResponse struct {
	Timestamp      string                         `json:"timestamp"`
	TotalProviders int                            `json:"totalProviders"`
	HealthyCount   int                            `json:"healthyCount"`
	UnhealthyCount int                            `json:"unhealthyCount"`
	DisabledCount  int                            `json:"disabledCount"`
	Providers      map[string][]ProviderHealthEntry `json:"providers"`
}

// ProviderHealthHandler returns a Gin handler for GET /provider_health.
//
// Supported query parameters
//
//	provider                – filter to a single provider type (e.g. "gemini-cli-oauth")
//	customName              – filter entries by custom name (case-insensitive substring match)
//	unhealthRatioThreshold  – float 0–1; if set, only providers whose error/usage
//	                          ratio is >= this value are returned.
//
// poolManager is an interface{} so that this handler compiles independently of
// any concrete pool-manager type; at runtime it is expected to be a
// *ProviderPoolManager (or nil) that exposes the same field structure as
// cfg.ProviderPools.
func ProviderHealthHandler(cfg *config.Config, poolManager interface{}) gin.HandlerFunc {
	return func(c *gin.Context) {
		// --- Query parameter parsing ---
		filterProvider := strings.TrimSpace(c.Query("provider"))
		filterCustomName := strings.TrimSpace(c.Query("customName"))
		filterCustomNameLower := strings.ToLower(filterCustomName)

		var unhealthThreshold float64
		var hasThreshold bool
		if raw := c.Query("unhealthRatioThreshold"); raw != "" {
			if v, err := strconv.ParseFloat(raw, 64); err == nil && v >= 0 && v <= 1 {
				unhealthThreshold = v
				hasThreshold = true
			}
		}

		// --- Source the pool data ---
		// We read directly from cfg.ProviderPools which is populated by LoadConfig.
		// A live poolManager would carry updated runtime state; if it implements
		// the poolDataProvider interface we prefer that.
		poolData := resolvePoolData(cfg, poolManager)

		// --- Build response ---
		result := make(map[string][]ProviderHealthEntry)
		var totalCount, healthyCount, unhealthyCount, disabledCount int

		for providerType, entries := range poolData {
			// Provider-type filter.
			if filterProvider != "" && !strings.EqualFold(providerType, filterProvider) {
				continue
			}

			var filtered []ProviderHealthEntry
			for _, pc := range entries {
				// Custom-name filter.
				if filterCustomNameLower != "" &&
					!strings.Contains(strings.ToLower(pc.CustomName), filterCustomNameLower) {
					continue
				}

				// Unhealth-ratio filter.
				if hasThreshold {
					ratio := unhealthRatio(pc.UsageCount, pc.ErrorCount)
					if ratio < unhealthThreshold {
						continue
					}
				}

				entry := ProviderHealthEntry{
					UUID:             pc.UUID,
					Provider:         providerType,
					CustomName:       pc.CustomName,
					IsHealthy:        pc.IsHealthy,
					IsDisabled:       pc.IsDisabled,
					UsageCount:       pc.UsageCount,
					ErrorCount:       pc.ErrorCount,
					LastUsed:         pc.LastUsed,
					LastErrorTime:    pc.LastErrorTime,
					LastErrorMessage: pc.LastErrorMessage,
					CheckModel:       pc.CheckModel,
				}
				filtered = append(filtered, entry)

				totalCount++
				switch {
				case pc.IsDisabled:
					disabledCount++
				case pc.IsHealthy:
					healthyCount++
				default:
					unhealthyCount++
				}
			}

			if len(filtered) > 0 {
				result[providerType] = filtered
			}
		}

		c.JSON(http.StatusOK, ProviderHealthResponse{
			Timestamp:      time.Now().UTC().Format(time.RFC3339),
			TotalProviders: totalCount,
			HealthyCount:   healthyCount,
			UnhealthyCount: unhealthyCount,
			DisabledCount:  disabledCount,
			Providers:      result,
		})
	}
}

// unhealthRatio returns the fraction of requests that resulted in an error.
// Returns 0 when usageCount is 0 to avoid division by zero.
func unhealthRatio(usageCount, errorCount int) float64 {
	if usageCount <= 0 {
		return 0
	}
	return float64(errorCount) / float64(usageCount)
}

// poolDataProvider is the minimal interface a concrete pool manager must
// satisfy for this handler to read live state from it.
type poolDataProvider interface {
	GetProviderPools() map[string][]config.ProviderConfig
}

// resolvePoolData returns the best available snapshot of provider pool state.
// If poolManager implements poolDataProvider, its live data is used; otherwise
// we fall back to the static snapshot in cfg.ProviderPools.
func resolvePoolData(cfg *config.Config, poolManager interface{}) map[string][]config.ProviderConfig {
	if pm, ok := poolManager.(poolDataProvider); ok {
		if data := pm.GetProviderPools(); data != nil {
			return data
		}
	}
	if cfg.ProviderPools != nil {
		return cfg.ProviderPools
	}
	return map[string][]config.ProviderConfig{}
}
