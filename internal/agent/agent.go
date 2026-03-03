package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"aurago/internal/budget"
	"aurago/internal/config"
	"aurago/internal/inventory"
	"aurago/internal/llm"
	loggerPkg "aurago/internal/logger"
	"aurago/internal/memory"
	"aurago/internal/prompts"
	"aurago/internal/remote"
	"aurago/internal/security"
	"aurago/internal/services"
	"aurago/internal/tools"

	"github.com/sashabaranov/go-openai"
)

// Agent encapsulates the agent's dependencies and state.
type Agent struct {
	Cfg          *config.Config
	Logger       *slog.Logger
	ShortTermMem *memory.SQLiteMemory
	LongTermMem  memory.VectorDB
	Vault        *security.Vault
	Registry     *tools.ProcessRegistry
	CronManager  *tools.CronManager
	KG           *memory.KnowledgeGraph
	InventoryDB  *sql.DB
}

// NewAgent creates a new Agent instance.
func NewAgent(cfg *config.Config, logger *slog.Logger, stm *memory.SQLiteMemory, ltm memory.VectorDB, vault *security.Vault, registry *tools.ProcessRegistry, cron *tools.CronManager, kg *memory.KnowledgeGraph, inventoryDB *sql.DB) *Agent {
	return &Agent{
		Cfg:          cfg,
		Logger:       logger,
		ShortTermMem: stm,
		LongTermMem:  ltm,
		Vault:        vault,
		Registry:     registry,
		CronManager:  cron,
		KG:           kg,
		InventoryDB:  inventoryDB,
	}
}

// Shutdown ensures all agent resources are released properly.
func (a *Agent) Shutdown() error {
	a.Logger.Info("Agent shutdown initiated...")

	if a.ShortTermMem != nil {
		if err := a.ShortTermMem.Close(); err != nil {
			a.Logger.Error("Failed to close SQLite memory", "error", err)
		}
	}

	if a.LongTermMem != nil {
		if err := a.LongTermMem.Close(); err != nil {
			a.Logger.Error("Failed to close VectorDB", "error", err)
		}
	}

	if a.KG != nil {
		if err := a.KG.Close(); err != nil {
			a.Logger.Error("Failed to close Knowledge Graph", "error", err)
		}
	}

	a.Logger.Info("Agent shutdown completed.")
	return nil
}

// FeedbackBroker provides an abstraction for real-time status updates,
// allowing the reasoning loop to be used by multiple transports (SSE, Telegram, etc.)

var (
	GlobalTokenCount     int
	GlobalTokenEstimated bool
	muTokens             sync.Mutex

	sessionInterrupts = make(map[string]bool)
	muInterrupts      sync.Mutex

	debugModeEnabled bool
	muDebugMode      sync.Mutex
)

// SetDebugMode enables or disables the runtime debug mode for the agent.
// When enabled, the agent's system prompt includes an extra debugging instruction.
func SetDebugMode(enabled bool) {
	muDebugMode.Lock()
	defer muDebugMode.Unlock()
	debugModeEnabled = enabled
}

// GetDebugMode returns whether debug mode is currently active.
func GetDebugMode() bool {
	muDebugMode.Lock()
	defer muDebugMode.Unlock()
	return debugModeEnabled
}

// ToggleDebugMode flips the current debug mode state and returns the new value.
func ToggleDebugMode() bool {
	muDebugMode.Lock()
	defer muDebugMode.Unlock()
	debugModeEnabled = !debugModeEnabled
	return debugModeEnabled
}

// InterruptSession marks a specific session as interrupted.
func InterruptSession(sessionID string) {
	muInterrupts.Lock()
	defer muInterrupts.Unlock()
	sessionInterrupts[sessionID] = true
}

// checkAndClearInterrupt returns true if the session was interrupted and clears the flag.
func checkAndClearInterrupt(sessionID string) bool {
	muInterrupts.Lock()
	defer muInterrupts.Unlock()
	if sessionInterrupts[sessionID] {
		delete(sessionInterrupts, sessionID)
		return true
	}
	return false
}

// estimateTokens provides a rough character-based token count for when the API doesn't return one.
func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	// Rough heuristic: 1 token per 4 characters
	return len(text) / 4
}

// ── Recency-Boosted Re-ranking (Phase A3) ──────────────────────────

type FeedbackBroker interface {
	Send(event, message string)
	SendJSON(jsonStr string)
}

// NoopBroker is a silent fallback for transports that don't support real-time feedback
type NoopBroker struct{}

func (n NoopBroker) Send(event, message string) {}
func (n NoopBroker) SendJSON(jsonStr string)    {}

// ToolCall represents a parsed tool invocation from the LLM.
type ToolCall struct {
	Action             string                 `json:"action"`
	Code               string                 `json:"code"`
	Key                string                 `json:"key"`
	Value              string                 `json:"value"`
	Name               string                 `json:"name"`
	Description        string                 `json:"description"`
	Package            string                 `json:"package"`
	Args               interface{}            `json:"args"`
	Background         bool                   `json:"background"`
	PID                int                    `json:"pid"`
	IsTool             bool                   `json:"-"`
	RawCodeDetected    bool                   `json:"-"`
	RawJSON            string                 `json:"-"`
	Operation          string                 `json:"operation"`
	Fact               string                 `json:"fact"`
	ID                 string                 `json:"id"`
	CronExpr           string                 `json:"cron_expr"`
	TaskPrompt         string                 `json:"task_prompt"`
	Skill              string                 `json:"skill"`
	SkillArgs          map[string]interface{} `json:"skill_args"`
	Content            string                 `json:"content"`
	Query              string                 `json:"query"` // Alias for content in query_memory
	Metadata           map[string]interface{} `json:"metadata"`
	FilePath           string                 `json:"file_path"`
	Path               string                 `json:"path"` // Alias for file_path
	Destination        string                 `json:"destination"`
	Dest               string                 `json:"dest"` // Alias for destination
	URL                string                 `json:"url"`
	Method             string                 `json:"method"`
	Headers            map[string]string      `json:"headers"`
	Params             map[string]interface{} `json:"params"`
	Tag                string                 `json:"tag"`
	Hostname           string                 `json:"hostname"`
	ServerID           string                 `json:"server_id"`
	MemoryKey          string                 `json:"memory_key"`   // Synonym for fact
	MemoryValue        string                 `json:"memory_value"` // Synonym for fact/content
	NotifyOnCompletion bool                   `json:"notify_on_completion"`
	Body               string                 `json:"body"`
	Source             string                 `json:"source"`
	Target             string                 `json:"target"`
	Relation           string                 `json:"relation"`
	Properties         map[string]string      `json:"properties"`
	Preview            bool                   `json:"preview"`
	Port               int                    `json:"port"`
	Username           string                 `json:"username"`
	Password           string                 `json:"password"`
	PrivateKeyPath     string                 `json:"private_key_path"`
	Tags               string                 `json:"tags"`
	Direction          string                 `json:"direction"`
	LocalPath          string                 `json:"local_path"`
	RemotePath         string                 `json:"remote_path"`
	ToolName           string                 `json:"tool_name"`
	Tool               string                 `json:"tool"`         // Hallucination fallback
	Arguments          interface{}            `json:"arguments"`    // Hallucination fallback
	ActionInput        map[string]interface{} `json:"action_input"` // LangChain-style nested params
	Label              string                 `json:"label"`
	Command            string                 `json:"command"`
	ThresholdLow       int                    `json:"threshold_low"`
	ThresholdMedium    int                    `json:"threshold_medium"`
	Pinned             bool                   `json:"pinned"`
	IPAddress          string                 `json:"ip_address"`
	To                 string                 `json:"to"`
	CC                 string                 `json:"cc"`
	Subject            string                 `json:"subject"`
	Folder             string                 `json:"folder"`
	Limit              int                    `json:"limit"`
	ChannelID          string                 `json:"channel_id"`
	Message            string                 `json:"message"`
	// Notes / To-Do fields
	Title    string `json:"title"`
	Priority int    `json:"priority"`
	DueDate  string `json:"due_date"`
	Category string `json:"category"`
	Done     int    `json:"done"` // -1=all, 0=open, 1=done (filter for list)
	// Inventory / Device fields
	DeviceType string `json:"device_type,omitempty"`
	NoteID     int64  `json:"note_id"`
	// Google Workspace fields
	DocumentID string `json:"document_id"`
	MaxResults int    `json:"max_results"`
	Append     bool   `json:"append"`
	// Vision / STT fields
	Prompt string `json:"prompt"`
	// Home Assistant fields
	EntityID    string                 `json:"entity_id"`
	Domain      string                 `json:"domain"`
	Service     string                 `json:"service"`
	ServiceData map[string]interface{} `json:"service_data"`
	// Docker fields
	ContainerID string            `json:"container_id"`
	Image       string            `json:"image"`
	Env         []string          `json:"env"`
	Ports       map[string]string `json:"ports"`
	Volumes     []string          `json:"volumes"`
	Restart     string            `json:"restart"`
	Force       bool              `json:"force"`
	Tail        int               `json:"tail"`
	All         bool              `json:"all"`
	Network     string            `json:"network"`
	Driver      string            `json:"driver"`
	User        string            `json:"user"`
	File        string            `json:"file"`
	// Co-Agent fields
	CoAgentID    string   `json:"co_agent_id"`
	Task         string   `json:"task"`
	ContextHints []string `json:"context_hints"`
	// TTS / Chromecast fields
	Text        string  `json:"text"`
	DeviceAddr  string  `json:"device_addr"`
	DevicePort  int     `json:"device_port"`
	Volume      float64 `json:"volume"`
	ContentType string  `json:"content_type"`
	Language    string  `json:"language"`
	// MDNS fields
	ServiceType string `json:"service_type"`
	Timeout     int    `json:"timeout"`
	// Notification fields
	Channel string `json:"channel"`
}

// GetArgs returns Args as a string slice, handling various input types (slice of strings or interface).
func (tc ToolCall) GetArgs() []string {
	if tc.Args == nil {
		return nil
	}
	if slice, ok := tc.Args.([]string); ok {
		return slice
	}
	if slice, ok := tc.Args.([]interface{}); ok {
		var res []string
		for _, v := range slice {
			if s, ok := v.(string); ok {
				res = append(res, s)
			} else {
				res = append(res, fmt.Sprintf("%v", v))
			}
		}
		return res
	}
	return nil
}

// RunConfig holds all the dependencies required to run the agent loop,
// consolidating the parameter list that was previously over 20 items long.
type RunConfig struct {
	Config          *config.Config
	Logger          *slog.Logger
	LLMClient       llm.ChatClient
	ShortTermMem    *memory.SQLiteMemory
	HistoryManager  *memory.HistoryManager
	LongTermMem     memory.VectorDB
	KG              *memory.KnowledgeGraph
	InventoryDB     *sql.DB
	Vault           *security.Vault
	Registry        *tools.ProcessRegistry
	Manifest        *tools.Manifest
	CronManager     *tools.CronManager
	CoAgentRegistry *CoAgentRegistry
	BudgetTracker   *budget.Tracker
	SessionID       string
	IsMaintenance   bool
	SurgeryPlan     string
}

