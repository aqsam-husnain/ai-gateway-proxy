// Package handlers provides HTTP handler factories for the UI management REST API.
// These endpoints are used by the frontend dashboard and use their own session-token
// auth system (separate from the API key auth used by AI endpoints).
package handlers

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"ai-cli-proxy-go/config"
	"ai-cli-proxy-go/providers"
)

// ---------------------------------------------------------------------------
// Package-level state
// ---------------------------------------------------------------------------

// tokenStore is the in-memory session token store for UI auth.
// Keys are hex-encoded random tokens; values are the time they were issued.
var tokenStore = struct {
	sync.RWMutex
	tokens map[string]time.Time
}{tokens: make(map[string]time.Time)}

// startTime records when the process started, used to compute uptime.
var startTime = time.Now()

// sseClients is the broadcaster registry for Server-Sent Events.
var sseClients = struct {
	sync.RWMutex
	channels map[chan string]struct{}
}{channels: make(map[chan string]struct{})}

// ---------------------------------------------------------------------------
// Log broadcasting — hooks into the standard log package so every log line
// is forwarded to connected SSE clients as an "event: log" frame.
// ---------------------------------------------------------------------------

// logWriter wraps a destination writer and broadcasts each complete line as
// an SSE log event to all connected UI clients.
type logWriter struct {
	dst io.Writer // original destination (os.Stderr)
	buf bytes.Buffer
	mu  sync.Mutex
}

func (lw *logWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()

	// Always write to original destination first.
	n, err := lw.dst.Write(p)

	lw.buf.Write(p)
	// Flush complete lines.
	for {
		line, rest, found := bytes.Cut(lw.buf.Bytes(), []byte("\n"))
		if !found {
			break
		}
		msg := strings.TrimSpace(string(line))
		if msg != "" {
			level := "info"
			lower := strings.ToLower(msg)
			if strings.Contains(lower, "error") || strings.Contains(lower, "fatal") {
				level = "error"
			} else if strings.Contains(lower, "warn") {
				level = "warn"
			}
			entry := map[string]string{
				"timestamp": time.Now().UTC().Format(time.RFC3339),
				"level":     level,
				"message":   msg,
			}
			if data, jsonErr := json.Marshal(entry); jsonErr == nil {
				frame := fmt.Sprintf("event: log\ndata: %s\n\n", data)
				BroadcastRaw(frame)
			}
		}
		lw.buf.Reset()
		lw.buf.Write(rest)
	}
	return n, err
}

// InitLogBroadcaster replaces the standard logger's output with a writer
// that also fans out every log line to SSE clients.
// Call this once from main() after the router is set up.
func InitLogBroadcaster() {
	lw := &logWriter{dst: os.Stderr}
	log.SetOutput(lw)
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// generateToken produces a cryptographically-random 32-byte hex string.
func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// readAdminPassword reads the plaintext password from configs/pwd.
// Returns ("", false) when the file does not exist (any password is accepted).
func readAdminPassword() (string, bool) {
	data, err := os.ReadFile("configs/pwd")
	if err != nil {
		return "", false
	}
	// Trim trailing newline / whitespace.
	pwd := string(data)
	for len(pwd) > 0 && (pwd[len(pwd)-1] == '\n' || pwd[len(pwd)-1] == '\r' || pwd[len(pwd)-1] == ' ') {
		pwd = pwd[:len(pwd)-1]
	}
	return pwd, true
}

// saveConfigToFile serialises cfg to configs/config.json.
func saveConfigToFile(cfg *config.Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("saveConfigToFile: marshal: %w", err)
	}
	if err := os.MkdirAll("configs", 0o755); err != nil {
		return fmt.Errorf("saveConfigToFile: mkdir: %w", err)
	}
	if err := os.WriteFile("configs/config.json", data, 0o644); err != nil {
		return fmt.Errorf("saveConfigToFile: write: %w", err)
	}
	return nil
}

// saveProviderPoolsFile writes the current pool state from poolManager to the
// configured provider pools file.
func saveProviderPoolsFile(cfg *config.Config, poolManager *providers.ProviderPoolManager) error {
	filePath := cfg.ProviderPoolsFilePath
	if filePath == "" {
		filePath = "configs/provider_pools.json"
	}
	return poolManager.SaveProviderPools(filePath)
}

// reloadPoolManager rebuilds the ProviderPoolManager's internal state from
// the pools stored in poolManager.ProviderPools.  This is a lightweight
// re-initialisation used after mutations.
func reloadPoolManager(poolManager *providers.ProviderPoolManager) {
	poolManager.InitializeProviderStatus()
}

// ---------------------------------------------------------------------------
// Auth endpoints
// ---------------------------------------------------------------------------

