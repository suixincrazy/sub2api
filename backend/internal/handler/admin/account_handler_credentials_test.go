package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func setupAccountCredentialsRouter(account *service.Account) *gin.Engine {
	gin.SetMode(gin.TestMode)
	stub := newStubAdminService()
	stub.getAccountResult = account
	handler := NewAccountHandler(stub, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.GET("/accounts/:id", handler.GetByID)
	router.GET("/accounts/:id/credentials", handler.GetCredentials)
	return router
}

func TestAccountGetCredentialsReturnsPlaintext(t *testing.T) {
	router := setupAccountCredentialsRouter(&service.Account{
		ID:       7,
		Platform: service.PlatformAnthropic,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-ant-plaintext",
			"base_url": "https://api.anthropic.com",
		},
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/accounts/7/credentials", nil))

	require.Equal(t, http.StatusOK, recorder.Code)

	var body struct {
		Data struct {
			ID          int64          `json:"id"`
			Type        string         `json:"type"`
			Platform    string         `json:"platform"`
			Credentials map[string]any `json:"credentials"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, int64(7), body.Data.ID)
	require.Equal(t, "apikey", body.Data.Type)
	require.Equal(t, "anthropic", body.Data.Platform)
	require.Equal(t, "sk-ant-plaintext", body.Data.Credentials["api_key"])
	require.Equal(t, "https://api.anthropic.com", body.Data.Credentials["base_url"])
}

// GetByID 必须保持脱敏——新增的凭证接口是全库唯一回传原文的路径，
// 若哪天有人图省事把原文塞回详情接口，渠道页/分组页会跟着批量泄露。
func TestAccountGetByIDStaysRedactedAfterCredentialsEndpoint(t *testing.T) {
	router := setupAccountCredentialsRouter(&service.Account{
		ID:       7,
		Platform: service.PlatformAnthropic,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-ant-plaintext",
			"base_url": "https://api.anthropic.com",
		},
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/accounts/7", nil))

	require.Equal(t, http.StatusOK, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "sk-ant-plaintext")

	var body struct {
		Data struct {
			Credentials       map[string]any  `json:"credentials"`
			CredentialsStatus map[string]bool `json:"credentials_status"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.NotContains(t, body.Data.Credentials, "api_key")
	require.True(t, body.Data.CredentialsStatus["has_api_key"])
}

func TestAccountGetCredentialsRejectsInvalidID(t *testing.T) {
	router := setupAccountCredentialsRouter(&service.Account{ID: 7})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/accounts/abc/credentials", nil))

	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

// 账号没有凭证时返回 null 而不是空对象，前端据此判断"无可回填内容"。
func TestAccountGetCredentialsNilCredentials(t *testing.T) {
	router := setupAccountCredentialsRouter(&service.Account{
		ID:       7,
		Platform: service.PlatformAnthropic,
		Type:     service.AccountTypeAPIKey,
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/accounts/7/credentials", nil))

	require.Equal(t, http.StatusOK, recorder.Code)

	var body struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.JSONEq(t, "null", string(body.Data["credentials"]))
}
