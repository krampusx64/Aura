package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"aurago/internal/config"
	"aurago/internal/security"
)

// EmailWatcher polls an IMAP folder for unseen messages and wakes
// the agent via a loopback HTTP request when new mail arrives.
type EmailWatcher struct {
	cfg      *config.Config
	logger   *slog.Logger
	guardian *security.Guardian
	stopCh   chan struct{}
	mu       sync.Mutex
	running  bool
	lastUIDs map[uint32]bool // track UIDs we already reported
}

func NewEmailWatcher(cfg *config.Config, logger *slog.Logger, guardian *security.Guardian) *EmailWatcher {
	return &EmailWatcher{
		cfg:      cfg,
		logger:   logger,
		guardian: guardian,
		stopCh:   make(chan struct{}),
		lastUIDs: make(map[uint32]bool),
	}
}

// Start begins the polling loop in a background goroutine.
func (ew *EmailWatcher) Start() {
	ew.mu.Lock()
	defer ew.mu.Unlock()
	if ew.running {
		return
	}
	ew.running = true

	interval := time.Duration(ew.cfg.Email.WatchInterval) * time.Second
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}

	ew.logger.Info("[EmailWatcher] Starting", "folder", ew.cfg.Email.WatchFolder, "interval", interval)

	go func() {
		// Initial seed: record current unseen UIDs without triggering
		ew.seedExistingUIDs()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ew.stopCh:
				ew.logger.Info("[EmailWatcher] Stopped")
				return
			case <-ticker.C:
				ew.poll()
			}
		}
	}()
}

func (ew *EmailWatcher) Stop() {
	ew.mu.Lock()
	defer ew.mu.Unlock()
	if !ew.running {
		return
	}
	ew.running = false
	close(ew.stopCh)
}

// seedExistingUIDs fetches current unseen UIDs so we don't alert on old mail at startup.
func (ew *EmailWatcher) seedExistingUIDs() {
	uids, err := SearchUnseenUIDs(
		ew.cfg.Email.IMAPHost, ew.cfg.Email.IMAPPort,
		ew.cfg.Email.Username, ew.cfg.Email.Password,
		ew.cfg.Email.WatchFolder,
	)
	if err != nil {
		ew.logger.Warn("[EmailWatcher] Seed fetch failed", "error", err)
		return
	}
	for _, uid := range uids {
		ew.lastUIDs[uid] = true
	}
	ew.logger.Info("[EmailWatcher] Seeded existing unseen UIDs", "count", len(uids))
}

func (ew *EmailWatcher) poll() {
	uids, err := SearchUnseenUIDs(
		ew.cfg.Email.IMAPHost, ew.cfg.Email.IMAPPort,
		ew.cfg.Email.Username, ew.cfg.Email.Password,
		ew.cfg.Email.WatchFolder,
	)
	if err != nil {
		ew.logger.Warn("[EmailWatcher] Poll failed", "error", err)
		return
	}

	var newUIDs []uint32
	for _, uid := range uids {
		if !ew.lastUIDs[uid] {
			newUIDs = append(newUIDs, uid)
			ew.lastUIDs[uid] = true
		}
	}

	if len(newUIDs) == 0 {
		return
	}

	ew.logger.Info("[EmailWatcher] New unseen emails detected", "count", len(newUIDs))

	// Fetch the new messages for a summary
	messages, err := FetchEmails(
		ew.cfg.Email.IMAPHost, ew.cfg.Email.IMAPPort,
		ew.cfg.Email.Username, ew.cfg.Email.Password,
		ew.cfg.Email.WatchFolder, len(newUIDs),
		ew.logger,
	)
	if err != nil {
		ew.logger.Warn("[EmailWatcher] Fetch for notification failed", "error", err)
		// Still notify the agent even without details
		ew.notifyAgent(fmt.Sprintf("[EMAIL NOTIFICATION] %d new email(s) in %s. Fetch details with fetch_email.", len(newUIDs), ew.cfg.Email.WatchFolder))
		return
	}

	// Build summary and run through Guardian
	var summary string
	for i, msg := range messages {
		content := fmt.Sprintf("From: %s | Subject: %s | Snippet: %s", msg.From, msg.Subject, msg.Snippet)
		// Guardian scan on email content
		if ew.guardian != nil {
			scanResult := ew.guardian.ScanForInjection(content)
			if scanResult.Level >= security.ThreatHigh {
				ew.logger.Warn("[EmailWatcher] HIGH threat detected in email, sanitizing", "from", msg.From, "subject", msg.Subject, "threat", scanResult.Level.String())
				content = fmt.Sprintf("From: %s | Subject: [SANITIZED - injection detected] | Snippet: [REDACTED]", msg.From)
			}
		}
		summary += fmt.Sprintf("\n%d. %s", i+1, content)
	}

	prompt := fmt.Sprintf("[EMAIL NOTIFICATION] %d new email(s) in %s:%s\n\nYou can use fetch_email for full content or send_email to reply.", len(messages), ew.cfg.Email.WatchFolder, summary)
	ew.notifyAgent(prompt)
}

// notifyAgent sends a loopback HTTP request to wake the agent.
func (ew *EmailWatcher) notifyAgent(prompt string) {
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", ew.cfg.Server.Port)

	msg := map[string]interface{}{
		"model": ew.cfg.LLM.Model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}

	payload, _ := json.Marshal(msg)

	client := &http.Client{Timeout: 3 * time.Minute}
	resp, err := client.Post(url, "application/json", bytes.NewBuffer(payload))
	if err != nil {
		ew.logger.Error("[EmailWatcher] Loopback notification failed", "error", err)
		return
	}
	resp.Body.Close()
	ew.logger.Info("[EmailWatcher] Agent notified", "status", resp.Status)
}

// StartEmailWatcher creates and starts an email watcher if enabled in config.
func StartEmailWatcher(cfg *config.Config, logger *slog.Logger, guardian *security.Guardian) *EmailWatcher {
	if !cfg.Email.Enabled || !cfg.Email.WatchEnabled {
		if cfg.Email.Enabled {
			logger.Info("[Email] Email enabled but watch_enabled is false — watcher not started")
		}
		return nil
	}

	if cfg.Email.IMAPHost == "" || cfg.Email.Username == "" || cfg.Email.Password == "" {
		logger.Warn("[EmailWatcher] Cannot start: IMAP host, username, or password not configured")
		return nil
	}

	watcher := NewEmailWatcher(cfg, logger, guardian)
	watcher.Start()
	return watcher
}
