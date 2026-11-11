package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"piui/internal/store"
)

// ProviderTestResult summarizes an end-to-end connectivity & authentication probe for a ProviderProfile.
type ProviderTestResult struct {
	LatencyMs int64
	Endpoint  string
	ModelID   string
	Message   string
}

// TestProviderConnection verifies the profile's Base URL, API Key, wire protocol format,
// and active ModelID via a minimal 1-token roundtrip (or /models probe if ModelID is unset).
func TestProviderConnection(ctx context.Context, profile store.ProviderProfile) (*ProviderTestResult, error) {
	start := time.Now()
	modelID := strings.TrimSpace(profile.ModelID)
	if modelID == "" && len(profile.Models) > 0 {
		modelID = strings.TrimSpace(profile.Models[0])
	}

	// When no model is selected yet, verify connectivity via /v1/models discovery.
	if modelID == "" {
		models, err := FetchModels(ctx, profile)
		if err != nil {
			return nil, err
		}
		lat := time.Since(start).Milliseconds()
		if lat < 1 {
			lat = 1
		}
		return &ProviderTestResult{
			LatencyMs: lat,
			Message:   fmt.Sprintf("连接成功 (%dms) · 发现 %d 个可用模型", lat, len(models)),
		}, nil
	}

	endpoint, payload, headers, err := buildMinimalChatProbe(profile, modelID)
	if err != nil {
		return nil, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("网络请求失败 (%s): %w", endpoint, err)
	}
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	_ = resp.Body.Close()

	lat := time.Since(start).Milliseconds()
	if lat < 1 {
		lat = 1
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := strings.TrimSpace(string(bodyBytes))
		if len(snippet) > 240 {
			snippet = snippet[:240] + "..."
		}
		if snippet != "" {
			return nil, fmt.Errorf("HTTP %d (%s): %s", resp.StatusCode, endpoint, snippet)
		}
		return nil, fmt.Errorf("HTTP %d (%s)", resp.StatusCode, endpoint)
	}

	return &ProviderTestResult{
		LatencyMs: lat,
		Endpoint:  endpoint,
		ModelID:   modelID,
		Message:   fmt.Sprintf("连接正常 (%dms) · 协议 %s · 模型 %s 验证通过", lat, profile.Format, modelID),
	}, nil
}

func resolveProfileBaseAndKey(profile store.ProviderProfile) (base, apiKey string) {
	base = strings.TrimSpace(profile.BaseURL)
	if base == "" {
		if profile.Format == store.FormatAnthropicMessages {
			base = strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL"))
			if base == "" {
				base = "https://api.anthropic.com"
			}
		} else {
			base = strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
			if base == "" {
				base = "https://api.openai.com/v1"
			}
		}
	}
	base = strings.TrimRight(base, "/")

	apiKey = strings.TrimSpace(profile.APIKey)
	if apiKey == "" {
		if profile.Format == store.FormatAnthropicMessages {
			apiKey = strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
			if apiKey == "" {
				apiKey = strings.TrimSpace(os.Getenv("ANTHROPIC_AUTH_TOKEN"))
			}
		} else {
			apiKey = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
		}
	}
	return base, apiKey
}

func buildMinimalChatProbe(profile store.ProviderProfile, modelID string) (endpoint string, body []byte, headers map[string]string, err error) {
	base, apiKey := resolveProfileBaseAndKey(profile)
	if base == "" {
		return "", nil, nil, fmt.Errorf("无效的 Base URL")
	}
	// Strip accidental trailing endpoint suffix if user pasted full path.
	for _, suffix := range []string{"/chat/completions", "/completions", "/messages", "/responses"} {
		if strings.HasSuffix(strings.ToLower(base), suffix) {
			base = strings.TrimRight(base[:len(base)-len(suffix)], "/")
			break
		}
	}
	lower := strings.ToLower(base)
	hasVer := strings.HasSuffix(lower, "/v1") || strings.HasSuffix(lower, "/v1beta") || strings.HasSuffix(lower, "/v2") || strings.HasSuffix(lower, "/v3") || strings.HasSuffix(lower, "/v4")

	headers = map[string]string{}
	switch profile.Format {
	case store.FormatAnthropicMessages:
		if hasVer {
			endpoint = base + "/messages"
		} else {
			endpoint = base + "/v1/messages"
		}
		headers["anthropic-version"] = "2023-06-01"
		if apiKey != "" {
			headers["x-api-key"] = apiKey
			headers["Authorization"] = "Bearer " + apiKey
		}
		body, err = json.Marshal(map[string]any{
			"model":      modelID,
			"max_tokens": 1,
			"messages": []map[string]string{
				{"role": "user", "content": "ping"},
			},
		})

	case store.FormatOpenAIResponses:
		if hasVer {
			endpoint = base + "/responses"
		} else {
			endpoint = base + "/v1/responses"
		}
		if apiKey != "" {
			headers["Authorization"] = "Bearer " + apiKey
		}
		body, err = json.Marshal(map[string]any{
			"model":             modelID,
			"input":             "ping",
			"max_output_tokens": 16,
		})

	default:
		if hasVer {
			endpoint = base + "/chat/completions"
		} else {
			endpoint = base + "/v1/chat/completions"
		}
		if apiKey != "" {
			headers["Authorization"] = "Bearer " + apiKey
		}
		body, err = json.Marshal(map[string]any{
			"model":      modelID,
			"max_tokens": 1,
			"messages": []map[string]string{
				{"role": "user", "content": "ping"},
			},
		})
	}
	return endpoint, body, headers, err
}

