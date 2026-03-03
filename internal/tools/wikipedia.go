package tools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// ExecuteWikipediaSearch queries the Wikipedia REST API for page summaries
func ExecuteWikipediaSearch(query, lang string) string {
	if lang == "" {
		lang = "de"
	}

	// We use the REST API for summary
	apiURL := fmt.Sprintf("https://%s.wikipedia.org/api/rest_v1/page/summary/%s", lang, url.PathEscape(query))

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return formatError(fmt.Sprintf("Failed to create request: %v", err))
	}
	req.Header.Set("User-Agent", "AuraGo/1.0 (Integration)")

	resp, err := client.Do(req)
	if err != nil {
		return formatError(fmt.Sprintf("Wikipedia request failed: %v", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return formatError(fmt.Sprintf("Es wurde kein Wikipedia-Artikel für '%s' gefunden.", query))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return formatError(fmt.Sprintf("Wikipedia HTTP Error %d", resp.StatusCode))
	}

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return formatError(fmt.Sprintf("Failed to parse Wikipedia response: %v", err))
	}

	// Check for disambiguation
	if t, ok := data["type"].(string); ok && t == "disambiguation" {
		return formatError(fmt.Sprintf("Der Begriff '%s' ist mehrdeutig. Bitte präzisiere deine Suche.", query))
	}

	title, _ := data["title"].(string)
	pageURL, _ := data["content_urls"].(map[string]interface{})["desktop"].(map[string]interface{})["page"].(string)
	summary, _ := data["extract"].(string)

	result := map[string]interface{}{
		"status":  "success",
		"title":   fmt.Sprintf("<external_data>%s</external_data>", title),
		"url":     pageURL,
		"summary": fmt.Sprintf("<external_data>%s</external_data>", summary),
		"content": fmt.Sprintf("<external_data>%s</external_data>", summary), // Summary acts as content, limit is well within 15k
	}

	b, _ := json.Marshal(result)
	return string(b)
}
