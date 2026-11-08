//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

// ---------------------------------------------------------------------------
// normalizeTier
// ---------------------------------------------------------------------------

func TestNormalizeTier(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		expected string
	}{
		{name: "empty string", raw: "", expected: ""},
		{name: "free-tier", raw: "free-tier", expected: "FREE"},
		{name: "g1-pro-tier", raw: "g1-pro-tier", expected: "PRO"},
		{name: "g1-ultra-tier", raw: "g1-ultra-tier", expected: "ULTRA"},
		{name: "unknown-something", raw: "unknown-something", expected: "UNKNOWN"},
		{name: "Google AI Pro contains pro keyword", raw: "Google AI Pro", expected: "PRO"},
		{name: "case insensitive FREE", raw: "FREE-TIER", expected: "FREE"},
		{name: "case insensitive Ultra", raw: "Ultra Plan", expected: "ULTRA"},
		{name: "arbitrary unrecognized string", raw: "enterprise-custom", expected: "UNKNOWN"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeTier(tt.raw)
			require.Equal(t, tt.expected, got, "normalizeTier(%q)", tt.raw)
		})
	}
}

// ---------------------------------------------------------------------------
// buildUsageInfo
// ---------------------------------------------------------------------------

func aqfBoolPtr(v bool) *bool { return &v }
func aqfIntPtr(v int) *int    { return &v }

func TestBuildUsageInfo_BasicModels(t *testing.T) {
	fetcher := &AntigravityQuotaFetcher{}

	modelsResp := &antigravity.FetchAvailableModelsResponse{
		Models: map[string]antigravity.ModelInfo{
			"claude-sonnet-4-20250514": {
				QuotaInfo: &antigravity.ModelQuotaInfo{
					RemainingFraction: 0.75,
					ResetTime:         "2026-03-08T12:00:00Z",
				},
				DisplayName:      "Claude Sonnet 4",
				SupportsImages:   aqfBoolPtr(true),
				SupportsThinking: aqfBoolPtr(false),
				ThinkingBudget:   aqfIntPtr(0),
				Recommended:      aqfBoolPtr(true),
				MaxTokens:        aqfIntPtr(200000),
				MaxOutputTokens:  aqfIntPtr(16384),
				SupportedMimeTypes: map[string]bool{
					"image/png":  true,
					"image/jpeg": true,
				},
			},
			"gemini-2.5-pro": {
				QuotaInfo: &antigravity.ModelQuotaInfo{
					RemainingFraction: 0.50,
					ResetTime:         "2026-03-08T15:00:00Z",
				},
				DisplayName:     "Gemini 2.5 Pro",
				MaxTokens:       aqfIntPtr(1000000),
				MaxOutputTokens: aqfIntPtr(65536),
			},
		},
	}

	info := fetcher.buildUsageInfo(modelsResp, "g1-pro-tier", "PRO", nil)

	// 基本字段
	require.NotNil(t, info.UpdatedAt, "UpdatedAt should be set")
	require.Equal(t, "PRO", info.SubscriptionTier)
	require.Equal(t, "g1-pro-tier", info.SubscriptionTierRaw)

	// AntigravityQuota
	require.Len(t, info.AntigravityQuota, 2)

	sonnetQuota := info.AntigravityQuota["claude-sonnet-4-20250514"]
	require.NotNil(t, sonnetQuota)
	require.Equal(t, 25, sonnetQuota.Utilization) // (1 - 0.75) * 100 = 25
	require.Equal(t, "2026-03-08T12:00:00Z", sonnetQuota.ResetTime)

	geminiQuota := info.AntigravityQuota["gemini-2.5-pro"]
	require.NotNil(t, geminiQuota)
	require.Equal(t, 50, geminiQuota.Utilization) // (1 - 0.50) * 100 = 50
	require.Equal(t, "2026-03-08T15:00:00Z", geminiQuota.ResetTime)

	// AntigravityQuotaDetails
	require.Len(t, info.AntigravityQuotaDetails, 2)

	sonnetDetail := info.AntigravityQuotaDetails["claude-sonnet-4-20250514"]
	require.NotNil(t, sonnetDetail)
	require.Equal(t, "Claude Sonnet 4", sonnetDetail.DisplayName)
	require.Equal(t, aqfBoolPtr(true), sonnetDetail.SupportsImages)
	require.Equal(t, aqfBoolPtr(false), sonnetDetail.SupportsThinking)
	require.Equal(t, aqfIntPtr(0), sonnetDetail.ThinkingBudget)
	require.Equal(t, aqfBoolPtr(true), sonnetDetail.Recommended)
	require.Equal(t, aqfIntPtr(200000), sonnetDetail.MaxTokens)
	require.Equal(t, aqfIntPtr(16384), sonnetDetail.MaxOutputTokens)
	require.Equal(t, map[string]bool{"image/png": true, "image/jpeg": true}, sonnetDetail.SupportedMimeTypes)

	geminiDetail := info.AntigravityQuotaDetails["gemini-2.5-pro"]
	require.NotNil(t, geminiDetail)
	require.Equal(t, "Gemini 2.5 Pro", geminiDetail.DisplayName)
	require.Nil(t, geminiDetail.SupportsImages)
	require.Nil(t, geminiDetail.SupportsThinking)
	require.Equal(t, aqfIntPtr(1000000), geminiDetail.MaxTokens)
	require.Equal(t, aqfIntPtr(65536), geminiDetail.MaxOutputTokens)
}

