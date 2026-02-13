package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	cfg "ai-cli-proxy-go/config"
	"ai-cli-proxy-go/providers"
)

const ollamaVersion = "0.12.10"

// ─────────────────────────────────────────────
// Ollama request / response types
// ─────────────────────────────────────────────

// OllamaMessage mirrors the Ollama chat message structure (same as OpenAI).
type OllamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// OllamaChatRequest is the body sent to POST /api/chat.
type OllamaChatRequest struct {
	Model    string                 `json:"model"`
	Messages []OllamaMessage        `json:"messages"`
	Stream   bool                   `json:"stream"`
	Options  map[string]interface{} `json:"options,omitempty"`
}

// OllamaGenerateRequest is the body sent to POST /api/generate.
type OllamaGenerateRequest struct {
	Model   string                 `json:"model"`
	Prompt  string                 `json:"prompt"`
	Stream  bool                   `json:"stream"`
	Options map[string]interface{} `json:"options,omitempty"`
}

// OllamaShowRequest is the body sent to POST /api/show.
type OllamaShowRequest struct {
	Name  string `json:"name"`
	Model string `json:"model"`
}

// OllamaModelDetails contains extended model metadata.
type OllamaModelDetails struct {
	Format            string   `json:"format"`
	Family            string   `json:"family"`
	Families          []string `json:"families,omitempty"`
	ParameterSize     string   `json:"parameter_size"`
	QuantizationLevel string   `json:"quantization_level"`
}

// OllamaModel is one entry in /api/tags response.
type OllamaModel struct {
	Name       string             `json:"name"`
	Model      string             `json:"model"`
	ModifiedAt string             `json:"modified_at"`
	Size       int64              `json:"size"`
	Digest     string             `json:"digest"`
	Details    OllamaModelDetails `json:"details"`
}

// OllamaTagsResponse is the /api/tags response envelope.
type OllamaTagsResponse struct {
	Models []OllamaModel `json:"models"`
}

// OllamaShowResponse is the /api/show response.
type OllamaShowResponse struct {
	License    string                 `json:"license"`
	Modelfile  string                 `json:"modelfile"`
	Parameters string                 `json:"parameters"`
	Template   string                 `json:"template"`
	Details    OllamaModelDetails     `json:"details"`
	ModelInfo  map[string]interface{} `json:"model_info"`
}

// OllamaChatStreamChunk is one newline-delimited JSON object for chat streaming.
type OllamaChatStreamChunk struct {
	Model     string        `json:"model"`
	CreatedAt string        `json:"created_at"`
	Message   OllamaMessage `json:"message"`
	Done      bool          `json:"done"`

	// Only present on final chunk (done=true).
	DoneReason      string `json:"done_reason,omitempty"`
	TotalDuration   int64  `json:"total_duration,omitempty"`
	LoadDuration    int64  `json:"load_duration,omitempty"`
	PromptEvalCount int    `json:"prompt_eval_count,omitempty"`
	EvalCount       int    `json:"eval_count,omitempty"`
}

// OllamaGenerateStreamChunk is one newline-delimited JSON object for generate streaming.
type OllamaGenerateStreamChunk struct {
	Model     string `json:"model"`
	CreatedAt string `json:"created_at"`
	Response  string `json:"response"`
	Done      bool   `json:"done"`

	// Only present on final chunk (done=true).
	DoneReason      string `json:"done_reason,omitempty"`
	TotalDuration   int64  `json:"total_duration,omitempty"`
	LoadDuration    int64  `json:"load_duration,omitempty"`
	PromptEvalCount int    `json:"prompt_eval_count,omitempty"`
	EvalCount       int    `json:"eval_count,omitempty"`
}

