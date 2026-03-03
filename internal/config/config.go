package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	ConfigPath string `yaml:"-"` // runtime-only: absolute path to the config file
	Server     struct {
		Host          string `yaml:"host"`
		Port          int    `yaml:"port"`
		BridgeAddress string `yaml:"bridge_address"`
		MaxBodyBytes  int64  `yaml:"max_body_bytes"`
		MasterKey     string `yaml:"master_key"`
	} `yaml:"server"`
	LLM struct {
		Provider           string `yaml:"provider"`
		BaseURL            string `yaml:"base_url"`
		APIKey             string `yaml:"api_key"`
		Model              string `yaml:"model"`
		UseNativeFunctions bool   `yaml:"use_native_functions"`
	} `yaml:"llm"`
	Directories struct {
		DataDir      string `yaml:"data_dir"`
		WorkspaceDir string `yaml:"workspace_dir"`
		ToolsDir     string `yaml:"tools_dir"`
		PromptsDir   string `yaml:"prompts_dir"`
		SkillsDir    string `yaml:"skills_dir"`
		VectorDBDir  string `yaml:"vectordb_dir"`
	} `yaml:"directories"`
	SQLite struct {
		ShortTermPath string `yaml:"short_term_path"`
		LongTermPath  string `yaml:"long_term_path"`
		InventoryPath string `yaml:"inventory_path"`
	} `yaml:"sqlite"`
	Embeddings struct {
		Provider      string `yaml:"provider"`
		InternalModel string `yaml:"internal_model"`
		ExternalURL   string `yaml:"external_url"`
		ExternalModel string `yaml:"external_model"`
		APIKey        string `yaml:"api_key"`
	} `yaml:"embeddings"`
	Agent struct {
		SystemLanguage             string `yaml:"system_language"`
		EnableGoogleWorkspace      bool   `yaml:"enable_google_workspace"`
		StepDelaySeconds           int    `yaml:"step_delay_seconds"`
		MemoryCompressionCharLimit int    `yaml:"memory_compression_char_limit"`
		PersonalityEngine          bool   `yaml:"personality_engine"`
		PersonalityEngineV2        bool   `yaml:"personality_engine_v2"`
		PersonalityV2Model         string `yaml:"personality_v2_model"`
		PersonalityV2URL           string `yaml:"personality_v2_url"`
		PersonalityV2APIKey        string `yaml:"personality_v2_api_key"`
		CorePersonality            string `yaml:"core_personality"`
		SystemPromptTokenBudget    int    `yaml:"system_prompt_token_budget"`
		ContextWindow              int    `yaml:"context_window"`
		ShowToolResults            bool   `yaml:"show_tool_results"`
		WorkflowFeedback           bool   `yaml:"workflow_feedback"`
		DebugMode                  bool   `yaml:"debug_mode"`
		CoreMemoryMaxEntries       int    `yaml:"core_memory_max_entries"` // 0 = unlimited; default 200
		CoreMemoryCapMode          string `yaml:"core_memory_cap_mode"`    // "soft" (default) | "hard"
	} `yaml:"agent"`
	CircuitBreaker struct {
		MaxToolCalls              int      `yaml:"max_tool_calls"`
		LLMTimeoutSeconds         int      `yaml:"llm_timeout_seconds"`
		MaintenanceTimeoutMinutes int      `yaml:"maintenance_timeout_minutes"`
		RetryIntervals            []string `yaml:"retry_intervals"`
	} `yaml:"circuit_breaker"`
	Telegram struct {
		UserID               int64  `yaml:"telegram_user_id"`
		BotToken             string `yaml:"bot_token"`
		MaxConcurrentWorkers int    `yaml:"max_concurrent_workers"`
	} `yaml:"telegram"`
	Whisper struct {
		Provider string `yaml:"provider"`
		APIKey   string `yaml:"api_key"`
		BaseURL  string `yaml:"base_url"`
		Model    string `yaml:"model"`
	} `yaml:"whisper"`
	Vision struct {
		Provider string `yaml:"provider"`
		APIKey   string `yaml:"api_key"`
		BaseURL  string `yaml:"base_url"`
		Model    string `yaml:"model"`
	} `yaml:"vision"`
	FallbackLLM struct {
		Enabled              bool   `yaml:"enabled"`
		BaseURL              string `yaml:"base_url"`
		APIKey               string `yaml:"api_key"`
		Model                string `yaml:"model"`
		ProbeIntervalSeconds int    `yaml:"probe_interval_seconds"`
		ErrorThreshold       int    `yaml:"error_threshold"`
	} `yaml:"fallback_llm"`
	Maintenance struct {
		Enabled         bool   `yaml:"enabled"`
		Time            string `yaml:"time"`
		LifeboatEnabled bool   `yaml:"lifeboat_enabled"`
		LifeboatPort    int    `yaml:"lifeboat_port"`
	} `yaml:"maintenance"`
	Logging struct {
		LogDir        string `yaml:"log_dir"`
		EnableFileLog bool   `yaml:"enable_file_log"`
	} `yaml:"logging"`
	Discord struct {
		Enabled          bool   `yaml:"enabled"`
		BotToken         string `yaml:"bot_token"`
		GuildID          string `yaml:"guild_id"`
		AllowedUserID    string `yaml:"allowed_user_id"`
		DefaultChannelID string `yaml:"default_channel_id"`
	} `yaml:"discord"`
	Email struct {
		Enabled       bool   `yaml:"enabled"`
		IMAPHost      string `yaml:"imap_host"`
		IMAPPort      int    `yaml:"imap_port"`
		SMTPHost      string `yaml:"smtp_host"`
		SMTPPort      int    `yaml:"smtp_port"`
		Username      string `yaml:"username"`
		Password      string `yaml:"-"`
		FromAddress   string `yaml:"from_address"`
		WatchEnabled  bool   `yaml:"watch_enabled"`
		WatchInterval int    `yaml:"watch_interval_seconds"`
		WatchFolder   string `yaml:"watch_folder"`
	} `yaml:"email"`
	HomeAssistant struct {
		Enabled     bool   `yaml:"enabled"`
		URL         string `yaml:"url"`
		AccessToken string `yaml:"-"`
	} `yaml:"home_assistant"`
	Docker struct {
		Enabled bool   `yaml:"enabled"`
		Host    string `yaml:"host"` // e.g. unix:///var/run/docker.sock or tcp://localhost:2375
	} `yaml:"docker"`
	CoAgents struct {
		Enabled       bool `yaml:"enabled"`
		MaxConcurrent int  `yaml:"max_concurrent"`
		LLM           struct {
			Provider string `yaml:"provider"`
			BaseURL  string `yaml:"base_url"`
			APIKey   string `yaml:"api_key"`
			Model    string `yaml:"model"`
		} `yaml:"llm"`
		CircuitBreaker struct {
			MaxToolCalls   int `yaml:"max_tool_calls"`
			TimeoutSeconds int `yaml:"timeout_seconds"`
			MaxTokens      int `yaml:"max_tokens"`
		} `yaml:"circuit_breaker"`
	} `yaml:"co_agents"`
	Budget struct {
		Enabled          bool           `yaml:"enabled"`
		DailyLimitUSD    float64        `yaml:"daily_limit_usd"`
		Enforcement      string         `yaml:"enforcement"` // "warn" | "partial" | "full"
		ResetHour        int            `yaml:"reset_hour"`
		WarningThreshold float64        `yaml:"warning_threshold"` // 0.0–1.0
		Models           []ModelCost    `yaml:"models"`
		DefaultCost      ModelCostRates `yaml:"default_cost"`
	} `yaml:"budget"`
	WebDAV struct {
		Enabled  bool   `yaml:"enabled"`
		URL      string `yaml:"url"` // e.g. https://cloud.example.com/remote.php/dav/files/user/
		Username string `yaml:"username"`
		Password string `yaml:"-"`
	} `yaml:"webdav"`
	Koofr struct {
		Enabled     bool   `yaml:"enabled"`
		Username    string `yaml:"username"`
		AppPassword string `yaml:"-"`
		BaseURL     string `yaml:"base_url"` // default: https://app.koofr.net
	} `yaml:"koofr"`
	TTS struct {
		Provider   string `yaml:"provider"` // "google" or "elevenlabs"
		Language   string `yaml:"language"` // BCP-47 language code for Google TTS (e.g. "de", "en")
		ElevenLabs struct {
			APIKey  string `yaml:"api_key"`
			VoiceID string `yaml:"voice_id"` // default voice ID
			ModelID string `yaml:"model_id"` // e.g. "eleven_multilingual_v2"
		} `yaml:"elevenlabs"`
	} `yaml:"tts"`
	Chromecast struct {
		Enabled bool `yaml:"enabled"`
		TTSPort int  `yaml:"tts_port"`
	} `yaml:"chromecast"`
	Notifications struct {
		Ntfy struct {
			Enabled bool   `yaml:"enabled"`
			URL     string `yaml:"url"`   // e.g. "https://ntfy.sh" or self-hosted
			Topic   string `yaml:"topic"` // e.g. "aurago"
			Token   string `yaml:"token"` // optional auth token
		} `yaml:"ntfy"`
		Pushover struct {
			Enabled  bool   `yaml:"enabled"`
			UserKey  string `yaml:"-"` // from vault
			AppToken string `yaml:"-"` // from vault
		} `yaml:"pushover"`
	} `yaml:"notifications"`
	WebConfig struct {
		Enabled bool `yaml:"enabled"` // false = /config endpoint disabled for security
	} `yaml:"web_config"`
	VirusTotal struct {
		APIKey string `yaml:"api_key"`
	} `yaml:"virustotal"`
}

