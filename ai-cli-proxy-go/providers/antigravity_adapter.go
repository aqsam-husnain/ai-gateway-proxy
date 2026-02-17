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

// AntigravityOAuthCreds holds the OAuth credentials for the Antigravity service.
// The JSON structure mirrors that used by GeminiOAuthCreds.
type AntigravityOAuthCreds struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiryDate   int64  `json:"expiry_date"` // Unix milliseconds (used by gemini-cli token files)
	Expiry       string `json:"expiry"`      // RFC3339 fallback
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	TokenType    string `json:"token_type"`
}

// AntigravityAdapter implements ServiceAdapter for the Antigravity (internal)
// AI service. It shares the same OAuth flow as GeminiAdapter but targets a
// different endpoint URL.
type AntigravityAdapter struct {
	mu            sync.Mutex
	credsFilePath string
	credsBase64   string
	baseURL       string
	creds         *AntigravityOAuthCreds
	expiryTime    time.Time
	httpClient    *http.Client
}

// NewAntigravityAdapterFromFile creates an AntigravityAdapter loading OAuth
// credentials from a JSON file on disk. baseURL must be one of the daily or
// autopush URLs provided by the Antigravity service configuration.
func NewAntigravityAdapterFromFile(credsFilePath, baseURL string) (*AntigravityAdapter, error) {
	a := &AntigravityAdapter{
		credsFilePath: credsFilePath,
		baseURL:       strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: httpTimeoutSeconds,
		},
	}
	if err := a.loadCredsFromFile(); err != nil {
		return nil, fmt.Errorf("antigravity: failed to load credentials from file: %w", err)
	}
	return a, nil
}

// NewAntigravityAdapterFromBase64 creates an AntigravityAdapter loading OAuth
// credentials from a base64-encoded JSON string.
func NewAntigravityAdapterFromBase64(credsBase64, baseURL string) (*AntigravityAdapter, error) {
	a := &AntigravityAdapter{
		credsBase64: credsBase64,
		baseURL:     strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: httpTimeoutSeconds,
		},
	}
	if err := a.loadCredsFromBase64(); err != nil {
		return nil, fmt.Errorf("antigravity: failed to load credentials from base64: %w", err)
	}
	return a, nil
}

// loadCredsFromFile reads and parses the OAuth credentials JSON file.
func (a *AntigravityAdapter) loadCredsFromFile() error {
	data, err := os.ReadFile(a.credsFilePath)
	if err != nil {
		return fmt.Errorf("reading credentials file %q: %w", a.credsFilePath, err)
	}
	return a.parseCreds(data)
}

// loadCredsFromBase64 decodes and parses a base64-encoded credentials JSON string.
func (a *AntigravityAdapter) loadCredsFromBase64() error {
	data, err := base64.StdEncoding.DecodeString(a.credsBase64)
	if err != nil {
		data, err = base64.URLEncoding.DecodeString(a.credsBase64)
		if err != nil {
			return fmt.Errorf("decoding base64 credentials: %w", err)
		}
	}
	return a.parseCreds(data)
}

// parseCreds unmarshals raw JSON bytes into the internal creds struct and
// parses the expiry timestamp. Handles both expiry_date (Unix ms, used by
// gemini-cli token files) and expiry (RFC3339 string).
func (a *AntigravityAdapter) parseCreds(data []byte) error {
	var creds AntigravityOAuthCreds
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
			// Treat as already expired so refresh happens immediately.
			a.expiryTime = time.Now().Add(-1 * time.Second)
		}
		return nil
	}

	a.expiryTime = time.Now().Add(1 * time.Hour)
	return nil
}

// IsExpiryNear returns true when the current access token expires within 15 minutes.
func (a *AntigravityAdapter) IsExpiryNear() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Until(a.expiryTime) < tokenExpiryThreshold
}

