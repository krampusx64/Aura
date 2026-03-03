package tools

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ExecuteDDGSearch performs a DuckDuckGo HTML search
func ExecuteDDGSearch(query string, maxResults int) string {
	if maxResults <= 0 {
		maxResults = 5
	}

	client := &http.Client{Timeout: 10 * time.Second}

	// Create form data for DDG Lite
	formData := url.Values{}
	formData.Set("q", query)

	req, err := http.NewRequest("POST", "https://lite.duckduckgo.com/lite/", strings.NewReader(formData.Encode()))
	if err != nil {
		return formatError(fmt.Sprintf("Failed to create request: %v", err))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return formatError(fmt.Sprintf("DDG request failed: %v", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return formatError(fmt.Sprintf("DDG HTTP Error %d", resp.StatusCode))
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return formatError(fmt.Sprintf("Failed to read DDG response: %v", err))
	}
	htmlStr := string(bodyBytes)

	// Basic regex extraction for DuckDuckGo Lite results
	// <tr><td><a class="result-url" href="...">Title</a></td></tr>
	// <tr><td class="result-snippet">Snippet</td></tr>

	titleLinkRe := regexp.MustCompile(`(?is)<a class="result-url" href="([^"]+)">(.*?)</a>`)
	snippetRe := regexp.MustCompile(`(?is)<td class="result-snippet">(.*?)</td>`)

	titleMatches := titleLinkRe.FindAllStringSubmatch(htmlStr, -1)
	snippetMatches := snippetRe.FindAllStringSubmatch(htmlStr, -1)

	var results []map[string]interface{}
	limit := len(titleMatches)
	if limit > maxResults {
		limit = maxResults
	}
	if limit > len(snippetMatches) {
		limit = len(snippetMatches)
	}

	for i := 0; i < limit; i++ {
		link := titleMatches[i][1]
		title := stripHTML(titleMatches[i][2])
		snippet := stripHTML(snippetMatches[i][1])

		results = append(results, map[string]interface{}{
			"title":   fmt.Sprintf("<external_data>%s</external_data>", title),
			"link":    link,
			"snippet": fmt.Sprintf("<external_data>%s</external_data>", snippet),
		})
	}

	resultMap := map[string]interface{}{
		"status":  "success",
		"results": results,
	}
	b, _ := json.Marshal(resultMap)
	return string(b)
}

func stripHTML(htmlStr string) string {
	tagRe := regexp.MustCompile(`(?is)<[^>]+>`)
	text := tagRe.ReplaceAllString(htmlStr, " ")
	spaceRe := regexp.MustCompile(`\s+`)
	text = spaceRe.ReplaceAllString(text, " ")
	return strings.TrimSpace(text)
}