// HandleUILogin handles POST /api/login.
// It checks the supplied password against configs/pwd (any password is
// accepted when the file is missing) and returns a session token on success.
func HandleUILogin(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			Password string `json:"password"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid request body"})
			return
		}

		storedPwd, fileExists := readAdminPassword()
		if fileExists {
			if body.Password != storedPwd {
				c.JSON(http.StatusUnauthorized, gin.H{"success": false, "message": "Invalid password"})
				return
			}
		}
		// If the file does not exist: any password is accepted (open access).

		token, err := generateToken()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "Failed to generate token"})
			return
		}

		tokenStore.Lock()
		tokenStore.tokens[token] = time.Now()
		tokenStore.Unlock()

		log.Printf("[UILogin] New session token issued")
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"token":   token,
			"message": "Login successful",
		})
	}
}

// HandleUILogout handles POST /api/logout.
// It removes the caller's session token from the store.
func HandleUILogout(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := extractBearerToken(c)
		if token != "" {
			tokenStore.Lock()
			delete(tokenStore.tokens, token)
			tokenStore.Unlock()
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "Logout successful"})
	}
}

// HandleUIHealth handles GET /api/health.
// No authentication is required.
func HandleUIHealth() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":    "ok",
			"timestamp": time.Now().UnixMilli(),
		})
	}
}

// UITokenAuth is a Gin middleware that validates the UI session token.
// It reads the token from the Authorization: Bearer <token> header and
// checks it against the in-memory token store.
func UITokenAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := extractBearerToken(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"success": false, "message": "Unauthorized: missing token"})
			return
		}

		tokenStore.RLock()
		_, ok := tokenStore.tokens[token]
		tokenStore.RUnlock()

		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"success": false, "message": "Unauthorized: invalid or expired token"})
			return
		}
		c.Next()
	}
}

// extractBearerToken reads the token portion from an Authorization: Bearer header.
func extractBearerToken(c *gin.Context) string {
	auth := c.GetHeader("Authorization")
	const prefix = "Bearer "
	if len(auth) > len(prefix) && auth[:len(prefix)] == prefix {
		return auth[len(prefix):]
	}
	return ""
}

// ---------------------------------------------------------------------------
// Config endpoints
// ---------------------------------------------------------------------------

// HandleGetConfig handles GET /api/config.
// Returns the current configuration as JSON plus the system prompt content.
func HandleGetConfig(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Build a map from the config struct (JSON round-trip).
		data, err := json.Marshal(cfg)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to serialise config"})
			return
		}
		var out map[string]interface{}
		if err := json.Unmarshal(data, &out); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to deserialise config"})
			return
		}

		// Append the system prompt content read from the file.
		systemPrompt := ""
		if cfg.SystemPromptFilePath != "" {
			if b, err := os.ReadFile(cfg.SystemPromptFilePath); err == nil {
				systemPrompt = string(b)
			}
		}
		out["systemPrompt"] = systemPrompt

		c.JSON(http.StatusOK, out)
	}
}

// configUpdateRequest is the JSON body accepted by HandleUpdateConfig.
// All fields are optional (pointer or omitempty); only non-nil/non-zero
// fields overwrite the running config.
type configUpdateRequest struct {
	RequiredAPIKey              *string            `json:"REQUIRED_API_KEY"`
	Host                        *string            `json:"HOST"`
	ServerPort                  *int               `json:"SERVER_PORT"`
	ModelProvider               *string            `json:"MODEL_PROVIDER"`
	ProjectID                   *string            `json:"PROJECT_ID"`
	OpenAIAPIKey                *string            `json:"OPENAI_API_KEY"`
	OpenAIBaseURL               *string            `json:"OPENAI_BASE_URL"`
	ClaudeAPIKey                *string            `json:"CLAUDE_API_KEY"`
	ClaudeBaseURL               *string            `json:"CLAUDE_BASE_URL"`
	GeminiOAuthCredsBase64      *string            `json:"GEMINI_OAUTH_CREDS_BASE64"`
	GeminiOAuthCredsFilePath    *string            `json:"GEMINI_OAUTH_CREDS_FILE_PATH"`
	GeminiBaseURL               *string            `json:"GEMINI_BASE_URL"`
	SystemPromptFilePath        *string            `json:"SYSTEM_PROMPT_FILE_PATH"`
	SystemPromptMode            *string            `json:"SYSTEM_PROMPT_MODE"`
	PromptLogBaseName           *string            `json:"PROMPT_LOG_BASE_NAME"`
	PromptLogMode               *string            `json:"PROMPT_LOG_MODE"`
	RequestMaxRetries           *int               `json:"REQUEST_MAX_RETRIES"`
	RequestBaseDelay            *int               `json:"REQUEST_BASE_DELAY"`
	CronNearMinutes             *int               `json:"CRON_NEAR_MINUTES"`
	CronRefreshToken            *bool              `json:"CRON_REFRESH_TOKEN"`
	MaxErrorCount               *int               `json:"MAX_ERROR_COUNT"`
	QuickRetryIntervalSeconds   *int               `json:"QUICK_RETRY_INTERVAL_SECONDS"`
	QuickRetryMaxCount          *int               `json:"QUICK_RETRY_MAX_COUNT"`
	RateLimitCheckIntervalHours *float64           `json:"RATE_LIMIT_CHECK_INTERVAL_HOURS"`
	StandardCheckIntervalHours  *float64           `json:"STANDARD_CHECK_INTERVAL_HOURS"`
	AutoHealthCheckEnabled      *bool              `json:"AUTO_HEALTH_CHECK_ENABLED"`
	ProviderFallbackChain       map[string][]string `json:"providerFallbackChain"`
	SystemPrompt                *string            `json:"systemPrompt"`
}

// HandleUpdateConfig handles POST /api/config.
// Updates any subset of configuration fields in-memory and persists the result.
func HandleUpdateConfig(cfg *config.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req configUpdateRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid request body: " + err.Error()})
			return
		}

		if req.RequiredAPIKey != nil {
			cfg.RequiredAPIKey = *req.RequiredAPIKey
		}
		if req.Host != nil {
			cfg.Host = *req.Host
		}
		if req.ServerPort != nil {
			cfg.ServerPort = *req.ServerPort
		}
		if req.ModelProvider != nil {
			cfg.ModelProvider = *req.ModelProvider
		}
		if req.ProjectID != nil {
			cfg.ProjectID = *req.ProjectID
		}
		if req.OpenAIAPIKey != nil {
			cfg.OpenAIAPIKey = *req.OpenAIAPIKey
		}
		if req.OpenAIBaseURL != nil {
			cfg.OpenAIBaseURL = *req.OpenAIBaseURL
		}
		if req.ClaudeAPIKey != nil {
			cfg.ClaudeAPIKey = *req.ClaudeAPIKey
		}
		if req.ClaudeBaseURL != nil {
			cfg.ClaudeBaseURL = *req.ClaudeBaseURL
		}
		if req.GeminiOAuthCredsBase64 != nil {
			cfg.GeminiOAuthCredsBase64 = *req.GeminiOAuthCredsBase64
		}
		if req.GeminiOAuthCredsFilePath != nil {
			cfg.GeminiOAuthCredsFilePath = *req.GeminiOAuthCredsFilePath
		}
		if req.GeminiBaseURL != nil {
			cfg.GeminiBaseURL = *req.GeminiBaseURL
		}
		if req.SystemPromptFilePath != nil {
			cfg.SystemPromptFilePath = *req.SystemPromptFilePath
		}
		if req.SystemPromptMode != nil {
			cfg.SystemPromptMode = *req.SystemPromptMode
		}
		if req.PromptLogBaseName != nil {
			cfg.PromptLogBaseName = *req.PromptLogBaseName
		}
		if req.PromptLogMode != nil {
			cfg.PromptLogMode = *req.PromptLogMode
		}
		if req.RequestMaxRetries != nil {
			cfg.RequestMaxRetries = *req.RequestMaxRetries
		}
		if req.RequestBaseDelay != nil {
			cfg.RequestBaseDelay = *req.RequestBaseDelay
		}
		if req.CronNearMinutes != nil {
			cfg.CronNearMinutes = *req.CronNearMinutes
		}
		if req.CronRefreshToken != nil {
			cfg.CronRefreshToken = *req.CronRefreshToken
		}
		if req.MaxErrorCount != nil {
			cfg.MaxErrorCount = *req.MaxErrorCount
		}
		if req.QuickRetryIntervalSeconds != nil {
			cfg.QuickRetryIntervalSeconds = *req.QuickRetryIntervalSeconds
		}
		if req.QuickRetryMaxCount != nil {
			cfg.QuickRetryMaxCount = *req.QuickRetryMaxCount
		}
		if req.RateLimitCheckIntervalHours != nil {
			cfg.RateLimitCheckIntervalHours = *req.RateLimitCheckIntervalHours
		}
		if req.StandardCheckIntervalHours != nil {
			cfg.StandardCheckIntervalHours = *req.StandardCheckIntervalHours
		}
		if req.AutoHealthCheckEnabled != nil {
			cfg.AutoHealthCheckEnabled = *req.AutoHealthCheckEnabled
		}
		if req.ProviderFallbackChain != nil {
			cfg.ProviderFallbackChain = req.ProviderFallbackChain
		}

		// Write the system prompt to the configured file if provided.
		if req.SystemPrompt != nil && cfg.SystemPromptFilePath != "" {
			dir := filepath.Dir(cfg.SystemPromptFilePath)
			if err := os.MkdirAll(dir, 0o755); err == nil {
				_ = os.WriteFile(cfg.SystemPromptFilePath, []byte(*req.SystemPrompt), 0o644)
			}
			cfg.SystemPromptContent = *req.SystemPrompt
		}

		if err := saveConfigToFile(cfg); err != nil {
			log.Printf("[HandleUpdateConfig] save error: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "Failed to persist configuration: " + err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "message": "Configuration updated successfully"})
	}
}

// HandleGetSystem handles GET /api/system.
// Returns runtime system information.
func HandleGetSystem(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)

		allocMB := float64(ms.Alloc) / 1024 / 1024
		sysMB := float64(ms.Sys) / 1024 / 1024

		// gcCPU is the fraction of CPU time spent in GC (0–1 range); convert to %.
		// For a simple proxy, this is a reasonable approximation of CPU activity
		// visible without CGo or OS-level sampling.
		gcCPUPct := ms.GCCPUFraction * 100
		cpuUsage := fmt.Sprintf("%.1f%%", gcCPUPct)

		c.JSON(http.StatusOK, gin.H{
			"appVersion":  "1.0.0",
			"goVersion":   runtime.Version(),
			"serverTime":  time.Now().UTC().Format(time.RFC3339),
			"memoryUsage": fmt.Sprintf("%.1f MB / %.1f MB", allocMB, sysMB),
			"cpuUsage":    cpuUsage,
			"uptime":      time.Since(startTime).Seconds(),
			"platform":    runtime.GOOS,
			"pid":         os.Getpid(),
		})
	}
}

// ---------------------------------------------------------------------------
// Provider endpoints
// ---------------------------------------------------------------------------

// HandleGetProviders handles GET /api/providers.
// Returns the full provider pools map.
func HandleGetProviders(cfg *config.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		pools := poolManager.GetProviderPools()
		c.JSON(http.StatusOK, pools)
	}
}

// HandleGetProviderType handles GET /api/providers/:providerType.
// Returns providers for a specific provider type with counts.
func HandleGetProviderType(cfg *config.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		providerType := c.Param("providerType")
		pools := poolManager.GetProviderPools()
		providerList, ok := pools[providerType]
		if !ok {
			providerList = []config.ProviderConfig{}
		}

		healthyCount := 0
		for _, p := range providerList {
			if p.IsHealthy && !p.IsDisabled {
				healthyCount++
			}
		}

		c.JSON(http.StatusOK, gin.H{
			"providerType": providerType,
			"providers":    providerList,
			"totalCount":   len(providerList),
			"healthyCount": healthyCount,
		})
	}
}

// uiProviderModels is the hardcoded map of provider type -> model list,
// matching the JS getProviderModels() frontend function.
var uiProviderModels = map[string][]string{
	"gemini-cli-oauth": {
		"gemini-2.5-pro",
		"gemini-2.5-flash",
		"gemini-2.0-flash",
		"gemini-1.5-pro",
		"gemini-1.5-flash",
	},
	"openai-custom": {
		"gpt-4o",
		"gpt-4o-mini",
		"gpt-4-turbo",
		"gpt-3.5-turbo",
	},
	"claude-custom": {
		"claude-opus-4-6",
		"claude-sonnet-4-6",
		"claude-haiku-4-5-20251001",
		"claude-3-5-sonnet-20241022",
		"claude-3-7-sonnet-20250219",
	},
	"claudeCode-custom": {
		"haiku",
		"sonnet",
		"opus",
	},
	"gemini-antigravity": {
		"gemini-2.5-pro",
		"gemini-2.5-flash",
	},
}

// HandleGetProviderModels handles GET /api/provider-models.
// Returns all provider types with their model lists.
func HandleGetProviderModels() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, uiProviderModels)
	}
}

// HandleGetProviderModelsByType handles GET /api/provider-models/:providerType.
// Returns the model list for a specific provider type.
func HandleGetProviderModelsByType() gin.HandlerFunc {
	return func(c *gin.Context) {
		providerType := c.Param("providerType")
		models, ok := uiProviderModels[providerType]
		if !ok {
			models = []string{}
		}
		c.JSON(http.StatusOK, gin.H{
			"providerType": providerType,
			"models":       models,
		})
	}
}

// HandleAddProvider handles POST /api/providers.
// Adds a new provider to the pool and persists the change.
func HandleAddProvider(cfg *config.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			ProviderType   string                 `json:"providerType"`
			ProviderConfig map[string]interface{} `json:"providerConfig"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid request body: " + err.Error()})
			return
		}
		if body.ProviderType == "" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "providerType is required"})
			return
		}

		// Round-trip via JSON to populate a ProviderConfig.
		raw, err := json.Marshal(body.ProviderConfig)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid providerConfig"})
			return
		}
		var pc config.ProviderConfig
		if err := json.Unmarshal(raw, &pc); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid providerConfig: " + err.Error()})
			return
		}

		// Generate a UUID if missing.
		if pc.UUID == "" {
			uuid, err := generateUUID()
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "Failed to generate UUID"})
				return
			}
			pc.UUID = uuid
		}

		// Set defaults.
		pc.IsHealthy = true
		pc.IsDisabled = false
		pc.UsageCount = 0
		pc.ErrorCount = 0
		pc.LastUsed = nil
		pc.LastErrorTime = nil

		// Append to ProviderPools.
		poolManager.ProviderPools[body.ProviderType] = append(poolManager.ProviderPools[body.ProviderType], pc)
		reloadPoolManager(poolManager)

		if err := saveProviderPoolsFile(cfg, poolManager); err != nil {
			log.Printf("[HandleAddProvider] save error: %v", err)
		}

		c.JSON(http.StatusOK, gin.H{
			"success":      true,
			"provider":     pc,
			"providerType": body.ProviderType,
		})
	}
}

