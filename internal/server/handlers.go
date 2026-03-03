package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"aurago/internal/agent"
	"aurago/internal/commands"
	"aurago/internal/llm"
	"aurago/internal/memory"
	"aurago/internal/prompts"
	"aurago/internal/tools"

	"github.com/google/uuid"
	"github.com/sashabaranov/go-openai"
)

var (
	followUpDepths = make(map[string]int)
	muFollowUp     sync.Mutex
)

func handleChatCompletions(s *Server, sse *SSEBroadcaster) http.HandlerFunc {
	// Pre-create manifest once — it caches internally and auto-reloads on file changes
	manifest := tools.NewManifest(s.Cfg.Directories.ToolsDir)
	return func(w http.ResponseWriter, r *http.Request) {
		// Maintenance check: Inform the log but allow interaction via agent loop
		inMaintenance := tools.IsBusy()
		if inMaintenance {
			s.Logger.Info("Processing request in Maintenance Mode")
		}

		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Limit request body to 1 MB to prevent abuse
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

		var req openai.ChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.Logger.Error("Failed to decode request body", "error", err)
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}

		s.Logger.Debug("Received chat completion request", "model", req.Model, "messages_count", len(req.Messages), "stream", req.Stream)

		// Check for Follow-Up loop protection
		isFollowUp := r.Header.Get("X-Internal-FollowUp") == "true"
		followUpKey := "default" // sessionID is resolved later; hardcoded for now
		muFollowUp.Lock()
		if !isFollowUp {
			followUpDepths[followUpKey] = 0 // Reset on real user request
		} else {
			followUpDepths[followUpKey]++
			if followUpDepths[followUpKey] > 10 {
				muFollowUp.Unlock()
				s.Logger.Warn("Blocked follow_up execution to prevent infinite loop", "depth", followUpDepths[followUpKey])
				http.Error(w, `{"error": "Follow-up circuit breaker tripped. Max recursion depth reached."}`, http.StatusTooManyRequests)
				return
			}
		}
		muFollowUp.Unlock()

		// Override the model with the configured backend model
		if s.Cfg.LLM.Model != "" {
			s.Logger.Debug("Overriding model", "from", req.Model, "to", s.Cfg.LLM.Model)
			req.Model = s.Cfg.LLM.Model
		}

		if len(req.Messages) == 0 {
			http.Error(w, "No messages provided", http.StatusBadRequest)
			return
		}

		// 1. Save User Input to Short-Term Memory
		lastUserMsg := req.Messages[len(req.Messages)-1]
		sessionID := "default" // hardcoded until API supports it

		// Guardian: Scan user input for injection patterns (log only, never block)
		if lastUserMsg.Role == openai.ChatMessageRoleUser && s.Guardian != nil {
			s.Guardian.ScanUserInput(lastUserMsg.Content)
		}

		// Phase: Command Interception
		if lastUserMsg.Role == openai.ChatMessageRoleUser && strings.HasPrefix(lastUserMsg.Content, "/") {
			// Intercept Slash Commands
			cmdCtx := commands.Context{
				STM:           s.ShortTermMem,
				HM:            s.HistoryManager,
				Vault:         s.Vault,
				InventoryDB:   s.InventoryDB,
				BudgetTracker: s.BudgetTracker,
				Cfg:           s.Cfg,
				PromptsDir:    s.Cfg.Directories.PromptsDir,
			}
			cmdResult, isCommand, err := commands.Handle(lastUserMsg.Content, cmdCtx)
			if err != nil {
				s.Logger.Error("Command execution failed", "error", err)
				http.Error(w, "Command failed", http.StatusInternalServerError)
				return
			}
			if isCommand {
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
					ID:      "cmd-" + uuid.New().String(),
					Object:  "chat.completion",
					Created: time.Now().Unix(),
					Model:   "aurago-cmd",
					Choices: []openai.ChatCompletionChoice{
						{
							Index: 0,
							Message: openai.ChatCompletionMessage{
								Role:    openai.ChatMessageRoleAssistant,
								Content: cmdResult,
							},
							FinishReason: openai.FinishReasonStop,
						},
					},
				}); err != nil {
					s.Logger.Error("Failed to encode command response", "error", err)
					http.Error(w, "Internal server error", http.StatusInternalServerError)
				}
				return
			}
		}

		if lastUserMsg.Role == openai.ChatMessageRoleUser {
			id, err := s.ShortTermMem.InsertMessage(sessionID, lastUserMsg.Role, lastUserMsg.Content, false, false)
			if err != nil {
				s.Logger.Error("Failed to insert user message", "error", err)
			}
			if sessionID == "default" {
				s.HistoryManager.Add(lastUserMsg.Role, lastUserMsg.Content, id, false, false)
			}
		}

		// 2. Rebuild the Context
		recentMessages := s.HistoryManager.Get()

		// Phase 33: Recursive Context Compression (Character Based)
		charLimit := s.Cfg.Agent.MemoryCompressionCharLimit
		if s.HistoryManager.TotalChars() >= charLimit {
			if s.HistoryManager.TryLockCompression() {
				// Safety Check: Check if pinned messages exceed 50% of the limit
				pinnedChars := s.HistoryManager.TotalPinnedChars()
				if pinnedChars > charLimit/2 {
					s.Logger.Warn("[Compression] Context overcrowded with pinned messages", "pinned_chars", pinnedChars, "limit", charLimit)
					warningMsg := fmt.Sprintf("WARNING: Pinned messages are consuming %d characters, which is over 50%% of your memory limit (%d). Consider unpinning old information to maintain full context reliability.", pinnedChars, charLimit)
					// Inject warning to agent
					id, err := s.ShortTermMem.InsertMessage(sessionID, openai.ChatMessageRoleSystem, warningMsg, false, false)
					if err != nil {
						s.Logger.Error("Failed to insert compression warning", "error", err)
					}
					s.HistoryManager.Add(openai.ChatMessageRoleSystem, warningMsg, id, false, false)
				}

				// We want to compress about 20% of the limit or at least enough to be under the limit
				targetPruneChars := charLimit / 5
				messagesToSummarize, actualChars := s.HistoryManager.GetOldestMessagesForPruning(targetPruneChars)

				if len(messagesToSummarize) > 0 {
					go func(msgs []memory.HistoryMessage, charsPruned int, existingSummary string) {
						defer s.HistoryManager.UnlockCompression()
						defer func() {
							if r := recover(); r != nil {
								s.Logger.Error("[Compression] Goroutine panic recovered", "error", r)
							}
						}()
						s.Logger.Info("[Compression] Triggering character-based context compression",
							"msg_count", len(msgs), "chars", charsPruned, "limit", charLimit)

						prompt := "Update the following 'Persistent Summary' with the details from the 'Recent Messages' below. Maintain a chronological flow of facts, technical decisions, and user preferences. Ensure metadata is explicitly protected. Result must be a concise briefing.\n\n"
						if existingSummary != "" {
							prompt += "[\"Persistent Summary\"]:\n" + existingSummary + "\n\n"
						}
						prompt += "[\"Recent Messages\"]:\n"
						var dropIDs []int64
						for _, m := range msgs {
							prompt += fmt.Sprintf("[%s]: %s\n\n", m.Role, m.Content)
							dropIDs = append(dropIDs, m.ID)
						}

						summaryReq := openai.ChatCompletionRequest{
							Model: s.Cfg.LLM.Model,
							Messages: []openai.ChatCompletionMessage{
								{Role: openai.ChatMessageRoleSystem, Content: prompt},
							},
							MaxTokens:   1000,
							Temperature: 0.3,
						}

						bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
						defer cancel()

						resp, err := llm.ExecuteWithRetry(bgCtx, s.LLMClient, summaryReq, s.Logger, nil)
						if err != nil {
							s.Logger.Error("[Compression] Background summarization failed", "error", err)
							return
						}

						if len(resp.Choices) > 0 {
							newSummary := resp.Choices[0].Message.Content
							s.HistoryManager.SetSummary(newSummary)
							s.HistoryManager.DropMessages(dropIDs)
							// In SQLite we still delete by count for now, or we could update ShortTermMem to delete by ID list
							// For simplicity and since HistoryManager is the source of truth for active context, we'll stick to this.
							// However, stm.DeleteOldMessages might delete pinned ones if we are not careful.
							// Requirement: "rest weiterhin komprimiert wird".
							// Let's add a DeleteMessagesByID to ShortTermMem too.
							if err := s.ShortTermMem.DeleteMessagesByID(sessionID, dropIDs); err != nil {
								s.Logger.Error("[Compression] Failed to clean up SQLite memory", "error", err)
							}
							s.Logger.Info("[Compression] Background summarization complete and saved",
								"summary_len", len(newSummary), "messages_dropped", len(dropIDs))
						}
					}(messagesToSummarize, actualChars, s.HistoryManager.GetSummary())
				} else {
					s.HistoryManager.UnlockCompression()
				}
			}
		}

		// 3. Inject Dynamic Core System Prompt with RAG
		var retrievedMemories string
		if lastUserMsg.Role == openai.ChatMessageRoleUser && lastUserMsg.Content != "" {
			memories, docIDs, err := s.LongTermMem.SearchSimilar(lastUserMsg.Content, 2)
			if err == nil {
				for _, docID := range docIDs {
					_ = s.ShortTermMem.UpdateMemoryAccess(docID)
				}
				if len(memories) > 0 {
					retrievedMemories = strings.Join(memories, "\n---\n")
					s.Logger.Debug("RAG: Retrieved memories", "count", len(memories))
				}
			}
		}

		// Load Core Memory (Semi-Static)
		coreMem := s.ShortTermMem.ReadCoreMemory()

		flags := prompts.ContextFlags{
			IsErrorState:           false,
			RequiresCoding:         false,
			RetrievedMemories:      retrievedMemories,
			SystemLanguage:         s.Cfg.Agent.SystemLanguage,
			CorePersonality:        s.Cfg.Agent.CorePersonality,
			LifeboatEnabled:        s.Cfg.Maintenance.LifeboatEnabled,
			IsMaintenanceMode:      inMaintenance,
			TokenBudget:            s.Cfg.Agent.SystemPromptTokenBudget,
			MessageCount:           len(recentMessages),
			DiscordEnabled:         s.Cfg.Discord.Enabled,
			EmailEnabled:           s.Cfg.Email.Enabled,
			DockerEnabled:          s.Cfg.Docker.Enabled,
			HomeAssistantEnabled:   s.Cfg.HomeAssistant.Enabled,
			WebDAVEnabled:          s.Cfg.WebDAV.Enabled,
			KoofrEnabled:           s.Cfg.Koofr.Enabled,
			ChromecastEnabled:      s.Cfg.Chromecast.Enabled,
			CoAgentEnabled:         s.Cfg.CoAgents.Enabled,
			GoogleWorkspaceEnabled: s.Cfg.Agent.EnableGoogleWorkspace,
		}
		sysPrompt := prompts.BuildSystemPrompt(s.Cfg.Directories.PromptsDir, flags, coreMem, s.Logger)

		finalMessages := []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: sysPrompt},
		}

		currentSummary := s.HistoryManager.GetSummary()
		if currentSummary != "" {
			finalMessages = append(finalMessages, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleSystem,
				Content: "[CONTEXT_RECAP]: The following is a summary of previous relevant discussions for context. DO NOT echo or repeat this recap in your response:\n" + currentSummary,
			})
		}

		finalMessages = append(finalMessages, recentMessages...)

		// First-start: inject a one-time naming prompt so the agent asks the user
		// for a personal name on the very first conversation.
		if s.IsFirstStart {
			s.muFirstStart.Lock()
			if !s.firstStartDone {
				s.firstStartDone = true
				s.muFirstStart.Unlock()
				s.Logger.Info("[FirstStart] Injecting one-time naming prompt")
				finalMessages = append(finalMessages, openai.ChatCompletionMessage{
					Role: openai.ChatMessageRoleSystem,
					Content: "[FIRST START INITIALIZATION — ONE TIME ONLY] Before responding to the user's message, " +
						"ask them: would they like to give you a personal name, or should you choose one yourself? " +
						"Wait for their answer, then settle on a name. " +
						"Immediately after the name is decided, save it permanently to core memory " +
						"using the manage_memory tool (operation \"add\", fact: \"My name is <chosen_name>\"). " +
						"Do not skip this step.",
				})
			} else {
				s.muFirstStart.Unlock()
			}
		}

		req.Messages = finalMessages

		// 4. Pass execution to the unified agent loop
		runCfg := agent.RunConfig{
			Config:          s.Cfg,
			Logger:          s.Logger,
			LLMClient:       s.LLMClient,
			ShortTermMem:    s.ShortTermMem,
			HistoryManager:  s.HistoryManager,
			LongTermMem:     s.LongTermMem,
			KG:              s.KG,
			InventoryDB:     s.InventoryDB,
			Vault:           s.Vault,
			Registry:        s.Registry,
			Manifest:        manifest,
			CronManager:     s.CronManager,
			CoAgentRegistry: s.CoAgentRegistry,
			BudgetTracker:   s.BudgetTracker,
			SessionID:       sessionID,
			IsMaintenance:   inMaintenance,
			SurgeryPlan:     "", // UI-driven chats don't currently pass a formal surgery plan
		}

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")

			flusher, ok := w.(http.Flusher)
			if !ok {
				s.Logger.Error("Streaming not supported by ResponseWriter")
				http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
				return
			}
			// Initial flush to establish SSE connection
			flusher.Flush()

			_, err := agent.ExecuteAgentLoop(r.Context(), req, runCfg, true, sse)
			if err != nil {
				s.Logger.Error("Streamed agent loop failed", "error", err)
				return
			}

			// Conclude SSE stream nicely
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			flusher.Flush()

		} else {
			resp, err := agent.ExecuteAgentLoop(r.Context(), req, runCfg, false, sse)
			if err != nil {
				s.Logger.Error("Sync agent loop failed", "error", err)
				// Return a user-visible error as a proper OpenAI response instead of HTTP 500
				errMsg := "⚠️ The request timed out — the model did not respond in time. Please try again or switch to a faster model."
				if !strings.Contains(err.Error(), "context deadline exceeded") && !strings.Contains(err.Error(), "context canceled") {
					errMsg = "⚠️ An internal error occurred. Check server logs for details."
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
					ID:      "err-" + sessionID,
					Object:  "chat.completion",
					Created: time.Now().Unix(),
					Model:   "aurago",
					Choices: []openai.ChatCompletionChoice{{
						Index: 0,
						Message: openai.ChatCompletionMessage{
							Role:    openai.ChatMessageRoleAssistant,
							Content: errMsg,
						},
						FinishReason: openai.FinishReasonStop,
					}},
				})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		}
	}
}