// ExecuteAgentLoop executes the multi-turn reasoning and tool execution loop.
// It supports both synchronous returns and asynchronous streaming via the broker.
func ExecuteAgentLoop(ctx context.Context, req openai.ChatCompletionRequest, runCfg RunConfig, stream bool, broker FeedbackBroker) (openai.ChatCompletionResponse, error) {
	cfg := runCfg.Config
	logger := runCfg.Logger
	client := runCfg.LLMClient
	shortTermMem := runCfg.ShortTermMem
	historyManager := runCfg.HistoryManager
	longTermMem := runCfg.LongTermMem
	kg := runCfg.KG
	inventoryDB := runCfg.InventoryDB
	vault := runCfg.Vault
	registry := runCfg.Registry
	manifest := runCfg.Manifest
	cronManager := runCfg.CronManager
	coAgentRegistry := runCfg.CoAgentRegistry
	budgetTracker := runCfg.BudgetTracker
	sessionID := runCfg.SessionID
	isMaintenance := runCfg.IsMaintenance
	surgeryPlan := runCfg.SurgeryPlan
	flags := prompts.ContextFlags{
		IsErrorState:           false,
		RequiresCoding:         false,
		SystemLanguage:         cfg.Agent.SystemLanguage,
		LifeboatEnabled:        cfg.Maintenance.LifeboatEnabled,
		IsMaintenanceMode:      isMaintenance,
		SurgeryPlan:            surgeryPlan,
		CorePersonality:        cfg.Agent.CorePersonality,
		TokenBudget:            cfg.Agent.SystemPromptTokenBudget,
		IsDebugMode:            cfg.Agent.DebugMode || GetDebugMode(),
		IsCoAgent:              strings.HasPrefix(sessionID, "coagent-"),
		DiscordEnabled:         cfg.Discord.Enabled,
		EmailEnabled:           cfg.Email.Enabled,
		DockerEnabled:          cfg.Docker.Enabled,
		HomeAssistantEnabled:   cfg.HomeAssistant.Enabled,
		WebDAVEnabled:          cfg.WebDAV.Enabled,
		KoofrEnabled:           cfg.Koofr.Enabled,
		ChromecastEnabled:      cfg.Chromecast.Enabled,
		CoAgentEnabled:         cfg.CoAgents.Enabled,
		GoogleWorkspaceEnabled: cfg.Agent.EnableGoogleWorkspace,
	}
	toolCallCount := 0
	rawCodeCount := 0
	missedToolCount := 0
	announcementCount := 0
	sessionTokens := 0
	emptyRetried := false // Prevents infinite retry on persistent empty responses
	stepsSinceLastFeedback := 0

	// Guardian: prompt injection defense
	guardian := security.NewGuardian(logger)

	var currentLogger *slog.Logger = logger
	lastActivity := time.Now()
	lastTool := ""
	recentTools := make([]string, 0, 5) // Track last 5 tools for lazy schema injection
	explicitTools := make([]string, 0)  // Explicit tool guides requested via <workflow_plan> tag
	workflowPlanCount := 0              // Prevent infinite workflow_plan loops
	lastResponseWasTool := false        // True when the previous iteration was a tool call; suppresses announcement detector on completion messages
	pendingTCs := make([]ToolCall, 0)   // Queued tool calls from multi-tool responses (processed without a new LLM call)

	// Core memory cache: read once, invalidate on manage_memory calls
	coreMemCache := ""
	coreMemDirty := true // Force initial load

	// Phase D: Personality Engine (opt-in)
	personalityEnabled := cfg.Agent.PersonalityEngine
	if personalityEnabled && shortTermMem != nil {
		if err := shortTermMem.InitPersonalityTables(); err != nil {
			logger.Error("[Personality] Failed to init tables, disabling", "error", err)
			personalityEnabled = false
		}
	}

	// Native function calling: build tool schemas once and attach to request
	toolGuidesDir := filepath.Join(cfg.Directories.PromptsDir, "tools_manuals")

	// Auto-detect DeepSeek and enable native function calling
	if strings.Contains(strings.ToLower(cfg.LLM.Model), "deepseek") && !cfg.LLM.UseNativeFunctions {
		cfg.LLM.UseNativeFunctions = true
		logger.Info("[NativeTools] DeepSeek detected, auto-enabling native function calling")
	}

	if cfg.LLM.UseNativeFunctions {
		ff := ToolFeatureFlags{
			HomeAssistantEnabled: cfg.HomeAssistant.Enabled,
			DockerEnabled:        cfg.Docker.Enabled,
			CoAgentEnabled:       cfg.CoAgents.Enabled,
		}
		ntSchemas := BuildNativeToolSchemas(cfg.Directories.SkillsDir, manifest, cfg.Agent.EnableGoogleWorkspace, ff, logger)
		req.Tools = ntSchemas
		req.ToolChoice = "auto"
		logger.Info("[NativeTools] Native function calling enabled", "tool_count", len(ntSchemas))
	}

	for {
		// Check for user interrupt
		if checkAndClearInterrupt(sessionID) {
			currentLogger.Warn("[Sync] User interrupted the agent")
			interruptMsg := "the user has interrupted your work. ask what is wrong"
			req.Messages = append(req.Messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: interruptMsg})
			// Reset error states to focus on the new interrupt msg
			flags.IsErrorState = false
			broker.Send("thinking", "User interrupted. Asking for instructions...")
			continue
		}

		// Revive logic: If idle in lifeboat for too long, poke the agent
		if isMaintenance && time.Since(lastActivity) > time.Duration(cfg.CircuitBreaker.MaintenanceTimeoutMinutes)*time.Minute {
			currentLogger.Warn("[Sync] Lifeboat idle for too long, injecting revive prompt", "minutes", cfg.CircuitBreaker.MaintenanceTimeoutMinutes)
			reviveMsg := "You are idle in the lifeboat. finish your tasks or change back to the supervisor."
			req.Messages = append(req.Messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: reviveMsg})
			lastActivity = time.Now() // Reset timer
		}

		// Refresh maintenance status to account for mid-loop handovers
		isMaintenance = isMaintenance || tools.IsBusy()
		flags.IsMaintenanceMode = isMaintenance

		// Caching the logger to avoid opening file on every iteration (leaking FDs)
		if isMaintenance && currentLogger == nil {
			logPath := filepath.Join(cfg.Logging.LogDir, "lifeboat.log")
			if l, err := loggerPkg.SetupWithFile(true, logPath, true); err == nil {
				currentLogger = l.Logger
			}
		}
		if currentLogger == nil {
			currentLogger = logger
		}

		currentLogger.Debug("[Sync] Agent loop iteration starting", "is_maintenance", isMaintenance, "lock_exists", tools.IsBusy())

		// Process queued tool calls from multi-tool responses (skip LLM for these)
		if len(pendingTCs) > 0 {
			ptc := pendingTCs[0]
			pendingTCs = pendingTCs[1:]
			toolCallCount++
			broker.Send("thinking", fmt.Sprintf("[%d] Running %s...", toolCallCount, ptc.Action))
			ptcJSON := ptc.RawJSON
			if ptcJSON == "" {
				ptcJSON = fmt.Sprintf(`{"action":"%s"}`, ptc.Action)
			}
			id, idErr := shortTermMem.InsertMessage(sessionID, openai.ChatMessageRoleAssistant, ptcJSON, false, true)
			if idErr != nil {
				currentLogger.Error("Failed to persist queued tool-call message", "error", idErr)
			}
			if sessionID == "default" {
				historyManager.Add(openai.ChatMessageRoleAssistant, ptcJSON, id, false, true)
			}
			broker.Send("tool_call", ptcJSON)
			broker.Send("tool_start", ptc.Action)
			pResultContent := DispatchToolCall(ctx, ptc, cfg, currentLogger, client, vault, registry, manifest, cronManager, longTermMem, shortTermMem, kg, inventoryDB, historyManager, tools.IsBusy(), surgeryPlan, guardian, sessionID, coAgentRegistry, budgetTracker)
			broker.Send("tool_output", pResultContent)
			broker.Send("tool_end", ptc.Action)
			lastActivity = time.Now()
			if ptc.Action == "manage_memory" || ptc.Action == "core_memory" {
				coreMemDirty = true
			}
			id, idErr = shortTermMem.InsertMessage(sessionID, openai.ChatMessageRoleUser, pResultContent, false, true)
			if idErr != nil {
				currentLogger.Error("Failed to persist queued tool-result message", "error", idErr)
			}
			if sessionID == "default" {
				historyManager.Add(openai.ChatMessageRoleUser, pResultContent, id, false, true)
			}
			req.Messages = append(req.Messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: ptcJSON})
			req.Messages = append(req.Messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: pResultContent})
			lastResponseWasTool = true
			continue
		}

		// Load Personality Meta
		var meta memory.PersonalityMeta
		if personalityEnabled {
			meta = prompts.GetCorePersonalityMeta(cfg.Directories.PromptsDir, flags.CorePersonality)
		}

		// Circuit breaker
		effectiveMaxCalls := cfg.CircuitBreaker.MaxToolCalls
		if personalityEnabled && cfg.Agent.PersonalityEngineV2 && shortTermMem != nil {
			if traits, err := shortTermMem.GetTraits(); err == nil {
				if thoroughness, ok := traits[memory.TraitThoroughness]; ok && thoroughness > 0.8 {
					// High thoroughness trait allows 50% more tool calls before triggering the breaker
					effectiveMaxCalls = int(float64(effectiveMaxCalls) * 1.5)
					currentLogger.Debug("[Behavioral Tool Calling] Increased MaxToolCalls due to high Thoroughness", "new_max", effectiveMaxCalls)
				}
			}
		}

		if toolCallCount >= effectiveMaxCalls {
			currentLogger.Warn("[Sync] Circuit breaker triggered", "count", toolCallCount, "limit", effectiveMaxCalls)
			breakerMsg := fmt.Sprintf("CIRCUIT BREAKER: You have reached the maximum of %d consecutive tool calls. You MUST now summarize your progress and respond to the user with a final answer.", effectiveMaxCalls)
			req.Messages = append(req.Messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: breakerMsg})
		}

		flags.ActiveProcesses = GetActiveProcessStatus(registry)

		// Load Core Memory (cached, invalidated when manage_memory is called)
		if coreMemDirty {
			if shortTermMem != nil {
				coreMemCache = shortTermMem.ReadCoreMemory()
			}
			coreMemDirty = false
		}

		// Extract explicit workflow tools if present (populated from previous iteration's <workflow_plan> tag)
		// explicitTools is persistent across loop iterations

		// Prepare Dynamic Tool Guides
		lastUserMsg := ""
		if len(req.Messages) > 0 && req.Messages[len(req.Messages)-1].Role == openai.ChatMessageRoleUser {
			lastUserMsg = req.Messages[len(req.Messages)-1].Content
		}

		// Get the mood trigger context from the message history
		triggerValue := getMoodTrigger(req.Messages, lastUserMsg)
		moodTrigger := func() string { return triggerValue }

		// Note: The call to PrepareDynamicGuides will happen after the response is received
		// We initialize flags.PredictedGuides now with empty explicit tools to satisfy builder.go for the first prompt
		flags.PredictedGuides = prompts.PrepareDynamicGuides(longTermMem, shortTermMem, lastUserMsg, lastTool, toolGuidesDir, recentTools, explicitTools, currentLogger)

		// Automatic RAG: retrieve relevant long-term memories for the current user message
		// Phase A3: Over-fetch and re-rank with recency boost from memory_meta
		flags.RetrievedMemories = ""
		flags.PredictedMemories = ""
		if lastUserMsg != "" && longTermMem != nil {
			// Over-fetch 6 candidates, then re-rank to keep best 3
			memories, docIDs, err := longTermMem.SearchSimilar(lastUserMsg, 6)
			if err == nil && len(memories) > 0 {
				ranked := rerankWithRecency(memories, docIDs, shortTermMem, currentLogger)
				for _, r := range ranked {
					_ = shortTermMem.UpdateMemoryAccess(r.docID)
				}
				if len(ranked) > 3 {
					ranked = ranked[:3]
				}
				var topMemories []string
				for _, r := range ranked {
					topMemories = append(topMemories, r.text)
				}
				flags.RetrievedMemories = strings.Join(topMemories, "\n---\n")
				currentLogger.Debug("[Sync] RAG: Retrieved memories (recency-boosted)", "count", len(ranked))
			}

			// Phase A4: Record interaction pattern for temporal learning
			if shortTermMem != nil {
				topic := lastUserMsg
				if len(topic) > 80 {
					topic = topic[:80]
				}
				_ = shortTermMem.RecordInteraction(topic)
			}

			// Phase B: Predictive pre-fetch based on temporal patterns + tool transitions
			if shortTermMem != nil {
				now := time.Now()
				predictions, err := shortTermMem.PredictNextQuery(lastTool, now.Hour(), int(now.Weekday()), 2)
				if err == nil && len(predictions) > 0 {
					var predictedResults []string
					for _, pred := range predictions {
						pMem, _, pErr := longTermMem.SearchSimilar(pred, 1)
						if pErr == nil && len(pMem) > 0 {
							predictedResults = append(predictedResults, pMem[0])
						}
					}
					if len(predictedResults) > 0 {
						flags.PredictedMemories = strings.Join(predictedResults, "\n---\n")
						currentLogger.Debug("[Sync] Predictive RAG: Pre-fetched memories", "count", len(predictedResults), "predictions", predictions)
					}
				}
			}
		}

		// Phase D: Inject personality line before building system prompt
		if personalityEnabled && shortTermMem != nil {
			if cfg.Agent.PersonalityEngineV2 {
				// V2 Feature: Narrative Events based on Milestones & Loneliness
				processBehavioralEvents(shortTermMem, &req.Messages, sessionID, meta, currentLogger)
			}
			flags.PersonalityLine = shortTermMem.GetPersonalityLine(cfg.Agent.PersonalityEngineV2)
		}

		// Adaptive tier: adjust prompt complexity based on conversation length
		flags.MessageCount = len(req.Messages)
		flags.Tier = prompts.DetermineTier(flags.MessageCount)
		flags.RecentlyUsedTools = recentTools
		flags.IsDebugMode = cfg.Agent.DebugMode || GetDebugMode() // re-check each iteration (toggleable at runtime)

		sysPrompt := prompts.BuildSystemPrompt(cfg.Directories.PromptsDir, flags, coreMemCache, currentLogger)

		// Inject budget hint into system prompt when threshold is crossed
		if budgetTracker != nil {
			if hint := budgetTracker.GetPromptHint(); hint != "" {
				sysPrompt += "\n\n" + hint
			}
		}

		currentLogger.Debug("[Sync] System prompt rebuilt", "length", len(sysPrompt), "tier", flags.Tier, "tokens", prompts.CountTokens(sysPrompt), "error_state", flags.IsErrorState, "coding_mode", flags.RequiresCoding, "active_daemons", flags.ActiveProcesses)

		if len(req.Messages) > 0 && req.Messages[0].Role == openai.ChatMessageRoleSystem {
			req.Messages[0].Content = sysPrompt
		} else {
			req.Messages = append([]openai.ChatCompletionMessage{
				{Role: openai.ChatMessageRoleSystem, Content: sysPrompt},
			}, req.Messages...)
		}

		// Verbose Logging of LLM Request
		if len(req.Messages) > 0 {
			lastMsg := req.Messages[len(req.Messages)-1]
			// Keep conversation logs in the original logger (stdout) to avoid pollution of technical log
			logger.Info("[LLM Request]", "role", lastMsg.Role, "content_len", len(lastMsg.Content), "preview", Truncate(lastMsg.Content, 200))
			currentLogger.Info("[LLM Request Redirected]", "role", lastMsg.Role, "content_len", len(lastMsg.Content))
			currentLogger.Debug("[LLM Full History]", "messages_count", len(req.Messages))
		}

		broker.Send("thinking", "")

		// Budget check: block if daily budget exceeded and enforcement = full
		if budgetTracker != nil && budgetTracker.IsBlocked("chat") {
			broker.Send("budget_blocked", "Daily budget exceeded. All LLM calls blocked until reset.")
			return openai.ChatCompletionResponse{}, fmt.Errorf("budget exceeded (enforcement=full)")
		}

		// Configurable timeout for each individual LLM call to prevent infinite hangs
		llmCtx, cancelResp := context.WithTimeout(ctx, time.Duration(cfg.CircuitBreaker.LLMTimeoutSeconds)*time.Second)

		var resp openai.ChatCompletionResponse
		var content string
		var err error
		var promptTokens, completionTokens, totalTokens int

		if stream {
			stm, streamErr := llm.ExecuteStreamWithRetry(llmCtx, client, req, currentLogger, broker)
			if streamErr != nil {
				cancelResp()
				return openai.ChatCompletionResponse{}, streamErr
			}

			var assembledResponse strings.Builder
			for {
				chunk, rErr := stm.Recv()
				if rErr != nil {
					if rErr.Error() != "EOF" {
						currentLogger.Error("Stream error", "error", rErr)
					}
					break
				}
				if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
					assembledResponse.WriteString(chunk.Choices[0].Delta.Content)
					// Proxy the JSON chunk to the broker if it supports dynamic passthrough (SSE)
					// We'll marshal it so we can push it cleanly
					if chunkData, mErr := json.Marshal(chunk); mErr == nil {
						broker.SendJSON(fmt.Sprintf("data: %s\n\n", string(chunkData)))
					}
				}
			}
			stm.Close()
			content = assembledResponse.String()

			// Estimate streaming tokens
			completionTokens = estimateTokens(content)
			for _, m := range req.Messages {
				promptTokens += estimateTokens(m.Content)
			}
			totalTokens = promptTokens + completionTokens

			// Mock a response object for remaining loop logic
			resp = openai.ChatCompletionResponse{
				Choices: []openai.ChatCompletionChoice{
					{Message: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: content}},
				},
				Usage: openai.Usage{
					PromptTokens:     promptTokens,
					CompletionTokens: completionTokens,
					TotalTokens:      totalTokens,
				},
			}
		} else {
			resp, err = llm.ExecuteWithRetry(llmCtx, client, req, currentLogger, broker)
			if err != nil {
				cancelResp()
				return openai.ChatCompletionResponse{}, err
			}
			if len(resp.Choices) == 0 {
				cancelResp()
				return openai.ChatCompletionResponse{}, fmt.Errorf("no choices returned from LLM")
			}
			content = resp.Choices[0].Message.Content
		}

		cancelResp()

		// Empty response recovery: if the LLM returns nothing, trim history and retry once.
		// This typically happens when the total context exceeds the model's window.
		if strings.TrimSpace(content) == "" && len(resp.Choices[0].Message.ToolCalls) == 0 && len(req.Messages) > 4 && !emptyRetried {
			emptyRetried = true
			currentLogger.Warn("[Sync] Empty LLM response detected, trimming history and retrying", "messages_count", len(req.Messages))
			broker.Send("thinking", "Context too large, retrimming...")
			// Keep system prompt (index 0) + optional summary (index 1 if system) + last 4 messages
			var trimmed []openai.ChatCompletionMessage
			trimmed = append(trimmed, req.Messages[0]) // system prompt
			// Keep second message if it's a system summary
			startIdx := 1
			if len(req.Messages) > 1 && req.Messages[1].Role == openai.ChatMessageRoleSystem {
				trimmed = append(trimmed, req.Messages[1])
				startIdx = 2
			}
			// Keep last 4 messages from history
			historyMsgs := req.Messages[startIdx:]
			if len(historyMsgs) > 4 {
				historyMsgs = historyMsgs[len(historyMsgs)-4:]
			}
			trimmed = append(trimmed, historyMsgs...)
			req.Messages = trimmed
			currentLogger.Info("[Sync] Retrying with trimmed context", "new_messages_count", len(req.Messages))
			continue
		}

		// Safety Check: Strip "RECAP" hallucinations if the model is still stuck in the old pattern
		content = strings.TrimPrefix(content, "[RECAP OF PREVIOUS DISCUSSIONS]:")
		content = strings.TrimPrefix(content, "[RECAP OF PREVIOUS DISCUSSIONS]:\n")
		content = strings.TrimPrefix(content, "[CONTEXT_RECAP]:")
		content = strings.TrimPrefix(content, "[CONTEXT_RECAP]:\n")
		content = strings.TrimSpace(content)

		// Conversation log to stdout
		logger.Info("[LLM Response]", "content_len", len(content), "preview", Truncate(content, 200))
		// Activity log to file
		currentLogger.Info("[LLM Response Received]", "content_len", len(content))
		lastActivity = time.Now() // LLM activity

		// Detect tool call: native API-level ToolCalls (use_native_functions=true) or text-based JSON
		var tc ToolCall
		useNativePath := false
		nativeAssistantMsg := resp.Choices[0].Message // snapshot for role=tool continuation

		if len(resp.Choices[0].Message.ToolCalls) > 0 {
			nativeCall := resp.Choices[0].Message.ToolCalls[0]
			// Primary native path: parse directly from API-level ToolCall object
			// We now take this path if UseNativeFunctions is true OR if the model sent them anyway
			tc = NativeToolCallToToolCall(nativeCall, currentLogger)
			useNativePath = true
			currentLogger.Info("[Sync] Native tool call detected", "function", tc.Action, "id", nativeCall.ID, "forced", !cfg.LLM.UseNativeFunctions)
		}

		// Text-based fallback: parse JSON from content string if native path not taken
		if !useNativePath {
			tc = ParseToolCall(content)
			// If the response contains multiple tool calls (e.g. two manage_memory adds),
			// queue the extras so they execute in subsequent iterations without a new LLM call.
			if tc.IsTool {
				extras := extractExtraToolCalls(content, tc.RawJSON)
				if len(extras) > 0 {
					currentLogger.Info("[MultiTool] Queued additional tool calls from response", "count", len(extras))
					pendingTCs = append(pendingTCs, extras...)
				}
			}
		}

		// Obsolete: we now send it later when histContent is fully assembled.
		if !stream {
			promptTokens = resp.Usage.PromptTokens
			completionTokens = resp.Usage.CompletionTokens
			totalTokens = resp.Usage.TotalTokens
		}

		if totalTokens == 0 {
			// Estimate tokens if usage is missing
			muTokens.Lock()
			GlobalTokenEstimated = true
			muTokens.Unlock()

			// Estimate prompt tokens from all messages in request
			for _, m := range req.Messages {
				promptTokens += estimateTokens(m.Content)
			}
			// Estimate completion tokens from response content
			completionTokens = estimateTokens(content)
			totalTokens = promptTokens + completionTokens
		}

		sessionTokens += totalTokens
		muTokens.Lock()
		GlobalTokenCount += totalTokens
		localGlobalTotal := GlobalTokenCount
		localIsEstimated := GlobalTokenEstimated
		muTokens.Unlock()

		broker.SendJSON(fmt.Sprintf(`{"event":"tokens","prompt":%d,"completion":%d,"total":%d,"session_total":%d,"global_total":%d,"is_estimated":%t}`,
			promptTokens, completionTokens, totalTokens, sessionTokens, localGlobalTotal, localIsEstimated))

		// Budget tracking: record cost and send status to UI
		if budgetTracker != nil {
			actualModel := resp.Model
			if actualModel == "" {
				actualModel = req.Model
			}
			crossedWarning := budgetTracker.Record(actualModel, promptTokens, completionTokens)
			budgetJSON := budgetTracker.GetStatusJSON()
			if budgetJSON != "" {
				broker.SendJSON(budgetJSON)
			}
			if crossedWarning {
				bs := budgetTracker.GetStatus()
				warnMsg := fmt.Sprintf("\u26a0\ufe0f Budget warning: %.0f%% used ($%.4f / $%.2f)", bs.Percentage*100, bs.SpentUSD, bs.DailyLimit)
				broker.Send("budget_warning", warnMsg)
			}
			if budgetTracker.IsExceeded() {
				bs := budgetTracker.GetStatus()
				exMsg := fmt.Sprintf("\u26d4 Budget exceeded! $%.4f / $%.2f (enforcement: %s)", bs.SpentUSD, bs.DailyLimit, bs.Enforcement)
				broker.Send("budget_blocked", exMsg)
			}
		}

		currentLogger.Debug("[Sync] Tool detection", "is_tool", tc.IsTool, "action", tc.Action, "raw_code", tc.RawCodeDetected)

		// Clear explicit tools after they've been consumed (they were injected this iteration)
		if len(explicitTools) > 0 {
			explicitTools = explicitTools[:0]
		}

		// Detect <workflow_plan>["tool1","tool2"]</workflow_plan> in the response
		if workflowPlanCount < 3 {
			if parsed, stripped := parseWorkflowPlan(content); len(parsed) > 0 {
				workflowPlanCount++
				explicitTools = parsed
				currentLogger.Info("[Sync] Workflow plan detected, loading tool guides", "tools", parsed, "attempt", workflowPlanCount)
				broker.Send("workflow_plan", strings.Join(parsed, ", "))

				// Store the stripped content as assistant message
				strippedContent := strings.TrimSpace(stripped)
				if strippedContent != "" {
					id, err := shortTermMem.InsertMessage(sessionID, openai.ChatMessageRoleAssistant, strippedContent, false, false)
					if err != nil {
						currentLogger.Error("Failed to persist workflow plan message", "error", err)
					}
					if sessionID == "default" {
						historyManager.Add(openai.ChatMessageRoleAssistant, strippedContent, id, false, false)
					}
					req.Messages = append(req.Messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: strippedContent})
				}

				// Inject a system nudge so the agent knows the guides are available
				nudge := fmt.Sprintf("Tool manuals loaded for: %s. Proceed with your plan.", strings.Join(parsed, ", "))
				req.Messages = append(req.Messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: nudge})
				continue
			}
		}

		if tc.RawCodeDetected && rawCodeCount < 2 {
			rawCodeCount++
			currentLogger.Warn("[Sync] Raw code detected, sending corrective feedback", "attempt", rawCodeCount)
			broker.Send("error_recovery", "Raw code detected, requesting JSON format...")

			id, err := shortTermMem.InsertMessage(sessionID, openai.ChatMessageRoleAssistant, content, false, true)
			if err != nil {
				currentLogger.Error("Failed to persist assistant message to SQLite", "error", err)
			}
			if sessionID == "default" {
				historyManager.Add(openai.ChatMessageRoleAssistant, content, id, false, true)
			}

			feedbackMsg := "ERROR: You sent raw Python code instead of a JSON tool call. My supervisor only understands JSON tool calls. Please wrap your code in a valid JSON object: {\"action\": \"save_tool\", \"name\": \"script.py\", \"description\": \"...\", \"code\": \"<your python code with \\n escaped>\"}."
			id, err = shortTermMem.InsertMessage(sessionID, openai.ChatMessageRoleUser, feedbackMsg, false, false)
			if err != nil {
				currentLogger.Error("Failed to persist feedback message to SQLite", "error", err)
			}
			if sessionID == "default" {
				historyManager.Add(openai.ChatMessageRoleUser, feedbackMsg, id, false, false)
			}

			req.Messages = append(req.Messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: content})
			req.Messages = append(req.Messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: feedbackMsg})
			continue
		}

		// Recovery: model sent an announcement/preamble instead of a tool call
		// Triggered when: no tool, short response, contains action-intent phrases
		announcementPhrases := []string{
			"lass mich", "ich starte", "ich werde", "ich führe", "ich teste",
			"let me", "i will", "i'll", "i am going to", "i'm going to",
			"let's start", "starting", "launching", "i'll start", "i'll run",
			"alles klar", "okay, let", "sure, let", "sure, i",
			"ich suche nach", "ich schaue nach", "ich prüfe", "ich überprüfe",
			"ich sehe mir", "lass mich sehen", "ich werde nachschauen",
			"i'll check", "let me check", "checking", "searching", "looking",
			"i am looking", "i will look", "i'll search", "i will search",
			"ich frage ab", "ich lade", "i'll load", "i am loading",
		}
		isAnnouncement := func() bool {
			if tc.IsTool || useNativePath || tc.RawCodeDetected || len(content) > 1000 {
				return false
			}
			// A response ending with '?' is a conversational reply, not an action announcement
			if strings.HasSuffix(strings.TrimRight(strings.TrimSpace(content), "\"'"), "?") {
				return false
			}
			// If the LLM just completed a tool call, a text response is a completion confirmation, not an announcement
			if lastResponseWasTool {
				return false
			}
			lc := strings.ToLower(content)
			for _, phrase := range announcementPhrases {
				if strings.Contains(lc, phrase) {
					return true
				}
			}
			return false
		}()
		if isAnnouncement && announcementCount < 2 {
			announcementCount++
			currentLogger.Warn("[Sync] Announcement-only response detected, requesting immediate tool call", "attempt", announcementCount, "content_preview", Truncate(content, 120))
			broker.Send("error_recovery", "Announcement without action detected, requesting tool call...")

			id, err := shortTermMem.InsertMessage(sessionID, openai.ChatMessageRoleAssistant, content, false, true)
			if err != nil {
				currentLogger.Error("Failed to persist assistant message to SQLite", "error", err)
			}
			if sessionID == "default" {
				historyManager.Add(openai.ChatMessageRoleAssistant, content, id, false, true)
			}

			feedbackMsg := "ERROR: You announced what you were going to do but did not output a tool call. When executing a task, your ENTIRE response must be ONLY the raw JSON tool call — no explanation before it. Output the JSON tool call NOW."
			id, err = shortTermMem.InsertMessage(sessionID, openai.ChatMessageRoleUser, feedbackMsg, false, false)
			if err != nil {
				currentLogger.Error("Failed to persist feedback message to SQLite", "error", err)
			}
			if sessionID == "default" {
				historyManager.Add(openai.ChatMessageRoleUser, feedbackMsg, id, false, false)
			}

			req.Messages = append(req.Messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: content})
			req.Messages = append(req.Messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: feedbackMsg})
			continue
		}

		// Recovery: model wrapped tool call in markdown fence instead of bare JSON
		if !tc.IsTool && !tc.RawCodeDetected && missedToolCount < 2 &&
			(strings.Contains(content, "```") || strings.Contains(content, "{")) &&
			(strings.Contains(content, `"action"`) || strings.Contains(content, `'action'`)) {
			missedToolCount++
			currentLogger.Warn("[Sync] Missed tool call in fence, sending corrective feedback", "attempt", missedToolCount, "content_preview", Truncate(content, 150))
			broker.Send("error_recovery", "Tool call wrapped in fence, requesting raw JSON...")

			id, err := shortTermMem.InsertMessage(sessionID, openai.ChatMessageRoleAssistant, content, false, true)
			if err != nil {
				currentLogger.Error("Failed to persist assistant message to SQLite", "error", err)
			}
			if sessionID == "default" {
				historyManager.Add(openai.ChatMessageRoleAssistant, content, id, false, true)
			}

			feedbackMsg := "ERROR: Your response contained explanation text and/or markdown fences (```json). Tool calls MUST be a raw JSON object ONLY - no explanation before or after, no markdown, no fences. Output ONLY the JSON object, starting with { and ending with }. Example: {\"action\": \"co_agent\", \"operation\": \"spawn\", \"task\": \"...\"}"
			id, err = shortTermMem.InsertMessage(sessionID, openai.ChatMessageRoleUser, feedbackMsg, false, false)
			if err != nil {
				currentLogger.Error("Failed to persist feedback message to SQLite", "error", err)
			}
			if sessionID == "default" {
				historyManager.Add(openai.ChatMessageRoleUser, feedbackMsg, id, false, false)
			}

			req.Messages = append(req.Messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: content})
			req.Messages = append(req.Messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: feedbackMsg})
			continue
		}

		if tc.IsTool && toolCallCount < cfg.CircuitBreaker.MaxToolCalls {
			toolCallCount++
			broker.Send("thinking", fmt.Sprintf("[%d] Running %s...", toolCallCount, tc.Action))

			// Persist tool call to history: native path synthesizes a text representation
			histContent := content
			if useNativePath && histContent == "" && len(nativeAssistantMsg.ToolCalls) > 0 {
				nc := nativeAssistantMsg.ToolCalls[0]
				histContent = fmt.Sprintf("{\"action\": \"%s\"}", nc.Function.Name)
				if nc.Function.Arguments != "" && len(nc.Function.Arguments) > 2 {
					args := strings.TrimSpace(nc.Function.Arguments)
					if strings.HasPrefix(args, "{") && strings.HasSuffix(args, "}") {
						inner := args[1 : len(args)-1]
						if inner != "" {
							histContent = fmt.Sprintf("{\"action\": \"%s\", %s}", nc.Function.Name, inner)
						}
					}
				}
			}
			id, err := shortTermMem.InsertMessage(sessionID, openai.ChatMessageRoleAssistant, histContent, false, true)
			if err != nil {
				currentLogger.Error("Failed to persist tool-call message to SQLite", "error", err)
			}
			if sessionID == "default" {
				historyManager.Add(openai.ChatMessageRoleAssistant, histContent, id, false, true)
			}

			broker.Send("tool_call", histContent)
			broker.Send("tool_start", tc.Action)

			if tc.Action == "execute_python" {
				flags.RequiresCoding = true
				broker.Send("coding", "Executing Python script...")
			}

			// Co-agent spawn: send a dedicated status event with a task preview
			if (tc.Action == "co_agent" || tc.Action == "co_agents") &&
				(tc.Operation == "spawn" || tc.Operation == "start" || tc.Operation == "create") {
				taskPreview := tc.Task
				if taskPreview == "" {
					taskPreview = tc.Content
				}
				if len(taskPreview) > 80 {
					taskPreview = taskPreview[:80] + "…"
				}
				broker.Send("co_agent_spawn", taskPreview)
			}

			resultContent := DispatchToolCall(ctx, tc, cfg, currentLogger, client, vault, registry, manifest, cronManager, longTermMem, shortTermMem, kg, inventoryDB, historyManager, tools.IsBusy(), surgeryPlan, guardian, sessionID, coAgentRegistry, budgetTracker)
			broker.Send("tool_output", resultContent)
			broker.Send("tool_end", tc.Action)
			lastActivity = time.Now() // Tool activity

			// Invalidate core memory cache when it was modified
			if tc.Action == "manage_memory" {
				coreMemDirty = true
			}

			// Record transition
			if lastTool != "" {
				_ = shortTermMem.RecordToolTransition(lastTool, tc.Action)
			}
			lastTool = tc.Action
			// Track recent tools for lazy schema injection (keep last 5, dedup)
			found := false
			for _, rt := range recentTools {
				if rt == tc.Action {
					found = true
					break
				}
			}
			if !found {
				recentTools = append(recentTools, tc.Action)
				if len(recentTools) > 5 {
					recentTools = recentTools[len(recentTools)-5:]
				}
			}

			// Proactive Workflow Feedback (Phase: Keep the user engaged during long chains)
			if cfg.Agent.WorkflowFeedback && !flags.IsCoAgent && sessionID == "default" {
				stepsSinceLastFeedback++
				if stepsSinceLastFeedback >= 4 {
					stepsSinceLastFeedback = 0
					feedbackPhrases := []string{
						"Ich brauche noch einen Moment, bin aber dran...",
						"Die Analyse läuft noch, einen Augenblick bitte...",
						"Ich suche noch nach weiteren Informationen...",
						"Bin gleich fertig mit der Bearbeitung...",
						"Das dauert einen Moment länger als erwartet, bleib dran...",
						"Ich verarbeite die Daten noch...",
					}
					// Simple pseudo-random selection based on time
					phrase := feedbackPhrases[time.Now().Unix()%int64(len(feedbackPhrases))]
					broker.Send("progress", phrase)
				}
			}

			// Phase D: Mood detection after each tool call
			if personalityEnabled && shortTermMem != nil {
				triggerInfo := moodTrigger()
				if strings.Contains(resultContent, "ERROR") || strings.Contains(resultContent, "error") {
					triggerInfo = moodTrigger() + " [tool error]"
				}

				if cfg.Agent.PersonalityEngineV2 {
					// ── V2: Asynchronous LLM-Based Mood Analysis ──
					// Extract recent context (e.g. last 5 messages) for the analyzer
					recentMsgs := req.Messages
					if len(recentMsgs) > 5 {
						recentMsgs = recentMsgs[len(recentMsgs)-5:]
					}
					var historyBuilder strings.Builder
					for _, m := range recentMsgs {
						historyBuilder.WriteString(fmt.Sprintf("%s: %s\n", m.Role, m.Content))
					}
					historyBuilder.WriteString(fmt.Sprintf("Tool Result: %s\n", resultContent))

					var v2Client memory.PersonalityAnalyzerClient = client
					if cfg.Agent.PersonalityV2URL != "" {
						key := cfg.Agent.PersonalityV2APIKey
						if key == "" {
							key = "dummy" // Ollama sometimes requires a non-empty string
						}
						v2Cfg := openai.DefaultConfig(key)
						v2Cfg.BaseURL = cfg.Agent.PersonalityV2URL
						v2Client = openai.NewClientWithConfig(v2Cfg)
					}

					go func(contextHistory string, tInfo string, modelName string, analyzerClient memory.PersonalityAnalyzerClient, m memory.PersonalityMeta) {
						v2Ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()

						mood, affDelta, traitDeltas, err := shortTermMem.AnalyzeMoodV2(v2Ctx, analyzerClient, modelName, contextHistory, m)
						if err != nil {
							currentLogger.Warn("[Personality V2] Failed to analyze mood", "error", err)
							return
						}

						_ = shortTermMem.LogMood(mood, tInfo)
						for trait, delta := range traitDeltas {
							_ = shortTermMem.UpdateTrait(trait, delta)
						}
						_ = shortTermMem.UpdateTrait(memory.TraitAffinity, affDelta)
						currentLogger.Debug("[Personality V2] Asynchronous mood analysis complete", "mood", mood, "affinity_delta", affDelta)
					}(historyBuilder.String(), triggerInfo, cfg.Agent.PersonalityV2Model, v2Client, meta)

				} else {
					// ── V1: Synchronous Heuristic-Based Mood Analysis ──
					mood, traitDeltas := memory.DetectMood(lastUserMsg, resultContent, meta)
					_ = shortTermMem.LogMood(mood, triggerInfo)
					for trait, delta := range traitDeltas {
						_ = shortTermMem.UpdateTrait(trait, delta)
					}
				}
				flags.PersonalityLine = shortTermMem.GetPersonalityLine(cfg.Agent.PersonalityEngineV2)
			}

			if tc.NotifyOnCompletion {
				resultContent = fmt.Sprintf(
					"[TOOL COMPLETION NOTIFICATION]\nAction: %s\nStatus: Completed\nTimestamp: %s\nOutput:\n%s",
					tc.Action,
					time.Now().Format(time.RFC3339),
					resultContent,
				)
			}
			// Make sure errors from execute_python trigger recovery mode
			if tc.Action == "execute_python" {
				if strings.Contains(resultContent, "[EXECUTION ERROR]") || strings.Contains(resultContent, "TIMEOUT") {
					flags.IsErrorState = true
					broker.Send("error_recovery", "Script error detected, retrying...")
				} else {
					flags.IsErrorState = false
				}
			}
			id, err = shortTermMem.InsertMessage(sessionID, openai.ChatMessageRoleSystem, resultContent, false, true)
			if err != nil {
				currentLogger.Error("Failed to persist tool-result message to SQLite", "error", err)
			}
			if sessionID == "default" {
				historyManager.Add(openai.ChatMessageRoleSystem, resultContent, id, false, true)
			}

			// Phase 72: Broadcast the supervisor's result to the UI (shown only in debug mode)
			broker.Send("tool_output", resultContent)

			// Phase 1: Lifecycle Handover check
			if strings.Contains(resultContent, "Maintenance Mode activated") {
				currentLogger.Info("Handover sentinel detected, Sidecar taking over...")
				// We return the response so the user sees the handover message,
				// and the loop terminates. The process stays alive in "busy" mode
				// until the sidecar triggers a reload.
				id, err := shortTermMem.InsertMessage(sessionID, resp.Choices[0].Message.Role, content, false, false)
				if err != nil {
					currentLogger.Error("Failed to persist handover message to SQLite", "error", err)
				}
				if sessionID == "default" {
					historyManager.Add(resp.Choices[0].Message.Role, content, id, false, false)
				}
				return resp, nil
			}

			if useNativePath {
				// Native path: use proper role=tool format so the LLM gets structured multi-turn context
				req.Messages = append(req.Messages, nativeAssistantMsg)
				req.Messages = append(req.Messages, openai.ChatCompletionMessage{
					Role:       openai.ChatMessageRoleTool,
					Content:    resultContent,
					ToolCallID: nativeAssistantMsg.ToolCalls[0].ID,
				})
			} else {
				req.Messages = append(req.Messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: content})
				req.Messages = append(req.Messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: resultContent})
			}

			// Support early exit for Lifeboat
			if strings.Contains(resultContent, "[LIFEBOAT_EXIT_SIGNAL]") {
				currentLogger.Info("[Sync] Early exit signal received, stopping loop.")
				return resp, nil
			}

			// 429 Mitigation: Add a delay between turns to respect rate limits (controlled by config)
			select {
			case <-time.After(time.Duration(cfg.Agent.StepDelaySeconds) * time.Second):
				// Continue to next turn
			case <-ctx.Done():
				return resp, ctx.Err()
			}
			lastResponseWasTool = true
			continue
		}

		// Final answer
		if content == "" {
			content = "[Empty Response]"
		}
		currentLogger.Debug("[Sync] Final answer", "content_len", len(content), "content_preview", Truncate(content, 200))
		broker.Send("done", "Response complete.")

		// Don't persist [Empty Response] as a real message — it pollutes future context
		isEmpty := content == "[Empty Response]"
		if !isEmpty {
			id, err := shortTermMem.InsertMessage(sessionID, resp.Choices[0].Message.Role, content, false, false)
			if err != nil {
				currentLogger.Error("Failed to persist final-answer message to SQLite", "error", err)
			}
			if sessionID == "default" {
				historyManager.Add(resp.Choices[0].Message.Role, content, id, false, false)
			}
		} else {
			currentLogger.Warn("[Sync] Skipping history persistence for empty response")
		}

		// Phase D: Final mood + trait update + milestone check at session end
		if personalityEnabled && shortTermMem != nil {
			mood, traitDeltas := memory.DetectMood(lastUserMsg, "", meta)
			_ = shortTermMem.LogMood(mood, moodTrigger())
			for trait, delta := range traitDeltas {
				_ = shortTermMem.UpdateTrait(trait, delta)
			}
			// Milestone check
			traits, tErr := shortTermMem.GetTraits()
			if tErr == nil {
				for _, m := range memory.CheckMilestones(traits) {
					has, err := shortTermMem.HasMilestone(m.Label)
					if err != nil {
						continue // skip on DB error
					}
					if !has {
						trigger := shortTermMem.GetLastMoodTrigger()
						details := fmt.Sprintf("%s %s %.2f", m.Trait, m.Direction, m.Threshold)
						if trigger != "" {
							details = fmt.Sprintf("%s (Trigger: %q)", details, trigger)
						}
						_ = shortTermMem.AddMilestone(m.Label, details)
					}
				}
			}
		}

		return resp, nil
	}
}