// HandleUpdateProvider handles PUT /api/providers/:providerType/:uuid.
// Merges the supplied providerConfig into the existing entry.
func HandleUpdateProvider(cfg *config.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		providerType := c.Param("providerType")
		uuid := c.Param("uuid")

		var body struct {
			ProviderConfig map[string]interface{} `json:"providerConfig"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid request body: " + err.Error()})
			return
		}

		pool, ok := poolManager.ProviderPools[providerType]
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "Provider type not found"})
			return
		}

		idx := -1
		for i, p := range pool {
			if p.UUID == uuid {
				idx = i
				break
			}
		}
		if idx == -1 {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "Provider not found"})
			return
		}

		existing := pool[idx]

		// Merge: marshal incoming fields over a copy of the existing config,
		// then restore the fields that must not be overwritten by the caller.
		raw, err := json.Marshal(body.ProviderConfig)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid providerConfig"})
			return
		}
		if err := json.Unmarshal(raw, &existing); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid providerConfig: " + err.Error()})
			return
		}

		// Preserve immutable / runtime-managed fields.
		existing.UUID = pool[idx].UUID
		existing.LastUsed = pool[idx].LastUsed
		existing.UsageCount = pool[idx].UsageCount
		existing.ErrorCount = pool[idx].ErrorCount
		existing.LastErrorTime = pool[idx].LastErrorTime

		poolManager.ProviderPools[providerType][idx] = existing
		reloadPoolManager(poolManager)

		if err := saveProviderPoolsFile(cfg, poolManager); err != nil {
			log.Printf("[HandleUpdateProvider] save error: %v", err)
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "provider": existing})
	}
}

// HandleDeleteProvider handles DELETE /api/providers/:providerType/:uuid.
// Removes the provider from the pool and persists the change.
func HandleDeleteProvider(cfg *config.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		providerType := c.Param("providerType")
		uuid := c.Param("uuid")

		pool, ok := poolManager.ProviderPools[providerType]
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "Provider type not found"})
			return
		}

		idx := -1
		for i, p := range pool {
			if p.UUID == uuid {
				idx = i
				break
			}
		}
		if idx == -1 {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "Provider not found"})
			return
		}

		deleted := pool[idx]
		poolManager.ProviderPools[providerType] = append(pool[:idx], pool[idx+1:]...)
		reloadPoolManager(poolManager)

		if err := saveProviderPoolsFile(cfg, poolManager); err != nil {
			log.Printf("[HandleDeleteProvider] save error: %v", err)
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "deletedProvider": deleted})
	}
}

// HandleDisableProvider handles POST /api/providers/:providerType/:uuid/disable.
func HandleDisableProvider(cfg *config.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		providerType := c.Param("providerType")
		uuid := c.Param("uuid")

		if err := setProviderDisabled(cfg, poolManager, providerType, uuid, true); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "message": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "Provider disabled"})
	}
}

// HandleEnableProvider handles POST /api/providers/:providerType/:uuid/enable.
func HandleEnableProvider(cfg *config.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		providerType := c.Param("providerType")
		uuid := c.Param("uuid")

		if err := setProviderDisabled(cfg, poolManager, providerType, uuid, false); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "message": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "Provider enabled"})
	}
}

// setProviderDisabled is a shared helper for enable/disable operations.
func setProviderDisabled(cfg *config.Config, poolManager *providers.ProviderPoolManager, providerType, uuid string, disabled bool) error {
	pool, ok := poolManager.ProviderPools[providerType]
	if !ok {
		return fmt.Errorf("provider type not found: %s", providerType)
	}
	for i, p := range pool {
		if p.UUID == uuid {
			poolManager.ProviderPools[providerType][i].IsDisabled = disabled
			reloadPoolManager(poolManager)
			if err := saveProviderPoolsFile(cfg, poolManager); err != nil {
				log.Printf("[setProviderDisabled] save error: %v", err)
			}
			return nil
		}
	}
	return fmt.Errorf("provider not found: %s", uuid)
}

// HandleResetProviderHealth handles POST /api/providers/:providerType/reset-health.
// Resets isHealthy, errorCount and lastErrorTime for every provider of the given type.
func HandleResetProviderHealth(cfg *config.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		providerType := c.Param("providerType")

		pool, ok := poolManager.ProviderPools[providerType]
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "Provider type not found"})
			return
		}

		for i := range pool {
			poolManager.ProviderPools[providerType][i].IsHealthy = true
			poolManager.ProviderPools[providerType][i].ErrorCount = 0
			poolManager.ProviderPools[providerType][i].LastErrorTime = nil
		}
		reloadPoolManager(poolManager)

		if err := saveProviderPoolsFile(cfg, poolManager); err != nil {
			log.Printf("[HandleResetProviderHealth] save error: %v", err)
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "message": fmt.Sprintf("Health reset for all providers of type %s", providerType)})
	}
}

// ---------------------------------------------------------------------------
// SSE broadcast
// ---------------------------------------------------------------------------

// BroadcastRaw sends a pre-formatted SSE frame to all registered clients.
func BroadcastRaw(frame string) {
	sseClients.RLock()
	defer sseClients.RUnlock()
	for ch := range sseClients.channels {
		select {
		case ch <- frame:
		default:
		}
	}
}

// BroadcastEvent sends a Server-Sent Event to all registered SSE clients.
// eventType is the SSE event name; data is the JSON payload string.
func BroadcastEvent(eventType, data string) {
	BroadcastRaw(fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, data))
}

// HandleSSEEvents handles GET /api/events.
// Establishes a Server-Sent Events stream and keeps it alive with pings.
func HandleSSEEvents(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")

		clientCh := make(chan string, 32)

		sseClients.Lock()
		sseClients.channels[clientCh] = struct{}{}
		sseClients.Unlock()

		defer func() {
			sseClients.Lock()
			delete(sseClients.channels, clientCh)
			sseClients.Unlock()
			close(clientCh)
		}()

		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		// Send an initial ping so the client knows the stream is live.
		c.SSEvent("", `{"type":"ping"}`)
		c.Writer.Flush()

		notify := c.Request.Context().Done()

		for {
			select {
			case <-notify:
				return
			case msg, ok := <-clientCh:
				if !ok {
					return
				}
				// msg is already a fully-formatted SSE frame; write it raw.
				_, err := io.WriteString(c.Writer, msg)
				if err != nil {
					return
				}
				c.Writer.Flush()
			case <-ticker.C:
				_, err := io.WriteString(c.Writer, "data: {\"type\":\"ping\"}\n\n")
				if err != nil {
					return
				}
				c.Writer.Flush()
			}
		}
	}
}

// ---------------------------------------------------------------------------
// File upload
// ---------------------------------------------------------------------------

// HandleUploadOAuthCredentials handles POST /api/upload-oauth-credentials.
// Saves an OAuth credentials JSON file to configs/{provider}/{filename}.
func HandleUploadOAuthCredentials(cfg *config.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Failed to parse multipart form: " + err.Error()})
			return
		}

		provider := c.PostForm("provider")
		if provider == "" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "provider field is required"})
			return
		}

		fileHeader, err := c.FormFile("file")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "file field is required: " + err.Error()})
			return
		}

		originalName := fileHeader.Filename
		destDir := filepath.Join("configs", provider)
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "Failed to create directory: " + err.Error()})
			return
		}

		destPath := filepath.Join(destDir, originalName)

		src, err := fileHeader.Open()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "Failed to open uploaded file: " + err.Error()})
			return
		}
		defer func(src multipart.File) {
			_ = src.Close()
		}(src)

		dst, err := os.Create(destPath)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "Failed to create destination file: " + err.Error()})
			return
		}
		defer func(dst *os.File) {
			_ = dst.Close()
		}(dst)

		if _, err := io.Copy(dst, src); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "Failed to save file: " + err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"success":      true,
			"filePath":     destPath,
			"originalName": originalName,
			"provider":     provider,
		})
	}
}

// ---------------------------------------------------------------------------
// Admin password
// ---------------------------------------------------------------------------

// HandleUpdateAdminPassword handles POST /api/admin-password.
// Writes the new plaintext password to configs/pwd.
func HandleUpdateAdminPassword() gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			Password string `json:"password"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid request body: " + err.Error()})
			return
		}
		if body.Password == "" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "password is required"})
			return
		}

		if err := os.MkdirAll("configs", 0o755); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "Failed to create configs directory: " + err.Error()})
			return
		}
		if err := os.WriteFile("configs/pwd", []byte(body.Password), 0o600); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "Failed to write password file: " + err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "message": "Admin password updated successfully"})
	}
}