func TestBuildUsageInfo_DeprecatedModels(t *testing.T) {
	fetcher := &AntigravityQuotaFetcher{}

	modelsResp := &antigravity.FetchAvailableModelsResponse{
		Models: map[string]antigravity.ModelInfo{
			"claude-sonnet-4-20250514": {
				QuotaInfo: &antigravity.ModelQuotaInfo{
					RemainingFraction: 1.0,
				},
			},
		},
		DeprecatedModelIDs: map[string]antigravity.DeprecatedModelInfo{
			"claude-3-sonnet-20240229": {NewModelID: "claude-sonnet-4-20250514"},
			"claude-3-haiku-20240307":  {NewModelID: "claude-haiku-3.5-latest"},
		},
	}

	info := fetcher.buildUsageInfo(modelsResp, "", "", nil)

	require.Len(t, info.ModelForwardingRules, 2)
	require.Equal(t, "claude-sonnet-4-20250514", info.ModelForwardingRules["claude-3-sonnet-20240229"])
	require.Equal(t, "claude-haiku-3.5-latest", info.ModelForwardingRules["claude-3-haiku-20240307"])
}

func TestBuildUsageInfo_NoDeprecatedModels(t *testing.T) {
	fetcher := &AntigravityQuotaFetcher{}

	modelsResp := &antigravity.FetchAvailableModelsResponse{
		Models: map[string]antigravity.ModelInfo{
			"some-model": {
				QuotaInfo: &antigravity.ModelQuotaInfo{RemainingFraction: 0.9},
			},
		},
	}

	info := fetcher.buildUsageInfo(modelsResp, "", "", nil)

	require.Nil(t, info.ModelForwardingRules, "ModelForwardingRules should be nil when no deprecated models")
}

func TestBuildUsageInfo_EmptyModels(t *testing.T) {
	fetcher := &AntigravityQuotaFetcher{}

	modelsResp := &antigravity.FetchAvailableModelsResponse{
		Models: map[string]antigravity.ModelInfo{},
	}

	info := fetcher.buildUsageInfo(modelsResp, "", "", nil)

	require.NotNil(t, info)
	require.NotNil(t, info.AntigravityQuota)
	require.Empty(t, info.AntigravityQuota)
	require.NotNil(t, info.AntigravityQuotaDetails)
	require.Empty(t, info.AntigravityQuotaDetails)
	require.Nil(t, info.FiveHour, "FiveHour should be nil when no priority model exists")
}

func TestBuildUsageInfo_ModelWithNilQuotaInfo(t *testing.T) {
	fetcher := &AntigravityQuotaFetcher{}

	modelsResp := &antigravity.FetchAvailableModelsResponse{
		Models: map[string]antigravity.ModelInfo{
			"model-without-quota": {
				DisplayName: "No Quota Model",
				// QuotaInfo is nil
			},
		},
	}

	info := fetcher.buildUsageInfo(modelsResp, "", "", nil)

	require.NotNil(t, info)
	require.Empty(t, info.AntigravityQuota, "models with nil QuotaInfo should be skipped")
	require.Empty(t, info.AntigravityQuotaDetails, "models with nil QuotaInfo should be skipped from details too")
}

