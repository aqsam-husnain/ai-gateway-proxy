package handlers

import (
	"encoding/json"
	"strings"
)

// extractModelNames extracts a flat []string of model ID strings from the
// interface{} value returned by ServiceAdapter.ListModels().  It handles the
// common response shapes:
//
//   - OpenAI ModelList: {"object":"list","data":[{"id":"model-id",...},...]}
//   - Gemini model list: {"models":[{"name":"models/gemini-..."},...]}
//   - Plain []string of IDs
//   - []map[string]interface{} with an "id" key
func extractModelNames(raw interface{}) []string {
	if raw == nil {
		return nil
	}
	// JSON round-trip so we can work with a generic structure.
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}

	// Try OpenAI format first: {"data":[{"id":"..."},...]}
	var openaiList struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &openaiList); err == nil && len(openaiList.Data) > 0 {
		out := make([]string, 0, len(openaiList.Data))
		for _, d := range openaiList.Data {
			if d.ID != "" {
				out = append(out, d.ID)
			}
		}
		return out
	}

	// Try Gemini format: {"models":[{"name":"models/gemini-..."},...]}
	var geminiList struct {
		Models []struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &geminiList); err == nil && len(geminiList.Models) > 0 {
		out := make([]string, 0, len(geminiList.Models))
		for _, m := range geminiList.Models {
			id := strings.TrimPrefix(m.Name, "models/")
			if id == "" {
				id = m.DisplayName
			}
			if id != "" {
				out = append(out, id)
			}
		}
		return out
	}

	// Try plain []string.
	var ids []string
	if err := json.Unmarshal(data, &ids); err == nil {
		return ids
	}

	// Try []interface{} where each element may be a string or a map with "id".
	var slice []interface{}
	if err := json.Unmarshal(data, &slice); err == nil {
		out := make([]string, 0, len(slice))
		for _, item := range slice {
			switch v := item.(type) {
			case string:
				out = append(out, v)
			case map[string]interface{}:
				if id, ok := v["id"].(string); ok && id != "" {
					out = append(out, id)
				}
			}
		}
		return out
	}

	return nil
}