// ---------------------------------------------------------------------------
// Service mode endpoint
// ---------------------------------------------------------------------------

// HandleServiceMode handles GET /api/service-mode.
// Returns process/service mode info (always "standalone" in Go binary).
func HandleServiceMode() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"mode":          "standalone",
			"pid":           os.Getpid(),
			"uptime":        time.Since(startTime).Seconds(),
			"canAutoRestart": false,
			"platform":      runtime.GOOS,
			"goVersion":     runtime.Version(),
		})
	}
}

// ---------------------------------------------------------------------------
// Metrics endpoints (stub responses — metrics DB not implemented yet)
// ---------------------------------------------------------------------------

// emptyTimeSeries returns a zero-filled time series for the given range.
func emptyTimeSeries(rangeStr string) gin.H {
	points := 24
	if rangeStr == "7d" {
		points = 7
	} else if rangeStr == "30d" {
		points = 30
	}
	labels := make([]string, points)
	data := make([]int, points)
	for i := range labels {
		labels[i] = fmt.Sprintf("t-%d", points-i)
	}
	return gin.H{"labels": labels, "data": data, "total": 0}
}

// HandleMetricsOverview handles GET /api/metrics/overview.
func HandleMetricsOverview() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"totalRequests":   0,
			"successRequests": 0,
			"failedRequests":  0,
			"avgLatency":      0,
			"totalTokens":     0,
			"inputTokens":     0,
			"outputTokens":    0,
			"cacheHitRate":    0,
			"activeProviders": 0,
		})
	}
}

