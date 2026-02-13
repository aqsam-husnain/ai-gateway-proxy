// Package config provides configuration loading and management for the AI CLI proxy.
// It mirrors the Node.js config-manager.js behaviour: JSON file first, then CLI args,
// then env-var overrides, with sensible defaults throughout.
package config

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

// AllModelProviders is the canonical list of recognised provider identifiers.
var AllModelProviders = []string{
	"gemini-cli-oauth",
	"gemini-antigravity",
	"openai-custom",
	"openaiResponses-custom",
	"claude-custom",
	"claudeCode-custom",
}

// Config holds the full application configuration.
type Config struct {
	RequiredAPIKey              string                    `json:"REQUIRED_API_KEY"`
	ServerPort                  int                       `json:"SERVER_PORT"`
	Host                        string                    `json:"HOST"`
	ModelProvider               string                    `json:"MODEL_PROVIDER"`
	ProjectID                   string                    `json:"PROJECT_ID"`
	GeminiOAuthCredsBase64      string                    `json:"GEMINI_OAUTH_CREDS_BASE64"`
	GeminiOAuthCredsFilePath    string                    `json:"GEMINI_OAUTH_CREDS_FILE_PATH"`
	GeminiBaseURL               string                    `json:"GEMINI_BASE_URL"`
	OpenAIAPIKey                string                    `json:"OPENAI_API_KEY"`
	OpenAIBaseURL               string                    `json:"OPENAI_BASE_URL"`
	ClaudeAPIKey                string                    `json:"CLAUDE_API_KEY"`
	ClaudeBaseURL               string                    `json:"CLAUDE_BASE_URL"`
	SystemPromptFilePath        string                    `json:"SYSTEM_PROMPT_FILE_PATH"`
	SystemPromptMode            string                    `json:"SYSTEM_PROMPT_MODE"`
	SystemPromptContent         string                    `json:"-"`
	PromptLogBaseName           string                    `json:"PROMPT_LOG_BASE_NAME"`
	PromptLogMode               string                    `json:"PROMPT_LOG_MODE"`
	RequestMaxRetries           int                       `json:"REQUEST_MAX_RETRIES"`
	RequestBaseDelay            int                       `json:"REQUEST_BASE_DELAY"`
	CronNearMinutes             int                       `json:"CRON_NEAR_MINUTES"`
	CronRefreshToken            bool                      `json:"CRON_REFRESH_TOKEN"`
	ProviderPoolsFilePath       string                    `json:"PROVIDER_POOLS_FILE_PATH"`
	MaxErrorCount               int                       `json:"MAX_ERROR_COUNT"`
	HealthCheckIntervalHours    float64                   `json:"HEALTH_CHECK_INTERVAL_HOURS"`
	QuickRetryIntervalSeconds   int                       `json:"QUICK_RETRY_INTERVAL_SECONDS"`
	QuickRetryMaxCount          int                       `json:"QUICK_RETRY_MAX_COUNT"`
	RateLimitCheckIntervalHours float64                   `json:"RATE_LIMIT_CHECK_INTERVAL_HOURS"`
	StandardCheckIntervalHours  float64                   `json:"STANDARD_CHECK_INTERVAL_HOURS"`
	AutoHealthCheckEnabled      bool                      `json:"AUTO_HEALTH_CHECK_ENABLED"`
	ProviderFallbackChain       map[string][]string        `json:"providerFallbackChain"`
	ProviderPools               map[string][]ProviderConfig `json:"providerPools,omitempty"`
	DefaultModelProviders       []string                  `json:"-"`
	RedisURL                    string                    `json:"REDIS_URL"`
	PostgresDSN                 string                    `json:"POSTGRES_DSN"`
}