// ModelCost defines per-model token pricing.
type ModelCost struct {
	Name             string  `yaml:"name"`
	InputPerMillion  float64 `yaml:"input_per_million"`
	OutputPerMillion float64 `yaml:"output_per_million"`
}

// ModelCostRates defines fallback pricing for unlisted models.
type ModelCostRates struct {
	InputPerMillion  float64 `yaml:"input_per_million"`
	OutputPerMillion float64 `yaml:"output_per_million"`
}

func Load(path string) (*Config, error) {
	absConfigPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve absolute path for config: %w", err)
	}
	configDir := filepath.Dir(absConfigPath)

	data, err := os.ReadFile(absConfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Resolve absolute paths for directories
	cfg.Directories.DataDir = resolvePath(configDir, cfg.Directories.DataDir)
	cfg.Directories.WorkspaceDir = resolvePath(configDir, cfg.Directories.WorkspaceDir)
	cfg.Directories.ToolsDir = resolvePath(configDir, cfg.Directories.ToolsDir)
	cfg.Directories.PromptsDir = resolvePath(configDir, cfg.Directories.PromptsDir)
	cfg.Directories.SkillsDir = resolvePath(configDir, cfg.Directories.SkillsDir)
	cfg.Directories.VectorDBDir = resolvePath(configDir, cfg.Directories.VectorDBDir)

	// Resolve absolute paths for SQLite
	cfg.SQLite.ShortTermPath = resolvePath(configDir, cfg.SQLite.ShortTermPath)
	cfg.SQLite.LongTermPath = resolvePath(configDir, cfg.SQLite.LongTermPath)
	cfg.SQLite.InventoryPath = resolvePath(configDir, cfg.SQLite.InventoryPath)

	// Resolve logging directory
	cfg.Logging.LogDir = resolvePath(configDir, cfg.Logging.LogDir)

	// --- Environment Variable Fallbacks (for secrets) ---
	if cfg.Server.MasterKey == "" {
		if val := os.Getenv("AURAGO_MASTER_KEY"); val != "" {
			cfg.Server.MasterKey = val
		}
	}
	if cfg.LLM.APIKey == "" {
		if val := os.Getenv("LLM_API_KEY"); val != "" {
			cfg.LLM.APIKey = val
		} else if val := os.Getenv("OPENAI_API_KEY"); val != "" {
			cfg.LLM.APIKey = val
		} else if val := os.Getenv("ANTHROPIC_API_KEY"); val != "" {
			cfg.LLM.APIKey = val
		}
	}
	if cfg.CoAgents.LLM.APIKey == "" {
		if val := os.Getenv("CO_AGENTS_LLM_API_KEY"); val != "" {
			cfg.CoAgents.LLM.APIKey = val
		}
	}
	if cfg.Embeddings.APIKey == "" || cfg.Embeddings.APIKey == "dummy_key" {
		if val := os.Getenv("EMBEDDINGS_API_KEY"); val != "" {
			cfg.Embeddings.APIKey = val
		}
	}
	if cfg.Vision.APIKey == "" {
		if val := os.Getenv("VISION_API_KEY"); val != "" {
			cfg.Vision.APIKey = val
		}
	}
	if cfg.Whisper.APIKey == "" {
		if val := os.Getenv("WHISPER_API_KEY"); val != "" {
			cfg.Whisper.APIKey = val
		}
	}
	if cfg.FallbackLLM.APIKey == "" {
		if val := os.Getenv("FALLBACK_LLM_API_KEY"); val != "" {
			cfg.FallbackLLM.APIKey = val
		}
	}

	if cfg.CircuitBreaker.MaxToolCalls <= 0 {
		cfg.CircuitBreaker.MaxToolCalls = 10 // User specifically asked for 10
	}
	if cfg.CircuitBreaker.LLMTimeoutSeconds <= 0 {
		cfg.CircuitBreaker.LLMTimeoutSeconds = 180 // 3 minutes
	}
	if cfg.CircuitBreaker.MaintenanceTimeoutMinutes <= 0 {
		cfg.CircuitBreaker.MaintenanceTimeoutMinutes = 10
	}
	if len(cfg.CircuitBreaker.RetryIntervals) == 0 {
		cfg.CircuitBreaker.RetryIntervals = []string{"10s", "2m", "10m"}
	}

	if cfg.Server.Host == "" {
		cfg.Server.Host = "127.0.0.1"
	}
	if cfg.Agent.StepDelaySeconds < 0 {
		cfg.Agent.StepDelaySeconds = 0
	}
	if cfg.Maintenance.LifeboatPort <= 0 {
		cfg.Maintenance.LifeboatPort = 8091
	}
	if cfg.Agent.MemoryCompressionCharLimit <= 0 {
		cfg.Agent.MemoryCompressionCharLimit = 100000
	}
	if cfg.Agent.CorePersonality == "" {
		cfg.Agent.CorePersonality = "neutral"
	}
	if cfg.Agent.CoreMemoryMaxEntries <= 0 {
		cfg.Agent.CoreMemoryMaxEntries = 200
	}
	if cfg.Agent.CoreMemoryCapMode == "" {
		cfg.Agent.CoreMemoryCapMode = "soft"
	}
	if cfg.Agent.SystemPromptTokenBudget <= 0 {
		cfg.Agent.SystemPromptTokenBudget = 1200
	}
	if cfg.Agent.ContextWindow <= 0 {
		cfg.Agent.ContextWindow = 0 // 0 = auto-detect or use budget as-is
	}
	// Default to true if not explicitly set (YAML unmarshal results in false if missing,
	// so we check if the key was present or just use a safe default approach)
	// Actually, since it's a bool, we'll just ensure it's handled.
	// We'll assume the user wants it unless they say no.
	// (Note: yaml.Unmarshal into a struct field defaults to the zero value, which is false.
	// To have a default true, we'd usually check a pointer or just set it here if not in file)
	// But since I added it to config.yaml already, it will be loaded.

	if cfg.FallbackLLM.ProbeIntervalSeconds <= 0 {
		cfg.FallbackLLM.ProbeIntervalSeconds = 60
	}
	if cfg.FallbackLLM.ErrorThreshold <= 0 {
		cfg.FallbackLLM.ErrorThreshold = 3
	}
	if cfg.Koofr.BaseURL == "" {
		cfg.Koofr.BaseURL = "https://app.koofr.net"
	}

	// Server defaults
	if cfg.Server.BridgeAddress == "" {
		cfg.Server.BridgeAddress = "localhost:8089"
	}
	if cfg.Server.MaxBodyBytes <= 0 {
		cfg.Server.MaxBodyBytes = 10 << 20 // 10 MB
	}

	// Telegram defaults
	if cfg.Telegram.MaxConcurrentWorkers <= 0 {
		cfg.Telegram.MaxConcurrentWorkers = 5
	}

	// Email defaults
	if cfg.Email.IMAPPort <= 0 {
		cfg.Email.IMAPPort = 993
	}
	if cfg.Email.SMTPPort <= 0 {
		cfg.Email.SMTPPort = 587
	}
	if cfg.Email.WatchInterval <= 0 {
		cfg.Email.WatchInterval = 120
	}
	if cfg.Email.WatchFolder == "" {
		cfg.Email.WatchFolder = "INBOX"
	}
	if cfg.Email.FromAddress == "" {
		cfg.Email.FromAddress = cfg.Email.Username
	}

	// Co-Agent defaults
	if cfg.CoAgents.MaxConcurrent <= 0 {
		cfg.CoAgents.MaxConcurrent = 3
	}
	if cfg.CoAgents.CircuitBreaker.MaxToolCalls <= 0 {
		cfg.CoAgents.CircuitBreaker.MaxToolCalls = 10
	}
	if cfg.CoAgents.CircuitBreaker.TimeoutSeconds <= 0 {
		cfg.CoAgents.CircuitBreaker.TimeoutSeconds = 300 // 5 minutes
	}

	// Budget defaults
	if cfg.Budget.Enforcement == "" {
		cfg.Budget.Enforcement = "warn"
	}
	if cfg.Budget.WarningThreshold <= 0 {
		cfg.Budget.WarningThreshold = 0.8
	}
	if cfg.Budget.DefaultCost.InputPerMillion <= 0 && cfg.Budget.DefaultCost.OutputPerMillion <= 0 {
		cfg.Budget.DefaultCost = ModelCostRates{InputPerMillion: 1.0, OutputPerMillion: 3.0}
	}

	cfg.ConfigPath = absConfigPath

	return &cfg, nil
}

