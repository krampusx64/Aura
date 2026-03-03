package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"aurago/internal/agent"
	"aurago/internal/budget"
	"aurago/internal/config"
	"aurago/internal/discord"
	"aurago/internal/llm"
	"aurago/internal/memory"
	"aurago/internal/security"
	"aurago/internal/telegram"
	"aurago/internal/tools"
	"aurago/ui"
)

// normalizeLang converts the config language string to an ISO code for the frontend
func normalizeLang(lang string) string {
	l := strings.ToLower(strings.TrimSpace(lang))
	switch {
	case strings.Contains(l, "german") || strings.Contains(l, "deutsch") || l == "de":
		return "de"
	case strings.Contains(l, "english") || l == "en":
		return "en"
	case strings.Contains(l, "chinese") || strings.Contains(l, "mandarin") || l == "zh":
		return "zh"
	case strings.Contains(l, "hindi") || l == "hi":
		return "hi"
	default:
		return "en" // Fallback
	}
}

// Server holds the state and dependencies for the web server and socket bridge.
type Server struct {
	Cfg             *config.Config
	CfgMu           sync.RWMutex // protects Cfg during hot-reload
	Logger          *slog.Logger
	LLMClient       llm.ChatClient
	ShortTermMem    *memory.SQLiteMemory
	LongTermMem     memory.VectorDB
	Vault           *security.Vault
	Registry        *tools.ProcessRegistry
	CronManager     *tools.CronManager
	HistoryManager  *memory.HistoryManager
	KG              *memory.KnowledgeGraph
	InventoryDB     *sql.DB
	Guardian        *security.Guardian
	CoAgentRegistry *agent.CoAgentRegistry
	BudgetTracker   *budget.Tracker
	// IsFirstStart is true if core_memory.md was just freshly created (no prior data).
	IsFirstStart   bool
	ShutdownCh     chan struct{} // signal channel for graceful shutdown
	firstStartDone bool
	muFirstStart   sync.Mutex
}

func Start(cfg *config.Config, logger *slog.Logger, llmClient llm.ChatClient, shortTermMem *memory.SQLiteMemory, longTermMem memory.VectorDB, vault *security.Vault, registry *tools.ProcessRegistry, cronManager *tools.CronManager, historyManager *memory.HistoryManager, kg *memory.KnowledgeGraph, inventoryDB *sql.DB, isFirstStart bool, shutdownCh chan struct{}) error {
	s := &Server{
		Cfg:             cfg,
		Logger:          logger,
		LLMClient:       llmClient,
		ShortTermMem:    shortTermMem,
		LongTermMem:     longTermMem,
		Vault:           vault,
		Registry:        registry,
		CronManager:     cronManager,
		HistoryManager:  historyManager,
		KG:              kg,
		InventoryDB:     inventoryDB,
		Guardian:        security.NewGuardian(logger),
		CoAgentRegistry: agent.NewCoAgentRegistry(cfg.CoAgents.MaxConcurrent, logger),
		BudgetTracker:   budget.NewTracker(cfg, logger, cfg.Directories.DataDir),
		IsFirstStart:    isFirstStart,
		ShutdownCh:      shutdownCh,
	}

	// Initialize runtime debug mode from config
	agent.SetDebugMode(cfg.Agent.DebugMode)

	return s.run(shutdownCh)
}