func handleArchiveMemory(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Limit request body to 10 MB for batch archive uploads
		r.Body = http.MaxBytesReader(w, r.Body, 10<<20)

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			s.Logger.Error("Failed to read archive request body", "error", err)
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}

		trimmed := strings.TrimSpace(string(bodyBytes))

		if strings.HasPrefix(trimmed, "[") {
			var items []memory.ArchiveItem
			if err := json.Unmarshal(bodyBytes, &items); err != nil {
				s.Logger.Error("Failed to decode batch archive request", "error", err)
				http.Error(w, "Bad request", http.StatusBadRequest)
				return
			}

			if len(items) == 0 {
				http.Error(w, "Empty batch", http.StatusBadRequest)
				return
			}

			storedIDs, err := s.LongTermMem.StoreBatch(items)
			if err != nil {
				s.Logger.Error("Failed to archive batch", "error", err)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}
			for _, id := range storedIDs {
				_ = s.ShortTermMem.UpsertMemoryMeta(id)
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "archived": len(items)})
		} else {
			var req memory.ArchiveItem
			if err := json.Unmarshal(bodyBytes, &req); err != nil {
				s.Logger.Error("Failed to decode archive request", "error", err)
				http.Error(w, "Bad request", http.StatusBadRequest)
				return
			}

			if req.Concept == "" || req.Content == "" {
				http.Error(w, "Both 'concept' and 'content' are required", http.StatusBadRequest)
				return
			}

			storedIDs, err := s.LongTermMem.StoreDocument(req.Concept, req.Content)
			if err != nil {
				s.Logger.Error("Failed to archive memory", "error", err)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}
			for _, id := range storedIDs {
				_ = s.ShortTermMem.UpsertMemoryMeta(id)
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "ok", "concept": req.Concept})
		}
	}
}