// HandleMetricsRequests handles GET /api/metrics/requests.
func HandleMetricsRequests() gin.HandlerFunc {
	return func(c *gin.Context) {
		rangeStr := c.DefaultQuery("range", "24h")
		c.JSON(http.StatusOK, emptyTimeSeries(rangeStr))
	}
}

// HandleMetricsLatency handles GET /api/metrics/latency.
func HandleMetricsLatency() gin.HandlerFunc {
	return func(c *gin.Context) {
		rangeStr := c.DefaultQuery("range", "24h")
		c.JSON(http.StatusOK, emptyTimeSeries(rangeStr))
	}
}

// HandleMetricsErrors handles GET /api/metrics/errors.
func HandleMetricsErrors() gin.HandlerFunc {
	return func(c *gin.Context) {
		rangeStr := c.DefaultQuery("range", "24h")
		c.JSON(http.StatusOK, emptyTimeSeries(rangeStr))
	}
}

// HandleMetricsTokens handles GET /api/metrics/tokens.
func HandleMetricsTokens() gin.HandlerFunc {
	return func(c *gin.Context) {
		rangeStr := c.DefaultQuery("range", "24h")
		c.JSON(http.StatusOK, emptyTimeSeries(rangeStr))
	}
}

// HandleMetricsProviderHealthTimeline handles GET /api/metrics/providers/health-timeline.
func HandleMetricsProviderHealthTimeline() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"timeline": []any{}, "providers": []string{}})
	}
}

// HandleMetricsProviderLoad handles GET /api/metrics/providers/load.
func HandleMetricsProviderLoad() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"providers": []any{}})
	}
}

