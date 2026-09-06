//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Replay observed lengths and usage without storing private conversation content.
func slowTurnEvents(initialContent bool) []string {
	thinking, prose := strings.Repeat("t", 6599), strings.Repeat("p", 583)
	events := []string{`data: {"type":"message_start","message":{"usage":{"input_tokens":16626,"output_tokens":1}}}`}
	if initialContent {
		events = append(events, fmt.Sprintf(`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":%q}}`, thinking))
	} else {
		events = append(events,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%q}}`, thinking))
	}
	events = append(events, `data: {"type":"content_block_stop","index":0}`)
	if initialContent {
		events = append(events, fmt.Sprintf(`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":%q}}`, prose))
	} else {
		events = append(events,
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			fmt.Sprintf(`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":%q}}`, prose))
	}
	return append(events,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1697}}`,
		`data: {"type":"message_stop"}`)
}

func TestAnthropicSlowTurn_FirstContentRetrySurvivesElapsedBudget(t *testing.T) {
	for _, initialContent := range []bool{false, true} {
		t.Run(fmt.Sprintf("initial_content=%t", initialContent), func(t *testing.T) {
			const session = "slow-first-content"
			cache := newShortTurnStreakCache(session, 11)
			svc, _ := newHoldbackTestGatewayService(t, cache, 15000)
			svc.cfg.Gateway.AnthropicHoldbackMaxHoldMs = 10000
			svc.cfg.Gateway.AnthropicHoldbackDeadAirBudgetMs = 25000
			svc.cfg.Gateway.AnthropicHoldbackLongThinkingHoldMs = 360000
			svc.cfg.Gateway.AnthropicHoldbackDiscardBudgetMs = 120000
			c, rec := newRefusalTestContext(t)
			noteAnthropicDiscardBudgetStart(c, time.Now().Add(-156*time.Second))
			ctx := WithStickySessionScope(context.Background(), 8, session, false)
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
				Body: sseBody(strings.Join(slowTurnEvents(initialContent), "\n\n") + "\n\n")}
			result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(ctx, resp, c,
				&Account{ID: 11, Name: "test-upstream", Platform: PlatformAnthropic},
				time.Now().Add(-136419*time.Millisecond), "claude-opus-5")
			var failover *UpstreamFailoverError
			require.True(t, errors.As(err, &failover), "HTTP failovers must not consume the first content retry")
			require.Nil(t, result, "discarded output must not be billed as a delivered turn")
			require.True(t, failover.ShouldRetryNextAccount())
			require.Equal(t, 1, anthropicHoldbackDiscardsUsed(c))
			require.Empty(t, rec.Body.String())
			require.False(t, c.Writer.Written())

			result, err = svc.handleStreamingResponseAnthropicAPIKeyPassthrough(ctx,
				shortTurnSSE("healthy-retry", 600, true), c,
				&Account{ID: 12, Platform: PlatformAnthropic}, time.Now(), "claude-opus-5")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Contains(t, rec.Body.String(), "healthy-retry")
			require.Contains(t, rec.Body.String(), `"type":"message_stop"`)
			require.NotContains(t, rec.Body.String(), strings.Repeat("p", 20))
			require.NotContains(t, rec.Body.String(), strings.Repeat("t", 20))
			require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"message_start"`))
		})
	}
}

func TestAnthropicSlowTurn_InitialAndDeltaContentCountsMatchNonStream(t *testing.T) {
	var o anthropicHoldbackObserver
	frames := []string{
		`{"type":"content_block_start","content_block":{"type":"thinking","thinking":"abc"}}`,
		`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"def"}}`,
		`{"type":"content_block_start","content_block":{"type":"redacted_thinking","data":"AAAA"}}`,
		`{"type":"content_block_delta","delta":{"type":"signature_delta","signature":"not-content"}}`,
		`{"type":"content_block_start","content_block":{"type":"text","text":"\u4e2d\ud83d\ude00"}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"xyz"}}`,
	}
	for _, frame := range frames {
		o.observe(gjson.Parse(frame), true, time.Now())
	}
	shape := anthropicNonStreamTurnShapeFromBody([]byte(`{"content":[
		{"type":"thinking","thinking":"abcdef"},
		{"type":"redacted_thinking","data":"AAAA"},
		{"type":"text","text":"\u4e2d\ud83d\ude00xyz"}]}`))
	require.Equal(t, 5, o.proseRunes)
	require.Equal(t, 10, o.thinkingRunes)
	require.Equal(t, shape.proseRunes, o.proseRunes)
	require.Equal(t, shape.thinkingRunes, o.thinkingRunes)
}

func TestAnthropicSlowTurn_HealthyInitialContentIsNotEmpty(t *testing.T) {
	for _, outputTokens := range []int{0, 900} {
		t.Run(fmt.Sprintf("output_tokens=%d", outputTokens), func(t *testing.T) {
			svc, repo := newHoldbackTestGatewayService(t, nil, 15000)
			c, rec := newRefusalTestContext(t)
			text := strings.Repeat("h", anthropicShortTurnProseRuneLimit+1)
			body := strings.Join([]string{
				`data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}`,
				fmt.Sprintf(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":%q}}`, text),
				`data: {"type":"content_block_stop","index":0}`,
				fmt.Sprintf(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":%d}}`, outputTokens),
				`data: {"type":"message_stop"}`,
			}, "\n\n") + "\n\n"
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: sseBody(body)}
			result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c,
				&Account{ID: 11, Platform: PlatformAnthropic}, time.Now(), "claude-opus-5")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, body, rec.Body.String())
			require.Zero(t, anthropicHoldbackDiscardsUsed(c))
			require.Zero(t, repo.tempCalls, "initial content must not trigger empty-stream penalties")
		})
	}
}

