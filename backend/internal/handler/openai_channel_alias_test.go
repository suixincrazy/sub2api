//go:build unit

package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type channelAliasUpstream struct {
	service.HTTPUpstream
	accountIDs []int64
	model      string
	url        string
}

func (u *channelAliasUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	u.accountIDs = append(u.accountIDs, accountID)
	u.model = gjson.GetBytes(body, "model").String()
	u.url = req.URL.String()
	reply := `{"input_tokens":42}`
	if strings.HasSuffix(req.URL.Path, "/alpha/search") {
		reply = `{"output":"search result"}`
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(reply))}, nil
}

func TestOpenAIChannelAliasRoutesToMappedAccount(t *testing.T) {
	const alias, target, dispatch = "claude-fable-5-1", "gpt-6.2-sol", "gpt-6.1-terra"
	const countPath, searchPath = "/v1/messages/count_tokens", "/v1/alpha/search"
	for _, tc := range []struct {
		name, path, groupDispatch string
		accountMapping            map[string]any
		wantModel                 string
	}{
		{"count by channel", countPath, "", nil, target},
		{"search by channel", searchPath, "", nil, target},
		{"search honors account mapping of channel target", searchPath, "", map[string]any{target: "gpt-6-astra"}, "gpt-6-astra"},
		{"explicit Messages dispatch selects account", countPath, dispatch, map[string]any{dispatch: dispatch}, dispatch},
		{"account mapping overrides Messages fallback", countPath, dispatch, map[string]any{dispatch: dispatch, target: "gpt-6-astra"}, "gpt-6-astra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newOpenAIResponsesFailoverTestContext(t, nil)
			key, _ := middleware.GetAPIKeyFromContext(c)
			key.Group.AllowMessagesDispatch = true
			if tc.groupDispatch != "" {
				key.Group.MessagesDispatchModelConfig.ExactModelMappings = map[string]string{alias: tc.groupDispatch}
			}
			credentials := map[string]any{"access_token": "fixture"}
			if tc.accountMapping != nil {
				credentials["model_mapping"] = tc.accountMapping
			}
			right := service.Account{ID: 2, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Priority: 5,
				Status: service.StatusActive, Schedulable: true, Credentials: credentials}
			wrong := right
			wrong.ID, wrong.Priority = 1, 0
			wrong.Credentials = map[string]any{"access_token": "fixture", "model_mapping": map[string]any{alias: "wrong-target"}}
			if tc.groupDispatch != "" {
				wrong.Credentials["model_mapping"] = map[string]any{target: "wrong-target"}
			}
			channels := service.NewChannelService(&openAIWSUsageHandlerChannelRepoStub{
				channels: []service.Channel{{ID: 1, Status: service.StatusActive, GroupIDs: []int64{key.Group.ID},
					ModelMapping: map[string]map[string]string{service.PlatformOpenAI: {alias: target}}}},
				groupPlatforms: map[int64]string{key.Group.ID: service.PlatformOpenAI},
			}, nil, nil, nil, nil)
			cfg := &config.Config{RunMode: config.RunModeSimple}
			upstream := &channelAliasUpstream{}
			billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billing.Stop)
			svc := service.NewOpenAIGatewayService(openAIImagesFailoverAccountRepo{accounts: []service.Account{wrong, right}},
				&openAIWSUsageHandlerUsageLogRepoStub{}, nil, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, billing,
				upstream, &service.DeferredService{}, nil, nil, nil, channels, nil, nil, nil)
			h := NewOpenAIGatewayHandler(svc, service.NewConcurrencyService(nil), billing, &service.APIKeyService{}, nil, nil, nil, nil, cfg)
			payload := `{"model":"` + alias + `","messages":[{"role":"user","content":"hello"}]}`
			if tc.path == searchPath {
				payload = `{"model":"` + alias + `","commands":{"search_query":[{"q":"hello"}]}}`
			}
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(payload))
			c.Request.Header.Set("Content-Type", "application/json")
			if tc.path == searchPath {
				h.AlphaSearch(c)
				require.Equal(t, "https://chatgpt.com/backend-api/codex/alpha/search", upstream.url)
			} else {
				h.CountTokens(c)
				require.Equal(t, "https://api.openai.com/v1/responses/input_tokens", upstream.url)
			}
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, []int64{right.ID}, upstream.accountIDs)
			require.Equal(t, tc.wantModel, upstream.model)
		})
	}
}

func TestChannelAliasRespectsResolvedProvider(t *testing.T) {
	const alias = "claude-fable-5-1"
	c, rec := newOpenAIResponsesFailoverTestContext(t, nil)
	key, _ := middleware.GetAPIKeyFromContext(c)
	key.Group.Platform = service.PlatformComposite
	key.Group.MessagesDispatchModelConfig.ExactModelMappings = map[string]string{alias: "gpt-6.2-sol"}
	target := service.Account{ID: 8, Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"model_mapping": map[string]any{alias: "deepseek-flash"}}}
	other := service.Account{ID: 9, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true,
		Credentials: map[string]any{"access_token": "fixture"}}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	upstream := &channelAliasUpstream{}
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	svc := service.NewOpenAIGatewayService(openAIImagesFailoverAccountRepo{accounts: []service.Account{other, target}},
		nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	h := NewOpenAIGatewayHandler(svc, service.NewConcurrencyService(nil), billing, &service.APIKeyService{}, nil, nil, nil, nil, cfg)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"`+alias+`","messages":[{"role":"user","content":"hello"}]}`))
	c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), target.Platform))
	h.CountTokens(c)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, target.ID, c.GetInt64(opsAccountIDKey))
	require.Positive(t, gjson.GetBytes(rec.Body.Bytes(), "input_tokens").Int())
	require.Empty(t, upstream.accountIDs, "DeepSeek uses the local token estimator")

	c, rec = newOpenAIResponsesFailoverTestContext(t, nil)
	key, _ = middleware.GetAPIKeyFromContext(c)
	key.Group.Platform = service.PlatformComposite
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(`{"model":"`+alias+`"}`))
	c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), target.Platform))
	h.AlphaSearch(c)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Empty(t, upstream.accountIDs)
}
