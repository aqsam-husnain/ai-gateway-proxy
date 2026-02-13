package providers

import "strings"

// ---------------------------------------------------------------------------
// Provider type constants
// ---------------------------------------------------------------------------

const (
	ProviderGeminiCLI   = "gemini-cli-oauth"
	ProviderAntigravity = "gemini-antigravity"
	ProviderOpenAI      = "openai-custom"
	ProviderOpenAIResp  = "openaiResponses-custom"
	ProviderClaude      = "claude-custom"
	ProviderClaudeCode  = "claudeCode-custom"
)

// ---------------------------------------------------------------------------
// Protocol prefix constants
// ---------------------------------------------------------------------------

const (
	ProtocolGemini      = "gemini"
	ProtocolOpenAI      = "openai"
	ProtocolOpenAIResp  = "openaiResponses"
	ProtocolClaude      = "claude"
	ProtocolClaudeCode  = "claudeCode"
	ProtocolOllama      = "ollama"
)

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

// GetProtocolPrefix extracts the protocol from a provider string.
// The protocol is everything up to (but not including) the first hyphen that
// begins the "suffix" part of the name.
//
// Examples:
//
//	"gemini-cli-oauth"       -> "gemini"
//	"gemini-antigravity"     -> "gemini"
//	"openai-custom"          -> "openai"
//	"openaiResponses-custom" -> "openaiResponses"
//	"claude-custom"          -> "claude"
//	"claudeCode-custom"      -> "claudeCode"
//	"ollama-local"           -> "ollama"
func GetProtocolPrefix(provider string) string {
	// The protocol is the portion up to the first '-' that is NOT part of a
	// known multi-word prefix such as "openaiResponses" or "claudeCode".
	// We use a fixed ordered prefix list so that longer, more specific
	// prefixes are matched before shorter ones.
	knownPrefixes := []string{
		ProtocolOpenAIResp, // "openaiResponses" — must come before "openai"
		ProtocolClaudeCode, // "claudeCode"      — must come before "claude"
		ProtocolGemini,
		ProtocolOpenAI,
		ProtocolClaude,
		ProtocolOllama,
	}

	for _, prefix := range knownPrefixes {
		if strings.HasPrefix(provider, prefix) {
			return prefix
		}
	}

	// Fallback: return everything up to the first hyphen.
	if idx := strings.Index(provider, "-"); idx != -1 {
		return provider[:idx]
	}
	return provider
}

// GetProviderModels returns the known model names for a given provider type.
// Providers that accept any model (openai-custom, claude-custom,
// openaiResponses-custom) return an empty slice indicating "all models
// accepted".
func GetProviderModels(provider string) []string {
	switch provider {
	case ProviderGeminiCLI:
		return []string{
			"gemini-2.5-flash",
			"gemini-2.5-flash-lite",
			"gemini-2.5-pro",
			"gemini-2.5-pro-preview-06-05",
			"gemini-2.5-flash-preview-09-2025",
			"gemini-3-pro-preview",
			"gemini-3-flash-preview",
		}
	case ProviderAntigravity:
		return []string{
			"gemini-2.5-computer-use-preview-10-2025",
			"gemini-3-pro-image-preview",
			"gemini-3-pro-preview",
			"gemini-3-pro-low",
			"gemini-3-flash-preview",
			"gemini-2.5-flash",
			"gemini-2.5-pro",
			"gemini-2.5-flash-lite",
			"gemini-2.5-flash-thinking",
			"gpt-oss-120b-medium",
			"gemini-claude-sonnet-4-5",
			"gemini-claude-sonnet-4-5-thinking",
			"gemini-claude-opus-4-5-thinking",
		}
	case ProviderClaudeCode:
		return []string{"opus", "sonnet", "haiku"}
	case ProviderOpenAI, ProviderOpenAIResp, ProviderClaude:
		// These providers accept whatever models the underlying API offers.
		return []string{}
	default:
		return []string{}
	}
}

// ---------------------------------------------------------------------------
// Shared / generic types
// ---------------------------------------------------------------------------

// ContentPart represents a single part inside a multimodal message.
type ContentPart struct {
	Type     string    `json:"type"`                // "text" | "image_url"
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL is the OpenAI-style image reference.
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"` // "low" | "high" | "auto"
}

// Message is the shared conversation-turn type used across OpenAI-compatible
// requests.  The Content field can hold either a plain string or an array of
// ContentParts; callers should use ContentString / ContentParts helpers when
// they need to distinguish the two.
type Message struct {
	Role       string        `json:"role"`
	Content    interface{}   `json:"content"` // string | []ContentPart
	Name       string        `json:"name,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall    `json:"tool_calls,omitempty"`
}

// ToolCall represents a model-generated tool invocation.
type ToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"` // always "function"
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction is the function payload inside a ToolCall.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded string
}