// ProviderConfig holds the configuration for a single provider in a pool.
type ProviderConfig struct {
	UUID                          string  `json:"uuid"`
	IsHealthy                     bool    `json:"isHealthy"`
	IsDisabled                    bool    `json:"isDisabled"`
	CustomName                    string  `json:"customName,omitempty"`
	UsageCount                    int     `json:"usageCount"`
	ErrorCount                    int     `json:"errorCount"`
	LastUsed                      *string `json:"lastUsed"`
	LastErrorTime                 *string `json:"lastErrorTime"`
	LastErrorMessage              string  `json:"lastErrorMessage,omitempty"`
	CheckModel                    string   `json:"checkModelName,omitempty"`
	CheckHealth                   bool     `json:"checkHealth,omitempty"`
	NotSupportedModels            []string `json:"notSupportedModels,omitempty"`
	LastHealthCheckTime           *string  `json:"lastHealthCheckTime,omitempty"`
	// Gemini fields
	GeminiOAuthCredsFilePath      string  `json:"GEMINI_OAUTH_CREDS_FILE_PATH,omitempty"`
	ProjectID                     string  `json:"PROJECT_ID,omitempty"`
	GeminiBaseURL                 string  `json:"GEMINI_BASE_URL,omitempty"`
	// Antigravity fields
	AntigravityOAuthCredsFilePath string  `json:"ANTIGRAVITY_OAUTH_CREDS_FILE_PATH,omitempty"`
	AntigravityBaseURLDaily       string  `json:"ANTIGRAVITY_BASE_URL_DAILY,omitempty"`
	AntigravityBaseURLAutopush    string  `json:"ANTIGRAVITY_BASE_URL_AUTOPUSH,omitempty"`
	// OpenAI fields
	OpenAIAPIKey                  string  `json:"OPENAI_API_KEY,omitempty"`
	OpenAIBaseURL                 string  `json:"OPENAI_BASE_URL,omitempty"`
	// Claude fields
	ClaudeAPIKey                  string  `json:"CLAUDE_API_KEY,omitempty"`
	ClaudeBaseURL                 string  `json:"CLAUDE_BASE_URL,omitempty"`
}

// defaults returns a Config populated with the same defaults as the JS original.
func defaults() *Config {
	return &Config{
		RequiredAPIKey:              "123456",
		ServerPort:                  3000,
		Host:                        "0.0.0.0",
		ModelProvider:               "gemini-cli-oauth",
		GeminiBaseURL:               "https://cloudcode-pa.googleapis.com",
		SystemPromptFilePath:        "configs/input_system_prompt.txt",
		SystemPromptMode:            "append",
		PromptLogBaseName:           "prompt_log",
		PromptLogMode:               "none",
		RequestMaxRetries:           3,
		RequestBaseDelay:            1000,
		CronNearMinutes:             15,
		CronRefreshToken:            true,
		ProviderPoolsFilePath:       "configs/provider_pools.json",
		MaxErrorCount:               3,
		HealthCheckIntervalHours:    1.0,
		QuickRetryIntervalSeconds:   10,
		QuickRetryMaxCount:          3,
		RateLimitCheckIntervalHours: 3.0,
		StandardCheckIntervalHours:  3.0,
		AutoHealthCheckEnabled:      true,
		ProviderFallbackChain:       map[string][]string{},
		ProviderPools:               map[string][]ProviderConfig{},
	}
}

// LoadConfig loads configuration from the JSON file, then applies CLI args.
// args should be the raw CLI args slice (e.g. os.Args[1:]).
func LoadConfig(configFilePath string, args []string) (*Config, error) {
	cfg := defaults()

	// --- 1. Load from JSON file ---
	if configFilePath == "" {
		configFilePath = "configs/config.json"
	}
	if data, err := os.ReadFile(configFilePath); err == nil {
		// Unmarshal into cfg directly; unknown fields are silently ignored.
		if jsonErr := json.Unmarshal(data, cfg); jsonErr != nil {
			log.Printf("[Config Warning] Could not parse %s: %v — using defaults", configFilePath, jsonErr)
		} else {
			log.Printf("[Config] Loaded configuration from %s", configFilePath)
		}
	} else {
		log.Printf("[Config Warning] Could not read %s: %v — using defaults", configFilePath, err)
	}

	// --- 2. Apply environment variable overrides ---
	applyEnvOverrides(cfg)

	// --- 3. Apply CLI argument overrides ---
	applyArgs(cfg, args)

	// --- 4. Normalise provider list ---
	cfg.NormalizeProviders()

	// --- 5. Resolve system-prompt content ---
	if cfg.SystemPromptFilePath == "" {
		cfg.SystemPromptFilePath = "configs/input_system_prompt.txt"
	}
	cfg.SystemPromptContent = GetSystemPromptContent(cfg.SystemPromptFilePath)

	// --- 6. Load provider pools ---
	if cfg.ProviderPoolsFilePath == "" {
		cfg.ProviderPoolsFilePath = "configs/provider_pools.json"
	}
	loadProviderPools(cfg)

	return cfg, nil
}

