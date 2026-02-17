// Package router configures the Gin engine with all routes for the AI API proxy.
//
// Route groups:
//
//   - Static files (no auth): /, /index.html, /login.html, /dashboard.html, /static/*, etc.
//   - UI API (own session-token auth): /api/login, /api/logout, /api/health, /api/events,
//     /api/config, /api/system, /api/providers/*, /api/provider-models/*, etc.
//   - Unauthenticated AI routes: /health, /provider_health, GET /api/version
//   - Authenticated (RequiredAPIKey guard):
//   - OpenAI-compatible : /v1/chat/completions, /v1/models, /v1/responses
//   - Claude-compatible : /v1/messages, /v1/messages/count_tokens
//   - Gemini-compatible : /v1beta/models, /v1beta/models/:model/:action
//   - Ollama-compatible : /api/tags, /api/chat, /api/generate, /api/show
//   - Provider-prefixed : /<provider>/v1/chat/completions, etc.
package router

import (
	"net/http"
	"path/filepath"

	"github.com/gin-gonic/gin"

	cfg "ai-cli-proxy-go/config"
	"ai-cli-proxy-go/handlers"
	"ai-cli-proxy-go/middleware"
	"ai-cli-proxy-go/providers"
)

// SetupRouter constructs and returns a Gin engine with every route registered.
// config, poolManager, and staticDir are threaded through to the handlers that
// need them. staticDir must be an absolute path to the static/ directory.
func SetupRouter(config *cfg.Config, poolManager *providers.ProviderPoolManager, staticDir string) *gin.Engine {
	// Use gin.New() instead of gin.Default() so we control middleware order.
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(middleware.Logger())
	r.Use(middleware.CORS())

	// -------------------------------------------------------------------------
	// Static files — served from staticDir (absolute path resolved at startup).
	// -------------------------------------------------------------------------
	r.StaticFS("/static", http.Dir(staticDir))
	r.StaticFile("/favicon.ico", filepath.Join(staticDir, "favicon.ico"))
	r.StaticFile("/openapi.json", filepath.Join(staticDir, "openapi.json"))
	r.GET("/", func(c *gin.Context) { c.File(filepath.Join(staticDir, "index.html")) })
	r.GET("/index.html", func(c *gin.Context) { c.File(filepath.Join(staticDir, "index.html")) })
	r.GET("/login.html", func(c *gin.Context) { c.File(filepath.Join(staticDir, "login.html")) })
	r.GET("/dashboard.html", func(c *gin.Context) { c.File(filepath.Join(staticDir, "dashboard.html")) })
	r.GET("/dashboard", func(c *gin.Context) { c.Redirect(http.StatusMovedPermanently, "/dashboard.html") })
	r.GET("/app/*filepath", func(c *gin.Context) {
		c.File(filepath.Join(staticDir, "app", c.Param("filepath")))
	})

	// -------------------------------------------------------------------------
	// UI API routes — use their own session-token auth, not the API key.
	// -------------------------------------------------------------------------
	uiAPI := r.Group("/api")
	{
		uiAPI.POST("/login", handlers.HandleUILogin(config))
		uiAPI.POST("/logout", handlers.HandleUILogout(config))
		uiAPI.GET("/health", handlers.HandleUIHealth())
		uiAPI.GET("/events", handlers.HandleSSEEvents(config))

		// Protected UI routes (require valid UI session token).
		protected := uiAPI.Group("/")
		protected.Use(handlers.UITokenAuth())
		{
			protected.GET("/config", handlers.HandleGetConfig(config))
			protected.POST("/config", handlers.HandleUpdateConfig(config, poolManager))
			protected.GET("/system", handlers.HandleGetSystem(config))
			protected.GET("/service-mode", handlers.HandleServiceMode())

			// Metrics endpoints
			protected.GET("/metrics/overview", handlers.HandleMetricsOverview())
			protected.GET("/metrics/requests", handlers.HandleMetricsRequests())
			protected.GET("/metrics/latency", handlers.HandleMetricsLatency())
			protected.GET("/metrics/errors", handlers.HandleMetricsErrors())
			protected.GET("/metrics/tokens", handlers.HandleMetricsTokens())
			protected.GET("/metrics/providers/health-timeline", handlers.HandleMetricsProviderHealthTimeline())
			protected.GET("/metrics/providers/load", handlers.HandleMetricsProviderLoad())
			protected.GET("/cache/stats", handlers.HandleCacheStats())

			// Usage endpoints
			protected.GET("/usage", handlers.HandleGetUsage(config, poolManager))
			protected.GET("/usage/:providerType", handlers.HandleGetUsageByProvider(config, poolManager))

			// Upload-configs endpoints
			protected.GET("/upload-configs", handlers.HandleGetUploadConfigs(config))
			protected.GET("/upload-configs/view/*path", handlers.HandleViewUploadConfig())
			protected.DELETE("/upload-configs/delete/*path", handlers.HandleDeleteUploadConfig())
			protected.GET("/upload-configs/download-all", handlers.HandleDownloadAllConfigs(config))

			// Reload / quick-link
			protected.POST("/reload-config", handlers.HandleReloadConfig(config, poolManager))
			protected.POST("/quick-link-provider", handlers.HandleQuickLinkProvider(config, poolManager))

			// Providers management
			protected.GET("/providers", handlers.HandleGetProviders(config, poolManager))
			protected.GET("/providers/:providerType", handlers.HandleGetProviderType(config, poolManager))
			protected.POST("/providers", handlers.HandleAddProvider(config, poolManager))
			protected.PUT("/providers/:providerType/:uuid", handlers.HandleUpdateProvider(config, poolManager))
			protected.DELETE("/providers/:providerType/:uuid", handlers.HandleDeleteProvider(config, poolManager))
			protected.POST("/providers/:providerType/:uuid/disable", handlers.HandleDisableProvider(config, poolManager))
			protected.POST("/providers/:providerType/:uuid/enable", handlers.HandleEnableProvider(config, poolManager))
			protected.POST("/providers/:providerType/reset-health", handlers.HandleResetProviderHealth(config, poolManager))
			protected.POST("/providers/:providerType/generate-auth-url", handlers.HandleGenerateAuthURL(config, poolManager))
			protected.GET("/provider-models", handlers.HandleGetProviderModels())
			protected.GET("/provider-models/:providerType", handlers.HandleGetProviderModelsByType())
			protected.POST("/upload-oauth-credentials", handlers.HandleUploadOAuthCredentials(config, poolManager))
			protected.POST("/admin-password", handlers.HandleUpdateAdminPassword())
		}
	}

	// -------------------------------------------------------------------------
	// Health endpoints — no authentication required.
	// -------------------------------------------------------------------------
	r.GET("/health", handlers.HealthHandler(config))
	r.GET("/provider_health", handlers.ProviderHealthHandler(config, poolManager))

	// -------------------------------------------------------------------------
	// Ollama version — no authentication (Ollama clients don't use auth).
	// -------------------------------------------------------------------------
	r.GET("/ollama/api/version", handlers.HandleOllamaVersion)

	// -------------------------------------------------------------------------
	// Authenticated API endpoints.
	// -------------------------------------------------------------------------
	api := r.Group("/")
	api.Use(middleware.Auth(config.RequiredAPIKey))

	// -- OpenAI-compatible endpoints ------------------------------------------
	api.GET("/v1/models", handlers.HandleOpenAIModels(config, poolManager))
	api.POST("/v1/chat/completions", handlers.HandleOpenAIChat(config, poolManager))
	api.POST("/v1/responses", handlers.HandleOpenAIResponses(config, poolManager))

	// -- Claude-compatible endpoints ------------------------------------------
	api.POST("/v1/messages", handlers.HandleClaudeMessages(config, poolManager))
	api.POST("/v1/messages/count_tokens", handlers.HandleCountTokens())

	// -- Gemini-compatible endpoints ------------------------------------------
	// Gemini uses paths like:
	//   GET  /v1beta/models
	//   POST /v1beta/models/gemini-2.5-pro:generateContent
	//   POST /v1beta/models/gemini-2.5-pro:streamGenerateContent
	//
	// We capture the ":model" wildcard which may contain the colon-action suffix
	// (e.g. "gemini-2.5-pro:generateContent").  The handler splits on ":" itself.
	api.GET("/v1beta/models", handlers.HandleGeminiModels(config, poolManager))
	api.POST("/v1beta/models/:model_action", handlers.HandleGeminiContent(config, poolManager))

	// -- Ollama-compatible endpoints (authenticated) --------------------------
	api.GET("/api/tags", handlers.HandleOllamaTags(config, poolManager))
	api.GET("/ollama/api/tags", handlers.HandleOllamaTags(config, poolManager))
	api.POST("/api/chat", handlers.HandleOllamaChat(config, poolManager))
	api.POST("/ollama/api/chat", handlers.HandleOllamaChat(config, poolManager))
	api.POST("/api/generate", handlers.HandleOllamaGenerate(config, poolManager))
	api.POST("/ollama/api/generate", handlers.HandleOllamaGenerate(config, poolManager))
	api.POST("/api/show", handlers.HandleOllamaShow)
	api.POST("/ollama/api/show", handlers.HandleOllamaShow)

	// -- Provider-prefixed routing --------------------------------------------
	// Allows clients to target a specific provider explicitly, e.g.:
	//   POST /gemini-cli-oauth/v1/chat/completions
	//   POST /claude-custom/v1/messages
	//   GET  /openai-custom/v1/models
	for _, p := range []string{
		"gemini-cli-oauth",
		"gemini-antigravity",
		"openai-custom",
		"openaiResponses-custom",
		"claude-custom",
		"claudeCode-custom",
	} {
		providerType := p // capture loop variable

		api.POST("/"+providerType+"/v1/chat/completions",
			handlers.HandleOpenAIChatWithProvider(config, poolManager, providerType))

		api.POST("/"+providerType+"/v1/messages",
			handlers.HandleClaudeMessagesWithProvider(config, poolManager, providerType))

		api.GET("/"+providerType+"/v1/models",
			handlers.HandleOpenAIModelsWithProvider(config, poolManager, providerType))
	}

	return r
}
