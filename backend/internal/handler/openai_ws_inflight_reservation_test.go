package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type wsInflightBillingCache struct {
	*handlerInflightCache
	reserved chan float64
	pricing  chan time.Time
}

func (c *wsInflightBillingCache) ReserveInflightBalance(ctx context.Context, userID int64, id string, amount, balance float64, ttl time.Duration) (bool, float64, error) {
	allowed, total, err := c.handlerInflightCache.ReserveInflightBalance(ctx, userID, id, amount, balance, ttl)
	if allowed && err == nil {
		c.reserved <- amount
		c.pricing <- service.OpenAIPricingAtFromContext(ctx)
	}
	return allowed, total, err
}

func (c *wsInflightBillingCache) DeductUserBalance(_ context.Context, _ int64, amount float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.balance -= amount
	return nil
}

type wsInflightBillingRepo struct {
	service.UsageBillingRepository
	started chan *service.UsageBillingCommand
	permit  chan struct{}
}

func (r *wsInflightBillingRepo) Apply(ctx context.Context, cmd *service.UsageBillingCommand) (*service.UsageBillingApplyResult, error) {
	copy := *cmd
	r.started <- &copy
	select {
	case <-r.permit:
		return &service.UsageBillingApplyResult{Applied: true}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type wsInflightRPMCache struct {
	service.UserRPMCache
	calls atomic.Int64
}

func (c *wsInflightRPMCache) IncrementUserRPM(context.Context, int64) (int, error) {
	return int(c.calls.Add(1)), nil
}

type wsInflightHarness struct {
	handler            *OpenAIGatewayHandler
	client             *coderws.Conn
	cache              *wsInflightBillingCache
	billing            *wsInflightBillingRepo
	rpm                *wsInflightRPMCache
	apiKey             *service.APIKey
	requests           chan []byte
	responses          chan []byte
	usage              chan *service.UsageLog
	handlerDone        chan struct{}
	disconnectUpstream context.CancelFunc
}

func newWSInflightHarness(t *testing.T, mode string, extraAccounts ...service.Account) *wsInflightHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	harness := &wsInflightHarness{
		cache:       &wsInflightBillingCache{handlerInflightCache: newHandlerInflightCache(1), reserved: make(chan float64, 8), pricing: make(chan time.Time, 8)},
		billing:     &wsInflightBillingRepo{started: make(chan *service.UsageBillingCommand, 8), permit: make(chan struct{}, 8)},
		rpm:         &wsInflightRPMCache{},
		requests:    make(chan []byte, 8),
		responses:   make(chan []byte, 8),
		usage:       make(chan *service.UsageLog, 8),
		handlerDone: make(chan struct{}),
	}
	upstreamCtx, stopUpstream := context.WithCancel(context.Background())
	harness.disconnectUpstream = stopUpstream
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		for {
			_, payload, err := conn.Read(upstreamCtx)
			if err != nil {
				return
			}
			harness.requests <- payload
			select {
			case response := <-harness.responses:
				if response == nil {
					return
				}
				if err := conn.Write(upstreamCtx, coderws.MessageText, response); err != nil {
					return
				}
			case <-upstreamCtx.Done():
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(stopUpstream)

	groupID := int64(4201)
	account := service.Account{
		ID: 9901, Name: "ws-inflight", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: 2,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": upstream.URL},
		Extra: map[string]any{
			"openai_apikey_responses_websockets_v2_enabled": true,
			"openai_apikey_responses_websockets_v2_mode":    mode,
		},
	}
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 10
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	cfg.Billing.InflightReservation = config.InflightReservationConfig{Enabled: true, TTLSeconds: 60}
	billingCache := service.NewBillingCacheService(harness.cache, nil, nil, nil, harness.rpm, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	gateway := service.NewOpenAIGatewayService(
		&openAIWSFailoverHandlerAccountRepoStub{accounts: append(extraAccounts, account)},
		&openAIWSUsageHandlerUsageLogRepoStub{created: harness.usage}, harness.billing,
		nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, billingCache,
		&compositeWSHTTPUpstream{}, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil,
	)
	t.Cleanup(gateway.CloseOpenAIWSPool)
	pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{
		WorkerCount: 1, QueueSize: 8, TaskTimeout: 5 * time.Second, OverflowPolicy: config.UsageRecordOverflowPolicySync,
	})
	t.Cleanup(pool.Stop)
	t.Cleanup(func() { close(harness.billing.permit) })
	concurrency := &concurrencyCacheMock{
		acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
	}
	harness.handler = &OpenAIGatewayHandler{
		cfg: cfg, gatewayService: gateway, billingCacheService: billingCache,
		apiKeyService: &service.APIKeyService{}, usageRecordWorkerPool: pool,
		concurrencyHelper:  NewConcurrencyHelper(service.NewConcurrencyService(concurrency), SSEPingFormatNone, time.Second),
		maxAccountSwitches: 1,
	}
	harness.apiKey = &service.APIKey{
		ID: 1801, UserID: 1701, GroupID: &groupID,
		User:  &service.User{ID: 1701, Status: service.StatusActive, Balance: 1, RPMLimit: 100},
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, RateMultiplier: 1},
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), harness.apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: harness.apiKey.User.ID, Concurrency: 1})
		c.Next()
	})
	router.GET("/openai/v1/responses", func(c *gin.Context) {
		defer close(harness.handlerDone)
		harness.handler.ResponsesWebSocket(c)
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/openai/v1/responses", &coderws.DialOptions{CompressionMode: coderws.CompressionContextTakeover})
	require.NoError(t, err)
	harness.client = client
	t.Cleanup(func() {
		_ = client.CloseNow()
		stopUpstream()
		select {
		case <-harness.handlerDone:
		case <-time.After(3 * time.Second):
			t.Error("websocket handler did not stop")
		}
	})
	return harness
}

func (h *wsInflightHarness) send(t *testing.T, payload string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, h.client.Write(ctx, coderws.MessageText, []byte(payload)))
}

func wsInflightReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for websocket billing event")
		var zero T
		return zero
	}
}

func (h *wsInflightHarness) complete(t *testing.T, turn int, model string) *service.UsageBillingCommand {
	t.Helper()
	h.responses <- []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_inflight_%d","model":%q,"usage":{"input_tokens":2,"output_tokens":1}}}`, turn, model))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		_, payload, err := h.client.Read(ctx)
		require.NoError(t, err)
		if gjson.GetBytes(payload, "type").String() == "response.completed" {
			break
		}
	}
	return wsInflightReceive(t, h.billing.started)
}

func (h *wsInflightHarness) settle(t *testing.T) {
	t.Helper()
	h.billing.permit <- struct{}{}
	wsInflightReceive(t, h.usage)
	require.Eventually(t, func() bool { return h.cache.count() == 0 }, time.Second, time.Millisecond, "idle connection must not retain a settled turn reservation")
}

func TestOpenAIWSInflight_TurnCompletionReleasesWhileConnected(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeCtxPool} {
		t.Run(mode, func(t *testing.T) {
			h := newWSInflightHarness(t, mode)
			h.send(t, `{"type":"response.create","model":"gpt-5.1","input":"one","max_output_tokens":128}`)
			wsInflightReceive(t, h.requests)
			require.Positive(t, wsInflightReceive(t, h.cache.reserved))
			require.Equal(t, 1, h.cache.count(), "first frame must reserve before reaching upstream")
			cmd := h.complete(t, 1, "gpt-5.1")
			require.Positive(t, cmd.BalanceCost)
			require.Equal(t, 1, h.cache.count(), "completed response must retain its reservation while billing is pending")
			h.settle(t)
			other, err := h.handler.billingCacheService.ReserveInflight(context.Background(), h.apiKey.User, h.apiKey.Group, nil, 0.999)
			require.NoError(t, err, "settled idle connection must not block another large request")
			other.HandlerDone()
			select {
			case <-h.handlerDone:
				t.Fatal("reservation must be released without closing the client connection")
			default:
			}
			require.EqualValues(t, 1, h.rpm.calls.Load(), "first frame must consume RPM once")
		})
	}
}

