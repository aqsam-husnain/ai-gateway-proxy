// Package handlers provides HTTP handler functions for the AI CLI proxy.
// This file implements the core AI endpoint handlers: OpenAI, Claude, Gemini,
// and model-list endpoints.
package handlers

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	cfg "ai-cli-proxy-go/config"
	"ai-cli-proxy-go/converters"
	"ai-cli-proxy-go/providers"
)

// ============================================================================
// Models list helpers
// ============================================================================

// buildOpenAIModelList returns the merged list of models from every healthy
// provider in the pool, formatted as an OpenAI model-list response.
func buildOpenAIModelList(config *cfg.Config, poolManager *providers.ProviderPoolManager) map[string]interface{} {
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}

	var data []interface{}
	seen := make(map[string]bool)
	now := time.Now().Unix()

	addModels := func(providerType string, modelNames []string) {
		for _, m := range modelNames {
			if seen[m] {
				continue
			}
			seen[m] = true
			data = append(data, map[string]interface{}{
				"id":       m,
				"object":   "model",
				"created":  now,
				"owned_by": providerType,
			})
		}
	}

	if poolManager != nil {
		poolManager.ForEachHealthyProvider(func(providerType string, providerCfg *providers.ProviderConfig) {
			adapter, err := poolManager.GetAdapterForProvider(providerType, providerCfg)
			if err != nil {
				log.Printf("[api_handler] Could not get adapter for %s: %v", providerType, err)
				// Fall back to static list.
				addModels(providerType, providers.GetProviderModels(providerType))
				return
			}
			list, err := adapter.ListModels()
			if err != nil {
				log.Printf("[api_handler] ListModels error for %s: %v", providerType, err)
				addModels(providerType, providers.GetProviderModels(providerType))
				return
			}
			names := extractModelNames(list)
			if len(names) == 0 {
				names = providers.GetProviderModels(providerType)
			}
			addModels(providerType, names)
		})
	}

	// Fallback to default providers when pool is empty.
	if len(data) == 0 {
		for _, pt := range config.DefaultModelProviders {
			addModels(pt, providers.GetProviderModels(pt))
		}
	}

	return map[string]interface{}{
		"object": "list",
		"data":   data,
	}
}

// buildGeminiModelList returns a Gemini-format model list response.
func buildGeminiModelList(config *cfg.Config, poolManager *providers.ProviderPoolManager) map[string]interface{} {
	openai := buildOpenAIModelList(config, poolManager)
	data, _ := openai["data"].([]interface{})

	var models []interface{}
	for _, raw := range data {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		id := ""
		if v, ok := m["id"].(string); ok {
			id = v
		}
		models = append(models, map[string]interface{}{
			"name":        "models/" + id,
			"version":     "1.0.0",
			"displayName": id,
			"description": fmt.Sprintf("A generative model. ID: %s", id),
			"inputTokenLimit":  1000000,
			"outputTokenLimit": 65536,
			"supportedGenerationMethods": []interface{}{
				"generateContent",
				"streamGenerateContent",
			},
		})
	}

	return map[string]interface{}{
		"models": models,
	}
}

// ============================================================================
// Service selection helper
// ============================================================================

// providerSelection bundles what getServiceForRequest returns.
type providerSelection struct {
	adapter      providers.ServiceAdapter
	providerType string // e.g. "gemini-cli-oauth"
	protocol     string // e.g. "gemini"
	uuid         string
	providerCfg  *providers.ProviderConfig
}