func TestAnthropicSlowTurn_NonStreamFirstContentRetrySurvivesElapsedBudget(t *testing.T) {
	svc, _ := newHoldbackTestGatewayService(t, nil, 15000)
	svc.cfg.Gateway.AnthropicHoldbackDiscardBudgetMs = 120000
	c, rec := newRefusalTestContext(t)
	noteAnthropicDiscardBudgetStart(c, time.Now().Add(-156*time.Second))
	body := []byte(fmt.Sprintf(`{"stop_reason":"end_turn","content":[
		{"type":"thinking","thinking":%q},{"type":"text","text":%q}],"usage":{"output_tokens":1697}}`,
		strings.Repeat("t", 6599), strings.Repeat("p", 583)))
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
	account := &Account{ID: 11, Platform: PlatformAnthropic}
	err := svc.discardNonStreamTurnIfSuspicious(context.Background(), c, resp, account, "claude-opus-5", body)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.True(t, failover.ShouldRetryNextAccount())
	require.Equal(t, 1, anthropicHoldbackDiscardsUsed(c))
	require.Empty(t, rec.Body.String())
	require.False(t, c.Writer.Written())
	err = svc.discardNonStreamTurnIfSuspicious(context.Background(), c, resp, account, "claude-opus-5", body)
	require.NoError(t, err, "elapsed budget must still cap repeated content retries")
	require.Equal(t, 1, anthropicHoldbackDiscardsUsed(c))
}

func TestAnthropicSlowTurn_ElapsedBudgetStillCapsRepeatedContentRetries(t *testing.T) {
	svc, _ := newHoldbackTestGatewayService(t, nil, 15000)
	svc.cfg.Gateway.AnthropicHoldbackDiscardBudgetMs = 120000
	c, rec := newRefusalTestContext(t)
	noteAnthropicDiscardBudgetStart(c, time.Now().Add(-156*time.Second))
	noteAnthropicHoldbackDiscard(c)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
		Body: sseBody(strings.Join(slowTurnEvents(false), "\n\n") + "\n\n")}
	_, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c,
		&Account{ID: 11, Platform: PlatformAnthropic}, time.Now(), "claude-opus-5")
	require.NoError(t, err)
	require.Equal(t, 1, anthropicHoldbackDiscardsUsed(c))
	require.Contains(t, rec.Body.String(), `"type":"message_stop"`)
}

func TestAnthropicSlowTurn_FirstTokenMeasuresArrivalNotHoldbackRelease(t *testing.T) {
	svc, _ := newHoldbackTestGatewayService(t, nil, 15000)
	c, _ := newRefusalTestContext(t)
	c.Set(anthropicHoldbackDiscardsKey, anthropicEmptyAnswerDiscardBudget)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
		Body: &pacedBody{events: slowTurnEvents(false), gap: 25 * time.Millisecond}}
	start := time.Now()
	result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c,
		&Account{ID: 11, Platform: PlatformAnthropic}, start, "claude-opus-5")
	require.NoError(t, err)
	require.NotNil(t, result.firstTokenMs)
	require.Greater(t, time.Since(start).Milliseconds()-int64(*result.firstTokenMs), int64(150),
		"the first-token measurement must exclude time spent holding later frames")
}

func TestAnthropicSlowTurn_HTTPFailoversThenContentRetry(t *testing.T) {
	svc, _ := newHoldbackTestGatewayService(t, nil, 15000)
	svc.cfg.Gateway.AnthropicHoldbackDiscardBudgetMs = 120000
	svc.rateLimitService = nil
	upstream := &anthropicHTTPUpstreamRecorder{}
	svc.httpUpstream = upstream
	c, rec := newRefusalTestContext(t)
	start := time.Now().Add(-156 * time.Second)
	noteAnthropicDiscardBudgetStart(c, start)
	requestBody := []byte(`{"model":"claude-opus-5","stream":true,"messages":[{"role":"user","content":"Continue the task."}]}`)
	responses := []*http.Response{
		{StatusCode: 502, Header: http.Header{}, Body: sseBody(`{"error":{"message":"origin unavailable"}}`)},
		{StatusCode: 503, Header: http.Header{}, Body: sseBody(`{"error":{"message":"no available accounts"}}`)},
		{StatusCode: 200, Header: http.Header{}, Body: sseBody(strings.Join(slowTurnEvents(false), "\n\n") + "\n\n")},
		shortTurnSSE("healthy-after-http-failovers", 600, true),
	}
	for i, resp := range responses {
		upstream.resp = resp
		account := newAnthropicAPIKeyAccountForTest()
		account.ID += int64(i)
		result, err := svc.forwardAnthropicAPIKeyPassthrough(context.Background(), c, account,
			requestBody, "claude-opus-5", "claude-opus-5", true, time.Now())
		require.JSONEq(t, string(requestBody), string(upstream.lastBody), "failover must preserve the request")
		if i < len(responses)-1 {
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.True(t, failover.ShouldRetryNextAccount())
			require.Nil(t, result)
			require.Empty(t, rec.Body.String())
			require.False(t, c.Writer.Written())
			continue
		}
		require.NoError(t, err)
		require.NotNil(t, result)
	}
	require.Equal(t, 1, anthropicHoldbackDiscardsUsed(c))
	require.Equal(t, start, c.MustGet(anthropicDiscardBudgetStartKey))
	require.Contains(t, rec.Body.String(), "healthy-after-http-failovers")
	require.Contains(t, rec.Body.String(), `"type":"message_stop"`)
	require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"message_start"`))
	require.NotContains(t, rec.Body.String(), strings.Repeat("p", 20))
}