// RefreshToken uses the stored refresh_token to obtain a new access_token from
// Google's OAuth2 token endpoint if the current token is near expiry.
// Falls back to the built-in gemini-cli OAuth client credentials when the creds
// file does not contain client_id / client_secret.
func (a *AntigravityAdapter) RefreshToken() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Double-checked locking: another goroutine may have already refreshed.
	if time.Until(a.expiryTime) >= tokenExpiryThreshold {
		return nil
	}

	if a.creds == nil {
		return fmt.Errorf("antigravity: no credentials loaded")
	}
	if a.creds.RefreshToken == "" {
		return fmt.Errorf("antigravity: no refresh_token available for token refresh")
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
		return fmt.Errorf("antigravity: client_id and client_secret are required for token refresh (set GEMINI_CLI_OAUTH_CLIENT_ID / GEMINI_CLI_OAUTH_CLIENT_SECRET)")
	}

	formBody := strings.NewReader(fmt.Sprintf(
		"client_id=%s&client_secret=%s&refresh_token=%s&grant_type=refresh_token",
		clientID,
		clientSecret,
		a.creds.RefreshToken,
	))

	req, err := http.NewRequest(http.MethodPost, googleTokenEndpoint, formBody)
	if err != nil {
		return fmt.Errorf("antigravity: creating token refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("antigravity: executing token refresh request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("antigravity: reading token refresh response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("antigravity: token refresh returned HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(respBytes, &tokenResp); err != nil {
		return fmt.Errorf("antigravity: parsing token refresh response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return fmt.Errorf("antigravity: token refresh response missing access_token")
	}

	a.creds.AccessToken = tokenResp.AccessToken
	if tokenResp.ExpiresIn > 0 {
		a.expiryTime = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	} else {
		a.expiryTime = time.Now().Add(1 * time.Hour)
	}

	// Persist the refreshed token back to disk when file-based credentials are used.
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

// accessToken returns the current access token, refreshing it first if necessary.
func (a *AntigravityAdapter) accessToken() (string, error) {
	if a.IsExpiryNear() {
		if err := a.RefreshToken(); err != nil {
			return "", err
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.creds == nil || a.creds.AccessToken == "" {
		return "", fmt.Errorf("antigravity: no access token available")
	}
	return a.creds.AccessToken, nil
}

// GenerateContent sends a non-streaming request to the Antigravity endpoint
// and returns the parsed JSON response as a map.
func (a *AntigravityAdapter) GenerateContent(model string, requestBody map[string]interface{}) (map[string]interface{}, error) {
	token, err := a.accessToken()
	if err != nil {
		return nil, err
	}

	body := make(map[string]interface{}, len(requestBody)+1)
	for k, v := range requestBody {
		body[k] = v
	}
	body["model"] = model
	delete(body, "stream")

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("antigravity: marshalling request body: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, a.baseURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("antigravity: creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("antigravity: executing request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("antigravity: reading response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("antigravity: API returned HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return nil, fmt.Errorf("antigravity: parsing response JSON: %w", err)
	}
	return result, nil
}

// GenerateContentStream sends a streaming request to the Antigravity endpoint
// and returns a channel of StreamChunk values. Each chunk carries the raw SSE
// data payload bytes. The channel is closed when the stream ends or an error
// is encountered.
func (a *AntigravityAdapter) GenerateContentStream(model string, requestBody map[string]interface{}) (<-chan StreamChunk, error) {
	token, err := a.accessToken()
	if err != nil {
		return nil, err
	}

	body := make(map[string]interface{}, len(requestBody)+2)
	for k, v := range requestBody {
		body[k] = v
	}
	body["model"] = model
	body["stream"] = true

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("antigravity: marshalling streaming request body: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, a.baseURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("antigravity: creating streaming request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("antigravity: executing streaming request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		bodyData, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("antigravity: streaming API returned HTTP %d: %s", resp.StatusCode, string(bodyData))
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
			ch <- StreamChunk{Error: fmt.Errorf("antigravity: reading SSE stream: %w", err)}
		} else {
			ch <- StreamChunk{Done: true}
		}
	}()

	return ch, nil
}

// ListModels returns a hardcoded list of Antigravity model names.
func (a *AntigravityAdapter) ListModels() (interface{}, error) {
	models := []map[string]string{
		{"id": "antigravity-pro", "object": "model", "owned_by": "google-deepmind"},
		{"id": "antigravity-flash", "object": "model", "owned_by": "google-deepmind"},
		{"id": "antigravity-ultra", "object": "model", "owned_by": "google-deepmind"},
		{"id": "antigravity-nano", "object": "model", "owned_by": "google-deepmind"},
	}
	return map[string]interface{}{
		"object": "list",
		"data":   models,
	}, nil
}
