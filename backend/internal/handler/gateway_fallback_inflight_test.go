//go:build unit

package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type fallbackInflightUpstream struct {
	call func(*http.Request, int64) *http.Response
}

func (u *fallbackInflightUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	return u.call(req, accountID), nil
}

func (u *fallbackInflightUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

func TestGatewayMessages_FallbackReestimatesInflight(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name        string
		primaryRate float64
		balance     float64
		held        float64
		pending     bool
		reject      bool
	}{
		{name: "free to paid rejects concurrent excess", primaryRate: 0, balance: 0.01, held: 0.001, reject: true},
		{name: "cheap to costly rejects concurrent excess", primaryRate: 0.1, balance: 0.01, held: 0.001, reject: true},
		{name: "free to paid reserves before forwarding", primaryRate: 0, balance: 100},
		{name: "replaces own reservation without double counting", primaryRate: 0.1, balance: 0.016},
		{name: "keeps pending billing reference", primaryRate: 0.1, balance: 100, pending: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Billing.InflightReservation = config.InflightReservationConfig{Enabled: true, TTLSeconds: 60}
			cache := newHandlerInflightCache(tc.balance)
			billingCache := service.NewBillingCacheService(cache, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billingCache.Stop)
			billing := service.NewBillingService(cfg, nil)
			fallback := &service.Group{ID: 9102, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive, RateMultiplier: 1}
			primary := &service.Group{ID: 9101, Hydrated: true, Platform: service.PlatformAntigravity, Status: service.StatusActive,
				RateMultiplier: tc.primaryRate, FallbackGroupIDOnInvalidRequest: &fallback.ID}
			key := &service.APIKey{ID: 9103, UserID: 9104, GroupID: &primary.ID, Group: primary, Status: service.StatusActive,
				User: &service.User{ID: 9104, Balance: tc.balance, Concurrency: 10}}
			primaryAccount := &service.Account{ID: 9111, Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth,
				Status: service.StatusActive, Schedulable: true, Concurrency: 1,
				Credentials:   map[string]any{"access_token": "test-token", "project_id": "test-project"},
				AccountGroups: []service.AccountGroup{{AccountID: 9111, GroupID: primary.ID}}}
			fallbackAccount := &service.Account{ID: 9112, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
				Status: service.StatusActive, Schedulable: true, Concurrency: 1,
				Credentials:   map[string]any{"api_key": "test-key", "base_url": "https://fallback.example.test"},
				AccountGroups: []service.AccountGroup{{AccountID: 9112, GroupID: fallback.ID}}}
			scheduler := service.NewSchedulerSnapshotService(&groupScopedSchedulerCache{fakeSchedulerCache: &fakeSchedulerCache{
				accounts: []*service.Account{primaryAccount, fallbackAccount},
			}}, nil, nil, nil, nil)
			upstream := &fallbackInflightUpstream{}
			settings := service.NewSettingService(&oauthCaptchaSettingRepo{}, cfg)
			gateway := service.NewGatewayService(nil, &groupMapRepo{fakeGroupRepo: &fakeGroupRepo{}, groups: map[int64]*service.Group{
				primary.ID: primary, fallback.ID: fallback,
			}}, nil, nil, nil, nil, nil, nil, cfg, scheduler, nil, billing, nil, billingCache,
				nil, upstream, nil, nil, nil, nil, nil, settings, nil, nil, service.NewModelPricingResolver(nil, billing), nil, nil, nil)
			h := &GatewayHandler{gatewayService: gateway, billingCacheService: billingCache, cfg: cfg, maxAccountSwitches: 1,
				concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(&fakeConcurrencyCache{}), SSEPingFormatClaude, 0),
				antigravityGatewayService: service.NewAntigravityGatewayService(nil, nil, scheduler,
					service.NewAntigravityTokenProvider(nil, nil, nil), nil, upstream, nil, nil)}
			body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":1000,"messages":[{"role":"user","content":"hello"}]}`)
			wantCost, priced := gateway.EstimateInflightReservation(context.Background(), cloneAPIKeyWithGroup(key, fallback), tokenInflightEstimate("claude-sonnet-4-5", body))
			require.True(t, priced)
			require.Positive(t, wantCost)
			if tc.held > 0 {
				other, err := billingCache.ReserveInflight(context.Background(), key.User, primary, nil, tc.held)
				require.NoError(t, err)
				t.Cleanup(other.HandlerDone)
			}

			var firstReservation *service.InflightReservation
			var finishBilling func()
			primaryCalls, fallbackCalls := 0, 0
			upstream.call = func(req *http.Request, accountID int64) *http.Response {
				message := "fallback request rejected by fixture"
				if accountID == primaryAccount.ID {
					primaryCalls++
					firstReservation = service.InflightReservationFromContext(req.Context())
					if tc.pending {
						require.NotNil(t, firstReservation)
						finishBilling = firstReservation.Acquire()
						t.Cleanup(finishBilling)
					}
					message = "Prompt is too long"
				} else {
					require.Equal(t, fallbackAccount.ID, accountID)
					fallbackCalls++
					reservation := service.InflightReservationFromContext(req.Context())
					require.NotNil(t, reservation, "paid fallback must reserve before contacting its upstream")
					require.NotSame(t, firstReservation, reservation, "fallback must replace the primary estimate")
					require.InDelta(t, wantCost, reservation.Amount(), 1e-12)
					wantCount := 1
					if tc.pending {
						wantCount++
					}
					require.Equal(t, wantCount, cache.count())
				}
				return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"` + message + `"}}`))}
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			ctx := context.WithValue(context.Background(), ctxkey.Group, primary)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)).WithContext(ctx)
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(string(middleware.ContextKeyAPIKey), key)
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: key.UserID, Concurrency: 10})
			h.Messages(c)
			require.Equal(t, 1, primaryCalls)
			if tc.reject {
				require.Zero(t, fallbackCalls)
				require.Contains(t, strings.ToLower(recorder.Body.String()), "balance")
				require.Equal(t, 1, cache.count(), "only the unrelated reservation remains")
			} else {
				require.Equal(t, 1, fallbackCalls, recorder.Body.String())
				if tc.pending {
					require.Equal(t, 1, cache.count(), "pending billing retains its own reference")
					finishBilling()
				}
				require.Zero(t, cache.count(), "the final handler reservation must be released")
			}
		})
	}
}