// OllamaChatResponse is the non-streaming /api/chat response.
type OllamaChatResponse struct {
	Model           string        `json:"model"`
	CreatedAt       string        `json:"created_at"`
	Message         OllamaMessage `json:"message"`
	Done            bool          `json:"done"`
	DoneReason      string        `json:"done_reason"`
	TotalDuration   int64         `json:"total_duration"`
	LoadDuration    int64         `json:"load_duration"`
	PromptEvalCount int           `json:"prompt_eval_count"`
	EvalCount       int           `json:"eval_count"`
}

// OllamaGenerateResponse is the non-streaming /api/generate response.
type OllamaGenerateResponse struct {
	Model           string `json:"model"`
	CreatedAt       string `json:"created_at"`
	Response        string `json:"response"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	TotalDuration   int64  `json:"total_duration"`
	LoadDuration    int64  `json:"load_duration"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
}

// ─────────────────────────────────────────────
// Helper: model prefix utilities
// ─────────────────────────────────────────────

var modelPrefixPattern = regexp.MustCompile(`^\[.*?\]\s+`)

// addModelPrefix prepends a human-readable provider label to a model name.
// Supported providers: gemini-cli-oauth, claude-custom, openai-custom.
func addModelPrefix(name, provider string) string {
	if name == "" {
		return name
	}
	// Don't double-prefix.
	if modelPrefixPattern.MatchString(name) {
		return name
	}
	switch provider {
	case "gemini-cli-oauth", "gemini-antigravity":
		return "[Gemini CLI] " + name
	case "claude-custom", "claudeCode-custom":
		return "[Claude] " + name
	case "openai-custom", "openaiResponses-custom":
		return "[OpenAI] " + name
	default:
		return name
	}
}

// removeModelPrefix strips a leading "[...] " label from a model name.
func removeModelPrefix(name string) string {
	return modelPrefixPattern.ReplaceAllString(name, "")
}

// getProviderFromModelName infers the provider type from a model name.
// Falls back to defaultProvider when the name gives no hint.
func getProviderFromModelName(name string, defaultProvider string) string {
	clean := strings.ToLower(removeModelPrefix(name))

	switch {
	case strings.Contains(clean, "gemini"):
		return "gemini-cli-oauth"
	case strings.Contains(clean, "claude") ||
		strings.Contains(clean, "sonnet") ||
		strings.Contains(clean, "opus") ||
		strings.Contains(clean, "haiku"):
		return "claude-custom"
	case strings.Contains(clean, "gpt") ||
		strings.Contains(clean, "o1") ||
		strings.Contains(clean, "o3"):
		return "openai-custom"
	default:
		return defaultProvider
	}
}

// ollamaNow returns the current time formatted in RFC3339Nano, which Ollama uses.
func ollamaNow() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// setOllamaHeaders writes the standard Ollama response headers to c.
func setOllamaHeaders(c *gin.Context) {
	c.Header("Server", "ollama/"+ollamaVersion)
}

// ─────────────────────────────────────────────
// Handler: GET /api/version
// ─────────────────────────────────────────────

// HandleOllamaVersion handles GET /api/version (and /ollama/api/version).
// It returns the fixed Ollama version string with no authentication required.
func HandleOllamaVersion(c *gin.Context) {
	setOllamaHeaders(c)
	c.JSON(http.StatusOK, gin.H{"version": ollamaVersion})
}

// ─────────────────────────────────────────────
// Handler: GET /api/tags
// ─────────────────────────────────────────────