// getServiceForRequest selects a healthy provider for the request using the
// pool manager.  It falls back to the configured defaults when no pool is
// available.
func getServiceForRequest(
	config *cfg.Config,
	poolManager *providers.ProviderPoolManager,
	model string,
) (*providerSelection, error) {
	if poolManager == nil {
		// No pool — use static config.
		pt := config.ModelProvider
		return &providerSelection{
			providerType: pt,
			protocol:     providers.GetProtocolPrefix(pt),
		}, nil
	}

	// Walk the configured primary providers (and their fallback chains).
	for _, primaryProviderType := range config.DefaultModelProviders {
		pc, actualType, _ := poolManager.SelectProviderWithFallback(primaryProviderType, model)
		if pc == nil {
			continue
		}
		adapter, err := poolManager.GetAdapterForProvider(actualType, pc)
		if err != nil {
			log.Printf("[api_handler] GetAdapterForProvider(%s, %s) error: %v", actualType, pc.UUID, err)
			poolManager.MarkProviderUnhealthy(actualType, pc.UUID, err.Error(), false)
			continue
		}
		return &providerSelection{
			adapter:      adapter,
			providerType: actualType,
			protocol:     providers.GetProtocolPrefix(actualType),
			uuid:         pc.UUID,
			providerCfg:  pc,
		}, nil
	}

	return nil, fmt.Errorf("no healthy provider available")
}

// ============================================================================
// Retry / backoff helper
// ============================================================================

// backoffDelay returns an exponential back-off duration.
// attempt is 0-based (first retry = attempt 0).
func backoffDelay(config *cfg.Config, attempt int) time.Duration {
	base := float64(config.RequestBaseDelay) // milliseconds
	delay := base * math.Pow(2, float64(attempt))
	// Cap at 30 seconds.
	if delay > 30000 {
		delay = 30000
	}
	return time.Duration(delay) * time.Millisecond
}

// ============================================================================
// Streaming SSE writer helper
// ============================================================================

// writeSSEChunks reads chunks from the upstream streaming channel, converts
// them from backendProtocol to clientProtocol, and writes them to the Gin
// response as Server-Sent Events.
func writeSSEChunks(
	c *gin.Context,
	ch <-chan providers.StreamChunk,
	backendProtocol, clientProtocol, model string,
) {
	flusher, canFlush := c.Writer.(http.Flusher)

	for chunk := range ch {
		if chunk.Error != nil {
			errLine := converters.CreateStreamErrorResponse(502, chunk.Error.Error(), clientProtocol)
			fmt.Fprintf(c.Writer, "%s\n\n", errLine)
			if canFlush {
				flusher.Flush()
			}
			return
		}
		if chunk.Done {
			fmt.Fprintf(c.Writer, "data: [DONE]\n\n")
			if canFlush {
				flusher.Flush()
			}
			return
		}

		lines := converters.ConvertStreamChunk(chunk.Data, backendProtocol, clientProtocol, model)
		for _, line := range lines {
			fmt.Fprintf(c.Writer, "%s\n\n", line)
		}
		if canFlush {
			flusher.Flush()
		}
	}
	// Channel closed without a Done chunk — write the DONE sentinel.
	fmt.Fprintf(c.Writer, "data: [DONE]\n\n")
	if canFlush {
		flusher.Flush()
	}
}

// ============================================================================
// Non-stream request executor (with retry)
// ============================================================================

// executeWithRetry attempts to call the backend up to maxRetries times.
// On each failure it marks the provider unhealthy and picks a new one.
// It returns the raw backend response and the providerSelection used.
func executeWithRetry(
	config *cfg.Config,
	poolManager *providers.ProviderPoolManager,
	model string,
	backendReq map[string]interface{},
) (map[string]interface{}, *providerSelection, error) {
	maxRetries := config.RequestMaxRetries
	if maxRetries < 1 {
		maxRetries = 1
	}

	excludeUUIDs := make(map[string]bool)
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(backoffDelay(config, attempt-1))
		}

		sel, err := getServiceForRequest(config, poolManager, model)
		if err != nil {
			lastErr = err
			log.Printf("[api_handler] Attempt %d/%d: no provider available: %v", attempt+1, maxRetries, err)
			break
		}
		if excludeUUIDs[sel.uuid] {
			log.Printf("[api_handler] Attempt %d/%d: skipping already-failed uuid=%s", attempt+1, maxRetries, sel.uuid)
			continue
		}

		if sel.adapter == nil {
			lastErr = fmt.Errorf("no adapter available")
			break
		}

		resp, err := sel.adapter.GenerateContent(model, backendReq)
		if err != nil {
			lastErr = err
			log.Printf("[api_handler] Attempt %d/%d provider=%s uuid=%s error: %v",
				attempt+1, maxRetries, sel.providerType, sel.uuid, err)
			if poolManager != nil && sel.uuid != "" {
				isRateLimit := strings.Contains(err.Error(), "429") || strings.Contains(strings.ToLower(err.Error()), "rate limit")
				poolManager.MarkProviderUnhealthy(sel.providerType, sel.uuid, err.Error(), isRateLimit)
			}
			if sel.uuid != "" {
				excludeUUIDs[sel.uuid] = true
			}
			continue
		}
		return resp, sel, nil
	}

	return nil, nil, fmt.Errorf("all %d attempt(s) failed; last error: %w", maxRetries, lastErr)
}

