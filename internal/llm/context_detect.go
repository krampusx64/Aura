package llm

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// ModelInfo contains context window information fetched from the provider API.
type ModelInfo struct {
	ContextLength int `json:"context_length"`
}

// DetectContextWindow queries the OpenRouter (or compatible) API for the model's context window size.
// Returns the context length in tokens, or 0 if detection fails.
func DetectContextWindow(baseURL, apiKey, model string, logger *slog.Logger) int {
	// OpenRouter exposes model info at /api/v1/models
	// We query the list and find our model
	modelsURL := strings.TrimSuffix(baseURL, "/v1") + "/api/v1/models"
	// Also try the direct base if it already contains /api
	if strings.Contains(baseURL, "/api/v1") {
		modelsURL = strings.TrimSuffix(baseURL, "/v1") + "/v1/models"
	}

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", modelsURL, nil)
	if err != nil {
		logger.Debug("[ContextDetect] Failed to create request", "error", err)
		return 0
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := client.Do(req)
	if err != nil {
		logger.Debug("[ContextDetect] Failed to query models API", "error", err, "url", modelsURL)
		return 0
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logger.Debug("[ContextDetect] Models API returned non-200", "status", resp.StatusCode)
		return 0
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024)) // 2MB limit
	if err != nil {
		logger.Debug("[ContextDetect] Failed to read models response", "error", err)
		return 0
	}

	// Parse response — OpenRouter returns { "data": [ { "id": "...", "context_length": N, ... } ] }
	var result struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		logger.Debug("[ContextDetect] Failed to parse models response", "error", err)
		return 0
	}

	for _, m := range result.Data {
		if m.ID == model {
			logger.Info("[ContextDetect] Detected model context window", "model", model, "context_length", m.ContextLength)
			return m.ContextLength
		}
	}

	logger.Debug("[ContextDetect] Model not found in API response", "model", model, "total_models", len(result.Data))
	return 0
}

// AutoConfigureBudget sets the system prompt token budget based on the detected context window.
// Budget allocation: 20% system prompt, 50% history, 30% response.
// Only overrides if current budget is the default (1200) and context window was detected.
func AutoConfigureBudget(contextWindow, currentBudget int, logger *slog.Logger) (tokenBudget int, contextWindowOut int) {
	if contextWindow <= 0 {
		return currentBudget, 0
	}

	suggestedBudget := contextWindow * 20 / 100 // 20% for system prompt
	if suggestedBudget < 500 {
		suggestedBudget = 500 // Minimum viable budget
	}
	if suggestedBudget > 8000 {
		suggestedBudget = 8000 // Cap — balances prompt richness vs. history/response space
	}

	logger.Info(fmt.Sprintf("[ContextDetect] Auto-configured: context_window=%d, system_budget=%d (was %d)",
		contextWindow, suggestedBudget, currentBudget))

	return suggestedBudget, contextWindow
}