// HandleOllamaTags returns a combined list of models from every healthy provider
// in the pool, each model name prefixed with a human-readable provider label.
func HandleOllamaTags(config *cfg.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		setOllamaHeaders(c)

		allModels := []OllamaModel{}

		if poolManager == nil {
			c.JSON(http.StatusOK, OllamaTagsResponse{Models: allModels})
			return
		}

		now := ollamaNow()

		poolManager.ForEachHealthyProvider(func(providerType string, providerCfg *providers.ProviderConfig) {
			adapter, err := poolManager.GetAdapterForProvider(providerType, providerCfg)
			if err != nil {
				log.Printf("[Ollama Tags] Could not get adapter for %s: %v", providerType, err)
				return
			}

			modelList, err := adapter.ListModels()
			if err != nil {
				log.Printf("[Ollama Tags] listModels error for %s: %v", providerType, err)
				return
			}

			// ListModels() returns interface{}; extract model name strings from
			// whatever structure the adapter returned.
			modelNames := extractModelNames(modelList)
			for _, m := range modelNames {
				prefixedName := addModelPrefix(m, providerType)
				allModels = append(allModels, OllamaModel{
					Name:       prefixedName,
					Model:      prefixedName,
					ModifiedAt: now,
					Size:       0,
					Digest:     "",
					Details: OllamaModelDetails{
						Format:            "gguf",
						Family:            providerType,
						ParameterSize:     "unknown",
						QuantizationLevel: "Q4_0",
					},
				})
			}
		})

		c.JSON(http.StatusOK, OllamaTagsResponse{Models: allModels})
	}
}

// ─────────────────────────────────────────────
// Handler: POST /api/show
// ─────────────────────────────────────────────

// HandleOllamaShow handles POST /api/show.
// It reads the model name from the request body and returns mock model metadata.
func HandleOllamaShow(c *gin.Context) {
	setOllamaHeaders(c)

	var req OllamaShowRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	modelName := req.Name
	if modelName == "" {
		modelName = req.Model
	}
	if modelName == "" {
		modelName = "unknown"
	}

	// Determine a plausible family from the model name.
	family := "llm"
	lowerModel := strings.ToLower(modelName)
	switch {
	case strings.Contains(lowerModel, "gemini"):
		family = "gemini"
	case strings.Contains(lowerModel, "claude") ||
		strings.Contains(lowerModel, "sonnet") ||
		strings.Contains(lowerModel, "opus") ||
		strings.Contains(lowerModel, "haiku"):
		family = "claude"
	case strings.Contains(lowerModel, "gpt"):
		family = "gpt"
	}

	resp := OllamaShowResponse{
		License:    "See provider terms of service",
		Modelfile:  fmt.Sprintf("FROM %s\n", modelName),
		Parameters: "num_ctx 4096\ntemperature 1",
		Template:   "{{ if .System }}<|start_header_id|>system<|end_header_id|>\n{{ .System }}<|eot_id|>{{ end }}{{ range .Messages }}<|start_header_id|>{{ .Role }}<|end_header_id|>\n{{ .Content }}<|eot_id|>{{ end }}<|start_header_id|>assistant<|end_header_id|>\n",
		Details: OllamaModelDetails{
			Format:            "gguf",
			Family:            family,
			ParameterSize:     "unknown",
			QuantizationLevel: "Q4_0",
		},
		ModelInfo: map[string]interface{}{},
	}

	c.JSON(http.StatusOK, resp)
}

// ─────────────────────────────────────────────
// Handler: POST /api/chat
// ─────────────────────────────────────────────