// HandleCacheStats handles GET /api/cache/stats.
func HandleCacheStats() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"enabled":  false,
			"hitRate":  0,
			"hits":     0,
			"misses":   0,
			"size":     0,
			"maxSize":  0,
		})
	}
}

// ---------------------------------------------------------------------------
// Usage endpoints (stub — reads from provider pools only)
// ---------------------------------------------------------------------------

// HandleGetUsage handles GET /api/usage.
func HandleGetUsage(cfg *config.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		pools := poolManager.GetProviderPools()
		result := gin.H{}
		for pt, providers := range pools {
			totalUsage := 0
			totalErrors := 0
			for _, p := range providers {
				totalUsage += p.UsageCount
				totalErrors += p.ErrorCount
			}
			result[pt] = gin.H{
				"providerType":  pt,
				"totalUsage":    totalUsage,
				"totalErrors":   totalErrors,
				"accountCount":  len(providers),
				"fromCache":     false,
			}
		}
		c.JSON(http.StatusOK, result)
	}
}

// HandleGetUsageByProvider handles GET /api/usage/:providerType.
func HandleGetUsageByProvider(cfg *config.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		pt := c.Param("providerType")
		pools := poolManager.GetProviderPools()
		list := pools[pt]
		totalUsage := 0
		totalErrors := 0
		for _, p := range list {
			totalUsage += p.UsageCount
			totalErrors += p.ErrorCount
		}
		c.JSON(http.StatusOK, gin.H{
			"providerType":  pt,
			"totalUsage":    totalUsage,
			"totalErrors":   totalErrors,
			"accountCount":  len(list),
			"fromCache":     false,
		})
	}
}

// ---------------------------------------------------------------------------
// Upload-configs endpoints (lists credential/config files under configs/)
// ---------------------------------------------------------------------------

// configFileInfo represents a credential file entry for the UI.
type configFileInfo struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Type    string `json:"type"`
	IsUsed  bool   `json:"isUsed"`
	Size    int64  `json:"size"`
	ModTime string `json:"modTime"`
	Content string `json:"content,omitempty"`
}

// walkConfigFiles walks configs/ and returns a list of JSON/txt files.
func walkConfigFiles(cfg *config.Config) []configFileInfo {
	var files []configFileInfo
	_ = filepath.Walk("configs", func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		ext := filepath.Ext(p)
		if ext != ".json" && ext != ".txt" && ext != ".yaml" && ext != ".yml" {
			return nil
		}
		// Skip the main config and pools file from this list.
		if p == "configs/config.json" || p == "configs/provider_pools.json" {
			return nil
		}
		fileType := "config"
		dir := filepath.Dir(p)
		if filepath.Base(dir) == "gemini" || filepath.Base(dir) == "antigravity" {
			fileType = "oauth"
		} else if ext == ".txt" {
			fileType = "system-prompt"
		}
		isUsed := cfg.GeminiOAuthCredsFilePath == p || cfg.SystemPromptFilePath == p
		files = append(files, configFileInfo{
			Name:    info.Name(),
			Path:    p,
			Type:    fileType,
			IsUsed:  isUsed,
			Size:    info.Size(),
			ModTime: info.ModTime().UTC().Format(time.RFC3339),
		})
		return nil
	})
	return files
}

// HandleGetUploadConfigs handles GET /api/upload-configs.
func HandleGetUploadConfigs(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		files := walkConfigFiles(cfg)
		if files == nil {
			files = []configFileInfo{}
		}
		c.JSON(http.StatusOK, files)
	}
}

// HandleViewUploadConfig handles GET /api/upload-configs/view/*path.
func HandleViewUploadConfig() gin.HandlerFunc {
	return func(c *gin.Context) {
		p := c.Param("path")
		// Strip leading slash added by wildcard
		if len(p) > 0 && p[0] == '/' {
			p = p[1:]
		}
		// Prevent path traversal
		clean := filepath.Clean(p)
		if !filepath.IsLocal(clean) {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid path"})
			return
		}
		data, err := os.ReadFile(clean)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "File not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"path":    clean,
			"content": string(data),
		})
	}
}

// HandleDeleteUploadConfig handles DELETE /api/upload-configs/delete/*path.
func HandleDeleteUploadConfig() gin.HandlerFunc {
	return func(c *gin.Context) {
		p := c.Param("path")
		if len(p) > 0 && p[0] == '/' {
			p = p[1:]
		}
		clean := filepath.Clean(p)
		if !filepath.IsLocal(clean) {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid path"})
			return
		}
		if err := os.Remove(clean); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "File deleted"})
	}
}

// HandleDownloadAllConfigs handles GET /api/upload-configs/download-all.
// Sends all config files as a JSON array (simple approach; the original sends a zip).
func HandleDownloadAllConfigs(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		files := walkConfigFiles(cfg)
		for i := range files {
			data, _ := os.ReadFile(files[i].Path)
			files[i].Content = string(data)
		}
		c.JSON(http.StatusOK, files)
	}
}

// ---------------------------------------------------------------------------
// Provider auto-link (mirrors autoLinkProviderConfigs from service-manager.js)
// ---------------------------------------------------------------------------

// providerMapping maps a configs/ subdirectory to a provider type and its
// credential path field name — mirrors PROVIDER_MAPPINGS in provider-utils.js.
type providerMapping struct {
	dirName           string
	providerType      string
	credPathKey       string
	defaultCheckModel string
	needsProjectId    bool
	urlKeys           []string
}

var providerMappings = []providerMapping{
	{
		dirName:           "gemini",
		providerType:      "gemini-cli-oauth",
		credPathKey:       "GEMINI_OAUTH_CREDS_FILE_PATH",
		defaultCheckModel: "gemini-2.5-flash",
		needsProjectId:    true,
		urlKeys:           []string{"GEMINI_BASE_URL"},
	},
	{
		dirName:           "antigravity",
		providerType:      "gemini-antigravity",
		credPathKey:       "ANTIGRAVITY_OAUTH_CREDS_FILE_PATH",
		defaultCheckModel: "gemini-2.5-computer-use-preview-10-2025",
		needsProjectId:    true,
		urlKeys:           []string{"ANTIGRAVITY_BASE_URL_DAILY", "ANTIGRAVITY_BASE_URL_AUTOPUSH"},
	},
}

