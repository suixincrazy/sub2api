package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type myResourceSettingRepo struct {
	values map[string]string
	err    error
}

func (r myResourceSettingRepo) Get(context.Context, string) (*service.Setting, error) {
	return nil, service.ErrSettingNotFound
}

func (r myResourceSettingRepo) GetValue(context.Context, string) (string, error) {
	return "", service.ErrSettingNotFound
}

func (r myResourceSettingRepo) Set(context.Context, string, string) error {
	return nil
}

func (r myResourceSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	if r.err != nil {
		return nil, r.err
	}
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		out[key] = r.values[key]
	}
	return out, nil
}

func (r myResourceSettingRepo) SetMultiple(context.Context, map[string]string) error {
	return nil
}

func (r myResourceSettingRepo) GetAll(context.Context) (map[string]string, error) {
	return r.values, r.err
}

func (r myResourceSettingRepo) Delete(context.Context, string) error {
	return nil
}

func TestMyResourceHandlerCurrentUserFeatureGateAndAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/my/groups", nil)
		return c, recorder
	}

	t.Run("feature gate disabled fails closed before using my resource endpoints", func(t *testing.T) {
		h := NewMyResourceHandler(nil, service.NewSettingService(myResourceSettingRepo{
			values: map[string]string{service.SettingKeyEnableUserResources: "false"},
		}, nil))
		c, recorder := newContext()
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 42})

		userID, ok := h.currentUser(c)

		if ok || userID != 0 {
			t.Fatalf("expected disabled feature gate to reject current user, got id=%d ok=%v", userID, ok)
		}
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("expected 403 when user resources disabled, got %d", recorder.Code)
		}
	})

	t.Run("missing feature gate service fails closed", func(t *testing.T) {
		h := NewMyResourceHandler(nil, nil)
		c, recorder := newContext()
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 42})

		userID, ok := h.currentUser(c)

		if ok || userID != 0 {
			t.Fatalf("expected missing setting service to reject current user, got id=%d ok=%v", userID, ok)
		}
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("expected 403 without feature gate service, got %d", recorder.Code)
		}
	})

	t.Run("missing auth subject is unauthorized", func(t *testing.T) {
		h := NewMyResourceHandler(nil, service.NewSettingService(myResourceSettingRepo{
			values: map[string]string{service.SettingKeyEnableUserResources: "true"},
		}, nil))
		c, recorder := newContext()

		userID, ok := h.currentUser(c)

		if ok || userID != 0 {
			t.Fatalf("expected missing auth subject to reject current user, got id=%d ok=%v", userID, ok)
		}
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 without auth subject, got %d", recorder.Code)
		}
	})

	t.Run("enabled feature gate returns authenticated user id", func(t *testing.T) {
		h := NewMyResourceHandler(nil, service.NewSettingService(myResourceSettingRepo{
			values: map[string]string{service.SettingKeyEnableUserResources: "true"},
		}, nil))
		c, recorder := newContext()
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 42})

		userID, ok := h.currentUser(c)

		if !ok || userID != 42 {
			t.Fatalf("expected authenticated user id 42, got id=%d ok=%v", userID, ok)
		}
		if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("Pragma") != "no-cache" {
			t.Fatalf("private user resource response is cacheable: %#v", recorder.Header())
		}
	})
}

func TestBindJSONMapRejectsTrailingAndOversizedBodies(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("trailing JSON object", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/my/groups", bytes.NewBufferString(`{"name":"one"}{"name":"two"}`))

		if _, ok := bindJSONMap(c); ok {
			t.Fatal("expected trailing JSON object to be rejected")
		}
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for trailing JSON, got %d", recorder.Code)
		}
	})

	t.Run("oversized body", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		body := append([]byte(`{"value":"`), bytes.Repeat([]byte("x"), int(myResourceJSONBodyMaxBytes))...)
		body = append(body, []byte(`"}`)...)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/my/groups", bytes.NewReader(body))

		if _, ok := bindJSONMap(c); ok {
			t.Fatal("expected oversized JSON body to be rejected")
		}
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for oversized body, got %d", recorder.Code)
		}
	})
}
