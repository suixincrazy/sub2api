//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type retry429Upstream struct {
	t         *testing.T
	repo      *anthropicWindowLimitRepo
	accountID int64
	calls     int
	respond   func(int, *http.Request) *http.Response
	check     func()
}

func (u *retry429Upstream) Do(req *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	u.calls++
	if u.check != nil {
		u.check()
	}
	require.Equal(u.t, u.accountID, id, "429 recovery must stay on the selected account")
	require.Zero(u.t, u.repo.rateLimitCalls, "account frozen before retries completed")
	require.Zero(u.t, u.repo.tempUnschedCalls, "temporary freeze before retries completed")
	require.Zero(u.t, u.repo.modelRateLimitCalls, "model frozen before retries completed")
	if req.Body != nil {
		_ = req.Body.Close()
	}
	if u.respond != nil {
		return u.respond(u.calls, req), nil
	}
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"rate limit reached"}}`)),
	}, nil
}

func TestForward429AllProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, platform := range []string{PlatformOpenAI, PlatformAnthropic} {
		for _, kind := range []string{AccountTypeAPIKey, AccountTypeOAuth, AccountTypeSetupToken} {
			for _, endpoint := range []string{"messages", "responses", "chat/completions"} {
				t.Run(platform+"/"+kind+"/"+endpoint, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						repo := &anthropicWindowLimitRepo{}
						account := &Account{ID: 902, Name: "retry-test", Platform: platform, Type: kind, Status: StatusActive, Schedulable: true, Concurrency: 1,
							Credentials: map[string]any{"api_key": "test", "access_token": "test", "chatgpt_account_id": "test", "base_url": "https://api.example.invalid", "pool_mode": true, "pool_mode_retry_count": float64(0)},
							Extra:       map[string]any{"anthropic_passthrough": true},
						}
						cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
						rateLimit := NewRateLimitService(repo, nil, cfg, nil, nil)
						upstream := &retry429Upstream{t: t, repo: repo, accountID: account.ID}
						c, rec := newTransportFailoverTestContext(t)
						c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+endpoint, nil)
						model := "gpt-5.4"
						if platform == PlatformAnthropic {
							model = "claude-sonnet-4-5"
						}
						body := []byte(`{"model":"` + model + `","instructions":"test","input":"hello","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
						var err error
						if platform == PlatformOpenAI {
							svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, rateLimitService: rateLimit}
							rateLimit.SetAccountRuntimeBlocker(svc)
							upstream.check = func() { require.False(t, svc.isOpenAIAccountRuntimeBlocked(account)) }
							switch endpoint {
							case "messages":
								_, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
							case "responses":
								_, err = svc.Forward(context.Background(), c, account, body)
							default:
								_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
							}
						} else {
							svc := &GatewayService{cfg: cfg, httpUpstream: upstream, rateLimitService: rateLimit}
							parsed := &ParsedRequest{Model: model, Body: NewRequestBodyRef(body)}
							switch endpoint {
							case "messages":
								_, err = svc.Forward(context.Background(), c, account, parsed)
							case "responses":
								_, err = svc.ForwardAsResponses(context.Background(), c, account, body, parsed)
							default:
								_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, parsed)
							}
						}
						var failover *UpstreamFailoverError
						require.ErrorAs(t, err, &failover)
						require.Equal(t, 7, upstream.calls)
						require.Equal(t, 1, repo.rateLimitCalls)
						require.False(t, failover.RetryableOnSameAccount)
						require.Empty(t, rec.Body.String())
					})
				})
			}
		}
	}
}