// autoLinkProviderConfigs scans configs/{dirName}/ for JSON credential files
// that aren't already in the pool, creates a new ProviderConfig for each, and
// saves the updated pools file — mirrors autoLinkProviderConfigs in service-manager.js.
func autoLinkProviderConfigs(cfg *config.Config, poolManager *providers.ProviderPoolManager) {
	poolsFile := cfg.ProviderPoolsFilePath
	if poolsFile == "" {
		poolsFile = "configs/provider_pools.json"
	}

	// Load current pools from disk so we have the freshest state.
	pools := poolManager.GetProviderPools()

	changed := false

	for _, mapping := range providerMappings {
		dir := filepath.Join("configs", mapping.dirName)

		// Build set of already-linked paths for this provider type.
		linked := make(map[string]struct{})
		for _, p := range pools[mapping.providerType] {
			val := providerConfigField(p, mapping.credPathKey)
			if val != "" {
				linked[normalizeLinkPath(val)] = struct{}{}
			}
		}

		// Scan the directory for JSON files.
		_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || filepath.Ext(p) != ".json" {
				return nil
			}
			norm := normalizeLinkPath(p)
			if _, exists := linked[norm]; exists {
				return nil // already linked
			}

			// Validate it's an OAuth creds file.
			if !isOAuthCredsFile(p) {
				return nil
			}

			// Create new provider config entry.
			uuid, _ := generateUUID()
			entry := config.ProviderConfig{
				UUID:        uuid,
				CheckModel:  mapping.defaultCheckModel,
				CheckHealth: false,
				IsHealthy:   true,
				IsDisabled:  false,
				UsageCount:  0,
				ErrorCount:  0,
			}
			// Set the credential path field dynamically.
			setProviderConfigField(&entry, mapping.credPathKey, "./"+norm)
			if mapping.needsProjectId {
				entry.ProjectID = ""
			}

			pools[mapping.providerType] = append(pools[mapping.providerType], entry)
			linked[norm] = struct{}{}
			changed = true
			log.Printf("[AutoLink] Linked %s -> %s (uuid=%s)", mapping.providerType, p, uuid)
			return nil
		})
	}

	if !changed {
		return
	}

	// Persist to disk.
	data, err := json.MarshalIndent(pools, "", "  ")
	if err != nil {
		log.Printf("[AutoLink] Failed to marshal pools: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(poolsFile), 0o755); err == nil {
		if err := os.WriteFile(poolsFile, data, 0o644); err != nil {
			log.Printf("[AutoLink] Failed to save pools: %v", err)
		}
	}

	// Refresh live pool manager.
	poolManager.SetProviderPools(pools)

	// Broadcast provider_update so the UI refreshes.
	payload, _ := json.Marshal(map[string]string{"action": "auto_link", "timestamp": time.Now().UTC().Format(time.RFC3339)})
	BroadcastEvent("provider_update", string(payload))
}

// normalizeLinkPath strips leading ./ and converts backslashes.
func normalizeLinkPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(p, "./")
	return p
}

// isOAuthCredsFile checks if a JSON file contains OAuth token fields.
func isOAuthCredsFile(p string) bool {
	data, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return false
	}
	for _, key := range []string{"access_token", "refresh_token", "accessToken", "refreshToken", "token", "credentials"} {
		if _, ok := m[key]; ok {
			return true
		}
	}
	return false
}

// providerConfigField reads a dynamic credential-path field by name.
func providerConfigField(p config.ProviderConfig, key string) string {
	switch key {
	case "GEMINI_OAUTH_CREDS_FILE_PATH":
		return p.GeminiOAuthCredsFilePath
	case "ANTIGRAVITY_OAUTH_CREDS_FILE_PATH":
		return p.AntigravityOAuthCredsFilePath
	}
	return ""
}

// setProviderConfigField writes a dynamic credential-path field by name.
func setProviderConfigField(p *config.ProviderConfig, key, val string) {
	switch key {
	case "GEMINI_OAUTH_CREDS_FILE_PATH":
		p.GeminiOAuthCredsFilePath = val
	case "ANTIGRAVITY_OAUTH_CREDS_FILE_PATH":
		p.AntigravityOAuthCredsFilePath = val
	}
}

// ---------------------------------------------------------------------------
// Reload config / quick-link endpoints
// ---------------------------------------------------------------------------

// HandleReloadConfig handles POST /api/reload-config.
func HandleReloadConfig(cfg *config.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Reload provider pools from disk.
		poolsFile := cfg.ProviderPoolsFilePath
		if poolsFile == "" {
			poolsFile = "configs/provider_pools.json"
		}
		data, err := os.ReadFile(poolsFile)
		if err == nil {
			var pools map[string][]config.ProviderConfig
			if jsonErr := json.Unmarshal(data, &pools); jsonErr == nil {
				poolManager.SetProviderPools(pools)
				reloadPoolManager(poolManager)
			}
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "Configuration reloaded"})
	}
}

