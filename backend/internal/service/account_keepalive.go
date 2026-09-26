package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const accountKeepaliveContextKey = "account_keepalive"
const keepalivePrompt = "Reply with exactly: OK"
const keepaliveMaxTokens = 128

type accountKeepaliveState struct {
	status     int
	retryAfter time.Duration
	observed   *http.Response
}

func keepaliveState(c *gin.Context) *accountKeepaliveState {
	value, _ := c.Get(accountKeepaliveContextKey)
	state, _ := value.(*accountKeepaliveState)
	return state
}

func isKeepaliveMediaModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return isOpenAIImageModel(model) || isImageGenerationModel(model) ||
		isGrokImageGenerationModel(model) || isGrokVideoGenerationModel(model) ||
		strings.Contains(model, "audio") || strings.Contains(model, "realtime") ||
		strings.Contains(model, "tts") || strings.HasPrefix(model, "whisper") ||
		strings.HasPrefix(model, "sora") || strings.HasPrefix(model, "dall-e")
}

// Keep the established authentication and client wire shape, but limit work to
// a short text answer. Codex OAuth does not accept max_output_tokens.
func applyKeepalivePayload(c *gin.Context, payload map[string]any, protocol string, oauth bool) {
	if keepaliveState(c) == nil {
		return
	}
	switch protocol {
	case "anthropic":
		payload["max_tokens"] = keepaliveMaxTokens
	case "responses":
		payload["input"] = []map[string]any{{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": keepalivePrompt}}}}
		if !oauth {
			payload["max_output_tokens"] = keepaliveMaxTokens
		}
		if _, exists := payload["reasoning"]; exists {
			payload["reasoning"] = map[string]any{"effort": "low"}
			payload["text"] = map[string]any{"verbosity": "low"}
		}
	case "chat":
		payload["max_completion_tokens"] = keepaliveMaxTokens
	}
}

func observeKeepaliveResponse(c *gin.Context, resp *http.Response) {
	state := keepaliveState(c)
	if state == nil || resp == nil || state.observed == resp {
		return
	}
	state.observed = resp
	state.status = resp.StatusCode
	if delay := retryAfter(resp.Header, time.Now()); delay > state.retryAfter {
		state.retryAfter = delay
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		if reset := calculateOpenAI429ResetTime(resp.Header); reset != nil {
			if delay := time.Until(*reset); delay > state.retryAfter {
				state.retryAfter = delay
			}
		}
		if window := selectAnthropicExhaustedWindow(resp.Header, time.Now()); window != nil {
			if delay := time.Until(window.resetAt); delay > state.retryAfter {
				state.retryAfter = delay
			}
		}
		if reset, ok := parseAnthropicAggregateReset(resp.Header, time.Now()); ok {
			if delay := time.Until(reset); delay > state.retryAfter {
				state.retryAfter = delay
			}
		}
		if resp.Body != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(strings.NewReader(string(body)))
			for _, reset := range []*int64{parseOpenAIRateLimitResetTime(body), ParseGeminiRateLimitResetTime(body)} {
				if reset != nil {
					if delay := time.Until(time.Unix(*reset, 0)); delay > state.retryAfter {
						state.retryAfter = delay
					}
				}
			}
			if resp.Header.Get("Retry-After") == "" {
				if match := accountTestQueueRetryAfterPattern.FindSubmatch(body); len(match) == 2 {
					if delay := retryAfter(http.Header{"Retry-After": []string{string(match[1])}}, time.Now()); delay > state.retryAfter {
						state.retryAfter = delay
					}
				}
			}
		}
	}
}

// RunKeepaliveBackground performs exactly one short probe. Retries belong to the
// persistent scheduler so errors cannot occupy a worker indefinitely.
func (s *AccountTestService) RunKeepaliveBackground(ctx context.Context, accountID int64, modelID string) (*ScheduledTestResult, error) {
	startedAt := time.Now()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
	state := &accountKeepaliveState{}
	c.Set(accountKeepaliveContextKey, state)
	testErr := s.TestAccountConnection(c, accountID, modelID, keepalivePrompt, AccountTestModeDefault)
	var text strings.Builder
	errMsg := ""
	completed := false
	for _, line := range strings.Split(w.Body.String(), "\n") {
		line = strings.TrimSpace(line)
		if !sseDataPrefix.MatchString(line) {
			continue
		}
		var event TestEvent
		if json.Unmarshal([]byte(sseDataPrefix.ReplaceAllString(line, "")), &event) != nil {
			continue
		}
		switch event.Type {
		case "content":
			_, _ = text.WriteString(event.Text)
		case "error":
			errMsg = event.Error
		case "test_complete":
			completed = event.Success
		}
	}
	responseText := text.String()
	if errMsg == "" && testErr != nil {
		errMsg = testErr.Error()
	}
	if errMsg == "" && ctx.Err() != nil {
		errMsg = ctx.Err().Error()
	}
	if errMsg == "" && (!completed || strings.TrimSpace(responseText) == "") {
		errMsg = "keepalive did not receive a completed text response"
	}
	status := "success"
	if errMsg != "" {
		status = "failed"
	}
	finishedAt := time.Now()
	return &ScheduledTestResult{
		Status: status, ResponseText: responseText, ErrorMessage: errMsg,
		StartedAt: startedAt, FinishedAt: finishedAt,
		LatencyMs:  finishedAt.Sub(startedAt).Milliseconds(),
		HTTPStatus: state.status, RetryAfter: state.retryAfter,
	}, nil
}
