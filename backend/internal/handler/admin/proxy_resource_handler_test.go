package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestProxyHandlerConstructors(t *testing.T) {
	legacy := NewProxyHandler(nil)
	require.NotNil(t, legacy)
	require.Nil(t, legacy.adminService)
	require.Nil(t, legacy.userResourceService)

	resourceService := &service.UserResourceService{}
	provided := ProvideProxyHandler(nil, resourceService)
	require.NotNil(t, provided)
	require.Nil(t, provided.adminService)
	require.Same(t, resourceService, provided.userResourceService)
}

func TestBindAdminProxyJSONMapAcceptsOneObject(t *testing.T) {
	context, recorder := newAdminProxyJSONTestContext(`{"content":"ss://example","is_public":true}`)

	payload, ok := bindAdminProxyJSONMap(context)

	require.True(t, ok)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "ss://example", payload["content"])
	require.Equal(t, true, payload["is_public"])
}

func TestBindAdminProxyJSONMapRejectsNonObjectAndTrailingValues(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "array", body: `[{"content":"one"}]`},
		{name: "null", body: `null`},
		{name: "two objects", body: `{"content":"one"}{"content":"two"}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			context, recorder := newAdminProxyJSONTestContext(test.body)

			payload, ok := bindAdminProxyJSONMap(context)

			require.False(t, ok)
			require.Nil(t, payload)
			require.Equal(t, http.StatusBadRequest, recorder.Code)
		})
	}
}

func TestBindAdminProxyJSONMapRejectsBodiesLargerThanEightMiB(t *testing.T) {
	body := `{"content":"` + strings.Repeat("x", int(maxAdminProxyJSONBodyBytes)) + `"}`
	context, recorder := newAdminProxyJSONTestContext(body)

	payload, ok := bindAdminProxyJSONMap(context)

	require.False(t, ok)
	require.Nil(t, payload)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

func TestImportProxyNodesRejectsInvalidContentFields(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing", body: `{}`},
		{name: "null", body: `{"content":null}`},
		{name: "empty", body: `{"content":""}`},
		{name: "whitespace", body: `{"content":" \t\r\n "}`},
		{name: "number", body: `{"content":123}`},
		{name: "boolean", body: `{"content":true}`},
		{name: "object", body: `{"content":{}}`},
		{name: "array", body: `{"content":[]}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			context, recorder := newAdminProxyJSONTestContext(test.body)
			handler := ProvideProxyHandler(nil, &service.UserResourceService{})

			handler.ImportProxyNodes(context)

			requireAdminProxyBadRequest(t, recorder, invalidAdminProxyImportContentMessage)
		})
	}
}

func TestImportProxyNodesRejectsNonStringNamePrefix(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "null", value: `null`},
		{name: "number", value: `123`},
		{name: "boolean", value: `true`},
		{name: "object", value: `{}`},
		{name: "array", value: `[]`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := `{"content":"ss://example","name_prefix":` + test.value + `}`
			context, recorder := newAdminProxyJSONTestContext(body)
			handler := ProvideProxyHandler(nil, &service.UserResourceService{})

			handler.ImportProxyNodes(context)

			requireAdminProxyBadRequest(t, recorder, invalidAdminProxyImportNamePrefixMessage)
		})
	}
}

func TestImportProxyNodesRejectsNonBooleanIsPublic(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "null", value: `null`},
		{name: "string", value: `"true"`},
		{name: "number", value: `1`},
		{name: "object", value: `{}`},
		{name: "array", value: `[]`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := `{"content":"ss://example","is_public":` + test.value + `}`
			context, recorder := newAdminProxyJSONTestContext(body)
			handler := ProvideProxyHandler(nil, &service.UserResourceService{})

			handler.ImportProxyNodes(context)

			requireAdminProxyBadRequest(t, recorder, invalidAdminProxyImportIsPublicMessage)
		})
	}
}

func TestParseAdminProxyImportRequestAcceptsOptionalStringNamePrefix(t *testing.T) {
	tests := []struct {
		name           string
		payload        map[string]any
		wantNamePrefix string
		wantIsPublic   bool
	}{
		{name: "omitted", payload: map[string]any{"content": "not a proxy node"}},
		{name: "empty", payload: map[string]any{"content": "not a proxy node", "name_prefix": ""}},
		{name: "whitespace", payload: map[string]any{"content": "not a proxy node", "name_prefix": "  "}, wantNamePrefix: "  "},
		{name: "string and public", payload: map[string]any{"content": "not a proxy node", "name_prefix": "edge", "is_public": true}, wantNamePrefix: "edge", wantIsPublic: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			context, recorder := newAdminProxyJSONTestContext(`{}`)

			request, ok := parseAdminProxyImportRequest(context, test.payload)

			require.True(t, ok)
			require.Equal(t, http.StatusOK, recorder.Code)
			require.Equal(t, "not a proxy node", request.content)
			require.Equal(t, test.wantNamePrefix, request.namePrefix)
			require.Equal(t, test.wantIsPublic, request.isPublic)
		})
	}
}

func requireAdminProxyBadRequest(t *testing.T, recorder *httptest.ResponseRecorder, message string) {
	t.Helper()

	var envelope struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Equal(t, http.StatusBadRequest, envelope.Code)
	require.Equal(t, message, envelope.Message)
}

func newAdminProxyJSONTestContext(body string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/proxies/import", strings.NewReader(body))
	context.Request.Header.Set("Content-Type", "application/json")
	return context, recorder
}
