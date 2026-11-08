package harness

import (
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