func handleInterrupt(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		s.Logger.Warn("Stop requested via Web UI")

		agent.InterruptSession("default")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "success",
			"message": "Agent interrupted. It will stop after the current step.",
		})
	}
}

// handleUpload receives a multipart file upload and saves it to
// {workspace_dir}/attachments/, returning the agent-visible path.
func handleUpload(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// 32 MB max upload size
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "failed to parse form", http.StatusBadRequest)
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing file field", http.StatusBadRequest)
			return
		}
		defer file.Close()

		// Sanitize filename
		base := filepath.Base(header.Filename)
		base = strings.ReplaceAll(base, " ", "_")
		base = strings.ReplaceAll(base, "..", "")
		if base == "" || base == "." {
			base = "upload.bin"
		}

		ts := time.Now().Format("20060102_150405")
		filename := ts + "_" + base

		// Save to {workspace_dir}/attachments/
		attachDir := filepath.Join(s.Cfg.Directories.WorkspaceDir, "attachments")
		if err := os.MkdirAll(attachDir, 0755); err != nil {
			s.Logger.Error("Failed to create attachments dir", "error", err)
			http.Error(w, "failed to create dir", http.StatusInternalServerError)
			return
		}

		destPath := filepath.Join(attachDir, filename)
		dst, err := os.Create(destPath)
		if err != nil {
			s.Logger.Error("Failed to create upload file", "error", err)
			http.Error(w, "failed to write file", http.StatusInternalServerError)
			return
		}
		defer dst.Close()

		if _, err := io.Copy(dst, file); err != nil {
			s.Logger.Error("Failed to write uploaded file", "error", err)
			http.Error(w, "failed to save file", http.StatusInternalServerError)
			return
		}

		s.Logger.Info("File uploaded via Web UI", "filename", filename, "size", header.Size)

		// Return the path the agent should use (relative to project root)
		agentPath := "agent_workspace/workdir/attachments/" + filename

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"path":     agentPath,
			"filename": header.Filename,
		})
	}
}