// HandleOllamaChat handles POST /api/chat.
// It translates an Ollama chat request into the appropriate provider call and
// returns the response in Ollama newline-delimited JSON format.
func HandleOllamaChat(config *cfg.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		setOllamaHeaders(c)

		var req OllamaChatRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		rawModel := req.Model
		cleanModel := removeModelPrefix(rawModel)

		defaultProvider := config.ModelProvider
		if defaultProvider == "" {
			defaultProvider = "gemini-cli-oauth"
		}
		providerType := getProviderFromModelName(rawModel, defaultProvider)

		log.Printf("[Ollama Chat] model=%s provider=%s stream=%v", cleanModel, providerType, req.Stream)

		// Obtain the service adapter for the detected provider.
		adapter, err := poolManager.SelectAdapter(providerType, cleanModel)
		if err != nil {
			log.Printf("[Ollama Chat] no adapter for %s: %v", providerType, err)
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": fmt.Sprintf("no healthy provider for %s", providerType)})
			return
		}

		// Build the OpenAI-format messages (already same schema as Ollama).
		messages := make([]map[string]interface{}, len(req.Messages))
		for i, m := range req.Messages {
			messages[i] = map[string]interface{}{
				"role":    m.Role,
				"content": m.Content,
			}
		}

		openaiBody := map[string]interface{}{
			"model":    cleanModel,
			"messages": messages,
			"stream":   req.Stream,
		}
		if req.Options != nil {
			if temp, ok := req.Options["temperature"]; ok {
				openaiBody["temperature"] = temp
			}
			if maxTokens, ok := req.Options["num_predict"]; ok {
				openaiBody["max_tokens"] = maxTokens
			}
		}

		now := ollamaNow()

		if req.Stream {
			// Ollama uses newline-delimited JSON, NOT SSE.
			c.Header("Content-Type", "application/json")
			c.Header("Transfer-Encoding", "chunked")

			streamCh, err := adapter.GenerateContentStream(cleanModel, openaiBody)
			if err != nil {
				log.Printf("[Ollama Chat Stream] GenerateContentStream error: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}

			c.Stream(func(w io.Writer) bool {
				chunk, ok := <-streamCh
				if !ok {
					// Channel closed – stream ended.
					return false
				}

				if chunk.Done {
					// Final done chunk.
					final := OllamaChatStreamChunk{
						Model:      cleanModel,
						CreatedAt:  ollamaNow(),
						Message:    OllamaMessage{Role: "assistant", Content: ""},
						Done:       true,
						DoneReason: "stop",
					}
					data, _ := json.Marshal(final)
					fmt.Fprintf(w, "%s\n", data)
					return false
				}

				if chunk.Error != nil {
					log.Printf("[Ollama Chat Stream] chunk error: %v", chunk.Error)
					final := OllamaChatStreamChunk{
						Model:      cleanModel,
						CreatedAt:  ollamaNow(),
						Message:    OllamaMessage{Role: "assistant", Content: ""},
						Done:       true,
						DoneReason: "stop",
					}
					data, _ := json.Marshal(final)
					fmt.Fprintf(w, "%s\n", data)
					return false
				}

				// Parse the SSE payload as an OpenAI stream chunk.
				text := extractTextFromSSEData(chunk.Data)
				piece := OllamaChatStreamChunk{
					Model:     cleanModel,
					CreatedAt: now,
					Message:   OllamaMessage{Role: "assistant", Content: text},
					Done:      false,
				}
				data, _ := json.Marshal(piece)
				fmt.Fprintf(w, "%s\n", data)
				return true
			})
			return
		}

		// Non-streaming path.
		response, err := adapter.GenerateContent(cleanModel, openaiBody)
		if err != nil {
			log.Printf("[Ollama Chat] GenerateContent error: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		content := extractTextFromOpenAIResponse(response)

		c.JSON(http.StatusOK, OllamaChatResponse{
			Model:      cleanModel,
			CreatedAt:  now,
			Message:    OllamaMessage{Role: "assistant", Content: content},
			Done:       true,
			DoneReason: "stop",
		})
	}
}

// ─────────────────────────────────────────────
// Handler: POST /api/generate
// ─────────────────────────────────────────────

// HandleOllamaGenerate handles POST /api/generate (the legacy text-completion API).
// It wraps the prompt in a user message and delegates to the same provider flow as chat.
func HandleOllamaGenerate(config *cfg.Config, poolManager *providers.ProviderPoolManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		setOllamaHeaders(c)

		var req OllamaGenerateRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		rawModel := req.Model
		cleanModel := removeModelPrefix(rawModel)

		defaultProvider := config.ModelProvider
		if defaultProvider == "" {
			defaultProvider = "gemini-cli-oauth"
		}
		providerType := getProviderFromModelName(rawModel, defaultProvider)

		log.Printf("[Ollama Generate] model=%s provider=%s stream=%v", cleanModel, providerType, req.Stream)

		adapter, err := poolManager.SelectAdapter(providerType, cleanModel)
		if err != nil {
			log.Printf("[Ollama Generate] no adapter for %s: %v", providerType, err)
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": fmt.Sprintf("no healthy provider for %s", providerType)})
			return
		}

		// Wrap the plain text prompt as a single user message.
		messages := []map[string]interface{}{
			{"role": "user", "content": req.Prompt},
		}

		openaiBody := map[string]interface{}{
			"model":    cleanModel,
			"messages": messages,
			"stream":   req.Stream,
		}
		if req.Options != nil {
			if temp, ok := req.Options["temperature"]; ok {
				openaiBody["temperature"] = temp
			}
			if maxTokens, ok := req.Options["num_predict"]; ok {
				openaiBody["max_tokens"] = maxTokens
			}
		}

		now := ollamaNow()

		if req.Stream {
			c.Header("Content-Type", "application/json")
			c.Header("Transfer-Encoding", "chunked")

			streamCh, err := adapter.GenerateContentStream(cleanModel, openaiBody)
			if err != nil {
				log.Printf("[Ollama Generate Stream] GenerateContentStream error: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}

			c.Stream(func(w io.Writer) bool {
				chunk, ok := <-streamCh
				if !ok {
					return false
				}

				if chunk.Done {
					final := OllamaGenerateStreamChunk{
						Model:      cleanModel,
						CreatedAt:  ollamaNow(),
						Response:   "",
						Done:       true,
						DoneReason: "stop",
					}
					data, _ := json.Marshal(final)
					fmt.Fprintf(w, "%s\n", data)
					return false
				}

				if chunk.Error != nil {
					log.Printf("[Ollama Generate Stream] chunk error: %v", chunk.Error)
					final := OllamaGenerateStreamChunk{
						Model:      cleanModel,
						CreatedAt:  ollamaNow(),
						Response:   "",
						Done:       true,
						DoneReason: "stop",
					}
					data, _ := json.Marshal(final)
					fmt.Fprintf(w, "%s\n", data)
					return false
				}

				text := extractTextFromSSEData(chunk.Data)
				piece := OllamaGenerateStreamChunk{
					Model:     cleanModel,
					CreatedAt: now,
					Response:  text,
					Done:      false,
				}
				data, _ := json.Marshal(piece)
				fmt.Fprintf(w, "%s\n", data)
				return true
			})
			return
		}

		// Non-streaming path.
		response, err := adapter.GenerateContent(cleanModel, openaiBody)
		if err != nil {
			log.Printf("[Ollama Generate] GenerateContent error: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		content := extractTextFromOpenAIResponse(response)

		c.JSON(http.StatusOK, OllamaGenerateResponse{
			Model:      cleanModel,
			CreatedAt:  now,
			Response:   content,
			Done:       true,
			DoneReason: "stop",
		})
	}
}

// ─────────────────────────────────────────────
// Internal helpers: response parsing
// ─────────────────────────────────────────────

// extractTextFromSSEData parses a raw SSE data payload (JSON bytes from an
// OpenAI stream chunk) and extracts the delta content text.
//
// Expected shape: {"choices":[{"delta":{"content":"text"},...}],...}
func extractTextFromSSEData(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(data, &obj); err != nil {
		return ""
	}
	choices, ok := obj["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return ""
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return ""
	}
	delta, ok := choice["delta"].(map[string]interface{})
	if !ok {
		return ""
	}
	content, _ := delta["content"].(string)
	return content
}

// extractTextFromOpenAIResponse pulls the assistant message content from a
// non-streaming OpenAI response body.
//
// Expected shape: {"choices":[{"message":{"content":"text"},...}],...}
func extractTextFromOpenAIResponse(resp map[string]interface{}) string {
	choices, ok := resp["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return ""
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return ""
	}
	message, ok := choice["message"].(map[string]interface{})
	if !ok {
		return ""
	}
	content, _ := message["content"].(string)
	return content
}