// applyEnvOverrides reads well-known environment variables and writes them into
// cfg.  Only non-empty env values override the current setting.
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("REQUIRED_API_KEY"); v != "" {
		cfg.RequiredAPIKey = v
	}
	if v := os.Getenv("SERVER_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.ServerPort = p
		}
	}
	if v := os.Getenv("HOST"); v != "" {
		cfg.Host = v
	}
	if v := os.Getenv("MODEL_PROVIDER"); v != "" {
		cfg.ModelProvider = v
	}
	if v := os.Getenv("PROJECT_ID"); v != "" {
		cfg.ProjectID = v
	}
	if v := os.Getenv("GEMINI_OAUTH_CREDS_BASE64"); v != "" {
		cfg.GeminiOAuthCredsBase64 = v
	}
	if v := os.Getenv("GEMINI_OAUTH_CREDS_FILE_PATH"); v != "" {
		cfg.GeminiOAuthCredsFilePath = v
	}
	if v := os.Getenv("GEMINI_BASE_URL"); v != "" {
		cfg.GeminiBaseURL = v
	}
	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		cfg.OpenAIAPIKey = v
	}
	if v := os.Getenv("OPENAI_BASE_URL"); v != "" {
		cfg.OpenAIBaseURL = v
	}
	if v := os.Getenv("CLAUDE_API_KEY"); v != "" {
		cfg.ClaudeAPIKey = v
	}
	if v := os.Getenv("CLAUDE_BASE_URL"); v != "" {
		cfg.ClaudeBaseURL = v
	}
	if v := os.Getenv("SYSTEM_PROMPT_FILE_PATH"); v != "" {
		cfg.SystemPromptFilePath = v
	}
	if v := os.Getenv("SYSTEM_PROMPT_MODE"); v != "" {
		cfg.SystemPromptMode = v
	}
	if v := os.Getenv("PROMPT_LOG_BASE_NAME"); v != "" {
		cfg.PromptLogBaseName = v
	}
	if v := os.Getenv("PROMPT_LOG_MODE"); v != "" {
		cfg.PromptLogMode = v
	}
	if v := os.Getenv("PROVIDER_POOLS_FILE_PATH"); v != "" {
		cfg.ProviderPoolsFilePath = v
	}
	if v := os.Getenv("REDIS_URL"); v != "" {
		cfg.RedisURL = v
	}
	if v := os.Getenv("POSTGRES_DSN"); v != "" {
		cfg.PostgresDSN = v
	}
	if v := os.Getenv("AUTO_HEALTH_CHECK_ENABLED"); v != "" {
		cfg.AutoHealthCheckEnabled = strings.ToLower(v) == "true"
	}
	if v := os.Getenv("CRON_REFRESH_TOKEN"); v != "" {
		cfg.CronRefreshToken = strings.ToLower(v) == "true"
	}
	if v := os.Getenv("MAX_ERROR_COUNT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxErrorCount = n
		}
	}
	if v := os.Getenv("HEALTH_CHECK_INTERVAL_HOURS"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.HealthCheckIntervalHours = f
		}
	}
	if v := os.Getenv("QUICK_RETRY_INTERVAL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.QuickRetryIntervalSeconds = n
		}
	}
	if v := os.Getenv("QUICK_RETRY_MAX_COUNT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.QuickRetryMaxCount = n
		}
	}
	if v := os.Getenv("RATE_LIMIT_CHECK_INTERVAL_HOURS"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.RateLimitCheckIntervalHours = f
		}
	}
	if v := os.Getenv("STANDARD_CHECK_INTERVAL_HOURS"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.StandardCheckIntervalHours = f
		}
	}
	if v := os.Getenv("REQUEST_MAX_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.RequestMaxRetries = n
		}
	}
	if v := os.Getenv("REQUEST_BASE_DELAY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.RequestBaseDelay = n
		}
	}
	if v := os.Getenv("CRON_NEAR_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.CronNearMinutes = n
		}
	}
}