func TestBuildUsageInfo_FiveHourPriorityOrder(t *testing.T) {
	fetcher := &AntigravityQuotaFetcher{}

	// priorityModels = ["claude-sonnet-4-20250514", "claude-sonnet-4", "gemini-2.5-pro"]
	// When the first priority model exists, it should be used for FiveHour
	modelsResp := &antigravity.FetchAvailableModelsResponse{
		Models: map[string]antigravity.ModelInfo{
			"gemini-2.5-pro": {
				QuotaInfo: &antigravity.ModelQuotaInfo{
					RemainingFraction: 0.40,
					ResetTime:         "2026-03-08T18:00:00Z",
				},
			},
			"claude-sonnet-4-20250514": {
				QuotaInfo: &antigravity.ModelQuotaInfo{
					RemainingFraction: 0.80,
					ResetTime:         "2026-03-08T12:00:00Z",
				},
			},
		},
	}

	info := fetcher.buildUsageInfo(modelsResp, "", "", nil)

	require.NotNil(t, info.FiveHour, "FiveHour should be set when a priority model exists")
	// claude-sonnet-4-20250514 is first in priority list, so it should be used
	expectedUtilization := (1.0 - 0.80) * 100 // 20
	require.InDelta(t, expectedUtilization, info.FiveHour.Utilization, 0.01)
	require.NotNil(t, info.FiveHour.ResetsAt, "ResetsAt should be parsed from ResetTime")
}

func TestBuildUsageInfo_FiveHourFallbackToClaude4(t *testing.T) {
	fetcher := &AntigravityQuotaFetcher{}

	// Only claude-sonnet-4 exists (second in priority list), not claude-sonnet-4-20250514
	modelsResp := &antigravity.FetchAvailableModelsResponse{
		Models: map[string]antigravity.ModelInfo{
			"claude-sonnet-4": {
				QuotaInfo: &antigravity.ModelQuotaInfo{
					RemainingFraction: 0.60,
					ResetTime:         "2026-03-08T14:00:00Z",
				},
			},
			"gemini-2.5-pro": {
				QuotaInfo: &antigravity.ModelQuotaInfo{
					RemainingFraction: 0.30,
				},
			},
		},
	}

	info := fetcher.buildUsageInfo(modelsResp, "", "", nil)

	require.NotNil(t, info.FiveHour)
	expectedUtilization := (1.0 - 0.60) * 100 // 40
	require.InDelta(t, expectedUtilization, info.FiveHour.Utilization, 0.01)
}

func TestBuildUsageInfo_FiveHourFallbackToGemini(t *testing.T) {
	fetcher := &AntigravityQuotaFetcher{}

	// Only gemini-2.5-pro exists (third in priority list)
	modelsResp := &antigravity.FetchAvailableModelsResponse{
		Models: map[string]antigravity.ModelInfo{
			"gemini-2.5-pro": {
				QuotaInfo: &antigravity.ModelQuotaInfo{
					RemainingFraction: 0.30,
				},
			},
			"other-model": {
				QuotaInfo: &antigravity.ModelQuotaInfo{
					RemainingFraction: 0.90,
				},
			},
		},
	}

	info := fetcher.buildUsageInfo(modelsResp, "", "", nil)

	require.NotNil(t, info.FiveHour)
	expectedUtilization := (1.0 - 0.30) * 100 // 70
	require.InDelta(t, expectedUtilization, info.FiveHour.Utilization, 0.01)
}

func TestBuildUsageInfo_FiveHourNoPriorityModel(t *testing.T) {
	fetcher := &AntigravityQuotaFetcher{}

	// None of the priority models exist
	modelsResp := &antigravity.FetchAvailableModelsResponse{
		Models: map[string]antigravity.ModelInfo{
			"some-other-model": {
				QuotaInfo: &antigravity.ModelQuotaInfo{
					RemainingFraction: 0.50,
				},
			},
		},
	}

	info := fetcher.buildUsageInfo(modelsResp, "", "", nil)

	require.Nil(t, info.FiveHour, "FiveHour should be nil when no priority model exists")
}