func TestForward429SuccessAndCancellationDoNotFreeze(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, platform := range []string{PlatformOpenAI, PlatformAnthropic} {
		for _, cancelRequest := range []bool{false, true} {
			t.Run(platform+"/cancel="+map[bool]string{true: "true", false: "false"}[cancelRequest], func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					repo := &anthropicWindowLimitRepo{}
					account := &Account{ID: 903, Name: "retry-test", Platform: platform, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test", "base_url": "https://api.example.invalid"}}
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					upstream := &retry429Upstream{t: t, repo: repo, accountID: account.ID}
					upstream.respond = func(attempt int, _ *http.Request) *http.Response {
						if attempt <= 2 {
							if cancelRequest {
								cancel()
							}
							return &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))}
						}
						body := `{"id":"msg_test","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
						if platform == PlatformOpenAI {
							body = `{"id":"resp_test","object":"response","status":"completed","model":"gpt-5.4","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
						}
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
					}
					cfg := &config.Config{}
					rl := NewRateLimitService(repo, nil, cfg, nil, nil)
					c, _ := newTransportFailoverTestContext(t)
					c.Request = c.Request.WithContext(ctx)
					var err error
					if platform == PlatformOpenAI {
						svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, rateLimitService: rl}
						rl.SetAccountRuntimeBlocker(svc)
						_, err = svc.Forward(ctx, c, account, []byte(`{"model":"gpt-5.4","input":"hello","stream":false}`))
						require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
					} else {
						svc := &GatewayService{cfg: cfg, httpUpstream: upstream, rateLimitService: rl}
						_, err = svc.Forward(ctx, c, account, &ParsedRequest{Model: "claude-sonnet-4-5", Body: NewRequestBodyRef([]byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}],"max_tokens":16}`))})
					}
					if cancelRequest {
						require.ErrorIs(t, err, context.Canceled)
						require.Equal(t, 1, upstream.calls)
					} else {
						require.NoError(t, err)
						require.Equal(t, 3, upstream.calls)
					}
					require.Zero(t, repo.rateLimitCalls)
					require.Zero(t, repo.modelRateLimitCalls)
					require.Zero(t, repo.tempUnschedCalls)
				})
			})
		}
	}
}

func (u *retry429Upstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}

func TestForward429RetriesSixTimesBeforeFreeze(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, platform := range []string{PlatformOpenAI, PlatformAnthropic} {
		t.Run(platform, func(t *testing.T) {
			repo := &anthropicWindowLimitRepo{}
			account := &Account{
				ID: 901, Name: "retry-429", Platform: platform, Type: AccountTypeAPIKey,
				Status: StatusActive, Schedulable: true, Concurrency: 1,
				Credentials: map[string]any{"api_key": "test-key", "base_url": "https://api.example.invalid"},
			}
			upstream := &retry429Upstream{t: t, repo: repo, accountID: account.ID}
			cfg := &config.Config{}
			rateLimit := NewRateLimitService(repo, nil, cfg, nil, nil)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			var err error
			if platform == PlatformOpenAI {
				svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, rateLimitService: rateLimit}
				rateLimit.SetAccountRuntimeBlocker(svc)
				_, err = svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.4","input":"hello","stream":false}`))
				require.True(t, svc.isOpenAIAccountRuntimeBlocked(account), "exhausted 429 must block runtime scheduling")
			} else {
				svc := &GatewayService{cfg: cfg, httpUpstream: upstream, rateLimitService: rateLimit}
				parsed := &ParsedRequest{Model: "claude-sonnet-4-5", Body: NewRequestBodyRef([]byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`))}
				_, err = svc.Forward(context.Background(), c, account, parsed)
			}
			var failoverErr *UpstreamFailoverError
			require.ErrorAs(t, err, &failoverErr)
			require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
			require.Equal(t, 7, upstream.calls, "initial request plus six retries")
			require.Equal(t, 1, repo.rateLimitCalls, "freeze exactly once after retry exhaustion")
			require.False(t, failoverErr.RetryableOnSameAccount, "handler must not multiply the exhausted retry budget")
			require.Empty(t, rec.Body.String(), "failover must remain possible")
		})
	}
}

func TestForward429QuotaAndModelPoliciesAreDeferred(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, scenario := range []string{"openai_quota", "anthropic_quota", "openai_temp_model", "anthropic_fable", "openai_spark", "openai_sse"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				repo := &anthropicWindowLimitRepo{}
				account := &Account{ID: 906, Name: scenario, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "test", "chatgpt_account_id": "test", "api_key": "test"}}
				model := "gpt-5.4"
				headers := http.Header{}
				expectedModelFreeze := false
				switch scenario {
				case "openai_quota", "openai_spark", "openai_sse":
					headers.Set("X-Codex-Primary-Used-Percent", "100")
					headers.Set("X-Codex-Primary-Reset-After-Seconds", "7200")
					headers.Set("X-Codex-Primary-Window-Minutes", "300")
					if scenario == "openai_spark" {
						model = "gpt-5.3-codex-spark"
						expectedModelFreeze = true
					}
				case "anthropic_quota":
					account.Platform = PlatformAnthropic
					model = "claude-sonnet-4-5"
					headers.Set("anthropic-ratelimit-unified-5h-utilization", "1.02")
					headers.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10))
				case "anthropic_fable":
					account.Platform = PlatformAnthropic
					model = "claude-fable-5"
					headers = fable429Headers(time.Now().Add(time.Hour), time.Now().Add(24*time.Hour))
					expectedModelFreeze = true
				case "openai_temp_model":
					account.Type = AccountTypeAPIKey
					account.Credentials["temp_unschedulable_enabled"] = true
					account.Credentials["temp_unschedulable_rules"] = []any{map[string]any{"error_code": float64(429), "keywords": []any{"rate limit"}, "duration_minutes": float64(1)}}
					expectedModelFreeze = true
				}
				cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
				rl := NewRateLimitService(repo, nil, cfg, nil, nil)
				upstream := &retry429Upstream{t: t, repo: repo, accountID: account.ID, respond: func(_ int, _ *http.Request) *http.Response {
					status, body := 429, `{"error":{"type":"rate_limit_error","message":"rate limit reached"}}`
					if scenario == "openai_sse" {
						status = 200
						headers.Set("Content-Type", "text/event-stream")
						body = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"pending\"}}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"rate limit reached\"}}}\n\n"
					}
					return &http.Response{StatusCode: status, Header: headers.Clone(), Body: io.NopCloser(strings.NewReader(body))}
				}}
				c, rec := newTransportFailoverTestContext(t)
				body := []byte(`{"model":"` + model + `","instructions":"test","input":"hello","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
				var err error
				if account.Platform == PlatformOpenAI {
					svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, rateLimitService: rl}
					rl.SetAccountRuntimeBlocker(svc)
					upstream.check = func() { require.False(t, svc.isOpenAIAccountRuntimeBlocked(account)) }
					_, err = svc.Forward(context.Background(), c, account, body)
				} else {
					svc := &GatewayService{cfg: cfg, httpUpstream: upstream, rateLimitService: rl}
					_, err = svc.Forward(context.Background(), c, account, &ParsedRequest{Model: model, Body: NewRequestBodyRef(body)})
				}
				var failure *UpstreamFailoverError
				require.ErrorAs(t, err, &failure)
				require.Equal(t, 7, upstream.calls)
				if expectedModelFreeze {
					require.Equal(t, 1, repo.modelRateLimitCalls)
					require.Zero(t, repo.rateLimitCalls)
				} else {
					require.Equal(t, 1, repo.rateLimitCalls)
					require.Zero(t, repo.modelRateLimitCalls)
				}
				require.Zero(t, repo.tempUnschedCalls)
				require.Empty(t, rec.Body.String())
			})
		})
	}
}