// Tool describes a callable tool provided to the model.
type Tool struct {
	Type     string       `json:"type"` // "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction is the schema definition of a callable function.
type ToolFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

// Usage holds token-count billing information.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ErrorDetail is the inner object of an ErrorResponse.
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}

// ErrorResponse is the standard error envelope used by OpenAI-compatible APIs.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ---------------------------------------------------------------------------
// OpenAI chat-completion types
// ---------------------------------------------------------------------------

// OpenAIRequest is the request body for POST /v1/chat/completions.
type OpenAIRequest struct {
	Model            string                 `json:"model"`
	Messages         []Message              `json:"messages"`
	Stream           bool                   `json:"stream,omitempty"`
	Temperature      *float64               `json:"temperature,omitempty"`
	TopP             *float64               `json:"top_p,omitempty"`
	MaxTokens        *int                   `json:"max_tokens,omitempty"`
	N                *int                   `json:"n,omitempty"`
	Stop             interface{}            `json:"stop,omitempty"` // string | []string
	PresencePenalty  *float64               `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64               `json:"frequency_penalty,omitempty"`
	LogitBias        map[string]float64     `json:"logit_bias,omitempty"`
	User             string                 `json:"user,omitempty"`
	Tools            []Tool                 `json:"tools,omitempty"`
	ToolChoice       interface{}            `json:"tool_choice,omitempty"` // "none"|"auto"|object
	ResponseFormat   *ResponseFormat        `json:"response_format,omitempty"`
	Seed             *int64                 `json:"seed,omitempty"`
	StreamOptions    *StreamOptions         `json:"stream_options,omitempty"`
	// Passthrough for any provider-specific extra fields.
	Extra map[string]interface{} `json:"-"`
}

// ResponseFormat controls structured output.
type ResponseFormat struct {
	Type string `json:"type"` // "text" | "json_object" | "json_schema"
}

// StreamOptions controls streaming behaviour extras.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// Choice is a single completion alternative inside an OpenAI response.
type Choice struct {
	Index        int      `json:"index"`
	Message      *Message `json:"message,omitempty"`
	Delta        *Message `json:"delta,omitempty"`
	FinishReason string   `json:"finish_reason"`
	Logprobs     *Logprobs `json:"logprobs,omitempty"`
}

// Logprobs holds optional token log-probability data.
type Logprobs struct {
	Content []LogprobContent `json:"content,omitempty"`
}

// LogprobContent is a single token with its log probability.
type LogprobContent struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`
	Bytes   []int   `json:"bytes,omitempty"`
}

// OpenAIResponse is the response body for a non-streaming completion.
type OpenAIResponse struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	Created           int64    `json:"created"`
	Model             string   `json:"model"`
	SystemFingerprint string   `json:"system_fingerprint,omitempty"`
	Choices           []Choice `json:"choices"`
	Usage             *Usage   `json:"usage,omitempty"`
}

// OpenAIStreamChunk is a single server-sent event chunk during streaming.
type OpenAIStreamChunk struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	Created           int64    `json:"created"`
	Model             string   `json:"model"`
	SystemFingerprint string   `json:"system_fingerprint,omitempty"`
	Choices           []Choice `json:"choices"`
	Usage             *Usage   `json:"usage,omitempty"`
}

// ---------------------------------------------------------------------------
// OpenAI model-list types
// ---------------------------------------------------------------------------

// ModelObject is a single entry in GET /v1/models.
type ModelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelList is the response envelope for GET /v1/models.
type ModelList struct {
	Object string        `json:"object"`
	Data   []ModelObject `json:"data"`
}

// GeminiModelList mirrors the Gemini API model-list response.
type GeminiModelList struct {
	Models []GeminiModelEntry `json:"models"`
}

// GeminiModelEntry is a single model entry in the Gemini format.
type GeminiModelEntry struct {
	Name                       string   `json:"name"`
	Version                    string   `json:"version,omitempty"`
	DisplayName                string   `json:"displayName,omitempty"`
	Description                string   `json:"description,omitempty"`
	InputTokenLimit            int      `json:"inputTokenLimit,omitempty"`
	OutputTokenLimit           int      `json:"outputTokenLimit,omitempty"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods,omitempty"`
	Temperature                float64  `json:"temperature,omitempty"`
	MaxTemperature             float64  `json:"maxTemperature,omitempty"`
	TopP                       float64  `json:"topP,omitempty"`
	TopK                       int      `json:"topK,omitempty"`
}

// ---------------------------------------------------------------------------
// Gemini types
// ---------------------------------------------------------------------------