// applyArgs parses the CLI args slice (key-value pairs separated by spaces, e.g.
// --port 3000) and overwrites the relevant config fields.
// Unknown flags are silently skipped, matching the JS behaviour.
func applyArgs(cfg *Config, args []string) {
	for i := 0; i < len(args); i++ {
		flag := args[i]
		// Helper to safely read the next argument.
		next := func() (string, bool) {
			if i+1 < len(args) {
				i++
				return args[i], true
			}
			log.Printf("[Config Warning] %s flag requires a value.", flag)
			return "", false
		}

		switch flag {
		case "--api-key":
			if v, ok := next(); ok {
				cfg.RequiredAPIKey = v
			}
		case "--port":
			if v, ok := next(); ok {
				if p, err := strconv.Atoi(v); err == nil {
					cfg.ServerPort = p
				} else {
					log.Printf("[Config Warning] Invalid value for --port: %s", v)
				}
			}
		case "--host":
			if v, ok := next(); ok {
				cfg.Host = v
			}
		case "--model-provider":
			if v, ok := next(); ok {
				cfg.ModelProvider = v
			}
		case "--openai-api-key":
			if v, ok := next(); ok {
				cfg.OpenAIAPIKey = v
			}
		case "--openai-base-url":
			if v, ok := next(); ok {
				cfg.OpenAIBaseURL = v
			}
		case "--claude-api-key":
			if v, ok := next(); ok {
				cfg.ClaudeAPIKey = v
			}
		case "--claude-base-url":
			if v, ok := next(); ok {
				cfg.ClaudeBaseURL = v
			}
		case "--gemini-base-url":
			if v, ok := next(); ok {
				cfg.GeminiBaseURL = v
			}
		case "--gemini-oauth-creds-base64":
			if v, ok := next(); ok {
				cfg.GeminiOAuthCredsBase64 = v
			}
		case "--gemini-oauth-creds-file":
			if v, ok := next(); ok {
				cfg.GeminiOAuthCredsFilePath = v
			}
		case "--project-id":
			if v, ok := next(); ok {
				cfg.ProjectID = v
			}
		case "--system-prompt-file":
			if v, ok := next(); ok {
				cfg.SystemPromptFilePath = v
			}
		case "--system-prompt-mode":
			if v, ok := next(); ok {
				if v == "overwrite" || v == "append" {
					cfg.SystemPromptMode = v
				} else {
					log.Printf("[Config Warning] Invalid mode for --system-prompt-mode: %q. Expected 'overwrite' or 'append'.", v)
				}
			}
		case "--log-prompts":
			if v, ok := next(); ok {
				if v == "console" || v == "file" || v == "none" {
					cfg.PromptLogMode = v
				} else {
					log.Printf("[Config Warning] Invalid mode for --log-prompts: %q. Expected 'console', 'file', or 'none'.", v)
				}
			}
		case "--prompt-log-base-name":
			if v, ok := next(); ok {
				cfg.PromptLogBaseName = v
			}
		case "--cron-near-minutes":
			if v, ok := next(); ok {
				if n, err := strconv.Atoi(v); err == nil {
					cfg.CronNearMinutes = n
				} else {
					log.Printf("[Config Warning] Invalid value for --cron-near-minutes: %s", v)
				}
			}
		case "--cron-refresh-token":
			if v, ok := next(); ok {
				cfg.CronRefreshToken = strings.ToLower(v) == "true"
			}
		case "--provider-pools-file":
			if v, ok := next(); ok {
				cfg.ProviderPoolsFilePath = v
			}
		case "--max-error-count":
			if v, ok := next(); ok {
				if n, err := strconv.Atoi(v); err == nil {
					cfg.MaxErrorCount = n
				} else {
					log.Printf("[Config Warning] Invalid value for --max-error-count: %s", v)
				}
			}
		case "--health-check-interval-hours":
			if v, ok := next(); ok {
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					cfg.HealthCheckIntervalHours = f
				} else {
					log.Printf("[Config Warning] Invalid value for --health-check-interval-hours: %s", v)
				}
			}
		case "--quick-retry-interval-seconds":
			if v, ok := next(); ok {
				if n, err := strconv.Atoi(v); err == nil {
					cfg.QuickRetryIntervalSeconds = n
				} else {
					log.Printf("[Config Warning] Invalid value for --quick-retry-interval-seconds: %s", v)
				}
			}
		case "--quick-retry-max-count":
			if v, ok := next(); ok {
				if n, err := strconv.Atoi(v); err == nil {
					cfg.QuickRetryMaxCount = n
				} else {
					log.Printf("[Config Warning] Invalid value for --quick-retry-max-count: %s", v)
				}
			}
		case "--rate-limit-check-interval-hours":
			if v, ok := next(); ok {
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					cfg.RateLimitCheckIntervalHours = f
				} else {
					log.Printf("[Config Warning] Invalid value for --rate-limit-check-interval-hours: %s", v)
				}
			}
		case "--standard-check-interval-hours":
			if v, ok := next(); ok {
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					cfg.StandardCheckIntervalHours = f
				} else {
					log.Printf("[Config Warning] Invalid value for --standard-check-interval-hours: %s", v)
				}
			}
		case "--auto-health-check-enabled":
			if v, ok := next(); ok {
				cfg.AutoHealthCheckEnabled = strings.ToLower(v) == "true"
			}
		case "--request-max-retries":
			if v, ok := next(); ok {
				if n, err := strconv.Atoi(v); err == nil {
					cfg.RequestMaxRetries = n
				} else {
					log.Printf("[Config Warning] Invalid value for --request-max-retries: %s", v)
				}
			}
		case "--request-base-delay":
			if v, ok := next(); ok {
				if n, err := strconv.Atoi(v); err == nil {
					cfg.RequestBaseDelay = n
				} else {
					log.Printf("[Config Warning] Invalid value for --request-base-delay: %s", v)
				}
			}
		// --antigravity-* flags kept for completeness
		case "--antigravity-base-url-daily", "--antigravity-base-url-autopush":
			// Consumed but not stored at the global config level; they belong to pool entries.
			next() //nolint:errcheck
		default:
			// Unknown flags are ignored.
		}
	}
}