// HandleQuickLinkProvider handles POST /api/quick-link-provider.
func HandleQuickLinkProvider(cfg *config.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			FilePath     string `json:"filePath"`
			ProviderType string `json:"providerType"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid request"})
			return
		}
		// For oauth providers, update the creds file path in config.
		switch body.ProviderType {
		case "gemini-cli-oauth":
			cfg.GeminiOAuthCredsFilePath = body.FilePath
		case "gemini-antigravity":
			// antigravity uses the same base field for now
			cfg.GeminiOAuthCredsFilePath = body.FilePath
		}
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"message": fmt.Sprintf("Linked %s to %s", body.ProviderType, body.FilePath),
		})
	}
}

// HandleGenerateAuthURL handles POST /api/providers/:providerType/generate-auth-url.
// Starts a local OAuth callback server and returns the Google authorization URL.
// Requires env vars:
//
//	GEMINI_CLI_OAUTH_CLIENT_ID / GEMINI_CLI_OAUTH_CLIENT_SECRET
//	GEMINI_ANTIGRAVITY_OAUTH_CLIENT_ID / GEMINI_ANTIGRAVITY_OAUTH_CLIENT_SECRET
func HandleGenerateAuthURL(cfg *config.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		providerType := c.Param("providerType")

		type reqBody struct {
			Port        int    `json:"port"`
			SaveToConfigs bool  `json:"saveToConfigs"`
			ProviderDir string `json:"providerDir"`
		}
		var body reqBody
		_ = c.ShouldBindJSON(&body)

		var clientID, clientSecret string
		var defaultPort int
		var providerDir string

		switch providerType {
		case "gemini-cli-oauth":
			clientID = os.Getenv("GEMINI_CLI_OAUTH_CLIENT_ID")
			clientSecret = os.Getenv("GEMINI_CLI_OAUTH_CLIENT_SECRET")
			defaultPort = 8085
			providerDir = "gemini"
		case "gemini-antigravity":
			clientID = os.Getenv("GEMINI_ANTIGRAVITY_OAUTH_CLIENT_ID")
			clientSecret = os.Getenv("GEMINI_ANTIGRAVITY_OAUTH_CLIENT_SECRET")
			defaultPort = 8086
			providerDir = "antigravity"
		default:
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"message": fmt.Sprintf("Unsupported provider type for OAuth: %s", providerType),
			})
			return
		}

		if clientID == "" || clientSecret == "" {
			var envPrefix string
			switch providerType {
			case "gemini-cli-oauth":
				envPrefix = "GEMINI_CLI"
			case "gemini-antigravity":
				envPrefix = "GEMINI_ANTIGRAVITY"
			}
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"message": fmt.Sprintf(
					"OAuth client ID/secret not configured for %s. Set %s_OAUTH_CLIENT_ID and %s_OAUTH_CLIENT_SECRET environment variables.",
					providerType, envPrefix, envPrefix,
				),
			})
			return
		}

		port := defaultPort
		if body.Port > 0 {
			port = body.Port
		}
		if body.ProviderDir != "" {
			providerDir = body.ProviderDir
		}
		saveToConfigs := body.SaveToConfigs

		callbackPort := port
		redirectURI := fmt.Sprintf("http://localhost:%d", callbackPort)

		// Build Google OAuth2 authorization URL manually (no external dependency).
		authURL := buildGoogleAuthURL(clientID, redirectURI)

		// Start the callback server in background.
		go runOAuthCallbackServer(providerType, clientID, clientSecret, redirectURI, callbackPort, providerDir, saveToConfigs, cfg, poolManager)

		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"authUrl": authURL,
			"authInfo": gin.H{
				"provider":     providerType,
				"redirectUri":  redirectURI,
				"callbackPort": callbackPort,
			},
		})
	}
}

// buildGoogleAuthURL constructs a Google OAuth2 authorization URL.
func buildGoogleAuthURL(clientID, redirectURI string) string {
	params := url.Values{}
	params.Set("client_id", clientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("response_type", "code")
	params.Set("scope", "https://www.googleapis.com/auth/cloud-platform")
	params.Set("access_type", "offline")
	params.Set("prompt", "select_account")
	return "https://accounts.google.com/o/oauth2/v2/auth?" + params.Encode()
}

// runOAuthCallbackServer starts a one-shot HTTP server on callbackPort that
// receives the OAuth code, exchanges it for tokens, saves the credential file,
// then broadcasts an oauth_success SSE event and shuts down.
func runOAuthCallbackServer(
	providerType, clientID, clientSecret, redirectURI string,
	port int, providerDir string, saveToConfigs bool,
	cfg *config.Config, poolManager *providers.ProviderPoolManager,
) {
	mux := http.NewServeMux()
	srv := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", port),
		Handler: mux,
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		errParam := r.URL.Query().Get("error")

		if errParam != "" {
			log.Printf("[OAuth] %s: authorization error from Google: %s", providerType, errParam)
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, "<h1>Authorization Failed</h1><p>%s</p>", errParam)
			go srv.Close()
			return
		}

		if code == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		log.Printf("[OAuth] %s: received callback, exchanging code for tokens", providerType)

		tokens, err := exchangeOAuthCode(clientID, clientSecret, code, redirectURI)
		if err != nil {
			log.Printf("[OAuth] %s: token exchange failed: %v", providerType, err)
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, "<h1>Token Exchange Failed</h1><p>%v</p>", err)
			go srv.Close()
			return
		}

		// Save tokens to disk.
		var credPath string
		if saveToConfigs {
			dir := filepath.Join("configs", providerDir)
			_ = os.MkdirAll(dir, 0o755)
			credPath = filepath.Join(dir, fmt.Sprintf("%d_oauth_creds.json", time.Now().UnixMilli()))
		} else {
			home, _ := os.UserHomeDir()
			dir := filepath.Join(home, "."+providerDir)
			_ = os.MkdirAll(dir, 0o755)
			credPath = filepath.Join(dir, "oauth_creds.json")
		}

		data, _ := json.MarshalIndent(tokens, "", "  ")
		if writeErr := os.WriteFile(credPath, data, 0o600); writeErr != nil {
			log.Printf("[OAuth] %s: failed to write credentials: %v", providerType, writeErr)
		} else {
			log.Printf("[OAuth] %s: credentials saved to %s", providerType, credPath)
		}

		// Broadcast success event to UI.
		payload, _ := json.Marshal(map[string]string{
			"provider":     providerType,
			"credPath":     credPath,
			"relativePath": credPath,
			"timestamp":    time.Now().UTC().Format(time.RFC3339),
		})
		BroadcastEvent("oauth_success", string(payload))

		// Auto-link the new credential file into provider pools.
		autoLinkProviderConfigs(cfg, poolManager)

		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<h1>Authorization Successful!</h1><p>You can close this window.</p>")
		go srv.Close()
	})

	log.Printf("[OAuth] %s: starting callback server on port %d", providerType, port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("[OAuth] %s: callback server error: %v", providerType, err)
	}
}

// exchangeOAuthCode exchanges an OAuth authorization code for tokens via
// the Google token endpoint (no external library needed).
func exchangeOAuthCode(clientID, clientSecret, code, redirectURI string) (map[string]any, error) {
	form := url.Values{}
	form.Set("code", code)
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("redirect_uri", redirectURI)
	form.Set("grant_type", "authorization_code")

	resp, err := http.PostForm("https://oauth2.googleapis.com/token", form)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if errVal, ok := result["error"]; ok {
		return nil, fmt.Errorf("token endpoint error: %v", errVal)
	}
	return result, nil
}

// oauthCallbackPortInUse checks whether a port is already in use.
func oauthCallbackPortInUse(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return true
	}
	ln.Close()
	return false
}

// bufio and net are used above; ensure scanner is available for the log hook.
var _ = bufio.NewScanner

// ---------------------------------------------------------------------------
// UUID generation helper
// ---------------------------------------------------------------------------

// generateUUID generates a random UUID v4 string.
func generateUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// Set version 4 and variant bits.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%12x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