func dispatchInner(ctx context.Context, tc ToolCall, cfg *config.Config, logger *slog.Logger, llmClient llm.ChatClient, vault *security.Vault, registry *tools.ProcessRegistry, manifest *tools.Manifest, cronManager *tools.CronManager, longTermMem memory.VectorDB, shortTermMem *memory.SQLiteMemory, kg *memory.KnowledgeGraph, inventoryDB *sql.DB, historyMgr *memory.HistoryManager, isMaintenance bool, surgeryPlan string, guardian *security.Guardian, sessionID string, coAgentRegistry *CoAgentRegistry, budgetTracker *budget.Tracker) string {
	// Co-Agent blacklist: co-agents (identified by sessionID prefix) cannot modify memory, notes, KG, or spawn sub-agents
	isCoAgent := strings.HasPrefix(sessionID, "coagent-")
	if isCoAgent {
		switch tc.Action {
		case "manage_memory":
			if tc.Operation != "read" && tc.Operation != "query" && tc.Operation != "" {
				return `Tool Output: {"status": "error", "message": "Co-Agents cannot modify memory. Only read/query operations are allowed."}`
			}
		case "knowledge_graph":
			if tc.Operation != "query" && tc.Operation != "search" && tc.Operation != "get" && tc.Operation != "" {
				return `Tool Output: {"status": "error", "message": "Co-Agents cannot modify the knowledge graph. Only read operations are allowed."}`
			}
		case "manage_notes":
			if tc.Operation != "list" {
				return `Tool Output: {"status": "error", "message": "Co-Agents cannot modify notes. Only 'list' is allowed."}`
			}
		case "co_agent", "co_agents":
			return `Tool Output: {"status": "error", "message": "Co-Agents cannot spawn sub-agents."}`
		case "follow_up":
			return `Tool Output: {"status": "error", "message": "Co-Agents cannot schedule follow-ups."}`
		case "cron_scheduler":
			return `Tool Output: {"status": "error", "message": "Co-Agents cannot manage cron jobs."}`
		}
	}

	switch tc.Action {
	case "execute_python":
		logger.Info("LLM requested python execution", "code_len", len(tc.Code), "background", tc.Background)
		if tc.Code == "" {
			return "Tool Output: [EXECUTION ERROR] 'code' field is empty. You MUST provide Python source code in the 'code' field. Do NOT use execute_python for SSH or remote tasks — use query_inventory / execute_remote_shell instead."
		}
		if tc.Background {
			logger.Info("LLM requested background Python execution", "code_len", len(tc.Code))
			pid, err := tools.ExecutePythonBackground(tc.Code, cfg.Directories.WorkspaceDir, cfg.Directories.ToolsDir, registry)
			if err != nil {
				return fmt.Sprintf("Tool Output: [EXECUTION ERROR] starting background process: %v", err)
			}
			return fmt.Sprintf("Tool Output: Process started in background. PID=%d. Use {\"action\": \"read_process_logs\", \"pid\": %d} to check output.", pid, pid)
		}
		logger.Debug("Executing Python (foreground)", "code_preview", Truncate(tc.Code, 300))
		logger.Info("LLM requested python execution", "code_len", len(tc.Code))
		stdout, stderr, err := tools.ExecutePython(tc.Code, cfg.Directories.WorkspaceDir, cfg.Directories.ToolsDir)

		var sb strings.Builder
		sb.WriteString("Tool Output:\n")
		if stdout != "" {
			sb.WriteString(fmt.Sprintf("STDOUT:\n%s\n", stdout))
		}
		if stderr != "" {
			sb.WriteString(fmt.Sprintf("STDERR:\n%s\n", stderr))
		}
		if err != nil {
			sb.WriteString(fmt.Sprintf("[EXECUTION ERROR]: %v\n", err))
		}
		return sb.String()

	case "execute_shell":
		logger.Info("LLM requested shell execution", "command", tc.Command, "background", tc.Background)
		if tc.Background {
			pid, err := tools.ExecuteShellBackground(tc.Command, cfg.Directories.WorkspaceDir, registry)
			if err != nil {
				return fmt.Sprintf("Tool Output: [EXECUTION ERROR] starting background shell process: %v", err)
			}
			return fmt.Sprintf("Tool Output: Shell process started in background. PID=%d. Use {\"action\": \"read_process_logs\", \"pid\": %d} to check output.", pid, pid)
		}
		stdout, stderr, err := tools.ExecuteShell(tc.Command, cfg.Directories.WorkspaceDir)

		var sb strings.Builder
		sb.WriteString("Tool Output:\n")
		if stdout != "" {
			sb.WriteString(fmt.Sprintf("STDOUT:\n%s\n", stdout))
		}
		if stderr != "" {
			sb.WriteString(fmt.Sprintf("STDERR:\n%s\n", stderr))
		}
		if err != nil {
			sb.WriteString(fmt.Sprintf("[EXECUTION ERROR]: %v\n", err))
		}
		return sb.String()

	case "install_package":
		logger.Info("LLM requested package installation", "package", tc.Package)
		if tc.Package == "" {
			return "Tool Output: [EXECUTION ERROR] 'package' is required for install_package"
		}
		stdout, stderr, err := tools.InstallPackage(tc.Package, cfg.Directories.WorkspaceDir)

		var sb strings.Builder
		sb.WriteString("Tool Output:\n")
		if stdout != "" {
			sb.WriteString(fmt.Sprintf("STDOUT:\n%s\n", stdout))
		}
		if stderr != "" {
			sb.WriteString(fmt.Sprintf("STDERR:\n%s\n", stderr))
		}
		if err != nil {
			sb.WriteString(fmt.Sprintf("[EXECUTION ERROR]: %v\n", err))
		}
		return sb.String()

	case "save_tool":
		logger.Info("LLM requested tool persistence", "name", tc.Name)
		if tc.Name == "" || tc.Code == "" {
			return "Tool Output: ERROR 'name' and 'code' are required for save_tool"
		}
		if err := manifest.SaveTool(cfg.Directories.ToolsDir, tc.Name, tc.Description, tc.Code); err != nil {
			return fmt.Sprintf("Tool Output: ERROR saving tool: %v", err)
		}
		return fmt.Sprintf("Tool Output: Tool '%s' saved and registered successfully.", tc.Name)

	case "list_tools":
		logger.Info("LLM requested to list tools")
		loaded, err := manifest.Load()
		if err != nil {
			return fmt.Sprintf("Tool Output: ERROR loading tool manifest: %v", err)
		}
		var sb strings.Builder
		if len(loaded) == 0 {
			sb.WriteString("Tool Output: No custom Python tools saved yet. Use 'save_tool' to create them.\n")
		} else {
			sb.WriteString("Tool Output: Saved Reusable Tools (Python):\n")
			for k, v := range loaded {
				sb.WriteString(fmt.Sprintf("- %s: %s\n", k, v))
			}
		}

		sb.WriteString("\n[NOTE] Core capabilities like 'filesystem', 'execute_python', 'core_memory', 'query_memory', 'execute_surgery' (Maintenance only) are built-in and always available. See your system prompt and 'get_tool_manual' for details.")
		return sb.String()

	case "run_tool":
		// Intercept LLM confusing Skills for Tools
		toolPath := filepath.Join(cfg.Directories.ToolsDir, tc.Name)
		if _, err := os.Stat(toolPath); os.IsNotExist(err) {
			skillCheckName := tc.Name
			if !strings.HasSuffix(skillCheckName, ".py") {
				skillCheckName += ".py"
			}
			skillPath := filepath.Join(cfg.Directories.SkillsDir, skillCheckName)
			if _, err2 := os.Stat(skillPath); err2 == nil {
				skillBase := strings.TrimSuffix(skillCheckName, ".py")
				return fmt.Sprintf("Tool Output: ERROR '%s' is a registered SKILL, not a generic tool. You MUST use {\"action\": \"execute_skill\", \"skill\": \"%s\", \"skill_args\": {\"arg1\": \"val1\"}} (JSON object) instead.", tc.Name, skillBase)
			}
		}

		if tc.Background {
			logger.Info("LLM requested background tool execution", "name", tc.Name)
			pid, err := tools.RunToolBackground(tc.Name, tc.GetArgs(), cfg.Directories.WorkspaceDir, cfg.Directories.ToolsDir, registry)
			if err != nil {
				return fmt.Sprintf("Tool Output: ERROR starting background tool: %v", err)
			}
			return fmt.Sprintf("Tool Output: Tool started in background. PID=%d. Use {\"action\": \"read_process_logs\", \"pid\": %d} to check output.", pid, pid)
		}
		logger.Info("LLM requested tool execution", "name", tc.Name)
		stdout, stderr, err := tools.RunTool(tc.Name, tc.GetArgs(), cfg.Directories.WorkspaceDir, cfg.Directories.ToolsDir)
		errStr := ""
		if err != nil {
			errStr = err.Error()
		}
		return fmt.Sprintf("Tool Output:\nSTDOUT:\n%s\nSTDERR:\n%s\nERROR:\n%s\n", stdout, stderr, errStr)

	case "list_processes":
		logger.Info("LLM requested process list")
		list := registry.List()
		if len(list) == 0 {
			return "Tool Output: No active background processes."
		}
		var sb strings.Builder
		sb.WriteString("Tool Output: Active processes:\n")
		for _, p := range list {
			pid, _ := p["pid"].(int)
			started, _ := p["started"].(string)
			sb.WriteString(fmt.Sprintf("- PID: %d, Started: %s\n", pid, started))
		}
		return sb.String()

	case "stop_process":
		logger.Info("LLM requested process stop", "pid", tc.PID)
		if err := registry.Terminate(tc.PID); err != nil {
			return fmt.Sprintf("Tool Output: ERROR stopping process %d: %v", tc.PID, err)
		}
		return fmt.Sprintf("Tool Output: Process %d stopped.", tc.PID)

	case "read_process_logs":
		logger.Info("LLM requested process logs", "pid", tc.PID)
		proc, ok := registry.Get(tc.PID)
		if !ok {
			return fmt.Sprintf("Tool Output: ERROR process %d not found", tc.PID)
		}
		return fmt.Sprintf("Tool Output: [LOGS for PID %d]\n%s", tc.PID, proc.ReadOutput())

	case "query_memory":
		searchContent := tc.Content
		if searchContent == "" {
			searchContent = tc.Query
		}
		logger.Info("LLM requested memory search", "content", searchContent)
		if searchContent == "" {
			return `Tool Output: {"status": "error", "message": "'content' or 'query' (search query) is required"}`
		}
		// Phase 69: Implement semantic query against the VectorDB
		results, _, err := longTermMem.SearchSimilar(searchContent, 5)
		if err != nil {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "VectorDB search failed: %v"}`, err)
		}
		if len(results) == 0 {
			return `Tool Output: {"status": "success", "message": "No matching long-term memories found."}`
		}
		b, _ := json.Marshal(results)
		return fmt.Sprintf(`Tool Output: {"status": "success", "data": %s}`, string(b))
	case "manage_updates":
		logger.Info("LLM requested update management", "operation", tc.Operation)
		switch tc.Operation {
		case "check":
			// git fetch origin main --quiet
			_, err := runGitCommand(filepath.Dir(cfg.ConfigPath), "fetch", "origin", "main", "--quiet")
			if err != nil {
				return fmt.Sprintf(`Tool Output: {"status": "error", "message": "Failed to fetch updates: %v"}`, err)
			}

			countOut, err := runGitCommand(filepath.Dir(cfg.ConfigPath), "rev-list", "HEAD..origin/main", "--count")
			if err != nil {
				return fmt.Sprintf(`Tool Output: {"status": "error", "message": "Failed to check update count: %v"}`, err)
			}
			countStr := strings.TrimSpace(string(countOut))
			count, _ := strconv.Atoi(countStr)

			if count == 0 {
				return `Tool Output: {"status": "success", "update_available": false, "message": "AuraGo is up to date."}`
			}

			logOut, _ := runGitCommand(filepath.Dir(cfg.ConfigPath), "log", "HEAD..origin/main", "--oneline", "-n", "10")

			return fmt.Sprintf(`Tool Output: {"status": "success", "update_available": true, "count": %d, "changelog": %q}`, count, string(logOut))

		case "install":
			logger.Warn("LLM requested update installation")
			updateScript := filepath.Join(filepath.Dir(cfg.ConfigPath), "update.sh")
			if _, err := os.Stat(updateScript); err != nil {
				return `Tool Output: {"status": "error", "message": "update.sh not found in application directory"}`
			}

			// Run ./update.sh --yes
			updateCmd := exec.Command("/bin/bash", "./update.sh", "--yes")
			updateCmd.Dir = filepath.Dir(cfg.ConfigPath)
			// Ensure environment is passed for update script too
			home, _ := os.UserHomeDir()
			if home != "" {
				updateCmd.Env = append(os.Environ(), "HOME="+home)
			}
			// Start update script. It will handle the rest, potentially killing this process.
			if err := updateCmd.Start(); err != nil {
				return fmt.Sprintf(`Tool Output: {"status": "error", "message": "Failed to start update script: %v"}`, err)
			}
			return `Tool Output: {"status": "success", "message": "Update initiated. The system will restart and apply changes shortly."}`

		default:
			return `Tool Output: {"status": "error", "message": "Invalid operation. Use 'check' or 'install'."}`
		}

	case "archive_memory":
		logger.Info("LLM requested memory archival", "id", tc.ID)
		return "Tool Output: " + runMemoryOrchestrator(tc, cfg, logger, llmClient, longTermMem, shortTermMem, kg)

	case "optimize_memory":
		logger.Info("LLM requested memory optimization")
		return "Tool Output: " + runMemoryOrchestrator(tc, cfg, logger, llmClient, longTermMem, shortTermMem, kg)

	case "manage_knowledge", "knowledge_graph":
		logger.Info("LLM requested knowledge graph operation", "op", tc.Operation)
		// Phase 69: Route to actual KnowledgeGraph implementation
		switch tc.Operation {
		case "add_node":
			err := kg.AddNode(tc.ID, tc.Label, tc.Properties)
			if err != nil {
				return fmt.Sprintf(`Tool Output: {"status": "error", "message": "%v"}`, err)
			}
			return `Tool Output: {"status": "success", "message": "Node added to graph"}`
		case "add_edge":
			err := kg.AddEdge(tc.Source, tc.Target, tc.Relation, tc.Properties)
			if err != nil {
				return fmt.Sprintf(`Tool Output: {"status": "error", "message": "%v"}`, err)
			}
			return `Tool Output: {"status": "success", "message": "Edge added to graph"}`
		case "delete_node":
			err := kg.DeleteNode(tc.ID)
			if err != nil {
				return fmt.Sprintf(`Tool Output: {"status": "error", "message": "%v"}`, err)
			}
			return `Tool Output: {"status": "success", "message": "Node deleted"}`
		case "delete_edge":
			err := kg.DeleteEdge(tc.Source, tc.Target, tc.Relation)
			if err != nil {
				return fmt.Sprintf(`Tool Output: {"status": "error", "message": "%v"}`, err)
			}
			return `Tool Output: {"status": "success", "message": "Edge deleted"}`
		case "search":
			res := kg.Search(tc.Content)
			return fmt.Sprintf("Tool Output: %s", res)
		case "optimize":
			res := runMemoryOrchestrator(tc, cfg, logger, llmClient, longTermMem, shortTermMem, kg)
			return fmt.Sprintf("Tool Output: %s", res)

		default:
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "Unknown graph operation: %s"}`, tc.Operation)
		}

	case "manage_memory", "core_memory":
		// Handle synonyms for 'fact'
		fact := tc.Fact
		if fact == "" {
			if tc.MemoryValue != "" {
				fact = tc.MemoryValue
			} else if tc.MemoryKey != "" {
				fact = tc.MemoryKey
			} else if tc.Value != "" {
				fact = tc.Value
			} else if tc.Content != "" {
				fact = tc.Content
			}
		}
		// When LLM uses separate key+value fields, combine into a meaningful fact (e.g. "agent_name: Nova")
		// Only for add/update, and only when key is a descriptive word (not a numeric ID)
		{
			op := strings.ToLower(tc.Operation)
			keyField := tc.Key
			if keyField == "" {
				keyField = tc.MemoryKey
			}
			if (op == "add" || op == "update") && keyField != "" && fact != "" && fact != keyField {
				if _, parseErr := strconv.ParseInt(keyField, 10, 64); parseErr != nil {
					// Key is not a numeric ID — prefix fact with key for context
					if !strings.HasPrefix(strings.ToLower(fact), strings.ToLower(keyField)+":") &&
						!strings.HasPrefix(strings.ToLower(fact), strings.ToLower(keyField)+" ") {
						fact = keyField + ": " + fact
					}
				}
			}
		}

		logger.Info("LLM requested core memory management", "op", tc.Operation, "fact", fact)
		if tc.Operation == "" {
			return `Tool Output: {"status": "error", "message": "'operation' is required for manage_memory"}`
		}
		var memID int64
		fmt.Sscanf(tc.ID, "%d", &memID)
		result, err := tools.ManageCoreMemory(tc.Operation, fact, memID, shortTermMem, cfg.Agent.CoreMemoryMaxEntries, cfg.Agent.CoreMemoryCapMode)
		if err != nil {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "%v"}`, err)
		}
		return fmt.Sprintf("Tool Output: %s", result)

	case "get_secret", "secrets_vault":
		op := strings.TrimSpace(strings.ToLower(tc.Operation))
		if op == "store" || op == "set" || (tc.Action == "set_secret") {
			logger.Info("LLM requested secret storage", "key", tc.Key)
			if tc.Key == "" || tc.Value == "" {
				return `Tool Output: {"status": "error", "message": "'key' and 'value' are required for set_secret/store"}`
			}
			err := vault.WriteSecret(tc.Key, tc.Value)
			if err != nil {
				return fmt.Sprintf(`Tool Output: {"status": "error", "message": "%v"}`, err)
			}
			return fmt.Sprintf(`Tool Output: {"status": "success", "message": "Secret '%s' stored safely."}`, tc.Key)
		}

		// Default: read/list
		logger.Info("LLM requested secret retrieval", "key", tc.Key)
		if tc.Key == "" {
			// List available secret keys when no key is specified
			keys, err := vault.ListKeys()
			if err != nil {
				return fmt.Sprintf(`Tool Output: {"status": "error", "message": "%v"}`, err)
			}
			b, _ := json.Marshal(keys)
			return fmt.Sprintf(`Tool Output: {"status": "success", "message": "Stored secret keys (use get_secret with 'key' to retrieve a value)", "keys": %s}`, string(b))
		}
		secret, err := vault.ReadSecret(tc.Key)
		if err != nil {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "%v"}`, err)
		}
		// JSON-encode the secret value to prevent injection from special characters
		safeVal, _ := json.Marshal(secret)
		return fmt.Sprintf(`Tool Output: {"status": "success", "key": "%s", "value": %s}`, tc.Key, string(safeVal))

	case "set_secret":
		logger.Info("LLM requested secret storage", "key", tc.Key)
		if tc.Key == "" || tc.Value == "" {
			return `Tool Output: {"status": "error", "message": "'key' and 'value' are required for set_secret"}`
		}
		err := vault.WriteSecret(tc.Key, tc.Value)
		if err != nil {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "%v"}`, err)
		}
		return fmt.Sprintf(`Tool Output: {"status": "success", "message": "Secret '%s' stored safely."}`, tc.Key)

	case "filesystem", "filesystem_op":
		// Parameter robustness: handle 'path' and 'dest' aliases frequently hallucinated by LLMs
		fpath := tc.FilePath
		if fpath == "" {
			fpath = tc.Path
		}
		fdest := tc.Destination
		if fdest == "" {
			fdest = tc.Dest
		}

		op := strings.TrimSpace(strings.ToLower(tc.Operation))
		if op == "list" || op == "ls" {
			op = "list_dir"
		}
		logger.Info("LLM requested filesystem operation", "op", op, "path", fpath, "dest", fdest)
		return tools.ExecuteFilesystem(op, fpath, fdest, tc.Content, cfg.Directories.WorkspaceDir)

	case "api_request":
		logger.Info("LLM requested generic API request", "url", tc.URL)
		return tools.ExecuteAPIRequest(tc.Method, tc.URL, tc.Body, tc.Headers)

	case "koofr", "koofr_api", "koofr_op":
		fpath := tc.FilePath
		if fpath == "" {
			fpath = tc.Path
		}
		fdest := tc.Destination
		if fdest == "" {
			fdest = tc.Dest
		}
		logger.Info("LLM requested koofr operation", "op", tc.Operation, "path", fpath, "dest", fdest)
		koofrCfg := tools.KoofrConfig{
			BaseURL:     cfg.Koofr.BaseURL,
			Username:    cfg.Koofr.Username,
			AppPassword: cfg.Koofr.AppPassword,
		}
		return tools.ExecuteKoofr(koofrCfg, tc.Operation, fpath, fdest, tc.Content)

	case "google_workspace", "gworkspace":
		op := tc.Operation
		if op == "" {
			op = tc.Action // Fallback if LLM puts it in action
		}
		logger.Info("LLM requested google_workspace operation", "op", op, "doc_id", tc.DocumentID)
		gConfig := tools.GoogleWorkspaceConfig{
			Action:     op,
			MaxResults: tc.MaxResults,
			DocumentID: tc.DocumentID,
			Title:      tc.Title,
			Text:       tc.Text,
			Append:     tc.Append,
		}
		res, err := tools.ExecuteGoogleWorkspace(vault, cfg.Directories.WorkspaceDir, cfg.Directories.ToolsDir, gConfig)
		if err != nil {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "%v"}`, err)
		}
		return res

	case "query_inventory":
		queryTag := tc.Tag
		if queryTag == "" {
			queryTag = tc.Tags
		}
		logger.Info("LLM requested inventory query", "tag", queryTag, "name", tc.Hostname)
		devices, err := inventory.QueryDevices(inventoryDB, queryTag, tc.DeviceType, tc.Hostname)
		if err != nil {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "Failed to query inventory: %v"}`, err)
		}
		b, _ := json.Marshal(devices)
		return fmt.Sprintf(`Tool Output: {"status": "success", "tag": "%s", "device_type": "%s", "name_match": "%s", "devices": %s}`, tc.Tag, tc.DeviceType, tc.Hostname, string(b))

	case "execute_remote_shell", "remote_execution":
		logger.Info("LLM requested remote shell execution", "server_id", tc.ServerID, "command", tc.Command)
		if tc.ServerID == "" || tc.Command == "" {
			return `Tool Output: {"status": "error", "message": "'server_id' and 'command' are required"}`
		}
		device, err := inventory.GetDeviceByID(inventoryDB, tc.ServerID)
		if err != nil {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "Device not found: %v"}`, err)
		}
		secret, err := vault.ReadSecret(device.VaultSecretID)
		if err != nil {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "Failed to fetch secret: %v"}`, err)
		}
		rCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		output, err := remote.ExecuteRemoteCommand(rCtx, device.Name, device.Port, device.Username, []byte(secret), tc.Command)
		if err != nil {
			safeOutput, _ := json.Marshal(output)
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "Remote execution failed", "output": %s, "error": "%v"}`, string(safeOutput), err)
		}
		safeOutput, _ := json.Marshal(output)
		return fmt.Sprintf(`Tool Output: {"status": "success", "output": %s}`, string(safeOutput))

	case "transfer_remote_file":
		logger.Info("LLM requested remote file transfer", "server_id", tc.ServerID, "direction", tc.Direction)
		if tc.ServerID == "" || tc.Direction == "" || tc.LocalPath == "" || tc.RemotePath == "" {
			return `Tool Output: {"status": "error", "message": "'server_id', 'direction', 'local_path', and 'remote_path' are required"}`
		}
		// Sanitize and restrict local path
		absLocal, err := filepath.Abs(tc.LocalPath)
		if err != nil {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "Invalid local path: %v"}`, err)
		}
		workspaceWorkdir := filepath.Join(cfg.Directories.WorkspaceDir, "workdir")
		if !strings.HasPrefix(strings.ToLower(absLocal), strings.ToLower(workspaceWorkdir)) {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "Permission denied: local_path must be within %s"}`, workspaceWorkdir)
		}

		device, err := inventory.GetDeviceByID(inventoryDB, tc.ServerID)
		if err != nil {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "Device not found: %v"}`, err)
		}
		secret, err := vault.ReadSecret(device.VaultSecretID)
		if err != nil {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "Failed to fetch secret: %v"}`, err)
		}
		rCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		err = remote.TransferFile(rCtx, device.Name, device.Port, device.Username, []byte(secret), absLocal, tc.RemotePath, tc.Direction)
		if err != nil {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "File transfer failed: %v"}`, err)
		}
		return fmt.Sprintf(`Tool Output: {"status": "success", "message": "File %s successfully"}`, tc.Direction)

	case "manage_schedule", "cron_scheduler":
		logger.Info("LLM requested cron management", "operation", tc.Operation)
		result, err := cronManager.ManageSchedule(tc.Operation, tc.ID, tc.CronExpr, tc.TaskPrompt)
		if err != nil {
			return fmt.Sprintf("Tool Output: ERROR in manage_schedule: %v", err)
		}
		return result

	case "schedule_cron":
		logger.Info("LLM requested cron scheduling", "expr", tc.CronExpr)
		result, err := cronManager.ManageSchedule("add", "", tc.CronExpr, tc.TaskPrompt)
		if err != nil {
			return fmt.Sprintf("Tool Output: ERROR scheduling cron: %v", err)
		}
		return result

	case "list_cron_jobs":
		logger.Info("LLM requested cron job list")
		result, _ := cronManager.ManageSchedule("list", "", "", "")
		return result

	case "remove_cron_job":
		logger.Info("LLM requested cron job removal", "id", tc.ID)
		result, _ := cronManager.ManageSchedule("remove", tc.ID, "", "")
		return result

	case "list_skills":
		logger.Info("LLM requested to list skills")
		skills, err := tools.ListSkills(cfg.Directories.SkillsDir, cfg.Agent.EnableGoogleWorkspace)
		if err != nil {
			return fmt.Sprintf("Tool Output: ERROR listing skills: %v", err)
		}
		if len(skills) == 0 {
			return "Tool Output: No internal skills found."
		}
		b, err := json.MarshalIndent(skills, "", "  ")
		if err != nil {
			return fmt.Sprintf("Tool Output: ERROR serializing skills list: %v", err)
		}
		return fmt.Sprintf("Tool Output: Internal Skills Configuration:\n%s", string(b))

	case "execute_skill":
		logger.Info("LLM requested skill execution", "skill", tc.Skill, "args", tc.SkillArgs, "params", tc.Params)
		// Robust argument lookup: handle both 'skill_args' and 'params'
		args := tc.SkillArgs
		if args == nil {
			args = tc.Params
		}

		skillName := tc.Skill
		if skillName == "" && args != nil {
			// Aggressive recovery: Check if LLM nested the skill name inside arguments
			for _, key := range []string{"skill", "skill_name", "name", "tool"} {
				if s, ok := args[key].(string); ok && s != "" {
					skillName = s
					logger.Info("[Recovery] Found nested skill name in arguments", "key", key, "skill", skillName)
					break
				}
			}
		}

		if skillName == "" {
			return "Tool Output: ERROR 'skill' name is required. Use {\"action\": \"execute_skill\", \"skill\": \"name\", \"params\": {...}}"
		}

		// Unwrap skill_args if the LLM nested the actual parameters under that key.
		// e.g. {"skill_name": "ddg_search", "skill_args": {"query": "..."}} → {"query": "..."}
		if innerArgs, ok := args["skill_args"].(map[string]interface{}); ok && len(innerArgs) > 0 {
			args = innerArgs
		} else {
			// Clean up metadata keys that aren't real skill parameters
			cleanArgs := make(map[string]interface{}, len(args))
			metaKeys := map[string]bool{"skill_name": true, "skill": true, "name": true, "tool": true, "action": true}
			for k, v := range args {
				if !metaKeys[k] {
					cleanArgs[k] = v
				}
			}
			args = cleanArgs
		}

		cleanSkillName := strings.TrimSuffix(skillName, ".py")
		switch cleanSkillName {
		case "web_scraper":
			urlStr, _ := args["url"].(string)
			return tools.ExecuteWebScraper(urlStr)
		case "wikipedia_search":
			queryStr, _ := args["query"].(string)
			langStr, _ := args["language"].(string)
			return tools.ExecuteWikipediaSearch(queryStr, langStr)
		case "ddg_search":
			queryStr, _ := args["query"].(string)
			maxRes, ok := args["max_results"].(float64)
			if !ok {
				maxRes = 5
			}
			return tools.ExecuteDDGSearch(queryStr, int(maxRes))
		case "virustotal_scan":
			resource, _ := args["resource"].(string)
			return tools.ExecuteVirusTotalScan(cfg.VirusTotal.APIKey, resource)
		case "git_backup_restore":
			reqJSON, _ := json.Marshal(args)
			var req tools.GitBackupRequest
			json.Unmarshal(reqJSON, &req)
			return tools.ExecuteGit(cfg.Directories.WorkspaceDir, req)
		case "google_workspace":
			op, _ := args["operation"].(string)
			limit, _ := args["limit"].(float64)
			docID, _ := args["document_id"].(string)
			title, _ := args["title"].(string)
			text, _ := args["text"].(string)
			appendMode, _ := args["append"].(bool)
			gConfig := tools.GoogleWorkspaceConfig{
				Action:     op,
				MaxResults: int(limit),
				DocumentID: docID,
				Title:      title,
				Text:       text,
				Append:     appendMode,
			}
			res, err := tools.ExecuteGoogleWorkspace(vault, cfg.Directories.WorkspaceDir, cfg.Directories.ToolsDir, gConfig)
			if err != nil {
				return fmt.Sprintf(`Tool Output: {"status": "error", "message": "%v"}`, err)
			}
			return res
		}

		res, err := tools.ExecuteSkill(cfg.Directories.SkillsDir, cfg.Directories.WorkspaceDir, skillName, args, cfg.Agent.EnableGoogleWorkspace)
		if err != nil {
			return fmt.Sprintf("Tool Output: ERROR executing skill: %v\nOutput: %s", err, res)
		}
		return fmt.Sprintf("Tool Output: %s", res)

	case "follow_up":
		logger.Info("LLM requested follow-up", "prompt", tc.TaskPrompt)
		if tc.TaskPrompt == "" {
			return "Tool Output: ERROR 'task_prompt' is required for follow_up"
		}

		// Trigger background follow-up request
		go func(prompt string, port int) {
			time.Sleep(2 * time.Second) // Let current response finish
			url := fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port)

			payload := map[string]interface{}{
				"model":  "aurago",
				"stream": false,
				"messages": []map[string]string{
					{"role": "user", "content": prompt},
				},
			}

			body, _ := json.Marshal(payload)
			req, err := http.NewRequest("POST", url, strings.NewReader(string(body)))
			if err != nil {
				logger.Error("Failed to create follow-up request", "error", err)
				return
			}

			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Internal-FollowUp", "true")

			client := &http.Client{Timeout: 10 * time.Minute}
			resp, err := client.Do(req)
			if err != nil {
				logger.Error("Follow-up request failed", "error", err)
				return
			}
			defer resp.Body.Close()
			logger.Info("Follow-up triggered successfully", "status", resp.Status)
		}(tc.TaskPrompt, cfg.Server.Port)

		return "Tool Output: Follow-up scheduled. I will continue in the background immediately after this message."

	case "get_tool_manual":
		logger.Info("LLM requested tool manual", "name", tc.ToolName)
		if tc.ToolName == "" {
			return "Tool Output: ERROR 'tool_name' is required"
		}

		// Fallback for LLMs getting creative with the manual name
		cleanName := strings.TrimSuffix(tc.ToolName, ".md")
		cleanName = strings.TrimSuffix(cleanName, "_tool_manual")
		manualPath := filepath.Join(cfg.Directories.PromptsDir, "tools_manuals", cleanName+".md")
		data, err := os.ReadFile(manualPath)
		if err != nil {
			return fmt.Sprintf("Tool Output: ERROR could not read manual for '%s': %v", tc.ToolName, err)
		}
		return fmt.Sprintf("Tool Output: [MANUAL FOR %s]\n%s", tc.ToolName, string(data))

	case "execute_surgery":
		if !isMaintenance {
			return "Tool Output: ERROR 'execute_surgery' can ONLY be used when in Maintenance mode (Lifeboat). You are currently in Supervisor mode. You MUST use 'initiate_handover' first to propose a plan and switch to Maintenance mode for complex code changes."
		}

		// Robustness: handle both 'task_prompt' and 'content' for the plan
		plan := tc.TaskPrompt
		if plan == "" {
			plan = tc.Content
		}

		logger.Info("LLM requested surgery via Gemini CLI", "plan_len", len(plan), "prompt_preview", Truncate(plan, 100))
		if plan == "" {
			return "Tool Output: ERROR surgery plan is required (via 'task_prompt' or 'content')"
		}
		// Using external Gemini CLI via the surgery tool
		res, err := tools.ExecuteSurgery(plan, cfg.Directories.WorkspaceDir, logger)
		if err != nil {
			return fmt.Sprintf("Tool Output: ERROR surgery failed: %v\nOutput: %s", err, res)
		}
		return fmt.Sprintf("Tool Output: Surgery successful.\nDetails:\n%s", res)

	case "exit_lifeboat":
		if !isMaintenance {
			return "Tool Output: ERROR 'exit_lifeboat' can only be used when already in maintenance mode. You are currently in the standard Supervisor mode."
		}
		logger.Info("LLM requested to exit lifeboat")
		tools.SetBusy(false)
		return "Tool Output: [LIFEBOAT_EXIT_SIGNAL] Maintenance complete. Attempting to return to main supervisor."

	case "initiate_handover":
		if isMaintenance {
			return "Tool Output: ERROR You are already in Lifeboat mode. Maintenance is active. Use 'exit_lifeboat' to return to the supervisor or 'execute_surgery' for code changes."
		}
		logger.Info("LLM requested lifeboat handover", "plan_len", len(tc.TaskPrompt))
		return tools.InitiateLifeboatHandover(tc.TaskPrompt, cfg)

	case "get_system_metrics", "system_metrics":
		logger.Info("LLM requested system metrics")
		return "Tool Output: " + tools.GetSystemMetrics()

	case "send_notification", "notification_center":
		logger.Info("LLM requested notification", "channel", tc.Channel, "title", tc.Title)
		// Use discord bridge (tools.DiscordSend) to avoid import cycle
		var discordSend tools.DiscordSendFunc
		if cfg.Discord.Enabled {
			discordSend = func(channelID, content string) error {
				return tools.DiscordSend(channelID, content, logger)
			}
		}
		priority := tc.Tag // reuse existing Tag field for priority
		return "Tool Output: " + tools.SendNotification(cfg, logger, tc.Channel, tc.Title, tc.Message, priority, discordSend)

	case "manage_processes", "process_management":
		logger.Info("LLM requested process management", "op", tc.Operation)
		return "Tool Output: " + tools.ManageProcesses(tc.Operation, int32(tc.PID))

	case "register_device", "register_server":
		logger.Info("LLM requested device registration", "name", tc.Hostname)
		tags := services.ParseTags(tc.Tags)
		deviceType := tc.DeviceType
		if deviceType == "" {
			deviceType = "server"
		}

		// If LLM hallucinated, putting IP in Hostname and leaving IPAddress empty:
		if tc.IPAddress == "" && net.ParseIP(tc.Hostname) != nil {
			tc.IPAddress = tc.Hostname
		}

		id, err := services.RegisterDevice(inventoryDB, vault, tc.Hostname, deviceType, tc.IPAddress, tc.Port, tc.Username, tc.Password, tc.PrivateKeyPath, tc.Description, tags)
		if err != nil {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "Failed to register device: %v"}`, err)
		}
		return fmt.Sprintf(`Tool Output: {"status": "success", "message": "Device registered successfully", "id": "%s"}`, id)

	case "pin_message":
		logger.Info("LLM requested message pinning", "id", tc.ID, "pinned", tc.Pinned)
		if tc.ID == "" {
			return `Tool Output: {"status": "error", "message": "'id' is required for pin_message"}`
		}
		// Try to parse ID as int64
		var msgID int64
		fmt.Sscanf(tc.ID, "%d", &msgID)
		if msgID == 0 {
			return `Tool Output: {"status": "error", "message": "Invalid 'id' format"}`
		}

		err := shortTermMem.SetMessagePinned(msgID, tc.Pinned)
		if err != nil {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "Failed to update SQLite: %v"}`, err)
		}
		if historyMgr != nil {
			_ = historyMgr.SetPinned(msgID, tc.Pinned)
		}
		status := "pinned"
		if !tc.Pinned {
			status = "unpinned"
		}
		return fmt.Sprintf(`Tool Output: {"status": "success", "message": "Message %d %s successfully."}`, msgID, status)

	case "fetch_email", "check_email":
		if !cfg.Email.Enabled {
			return `Tool Output: {"status": "error", "message": "Email is not enabled. Configure the email section in config.yaml."}`
		}
		logger.Info("LLM requested email fetch", "folder", tc.Folder)
		folder := tc.Folder
		if folder == "" {
			folder = cfg.Email.WatchFolder
		}
		limit := tc.Limit
		if limit <= 0 {
			limit = 10
		}
		messages, err := tools.FetchEmails(
			cfg.Email.IMAPHost, cfg.Email.IMAPPort,
			cfg.Email.Username, cfg.Email.Password,
			folder, limit, logger,
		)
		if err != nil {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "IMAP fetch failed: %v"}`, err)
		}
		// Guardian: scan each message body for injection attempts
		if guardian != nil {
			for i := range messages {
				combined := messages[i].From + " " + messages[i].Subject + " " + messages[i].Body
				scanRes := guardian.ScanForInjection(combined)
				if scanRes.Level >= security.ThreatHigh {
					logger.Warn("[Email] Guardian HIGH threat in message", "uid", messages[i].UID, "from", messages[i].From, "threat", scanRes.Level.String())
					messages[i].Body = "[REDACTED by Guardian — injection attempt detected]"
					messages[i].Subject = "[SANITIZED] " + messages[i].Subject
					messages[i].Snippet = "[REDACTED]"
				} else {
					messages[i].Body = guardian.SanitizeToolOutput("email", messages[i].Body)
				}
			}
		}
		result := tools.EmailResult{Status: "success", Count: len(messages), Data: messages}
		return "Tool Output: " + tools.EncodeEmailResult(result)

	case "send_email":
		if !cfg.Email.Enabled {
			return `Tool Output: {"status": "error", "message": "Email is not enabled. Configure the email section in config.yaml."}`
		}
		to := tc.To
		if to == "" {
			return `Tool Output: {"status": "error", "message": "'to' (recipient address) is required"}`
		}
		subject := tc.Subject
		if subject == "" {
			subject = "(no subject)"
		}
		body := tc.Body
		if body == "" {
			body = tc.Content
		}
		logger.Info("LLM requested email send", "to", to, "subject", subject)
		var sendErr error
		if cfg.Email.SMTPPort == 465 {
			sendErr = tools.SendEmailTLS(cfg.Email.SMTPHost, cfg.Email.SMTPPort, cfg.Email.Username, cfg.Email.Password, cfg.Email.FromAddress, to, subject, body, logger)
		} else {
			sendErr = tools.SendEmail(cfg.Email.SMTPHost, cfg.Email.SMTPPort, cfg.Email.Username, cfg.Email.Password, cfg.Email.FromAddress, to, subject, body, logger)
		}
		if sendErr != nil {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "SMTP send failed: %v"}`, sendErr)
		}
		result := tools.EmailResult{Status: "success", Message: fmt.Sprintf("Email sent to %s", to)}
		return "Tool Output: " + tools.EncodeEmailResult(result)

	case "send_discord":
		if !cfg.Discord.Enabled {
			return `Tool Output: {"status": "error", "message": "Discord is not enabled. Configure the discord section in config.yaml."}`
		}
		channelID := tc.ChannelID
		if channelID == "" {
			channelID = cfg.Discord.DefaultChannelID
		}
		if channelID == "" {
			return `Tool Output: {"status": "error", "message": "'channel_id' is required (or set default_channel_id in config)"}`
		}
		message := tc.Message
		if message == "" {
			message = tc.Content
		}
		if message == "" {
			message = tc.Body
		}
		if message == "" {
			return `Tool Output: {"status": "error", "message": "'message' (or 'content') is required"}`
		}
		logger.Info("LLM requested Discord send", "channel", channelID)
		if err := tools.DiscordSend(channelID, message, logger); err != nil {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "Discord send failed: %v"}`, err)
		}
		return fmt.Sprintf(`Tool Output: {"status": "success", "message": "Message sent to Discord channel %s"}`, channelID)

	case "fetch_discord":
		if !cfg.Discord.Enabled {
			return `Tool Output: {"status": "error", "message": "Discord is not enabled. Configure the discord section in config.yaml."}`
		}
		channelID := tc.ChannelID
		if channelID == "" {
			channelID = cfg.Discord.DefaultChannelID
		}
		if channelID == "" {
			return `Tool Output: {"status": "error", "message": "'channel_id' is required (or set default_channel_id in config)"}`
		}
		limit := tc.Limit
		if limit <= 0 {
			limit = 10
		}
		logger.Info("LLM requested Discord message fetch", "channel", channelID, "limit", limit)
		msgs, err := tools.DiscordFetch(channelID, limit, logger)
		if err != nil {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "Discord fetch failed: %v"}`, err)
		}
		// Guardian-sanitize external content
		if guardian != nil {
			for i := range msgs {
				scanRes := guardian.ScanForInjection(msgs[i].Author + " " + msgs[i].Content)
				if scanRes.Level >= security.ThreatHigh {
					logger.Warn("[Discord] Guardian HIGH threat in message", "author", msgs[i].Author, "threat", scanRes.Level.String())
					msgs[i].Content = "[REDACTED by Guardian — injection attempt detected]"
				} else {
					msgs[i].Content = guardian.SanitizeToolOutput("discord", msgs[i].Content)
				}
			}
		}
		data, _ := json.Marshal(map[string]interface{}{
			"status": "success",
			"count":  len(msgs),
			"data":   msgs,
		})
		return "Tool Output: " + string(data)

	case "list_discord_channels":
		if !cfg.Discord.Enabled {
			return `Tool Output: {"status": "error", "message": "Discord is not enabled."}`
		}
		guildID := cfg.Discord.GuildID
		if guildID == "" {
			return `Tool Output: {"status": "error", "message": "'guild_id' must be set in config.yaml"}`
		}
		logger.Info("LLM requested Discord channel list", "guild", guildID)
		channels, err := tools.DiscordListChannels(guildID, logger)
		if err != nil {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "Channel list failed: %v"}`, err)
		}
		data, _ := json.Marshal(map[string]interface{}{
			"status": "success",
			"count":  len(channels),
			"data":   channels,
		})
		return "Tool Output: " + string(data)

	case "manage_notes", "notes", "todo":
		logger.Info("LLM requested notes/todo management", "op", tc.Operation)
		if shortTermMem == nil {
			return `Tool Output: {"status": "error", "message": "Notes storage not available"}`
		}
		switch tc.Operation {
		case "add":
			if tc.Title == "" {
				return `Tool Output: {"status": "error", "message": "'title' is required for add"}`
			}
			id, err := shortTermMem.AddNote(tc.Category, tc.Title, tc.Content, tc.Priority, tc.DueDate)
			if err != nil {
				return fmt.Sprintf(`Tool Output: {"status": "error", "message": "%v"}`, err)
			}
			return fmt.Sprintf(`Tool Output: {"status": "success", "message": "Note created", "id": %d}`, id)
		case "list":
			notes, err := shortTermMem.ListNotes(tc.Category, tc.Done)
			if err != nil {
				return fmt.Sprintf(`Tool Output: {"status": "error", "message": "%v"}`, err)
			}
			return fmt.Sprintf(`Tool Output: {"status": "success", "count": %d, "notes": %s}`, len(notes), memory.FormatNotesJSON(notes))
		case "update":
			if tc.NoteID <= 0 {
				return `Tool Output: {"status": "error", "message": "'note_id' is required for update"}`
			}
			err := shortTermMem.UpdateNote(tc.NoteID, tc.Title, tc.Content, tc.Category, tc.Priority, tc.DueDate)
			if err != nil {
				return fmt.Sprintf(`Tool Output: {"status": "error", "message": "%v"}`, err)
			}
			return fmt.Sprintf(`Tool Output: {"status": "success", "message": "Note %d updated"}`, tc.NoteID)
		case "toggle":
			if tc.NoteID <= 0 {
				return `Tool Output: {"status": "error", "message": "'note_id' is required for toggle"}`
			}
			newState, err := shortTermMem.ToggleNoteDone(tc.NoteID)
			if err != nil {
				return fmt.Sprintf(`Tool Output: {"status": "error", "message": "%v"}`, err)
			}
			return fmt.Sprintf(`Tool Output: {"status": "success", "note_id": %d, "done": %t}`, tc.NoteID, newState)
		case "delete":
			if tc.NoteID <= 0 {
				return `Tool Output: {"status": "error", "message": "'note_id' is required for delete"}`
			}
			err := shortTermMem.DeleteNote(tc.NoteID)
			if err != nil {
				return fmt.Sprintf(`Tool Output: {"status": "error", "message": "%v"}`, err)
			}
			return fmt.Sprintf(`Tool Output: {"status": "success", "message": "Note %d deleted"}`, tc.NoteID)
		default:
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "Unknown notes operation: %s. Use add, list, update, toggle, or delete"}`, tc.Operation)
		}

	case "analyze_image", "vision":
		if budgetTracker != nil && budgetTracker.IsBlocked("vision") {
			return `Tool Output: {"status": "error", "message": "Vision blocked: daily budget exceeded. Try again tomorrow."}`
		}
		logger.Info("LLM requested image analysis", "file_path", tc.FilePath)
		fpath := tc.FilePath
		if fpath == "" {
			fpath = tc.Path
		}
		if fpath == "" {
			return `Tool Output: {"status": "error", "message": "'file_path' is required for analyze_image"}`
		}
		if strings.Contains(fpath, "..") {
			return `Tool Output: {"status": "error", "message": "path traversal sequences ('..') are not allowed"}`
		}
		prompt := tc.Prompt
		if prompt == "" {
			prompt = "Describe this image in detail. What do you see? If there is text, transcribe it. If there are people, describe their actions."
		}
		result, err := tools.AnalyzeImageWithPrompt(fpath, prompt, cfg)
		if err != nil {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "Vision analysis failed: %v"}`, err)
		}
		return fmt.Sprintf("Tool Output: %s", result)

	case "transcribe_audio", "speech_to_text":
		if budgetTracker != nil && budgetTracker.IsBlocked("stt") {
			return `Tool Output: {"status": "error", "message": "Speech-to-text blocked: daily budget exceeded. Try again tomorrow."}`
		}
		logger.Info("LLM requested audio transcription", "file_path", tc.FilePath)
		fpath := tc.FilePath
		if fpath == "" {
			fpath = tc.Path
		}
		if fpath == "" {
			return `Tool Output: {"status": "error", "message": "'file_path' is required for transcribe_audio"}`
		}
		if strings.Contains(fpath, "..") {
			return `Tool Output: {"status": "error", "message": "path traversal sequences ('..') are not allowed"}`
		}
		result, err := tools.TranscribeAudioFile(fpath, cfg)
		if err != nil {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "Transcription failed: %v"}`, err)
		}
		return fmt.Sprintf("Tool Output: %s", result)

	case "docker", "docker_management":
		if !cfg.Docker.Enabled {
			return `Tool Output: {"status": "error", "message": "Docker integration is not enabled. Set docker.enabled=true in config.yaml."}`
		}
		dockerCfg := tools.DockerConfig{Host: cfg.Docker.Host}
		containerID := tc.ContainerID
		if containerID == "" {
			containerID = tc.Name
		}
		switch tc.Operation {
		case "list_containers", "ps":
			logger.Info("LLM requested Docker list_containers", "all", tc.All)
			return "Tool Output: " + tools.DockerListContainers(dockerCfg, tc.All)
		case "inspect", "inspect_container":
			logger.Info("LLM requested Docker inspect", "container_id", containerID)
			return "Tool Output: " + tools.DockerInspectContainer(dockerCfg, containerID)
		case "start":
			logger.Info("LLM requested Docker start", "container_id", containerID)
			return "Tool Output: " + tools.DockerContainerAction(dockerCfg, containerID, "start", false)
		case "stop":
			logger.Info("LLM requested Docker stop", "container_id", containerID)
			return "Tool Output: " + tools.DockerContainerAction(dockerCfg, containerID, "stop", false)
		case "restart":
			logger.Info("LLM requested Docker restart", "container_id", containerID)
			return "Tool Output: " + tools.DockerContainerAction(dockerCfg, containerID, "restart", false)
		case "pause":
			logger.Info("LLM requested Docker pause", "container_id", containerID)
			return "Tool Output: " + tools.DockerContainerAction(dockerCfg, containerID, "pause", false)
		case "unpause":
			logger.Info("LLM requested Docker unpause", "container_id", containerID)
			return "Tool Output: " + tools.DockerContainerAction(dockerCfg, containerID, "unpause", false)
		case "remove", "rm":
			logger.Info("LLM requested Docker remove", "container_id", containerID, "force", tc.Force)
			return "Tool Output: " + tools.DockerContainerAction(dockerCfg, containerID, "remove", tc.Force)
		case "logs":
			logger.Info("LLM requested Docker logs", "container_id", containerID, "tail", tc.Tail)
			return "Tool Output: " + tools.DockerContainerLogs(dockerCfg, containerID, tc.Tail)
		case "create", "create_container", "run":
			logger.Info("LLM requested Docker create", "image", tc.Image, "name", tc.Name)
			var cmd []string
			if tc.Command != "" {
				cmd = strings.Fields(tc.Command)
			}
			restart := tc.Restart
			if restart == "" {
				restart = "no"
			}
			result := tools.DockerCreateContainer(dockerCfg, tc.Name, tc.Image, tc.Env, tc.Ports, tc.Volumes, cmd, restart)
			// Auto-start if operation was "run"
			if tc.Operation == "run" {
				var created map[string]interface{}
				if json.Unmarshal([]byte(result), &created) == nil {
					if id, ok := created["id"].(string); ok && id != "" {
						tools.DockerContainerAction(dockerCfg, id, "start", false)
						created["message"] = "Container created and started"
						updated, _ := json.Marshal(created)
						result = string(updated)
					}
				}
			}
			return "Tool Output: " + result
		case "list_images", "images":
			logger.Info("LLM requested Docker list_images")
			return "Tool Output: " + tools.DockerListImages(dockerCfg)
		case "pull_image", "pull":
			logger.Info("LLM requested Docker pull", "image", tc.Image)
			return "Tool Output: " + tools.DockerPullImage(dockerCfg, tc.Image)
		case "remove_image", "rmi":
			logger.Info("LLM requested Docker remove_image", "image", tc.Image, "force", tc.Force)
			return "Tool Output: " + tools.DockerRemoveImage(dockerCfg, tc.Image, tc.Force)
		case "list_networks", "networks":
			logger.Info("LLM requested Docker list_networks")
			return "Tool Output: " + tools.DockerListNetworks(dockerCfg)
		case "list_volumes", "volumes":
			logger.Info("LLM requested Docker list_volumes")
			return "Tool Output: " + tools.DockerListVolumes(dockerCfg)
		case "info", "system_info":
			logger.Info("LLM requested Docker system_info")
			return "Tool Output: " + tools.DockerSystemInfo(dockerCfg)
		case "exec":
			logger.Info("LLM requested Docker exec", "container_id", containerID, "cmd", tc.Command)
			return "Tool Output: " + tools.DockerExec(dockerCfg, containerID, tc.Command, tc.User)
		case "stats":
			logger.Info("LLM requested Docker stats", "container_id", containerID)
			return "Tool Output: " + tools.DockerStats(dockerCfg, containerID)
		case "top":
			logger.Info("LLM requested Docker top", "container_id", containerID)
			return "Tool Output: " + tools.DockerTop(dockerCfg, containerID)
		case "port":
			logger.Info("LLM requested Docker port", "container_id", containerID)
			return "Tool Output: " + tools.DockerPort(dockerCfg, containerID)
		case "cp", "copy":
			logger.Info("LLM requested Docker cp", "container_id", containerID, "src", tc.Source, "dest", tc.Destination, "direction", tc.Direction)
			return "Tool Output: " + tools.DockerCopy(dockerCfg, containerID, tc.Source, tc.Destination, tc.Direction)
		case "create_network":
			logger.Info("LLM requested Docker create_network", "name", tc.Name, "driver", tc.Driver)
			return "Tool Output: " + tools.DockerCreateNetwork(dockerCfg, tc.Name, tc.Driver)
		case "remove_network":
			logger.Info("LLM requested Docker remove_network", "name", tc.Name)
			return "Tool Output: " + tools.DockerRemoveNetwork(dockerCfg, tc.Name)
		case "connect":
			logger.Info("LLM requested Docker connect", "container_id", containerID, "network", tc.Network)
			return "Tool Output: " + tools.DockerConnectNetwork(dockerCfg, containerID, tc.Network)
		case "disconnect":
			logger.Info("LLM requested Docker disconnect", "container_id", containerID, "network", tc.Network)
			return "Tool Output: " + tools.DockerDisconnectNetwork(dockerCfg, containerID, tc.Network)
		case "create_volume":
			logger.Info("LLM requested Docker create_volume", "name", tc.Name, "driver", tc.Driver)
			return "Tool Output: " + tools.DockerCreateVolume(dockerCfg, tc.Name, tc.Driver)
		case "remove_volume":
			logger.Info("LLM requested Docker remove_volume", "name", tc.Name, "force", tc.Force)
			return "Tool Output: " + tools.DockerRemoveVolume(dockerCfg, tc.Name, tc.Force)
		case "compose":
			logger.Info("LLM requested Docker compose", "file", tc.File, "cmd", tc.Command)
			return "Tool Output: " + tools.DockerCompose(dockerCfg, tc.File, tc.Command)
		default:
			return `Tool Output: {"status": "error", "message": "Unknown docker operation. Use: list_containers, inspect, start, stop, restart, pause, unpause, remove, logs, create, run, list_images, pull, remove_image, list_networks, create_network, remove_network, connect, disconnect, list_volumes, create_volume, remove_volume, exec, stats, top, port, cp, compose, info"}`
		}

	case "webdav", "webdav_storage":
		if !cfg.WebDAV.Enabled {
			return `Tool Output: {"status": "error", "message": "WebDAV integration is not enabled. Set webdav.enabled=true in config.yaml."}`
		}
		davCfg := tools.WebDAVConfig{
			URL:      cfg.WebDAV.URL,
			Username: cfg.WebDAV.Username,
			Password: cfg.WebDAV.Password,
		}
		path := tc.Path
		if path == "" {
			path = tc.RemotePath
		}
		if path == "" {
			path = tc.FilePath
		}
		switch tc.Operation {
		case "list", "ls":
			logger.Info("LLM requested WebDAV list", "path", path)
			return "Tool Output: " + tools.WebDAVList(davCfg, path)
		case "read", "get", "download":
			logger.Info("LLM requested WebDAV read", "path", path)
			return "Tool Output: " + tools.WebDAVRead(davCfg, path)
		case "write", "put", "upload":
			logger.Info("LLM requested WebDAV write", "path", path)
			content := tc.Content
			if content == "" {
				content = tc.Body
			}
			return "Tool Output: " + tools.WebDAVWrite(davCfg, path, content)
		case "mkdir", "create_dir":
			logger.Info("LLM requested WebDAV mkdir", "path", path)
			return "Tool Output: " + tools.WebDAVMkdir(davCfg, path)
		case "delete", "rm":
			logger.Info("LLM requested WebDAV delete", "path", path)
			return "Tool Output: " + tools.WebDAVDelete(davCfg, path)
		case "move", "rename", "mv":
			logger.Info("LLM requested WebDAV move", "path", path, "destination", tc.Destination)
			dst := tc.Destination
			if dst == "" {
				dst = tc.Dest
			}
			return "Tool Output: " + tools.WebDAVMove(davCfg, path, dst)
		case "info", "stat":
			logger.Info("LLM requested WebDAV info", "path", path)
			return "Tool Output: " + tools.WebDAVInfo(davCfg, path)
		default:
			return `Tool Output: {"status": "error", "message": "Unknown webdav operation. Use: list, read, write, mkdir, delete, move, info"}`
		}

	case "home_assistant", "homeassistant", "ha":
		if !cfg.HomeAssistant.Enabled {
			return `Tool Output: {"status": "error", "message": "Home Assistant integration is not enabled. Set home_assistant.enabled=true in config.yaml."}`
		}
		haCfg := tools.HAConfig{
			URL:         cfg.HomeAssistant.URL,
			AccessToken: cfg.HomeAssistant.AccessToken,
		}
		// Merge service_data from Params if ServiceData is nil
		serviceData := tc.ServiceData
		if serviceData == nil && tc.Params != nil {
			if sd, ok := tc.Params["service_data"].(map[string]interface{}); ok {
				serviceData = sd
			}
		}
		switch tc.Operation {
		case "get_states", "list_states", "states":
			logger.Info("LLM requested HA get_states", "domain", tc.Domain)
			return "Tool Output: " + tools.HAGetStates(haCfg, tc.Domain)
		case "get_state", "state":
			logger.Info("LLM requested HA get_state", "entity_id", tc.EntityID)
			return "Tool Output: " + tools.HAGetState(haCfg, tc.EntityID)
		case "call_service", "service":
			logger.Info("LLM requested HA call_service", "domain", tc.Domain, "service", tc.Service, "entity_id", tc.EntityID)
			return "Tool Output: " + tools.HACallService(haCfg, tc.Domain, tc.Service, tc.EntityID, serviceData)
		case "list_services", "services":
			logger.Info("LLM requested HA list_services", "domain", tc.Domain)
			return "Tool Output: " + tools.HAListServices(haCfg, tc.Domain)
		default:
			return `Tool Output: {"status": "error", "message": "Unknown home_assistant operation. Use: get_states, get_state, call_service, list_services"}`
		}

	case "co_agent", "co_agents":
		if budgetTracker != nil && budgetTracker.IsBlocked("coagent") {
			return `Tool Output: {"status": "error", "message": "Co-Agent spawn blocked: daily budget exceeded. Try again tomorrow."}`
		}
		if !cfg.CoAgents.Enabled {
			return `Tool Output: {"status": "error", "message": "Co-Agent system is not enabled. Set co_agents.enabled=true in config.yaml."}`
		}
		if coAgentRegistry == nil {
			return `Tool Output: {"status": "error", "message": "Co-Agent registry not initialized."}`
		}
		switch tc.Operation {
		case "spawn", "start", "create":
			task := tc.Task
			if task == "" {
				task = tc.Content
			}
			if task == "" {
				return `Tool Output: {"status": "error", "message": "'task' is required to spawn a co-agent."}`
			}
			coReq := CoAgentRequest{
				Task:         task,
				ContextHints: tc.ContextHints,
			}
			id, err := SpawnCoAgent(cfg, ctx, logger, coAgentRegistry,
				shortTermMem, longTermMem, vault, registry, manifest, kg, inventoryDB, coReq, budgetTracker)
			if err != nil {
				return fmt.Sprintf(`Tool Output: {"status": "error", "message": "%v"}`, err)
			}
			slots := coAgentRegistry.AvailableSlots()
			return fmt.Sprintf(`Tool Output: {"status": "ok", "co_agent_id": "%s", "available_slots": %d, "message": "Co-Agent started. Use operation 'list' to check status and 'get_result' when completed."}`, id, slots)

		case "list", "status":
			list := coAgentRegistry.List()
			data, _ := json.Marshal(map[string]interface{}{
				"status":          "ok",
				"available_slots": coAgentRegistry.AvailableSlots(),
				"max_slots":       cfg.CoAgents.MaxConcurrent,
				"co_agents":       list,
			})
			return "Tool Output: " + string(data)

		case "get_result", "result":
			coID := tc.CoAgentID
			if coID == "" {
				coID = tc.ID
			}
			if coID == "" {
				return `Tool Output: {"status": "error", "message": "'co_agent_id' is required."}`
			}
			result, err := coAgentRegistry.GetResult(coID)
			if err != nil {
				return fmt.Sprintf(`Tool Output: {"status": "error", "message": "%v"}`, err)
			}
			out, _ := json.Marshal(map[string]interface{}{
				"status":      "ok",
				"co_agent_id": coID,
				"result":      result,
			})
			return "Tool Output: " + string(out)

		case "stop", "cancel", "kill":
			coID := tc.CoAgentID
			if coID == "" {
				coID = tc.ID
			}
			if coID == "" {
				return `Tool Output: {"status": "error", "message": "'co_agent_id' is required."}`
			}
			if err := coAgentRegistry.Stop(coID); err != nil {
				return fmt.Sprintf(`Tool Output: {"status": "error", "message": "%v"}`, err)
			}
			return fmt.Sprintf(`Tool Output: {"status": "ok", "message": "Co-Agent '%s' stopped."}`, coID)

		case "stop_all", "cancel_all":
			n := coAgentRegistry.StopAll()
			return fmt.Sprintf(`Tool Output: {"status": "ok", "message": "Stopped %d co-agent(s)."}`, n)

		default:
			return `Tool Output: {"status": "error", "message": "Unknown co_agent operation. Use: spawn, list, get_result, stop, stop_all"}`
		}

	case "mdns_scan":
		logger.Info("LLM requested mdns_scan", "service_type", tc.ServiceType, "timeout", tc.Timeout)
		return "Tool Output: " + tools.MDNSScan(logger, tc.ServiceType, tc.Timeout)

	case "tts":
		if !cfg.Chromecast.Enabled && cfg.TTS.Provider == "" {
			return `Tool Output: {"status": "error", "message": "TTS is not configured. Set tts.provider in config.yaml."}`
		}
		text := tc.Text
		if text == "" {
			text = tc.Content
		}
		ttsCfg := tools.TTSConfig{
			Provider: cfg.TTS.Provider,
			Language: tc.Language,
			DataDir:  cfg.Directories.DataDir,
		}
		if ttsCfg.Language == "" {
			ttsCfg.Language = cfg.TTS.Language
		}
		ttsCfg.ElevenLabs.APIKey = cfg.TTS.ElevenLabs.APIKey
		ttsCfg.ElevenLabs.VoiceID = cfg.TTS.ElevenLabs.VoiceID
		ttsCfg.ElevenLabs.ModelID = cfg.TTS.ElevenLabs.ModelID
		filename, err := tools.TTSSynthesize(ttsCfg, text)
		if err != nil {
			return fmt.Sprintf(`Tool Output: {"status": "error", "message": "TTS failed: %v"}`, err)
		}
		ttsPort := cfg.Chromecast.TTSPort
		if ttsPort == 0 {
			ttsPort = cfg.Server.Port // Fallback if chromecast integration is disabled
		}
		audioURL := fmt.Sprintf("http://%s:%d/tts/%s", getLocalIP(cfg), ttsPort, filename)
		return fmt.Sprintf(`Tool Output: {"status": "success", "file": "%s", "url": "%s"}`, filename, audioURL)

	case "chromecast":
		if !cfg.Chromecast.Enabled {
			return `Tool Output: {"status": "error", "message": "Chromecast is disabled. Set chromecast.enabled=true in config.yaml."}`
		}
		op := tc.Operation
		switch op {
		case "discover":
			return "Tool Output: " + tools.ChromecastDiscover(logger)
		case "play":
			url := tc.URL
			ct := tc.ContentType
			return "Tool Output: " + tools.ChromecastPlay(tc.DeviceAddr, tc.DevicePort, url, ct, logger)
		case "speak":
			text := tc.Text
			if text == "" {
				text = tc.Content
			}
			ttsCfg := tools.TTSConfig{
				Provider: cfg.TTS.Provider,
				Language: tc.Language,
				DataDir:  cfg.Directories.DataDir,
			}
			if ttsCfg.Language == "" {
				ttsCfg.Language = cfg.TTS.Language
			}
			ttsCfg.ElevenLabs.APIKey = cfg.TTS.ElevenLabs.APIKey
			ttsCfg.ElevenLabs.VoiceID = cfg.TTS.ElevenLabs.VoiceID
			ttsCfg.ElevenLabs.ModelID = cfg.TTS.ElevenLabs.ModelID
			ccCfg := tools.ChromecastConfig{
				ServerHost: cfg.Server.Host,
				ServerPort: cfg.Chromecast.TTSPort,
			}
			return "Tool Output: " + tools.ChromecastSpeak(tc.DeviceAddr, tc.DevicePort, text, ttsCfg, ccCfg, logger)
		case "stop":
			return "Tool Output: " + tools.ChromecastStop(tc.DeviceAddr, tc.DevicePort, logger)
		case "volume":
			return "Tool Output: " + tools.ChromecastVolume(tc.DeviceAddr, tc.DevicePort, tc.Volume, logger)
		case "status":
			return "Tool Output: " + tools.ChromecastStatus(tc.DeviceAddr, tc.DevicePort, logger)
		default:
			return `Tool Output: {"status": "error", "message": "Unknown chromecast operation. Use: discover, play, speak, stop, volume, status"}`
		}

	default:

		logger.Warn("LLM requested unknown action", "action", tc.Action)
		return fmt.Sprintf("Tool Output: ERROR unknown action '%s'", tc.Action)
	}
}

// DispatchToolCall executes the appropriate tool based on the parsed ToolCall.
// It automatically handles Redaction, Guardian sanitization, and ensures the output
// is correctly prefixed with "[Tool Output]\n" unless it's a known error marker.
func DispatchToolCall(ctx context.Context, tc ToolCall, cfg *config.Config, logger *slog.Logger, llmClient llm.ChatClient, vault *security.Vault, registry *tools.ProcessRegistry, manifest *tools.Manifest, cronManager *tools.CronManager, longTermMem memory.VectorDB, shortTermMem *memory.SQLiteMemory, kg *memory.KnowledgeGraph, inventoryDB *sql.DB, historyMgr *memory.HistoryManager, isMaintenance bool, surgeryPlan string, guardian *security.Guardian, sessionID string, coAgentRegistry *CoAgentRegistry, budgetTracker *budget.Tracker) string {

	rawResult := dispatchInner(ctx, tc, cfg, logger, llmClient, vault, registry, manifest, cronManager, longTermMem, shortTermMem, kg, inventoryDB, historyMgr, isMaintenance, surgeryPlan, guardian, sessionID, coAgentRegistry, budgetTracker)

	// Apply redaction to tool output
	sanitized := security.RedactSensitiveInfo(rawResult)

	// Guardian: Sanitize tool output (isolation + role-marker stripping)
	if guardian != nil {
		sanitized = guardian.SanitizeToolOutput(tc.Action, sanitized)
	}

	// Make sure errors from execute_python are preserved for context
	if tc.Action == "execute_python" {
		if strings.Contains(sanitized, "[EXECUTION ERROR]") || strings.Contains(sanitized, "TIMEOUT") {
			// handled outside in isErrorState flags if necessary, but we preserve the string here
		}
	}

	// Prefix to clearly identify it as tool output
	if !strings.HasPrefix(sanitized, "[TOOL ") && !strings.HasPrefix(sanitized, "[Tool ") {
		sanitized = "[Tool Output]\n" + sanitized
	}

	return sanitized
}

// getLocalIP returns a LAN-reachable IP address for the TTS audio server.
func getLocalIP(cfg *config.Config) string {
	host := cfg.Server.Host
	if host == "" || host == "127.0.0.1" || host == "0.0.0.0" {
		conn, err := net.Dial("udp", "8.8.8.8:80")
		if err == nil {
			defer conn.Close()
			return conn.LocalAddr().(*net.UDPAddr).IP.String()
		}
		return "127.0.0.1"
	}
	return host
}

// runMemoryOrchestrator handles the Priority-Based Forgetting System across both RAG and Knowledge Graph.
func runMemoryOrchestrator(tc ToolCall, cfg *config.Config, logger *slog.Logger, client llm.ChatClient, longTermMem memory.VectorDB, shortTermMem *memory.SQLiteMemory, kg *memory.KnowledgeGraph) string {
	thresholdLow := tc.ThresholdLow
	if thresholdLow == 0 {
		thresholdLow = 1
	}
	thresholdMedium := tc.ThresholdMedium
	if thresholdMedium == 0 {
		thresholdMedium = 3
	}

	metas, err := shortTermMem.GetAllMemoryMeta()
	if err != nil {
		logger.Error("Failed to fetch memory tracking metadata", "error", err)
		return fmt.Sprintf(`{"status": "error", "message": "Failed to fetch metadata: %v"}`, err)
	}

	highCount, mediumCount, lowCount := 0, 0, 0
	var lowDocs []string
	var mediumDocs []string

	for _, meta := range metas {
		if meta.Protected || meta.KeepForever {
			highCount++
			continue
		}

		lastA, err := time.Parse(time.RFC3339, strings.Replace(meta.LastAccessed, " ", "T", 1)+"Z")
		daysSince := 0
		if err == nil {
			daysSince = int(time.Since(lastA).Hours() / 24)
		}

		priority := meta.AccessCount - daysSince

		if priority < thresholdLow {
			lowCount++
			lowDocs = append(lowDocs, meta.DocID)
		} else if priority < thresholdMedium {
			mediumCount++
			mediumDocs = append(mediumDocs, meta.DocID)
		} else {
			highCount++
		}
	}

	graphRemoved := 0
	if !tc.Preview {
		// 1. Process VectorDB Low Priority
		for _, docID := range lowDocs {
			_ = longTermMem.DeleteDocument(docID)
			_ = shortTermMem.DeleteMemoryMeta(docID)
		}

		// 2. Process VectorDB Medium Priority (Compression)
		for _, docID := range mediumDocs {
			content, err := longTermMem.GetByID(docID)
			if err != nil || len(content) < 300 {
				continue
			}

			// Compress via LLM
			resp, err := llm.ExecuteWithRetry(
				context.Background(),
				client,
				openai.ChatCompletionRequest{
					Model: cfg.LLM.Model,
					Messages: []openai.ChatCompletionMessage{
						{Role: openai.ChatMessageRoleSystem, Content: "You are an AI compressing old memories. Summarize the following RAG memory into a dense, concise bullet-point list containing only core facts. Lose the verbose narrative immediately."},
						{Role: openai.ChatMessageRoleUser, Content: content},
					},
					MaxTokens: 500,
				},
				logger,
				nil,
			)
			if err == nil && len(resp.Choices) > 0 {
				compressed := resp.Choices[0].Message.Content

				parts := strings.SplitN(content, "\n\n", 2)
				concept := "Compressed Memory"
				if len(parts) == 2 {
					concept = parts[0]
				}

				newIDs, err2 := longTermMem.StoreDocument(concept, compressed)
				if err2 == nil {
					_ = longTermMem.DeleteDocument(docID)
					_ = shortTermMem.DeleteMemoryMeta(docID)
					for _, newID := range newIDs {
						_ = shortTermMem.UpsertMemoryMeta(newID)
					}
				}
			}
		}

		// 3. Process Graph Low Priority
		graphRemoved, _ = kg.OptimizeGraph(thresholdLow)
	}

	return fmt.Sprintf(
		`{"status": "success", "preview": %v, "memory_rag": {"high_kept": %d, "medium_compressed": %d, "low_archived": %d}, "graph_nodes_archived": %d}`,
		tc.Preview, highCount, mediumCount, lowCount, graphRemoved,
	)
}

// parseWorkflowPlan extracts tool names from a <workflow_plan>["t1","t2"]</workflow_plan> tag.
// Returns the parsed tool list and the content with the tag removed.
// If no tag is found, returns nil and the original content unchanged.
func parseWorkflowPlan(content string) ([]string, string) {
	const openTag = "<workflow_plan>"
	const closeTag = "</workflow_plan>"

	startIdx := strings.Index(content, openTag)
	if startIdx < 0 {
		return nil, content
	}
	endIdx := strings.Index(content[startIdx:], closeTag)
	if endIdx < 0 {
		return nil, content
	}
	endIdx += startIdx // absolute position

	inner := strings.TrimSpace(content[startIdx+len(openTag) : endIdx])
	if inner == "" {
		return nil, content
	}

	// Parse the JSON array of tool names
	var tools []string
	if err := json.Unmarshal([]byte(inner), &tools); err != nil {
		// Fallback: try comma-separated without JSON
		inner = strings.Trim(inner, "[]")
		for _, t := range strings.Split(inner, ",") {
			t = strings.Trim(strings.TrimSpace(t), "\"'")
			if t != "" {
				tools = append(tools, t)
			}
		}
	}

	if len(tools) == 0 {
		return nil, content
	}

	// Cap at 5 to prevent abuse
	if len(tools) > 5 {
		tools = tools[:5]
	}

	// Strip the tag from the content
	stripped := content[:startIdx] + content[endIdx+len(closeTag):]
	return tools, stripped
}

// extractExtraToolCalls scans content for additional valid JSON tool calls beyond the first
// one already parsed (identified by firstRawJSON). Used to handle LLM responses that contain
// multiple sequential tool calls in one message (e.g. two manage_memory adds).
func extractExtraToolCalls(content, firstRawJSON string) []ToolCall {
	var results []ToolCall
	// Skip past the already-extracted JSON blob so we don't re-parse it
	remaining := content
	if firstRawJSON != "" {
		idx := strings.Index(remaining, firstRawJSON)
		if idx >= 0 {
			remaining = remaining[idx+len(firstRawJSON):]
		}
	}
	// Extract all remaining valid JSON tool calls
	for {
		start := strings.Index(remaining, "{")
		if start == -1 {
			break
		}
		bStr := remaining[start:]
		found := false
		for j := strings.LastIndex(bStr, "}"); j > 0; {
			candidate := bStr[:j+1]
			var tmp ToolCall
			if json.Unmarshal([]byte(candidate), &tmp) == nil && tmp.Action != "" {
				tmp.IsTool = true
				tmp.RawJSON = candidate
				results = append(results, tmp)
				remaining = bStr[j+1:]
				found = true
				break
			}
			j = strings.LastIndex(bStr[:j], "}")
			if j < 0 {
				break
			}
		}
		if !found {
			break
		}
	}
	return results
}

func ParseToolCall(content string) ToolCall {
	var tc ToolCall
	lowerContent := strings.ToLower(content)

	// Stepfun / OpenRouter <tool_call> fallback
	// Format 1: <function=name> ... </function>
	// Format 2: <tool_calls><invoke name="..."> ... </invoke></tool_calls>
	if start := strings.Index(lowerContent, "<tool_calls>"); start != -1 {
		tc.IsTool = true
		// Extract first invoke
		if invStart := strings.Index(lowerContent[start:], "<invoke name="); invStart != -1 {
			invStart += start
			invNameStart := invStart + 13
			invEndChar := strings.Index(lowerContent[invNameStart:], ">")
			if invEndChar != -1 {
				tc.Action = strings.Trim(strings.TrimSpace(content[invNameStart:invNameStart+invEndChar]), "\"'")

				// Extract params
				bodyStart := invNameStart + invEndChar + 1
				bodyEnd := strings.Index(lowerContent[bodyStart:], "</invoke>")
				if bodyEnd != -1 {
					paramSearch := content[bodyStart : bodyStart+bodyEnd]
					parseXMLParams(&tc, paramSearch)
				}
			}
		}
		return tc
	}

	if start := strings.Index(lowerContent, "<function="); start != -1 {
		end := strings.Index(lowerContent[start:], ">")
		if end != -1 {
			actionName := content[start+10 : start+end]
			actionName = strings.Trim(strings.TrimSpace(actionName), "\"'")
			tc.IsTool = true
			tc.Action = actionName

			// Extract any JSON arguments inside <function=...>{...}</function> if present
			funcBodyStart := start + end + 1
			funcBodyEnd := strings.Index(lowerContent[funcBodyStart:], "</function>")
			if funcBodyEnd != -1 {
				jsonBody := content[funcBodyStart : funcBodyStart+funcBodyEnd]
				parseXMLParams(&tc, jsonBody)
			}

			// AGGRESSIVE RECOVERY for LLMs placing the python block OUTSIDE the JSON
			if (tc.Action == "execute_python" || tc.Action == "save_tool") && tc.Code == "" {
				if blockStart := strings.Index(content, "```python"); blockStart != -1 {
					if blockEnd := strings.Index(content[blockStart+9:], "```"); blockEnd != -1 {
						tc.Code = strings.TrimSpace(content[blockStart+9 : blockStart+9+blockEnd])
					}
				}
			}
			return tc
		}
	}

	if (strings.Contains(lowerContent, "\"action\"") || strings.Contains(lowerContent, "'action'") || strings.Contains(lowerContent, "\"tool\"") || strings.Contains(lowerContent, "\"command\"") || (strings.Contains(lowerContent, "\"name\"") && strings.Contains(lowerContent, "\"arguments\""))) && (strings.Contains(lowerContent, "{") || strings.Contains(lowerContent, "```")) {
		extractedFromFence := false

		// Try all common fence variants: ```json, ``` json, ```JSON, plain ```
		fenceVariants := []string{"```json\n", "```json\r\n", "```json ", "```json", "``` json", "```JSON"}
		for _, fv := range fenceVariants {
			if start := strings.Index(content, fv); start != -1 {
				after := content[start+len(fv):]
				// Trim any leading whitespace/newline after the fence marker
				after = strings.TrimLeft(after, " \t\r\n")
				// Find closing ```
				if end := strings.Index(after, "```"); end != -1 {
					candidate := strings.TrimSpace(after[:end])
					if strings.HasPrefix(candidate, "{") {
						var tmp ToolCall
						if json.Unmarshal([]byte(candidate), &tmp) == nil && tmp.Action != "" {
							tc = tmp
							extractedFromFence = true
							tc.RawJSON = candidate
							break
						}
					}
				}
			}
		}

		if !extractedFromFence {
			// No fence or fence extraction failed — try raw brace extraction from content.
			// Try all '{' positions as potential JSON starts.
			for i := 0; i < len(content); i++ {
				if content[i] == '{' {
					bStr := content[i:]
					// Search from the end for the furthest '}' that yields a valid ToolCall
					for j := strings.LastIndex(bStr, "}"); j != -1; j = strings.LastIndex(bStr[:j], "}") {
						candidate := bStr[:j+1]
						var tmp ToolCall
						if json.Unmarshal([]byte(candidate), &tmp) == nil && tmp.Action != "" {
							tc = tmp
							extractedFromFence = true
							tc.RawJSON = candidate
							break
						}
					}
				}
				if extractedFromFence {
					break
				}
			}
		}
		if tc.Action != "" {
			tc.IsTool = true

			// AGGRESSIVE RECOVERY: Handle wrappers like {"action": "execute_tool", "tool": "name", "args": {...}}
			if (tc.Action == "execute_tool" || tc.Action == "run_tool" || tc.Action == "execute_tool_call") && tc.Tool != "" {
				tc.Action = tc.Tool
			}

			// Fallback: LLM used "tool" key instead of "action"
			if tc.Action == "" && tc.Tool != "" {
				tc.Action = tc.Tool
			}

			// Fallback: LLM sent only "command" — treat as execute_shell
			if tc.Action == "" && tc.Command != "" {
				tc.Action = "execute_shell"
			}

			// Fallback: OpenAI native function_call format {"name": "tool", "arguments": {...}}
			if tc.Action == "" && tc.Name != "" {
				tc.Action = tc.Name
			}

			// If LLM uses 'arguments' (hallucination)
			if tc.Arguments != nil {
				if tc.Params == nil {
					tc.Params = make(map[string]interface{})
				}
				switch v := tc.Arguments.(type) {
				case map[string]interface{}:
					for k, val := range v {
						tc.Params[k] = val
					}
				case string:
					// Robust recovery: the LLM sometimes JSON-encodes the arguments into a string
					var argMap map[string]interface{}
					if err := json.Unmarshal([]byte(v), &argMap); err == nil {
						for k, val := range argMap {
							tc.Params[k] = val
						}
					}
				}
			}

			// Recovery for map-based 'args' which fails to unmarshal into tc.Args ([]string)
			if argsMap, ok := tc.Args.(map[string]interface{}); ok {
				if tc.Params == nil {
					tc.Params = make(map[string]interface{})
				}
				for k, v := range argsMap {
					tc.Params[k] = v
				}
			}

			// Flatten action_input (LangChain-style nested params) into Params
			if tc.ActionInput != nil {
				if tc.Params == nil {
					tc.Params = make(map[string]interface{})
				}
				for k, v := range tc.ActionInput {
					tc.Params[k] = v
				}
			}

			// Final parameter promotion: Ensure specific fields are populated from Params if missing
			if tc.Params != nil {
				promoteString := func(target *string, keys ...string) {
					if *target != "" {
						return
					}
					for _, k := range keys {
						if v, ok := tc.Params[k].(string); ok && v != "" {
							*target = v
							return
						}
					}
				}
				promoteInt := func(target *int, keys ...string) {
					if *target != 0 {
						return
					}
					for _, k := range keys {
						if v, ok := tc.Params[k].(float64); ok && v != 0 {
							*target = int(v)
							return
						}
					}
				}

				promoteString(&tc.Hostname, "hostname", "host", "server_id")
				promoteString(&tc.IPAddress, "ip_address", "ip", "address")
				promoteString(&tc.Username, "username", "user")
				promoteString(&tc.Password, "password", "pass")
				promoteString(&tc.Tags, "tags", "tag")
				promoteString(&tc.PrivateKeyPath, "private_key_path", "key_path", "private_key")
				promoteString(&tc.ServerID, "server_id", "serverId", "id", "hostname", "host")
				promoteString(&tc.Command, "command", "cmd")
				promoteString(&tc.Tag, "tag", "tags")
				promoteString(&tc.LocalPath, "local_path", "localPath", "source")
				promoteString(&tc.RemotePath, "remote_path", "remotePath", "destination", "dest")
				promoteString(&tc.Direction, "direction")
				promoteString(&tc.Operation, "operation", "op")
				promoteString(&tc.FilePath, "file_path", "path", "filepath", "filename", "file")
				if tc.FilePath != "" && tc.Path == "" {
					tc.Path = tc.FilePath
				}
				promoteString(&tc.Destination, "destination", "dest", "target")
				if tc.Destination != "" && tc.Dest == "" {
					tc.Dest = tc.Destination
				}
				promoteString(&tc.Content, "content", "query")
				promoteString(&tc.Query, "query", "content")
				promoteString(&tc.Name, "name")
				promoteString(&tc.Description, "description")
				promoteString(&tc.Code, "code", "script")
				promoteString(&tc.Package, "package", "package_name")
				promoteString(&tc.ToolName, "tool_name", "toolName")
				promoteString(&tc.Label, "label")
				promoteString(&tc.TaskPrompt, "task_prompt")
				promoteString(&tc.Prompt, "prompt")
				// Notes / Vision / STT fields
				promoteString(&tc.Title, "title")
				promoteString(&tc.Category, "category")
				promoteString(&tc.DueDate, "due_date", "dueDate")
				// Home Assistant fields
				promoteString(&tc.EntityID, "entity_id", "entityId", "entity")
				promoteString(&tc.Domain, "domain")
				promoteString(&tc.Service, "service")
				// Docker fields
				promoteString(&tc.ContainerID, "container_id", "containerId", "container")
				promoteString(&tc.Image, "image")
				promoteString(&tc.Restart, "restart", "restart_policy")
				// Co-Agent fields
				promoteString(&tc.CoAgentID, "co_agent_id", "coAgentId", "coagent_id", "agent_id", "agentId")
				promoteString(&tc.Task, "task")
				// context_hints is []string — promote manually
				if len(tc.ContextHints) == 0 {
					for _, k := range []string{"context_hints", "contextHints", "hints"} {
						if arr, ok := tc.Params[k].([]interface{}); ok && len(arr) > 0 {
							for _, v := range arr {
								if s, ok := v.(string); ok {
									tc.ContextHints = append(tc.ContextHints, s)
								}
							}
							break
						}
					}
				}

				promoteInt(&tc.Port, "port")
				promoteInt(&tc.PID, "pid")
				promoteInt(&tc.Priority, "priority")
				promoteInt(&tc.Done, "done")
				// NoteID is int64 — promote manually
				if tc.NoteID == 0 {
					for _, k := range []string{"note_id", "noteId", "id"} {
						if v, ok := tc.Params[k].(float64); ok && v != 0 {
							tc.NoteID = int64(v)
							break
						}
					}
				}
			}

			// AGGRESSIVE RECOVERY for LLMs placing the python block OUTSIDE the JSON
			if (tc.Action == "execute_python" || tc.Action == "save_tool") && tc.Code == "" {
				if blockStart := strings.Index(content, "```python"); blockStart != -1 {
					if blockEnd := strings.Index(content[blockStart+9:], "```"); blockEnd != -1 {
						tc.Code = strings.TrimSpace(content[blockStart+9 : blockStart+9+blockEnd])
					}
				}
			}
			return tc
		}
	}

	if strings.HasPrefix(lowerContent, "import ") ||
		strings.HasPrefix(lowerContent, "def ") ||
		strings.HasPrefix(lowerContent, "print(") ||
		strings.HasPrefix(lowerContent, "# ") ||
		strings.Contains(lowerContent, "```python") {
		return ToolCall{RawCodeDetected: true}
	}

	return ToolCall{}
}