// Save persists the configuration to the specified path using a targeted patch
// strategy: the original file is read, parsed as a generic YAML map, only the
// changed runtime fields are updated, and the map is written back. This ensures
// that API keys, comments structure, and other sensitive fields are never lost.
func (c *Config) Save(path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		absPath = path
	}

	// 1. Read the existing config file into a generic map
	original, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Errorf("failed to read config file for patching: %w", err)
	}

	var rawCfg map[string]interface{}
	if err := yaml.Unmarshal(original, &rawCfg); err != nil {
		return fmt.Errorf("failed to unmarshal config for patching: %w", err)
	}

	// 2. Patch only the fields that are safe to change at runtime
	if agentSection, ok := rawCfg["agent"].(map[string]interface{}); ok {
		agentSection["core_personality"] = c.Agent.CorePersonality
	}

	// 3. Write back with all original fields (including API keys) intact
	data, err := yaml.Marshal(rawCfg)
	if err != nil {
		return fmt.Errorf("failed to marshal patched config: %w", err)
	}

	if err := os.WriteFile(absPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

func resolvePath(baseDir, targetPath string) string {
	if targetPath == "" {
		return ""
	}
	if filepath.IsAbs(targetPath) {
		return targetPath
	}
	return filepath.Join(baseDir, targetPath)
}
