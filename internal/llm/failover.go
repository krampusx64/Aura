package llm

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"aurago/internal/config"

	"github.com/sashabaranov/go-openai"
)

// FailoverManager implements ChatClient.  It wraps a primary and an optional
// fallback LLM endpoint.  When the primary accumulates enough consecutive
// errors it transparently switches to the fallback.  A background goroutine
// periodically probes the primary and switches back on success.
//
// If fallback is disabled in config, FailoverManager is a thin passthrough
// around the primary client so existing behaviour is preserved.
type FailoverManager struct {
	mu sync.RWMutex

	primary       *openai.Client
	fallback      *openai.Client // nil when fallback not configured
	primaryModel  string
	fallbackModel string

	isOnFallback   bool
	errorCount     int
	errorThreshold int
	probeInterval  time.Duration

	stopCh chan struct{} // closed by Stop() to signal probeLoop to exit

	logger *slog.Logger
}

// NewFailoverManager creates a FailoverManager from cfg.
// The probe goroutine is started if fallback is enabled.
func NewFailoverManager(cfg *config.Config, logger *slog.Logger) *FailoverManager {
	primary := NewClient(cfg)

	fm := &FailoverManager{
		primary:        primary,
		primaryModel:   cfg.LLM.Model,
		errorThreshold: 3,
		probeInterval:  60 * time.Second,
		stopCh:         make(chan struct{}),
		logger:         logger,
	}

	fb := cfg.FallbackLLM
	if !fb.Enabled || fb.BaseURL == "" {
		return fm // passthrough mode
	}

	// Build fallback client reusing NewClient logic
	fallbackCfg := *cfg
	fallbackCfg.LLM.BaseURL = fb.BaseURL
	fallbackCfg.LLM.APIKey = fb.APIKey
	fallbackCfg.LLM.Model = fb.Model
	fm.fallback = NewClient(&fallbackCfg)
	fm.fallbackModel = fb.Model

	if fb.ErrorThreshold > 0 {
		fm.errorThreshold = fb.ErrorThreshold
	}
	if fb.ProbeIntervalSeconds > 0 {
		fm.probeInterval = time.Duration(fb.ProbeIntervalSeconds) * time.Second
	}

	go fm.probeLoop()
	return fm
}

// Stop signals the background probe goroutine to exit. Call during server shutdown.
func (fm *FailoverManager) Stop() {
	select {
	case <-fm.stopCh:
		// already closed
	default:
		close(fm.stopCh)
	}
}

// CreateChatCompletion satisfies ChatClient.
func (fm *FailoverManager) CreateChatCompletion(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	client, model := fm.active()
	req.Model = model

	resp, err := client.CreateChatCompletion(ctx, req)
	if err != nil {
		fm.recordError(err)
	} else {
		fm.recordSuccess()
	}
	return resp, err
}

// CreateChatCompletionStream satisfies ChatClient.
func (fm *FailoverManager) CreateChatCompletionStream(ctx context.Context, req openai.ChatCompletionRequest) (*openai.ChatCompletionStream, error) {
	client, model := fm.active()
	req.Model = model

	stream, err := client.CreateChatCompletionStream(ctx, req)
	if err != nil {
		fm.recordError(err)
	} else {
		fm.recordSuccess()
	}
	return stream, err
}

// active returns the currently active client and model under a read lock.
func (fm *FailoverManager) active() (*openai.Client, string) {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	if fm.isOnFallback {
		return fm.fallback, fm.fallbackModel
	}
	return fm.primary, fm.primaryModel
}

// recordError increments the error counter and switches to fallback if the
// threshold is reached.  Context-cancelled errors are ignored.
func (fm *FailoverManager) recordError(err error) {
	if err == nil || isContextError(err) {
		return
	}
	fm.mu.Lock()
	defer fm.mu.Unlock()

	fm.errorCount++
	if fm.fallback == nil || fm.isOnFallback {
		// No fallback configured, or already on fallback – just log.
		if fm.isOnFallback {
			fm.logger.Warn("LLM failover: error on fallback endpoint", "error", err, "count", fm.errorCount)
			fm.errorCount = 0 // reset so we don't spam
		}
		return
	}

	fm.logger.Warn("LLM failover: primary error recorded", "error", err, "count", fm.errorCount, "threshold", fm.errorThreshold)
	if fm.errorCount >= fm.errorThreshold {
		fm.logger.Warn("LLM failover: switching to fallback endpoint", "model", fm.fallbackModel)
		fm.isOnFallback = true
		fm.errorCount = 0
	}
}

// recordSuccess resets the error counter when we are on the primary.
func (fm *FailoverManager) recordSuccess() {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if !fm.isOnFallback {
		fm.errorCount = 0
	}
}

// probeLoop runs in a background goroutine.  While on fallback it pings the
// primary at probeInterval; on success it switches back.
func (fm *FailoverManager) probeLoop() {
	ticker := time.NewTicker(fm.probeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-fm.stopCh:
			fm.logger.Debug("LLM failover: probe loop stopped")
			return
		case <-ticker.C:
			fm.mu.RLock()
			onFallback := fm.isOnFallback
			fm.mu.RUnlock()

			if !onFallback {
				continue
			}

			fm.logger.Debug("LLM failover: probing primary endpoint…")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, err := fm.primary.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
				Model: fm.primaryModel,
				Messages: []openai.ChatCompletionMessage{
					{Role: openai.ChatMessageRoleUser, Content: "answer only with ok"},
				},
				MaxTokens: 5,
			})
			cancel()

			if err != nil {
				fm.logger.Debug("LLM failover: primary still unavailable", "error", err)
				continue
			}

			fm.mu.Lock()
			fm.isOnFallback = false
			fm.errorCount = 0
			fm.mu.Unlock()
			fm.logger.Info("LLM failover: primary recovered – switched back", "model", fm.primaryModel)
		}
	}
}

// isContextError returns true if the error is caused by context cancellation
// or deadline exceeded – these should not count towards the failure threshold.
func isContextError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "context canceled") ||
		strings.Contains(s, "context deadline exceeded")
}
