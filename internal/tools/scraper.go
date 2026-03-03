package tools

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ExecuteWebScraper fetches a URL, removes script/style tags, and extracts plain text
func ExecuteWebScraper(url string) string {
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return formatError(fmt.Sprintf("Failed to create request: %v", err))
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return formatError(fmt.Sprintf("Request failed: %v", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return formatError(fmt.Sprintf("HTTP Error %d: %s", resp.StatusCode, resp.Status))
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return formatError(fmt.Sprintf("Failed to read body: %v", err))
	}
	htmlStr := string(bodyBytes)

	// Remove scripts and styles
	scriptRe := regexp.MustCompile(`(?is)<script.*?>.*?</script>`)
	styleRe := regexp.MustCompile(`(?is)<style.*?>.*?</style>`)
	htmlStr = scriptRe.ReplaceAllString(htmlStr, " ")
	htmlStr = styleRe.ReplaceAllString(htmlStr, " ")

	// Remove all other HTML tags
	tagRe := regexp.MustCompile(`(?is)<[^>]+>`)
	textStr := tagRe.ReplaceAllString(htmlStr, " ")

	// Clean up whitespaces
	spaceRe := regexp.MustCompile(`\s+`)
	textStr = spaceRe.ReplaceAllString(textStr, " ")
	textStr = strings.TrimSpace(textStr)

	// Limit to 10k characters
	if len(textStr) > 10000 {
		textStr = textStr[:10000]
	}

	result := map[string]interface{}{
		"status":  "success",
		"content": fmt.Sprintf("<external_data>%s</external_data>", textStr),
	}
	b, _ := json.Marshal(result)
	return string(b)
}

func formatError(msg string) string {
	b, _ := json.Marshal(map[string]interface{}{
		"status":  "error",
		"message": msg,
	})
	return string(b)
}
