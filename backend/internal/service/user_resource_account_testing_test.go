package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func TestAvailableUserAccountTestModelsUsesOwnerAccountMapping(t *testing.T) {
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"custom-review-model": "gpt-5.4",
				"gpt-5.6":             "gpt-5.6",
			},
		},
		Extra: map[string]any{},
	}

	models := availableUserAccountTestModels(account)
	if len(models) != 2 {
		t.Fatalf("expected mapped model list, got %#v", models)
	}
	if models[0].ID != "custom-review-model" || models[0].DisplayName != "custom-review-model" {
		t.Fatalf("custom model metadata was not preserved: %#v", models[0])
	}
	if models[1].ID != "gpt-5.6" || models[1].DisplayName == "" {
		t.Fatalf("known model metadata was not resolved: %#v", models[1])
	}
}

func TestAvailableUserAccountTestModelsOpenAIPassthroughUsesDefaults(t *testing.T) {
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"custom-only": "custom-only"},
		},
		Extra: map[string]any{"openai_passthrough": true},
	}

	models := availableUserAccountTestModels(account)
	if len(models) < 2 || models[0].ID == "custom-only" {
		t.Fatalf("passthrough account should use the curated defaults: %#v", models)
	}
}

func TestAvailableUserAccountTestModelsCNProvidersUseProviderDefaults(t *testing.T) {
	expected := map[string]string{
		PlatformKimi: "kimi-k2.6", PlatformZhipu: "glm-5.2", PlatformDeepseek: "deepseek-chat",
	}
	for platform, firstModel := range expected {
		models := availableUserAccountTestModels(&Account{Platform: platform, Type: AccountTypeAPIKey})
		if len(models) == 0 || models[0].ID != firstModel {
			t.Fatalf("platform %s should expose provider test models: %#v", platform, models)
		}
	}
}

func TestStreamAccountTestRejectsForeignAccountBeforeUpstreamTest(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM accounts`).
		WithArgs(int64(42), int64(99)).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/my/accounts/42/test/stream", nil).WithContext(context.Background())

	svc := NewUserResourceService(db, nil, nil, nil)
	err = svc.StreamAccountTest(c, 99, 42, "gpt-5.6", "", "default")
	if err != ErrUserResourceNotFound {
		t.Fatalf("expected owner-scoped not found error, got %v", err)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("foreign account test wrote a response before ownership rejection: %q", recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestProvideUserResourceServiceWiresAccountMaintenanceServices(t *testing.T) {
	accountTestService := &AccountTestService{}
	tokenRefreshService := &TokenRefreshService{}
	svc := ProvideUserResourceService(
		nil, nil, nil, nil,
		nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil,
		accountTestService, tokenRefreshService,
	)
	defer func() { _ = svc.Close() }()

	if svc.accountTestService != accountTestService {
		t.Fatal("account test service was not wired into user resources")
	}
	if svc.tokenRefreshService != tokenRefreshService {
		t.Fatal("token refresh service was not wired into user resources")
	}
}