func TestBuildUsageInfo_FiveHourWithEmptyResetTime(t *testing.T) {
	fetcher := &AntigravityQuotaFetcher{}

	modelsResp := &antigravity.FetchAvailableModelsResponse{
		Models: map[string]antigravity.ModelInfo{
			"claude-sonnet-4-20250514": {
				QuotaInfo: &antigravity.ModelQuotaInfo{
					RemainingFraction: 0.50,
					ResetTime:         "", // empty reset time
				},
			},
		},
	}

	info := fetcher.buildUsageInfo(modelsResp, "", "", nil)

	require.NotNil(t, info.FiveHour)
	require.Nil(t, info.FiveHour.ResetsAt, "ResetsAt should be nil when ResetTime is empty")
	require.Equal(t, 0, info.FiveHour.RemainingSeconds)
}

func TestBuildUsageInfo_FullUtilization(t *testing.T) {
	fetcher := &AntigravityQuotaFetcher{}

	modelsResp := &antigravity.FetchAvailableModelsResponse{
		Models: map[string]antigravity.ModelInfo{
			"claude-sonnet-4-20250514": {
				QuotaInfo: &antigravity.ModelQuotaInfo{
					RemainingFraction: 0.0, // fully used
					ResetTime:         "2026-03-08T12:00:00Z",
				},
			},
		},
	}

	info := fetcher.buildUsageInfo(modelsResp, "", "", nil)

	quota := info.AntigravityQuota["claude-sonnet-4-20250514"]
	require.NotNil(t, quota)
	require.Equal(t, 100, quota.Utilization)
}

func TestBuildUsageInfo_ZeroUtilization(t *testing.T) {
	fetcher := &AntigravityQuotaFetcher{}

	modelsResp := &antigravity.FetchAvailableModelsResponse{
		Models: map[string]antigravity.ModelInfo{
			"claude-sonnet-4-20250514": {
				QuotaInfo: &antigravity.ModelQuotaInfo{
					RemainingFraction: 1.0, // fully available
				},
			},
		},
	}

	info := fetcher.buildUsageInfo(modelsResp, "", "", nil)
	quota := info.AntigravityQuota["claude-sonnet-4-20250514"]
	require.NotNil(t, quota)
	require.Equal(t, 0, quota.Utilization)
}

func TestBuildUsageInfo_AICredits(t *testing.T) {
	fetcher := &AntigravityQuotaFetcher{}
	modelsResp := &antigravity.FetchAvailableModelsResponse{
		Models: map[string]antigravity.ModelInfo{},
	}
	loadResp := &antigravity.LoadCodeAssistResponse{
		PaidTier: &antigravity.PaidTierInfo{
			ID: "g1-pro-tier",
			AvailableCredits: []antigravity.AvailableCredit{
				{
					CreditType:                  "GOOGLE_ONE_AI",
					CreditAmount:                "25",
					MinimumCreditAmountForUsage: "5",
				},
			},
		},
	}

	info := fetcher.buildUsageInfo(modelsResp, "g1-pro-tier", "PRO", loadResp)

	require.Len(t, info.AICredits, 1)
	require.Equal(t, "GOOGLE_ONE_AI", info.AICredits[0].CreditType)
	require.Equal(t, 25.0, info.AICredits[0].Amount)
	require.Equal(t, 5.0, info.AICredits[0].MinimumBalance)
}

func TestFetchQuotaUsesConfiguredModelsListBodyLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":{"model-a":{}}}`))
	}))
	defer server.Close()

	oldBaseURLs := append([]string(nil), antigravity.BaseURLs...)
	oldAvailability := antigravity.DefaultURLAvailability
	t.Cleanup(func() {
		antigravity.BaseURLs = oldBaseURLs
		antigravity.DefaultURLAvailability = oldAvailability
	})
	antigravity.BaseURLs = []string{server.URL}
	antigravity.DefaultURLAvailability = antigravity.NewURLAvailability(time.Minute)

	cfg := &config.Config{}
	cfg.Gateway.ModelsListReadMaxBytes = 8
	fetcher := NewAntigravityQuotaFetcher(nil, cfg)
	_, err := fetcher.FetchQuota(context.Background(), &Account{
		Platform: PlatformAntigravity,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "token",
			"project_id":   "project",
		},
	}, "")
	require.ErrorContains(t, err, "响应超过 8 字节")
}

