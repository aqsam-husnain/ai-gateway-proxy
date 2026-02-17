package providers

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	geminiBaseURL        = "https://cloudcode-pa.googleapis.com"
	geminiAPIVersion     = "v1internal"
	googleTokenEndpoint  = "https://oauth2.googleapis.com/token"
	tokenExpiryThreshold = 15 * time.Minute
	httpTimeoutSeconds   = 120 * time.Second
)

// GeminiOAuthCreds holds the OAuth credentials loaded from file or base64.
type GeminiOAuthCreds struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiryDate   int64  `json:"expiry_date"` // Unix milliseconds (used by gemini-cli token files)
	Expiry       string `json:"expiry"`      // RFC3339 fallback
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	TokenType    string `json:"token_type"`
}

// GeminiAdapter implements ServiceAdapter for Google's Gemini via CloudCode endpoint.
type GeminiAdapter struct {
	mu            sync.Mutex
	projectID     string
	credsFilePath string
	credsBase64   string
	creds         *GeminiOAuthCreds
	expiryTime    time.Time
	httpClient    *http.Client
}

// NewGeminiAdapterFromFile creates a GeminiAdapter using a credentials file path.
func NewGeminiAdapterFromFile(projectID, credsFilePath string) (*GeminiAdapter, error) {
	a := &GeminiAdapter{
		projectID:     projectID,
		credsFilePath: credsFilePath,
		httpClient: &http.Client{
			Timeout: httpTimeoutSeconds,
		},
	}
	if err := a.loadCredsFromFile(); err != nil {
		return nil, fmt.Errorf("gemini: failed to load credentials from file: %w", err)
	}
	return a, nil
}

// NewGeminiAdapterFromBase64 creates a GeminiAdapter using a base64-encoded credentials string.
func NewGeminiAdapterFromBase64(projectID, credsBase64 string) (*GeminiAdapter, error) {
	a := &GeminiAdapter{
		projectID:   projectID,
		credsBase64: credsBase64,
		httpClient: &http.Client{
			Timeout: httpTimeoutSeconds,
		},
	}
	if err := a.loadCredsFromBase64(); err != nil {
		return nil, fmt.Errorf("gemini: failed to load credentials from base64: %w", err)
	}
	return a, nil
}

// loadCredsFromFile reads and parses the OAuth credentials JSON file.
func (a *GeminiAdapter) loadCredsFromFile() error {
	data, err := os.ReadFile(a.credsFilePath)
	if err != nil {
		return fmt.Errorf("reading credentials file %q: %w", a.credsFilePath, err)
	}
	return a.parseCreds(data)
}

// loadCredsFromBase64 decodes and parses a base64-encoded credentials JSON string.
func (a *GeminiAdapter) loadCredsFromBase64() error {
	data, err := base64.StdEncoding.DecodeString(a.credsBase64)
	if err != nil {
		// Try URL-safe base64 as a fallback.
		data, err = base64.URLEncoding.DecodeString(a.credsBase64)
		if err != nil {
			return fmt.Errorf("decoding base64 credentials: %w", err)
		}
	}
	return a.parseCreds(data)
}

// parseCreds unmarshals the raw JSON bytes into the internal creds struct and
// parses the expiry timestamp. Handles both expiry_date (Unix ms, used by
// gemini-cli token files) and expiry (RFC3339 string).
func (a *GeminiAdapter) parseCreds(data []byte) error {
	var creds GeminiOAuthCreds
	if err := json.Unmarshal(data, &creds); err != nil {
		return fmt.Errorf("unmarshalling credentials JSON: %w", err)
	}
	a.creds = &creds

	// expiry_date (Unix milliseconds) takes priority — this is the format written
	// by the gemini-cli OAuth flow.
	if creds.ExpiryDate > 0 {
		a.expiryTime = time.Unix(0, creds.ExpiryDate*int64(time.Millisecond))
		return nil
	}

	// Fall back to RFC3339 expiry string.
	if creds.Expiry != "" {
		formats := []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02T15:04:05.999999999Z07:00",
		}
		var parseErr error
		for _, format := range formats {
			t, err := time.Parse(format, creds.Expiry)
			if err == nil {
				a.expiryTime = t
				parseErr = nil
				break
			}
			parseErr = err
		}
		if parseErr != nil {
			// Non-fatal: treat token as already expired so we refresh immediately.
			a.expiryTime = time.Now().Add(-1 * time.Second)
		}
		return nil
	}

	// No expiry info at all; assume valid for 1 hour so we try immediately on
	// first near-expiry check.
	a.expiryTime = time.Now().Add(1 * time.Hour)
	return nil
}