// executeStreamWithRetry attempts to start a streaming call up to maxRetries times.
// Returns the channel, the selection used, and any error.
func executeStreamWithRetry(
	config *cfg.Config,
	poolManager *providers.ProviderPoolManager,
	model string,
	backendReq map[string]interface{},
) (<-chan providers.StreamChunk, *providerSelection, error) {
	maxRetries := config.RequestMaxRetries
	if maxRetries < 1 {
		maxRetries = 1
	}

	excludeUUIDs := make(map[string]bool)
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(backoffDelay(config, attempt-1))
		}

		sel, err := getServiceForRequest(config, poolManager, model)
		if err != nil {
			lastErr = err
			break
		}
		if excludeUUIDs[sel.uuid] {
			continue
		}
		if sel.adapter == nil {
			lastErr = fmt.Errorf("no adapter available")
			break
		}

		ch, err := sel.adapter.GenerateContentStream(model, backendReq)
		if err != nil {
			lastErr = err
			log.Printf("[api_handler] Stream attempt %d/%d provider=%s uuid=%s error: %v",
				attempt+1, maxRetries, sel.providerType, sel.uuid, err)
			if poolManager != nil && sel.uuid != "" {
				isRateLimit := strings.Contains(err.Error(), "429")
				poolManager.MarkProviderUnhealthy(sel.providerType, sel.uuid, err.Error(), isRateLimit)
			}
			if sel.uuid != "" {
				excludeUUIDs[sel.uuid] = true
			}
			continue
		}
		return ch, sel, nil
	}

	return nil, nil, fmt.Errorf("stream: all attempts failed; last error: %w", lastErr)
}

// ============================================================================
// Request body reader
// ============================================================================

// readJSONBody reads and unmarshals the request body into a
// map[string]interface{}.  Returns an error when the body is not valid JSON.
func readJSONBody(c *gin.Context) (map[string]interface{}, error) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, fmt.Errorf("reading request body: %w", err)
	}
	if len(body) == 0 {
		return map[string]interface{}{}, nil
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	return req, nil
}

// ============================================================================
// HandleOpenAIChat — POST /v1/chat/completions
// ============================================================================

// HandleOpenAIChat handles POST /v1/chat/completions.
// It accepts an OpenAI chat completion request, routes it to the appropriate
// backend (converting the format as needed), and converts the response back to
// the OpenAI format before returning it to the caller.
func HandleOpenAIChat(config *cfg.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		clientReq, err := readJSONBody(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, converters.CreateErrorResponse(400, err.Error(), "openai"))
			return
		}

		model := converters.ExtractModelFromRequest(clientReq, "openai")
		stream := converters.ExtractStreamFlag(clientReq, "openai")

		// Select provider — determines target protocol.
		sel, selErr := getServiceForRequest(config, poolManager, model)
		if selErr != nil {
			log.Printf("[HandleOpenAIChat] Provider selection failed: %v", selErr)
			c.JSON(http.StatusServiceUnavailable,
				converters.CreateErrorResponse(503, "no healthy provider available", "openai"))
			return
		}

		toProtocol := sel.protocol // e.g. "gemini", "claude", "openai"

		// Apply system prompt if configured.
		enrichedReq := clientReq
		if config.SystemPromptContent != "" {
			enrichedReq = converters.ApplySystemPrompt(clientReq, config.SystemPromptContent, config.SystemPromptMode, toProtocol)
		}

		// Convert from OpenAI to backend format.
		backendReq := converters.ConvertRequest(enrichedReq, "openai", toProtocol)

		if stream {
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("X-Accel-Buffering", "no")

			ch, streamSel, err := executeStreamWithRetry(config, poolManager, model, backendReq)
			if err != nil {
				log.Printf("[HandleOpenAIChat] Stream error: %v", err)
				errLine := converters.CreateStreamErrorResponse(502, err.Error(), "openai")
				fmt.Fprintf(c.Writer, "%s\n\n", errLine)
				return
			}
			writeSSEChunks(c, ch, streamSel.protocol, "openai", model)
			return
		}

		// Non-streaming.
		backendResp, respSel, err := executeWithRetry(config, poolManager, model, backendReq)
		if err != nil {
			log.Printf("[HandleOpenAIChat] Error: %v", err)
			c.JSON(http.StatusBadGateway,
				converters.CreateErrorResponse(502, err.Error(), "openai"))
			return
		}

		openaiResp := converters.ConvertResponse(backendResp, respSel.protocol, "openai", model)
		c.JSON(http.StatusOK, openaiResp)
	}
}

