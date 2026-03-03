package llm

import (
	"aurago/internal/config"

	"github.com/sashabaranov/go-openai"
)

// NewClient creates a new OpenAI compatible client based on the routing configuration
func NewClient(cfg *config.Config) *openai.Client {
	// Start with default config for the provided API key
	clientConfig := openai.DefaultConfig(cfg.LLM.APIKey)

	// Override the BaseURL if provided in the configuration (crucial for Ollama/OpenRouter)
	if cfg.LLM.BaseURL != "" {
		clientConfig.BaseURL = cfg.LLM.BaseURL
	}

	return openai.NewClientWithConfig(clientConfig)
}