// handleBudgetStatus returns the current budget status as JSON.
func handleBudgetStatus(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if s.BudgetTracker == nil {
			w.Write([]byte(`{"enabled": false}`))
			return
		}
		w.Write([]byte(s.BudgetTracker.GetStatusJSON()))
	}
}

func handleListPersonalities(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		personalitiesDir := filepath.Join(s.Cfg.Directories.PromptsDir, "personalities")
		files, err := os.ReadDir(personalitiesDir)
		if err != nil {
			s.Logger.Error("Failed to read personalities directory", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		var profiles []string
		for _, f := range files {
			if !f.IsDir() && strings.HasSuffix(f.Name(), ".md") {
				profiles = append(profiles, strings.TrimSuffix(f.Name(), ".md"))
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"active":        s.Cfg.Agent.CorePersonality,
			"personalities": profiles,
		})
	}
}

func handlePersonalityState(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if !s.Cfg.Agent.PersonalityEngine {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"enabled": false})
			return
		}

		traits, err := s.ShortTermMem.GetTraits()
		if err != nil {
			s.Logger.Error("Failed to get personality traits", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		mood := s.ShortTermMem.GetCurrentMood()
		trigger := s.ShortTermMem.GetLastMoodTrigger()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled": true,
			"mood":    string(mood),
			"trigger": trigger,
			"traits":  traits,
		})
	}
}

func handleUpdatePersonality(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}

		if req.ID == "" {
			http.Error(w, "Personality ID is required", http.StatusBadRequest)
			return
		}

		// Verify existence
		profilePath := filepath.Join(s.Cfg.Directories.PromptsDir, "personalities", req.ID+".md")
		if _, err := os.Stat(profilePath); os.IsNotExist(err) {
			http.Error(w, "Personality not found", http.StatusNotFound)
			return
		}

		// Update config
		s.Cfg.Agent.CorePersonality = req.ID

		// Save config
		configPath := s.Cfg.ConfigPath
		if configPath == "" {
			configPath = "config.yaml"
		}
		if err := s.Cfg.Save(configPath); err != nil {
			s.Logger.Error("Failed to save config", "error", err)
			http.Error(w, "Failed to persist configuration", http.StatusInternalServerError)
			return
		}

		s.Logger.Info("Core personality updated", "id", req.ID)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "active": req.ID})
	}
}