func (s *Server) run(shutdownCh chan struct{}) error {
	mux := http.NewServeMux()
	sse := NewSSEBroadcaster()

	// Phase 34: Start the background daily reflection loop
	tools.StartDailyReflectionLoop(s.Cfg, s.Logger, s.LLMClient, s.HistoryManager, s.ShortTermMem)

	// Phase 68: Start the daily maintenance loop
	manifest := tools.NewManifest(s.Cfg.Directories.ToolsDir)
	agent.StartMaintenanceLoop(s.Cfg, s.Logger, s.LLMClient, s.Vault, s.Registry, manifest, s.CronManager, s.LongTermMem, s.ShortTermMem, s.HistoryManager, s.KG, s.InventoryDB)

	s.CoAgentRegistry.StartCleanupLoop()

	mux.HandleFunc("/v1/chat/completions", handleChatCompletions(s, sse))
	mux.HandleFunc("/api/memory/archive", handleArchiveMemory(s))
	mux.HandleFunc("/api/upload", handleUpload(s))
	mux.HandleFunc("/api/budget", handleBudgetStatus(s))
	mux.HandleFunc("/api/personalities", handleListPersonalities(s))
	mux.HandleFunc("/api/personality", handleUpdatePersonality(s))
	mux.HandleFunc("/api/personality/state", handlePersonalityState(s))
	mux.HandleFunc("/events", sse.ServeHTTP) // SSE usually authenticates via cookie/query; keeping open for now unless explicitly needed

	// Config UI endpoints (only when explicitly enabled for security)
	if s.Cfg.WebConfig.Enabled {
		mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				handleGetConfig(s)(w, r)
			case http.MethodPut:
				handleUpdateConfig(s)(w, r)
			default:
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			}
		})
		mux.HandleFunc("/api/config/schema", handleGetConfigSchema(s))
		mux.HandleFunc("/api/restart", handleRestart(s))
	}

	// Phase 35.2: Start the Telegram Long Polling loop
	telegram.StartLongPolling(s.Cfg, s.Logger, s.LLMClient, s.ShortTermMem, s.LongTermMem, s.Vault, s.Registry, s.CronManager, s.HistoryManager, s.KG, s.InventoryDB)

	// Discord Bot: listen for messages and relay to the agent
	discord.StartBot(s.Cfg, s.Logger, s.LLMClient, s.ShortTermMem, s.LongTermMem, s.Vault, s.Registry, s.CronManager, s.HistoryManager, s.KG, s.InventoryDB)

	// Email Watcher: poll IMAP for new messages and wake the agent
	tools.StartEmailWatcher(s.Cfg, s.Logger, s.Guardian)

	// Phase 34: Notifications endpoints
	mux.HandleFunc("/notifications", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		notes, err := s.ShortTermMem.GetUnreadNotifications()
		if err != nil {
			s.Logger.Error("Failed to fetch unread notifications", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(notes)
	})

	mux.HandleFunc("/notifications/read", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := s.ShortTermMem.MarkNotificationsRead(); err != nil {
			s.Logger.Error("Failed to mark notifications read", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/history", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		all := s.HistoryManager.GetAll()
		var filtered []memory.HistoryMessage
		for _, m := range all {
			if !m.IsInternal {
				filtered = append(filtered, m)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(filtered)
	})

	mux.HandleFunc("/clear", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := s.HistoryManager.Clear(); err != nil {
			s.Logger.Error("Failed to clear chat history", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/api/admin/stop", handleInterrupt(s))

	// Serve the embedded Web UI at root via html/template for i18n injection
	uiFS, err := fs.Sub(ui.Content, ".")
	if err != nil {
		return fmt.Errorf("failed to create UI filesystem: %w", err)
	}
	tmpl, err := template.ParseFS(uiFS, "index.html")
	if err != nil {
		s.Logger.Error("Failed to parse UI template", "error", err)
	}

	// Config page (separate template, guarded by WebConfig.Enabled)
	if s.Cfg.WebConfig.Enabled {
		cfgTmpl, cfgErr := template.ParseFS(uiFS, "config.html")
		if cfgErr != nil {
			s.Logger.Error("Failed to parse config UI template", "error", cfgErr)
		}
		mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
			if cfgTmpl == nil {
				http.Error(w, "Config template error", http.StatusInternalServerError)
				return
			}
			data := map[string]interface{}{
				"Lang": normalizeLang(s.Cfg.Agent.SystemLanguage),
			}
			if err := cfgTmpl.Execute(w, data); err != nil {
				s.Logger.Error("Failed to execute config template", "error", err)
				http.Error(w, "Template render error", http.StatusInternalServerError)
			}
		})
		// Serve the help texts JSON
		mux.HandleFunc("/config_help.json", func(w http.ResponseWriter, r *http.Request) {
			helpData, err := fs.ReadFile(uiFS, "config_help.json")
			if err != nil {
				http.Error(w, "Not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(helpData)
		})
		s.Logger.Info("Config UI enabled at /config")
	}

	staticHandler := http.FileServer(http.FS(uiFS))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			if tmpl != nil {
				data := map[string]interface{}{
					"Lang":               normalizeLang(s.Cfg.Agent.SystemLanguage),
					"ShowToolResults":    s.Cfg.Agent.ShowToolResults,
					"DebugMode":          agent.GetDebugMode(),
					"PersonalityEnabled": s.Cfg.Agent.PersonalityEngine,
				}
				if err := tmpl.Execute(w, data); err != nil {
					s.Logger.Error("Failed to execute UI template", "error", err)
					http.Error(w, "Template render error", http.StatusInternalServerError)
					return
				}
			} else {
				http.Error(w, "Template error", http.StatusInternalServerError)
			}
			return
		}
		// Serve static assets from embedded UI FS (logos, etc.)
		staticHandler.ServeHTTP(w, r)
	})

	// Serve static files securely from the workspace directory
	fsHandler := http.StripPrefix("/files/", http.FileServer(neuteredFileSystem{http.Dir(s.Cfg.Directories.WorkspaceDir)}))
	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fsHandler.ServeHTTP(w, r)
	})

	// Phase X: Dedicated TTS Server for Chromecast
	if s.Cfg.Chromecast.Enabled && s.Cfg.Chromecast.TTSPort > 0 {
		ttsDir := tools.TTSAudioDir(s.Cfg.Directories.DataDir)
		ttsMux := http.NewServeMux()
		ttsFsHandler := http.StripPrefix("/tts/", http.FileServer(http.Dir(ttsDir)))
		ttsMux.HandleFunc("/tts/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			ttsFsHandler.ServeHTTP(w, r)
		})

		ttsServer := &http.Server{
			Addr:    fmt.Sprintf("0.0.0.0:%d", s.Cfg.Chromecast.TTSPort),
			Handler: ttsMux,
		}

		go func() {
			defer func() {
				if r := recover(); r != nil {
					s.Logger.Error("[TTS Server] Goroutine panic recovered", "error", r)
				}
			}()
			s.Logger.Info("Starting Dedicated TTS Server", "host", "0.0.0.0", "port", s.Cfg.Chromecast.TTSPort)
			if err := ttsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				s.Logger.Warn("Dedicated TTS Server failed (Chromecast audio will not be available)", "error", err)
			}
		}()
	}

	addr := fmt.Sprintf("%s:%d", s.Cfg.Server.Host, s.Cfg.Server.Port)
	s.Logger.Info("Starting server", "host", s.Cfg.Server.Host, "port", s.Cfg.Server.Port)

	// Start Phase 1 TCP Bridge
	bridgeAddr := s.Cfg.Server.BridgeAddress
	if bridgeAddr == "" {
		bridgeAddr = "localhost:8089"
	}
	go s.StartTCPBridge(bridgeAddr)

	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute, // generous for streaming responses
		IdleTimeout:  2 * time.Minute,
	}

	// Graceful shutdown goroutine
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.Logger.Error("[Shutdown] Goroutine panic recovered", "error", r)
			}
		}()
		<-shutdownCh
		s.Logger.Info("Initiating graceful HTTP server shutdown...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			s.Logger.Error("HTTP server shutdown error", "error", err)
		}
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	s.Logger.Info("Server stopped gracefully")
	return nil
}