// FetchModels queries the generic OpenAI / Anthropic compatible `/models` (or `/v1/models`)
// endpoint for the given ProviderProfile and returns the list of available model IDs.
func FetchModels(ctx context.Context, profile store.ProviderProfile) ([]string, error) {
	base := strings.TrimSpace(profile.BaseURL)
	if base == "" {
		if profile.Format == store.FormatAnthropicMessages {
			base = strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL"))
			if base == "" {
				base = "https://api.anthropic.com"
			}
		} else {
			base = strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
			if base == "" {
				base = "https://api.openai.com/v1"
			}
		}
	}

	urls := buildModelsEndpointCandidates(base)
	if len(urls) == 0 {
		return nil, fmt.Errorf("无效的 Base URL")
	}

	apiKey := strings.TrimSpace(profile.APIKey)
	if apiKey == "" {
		if profile.Format == store.FormatAnthropicMessages {
			apiKey = strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
			if apiKey == "" {
				apiKey = strings.TrimSpace(os.Getenv("ANTHROPIC_AUTH_TOKEN"))
			}
		} else {
			apiKey = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
		}
	}

	client := &http.Client{Timeout: 15 * time.Second}
	var lastErr error

	for _, endpoint := range urls {
		reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}

		req.Header.Set("Accept", "application/json")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
			if profile.Format == store.FormatAnthropicMessages {
				req.Header.Set("x-api-key", apiKey)
				req.Header.Set("anthropic-version", "2023-06-01")
			}
		} else if profile.Format == store.FormatAnthropicMessages {
			req.Header.Set("anthropic-version", "2023-06-01")
		}

		resp, err := client.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}

		bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
		_ = resp.Body.Close()
		cancel()

		if readErr != nil {
			lastErr = readErr
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			snippet := strings.TrimSpace(string(bodyBytes))
			if len(snippet) > 200 {
				snippet = snippet[:200] + "..."
			}
			if snippet != "" {
				lastErr = fmt.Errorf("HTTP %d (%s): %s", resp.StatusCode, endpoint, snippet)
			} else {
				lastErr = fmt.Errorf("HTTP %d (%s)", resp.StatusCode, endpoint)
			}
			continue
		}

		models, parseErr := parseModelsPayload(bodyBytes)
		if parseErr != nil {
			lastErr = fmt.Errorf("解析 %s 响应失败: %w", endpoint, parseErr)
			continue
		}
		if len(models) == 0 {
			lastErr = fmt.Errorf("接口 %s 返回的模型列表为空", endpoint)
			continue
		}
		return models, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("未能从接口获取到模型列表")
}

func buildModelsEndpointCandidates(rawBase string) []string {
	base := strings.TrimSpace(rawBase)
	base = strings.TrimRight(base, "/")
	if base == "" {
		return nil
	}

	// Strip accidentally included endpoint paths while keeping version prefix if present.
	for _, suffix := range []string{
		"/chat/completions",
		"/completions",
		"/messages",
		"/responses",
	} {
		if strings.HasSuffix(strings.ToLower(base), suffix) {
			base = strings.TrimRight(base[:len(base)-len(suffix)], "/")
			break
		}
	}

	lower := strings.ToLower(base)
	if strings.HasSuffix(lower, "/models") {
		return []string{base}
	}

	hasVersionSuffix := false
	for _, ver := range []string{"/v1", "/v1beta", "/v2", "/v3", "/v4"} {
		if strings.HasSuffix(lower, ver) {
			hasVersionSuffix = true
			break
		}
	}

	if hasVersionSuffix {
		return []string{base + "/models"}
	}
	return []string{
		base + "/v1/models",
		base + "/models",
	}
}

func parseModelsPayload(raw []byte) ([]string, error) {
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var result []string
	addID := func(s string) {
		s = strings.TrimSpace(s)
		s = strings.TrimPrefix(s, "models/")
		if s != "" && !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}

	extractList := func(items []any) {
		for _, item := range items {
			switch v := item.(type) {
			case string:
				addID(v)
			case map[string]any:
				if id, ok := v["id"].(string); ok && strings.TrimSpace(id) != "" {
					addID(id)
				} else if name, ok := v["name"].(string); ok && strings.TrimSpace(name) != "" {
					addID(name)
				} else if model, ok := v["model"].(string); ok && strings.TrimSpace(model) != "" {
					addID(model)
				}
			}
		}
	}

	switch root := parsed.(type) {
	case []any:
		extractList(root)
	case map[string]any:
		if data, ok := root["data"].([]any); ok {
			extractList(data)
		} else if models, ok := root["models"].([]any); ok {
			extractList(models)
		} else if items, ok := root["items"].([]any); ok {
			extractList(items)
		}
	default:
		return nil, fmt.Errorf("未知的响应格式")
	}

	return result, nil
}