// ============================================================================
// HandleClaudeMessages — POST /v1/messages
// ============================================================================

// HandleClaudeMessages handles POST /v1/messages.
// It accepts requests in Claude messages format, routes to the appropriate
// backend, and returns the response in Claude format.
func HandleClaudeMessages(config *cfg.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		clientReq, err := readJSONBody(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, converters.CreateErrorResponse(400, err.Error(), "claude"))
			return
		}

		model := converters.ExtractModelFromRequest(clientReq, "claude")
		stream := converters.ExtractStreamFlag(clientReq, "claude")

		sel, selErr := getServiceForRequest(config, poolManager, model)
		if selErr != nil {
			log.Printf("[HandleClaudeMessages] Provider selection failed: %v", selErr)
			c.JSON(http.StatusServiceUnavailable,
				converters.CreateErrorResponse(503, "no healthy provider available", "claude"))
			return
		}

		toProtocol := sel.protocol

		// Apply system prompt.
		enrichedReq := clientReq
		if config.SystemPromptContent != "" {
			enrichedReq = converters.ApplySystemPrompt(clientReq, config.SystemPromptContent, config.SystemPromptMode, toProtocol)
		}

		// Convert from Claude to backend format.
		backendReq := converters.ConvertRequest(enrichedReq, "claude", toProtocol)

		if stream {
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("X-Accel-Buffering", "no")

			ch, streamSel, err := executeStreamWithRetry(config, poolManager, model, backendReq)
			if err != nil {
				log.Printf("[HandleClaudeMessages] Stream error: %v", err)
				errLine := converters.CreateStreamErrorResponse(502, err.Error(), "claude")
				fmt.Fprintf(c.Writer, "%s\n\n", errLine)
				return
			}
			writeSSEChunks(c, ch, streamSel.protocol, "claude", model)
			return
		}

		backendResp, respSel, err := executeWithRetry(config, poolManager, model, backendReq)
		if err != nil {
			log.Printf("[HandleClaudeMessages] Error: %v", err)
			c.JSON(http.StatusBadGateway,
				converters.CreateErrorResponse(502, err.Error(), "claude"))
			return
		}

		claudeResp := converters.ConvertResponse(backendResp, respSel.protocol, "claude", model)
		c.JSON(http.StatusOK, claudeResp)
	}
}

// ============================================================================
// HandleGeminiContent — POST /v1beta/models/:model/:action
// ============================================================================