func TestOpenAIWSInflight_SubsequentTurnsReestimateAndCheckBalance(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeCtxPool} {
		t.Run(mode, func(t *testing.T) {
			h := newWSInflightHarness(t, mode)
			h.send(t, `{"type":"response.create","model":"gpt-5.1","input":"one","max_output_tokens":128}`)
			wsInflightReceive(t, h.requests)
			firstAmount := wsInflightReceive(t, h.cache.reserved)
			firstPricingAt := wsInflightReceive(t, h.cache.pricing)
			require.False(t, firstPricingAt.IsZero())
			h.complete(t, 1, "gpt-5.1")
			h.settle(t)
			require.Eventually(t, func() bool { return time.Now().After(firstPricingAt) }, time.Second, time.Millisecond,
				"advance the clock before checking that the next turn freezes a new time")

			h.send(t, `{"type":"response.create","model":"gpt-5.2","input":"two","max_output_tokens":4096}`)
			wsInflightReceive(t, h.requests)
			require.Greater(t, wsInflightReceive(t, h.cache.reserved), firstAmount*10, "new turn must use its own model and output budget")
			require.True(t, wsInflightReceive(t, h.cache.pricing).After(firstPricingAt), "new turn must freeze a new pricing time before estimating")
			h.complete(t, 2, "gpt-5.2")
			h.settle(t)

			h.send(t, `{"type":"response.create","input":"three","max_output_tokens":512}`)
			wsInflightReceive(t, h.requests)
			require.Positive(t, wsInflightReceive(t, h.cache.reserved), "omitted model must use the current resolved model")
			h.complete(t, 3, "gpt-5.2")
			h.settle(t)
			require.EqualValues(t, 3, h.rpm.calls.Load(), "each accepted turn must consume RPM once")

			h.cache.mu.Lock()
			h.cache.balance = 0
			h.cache.mu.Unlock()
			h.send(t, `{"type":"response.create","model":"gpt-5.2","input":"four"}`)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, _, err := h.client.Read(ctx)
			require.Equal(t, coderws.StatusPolicyViolation, coderws.CloseStatus(err))
			wsInflightReceive(t, h.handlerDone)
			require.Empty(t, h.requests, "balance rejection must happen before upstream write")
			require.Zero(t, h.cache.count())
			require.EqualValues(t, 3, h.rpm.calls.Load(), "balance-rejected turn must not consume RPM")
		})
	}
}

