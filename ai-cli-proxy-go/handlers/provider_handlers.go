package handlers

import (
	"github.com/gin-gonic/gin"

	cfg "ai-cli-proxy-go/config"
	"ai-cli-proxy-go/providers"
)

// HandleOpenAIChatWithProvider is a variant of HandleOpenAIChat that routes
// the request to a specific named provider (e.g. "gemini-cli-oauth") instead
// of auto-detecting the provider from the model name.
//
// This enables URL-prefix routing such as:
//
//	POST /gemini-cli-oauth/v1/chat/completions
func HandleOpenAIChatWithProvider(config *cfg.Config, poolManager *providers.ProviderPoolManager, providerType string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Store the target provider in the Gin context so that HandleOpenAIChat
		// can pick it up via c.GetString("forced_provider").
		c.Set("forced_provider", providerType)
		HandleOpenAIChat(config, poolManager)(c)
	}
}

// HandleClaudeMessagesWithProvider is a variant of HandleClaudeMessages that
// routes the request to a specific named provider.
//
//	POST /claude-custom/v1/messages
func HandleClaudeMessagesWithProvider(config *cfg.Config, poolManager *providers.ProviderPoolManager, providerType string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("forced_provider", providerType)
		HandleClaudeMessages(config, poolManager)(c)
	}
}

// HandleOpenAIModelsWithProvider is a variant of HandleOpenAIModels that
// returns models belonging to a specific provider.
//
//	GET /gemini-cli-oauth/v1/models
func HandleOpenAIModelsWithProvider(config *cfg.Config, poolManager *providers.ProviderPoolManager, providerType string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("forced_provider", providerType)
		HandleOpenAIModels(config, poolManager)(c)
	}
}