// GeminiPart is a single content part in the Gemini API format.
type GeminiPart struct {
	Text           string            `json:"text,omitempty"`
	InlineData     *GeminiInlineData `json:"inlineData,omitempty"`
	FunctionCall   *GeminiFunctionCall   `json:"functionCall,omitempty"`
	FunctionResponse *GeminiFunctionResponse `json:"functionResponse,omitempty"`
}

// GeminiInlineData holds base-64 encoded binary data (e.g., images).
type GeminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"` // base64-encoded
}

// GeminiFunctionCall represents a model's request to call a function.
type GeminiFunctionCall struct {
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args,omitempty"`
}

// GeminiFunctionResponse is the result of a function call returned to the model.
type GeminiFunctionResponse struct {
	Name     string                 `json:"name"`
	Response map[string]interface{} `json:"response,omitempty"`
}

// GeminiContent is a single conversation turn in the Gemini API format.
type GeminiContent struct {
	Role  string       `json:"role,omitempty"` // "user" | "model"
	Parts []GeminiPart `json:"parts"`
}

// GeminiSystemInstruction wraps the system prompt for Gemini.
type GeminiSystemInstruction struct {
	Parts []GeminiPart `json:"parts"`
}

// GeminiGenerationConfig holds the generation parameters for Gemini.
type GeminiGenerationConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	TopK            *int     `json:"topK,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
	CandidateCount  *int     `json:"candidateCount,omitempty"`
	ThinkingConfig  *GeminiThinkingConfig `json:"thinkingConfig,omitempty"`
}

// GeminiThinkingConfig controls extended-thinking budget.
type GeminiThinkingConfig struct {
	ThinkingBudget int `json:"thinkingBudget,omitempty"`
}

// GeminiTool wraps a function declaration for the Gemini tool-use API.
type GeminiTool struct {
	FunctionDeclarations []GeminiFunctionDeclaration `json:"functionDeclarations,omitempty"`
}

// GeminiFunctionDeclaration describes a callable function for Gemini.
type GeminiFunctionDeclaration struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

// GeminiRequest is the request body for Gemini generateContent /
// streamGenerateContent.
type GeminiRequest struct {
	Contents            []GeminiContent          `json:"contents"`
	SystemInstruction   *GeminiSystemInstruction `json:"systemInstruction,omitempty"`
	GenerationConfig    *GeminiGenerationConfig  `json:"generationConfig,omitempty"`
	Tools               []GeminiTool             `json:"tools,omitempty"`
	SafetySettings      []GeminiSafetySetting    `json:"safetySettings,omitempty"`
}

// GeminiSafetySetting configures content safety thresholds.
type GeminiSafetySetting struct {
	Category  string `json:"category"`
	Threshold string `json:"threshold"`
}

// GeminiCandidate is a single candidate response from Gemini.
type GeminiCandidate struct {
	Content       GeminiContent        `json:"content"`
	FinishReason  string               `json:"finishReason,omitempty"`
	Index         int                  `json:"index"`
	SafetyRatings []GeminiSafetyRating `json:"safetyRatings,omitempty"`
}

// GeminiSafetyRating is a per-category content safety assessment.
type GeminiSafetyRating struct {
	Category    string `json:"category"`
	Probability string `json:"probability"`
	Blocked     bool   `json:"blocked,omitempty"`
}

// GeminiUsageMetadata holds token counts returned by Gemini.
type GeminiUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// GeminiResponse is the non-streaming response from Gemini.
type GeminiResponse struct {
	Candidates    []GeminiCandidate   `json:"candidates"`
	UsageMetadata GeminiUsageMetadata `json:"usageMetadata,omitempty"`
	ModelVersion  string              `json:"modelVersion,omitempty"`
}

// ---------------------------------------------------------------------------
// Claude types
// ---------------------------------------------------------------------------

// ClaudeContentBlock is a single block inside a Claude message.
type ClaudeContentBlock struct {
	Type  string `json:"type"`            // "text" | "image" | "tool_use" | "tool_result"
	Text  string `json:"text,omitempty"`
	// For tool_use
	ID    string                 `json:"id,omitempty"`
	Name  string                 `json:"name,omitempty"`
	Input map[string]interface{} `json:"input,omitempty"`
	// For tool_result
	ToolUseID string      `json:"tool_use_id,omitempty"`
	Content   interface{} `json:"content,omitempty"` // string | []ClaudeContentBlock
	IsError   bool        `json:"is_error,omitempty"`
	// For image
	Source *ClaudeImageSource `json:"source,omitempty"`
}

// ClaudeImageSource describes the source of an image content block.
type ClaudeImageSource struct {
	Type      string `json:"type"`       // "base64" | "url"
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// ClaudeMessage is a single conversation turn in the Claude API format.
// Content can be a plain string or []ClaudeContentBlock.
type ClaudeMessage struct {
	Role    string      `json:"role"`    // "user" | "assistant"
	Content interface{} `json:"content"` // string | []ClaudeContentBlock
}

// ClaudeTool describes a callable function for Claude.
type ClaudeTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"input_schema"`
}