// HandleGeminiContent handles POST /v1beta/models/:model/:action where action
// is "generateContent" or "streamGenerateContent".
// It accepts Gemini-format requests, routes to the backend, and returns a
// Gemini-format response.
func HandleGeminiContent(config *cfg.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Extract model name from URL path (Gin param).
		model := c.Param("model")
		// Strip potential URL-encoded colon-suffix (e.g. model:generateContent).
		if idx := strings.Index(model, ":"); idx != -1 {
			model = model[:idx]
		}
		action := c.Param("action")
		isStream := strings.Contains(strings.ToLower(action), "stream")

		clientReq, err := readJSONBody(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, converters.CreateErrorResponse(400, err.Error(), "gemini"))
			return
		}

		// Override/set model in body for converters that need it.
		if model != "" {
			clientReq["model"] = model
		} else {
			model = converters.ExtractModelFromRequest(clientReq, "gemini")
		}

		sel, selErr := getServiceForRequest(config, poolManager, model)
		if selErr != nil {
			log.Printf("[HandleGeminiContent] Provider selection failed: %v", selErr)
			c.JSON(http.StatusServiceUnavailable,
				converters.CreateErrorResponse(503, "no healthy provider available", "gemini"))
			return
		}

		toProtocol := sel.protocol

		// Apply system prompt.
		enrichedReq := clientReq
		if config.SystemPromptContent != "" {
			enrichedReq = converters.ApplySystemPrompt(clientReq, config.SystemPromptContent, config.SystemPromptMode, toProtocol)
		}

		// Convert from Gemini to backend format.
		backendReq := converters.ConvertRequest(enrichedReq, "gemini", toProtocol)

		if isStream {
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("X-Accel-Buffering", "no")

			ch, streamSel, err := executeStreamWithRetry(config, poolManager, model, backendReq)
			if err != nil {
				log.Printf("[HandleGeminiContent] Stream error: %v", err)
				errLine := converters.CreateStreamErrorResponse(502, err.Error(), "gemini")
				fmt.Fprintf(c.Writer, "%s\n\n", errLine)
				return
			}
			writeSSEChunks(c, ch, streamSel.protocol, "gemini", model)
			return
		}

		backendResp, respSel, err := executeWithRetry(config, poolManager, model, backendReq)
		if err != nil {
			log.Printf("[HandleGeminiContent] Error: %v", err)
			c.JSON(http.StatusBadGateway,
				converters.CreateErrorResponse(502, err.Error(), "gemini"))
			return
		}

		geminiResp := converters.ConvertResponse(backendResp, respSel.protocol, "gemini", model)
		c.JSON(http.StatusOK, geminiResp)
	}
}

// ============================================================================
// HandleOpenAIModels — GET /v1/models
// ============================================================================

// HandleOpenAIModels handles GET /v1/models.
// Returns the merged list of models from every healthy provider in OpenAI format.
func HandleOpenAIModels(config *cfg.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		modelList := buildOpenAIModelList(config, poolManager)
		c.JSON(http.StatusOK, modelList)
	}
}

// ============================================================================
// HandleGeminiModels — GET /v1beta/models
// ============================================================================

// HandleGeminiModels handles GET /v1beta/models.
// Returns models in Gemini format.
func HandleGeminiModels(config *cfg.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		modelList := buildGeminiModelList(config, poolManager)
		c.JSON(http.StatusOK, modelList)
	}
}

// ============================================================================
// HandleOpenAIResponses — POST /v1/responses
// ============================================================================

// HandleOpenAIResponses handles POST /v1/responses (OpenAI Responses API).
// The Responses API uses a different request shape (input instead of messages),
// but we normalise it to an OpenAI chat completion request internally, route
// it to the backend, and return the response in the Responses format.
func HandleOpenAIResponses(config *cfg.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		clientReq, err := readJSONBody(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, converters.CreateErrorResponse(400, err.Error(), "openai"))
			return
		}

		model := converters.ExtractModelFromRequest(clientReq, "openaiResponses")
		stream := converters.ExtractStreamFlag(clientReq, "openaiResponses")

		// Normalise OpenAI Responses API format to a standard chat completion:
		// "input" -> "messages".
		normalised := normaliseResponsesAPIRequest(clientReq)

		sel, selErr := getServiceForRequest(config, poolManager, model)
		if selErr != nil {
			log.Printf("[HandleOpenAIResponses] Provider selection failed: %v", selErr)
			c.JSON(http.StatusServiceUnavailable,
				converters.CreateErrorResponse(503, "no healthy provider available", "openai"))
			return
		}

		toProtocol := sel.protocol

		// Apply system prompt.
		enrichedReq := normalised
		if config.SystemPromptContent != "" {
			enrichedReq = converters.ApplySystemPrompt(normalised, config.SystemPromptContent, config.SystemPromptMode, toProtocol)
		}

		// Convert from OpenAI to backend format.
		backendReq := converters.ConvertRequest(enrichedReq, "openai", toProtocol)

		if stream {
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("X-Accel-Buffering", "no")

			ch, streamSel, err := executeStreamWithRetry(config, poolManager, model, backendReq)
			if err != nil {
				log.Printf("[HandleOpenAIResponses] Stream error: %v", err)
				errLine := converters.CreateStreamErrorResponse(502, err.Error(), "openai")
				fmt.Fprintf(c.Writer, "%s\n\n", errLine)
				return
			}
			writeSSEChunks(c, ch, streamSel.protocol, "openai", model)
			return
		}

		backendResp, respSel, err := executeWithRetry(config, poolManager, model, backendReq)
		if err != nil {
			log.Printf("[HandleOpenAIResponses] Error: %v", err)
			c.JSON(http.StatusBadGateway,
				converters.CreateErrorResponse(502, err.Error(), "openai"))
			return
		}

		openaiResp := converters.ConvertResponse(backendResp, respSel.protocol, "openai", model)
		// Wrap in Responses API envelope.
		responsesResp := wrapResponsesAPIResponse(openaiResp, model)
		c.JSON(http.StatusOK, responsesResp)
	}
}