// NormalizeProviders deduplicates and validates the MODEL_PROVIDER field,
// populating DefaultModelProviders.  Falls back to "gemini-cli-oauth" if
// nothing valid is supplied.
func (cfg *Config) NormalizeProviders() {
	const fallback = "gemini-cli-oauth"
	seen := make(map[string]bool)
	var result []string

	addProvider := func(raw string) {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return
		}
		// Case-insensitive match against the canonical list.
		var matched string
		for _, known := range AllModelProviders {
			if strings.EqualFold(known, trimmed) {
				matched = known
				break
			}
		}
		if matched == "" {
			log.Printf("[Config Warning] Unknown model provider %q — ignored.", trimmed)
			return
		}
		if !seen[matched] {
			seen[matched] = true
			result = append(result, matched)
		}
	}

	// MODEL_PROVIDER may be a comma-separated list (passed as a single string
	// after JSON unmarshalling or CLI).
	for _, part := range strings.Split(cfg.ModelProvider, ",") {
		addProvider(part)
	}

	if len(result) == 0 {
		result = []string{fallback}
	}

	cfg.DefaultModelProviders = result
	cfg.ModelProvider = result[0]
}

// GetSystemPromptContent reads the system prompt file and returns its content.
// Returns an empty string (rather than an error) if the file is absent or empty,
// mirroring the JS behaviour.
func GetSystemPromptContent(filePath string) string {
	if filePath == "" {
		return ""
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("[System Prompt] Specified system prompt file not found: %s", filePath)
		} else {
			log.Printf("[System Prompt] Error reading system prompt file %s: %v", filePath, err)
		}
		return ""
	}
	content := string(data)
	if strings.TrimSpace(content) == "" {
		return ""
	}
	log.Printf("[System Prompt] Loaded system prompt from %s", filePath)
	return content
}

// loadProviderPools reads the provider pools JSON file into cfg.ProviderPools.
func loadProviderPools(cfg *Config) {
	if cfg.ProviderPoolsFilePath == "" {
		cfg.ProviderPools = map[string][]ProviderConfig{}
		return
	}
	data, err := os.ReadFile(cfg.ProviderPoolsFilePath)
	if err != nil {
		log.Printf("[Config Warning] Could not read provider pools file %s: %v", cfg.ProviderPoolsFilePath, err)
		cfg.ProviderPools = map[string][]ProviderConfig{}
		return
	}
	var pools map[string][]ProviderConfig
	if jsonErr := json.Unmarshal(data, &pools); jsonErr != nil {
		log.Printf("[Config Error] Failed to parse provider pools %s: %v", cfg.ProviderPoolsFilePath, jsonErr)
		cfg.ProviderPools = map[string][]ProviderConfig{}
		return
	}
	cfg.ProviderPools = pools
	log.Printf("[Config] Loaded provider pools from %s", cfg.ProviderPoolsFilePath)
}

// Addr returns the listen address string suitable for net.Listen / http.ListenAndServe.
func (cfg *Config) Addr() string {
	return fmt.Sprintf("%s:%d", cfg.Host, cfg.ServerPort)
}
