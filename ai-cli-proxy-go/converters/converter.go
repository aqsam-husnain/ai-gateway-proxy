package converters

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// ============================================================================
// Constants
// ============================================================================

const (
	ClaudeDefaultMaxTokens    = 8192
	GeminiDefaultMaxTokens    = 65534
	GeminiMaxOutputTokensLimit = 65536
	GeminiDefaultTemperature  = 1.0
	GeminiDefaultTopP         = 0.95
	OpenAIDefaultMaxTokens    = 128000
	OpenAIDefaultTemperature  = 1.0
	OpenAIDefaultTopP         = 0.95
)

// ============================================================================
// Utility helpers
// ============================================================================

// newID generates a pseudo-unique ID suffix using nanosecond timestamp.
// In production you'd use a real UUID library (e.g. github.com/google/uuid).
func newID() string {
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

// getFloat retrieves a float64 from a map, returning the default if absent/zero.
func getFloat(m map[string]interface{}, key string, def float64) float64 {
	if v, ok := m[key]; ok {
		switch x := v.(type) {
		case float64:
			if x != 0 {
				return x
			}
		case int:
			if x != 0 {
				return float64(x)
			}
		case json.Number:
			if f, err := x.Float64(); err == nil && f != 0 {
				return f
			}
		}
	}
	return def
}

// getInt retrieves an int from a map, returning the default if absent/zero.
func getInt(m map[string]interface{}, key string, def int) int {
	if v, ok := m[key]; ok {
		switch x := v.(type) {
		case float64:
			if int(x) != 0 {
				return int(x)
			}
		case int:
			if x != 0 {
				return x
			}
		case json.Number:
			if i, err := x.Int64(); err == nil && i != 0 {
				return int(i)
			}
		}
	}
	return def
}

// getString retrieves a string value from a map.
func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// getBool retrieves a bool from a map.
func getBool(m map[string]interface{}, key string) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// getSlice retrieves a []interface{} from a map.
func getSlice(m map[string]interface{}, key string) []interface{} {
	if v, ok := m[key]; ok {
		if s, ok := v.([]interface{}); ok {
			return s
		}
	}
	return nil
}

// getMap retrieves a map[string]interface{} from a map.
func getMap(m map[string]interface{}, key string) map[string]interface{} {
	if v, ok := m[key]; ok {
		if mm, ok := v.(map[string]interface{}); ok {
			return mm
		}
	}
	return nil
}

// deepCopy performs a shallow-safe copy via JSON round-trip.
func deepCopy(src map[string]interface{}) map[string]interface{} {
	if src == nil {
		return nil
	}
	b, _ := json.Marshal(src)
	var dst map[string]interface{}
	_ = json.Unmarshal(b, &dst)
	return dst
}

// ============================================================================
// Protocol prefix extraction
// ============================================================================

// ExtractProtocolPrefix extracts the protocol prefix from a provider type string.
// E.g., "gemini-cli-oauth" -> "gemini", "openaiResponses-custom" -> "openaiResponses".
func ExtractProtocolPrefix(providerType string) string {
	idx := strings.Index(providerType, "-")
	if idx == -1 {
		return providerType
	}
	return providerType[:idx]
}

// ============================================================================
// Request conversion helpers — OpenAI -> Gemini
// ============================================================================

// extractOpenAISystemMessages separates system messages from the rest.
func extractOpenAISystemMessages(messages []interface{}) (systemText string, nonSystem []interface{}) {
	var sysParts []string
	for _, raw := range messages {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		role := getString(msg, "role")
		if role == "system" {
			sysParts = append(sysParts, extractTextFromContent(msg["content"]))
		} else {
			nonSystem = append(nonSystem, msg)
		}
	}
	systemText = strings.Join(sysParts, "\n")
	return
}

// extractTextFromContent converts OpenAI content (string or array) to plain text.
func extractTextFromContent(content interface{}) string {
	switch c := content.(type) {
	case string:
		return c
	case []interface{}:
		var parts []string
		for _, item := range c {
			if m, ok := item.(map[string]interface{}); ok {
				if getString(m, "type") == "text" {
					parts = append(parts, getString(m, "text"))
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// openAIContentToGeminiParts converts OpenAI message content to Gemini parts.
func openAIContentToGeminiParts(content interface{}) []interface{} {
	if content == nil {
		return []interface{}{}
	}
	switch c := content.(type) {
	case string:
		if c == "" {
			return []interface{}{}
		}
		return []interface{}{map[string]interface{}{"text": c}}
	case []interface{}:
		var parts []interface{}
		for _, item := range c {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			itemType := getString(m, "type")
			switch itemType {
			case "text":
				if t := getString(m, "text"); t != "" {
					parts = append(parts, map[string]interface{}{"text": t})
				}
			case "image_url":
				if iu := m["image_url"]; iu != nil {
					var imageURL string
					switch v := iu.(type) {
					case string:
						imageURL = v
					case map[string]interface{}:
						imageURL = getString(v, "url")
					}
					if strings.HasPrefix(imageURL, "data:") {
						idx := strings.Index(imageURL, ",")
						if idx > 0 {
							header := imageURL[:idx]
							data := imageURL[idx+1:]
							mimeType := "image/jpeg"
							if matches := strings.TrimPrefix(header, "data:"); matches != "" {
								if i := strings.Index(matches, ";"); i > 0 {
									mimeType = matches[:i]
								}
							}
							parts = append(parts, map[string]interface{}{
								"inlineData": map[string]interface{}{
									"mimeType": mimeType,
									"data":     data,
								},
							})
						}
					} else {
						parts = append(parts, map[string]interface{}{
							"fileData": map[string]interface{}{
								"mimeType": "image/jpeg",
								"fileUri":  imageURL,
							},
						})
					}
				}
			}
		}
		return parts
	}
	return []interface{}{}
}

// ============================================================================
// OpenAIToGemini — convert OpenAI chat request to Gemini generateContent format
// ============================================================================

// OpenAIToGemini converts an OpenAI chat completion request to Gemini generateContent format.
func OpenAIToGemini(req map[string]interface{}) map[string]interface{} {
	messages := getSlice(req, "messages")
	model := getString(req, "model")

	systemText, nonSystem := extractOpenAISystemMessages(messages)

	// Build contents
	var contents []interface{}
	type pending struct {
		role  string
		parts []interface{}
	}
	var last *pending

	for _, raw := range nonSystem {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		role := getString(msg, "role")
		geminiRole := role
		if role == "assistant" {
			geminiRole = "model"
		}

		if role == "tool" {
			// Flush last pending
			if last != nil {
				contents = append(contents, map[string]interface{}{
					"role":  last.role,
					"parts": last.parts,
				})
				last = nil
			}
			funcName := getString(msg, "name")
			if funcName == "" {
				funcName = "unknown"
			}
			contentStr := extractTextFromContent(msg["content"])
			contents = append(contents, map[string]interface{}{
				"role": "user",
				"parts": []interface{}{
					map[string]interface{}{
						"functionResponse": map[string]interface{}{
							"name": funcName,
							"response": map[string]interface{}{
								"name":    funcName,
								"content": contentStr,
							},
						},
					},
				},
			})
			continue
		}

		parts := openAIContentToGeminiParts(msg["content"])

		// Handle tool_calls in assistant messages
		if toolCalls := getSlice(msg, "tool_calls"); len(toolCalls) > 0 {
			for _, tc := range toolCalls {
				if tcMap, ok := tc.(map[string]interface{}); ok {
					if fn := getMap(tcMap, "function"); fn != nil {
						argsRaw := fn["arguments"]
						var argsObj interface{}
						switch v := argsRaw.(type) {
						case string:
							var parsed interface{}
							if err := json.Unmarshal([]byte(v), &parsed); err == nil {
								argsObj = parsed
							} else {
								argsObj = v
							}
						default:
							argsObj = v
						}
						parts = append(parts, map[string]interface{}{
							"functionCall": map[string]interface{}{
								"name": getString(fn, "name"),
								"args": argsObj,
							},
						})
					}
				}
			}
		}

		if len(parts) == 0 {
			continue
		}

		// Merge adjacent same-role text-only messages
		allText := true
		for _, p := range parts {
			pm, ok := p.(map[string]interface{})
			if !ok || pm["text"] == nil {
				allText = false
				break
			}
		}
		if last != nil && last.role == geminiRole && allText {
			lastAllText := true
			for _, p := range last.parts {
				pm, ok := p.(map[string]interface{})
				if !ok || pm["text"] == nil {
					lastAllText = false
					break
				}
			}
			if lastAllText {
				last.parts = append(last.parts, parts...)
				continue
			}
		}

		if last != nil {
			contents = append(contents, map[string]interface{}{
				"role":  last.role,
				"parts": last.parts,
			})
		}
		last = &pending{role: geminiRole, parts: parts}
	}
	if last != nil {
		contents = append(contents, map[string]interface{}{
			"role":  last.role,
			"parts": last.parts,
		})
	}

	result := map[string]interface{}{
		"contents": contents,
		"model":    model,
	}

	// System instruction
	if systemText != "" {
		result["systemInstruction"] = map[string]interface{}{
			"parts": []interface{}{map[string]interface{}{"text": systemText}},
		}
	}

	// Generation config
	genConfig := buildGeminiGenerationConfig(req)
	if len(genConfig) > 0 {
		result["generationConfig"] = genConfig
	}

	// Tools
	if tools := getSlice(req, "tools"); len(tools) > 0 {
		var funcDecls []interface{}
		for _, t := range tools {
			tm, ok := t.(map[string]interface{})
			if !ok {
				continue
			}
			fn := getMap(tm, "function")
			if fn == nil {
				continue
			}
			params := fn["parameters"]
			if params == nil {
				params = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
			}
			funcDecls = append(funcDecls, map[string]interface{}{
				"name":        getString(fn, "name"),
				"description": getString(fn, "description"),
				"parameters":  cleanJSONSchema(params),
			})
		}
		if len(funcDecls) > 0 {
			result["tools"] = []interface{}{
				map[string]interface{}{"functionDeclarations": funcDecls},
			}
		}
	}

	// Tool config
	if toolChoice := req["tool_choice"]; toolChoice != nil {
		tc := buildGeminiToolConfig(toolChoice)
		if tc != nil {
			result["toolConfig"] = tc
		}
	}

	return result
}

func buildGeminiGenerationConfig(req map[string]interface{}) map[string]interface{} {
	config := map[string]interface{}{}

	config["temperature"] = getFloat(req, "temperature", GeminiDefaultTemperature)
	maxTokens := getInt(req, "max_tokens", GeminiDefaultMaxTokens)
	if maxTokens > GeminiMaxOutputTokensLimit {
		maxTokens = GeminiMaxOutputTokensLimit
	}
	config["maxOutputTokens"] = maxTokens
	config["topP"] = getFloat(req, "top_p", GeminiDefaultTopP)

	if stop, ok := req["stop"]; ok {
		switch s := stop.(type) {
		case string:
			config["stopSequences"] = []interface{}{s}
		case []interface{}:
			config["stopSequences"] = s
		}
	}

	if rf := getMap(req, "response_format"); rf != nil {
		rfType := getString(rf, "type")
		if rfType == "json_object" {
			config["responseMimeType"] = "application/json"
		} else if rfType == "json_schema" {
			config["responseMimeType"] = "application/json"
			if js := getMap(rf, "json_schema"); js != nil {
				if schema := js["schema"]; schema != nil {
					config["responseSchema"] = schema
				}
			}
		}
	}

	// Gemini 2.5 / thinking models need responseModalities
	model := getString(req, "model")
	hasTools := len(getSlice(req, "tools")) > 0
	if !hasTools && model != "" &&
		(strings.Contains(model, "2.5") || strings.Contains(model, "thinking") || strings.Contains(model, "2.0-flash-thinking")) {
		config["responseModalities"] = []interface{}{"TEXT"}
	}

	return config
}

func buildGeminiToolConfig(toolChoice interface{}) interface{} {
	switch tc := toolChoice.(type) {
	case string:
		if tc == "none" || tc == "auto" {
			return map[string]interface{}{
				"functionCallingConfig": map[string]interface{}{
					"mode": strings.ToUpper(tc),
				},
			}
		}
	case map[string]interface{}:
		if fn := getMap(tc, "function"); fn != nil {
			return map[string]interface{}{
				"functionCallingConfig": map[string]interface{}{
					"mode":                 "ANY",
					"allowedFunctionNames": []interface{}{getString(fn, "name")},
				},
			}
		}
	}
	return nil
}

func cleanJSONSchema(schema interface{}) interface{} {
	m, ok := schema.(map[string]interface{})
	if !ok {
		return schema
	}
	allowed := map[string]bool{"type": true, "description": true, "properties": true, "required": true, "enum": true, "items": true}
	sanitized := map[string]interface{}{}
	for k, v := range m {
		if allowed[k] {
			sanitized[k] = v
		}
	}
	if props, ok := sanitized["properties"].(map[string]interface{}); ok {
		cleaned := map[string]interface{}{}
		for k, v := range props {
			cleaned[k] = cleanJSONSchema(v)
		}
		sanitized["properties"] = cleaned
	}
	if items, ok := sanitized["items"]; ok {
		sanitized["items"] = cleanJSONSchema(items)
	}
	return sanitized
}

// ============================================================================
// OpenAIToClaude — convert OpenAI chat request to Claude messages format
// ============================================================================

// OpenAIToClaude converts an OpenAI chat completion request to Claude messages format.
func OpenAIToClaude(req map[string]interface{}) map[string]interface{} {
	messages := getSlice(req, "messages")
	systemText, nonSystem := extractOpenAISystemMessages(messages)

	var claudeMessages []interface{}

	for _, raw := range nonSystem {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		role := getString(msg, "role")

		if role == "tool" {
			// Tool result
			toolUseID := getString(msg, "tool_call_id")
			contentRaw := msg["content"]
			var parsedContent interface{}
			if s, ok := contentRaw.(string); ok {
				var p interface{}
				if err := json.Unmarshal([]byte(s), &p); err == nil {
					parsedContent = p
				} else {
					parsedContent = s
				}
			} else {
				parsedContent = contentRaw
			}
			claudeMessages = append(claudeMessages, map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{
						"type":        "tool_result",
						"tool_use_id": toolUseID,
						"content":     parsedContent,
					},
				},
			})
			continue
		}

		// Assistant with tool_calls
		if role == "assistant" {
			if toolCalls := getSlice(msg, "tool_calls"); len(toolCalls) > 0 {
				var toolUseBlocks []interface{}
				for _, tc := range toolCalls {
					tcMap, ok := tc.(map[string]interface{})
					if !ok {
						continue
					}
					fn := getMap(tcMap, "function")
					if fn == nil {
						continue
					}
					argsRaw := fn["arguments"]
					var argsObj interface{}
					switch v := argsRaw.(type) {
					case string:
						var parsed interface{}
						if err := json.Unmarshal([]byte(v), &parsed); err == nil {
							argsObj = parsed
						} else {
							argsObj = map[string]interface{}{}
						}
					default:
						argsObj = v
					}
					toolUseBlocks = append(toolUseBlocks, map[string]interface{}{
						"type":  "tool_use",
						"id":    getString(tcMap, "id"),
						"name":  getString(fn, "name"),
						"input": argsObj,
					})
				}
				claudeMessages = append(claudeMessages, map[string]interface{}{
					"role":    "assistant",
					"content": toolUseBlocks,
				})
				continue
			}
		}

		// Regular message
		claudeRole := role
		if claudeRole != "assistant" {
			claudeRole = "user"
		}
		var content []interface{}
		switch c := msg["content"].(type) {
		case string:
			if c != "" {
				content = append(content, map[string]interface{}{"type": "text", "text": strings.TrimSpace(c)})
			}
		case []interface{}:
			for _, item := range c {
				im, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				switch getString(im, "type") {
				case "text":
					if t := getString(im, "text"); t != "" {
						content = append(content, map[string]interface{}{"type": "text", "text": strings.TrimSpace(t)})
					}
				case "image_url":
					if iu := im["image_url"]; iu != nil {
						var imageURL string
						switch v := iu.(type) {
						case string:
							imageURL = v
						case map[string]interface{}:
							imageURL = getString(v, "url")
						}
						if strings.HasPrefix(imageURL, "data:") {
							idx := strings.Index(imageURL, ",")
							if idx > 0 {
								header := imageURL[:idx]
								data := imageURL[idx+1:]
								mimeType := "image/jpeg"
								h := strings.TrimPrefix(header, "data:")
								if i := strings.Index(h, ";"); i > 0 {
									mimeType = h[:i]
								}
								content = append(content, map[string]interface{}{
									"type": "image",
									"source": map[string]interface{}{
										"type":       "base64",
										"media_type": mimeType,
										"data":       data,
									},
								})
							}
						} else {
							content = append(content, map[string]interface{}{"type": "text", "text": "[Image: " + imageURL + "]"})
						}
					}
				}
			}
		}

		if len(content) > 0 {
			claudeMessages = append(claudeMessages, map[string]interface{}{
				"role":    claudeRole,
				"content": content,
			})
		}
	}

	// Merge adjacent same-role messages
	claudeMessages = mergeAdjacentClaudeMessages(claudeMessages)

	result := map[string]interface{}{
		"model":      getString(req, "model"),
		"messages":   claudeMessages,
		"max_tokens": getInt(req, "max_tokens", ClaudeDefaultMaxTokens),
		"temperature": getFloat(req, "temperature", 1.0),
		"top_p":      getFloat(req, "top_p", 0.95),
	}

	if systemText != "" {
		result["system"] = systemText
	}

	// Tools
	if tools := getSlice(req, "tools"); len(tools) > 0 {
		var claudeTools []interface{}
		for _, t := range tools {
			tm, ok := t.(map[string]interface{})
			if !ok {
				continue
			}
			fn := getMap(tm, "function")
			if fn == nil {
				continue
			}
			params := fn["parameters"]
			if params == nil {
				params = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
			}
			claudeTools = append(claudeTools, map[string]interface{}{
				"name":         getString(fn, "name"),
				"description":  getString(fn, "description"),
				"input_schema": params,
			})
		}
		if len(claudeTools) > 0 {
			result["tools"] = claudeTools
			if tc := req["tool_choice"]; tc != nil {
				result["tool_choice"] = buildClaudeToolChoice(tc)
			}
		}
	}

	return result
}

func mergeAdjacentClaudeMessages(messages []interface{}) []interface{} {
	var merged []interface{}
	for _, raw := range messages {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			merged = append(merged, raw)
			continue
		}
		if len(merged) == 0 {
			merged = append(merged, deepCopy(msg))
			continue
		}
		last, ok := merged[len(merged)-1].(map[string]interface{})
		if !ok {
			merged = append(merged, deepCopy(msg))
			continue
		}
		if getString(last, "role") == getString(msg, "role") {
			// Merge content
			lastContent := getSlice(last, "content")
			newContent := getSlice(msg, "content")
			last["content"] = append(lastContent, newContent...)
		} else {
			merged = append(merged, deepCopy(msg))
		}
	}
	return merged
}

func buildClaudeToolChoice(toolChoice interface{}) interface{} {
	switch tc := toolChoice.(type) {
	case string:
		mapping := map[string]string{"auto": "auto", "none": "none", "required": "any"}
		if t, ok := mapping[tc]; ok {
			return map[string]interface{}{"type": t}
		}
	case map[string]interface{}:
		if fn := getMap(tc, "function"); fn != nil {
			return map[string]interface{}{"type": "tool", "name": getString(fn, "name")}
		}
	}
	return nil
}

// ============================================================================
// GeminiToOpenAI — convert Gemini request to OpenAI format
// ============================================================================

// GeminiToOpenAI converts a Gemini generateContent request to OpenAI chat format.
func GeminiToOpenAI(req map[string]interface{}) map[string]interface{} {
	result := map[string]interface{}{
		"messages":    []interface{}{},
		"model":       getString(req, "model"),
		"max_tokens":  getInt(req, "max_tokens", OpenAIDefaultMaxTokens),
		"temperature": getFloat(req, "temperature", OpenAIDefaultTemperature),
		"top_p":       getFloat(req, "top_p", OpenAIDefaultTopP),
	}

	// Check generationConfig
	if gc := getMap(req, "generationConfig"); gc != nil {
		if mt := getInt(gc, "maxOutputTokens", 0); mt != 0 {
			result["max_tokens"] = mt
		}
		if t := getFloat(gc, "temperature", 0); t != 0 {
			result["temperature"] = t
		}
		if tp := getFloat(gc, "topP", 0); tp != 0 {
			result["top_p"] = tp
		}
	}

	var msgs []interface{}

	// System instruction
	if si := getMap(req, "systemInstruction"); si != nil {
		if parts := getSlice(si, "parts"); len(parts) > 0 {
			var texts []string
			for _, p := range parts {
				pm, ok := p.(map[string]interface{})
				if !ok {
					continue
				}
				if t := getString(pm, "text"); t != "" {
					texts = append(texts, t)
				}
			}
			if len(texts) > 0 {
				msgs = append(msgs, map[string]interface{}{
					"role":    "system",
					"content": strings.Join(texts, "\n"),
				})
			}
		}
	}

	// Contents
	for _, raw := range getSlice(req, "contents") {
		content, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		parts := getSlice(content, "parts")
		role := getString(content, "role")
		if role == "model" {
			role = "assistant"
		}
		openaiContent := geminiPartsToOpenAIContent(parts)
		if openaiContent == nil {
			continue
		}
		msgs = append(msgs, map[string]interface{}{
			"role":    role,
			"content": openaiContent,
		})
	}

	result["messages"] = msgs
	return result
}

func geminiPartsToOpenAIContent(parts []interface{}) interface{} {
	if len(parts) == 0 {
		return nil
	}
	var contentArray []interface{}
	for _, p := range parts {
		pm, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		if t := getString(pm, "text"); t != "" {
			contentArray = append(contentArray, map[string]interface{}{"type": "text", "text": t})
		}
		if inlineData := getMap(pm, "inlineData"); inlineData != nil {
			mt := getString(inlineData, "mimeType")
			data := getString(inlineData, "data")
			contentArray = append(contentArray, map[string]interface{}{
				"type": "image_url",
				"image_url": map[string]interface{}{
					"url": "data:" + mt + ";base64," + data,
				},
			})
		}
	}
	if len(contentArray) == 1 {
		if m, ok := contentArray[0].(map[string]interface{}); ok {
			if getString(m, "type") == "text" {
				return getString(m, "text")
			}
		}
	}
	if len(contentArray) == 0 {
		return nil
	}
	return contentArray
}

// ============================================================================
// ClaudeToOpenAI — convert Claude messages request to OpenAI format
// ============================================================================

// ClaudeToOpenAI converts a Claude messages request to OpenAI chat format.
func ClaudeToOpenAI(req map[string]interface{}) map[string]interface{} {
	var msgs []interface{}
	model := getString(req, "model")

	if systemText := getString(req, "system"); systemText != "" {
		msgs = append(msgs, map[string]interface{}{
			"role":    "system",
			"content": systemText,
		})
	}

	for _, raw := range getSlice(req, "messages") {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		role := getString(msg, "role")

		// Check for tool results (user messages with tool_result blocks)
		if role == "user" {
			if content := getSlice(msg, "content"); len(content) > 0 {
				hasToolResult := false
				for _, item := range content {
					im, ok := item.(map[string]interface{})
					if ok && getString(im, "type") == "tool_result" {
						hasToolResult = true
						break
					}
				}
				if hasToolResult {
					for _, item := range content {
						im, ok := item.(map[string]interface{})
						if !ok || getString(im, "type") != "tool_result" {
							continue
						}
						toolUseID := getString(im, "tool_use_id")
						if toolUseID == "" {
							toolUseID = getString(im, "id")
						}
						var contentStr string
						switch c := im["content"].(type) {
						case string:
							contentStr = c
						default:
							b, _ := json.Marshal(c)
							contentStr = string(b)
						}
						msgs = append(msgs, map[string]interface{}{
							"role":         "tool",
							"tool_call_id": toolUseID,
							"content":      contentStr,
						})
					}
					continue
				}
			}
		}

		// Check for assistant tool_use blocks
		if role == "assistant" {
			if content := getSlice(msg, "content"); len(content) > 0 {
				if first, ok := content[0].(map[string]interface{}); ok && getString(first, "type") == "tool_use" {
					funcName := getString(first, "name")
					funcArgs := first["input"]
					if funcArgs == nil {
						funcArgs = map[string]interface{}{}
					}
					argsBytes, _ := json.Marshal(funcArgs)
					toolCallID := getString(first, "id")
					if toolCallID == "" {
						toolCallID = "call_" + funcName + "_1"
					}
					msgs = append(msgs, map[string]interface{}{
						"role":    "assistant",
						"content": "",
						"tool_calls": []interface{}{
							map[string]interface{}{
								"id":   toolCallID,
								"type": "function",
								"function": map[string]interface{}{
									"name":      funcName,
									"arguments": string(argsBytes),
								},
								"index": 0,
							},
						},
					})
					continue
				}
			}
		}

		// Regular text message
		openaiContent := claudeContentToOpenAIContent(msg["content"])
		if openaiContent != "" || openaiContent != nil {
			msgs = append(msgs, map[string]interface{}{
				"role":    role,
				"content": openaiContent,
			})
		}
	}

	result := map[string]interface{}{
		"model":       model,
		"messages":    msgs,
		"max_tokens":  getInt(req, "max_tokens", OpenAIDefaultMaxTokens),
		"temperature": getFloat(req, "temperature", OpenAIDefaultTemperature),
		"top_p":       getFloat(req, "top_p", OpenAIDefaultTopP),
		"stream":      getBool(req, "stream"),
	}

	// Tools
	if tools := getSlice(req, "tools"); len(tools) > 0 {
		var openaiTools []interface{}
		for _, t := range tools {
			tm, ok := t.(map[string]interface{})
			if !ok {
				continue
			}
			inputSchema := tm["input_schema"]
			if inputSchema == nil {
				inputSchema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
			}
			openaiTools = append(openaiTools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        getString(tm, "name"),
					"description": getString(tm, "description"),
					"parameters":  inputSchema,
				},
			})
		}
		result["tools"] = openaiTools
		result["tool_choice"] = "auto"
	}

	return result
}

func claudeContentToOpenAIContent(content interface{}) interface{} {
	switch c := content.(type) {
	case string:
		return c
	case []interface{}:
		var parts []string
		for _, item := range c {
			im, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if getString(im, "type") == "text" {
				parts = append(parts, getString(im, "text"))
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// ============================================================================
// ConvertRequest dispatcher
// ============================================================================

// ConvertRequest converts a request from one protocol format to another.
// fromProtocol and toProtocol are protocol prefix strings (gemini, openai, claude, openaiResponses, claudeCode).
func ConvertRequest(req map[string]interface{}, fromProtocol, toProtocol string) map[string]interface{} {
	if fromProtocol == toProtocol {
		return deepCopy(req)
	}

	switch fromProtocol {
	case "openai", "openaiResponses":
		switch toProtocol {
		case "gemini":
			return OpenAIToGemini(req)
		case "claude", "claudeCode":
			return OpenAIToClaude(req)
		default:
			return deepCopy(req)
		}
	case "gemini":
		switch toProtocol {
		case "openai", "openaiResponses":
			return GeminiToOpenAI(req)
		case "claude", "claudeCode":
			// Gemini -> Claude: go via OpenAI as intermediate
			openai := GeminiToOpenAI(req)
			return OpenAIToClaude(openai)
		default:
			return deepCopy(req)
		}
	case "claude", "claudeCode":
		switch toProtocol {
		case "openai", "openaiResponses":
			return ClaudeToOpenAI(req)
		case "gemini":
			// Claude -> Gemini: go via OpenAI as intermediate
			openai := ClaudeToOpenAI(req)
			return OpenAIToGemini(openai)
		default:
			return deepCopy(req)
		}
	default:
		return deepCopy(req)
	}
}

// ============================================================================
// Response conversions
// ============================================================================

// GeminiToOpenAIResponse converts a Gemini response to OpenAI chat completion format.
func GeminiToOpenAIResponse(resp map[string]interface{}, model string) map[string]interface{} {
	content := geminiResponseContent(resp)

	usage := map[string]interface{}{
		"prompt_tokens":     0,
		"completion_tokens": 0,
		"total_tokens":      0,
		"cached_tokens":     0,
		"prompt_tokens_details": map[string]interface{}{
			"cached_tokens": 0,
		},
		"completion_tokens_details": map[string]interface{}{
			"reasoning_tokens": 0,
		},
	}
	if um := getMap(resp, "usageMetadata"); um != nil {
		pt := getInt(um, "promptTokenCount", 0)
		ct := getInt(um, "candidatesTokenCount", 0)
		tt := getInt(um, "totalTokenCount", 0)
		cached := getInt(um, "cachedContentTokenCount", 0)
		reasoning := getInt(um, "thoughtsTokenCount", 0)
		usage["prompt_tokens"] = pt
		usage["completion_tokens"] = ct
		usage["total_tokens"] = tt
		usage["cached_tokens"] = cached
		usage["prompt_tokens_details"] = map[string]interface{}{"cached_tokens": cached}
		usage["completion_tokens_details"] = map[string]interface{}{"reasoning_tokens": reasoning}
	}

	return map[string]interface{}{
		"id":      "chatcmpl-" + newID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []interface{}{
			map[string]interface{}{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": "stop",
			},
		},
		"usage": usage,
	}
}

func geminiResponseContent(resp map[string]interface{}) string {
	var parts []string
	for _, rawCand := range getSlice(resp, "candidates") {
		cand, ok := rawCand.(map[string]interface{})
		if !ok {
			continue
		}
		content := getMap(cand, "content")
		if content == nil {
			continue
		}
		for _, rawPart := range getSlice(content, "parts") {
			pm, ok := rawPart.(map[string]interface{})
			if !ok {
				continue
			}
			if t := getString(pm, "text"); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// ClaudeToOpenAIResponse converts a Claude messages response to OpenAI chat completion format.
func ClaudeToOpenAIResponse(resp map[string]interface{}, model string) map[string]interface{} {
	choice := map[string]interface{}{
		"index":         0,
		"finish_reason": "stop",
	}

	if content := getSlice(resp, "content"); len(content) > 0 {
		var toolCalls []interface{}
		var textParts []string
		var reasoningText string

		for _, rawBlock := range content {
			block, ok := rawBlock.(map[string]interface{})
			if !ok {
				continue
			}
			switch getString(block, "type") {
			case "text":
				textParts = append(textParts, getString(block, "text"))
			case "thinking":
				reasoningText = getString(block, "thinking")
			case "tool_use":
				input := block["input"]
				argsBytes, _ := json.Marshal(input)
				toolCalls = append(toolCalls, map[string]interface{}{
					"id":   getString(block, "id"),
					"type": "function",
					"function": map[string]interface{}{
						"name":      getString(block, "name"),
						"arguments": string(argsBytes),
					},
				})
			}
		}

		msg := map[string]interface{}{
			"role":    "assistant",
			"content": strings.Join(textParts, ""),
		}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
			choice["finish_reason"] = "tool_calls"
		}
		if reasoningText != "" {
			msg["reasoning_content"] = reasoningText
		}
		choice["message"] = msg
	} else {
		choice["message"] = map[string]interface{}{
			"role":    "assistant",
			"content": "",
		}
	}

	stopReason := getString(resp, "stop_reason")
	if stopReason == "max_tokens" {
		choice["finish_reason"] = "length"
	} else if stopReason == "end_turn" || stopReason == "" {
		if choice["finish_reason"] == "stop" {
			choice["finish_reason"] = "stop"
		}
	}

	usage := map[string]interface{}{
		"prompt_tokens":     0,
		"completion_tokens": 0,
		"total_tokens":      0,
	}
	if u := getMap(resp, "usage"); u != nil {
		it := getInt(u, "input_tokens", 0)
		ot := getInt(u, "output_tokens", 0)
		usage["prompt_tokens"] = it
		usage["completion_tokens"] = ot
		usage["total_tokens"] = it + ot
		if cached := getInt(u, "cache_read_input_tokens", 0); cached > 0 {
			usage["prompt_tokens_details"] = map[string]interface{}{"cached_tokens": cached}
		}
	}

	return map[string]interface{}{
		"id":      "chatcmpl-" + newID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []interface{}{choice},
		"usage":   usage,
	}
}

// OpenAIToClaudeResponse converts an OpenAI chat completion response to Claude messages format.
func OpenAIToClaudeResponse(resp map[string]interface{}, model string) map[string]interface{} {
	usage := map[string]interface{}{
		"input_tokens":  0,
		"output_tokens": 0,
	}
	if u := getMap(resp, "usage"); u != nil {
		usage["input_tokens"] = getInt(u, "prompt_tokens", 0)
		usage["output_tokens"] = getInt(u, "completion_tokens", 0)
		if pd := getMap(u, "prompt_tokens_details"); pd != nil {
			usage["cache_read_input_tokens"] = getInt(pd, "cached_tokens", 0)
		}
		usage["cache_creation_input_tokens"] = 0
	}

	choices := getSlice(resp, "choices")
	if len(choices) == 0 {
		return map[string]interface{}{
			"id":            "msg_" + newID(),
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         model,
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
			"usage":         usage,
		}
	}

	choice, _ := choices[0].(map[string]interface{})
	message := getMap(choice, "message")
	var contentList []interface{}

	// Tool calls
	if message != nil {
		if toolCalls := getSlice(message, "tool_calls"); len(toolCalls) > 0 {
			for _, tc := range toolCalls {
				tcMap, ok := tc.(map[string]interface{})
				if !ok {
					continue
				}
				fn := getMap(tcMap, "function")
				if fn == nil {
					continue
				}
				argsStr := getString(fn, "arguments")
				var argsObj interface{}
				if err := json.Unmarshal([]byte(argsStr), &argsObj); err != nil {
					argsObj = map[string]interface{}{}
				}
				contentList = append(contentList, map[string]interface{}{
					"type":  "tool_use",
					"id":    getString(tcMap, "id"),
					"name":  getString(fn, "name"),
					"input": argsObj,
				})
			}
		}

		// Reasoning content
		if rc := getString(message, "reasoning_content"); rc != "" {
			contentList = append(contentList, map[string]interface{}{
				"type":     "thinking",
				"thinking": rc,
			})
		}

		// Text content
		if textContent := getString(message, "content"); textContent != "" {
			contentList = append(contentList, map[string]interface{}{
				"type": "text",
				"text": textContent,
			})
		}
	}

	finishReason := "stop"
	if choice != nil {
		finishReason = getString(choice, "finish_reason")
	}
	stopReason := "end_turn"
	switch finishReason {
	case "length":
		stopReason = "max_tokens"
	case "tool_calls":
		stopReason = "tool_use"
	}

	return map[string]interface{}{
		"id":            "msg_" + newID(),
		"type":          "message",
		"role":          "assistant",
		"content":       contentList,
		"model":         model,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         usage,
	}
}

// OpenAIToGeminiResponse converts an OpenAI chat completion response to Gemini generateContent format.
func OpenAIToGeminiResponse(resp map[string]interface{}, model string) map[string]interface{} {
	choices := getSlice(resp, "choices")
	var candidates []interface{}

	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]interface{})
		message := getMap(choice, "message")
		textContent := ""
		if message != nil {
			textContent = getString(message, "content")
		}
		finishReason := "STOP"
		if choice != nil {
			switch getString(choice, "finish_reason") {
			case "length":
				finishReason = "MAX_TOKENS"
			case "content_filter":
				finishReason = "SAFETY"
			}
		}
		candidates = append(candidates, map[string]interface{}{
			"content": map[string]interface{}{
				"role":  "model",
				"parts": []interface{}{map[string]interface{}{"text": textContent}},
			},
			"finishReason": finishReason,
			"index":        0,
		})
	}

	usageMetadata := map[string]interface{}{
		"promptTokenCount":     0,
		"candidatesTokenCount": 0,
		"totalTokenCount":      0,
	}
	if u := getMap(resp, "usage"); u != nil {
		usageMetadata["promptTokenCount"] = getInt(u, "prompt_tokens", 0)
		usageMetadata["candidatesTokenCount"] = getInt(u, "completion_tokens", 0)
		usageMetadata["totalTokenCount"] = getInt(u, "total_tokens", 0)
	}

	return map[string]interface{}{
		"candidates":    candidates,
		"modelVersion":  model,
		"usageMetadata": usageMetadata,
	}
}

// ConvertResponse converts a response from one protocol format to another.
func ConvertResponse(resp map[string]interface{}, fromProtocol, toProtocol string, model string) map[string]interface{} {
	if fromProtocol == toProtocol {
		return deepCopy(resp)
	}

	switch fromProtocol {
	case "gemini":
		switch toProtocol {
		case "openai", "openaiResponses":
			return GeminiToOpenAIResponse(resp, model)
		case "claude", "claudeCode":
			// Gemini -> Claude: go via OpenAI
			openai := GeminiToOpenAIResponse(resp, model)
			return OpenAIToClaudeResponse(openai, model)
		default:
			return deepCopy(resp)
		}
	case "claude", "claudeCode":
		switch toProtocol {
		case "openai", "openaiResponses":
			return ClaudeToOpenAIResponse(resp, model)
		case "gemini":
			openai := ClaudeToOpenAIResponse(resp, model)
			return OpenAIToGeminiResponse(openai, model)
		default:
			return deepCopy(resp)
		}
	case "openai", "openaiResponses":
		switch toProtocol {
		case "claude", "claudeCode":
			return OpenAIToClaudeResponse(resp, model)
		case "gemini":
			return OpenAIToGeminiResponse(resp, model)
		default:
			return deepCopy(resp)
		}
	default:
		return deepCopy(resp)
	}
}

// ============================================================================
// Stream chunk conversions
// ============================================================================

// ConvertStreamChunk converts an SSE chunk from one protocol format to another.
// It returns zero or more output SSE lines (each starting with "data: ").
// Input chunk is a raw SSE line (may start with "data: ").
func ConvertStreamChunk(chunk []byte, fromProtocol, toProtocol, model string) [][]byte {
	if fromProtocol == toProtocol {
		return [][]byte{chunk}
	}

	// Strip "data: " prefix
	raw := bytes.TrimSpace(chunk)
	if bytes.HasPrefix(raw, []byte("data: ")) {
		raw = raw[6:]
	}

	if string(raw) == "[DONE]" {
		return [][]byte{[]byte("data: [DONE]")}
	}

	var chunkData map[string]interface{}
	if err := json.Unmarshal(raw, &chunkData); err != nil {
		// Can't parse - pass through
		return [][]byte{chunk}
	}

	var outputObjects []interface{}

	switch fromProtocol {
	case "gemini":
		// Gemini chunk -> target protocol
		switch toProtocol {
		case "openai", "openaiResponses":
			result := geminiStreamChunkToOpenAI(chunkData, model)
			if result != nil {
				outputObjects = append(outputObjects, result)
			}
		case "claude", "claudeCode":
			result := geminiStreamChunkToClaude(chunkData, model)
			if result != nil {
				switch r := result.(type) {
				case []interface{}:
					outputObjects = append(outputObjects, r...)
				default:
					outputObjects = append(outputObjects, result)
				}
			}
		}
	case "claude", "claudeCode":
		switch toProtocol {
		case "openai", "openaiResponses":
			results := claudeStreamChunkToOpenAI(chunkData, model)
			outputObjects = append(outputObjects, results...)
		case "gemini":
			result := claudeStreamChunkToGemini(chunkData, model)
			if result != nil {
				outputObjects = append(outputObjects, result)
			}
		}
	case "openai", "openaiResponses":
		switch toProtocol {
		case "claude", "claudeCode":
			results := openAIStreamChunkToClaude(chunkData, model)
			outputObjects = append(outputObjects, results...)
		case "gemini":
			result := openAIStreamChunkToGemini(chunkData, model)
			if result != nil {
				outputObjects = append(outputObjects, result)
			}
		}
	}

	if len(outputObjects) == 0 {
		return nil
	}

	var lines [][]byte
	for _, obj := range outputObjects {
		b, err := json.Marshal(obj)
		if err != nil {
			continue
		}
		lines = append(lines, []byte("data: "+string(b)))
	}
	return lines
}

func geminiStreamChunkToOpenAI(chunk map[string]interface{}, model string) interface{} {
	candidates := getSlice(chunk, "candidates")
	if len(candidates) == 0 {
		return nil
	}
	cand, ok := candidates[0].(map[string]interface{})
	if !ok {
		return nil
	}

	content := getMap(cand, "content")
	var textContent string
	var toolCalls []interface{}

	if content != nil {
		for _, rawPart := range getSlice(content, "parts") {
			pm, ok := rawPart.(map[string]interface{})
			if !ok {
				continue
			}
			if t := getString(pm, "text"); t != "" {
				textContent += t
			}
			if fc := getMap(pm, "functionCall"); fc != nil {
				argsRaw := fc["args"]
				argsBytes, _ := json.Marshal(argsRaw)
				toolCalls = append(toolCalls, map[string]interface{}{
					"index": len(toolCalls),
					"id":    "call_" + newID(),
					"type":  "function",
					"function": map[string]interface{}{
						"name":      getString(fc, "name"),
						"arguments": string(argsBytes),
					},
				})
			}
		}
	}

	var finishReason interface{} = nil
	if fr := getString(cand, "finishReason"); fr != "" {
		switch fr {
		case "STOP":
			finishReason = "stop"
		case "MAX_TOKENS":
			finishReason = "length"
		default:
			finishReason = strings.ToLower(fr)
		}
		if len(toolCalls) > 0 && finishReason == "stop" {
			finishReason = "tool_calls"
		}
	}

	delta := map[string]interface{}{}
	if textContent != "" {
		delta["content"] = textContent
	}
	if len(toolCalls) > 0 {
		delta["tool_calls"] = toolCalls
	}

	if len(delta) == 0 && finishReason == nil {
		return nil
	}

	usage := buildGeminiUsage(chunk)

	return map[string]interface{}{
		"id":      "chatcmpl-" + newID(),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []interface{}{
			map[string]interface{}{
				"index":         0,
				"delta":         delta,
				"finish_reason": finishReason,
			},
		},
		"usage": usage,
	}
}

func geminiStreamChunkToClaude(chunk map[string]interface{}, model string) interface{} {
	candidates := getSlice(chunk, "candidates")
	if len(candidates) == 0 {
		return nil
	}
	cand, ok := candidates[0].(map[string]interface{})
	if !ok {
		return nil
	}

	content := getMap(cand, "content")
	if content != nil {
		parts := getSlice(content, "parts")
		var textParts []string
		for _, rawPart := range parts {
			pm, ok := rawPart.(map[string]interface{})
			if !ok {
				continue
			}
			if t := getString(pm, "text"); t != "" {
				textParts = append(textParts, t)
			}
		}
		if len(textParts) > 0 {
			return map[string]interface{}{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]interface{}{
					"type": "text_delta",
					"text": strings.Join(textParts, ""),
				},
			}
		}
	}

	if fr := getString(cand, "finishReason"); fr != "" {
		stopReason := "end_turn"
		if fr == "MAX_TOKENS" {
			stopReason = "max_tokens"
		}
		result := map[string]interface{}{
			"type": "message_delta",
			"delta": map[string]interface{}{
				"stop_reason": stopReason,
			},
		}
		if um := getMap(chunk, "usageMetadata"); um != nil {
			result["usage"] = map[string]interface{}{
				"input_tokens":                getInt(um, "promptTokenCount", 0),
				"output_tokens":               getInt(um, "candidatesTokenCount", 0),
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     getInt(um, "cachedContentTokenCount", 0),
			}
		}
		return result
	}

	return nil
}

func claudeStreamChunkToOpenAI(chunk map[string]interface{}, model string) []interface{} {
	var events []interface{}
	chunkType := getString(chunk, "type")

	switch chunkType {
	case "content_block_delta":
		delta := getMap(chunk, "delta")
		if delta == nil {
			return nil
		}
		deltaType := getString(delta, "type")
		var openaiDelta map[string]interface{}
		switch deltaType {
		case "text_delta":
			openaiDelta = map[string]interface{}{"content": getString(delta, "text")}
		case "thinking_delta":
			openaiDelta = map[string]interface{}{"reasoning_content": getString(delta, "thinking")}
		case "input_json_delta":
			// Tool arguments streaming
			openaiDelta = map[string]interface{}{
				"tool_calls": []interface{}{
					map[string]interface{}{
						"index": getInt(chunk, "index", 0),
						"function": map[string]interface{}{
							"arguments": getString(delta, "partial_json"),
						},
					},
				},
			}
		default:
			return nil
		}
		events = append(events, map[string]interface{}{
			"id":      "chatcmpl-" + newID(),
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []interface{}{
				map[string]interface{}{
					"index":         0,
					"delta":         openaiDelta,
					"finish_reason": nil,
				},
			},
		})

	case "message_delta":
		delta := getMap(chunk, "delta")
		if delta == nil {
			return nil
		}
		stopReason := getString(delta, "stop_reason")
		finishReason := "stop"
		if stopReason == "max_tokens" {
			finishReason = "length"
		} else if stopReason == "tool_use" {
			finishReason = "tool_calls"
		}
		usage := map[string]interface{}{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		}
		if u := getMap(chunk, "usage"); u != nil {
			it := getInt(u, "input_tokens", 0)
			ot := getInt(u, "output_tokens", 0)
			usage["prompt_tokens"] = it
			usage["completion_tokens"] = ot
			usage["total_tokens"] = it + ot
		}
		events = append(events, map[string]interface{}{
			"id":      "chatcmpl-" + newID(),
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []interface{}{
				map[string]interface{}{
					"index":         0,
					"delta":         map[string]interface{}{},
					"finish_reason": finishReason,
				},
			},
			"usage": usage,
		})
	}

	return events
}

func claudeStreamChunkToGemini(chunk map[string]interface{}, model string) interface{} {
	chunkType := getString(chunk, "type")
	if chunkType == "content_block_delta" {
		delta := getMap(chunk, "delta")
		if delta != nil && getString(delta, "type") == "text_delta" {
			return map[string]interface{}{
				"candidates": []interface{}{
					map[string]interface{}{
						"content": map[string]interface{}{
							"role":  "model",
							"parts": []interface{}{map[string]interface{}{"text": getString(delta, "text")}},
						},
					},
				},
			}
		}
	}
	if chunkType == "message_delta" {
		delta := getMap(chunk, "delta")
		if delta != nil {
			stopReason := getString(delta, "stop_reason")
			geminiFinish := "STOP"
			if stopReason == "max_tokens" {
				geminiFinish = "MAX_TOKENS"
			}
			return map[string]interface{}{
				"candidates": []interface{}{
					map[string]interface{}{
						"finishReason": geminiFinish,
					},
				},
			}
		}
	}
	return nil
}

func openAIStreamChunkToClaude(chunk map[string]interface{}, model string) []interface{} {
	var events []interface{}
	choices := getSlice(chunk, "choices")
	if len(choices) == 0 {
		return nil
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return nil
	}

	delta := getMap(choice, "delta")
	finishReason := getString(choice, "finish_reason")

	if delta != nil {
		if rc := getString(delta, "reasoning_content"); rc != "" {
			events = append(events, map[string]interface{}{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]interface{}{
					"type":     "thinking_delta",
					"thinking": rc,
				},
			})
		}
		if content := getString(delta, "content"); content != "" {
			events = append(events, map[string]interface{}{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]interface{}{
					"type": "text_delta",
					"text": content,
				},
			})
		}
	}

	if finishReason != "" {
		stopReason := "end_turn"
		if finishReason == "length" {
			stopReason = "max_tokens"
		} else if finishReason == "tool_calls" {
			stopReason = "tool_use"
		}
		events = append(events, map[string]interface{}{
			"type":  "content_block_stop",
			"index": 0,
		})

		usage := getMap(chunk, "usage")
		usageOut := map[string]interface{}{
			"input_tokens":                0,
			"output_tokens":               0,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     0,
		}
		if usage != nil {
			usageOut["input_tokens"] = getInt(usage, "prompt_tokens", 0)
			usageOut["output_tokens"] = getInt(usage, "completion_tokens", 0)
			if pd := getMap(usage, "prompt_tokens_details"); pd != nil {
				usageOut["cache_read_input_tokens"] = getInt(pd, "cached_tokens", 0)
			}
		}

		events = append(events, map[string]interface{}{
			"type": "message_delta",
			"delta": map[string]interface{}{
				"stop_reason":   stopReason,
				"stop_sequence": nil,
			},
			"usage": usageOut,
		})
		events = append(events, map[string]interface{}{
			"type": "message_stop",
		})
	}

	return events
}

func openAIStreamChunkToGemini(chunk map[string]interface{}, model string) interface{} {
	choices := getSlice(chunk, "choices")
	if len(choices) == 0 {
		return nil
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return nil
	}
	delta := getMap(choice, "delta")
	if delta == nil {
		return nil
	}
	textContent := getString(delta, "content")
	if textContent == "" {
		return nil
	}
	return map[string]interface{}{
		"candidates": []interface{}{
			map[string]interface{}{
				"content": map[string]interface{}{
					"role":  "model",
					"parts": []interface{}{map[string]interface{}{"text": textContent}},
				},
			},
		},
	}
}

func buildGeminiUsage(chunk map[string]interface{}) map[string]interface{} {
	usage := map[string]interface{}{
		"prompt_tokens":     0,
		"completion_tokens": 0,
		"total_tokens":      0,
		"cached_tokens":     0,
		"prompt_tokens_details": map[string]interface{}{
			"cached_tokens": 0,
		},
		"completion_tokens_details": map[string]interface{}{
			"reasoning_tokens": 0,
		},
	}
	if um := getMap(chunk, "usageMetadata"); um != nil {
		pt := getInt(um, "promptTokenCount", 0)
		ct := getInt(um, "candidatesTokenCount", 0)
		tt := getInt(um, "totalTokenCount", 0)
		cached := getInt(um, "cachedContentTokenCount", 0)
		reasoning := getInt(um, "thoughtsTokenCount", 0)
		usage["prompt_tokens"] = pt
		usage["completion_tokens"] = ct
		usage["total_tokens"] = tt
		usage["cached_tokens"] = cached
		usage["prompt_tokens_details"] = map[string]interface{}{"cached_tokens": cached}
		usage["completion_tokens_details"] = map[string]interface{}{"reasoning_tokens": reasoning}
	}
	return usage
}

// ============================================================================
// Error responses
// ============================================================================

// CreateErrorResponse creates an error response in the correct format for the protocol.
func CreateErrorResponse(statusCode int, message, fromProtocol string) map[string]interface{} {
	switch fromProtocol {
	case "claude", "claudeCode":
		errType := "api_error"
		switch statusCode {
		case 400:
			errType = "invalid_request_error"
		case 401:
			errType = "authentication_error"
		case 403:
			errType = "permission_error"
		case 404:
			errType = "not_found_error"
		case 429:
			errType = "rate_limit_error"
		case 529:
			errType = "overloaded_error"
		}
		return map[string]interface{}{
			"type": "error",
			"error": map[string]interface{}{
				"type":    errType,
				"message": message,
			},
		}
	case "gemini":
		return map[string]interface{}{
			"error": map[string]interface{}{
				"code":    statusCode,
				"message": message,
				"status":  httpStatusToGeminiStatus(statusCode),
			},
		}
	default: // openai, openaiResponses
		return map[string]interface{}{
			"error": map[string]interface{}{
				"message": message,
				"type":    "api_error",
				"code":    statusCode,
			},
		}
	}
}

func httpStatusToGeminiStatus(code int) string {
	switch code {
	case 400:
		return "INVALID_ARGUMENT"
	case 401:
		return "UNAUTHENTICATED"
	case 403:
		return "PERMISSION_DENIED"
	case 404:
		return "NOT_FOUND"
	case 429:
		return "RESOURCE_EXHAUSTED"
	case 500:
		return "INTERNAL"
	case 503:
		return "UNAVAILABLE"
	default:
		return "UNKNOWN"
	}
}

// CreateStreamErrorResponse creates an SSE error chunk in the correct format.
func CreateStreamErrorResponse(statusCode int, message, fromProtocol string) string {
	errResp := CreateErrorResponse(statusCode, message, fromProtocol)
	b, _ := json.Marshal(errResp)
	return "data: " + string(b)
}

// ============================================================================
// Helper functions
// ============================================================================

// ExtractModelFromRequest extracts the model name from a request based on protocol.
func ExtractModelFromRequest(req map[string]interface{}, fromProtocol string) string {
	switch fromProtocol {
	case "gemini":
		// In Gemini format the model is part of the URL, but may also be in the body
		return getString(req, "model")
	default:
		return getString(req, "model")
	}
}

// ExtractStreamFlag checks if the request indicates streaming.
func ExtractStreamFlag(req map[string]interface{}, fromProtocol string) bool {
	switch fromProtocol {
	case "gemini":
		// Gemini streaming is determined by the URL action (:streamGenerateContent)
		// The body flag is checked here as a fallback
		return getBool(req, "stream")
	default:
		return getBool(req, "stream")
	}
}

// ApplySystemPrompt applies a system prompt to the request.
// mode can be "append" or "overwrite".
func ApplySystemPrompt(req map[string]interface{}, systemPrompt, mode, toProtocol string) map[string]interface{} {
	if systemPrompt == "" {
		return req
	}

	result := deepCopy(req)

	switch toProtocol {
	case "gemini":
		existing := ""
		if si := getMap(result, "systemInstruction"); si != nil {
			if parts := getSlice(si, "parts"); len(parts) > 0 {
				pm, ok := parts[0].(map[string]interface{})
				if ok {
					existing = getString(pm, "text")
				}
			}
		}
		var finalPrompt string
		if mode == "overwrite" || existing == "" {
			finalPrompt = systemPrompt
		} else {
			finalPrompt = existing + "\n" + systemPrompt
		}
		result["systemInstruction"] = map[string]interface{}{
			"parts": []interface{}{map[string]interface{}{"text": finalPrompt}},
		}

	case "claude", "claudeCode":
		existing := getString(result, "system")
		var finalPrompt string
		if mode == "overwrite" || existing == "" {
			finalPrompt = systemPrompt
		} else {
			finalPrompt = existing + "\n" + systemPrompt
		}
		result["system"] = finalPrompt

	default: // openai, openaiResponses
		messages := getSlice(result, "messages")

		// Find existing system message
		var existingSystem string
		var systemIdx int = -1
		for i, rawMsg := range messages {
			msg, ok := rawMsg.(map[string]interface{})
			if ok && getString(msg, "role") == "system" {
				existingSystem = extractTextFromContent(msg["content"])
				systemIdx = i
				break
			}
		}

		var finalPrompt string
		if mode == "overwrite" || existingSystem == "" {
			finalPrompt = systemPrompt
		} else {
			finalPrompt = existingSystem + "\n" + systemPrompt
		}

		sysMsg := map[string]interface{}{
			"role":    "system",
			"content": finalPrompt,
		}

		if systemIdx >= 0 {
			messages[systemIdx] = sysMsg
		} else {
			messages = append([]interface{}{sysMsg}, messages...)
		}
		result["messages"] = messages
	}

	return result
}

// ============================================================================
// Suppress unused import error for math (used for Inf checks if needed)
// ============================================================================
var _ = math.MaxInt64
