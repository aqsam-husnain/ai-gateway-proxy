package providers

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	defaultClaudeBaseURL    = "https://api.anthropic.com"
	anthropicVersion        = "2023-06-01"
	claudeMessagesPath      = "/v1/messages"
)

// claudeHardcodedModels is the authoritative list of Claude models served by
// this proxy. It is returned by ListModels without making a live API call.
var claudeHardcodedModels = []map[string]string{
	{"id": "claude-opus-4-6", "object": "model", "owned_by": "anthropic"},
	{"id": "claude-sonnet-4-6", "object": "model", "owned_by": "anthropic"},
	{"id": "claude-haiku-4-5-20251001", "object": "model", "owned_by": "anthropic"},
	{"id": "claude-3-5-sonnet-20241022", "object": "model", "owned_by": "anthropic"},
	{"id": "claude-3-7-sonnet-20250219", "object": "model", "owned_by": "anthropic"},
	{"id": "claude-3-5-haiku-20241022", "object": "model", "owned_by": "anthropic"},
}

// ClaudeAdapter implements ServiceAdapter for the Anthropic Claude API.
type ClaudeAdapter struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

// NewClaudeAdapter creates a new ClaudeAdapter. If baseURL is empty it defaults
// to https://api.anthropic.com.
func NewClaudeAdapter(apiKey, baseURL string) *ClaudeAdapter {
	if baseURL == "" {
		baseURL = defaultClaudeBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	return &ClaudeAdapter{
		apiKey:  apiKey,
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: httpTimeoutSeconds,
		},
	}
}

// RefreshToken is a no-op for Claude because API keys do not expire.
func (a *ClaudeAdapter) RefreshToken() error {
	return nil
}

// applyClaudeHeaders sets the required headers for all Anthropic API requests.
func (a *ClaudeAdapter) applyClaudeHeaders(req *http.Request) {
	req.Header.Set("x-api-key", a.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("Content-Type", "application/json")
}

// GenerateContent sends a non-streaming request to the Claude /v1/messages
// endpoint and returns the parsed JSON response as a map.
func (a *ClaudeAdapter) GenerateContent(model string, requestBody map[string]interface{}) (map[string]interface{}, error) {
	body := make(map[string]interface{}, len(requestBody)+1)
	for k, v := range requestBody {
		body[k] = v
	}
	body["model"] = model
	// Ensure streaming is disabled.
	delete(body, "stream")

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("claude: marshalling request body: %w", err)
	}

	url := a.baseURL + claudeMessagesPath
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("claude: creating request: %w", err)
	}
	a.applyClaudeHeaders(req)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude: executing request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("claude: reading response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("claude: API returned HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return nil, fmt.Errorf("claude: parsing response JSON: %w", err)
	}
	return result, nil
}

// GenerateContentStream sends a streaming request to the Claude /v1/messages
// endpoint. Claude's SSE stream uses both "event:" and "data:" lines. This
// method forwards each "data:" payload as a StreamChunk. The channel is closed
// when the stream ends or an error is encountered.
func (a *ClaudeAdapter) GenerateContentStream(model string, requestBody map[string]interface{}) (<-chan StreamChunk, error) {
	body := make(map[string]interface{}, len(requestBody)+2)
	for k, v := range requestBody {
		body[k] = v
	}
	body["model"] = model
	body["stream"] = true

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("claude: marshalling streaming request body: %w", err)
	}

	url := a.baseURL + claudeMessagesPath
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("claude: creating streaming request: %w", err)
	}
	a.applyClaudeHeaders(req)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude: executing streaming request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		bodyData, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("claude: streaming API returned HTTP %d: %s", resp.StatusCode, string(bodyData))
	}

	ch := make(chan StreamChunk, 64)

	go func() {
		defer close(ch)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()

			// Skip blank lines and "event:" type lines; only forward "data:" lines.
			if strings.HasPrefix(line, "event:") || line == "" {
				continue
			}

			if !strings.HasPrefix(line, "data:") {
				continue
			}

			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))

			// End-of-stream sentinel used by some SSE implementations.
			if payload == "[DONE]" {
				ch <- StreamChunk{Done: true}
				return
			}

			// Claude signals stream end via message_stop event type; the
			// corresponding data line will contain {"type":"message_stop"}.
			// We still forward it so the caller can handle it, then close.
			ch <- StreamChunk{Data: []byte(payload)}

			// Detect message_stop to cleanly close the channel.
			var eventData map[string]interface{}
			if err := json.Unmarshal([]byte(payload), &eventData); err == nil {
				if t, ok := eventData["type"].(string); ok && t == "message_stop" {
					ch <- StreamChunk{Done: true}
					return
				}
			}
		}

		if err := scanner.Err(); err != nil {
			ch <- StreamChunk{Error: fmt.Errorf("claude: reading SSE stream: %w", err)}
		} else {
			ch <- StreamChunk{Done: true}
		}
	}()

	return ch, nil
}

// ListModels returns a hardcoded list of supported Claude model names without
// making a network request.
func (a *ClaudeAdapter) ListModels() (interface{}, error) {
	return map[string]interface{}{
		"object": "list",
		"data":   claudeHardcodedModels,
	}, nil
}