// normaliseResponsesAPIRequest converts an OpenAI Responses API request body
// (which uses "input" instead of "messages") to a standard chat completion
// request body.
func normaliseResponsesAPIRequest(req map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{}, len(req))
	for k, v := range req {
		result[k] = v
	}

	// Map "input" -> "messages" when "messages" is not already present.
	if _, hasMsgs := result["messages"]; !hasMsgs {
		if input, ok := result["input"]; ok {
			switch v := input.(type) {
			case []interface{}:
				result["messages"] = v
			case string:
				result["messages"] = []interface{}{
					map[string]interface{}{
						"role":    "user",
						"content": v,
					},
				}
			}
			delete(result, "input")
		}
	}

	return result
}

// wrapResponsesAPIResponse wraps an OpenAI chat completion response in the
// OpenAI Responses API response envelope.
func wrapResponsesAPIResponse(openaiResp map[string]interface{}, model string) map[string]interface{} {
	// Extract the first choice's message content.
	textContent := ""
	var choices []interface{}
	if c, ok := openaiResp["choices"].([]interface{}); ok {
		choices = c
	}
	if len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if msg, ok := choice["message"].(map[string]interface{}); ok {
				if content, ok := msg["content"].(string); ok {
					textContent = content
				}
			}
		}
	}

	usage := map[string]interface{}{
		"input_tokens":  0,
		"output_tokens": 0,
		"total_tokens":  0,
	}
	if u, ok := openaiResp["usage"].(map[string]interface{}); ok {
		usage["input_tokens"] = u["prompt_tokens"]
		usage["output_tokens"] = u["completion_tokens"]
		usage["total_tokens"] = u["total_tokens"]
	}

	outputID := "msg_" + fmt.Sprintf("%x", time.Now().UnixNano())

	return map[string]interface{}{
		"id":         openaiResp["id"],
		"object":     "response",
		"created_at": openaiResp["created"],
		"model":      model,
		"status":     "completed",
		"output": []interface{}{
			map[string]interface{}{
				"type": "message",
				"id":   outputID,
				"role": "assistant",
				"content": []interface{}{
					map[string]interface{}{
						"type": "output_text",
						"text": textContent,
					},
				},
			},
		},
		"usage": usage,
	}
}

// ============================================================================
// SSE raw passthrough (used when backend and client share the same protocol)
// ============================================================================

// passThroughSSE is a convenience that reads raw SSE bytes from an io.Reader
// and writes them directly to the Gin response without conversion.
// Currently unused externally but available as a utility.
func passThroughSSE(c *gin.Context, r io.Reader) {
	flusher, canFlush := c.Writer.(http.Flusher)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Bytes()
		c.Writer.Write(line)
		c.Writer.Write([]byte("\n"))
		if canFlush {
			flusher.Flush()
		}
	}
}

// suppress unused import warnings for packages that are conditionally used.
var (
	_ = bytes.NewBuffer
)