func TestOpenAIWSInflight_InternalRetryKeepsTurnReservation(t *testing.T) {
	h := newWSInflightHarness(t, service.OpenAIWSIngressModeCtxPool)
	h.send(t, `{"type":"response.create","model":"gpt-5.1","input":"one","max_output_tokens":128}`)
	wsInflightReceive(t, h.requests)
	wsInflightReceive(t, h.cache.reserved)
	h.complete(t, 1, "gpt-5.1")
	h.settle(t)

	h.send(t, `{"type":"response.create","model":"gpt-5.1","input":"two","max_output_tokens":128,"truncation":"auto"}`)
	first := wsInflightReceive(t, h.requests)
	require.True(t, gjson.GetBytes(first, "truncation").Exists())
	wsInflightReceive(t, h.cache.reserved)
	h.cache.mu.Lock()
	var reservationID string
	for id := range h.cache.res {
		reservationID = id
	}
	h.cache.mu.Unlock()
	h.responses <- []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"invalid_parameter","param":"truncation","message":"Unsupported parameter: truncation"}}`)
	retried := wsInflightReceive(t, h.requests)
	require.False(t, gjson.GetBytes(retried, "truncation").Exists())
	require.Empty(t, h.cache.reserved, "retry must reuse the reservation")
	h.cache.mu.Lock()
	_, sameReservation := h.cache.res[reservationID]
	h.cache.mu.Unlock()
	require.True(t, sameReservation)
	require.EqualValues(t, 2, h.rpm.calls.Load(), "retry must not consume another RPM")
	h.complete(t, 2, "gpt-5.1")
	h.settle(t)
}

func TestOpenAIWSInflight_DisconnectReleasesUnfinishedTurn(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeCtxPool} {
		t.Run(mode, func(t *testing.T) {
			h := newWSInflightHarness(t, mode)
			h.send(t, `{"type":"response.create","model":"gpt-5.1","input":"one"}`)
			wsInflightReceive(t, h.requests)
			wsInflightReceive(t, h.cache.reserved)
			require.Equal(t, 1, h.cache.count())
			require.NoError(t, h.client.CloseNow())
			h.disconnectUpstream()
			wsInflightReceive(t, h.handlerDone)
			require.Zero(t, h.cache.count(), "disconnect without a bill must release its reservation")
			require.Empty(t, h.billing.started)
		})
	}
}

func TestOpenAIWSInflight_CyberErrorBillingRetainsReservation(t *testing.T) {
	h := newWSInflightHarness(t, service.OpenAIWSIngressModeCtxPool)
	h.send(t, `{"type":"response.create","model":"gpt-5.1","input":"one"}`)
	wsInflightReceive(t, h.requests)
	wsInflightReceive(t, h.cache.reserved)
	h.responses <- []byte(`{"type":"error","error":{"code":"cyber_policy","message":"blocked"},"response":{"id":"resp_cyber_inflight","usage":{"input_tokens":11,"output_tokens":3}}}`)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, payload, err := h.client.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, "cyber_policy", gjson.GetBytes(payload, "error.code").String())
	h.disconnectUpstream()
	_, _, err = h.client.Read(ctx)
	require.Error(t, err)
	wsInflightReceive(t, h.handlerDone)
	cmd := wsInflightReceive(t, h.billing.started)
	require.Equal(t, 11, cmd.InputTokens)
	require.Equal(t, 3, cmd.OutputTokens)
	require.Positive(t, cmd.BalanceCost)
	require.Equal(t, 1, h.cache.count(), "cyber goroutine must acquire before turn and connection cleanup")
	h.settle(t)
	require.Empty(t, h.billing.started, "cyber failure must be billed once")
}

func TestOpenAIWSInflight_FailoverRebuildsFirstTurnReservation(t *testing.T) {
	failedRequests := make(chan []byte, 8)
	upstreamCtx, stop := context.WithCancel(context.Background())
	defer stop()
	failedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		for {
			_, payload, err := conn.Read(upstreamCtx)
			if err != nil {
				return
			}
			failedRequests <- payload
			if err := conn.Write(upstreamCtx, coderws.MessageText, []byte(`{"type":"error","error":{"code":"rate_limit_exceeded","type":"usage_limit_reached","message":"The usage limit has been reached"}}`)); err != nil {
				return
			}
		}
	}))
	defer failedUpstream.Close()
	account := service.Account{
		ID: 9902, Name: "ws-inflight-rate-limited", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: 1,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": failedUpstream.URL},
		Extra: map[string]any{
			"openai_apikey_responses_websockets_v2_enabled": true,
			"openai_apikey_responses_websockets_v2_mode":    service.OpenAIWSIngressModePassthrough,
		},
	}
	h := newWSInflightHarness(t, service.OpenAIWSIngressModePassthrough, account)
	h.send(t, `{"type":"response.create","model":"gpt-5.1","input":"one"}`)
	wsInflightReceive(t, failedRequests)
	firstAmount := wsInflightReceive(t, h.cache.reserved)
	firstAt := wsInflightReceive(t, h.cache.pricing)
	h.cache.mu.Lock()
	var firstID string
	for id := range h.cache.res {
		firstID = id
	}
	h.cache.mu.Unlock()
	select {
	case <-h.requests:
	case <-time.After(8 * time.Second):
		t.Fatal("rate-limited account did not fail over")
	}
	require.Len(t, failedRequests, 6, "all six retries should run within the first reservation")
	require.Equal(t, firstAmount, wsInflightReceive(t, h.cache.reserved))
	require.True(t, wsInflightReceive(t, h.cache.pricing).After(firstAt))
	h.cache.mu.Lock()
	_, staleReservation := h.cache.res[firstID]
	h.cache.mu.Unlock()
	require.False(t, staleReservation, "new account must not retain the old attempt's reservation")
	require.Equal(t, 1, h.cache.count())
	require.Empty(t, h.cache.reserved, "internal retries must not add reservations")
	require.EqualValues(t, 1, h.rpm.calls.Load(), "account attempts belong to the same logical turn")
	h.complete(t, 1, "gpt-5.1")
	h.settle(t)
}

func TestOpenAIWSTurnReservation_ContextAndAsyncHandoff(t *testing.T) {
	cache := newHandlerInflightCache(1)
	cfg := &config.Config{}
	cfg.Billing.InflightReservation = config.InflightReservationConfig{Enabled: true, TTLSeconds: 60}
	billing := service.NewBillingCacheService(cache, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	apiKey := &service.APIKey{User: &service.User{ID: 1}}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	var turn openAIWSTurnReservation
	defer turn.End()
	require.NoError(t, turn.Begin(parent, billing, &countingEstimator{cost: 0.6, priced: true}, apiKey, nil, service.InflightEstimateRequest{}))
	require.Nil(t, service.InflightReservationFromContext(parent), "connection context must remain independent")
	turnCtx := turn.Context(parent)
	first := service.InflightReservationFromContext(turnCtx)
	require.NotNil(t, first)
	done := first.Acquire()
	turn.End()
	turn.End()
	require.Nil(t, service.InflightReservationFromContext(turn.Context(parent)))
	require.Equal(t, 1, cache.count(), "ending the turn must preserve pending billing references")
	require.ErrorIs(t, turn.Begin(parent, billing, &countingEstimator{cost: 0.6, priced: true}, apiKey, nil, service.InflightEstimateRequest{}), service.ErrInsufficientBalance)
	require.Nil(t, service.InflightReservationFromContext(turn.Context(parent)), "failed begin must not expose the previous turn")
	done()
	require.Zero(t, cache.count())
	require.NoError(t, turn.Begin(parent, billing, &countingEstimator{cost: 0.6, priced: true}, apiKey, nil, service.InflightEstimateRequest{}))
	require.NotSame(t, first, service.InflightReservationFromContext(turn.Context(parent)))
	turn.End()
	require.Zero(t, cache.count())
}
