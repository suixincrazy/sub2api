package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIInflight_HTTPFreezesPricingBeforeReservation(t *testing.T) {
	for _, endpoint := range []string{"responses", "messages"} {
		t.Run(endpoint, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			cfg := &config.Config{}
			cfg.Default.RateMultiplier = 1
			cfg.Billing.InflightReservation = config.InflightReservationConfig{Enabled: true, TTLSeconds: 60}
			cache := &wsInflightBillingCache{handlerInflightCache: newHandlerInflightCache(1), reserved: make(chan float64, 1), pricing: make(chan time.Time, 1)}
			billing := service.NewBillingCacheService(cache, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billing.Stop)
			gateway := service.NewOpenAIGatewayService(
				&openAIWSUsageHandlerAccountRepoStub{}, nil, nil, nil, nil, nil, nil, cfg,
				nil, nil, service.NewBillingService(cfg, nil), nil, billing,
				nil, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil,
			)
			concurrency := &concurrencyCacheMock{
				acquireUserSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
			}
			h := &OpenAIGatewayHandler{
				cfg: cfg, gatewayService: gateway, billingCacheService: billing, apiKeyService: &service.APIKeyService{},
				concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(concurrency), SSEPingFormatNone, time.Second),
			}
			groupID := int64(1)
			key := &service.APIKey{
				ID: 1, UserID: 1, GroupID: &groupID,
				User:  &service.User{ID: 1, Balance: 1, Status: service.StatusActive},
				Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, RateMultiplier: 1, AllowMessagesDispatch: true},
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			body := `{"model":"gpt-5.1","input":"one","messages":[{"role":"user","content":"one"}],"max_tokens":128}`
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+endpoint, strings.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(string(middleware.ContextKeyAPIKey), key)
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1, Concurrency: 1})
			if endpoint == "responses" {
				h.Responses(c)
			} else {
				h.Messages(c)
			}
			require.Positive(t, wsInflightReceive(t, cache.reserved), "handler must reach reservation before selecting an account")
			pricingAt := wsInflightReceive(t, cache.pricing)
			require.False(t, pricingAt.IsZero(), "reservation must receive the request's frozen pricing time")
			require.Equal(t, pricingAt, service.OpenAIPricingAtFromContext(c.Request.Context()), "account selection must retain the reservation's pricing time")
			require.Zero(t, cache.count(), "failed account selection must release the reservation")
		})
	}
}

func TestOpenAIWSTurnPricing_ContextMatchesFrozenTime(t *testing.T) {
	var p openAIWSTurnPricing
	parent := context.Background()
	require.Equal(t, parent, p.contextOr(parent))
	gateway := &service.OpenAIGatewayService{}
	for range 2 {
		turnCtx, at := gateway.WithOpenAITurnPricingContext(parent, nil)
		p.freezeContext(turnCtx, at)
		require.Same(t, turnCtx, p.contextOr(parent))
		require.Equal(t, at, p.currentOr(time.Time{}))
		require.Equal(t, at, service.OpenAIPricingAtFromContext(p.contextOr(parent)))
	}
	require.True(t, service.OpenAIPricingAtFromContext(parent).IsZero())
}
