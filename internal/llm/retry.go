package llm

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sashabaranov/go-openai"
)

// RetryIntervals defines the wait times between retries: 30s, then 2m, then 10m (FinalRetryInterval).
var RetryIntervals = []time.Duration{
	30 * time.Second,
	2 * time.Minute,
}

const FinalRetryInterval = 10 * time.Minute

// FeedbackProvider allows the retry loop to notify the UI/Transports
type FeedbackProvider interface {
	Send(event, message string)
}

// ExecuteWithRetry wraps CreateChatCompletion with the specified retry logic
func ExecuteWithRetry(ctx context.Context, client ChatClient, req openai.ChatCompletionRequest, logger *slog.Logger, broker FeedbackProvider) (openai.ChatCompletionResponse, error) {
	return ExecuteWithCustomRetry(ctx, client, req, logger, broker, RetryIntervals, FinalRetryInterval)
}

// ExecuteWithCustomRetry allows specifying custom intervals
func ExecuteWithCustomRetry(ctx context.Context, client ChatClient, req openai.ChatCompletionRequest, logger *slog.Logger, broker FeedbackProvider, intervals []time.Duration, finalInterval time.Duration) (openai.ChatCompletionResponse, error) {
	attempt := 0

	for {
		resp, err := client.CreateChatCompletion(ctx, req)
		if err == nil {
			return resp, nil
		}

		// Check if it's a transient error or rate limit
		lowerErr := strings.ToLower(err.Error())
		isTransient := strings.Contains(lowerErr, "too many requests") ||
			strings.Contains(lowerErr, "rate limit") ||
			strings.Contains(lowerErr, "timeout") ||
			strings.Contains(lowerErr, "deadline") ||
			strings.Contains(lowerErr, "connection") ||
			strings.Contains(lowerErr, "503") ||
			strings.Contains(lowerErr, "502") ||
			strings.Contains(lowerErr, "504")

		if !isTransient {
			return openai.ChatCompletionResponse{}, err
		}

		// Determine wait time
		var waitTime time.Duration
		if attempt < len(intervals) {
			waitTime = intervals[attempt]
		} else {
			waitTime = finalInterval
		}

		attempt++
		msg := fmt.Sprintf("API Error (%s). Retrying in %v (Attempt %d)...", err.Error(), waitTime, attempt)
		logger.Warn("[LLM Retry]", "error", err, "wait", waitTime, "attempt", attempt)

		if broker != nil {
			broker.Send("api_retry", msg)
		}

		// Wait or cancel
		select {
		case <-time.After(waitTime):
			// continue loop
		case <-ctx.Done():
			return openai.ChatCompletionResponse{}, ctx.Err()
		}
	}
}

// ExecuteStreamWithRetry wraps CreateChatCompletionStream with the specified retry logic
func ExecuteStreamWithRetry(ctx context.Context, client ChatClient, req openai.ChatCompletionRequest, logger *slog.Logger, broker FeedbackProvider) (*openai.ChatCompletionStream, error) {
	return ExecuteStreamWithCustomRetry(ctx, client, req, logger, broker, RetryIntervals, FinalRetryInterval)
}

// ExecuteStreamWithCustomRetry allows specifying custom intervals
func ExecuteStreamWithCustomRetry(ctx context.Context, client ChatClient, req openai.ChatCompletionRequest, logger *slog.Logger, broker FeedbackProvider, intervals []time.Duration, finalInterval time.Duration) (*openai.ChatCompletionStream, error) {
	attempt := 0

	for {
		stream, err := client.CreateChatCompletionStream(ctx, req)
		if err == nil {
			return stream, nil
		}

		// Check if it's a transient error or rate limit
		lowerErr := strings.ToLower(err.Error())
		isTransient := strings.Contains(lowerErr, "too many requests") ||
			strings.Contains(lowerErr, "rate limit") ||
			strings.Contains(lowerErr, "timeout") ||
			strings.Contains(lowerErr, "deadline") ||
			strings.Contains(lowerErr, "connection") ||
			strings.Contains(lowerErr, "503") ||
			strings.Contains(lowerErr, "502") ||
			strings.Contains(lowerErr, "504")

		if !isTransient {
			return nil, err
		}

		// Determine wait time
		var waitTime time.Duration
		if attempt < len(intervals) {
			waitTime = intervals[attempt]
		} else {
			waitTime = finalInterval
		}

		attempt++
		msg := fmt.Sprintf("Stream API Error (%s). Retrying in %v (Attempt %d)...", err.Error(), waitTime, attempt)
		logger.Warn("[LLM Stream Retry]", "error", err, "wait", waitTime, "attempt", attempt)

		if broker != nil {
			broker.Send("api_retry", msg)
		}

		// Wait or cancel
		select {
		case <-time.After(waitTime):
			// continue loop
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