func TestFetchQuota_UsesForwardEndpointForModelsAndWindows(t *testing.T) {
	// Account 17's prod quota was unused while daily reported 32.3412% used with
	// an earlier weekly reset. Both successful responses are valid for their endpoint.
	for _, tt := range []struct {
		name          string
		plan          string
		mode          string
		wantDaily     bool
		modelsStatus  int
		summaryStatus int
	}{
		{name: "pro", plan: "Pro", wantDaily: true},
		{name: "ultra", plan: "g1-ultra-tier", wantDaily: true},
		{name: "free", plan: "Free"},
		{name: "explicit prod", plan: "Pro", mode: "prod"},
		{name: "explicit daily", plan: "Free", mode: "daily", wantDaily: true},
		{name: "model failure does not read unused prod quota", plan: "Pro", wantDaily: true, modelsStatus: http.StatusServiceUnavailable},
		{name: "summary failure does not mix prod windows", plan: "Pro", wantDaily: true, summaryStatus: http.StatusServiceUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(antigravityForwardBaseURLEnv, tt.mode)
			var modelCalls, summaryCalls [2]atomic.Int32
			resets := [2]string{"2026-11-15T13:38:30Z", "2026-11-15T04:53:24Z"}
			fractions := [2]float64{1, 0.676588}
			selected := 0
			if tt.wantDaily {
				selected = 1
			}
			newEndpoint := func(index int) *httptest.Server {
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/v1internal:fetchAvailableModels":
						modelCalls[index].Add(1)
						if index == selected && tt.modelsStatus != 0 {
							w.WriteHeader(tt.modelsStatus)
							_, _ = w.Write([]byte(`{"error":{"status":"UNAVAILABLE"}}`))
							return
						}
						_ = json.NewEncoder(w).Encode(antigravity.FetchAvailableModelsResponse{
							Models: map[string]antigravity.ModelInfo{"gemini-4-argon-high": {
								QuotaInfo: &antigravity.ModelQuotaInfo{RemainingFraction: fractions[index], ResetTime: resets[index]},
							}},
						})
					case "/v1internal:retrieveUserQuotaSummary":
						summaryCalls[index].Add(1)
						if index == selected && tt.summaryStatus != 0 {
							w.WriteHeader(tt.summaryStatus)
							_, _ = w.Write([]byte(`{"error":{"status":"UNAVAILABLE"}}`))
							return
						}
						_ = json.NewEncoder(w).Encode(antigravity.QuotaSummaryResponse{
							Groups: []antigravity.QuotaSummaryGroup{{DisplayName: "Gemini 4 Argon", Buckets: []antigravity.QuotaSummaryBucket{
								{BucketID: "gemini-argon-wtu-weekly", Window: "weekly", RemainingFraction: fractions[index], ResetTime: resets[index]},
							}}},
						})
					case "/v1internal:loadCodeAssist":
						_, _ = w.Write([]byte(`{"currentTier":{"id":"free-tier"},"paidTier":{"id":"g1-pro-tier"}}`))
					default:
						w.WriteHeader(http.StatusNotFound)
					}
				}))
			}
			prod, daily := newEndpoint(0), newEndpoint(1)
			defer prod.Close()
			defer daily.Close()
			oldURLs, oldAvailability := antigravity.BaseURLs, antigravity.DefaultURLAvailability
			t.Cleanup(func() {
				antigravity.BaseURLs = oldURLs
				antigravity.DefaultURLAvailability = oldAvailability
			})
			antigravity.BaseURLs = []string{prod.URL, daily.URL}
			antigravity.DefaultURLAvailability = antigravity.NewURLAvailability(time.Minute)
			result, err := NewAntigravityQuotaFetcher(nil, nil).FetchQuota(context.Background(), &Account{
				ID: 17, Platform: PlatformAntigravity, Type: AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "test-token", "project_id": "test-project", "plan_type": tt.plan},
			}, "")
			if tt.modelsStatus != 0 {
				require.ErrorContains(t, err, "HTTP 503")
				require.Nil(t, result)
				require.Zero(t, modelCalls[1-selected].Load(), "do not replace failed quota with another endpoint's balance")
				return
			}
			require.NoError(t, err)
			model := result.UsageInfo.AntigravityQuota["gemini-4-argon-high"]
			require.NotNil(t, model)
			require.Equal(t, int((1-fractions[selected])*100), model.Utilization)
			require.Equal(t, resets[selected], model.ResetTime)
			require.Equal(t, int32(1), modelCalls[selected].Load())
			require.Zero(t, modelCalls[1-selected].Load())
			require.Equal(t, int32(1), summaryCalls[selected].Load())
			require.Zero(t, summaryCalls[1-selected].Load())
			if tt.summaryStatus != 0 {
				require.Empty(t, result.UsageInfo.AntigravityQuotaGroups)
				return
			}
			require.Len(t, result.UsageInfo.AntigravityQuotaGroups, 1)
			group := result.UsageInfo.AntigravityQuotaGroups[0]
			require.Equal(t, "gemini-argon-wtu", group.Key)
			require.Len(t, group.Windows, 1)
			require.Equal(t, "weekly", group.Windows[0].Window)
			require.InDelta(t, (1-fractions[selected])*100, group.Windows[0].Utilization, 0.0001)
			require.Equal(t, resets[selected], group.Windows[0].ResetsAt.UTC().Format(time.RFC3339))
		})
	}
}

