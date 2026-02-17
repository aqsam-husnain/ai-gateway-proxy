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
	defaultOpenAIBaseURL = "https://api.openai.com/v1"
)

// OpenAIAdapter implements ServiceAdapter for the OpenAI API.
type OpenAIAdapter struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

// NewOpenAIAdapter creates a new OpenAIAdapter. If baseURL is empty, it defaults
// to https://api.openai.com/v1.
func NewOpenAIAdapter(apiKey, baseURL string) *OpenAIAdapter {
	if baseURL == "" {
		baseURL = defaultOpenAIBaseURL
	}
	// Remove trailing slash for consistent URL construction.
	baseURL = strings.TrimRight(baseURL, "/")

	return &OpenAIAdapter{
		apiKey:  apiKey,
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: httpTimeoutSeconds,
		},
	}
}

// RefreshToken is a no-op for the OpenAI adapter because API keys do not expire.
func (a *OpenAIAdapter) RefreshToken() error {
	return nil
}

// GetProviderType returns the canonical provider-type string for this adapter.
func (a *OpenAIAdapter) GetProviderType() string {
	return ProviderOpenAI
}

// GenerateContent sends a non-streaming chat completion request to the OpenAI
// /chat/completions endpoint and returns the parsed JSON response as a map.
func (a *OpenAIAdapter) GenerateContent(model string, requestBody map[string]interface{}) (map[string]interface{}, error) {
	body := make(map[string]interface{}, len(requestBody)+1)
	for k, v := range requestBody {
		body[k] = v
	}
	body["model"] = model
	// Ensure streaming is disabled.
	delete(body, "stream")

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai: marshalling request body: %w", err)
	}

	url := a.baseURL + "/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("openai: creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai: executing request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openai: reading response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai: API returned HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return nil, fmt.Errorf("openai: parsing response JSON: %w", err)
	}
	return result, nil
}

// GenerateContentStream sends a streaming chat completion request to the OpenAI
// /chat/completions endpoint and returns a channel of StreamChunk values.
// Each chunk carries the raw SSE data payload bytes. The channel is closed
// when the stream ends or an error is encountered.
func (a *OpenAIAdapter) GenerateContentStream(model string, requestBody map[string]interface{}) (<-chan StreamChunk, error) {
	body := make(map[string]interface{}, len(requestBody)+2)
	for k, v := range requestBody {
		body[k] = v
	}
	body["model"] = model
	body["stream"] = true

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai: marshalling streaming request body: %w", err)
	}

	url := a.baseURL + "/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("openai: creating streaming request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai: executing streaming request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		bodyData, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("openai: streaming API returned HTTP %d: %s", resp.StatusCode, string(bodyData))
	}

	ch := make(chan StreamChunk, 64)

	go func() {
		defer close(ch)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()

			if !strings.HasPrefix(line, "data:") {
				continue
			}

			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))

			if payload == "[DONE]" {
				ch <- StreamChunk{Done: true}
				return
			}

			ch <- StreamChunk{Data: []byte(payload)}
		}

		if err := scanner.Err(); err != nil {
			ch <- StreamChunk{Error: fmt.Errorf("openai: reading SSE stream: %w", err)}
		} else {
			ch <- StreamChunk{Done: true}
		}
	}()

	return ch, nil
}

// ListModels fetches the list of available models from the OpenAI /models endpoint
// and returns the raw parsed JSON response.
func (a *OpenAIAdapter) ListModels() (interface{}, error) {
	url := a.baseURL + "/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("openai: creating list models request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.apiKey)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai: executing list models request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openai: reading list models response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai: list models returned HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var result interface{}
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return nil, fmt.Errorf("openai: parsing list models response: %w", err)
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// OpenAIResponsesAdapter — wraps the OpenAI Responses API (/v1/responses)
// ---------------------------------------------------------------------------

// OpenAIResponsesAdapter implements ServiceAdapter for the OpenAI Responses API.
// It reuses OpenAIAdapter's HTTP client and credentials but targets the
// /v1/responses endpoint instead of /v1/chat/completions.
type OpenAIResponsesAdapter struct {
	OpenAIAdapter
}

// NewOpenAIResponsesAdapter creates an OpenAIResponsesAdapter.  If baseURL is
// empty it defaults to https://api.openai.com/v1.
func NewOpenAIResponsesAdapter(apiKey, baseURL string) *OpenAIResponsesAdapter {
	return &OpenAIResponsesAdapter{OpenAIAdapter: *NewOpenAIAdapter(apiKey, baseURL)}
}

// GetProviderType returns the canonical provider type for the OpenAI Responses API.
func (a *OpenAIResponsesAdapter) GetProviderType() string {
	return ProviderOpenAIResp
}

// GenerateContent sends a non-streaming request to the OpenAI /v1/responses endpoint.
func (a *OpenAIResponsesAdapter) GenerateContent(model string, requestBody map[string]interface{}) (map[string]interface{}, error) {
	body := make(map[string]interface{}, len(requestBody)+1)
	for k, v := range requestBody {
		body[k] = v
	}
	body["model"] = model
	delete(body, "stream")

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai-responses: marshalling request body: %w", err)
	}

	url := a.baseURL + "/responses"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("openai-responses: creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai-responses: executing request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openai-responses: reading response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai-responses: API returned HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return nil, fmt.Errorf("openai-responses: parsing response JSON: %w", err)
	}
	return result, nil
}

// GenerateContentStream sends a streaming request to the OpenAI /v1/responses endpoint.
func (a *OpenAIResponsesAdapter) GenerateContentStream(model string, requestBody map[string]interface{}) (<-chan StreamChunk, error) {
	body := make(map[string]interface{}, len(requestBody)+2)
	for k, v := range requestBody {
		body[k] = v
	}
	body["model"] = model
	body["stream"] = true

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai-responses: marshalling streaming request body: %w", err)
	}

	url := a.baseURL + "/responses"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("openai-responses: creating streaming request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai-responses: executing streaming request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		bodyData, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("openai-responses: API returned HTTP %d: %s", resp.StatusCode, string(bodyData))
	}

	ch := make(chan StreamChunk, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				ch <- StreamChunk{Done: true}
				return
			}
			ch <- StreamChunk{Data: []byte(payload)}
		}
		if err := scanner.Err(); err != nil {
			ch <- StreamChunk{Error: fmt.Errorf("openai-responses: reading SSE stream: %w", err)}
		} else {
			ch <- StreamChunk{Done: true}
		}
	}()
	return ch, nil
}