// IsExpiryNear returns true when the current access token expires within 15 minutes.
func (a *GeminiAdapter) IsExpiryNear() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Until(a.expiryTime) < tokenExpiryThreshold
}

// RefreshToken uses the stored refresh_token to obtain a new access_token from
// Google's OAuth2 token endpoint if the current token is near expiry.
// Falls back to the built-in gemini-cli OAuth client credentials when the creds
// file does not contain client_id / client_secret.
func (a *GeminiAdapter) RefreshToken() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Double-checked locking: another goroutine may have already refreshed.
	if time.Until(a.expiryTime) >= tokenExpiryThreshold {
		return nil
	}

	if a.creds == nil {
		return fmt.Errorf("gemini: no credentials loaded")
	}
	if a.creds.RefreshToken == "" {
		return fmt.Errorf("gemini: no refresh_token available for token refresh")
	}

	// Use creds-file client ID/secret if present; fall back to env vars
	// GEMINI_CLI_OAUTH_CLIENT_ID / GEMINI_CLI_OAUTH_CLIENT_SECRET.
	clientID := a.creds.ClientID
	if clientID == "" {
		clientID = os.Getenv("GEMINI_CLI_OAUTH_CLIENT_ID")
	}
	clientSecret := a.creds.ClientSecret
	if clientSecret == "" {
		clientSecret = os.Getenv("GEMINI_CLI_OAUTH_CLIENT_SECRET")
	}
	if clientID == "" || clientSecret == "" {
		return fmt.Errorf("gemini: client_id and client_secret are required for token refresh (set GEMINI_CLI_OAUTH_CLIENT_ID / GEMINI_CLI_OAUTH_CLIENT_SECRET)")
	}

	body := strings.NewReader(fmt.Sprintf(
		"client_id=%s&client_secret=%s&refresh_token=%s&grant_type=refresh_token",
		clientID,
		clientSecret,
		a.creds.RefreshToken,
	))

	req, err := http.NewRequest(http.MethodPost, googleTokenEndpoint, body)
	if err != nil {
		return fmt.Errorf("gemini: creating token refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("gemini: executing token refresh request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("gemini: reading token refresh response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gemini: token refresh returned HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(respBytes, &tokenResp); err != nil {
		return fmt.Errorf("gemini: parsing token refresh response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return fmt.Errorf("gemini: token refresh response missing access_token")
	}

	a.creds.AccessToken = tokenResp.AccessToken
	if tokenResp.ExpiresIn > 0 {
		a.expiryTime = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	} else {
		a.expiryTime = time.Now().Add(1 * time.Hour)
	}

	// Persist the new token back to file if that's how we loaded credentials.
	if a.credsFilePath != "" {
		// Write expiry_date as Unix milliseconds to match gemini-cli format.
		a.creds.ExpiryDate = a.expiryTime.UnixMilli()
		a.creds.Expiry = a.expiryTime.Format(time.RFC3339)
		if data, err := json.MarshalIndent(a.creds, "", "  "); err == nil {
			_ = os.WriteFile(a.credsFilePath, data, 0600)
		}
	}

	return nil
}

// accessToken returns the current access token, refreshing if necessary.
func (a *GeminiAdapter) accessToken() (string, error) {
	if a.IsExpiryNear() {
		if err := a.RefreshToken(); err != nil {
			return "", err
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.creds == nil || a.creds.AccessToken == "" {
		return "", fmt.Errorf("gemini: no access token available")
	}
	return a.creds.AccessToken, nil
}

// endpointURL returns the fully-formed CloudCode endpoint URL for the given
// method (e.g. "chatCompletions" or "streamChatCompletions").
// Format: https://cloudcode-pa.googleapis.com/v1internal:{method}
// Note: project ID goes in the request body, NOT the URL path.
func (a *GeminiAdapter) endpointURL(method string) string {
	return fmt.Sprintf("%s/%s:%s", geminiBaseURL, geminiAPIVersion, method)
}

// GenerateContent sends a non-streaming chat completion request to the Gemini
// CloudCode endpoint and returns the JSON response as a map.
func (a *GeminiAdapter) GenerateContent(model string, requestBody map[string]interface{}) (map[string]interface{}, error) {
	token, err := a.accessToken()
	if err != nil {
		return nil, err
	}

	// Wrap the request body as required by the CloudCode API:
	// { "model": ..., "project": ..., "request": <originalBody> }
	wrappedBody := map[string]interface{}{
		"model":   model,
		"project": a.projectID,
		"request": requestBody,
	}

	bodyBytes, err := json.Marshal(wrappedBody)
	if err != nil {
		return nil, fmt.Errorf("gemini: marshalling request body: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, a.endpointURL("chatCompletions"), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("gemini: creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini: executing request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gemini: reading response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gemini: API returned HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return nil, fmt.Errorf("gemini: parsing response JSON: %w", err)
	}
	return result, nil
}

// GenerateContentStream sends a streaming chat completion request to the Gemini
// CloudCode endpoint and returns a channel of StreamChunk values. Each chunk
// carries the raw SSE data line bytes. The channel is closed when the stream
// ends or an error occurs.
func (a *GeminiAdapter) GenerateContentStream(model string, requestBody map[string]interface{}) (<-chan StreamChunk, error) {
	token, err := a.accessToken()
	if err != nil {
		return nil, err
	}

	// Wrap the request body as required by the CloudCode API:
	// { "model": ..., "project": ..., "request": <originalBody> }
	wrappedBody := map[string]interface{}{
		"model":   model,
		"project": a.projectID,
		"request": requestBody,
	}

	bodyBytes, err := json.Marshal(wrappedBody)
	if err != nil {
		return nil, fmt.Errorf("gemini: marshalling streaming request body: %w", err)
	}

	// Stream URL uses ?alt=sse query parameter.
	streamURL := a.endpointURL("streamChatCompletions") + "?alt=sse"

	req, err := http.NewRequest(http.MethodPost, streamURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("gemini: creating streaming request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini: executing streaming request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("gemini: streaming API returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	ch := make(chan StreamChunk, 64)

	go func() {
		defer close(ch)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()

			// SSE lines begin with "data: ".
			if !strings.HasPrefix(line, "data:") {
				continue
			}

			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))

			// End-of-stream sentinel.
			if payload == "[DONE]" {
				ch <- StreamChunk{Done: true}
				return
			}

			ch <- StreamChunk{Data: []byte(payload)}
		}

		if err := scanner.Err(); err != nil {
			ch <- StreamChunk{Error: fmt.Errorf("gemini: reading SSE stream: %w", err)}
		} else {
			ch <- StreamChunk{Done: true}
		}
	}()

	return ch, nil
}

// ListModels returns a hardcoded list of supported Gemini model names.
func (a *GeminiAdapter) ListModels() (interface{}, error) {
	models := []map[string]string{
		{"id": "gemini-2.5-pro-preview-03-25", "object": "model", "owned_by": "google"},
		{"id": "gemini-2.0-flash-001", "object": "model", "owned_by": "google"},
		{"id": "gemini-2.0-flash-thinking-exp-01-21", "object": "model", "owned_by": "google"},
		{"id": "gemini-2.0-pro-exp-02-05", "object": "model", "owned_by": "google"},
		{"id": "gemini-1.5-pro-002", "object": "model", "owned_by": "google"},
		{"id": "gemini-1.5-flash-002", "object": "model", "owned_by": "google"},
		{"id": "gemini-1.5-pro-001", "object": "model", "owned_by": "google"},
		{"id": "gemini-1.5-flash-001", "object": "model", "owned_by": "google"},
	}
	return map[string]interface{}{
		"object": "list",
		"data":   models,
	}, nil
}