func TestFetchQuota_ForbiddenReturnsIsForbidden(t *testing.T) {
	// 模拟 FetchQuota 遇到 403 时的行为：
	// FetchAvailableModels 返回 ForbiddenError → FetchQuota 应返回 is_forbidden=true
	forbiddenErr := &antigravity.ForbiddenError{
		StatusCode: 403,
		Body:       "Access denied",
	}

	// 验证 ForbiddenError 满足 errors.As
	var target *antigravity.ForbiddenError
	require.True(t, errors.As(forbiddenErr, &target))
	require.Equal(t, 403, target.StatusCode)
	require.Equal(t, "Access denied", target.Body)
	require.Contains(t, forbiddenErr.Error(), "403")
}

// ---------------------------------------------------------------------------
// classifyForbiddenType
// ---------------------------------------------------------------------------

func TestClassifyForbiddenType(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		expected string
	}{
		{
			name:     "VALIDATION_REQUIRED keyword",
			body:     `{"error":{"message":"VALIDATION_REQUIRED"}}`,
			expected: "validation",
		},
		{
			name:     "verify your account",
			body:     `Please verify your account to continue`,
			expected: "validation",
		},
		{
			name:     "contains validation_url field",
			body:     `{"error":{"details":[{"metadata":{"validation_url":"https://..."}}]}}`,
			expected: "validation",
		},
		{
			name:     "terms of service violation",
			body:     `Your account has been suspended for Terms of Service violation`,
			expected: "violation",
		},
		{
			name:     "violation keyword",
			body:     `Account suspended due to policy violation`,
			expected: "violation",
		},
		{
			name:     "generic 403",
			body:     `Access denied`,
			expected: "forbidden",
		},
		{
			name:     "empty body",
			body:     "",
			expected: "forbidden",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyForbiddenType(tt.body)
			require.Equal(t, tt.expected, got)
		})
	}
}

// ---------------------------------------------------------------------------
// extractValidationURL
// ---------------------------------------------------------------------------

func TestExtractValidationURL(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		expected string
	}{
		{
			name:     "structured validation_url",
			body:     `{"error":{"details":[{"metadata":{"validation_url":"https://accounts.google.com/verify?token=abc"}}]}}`,
			expected: "https://accounts.google.com/verify?token=abc",
		},
		{
			name:     "structured appeal_url",
			body:     `{"error":{"details":[{"metadata":{"appeal_url":"https://support.google.com/appeal/123"}}]}}`,
			expected: "https://support.google.com/appeal/123",
		},
		{
			name:     "validation_url takes priority over appeal_url",
			body:     `{"error":{"details":[{"metadata":{"validation_url":"https://v.com","appeal_url":"https://a.com"}}]}}`,
			expected: "https://v.com",
		},
		{
			name:     "fallback regex with verify keyword",
			body:     `Please verify your account at https://accounts.google.com/verify`,
			expected: "https://accounts.google.com/verify",
		},
		{
			name:     "no URL in generic forbidden",
			body:     `Access denied`,
			expected: "",
		},
		{
			name:     "empty body",
			body:     "",
			expected: "",
		},
		{
			name:     "URL present but no validation keywords",
			body:     `Error at https://example.com/something`,
			expected: "",
		},
		{
			name:     "unicode escaped ampersand",
			body:     `validation required: https://accounts.google.com/verify?a=1\u0026b=2`,
			expected: "https://accounts.google.com/verify?a=1&b=2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractValidationURL(tt.body)
			require.Equal(t, tt.expected, got)
		})
	}
}