func parseXMLParams(tc *ToolCall, body string) {
	hasXMLParams := false
	lowerBody := strings.ToLower(body)
	paramSearch := lowerBody

	for {
		// Support <parameter=name> and <parameter name="...">
		pStart := strings.Index(paramSearch, "<parameter")
		if pStart == -1 {
			break
		}
		pAttrEnd := strings.Index(paramSearch[pStart:], ">")
		if pAttrEnd == -1 {
			break
		}
		pAttrEnd += pStart

		attrStr := body[pStart : pAttrEnd+1]
		paramName := ""
		if strings.Contains(attrStr, "=") {
			// <parameter=name>
			eqIdx := strings.Index(attrStr, "=")
			paramName = strings.Trim(strings.TrimSpace(attrStr[eqIdx+1:len(attrStr)-1]), "\"' ")
		} else if strings.Contains(attrStr, "name=") {
			// <parameter name="name">
			nameIdx := strings.Index(attrStr, "name=")
			paramName = strings.Trim(strings.TrimSpace(attrStr[nameIdx+5:len(attrStr)-1]), "\"' ")
		}

		vStart := pAttrEnd + 1
		vEndOffset := strings.Index(paramSearch[vStart:], "</parameter>")
		if vEndOffset == -1 {
			break
		}

		paramVal := strings.TrimSpace(body[vStart : vStart+vEndOffset])
		hasXMLParams = true

		switch paramName {
		case "code":
			tc.Code = paramVal
		case "name":
			tc.Name = strings.Trim(paramVal, "\"'")
		case "tool_name":
			tc.ToolName = strings.Trim(paramVal, "\"'")
		case "package":
			tc.Package = strings.Trim(paramVal, "\"'")
		case "key":
			tc.Key = strings.Trim(paramVal, "\"'")
		case "value":
			tc.Value = strings.Trim(paramVal, "\"'")
		case "skill":
			tc.Skill = strings.Trim(paramVal, "\"'")
		case "skill_args", "params":
			_ = json.Unmarshal([]byte(paramVal), &tc.Params)
			tc.SkillArgs = tc.Params
		case "operation":
			tc.Operation = strings.Trim(paramVal, "\"'")
		case "file_path", "path":
			tc.FilePath = strings.Trim(paramVal, "\"'")
			tc.Path = tc.FilePath
		case "destination", "dest":
			tc.Destination = strings.Trim(paramVal, "\"'")
			tc.Dest = tc.Destination
		case "content":
			tc.Content = paramVal
		case "query":
			tc.Query = paramVal
		case "task_prompt", "plan", "description":
			tc.TaskPrompt = paramVal
		case "prompt":
			tc.Prompt = paramVal
		case "title":
			tc.Title = strings.Trim(paramVal, "\"'")
		case "category":
			tc.Category = strings.Trim(paramVal, "\"'")
		case "priority":
			if v, err := strconv.Atoi(strings.TrimSpace(paramVal)); err == nil {
				tc.Priority = v
			}
		case "due_date":
			tc.DueDate = strings.Trim(paramVal, "\"'")
		case "note_id":
			if v, err := strconv.ParseInt(strings.TrimSpace(paramVal), 10, 64); err == nil {
				tc.NoteID = v
			}
		case "done":
			if v, err := strconv.Atoi(strings.TrimSpace(paramVal)); err == nil {
				tc.Done = v
			}
		case "args":
			_ = json.Unmarshal([]byte(paramVal), &tc.Args)
		}

		// advance
		advance := vStart + vEndOffset + 12
		if advance >= len(paramSearch) {
			break
		}
		paramSearch = paramSearch[advance:]
		body = body[advance:]
	}

	// 2. If no XML parameters were found, fallback to parsing as JSON
	if !hasXMLParams {
		jsonBody := strings.TrimSpace(body)
		// Strip markdown markdown block strings
		if strings.HasPrefix(jsonBody, "```json") {
			jsonBody = strings.TrimPrefix(jsonBody, "```json")
		} else if strings.HasPrefix(jsonBody, "```") {
			jsonBody = strings.TrimPrefix(jsonBody, "```")
		}
		jsonBody = strings.TrimSuffix(jsonBody, "```")
		jsonBody = strings.TrimSpace(jsonBody)

		if strings.HasPrefix(jsonBody, "{") {
			_ = json.Unmarshal([]byte(jsonBody), tc)
		}
	}
}

func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// GetActiveProcessStatus returns a comma-separated string of PIDs for the manifest sysprompt.
func GetActiveProcessStatus(registry *tools.ProcessRegistry) string {
	list := registry.List()
	if len(list) == 0 {
		return "None"
	}
	var names []string
	for _, p := range list {
		alive, _ := p["alive"].(bool)
		if alive {
			pid, _ := p["pid"].(int)
			names = append(names, fmt.Sprintf("PID:%d", pid))
		}
	}
	if len(names) == 0 {
		return "None"
	}
	return strings.Join(names, ", ")
}

// runGitCommand helper runs a git command with enforced environment and safe.directory config.
func runGitCommand(dir string, args ...string) ([]byte, error) {
	// Add safe.directory to bypass ownership warnings when running as root in user dirs
	fullArgs := append([]string{"-c", "safe.directory=" + dir}, args...)
	cmd := exec.Command("git", fullArgs...)
	cmd.Dir = dir

	// Ensure HOME is set, otherwise git may fail with exit status 128
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "/root" // Default for root-run services
	}
	cmd.Env = append(os.Environ(), "HOME="+home)

	return cmd.CombinedOutput()
}