// ClaudeRequest is the request body for POST /v1/messages.
type ClaudeRequest struct {
	Model         string          `json:"model"`
	Messages      []ClaudeMessage `json:"messages"`
	System        interface{}     `json:"system,omitempty"` // string | []ClaudeContentBlock
	MaxTokens     int             `json:"max_tokens"`
	Stream        bool            `json:"stream,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int            `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Tools         []ClaudeTool    `json:"tools,omitempty"`
	ToolChoice    interface{}     `json:"tool_choice,omitempty"` // "auto"|"any"|object
	Metadata      *ClaudeMetadata `json:"metadata,omitempty"`
}

// ClaudeMetadata holds optional per-request metadata for Claude.
type ClaudeMetadata struct {
	UserID string `json:"user_id,omitempty"`
}

// ClaudeUsage holds token counts from the Claude API.
type ClaudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ClaudeResponse is the non-streaming response from Claude.
type ClaudeResponse struct {
	ID           string               `json:"id"`
	Type         string               `json:"type"` // "message"
	Role         string               `json:"role"` // "assistant"
	Content      []ClaudeContentBlock `json:"content"`
	Model        string               `json:"model"`
	StopReason   string               `json:"stop_reason,omitempty"`
	StopSequence string               `json:"stop_sequence,omitempty"`
	Usage        ClaudeUsage          `json:"usage"`
}

// ClaudeStreamEvent is a single server-sent event during Claude streaming.
type ClaudeStreamEvent struct {
	Type  string      `json:"type"`
	Index *int        `json:"index,omitempty"`
	Delta interface{} `json:"delta,omitempty"`
	// Depending on event type, additional fields are populated.
	Message      *ClaudeResponse      `json:"message,omitempty"`
	ContentBlock *ClaudeContentBlock  `json:"content_block,omitempty"`
	Usage        *ClaudeUsage         `json:"usage,omitempty"`
}

// ---------------------------------------------------------------------------
// Ollama types
// ---------------------------------------------------------------------------

// OllamaMessage is a single conversation turn in the Ollama chat format.
type OllamaMessage struct {
	Role    string   `json:"role"`
	Content string   `json:"content"`
	Images  []string `json:"images,omitempty"` // base64-encoded image data
}

// OllamaOptions holds model parameter overrides for Ollama.
type OllamaOptions struct {
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"top_p,omitempty"`
	TopK          *int     `json:"top_k,omitempty"`
	NumPredict    *int     `json:"num_predict,omitempty"`
	Stop          []string `json:"stop,omitempty"`
	Seed          *int64   `json:"seed,omitempty"`
	NumCtx        *int     `json:"num_ctx,omitempty"`
	RepeatPenalty *float64 `json:"repeat_penalty,omitempty"`
}

// OllamaRequest is the request body for POST /api/chat or POST /api/generate.
type OllamaRequest struct {
	Model    string          `json:"model"`
	Messages []OllamaMessage `json:"messages,omitempty"` // for /api/chat
	Prompt   string          `json:"prompt,omitempty"`   // for /api/generate
	Stream   *bool           `json:"stream,omitempty"`
	Options  *OllamaOptions  `json:"options,omitempty"`
	Format   string          `json:"format,omitempty"` // "json" for structured output
	System   string          `json:"system,omitempty"`
	Template string          `json:"template,omitempty"`
	Context  []int           `json:"context,omitempty"`
	Raw      bool            `json:"raw,omitempty"`
	KeepAlive string         `json:"keep_alive,omitempty"`
}

// OllamaResponse is the response body for Ollama completions.
type OllamaResponse struct {
	Model              string        `json:"model"`
	CreatedAt          string        `json:"created_at"`
	Message            OllamaMessage `json:"message,omitempty"`
	Response           string        `json:"response,omitempty"` // for /api/generate
	Done               bool          `json:"done"`
	DoneReason         string        `json:"done_reason,omitempty"`
	Context            []int         `json:"context,omitempty"`
	TotalDuration      int64         `json:"total_duration,omitempty"`
	LoadDuration       int64         `json:"load_duration,omitempty"`
	PromptEvalCount    int           `json:"prompt_eval_count,omitempty"`
	PromptEvalDuration int64         `json:"prompt_eval_duration,omitempty"`
	EvalCount          int           `json:"eval_count,omitempty"`
	EvalDuration       int64         `json:"eval_duration,omitempty"`
}
