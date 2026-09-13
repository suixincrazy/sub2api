package handler

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/platform/liveattestation"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

type MyResourceHandler struct {
	userResourceService *service.UserResourceService
	settingService      *service.SettingService
}

const maxMyResourceBatchIDs = 1000

func NewMyResourceHandler(userResourceService *service.UserResourceService, settingService *service.SettingService) *MyResourceHandler {
	return &MyResourceHandler{
		userResourceService: userResourceService,
		settingService:      settingService,
	}
}

func (h *MyResourceHandler) currentUser(c *gin.Context) (int64, bool) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	if h.settingService == nil || !h.settingService.IsUserResourcesEnabled(c.Request.Context()) {
		response.Forbidden(c, "User resources are disabled")
		return 0, false
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Unauthorized(c, "User not found in context")
		return 0, false
	}
	return subject.UserID, true
}

func myListOptions(c *gin.Context) service.UserResourceListOptions {
	page, pageSize := response.ParsePagination(c)
	return service.UserResourceListOptions{
		Page:      page,
		PageSize:  pageSize,
		Search:    c.Query("search"),
		Status:    c.Query("status"),
		Platform:  c.Query("platform"),
		Type:      c.Query("type"),
		Protocol:  c.Query("protocol"),
		GroupID:   parseInt64Query(c, "group_id"),
		UserID:    parseInt64Query(c, "user_id"),
		APIKeyID:  parseInt64Query(c, "api_key_id"),
		AccountID: parseInt64Query(c, "account_id"),
		SourceID:  parseInt64Query(c, "source_id"),
		StartDate: c.Query("start_date"),
		EndDate:   c.Query("end_date"),
		Timezone:  c.Query("timezone"),
		SortBy:    c.Query("sort_by"),
		SortOrder: c.Query("sort_order"),
		OwnedOnly: parseBoolQuery(c.Query("owned_only")),
	}
}

func bindJSONMap(c *gin.Context) (map[string]any, bool) {
	payload := map[string]any{}
	if c.Request == nil || c.Request.Body == nil || c.Request.ContentLength == 0 {
		return payload, true
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, myResourceJSONBodyMaxBytes)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return nil, false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		response.BadRequest(c, "Invalid request: only one JSON object is allowed")
		return nil, false
	}
	return payload, true
}

func decodeMyResourcePayload(payload map[string]any, dst any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}

const myResourceJSONBodyMaxBytes int64 = 8 << 20

func parseInt64Param(c *gin.Context, name string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid "+name)
		return 0, false
	}
	return id, true
}

func parseInt64Query(c *gin.Context, name string) int64 {
	if raw := c.Query(name); raw != "" {
		id, _ := strconv.ParseInt(raw, 10, 64)
		return id
	}
	return 0
}

func boolQuery(c *gin.Context, name string) bool {
	if raw := c.Query(name); raw != "" {
		v, _ := strconv.ParseBool(raw)
		return v
	}
	return false
}

func int64SliceFromAny(v any) []int64 {
	switch t := v.(type) {
	case []any:
		out := make([]int64, 0, len(t))
		for _, item := range t {
			if id := int64FromAny(item); id > 0 {
				out = append(out, id)
			}
		}
		return out
	case []int64:
		return t
	case string:
		if t == "" {
			return nil
		}
		parts := []rune(t)
		ids := []int64{}
		start := 0
		for i, r := range parts {
			if r == ',' || r == '\n' || r == ' ' || r == '\t' {
				if start < i {
					id, _ := strconv.ParseInt(string(parts[start:i]), 10, 64)
					if id > 0 {
						ids = append(ids, id)
					}
				}
				start = i + 1
			}
		}
		if start < len(parts) {
			id, _ := strconv.ParseInt(string(parts[start:]), 10, 64)
			if id > 0 {
				ids = append(ids, id)
			}
		}
		return ids
	default:
		if id := int64FromAny(v); id > 0 {
			return []int64{id}
		}
		return nil
	}
}

func uniquePositiveIDs(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func int64FromAny(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case float64:
		return int64(t)
	case json.Number:
		id, _ := t.Int64()
		return id
	case string:
		id, _ := strconv.ParseInt(t, 10, 64)
		return id
	default:
		return 0
	}
}

func intFromAny(v any) int {
	return int(int64FromAny(v))
}

func stringSliceFromAny(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	default:
		return nil
	}
}

func boolFromAny(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		b, _ := strconv.ParseBool(t)
		return b
	case json.Number:
		i, _ := t.Int64()
		return i != 0
	case float64:
		return t != 0
	default:
		return false
	}
}

func (h *MyResourceHandler) ListGroups(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	page, err := h.userResourceService.ListGroups(c.Request.Context(), userID, myListOptions(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, page)
}

func (h *MyResourceHandler) GetGroupLiveCapability(c *gin.Context) {
	if _, ok := h.currentUser(c); !ok {
		return
	}
	err := liveattestation.NewProvider().Check(c.Request.Context())
	result := gin.H{"supported": err == nil}
	if err != nil {
		result["reason"] = err.Error()
	}
	response.Success(c, result)
}

func (h *MyResourceHandler) GetGroup(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	item, err := h.userResourceService.GetGroup(c.Request.Context(), userID, id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

func (h *MyResourceHandler) CreateGroup(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	item, err := h.userResourceService.CreateGroup(c.Request.Context(), userID, payload)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, item)
}

func (h *MyResourceHandler) UpdateGroup(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	item, err := h.userResourceService.UpdateGroup(c.Request.Context(), userID, id, payload)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

func (h *MyResourceHandler) DeleteGroup(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	if err := h.userResourceService.DeleteGroup(c.Request.Context(), userID, id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "Group deleted successfully"})
}

func (h *MyResourceHandler) GetGroupPoolHealth(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	if _, err := h.userResourceService.GetGroup(c.Request.Context(), userID, id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	health, err := h.userResourceService.GetPoolHealth(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, health)
}

func (h *MyResourceHandler) GetGroupUsageSummary(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	location := time.UTC
	if timezoneName := strings.TrimSpace(c.Query("timezone")); timezoneName != "" {
		parsed, err := time.LoadLocation(timezoneName)
		if err != nil {
			response.BadRequest(c, "Invalid timezone")
			return
		}
		location = parsed
	}
	now := time.Now().In(location)
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
	result, err := h.userResourceService.ListGroupUsageSummary(c.Request.Context(), userID, todayStart)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *MyResourceHandler) GetGroupCapacitySummary(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	result, err := h.userResourceService.ListGroupCapacitySummary(c.Request.Context(), userID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *MyResourceHandler) GetGroupModelsListCandidates(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	groupID, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	models, err := h.userResourceService.GetGroupModelsListCandidates(c.Request.Context(), userID, groupID, c.Query("platform"))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"models": models})
}

func (h *MyResourceHandler) GetGroupUserOverrides(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	groupID, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	entries, err := h.userResourceService.GetGroupUserOverrides(c.Request.Context(), userID, groupID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, entries)
}

func (h *MyResourceHandler) SetGroupRateMultipliers(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	groupID, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	var input struct {
		Entries []service.GroupRateMultiplierInput `json:"entries"`
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	if err := decodeMyResourcePayload(payload, &input); err != nil {
		response.BadRequest(c, "Invalid rate multiplier request")
		return
	}
	if err := h.userResourceService.SetGroupRateMultipliers(c.Request.Context(), userID, groupID, input.Entries); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "Rate multipliers updated"})
}

func (h *MyResourceHandler) ClearGroupRateMultipliers(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	groupID, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	if err := h.userResourceService.ClearGroupRateMultipliers(c.Request.Context(), userID, groupID); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "Rate multipliers cleared"})
}

func (h *MyResourceHandler) SetGroupRPMOverrides(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	groupID, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	var input struct {
		Entries []service.GroupRPMOverrideInput `json:"entries"`
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	if err := decodeMyResourcePayload(payload, &input); err != nil {
		response.BadRequest(c, "Invalid RPM override request")
		return
	}
	if err := h.userResourceService.SetGroupRPMOverrides(c.Request.Context(), userID, groupID, input.Entries); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "RPM overrides updated"})
}

func (h *MyResourceHandler) ClearGroupRPMOverrides(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	groupID, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	if err := h.userResourceService.ClearGroupRPMOverrides(c.Request.Context(), userID, groupID); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "RPM overrides cleared"})
}

func (h *MyResourceHandler) ListAccounts(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	page, err := h.userResourceService.ListAccounts(c.Request.Context(), userID, myListOptions(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	service.RedactAccountPageForUserResponse(page)
	response.Success(c, page)
}

func (h *MyResourceHandler) GetAccount(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	item, err := h.userResourceService.GetAccount(c.Request.Context(), userID, id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	service.RedactAccountForUserResponse(item)
	response.Success(c, item)
}

func writeMyUpstreamModelSyncError(c *gin.Context, err error) {
	var syncErr *service.UpstreamModelSyncError
	if !errors.As(err, &syncErr) {
		response.ErrorFrom(c, err)
		return
	}
	if syncErr.Kind == service.UpstreamModelSyncErrorConfiguration || syncErr.Kind == service.UpstreamModelSyncErrorUnsupported {
		response.BadRequest(c, syncErr.SafeMessage())
		return
	}
	response.Error(c, http.StatusBadGateway, syncErr.SafeMessage())
}

func (h *MyResourceHandler) SyncAccountUpstreamModels(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	accountID, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	models, err := h.userResourceService.SyncAccountUpstreamModels(c.Request.Context(), userID, accountID)
	if err != nil {
		writeMyUpstreamModelSyncError(c, err)
		return
	}
	response.Success(c, gin.H{"models": models})
}

func (h *MyResourceHandler) SyncAccountUpstreamModelsPreview(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	var input service.UserUpstreamModelsPreviewInput
	if err := decodeMyResourcePayload(payload, &input); err != nil {
		response.BadRequest(c, "Invalid upstream model sync preview request")
		return
	}
	models, err := h.userResourceService.SyncAccountUpstreamModelsPreview(c.Request.Context(), userID, input)
	if err != nil {
		writeMyUpstreamModelSyncError(c, err)
		return
	}
	response.Success(c, gin.H{"models": models})
}

func (h *MyResourceHandler) GetGeminiOAuthCapabilities(c *gin.Context) {
	if _, ok := h.currentUser(c); !ok {
		return
	}
	capabilities, err := h.userResourceService.GetGeminiOAuthCapabilities()
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, capabilities)
}

func (h *MyResourceHandler) GetAntigravityDefaultModelMapping(c *gin.Context) {
	if _, ok := h.currentUser(c); !ok {
		return
	}
	response.Success(c, h.userResourceService.GetAntigravityDefaultModelMapping())
}

func (h *MyResourceHandler) CreateAccount(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	item, err := h.userResourceService.CreateAccount(c.Request.Context(), userID, payload)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	service.RedactAccountForUserResponse(item)
	response.Created(c, item)
}

func (h *MyResourceHandler) UpdateAccount(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	item, err := h.userResourceService.UpdateAccount(c.Request.Context(), userID, id, payload)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	service.RedactAccountForUserResponse(item)
	response.Success(c, item)
}

func (h *MyResourceHandler) DeleteAccount(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	if err := h.userResourceService.DeleteAccount(c.Request.Context(), userID, id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "Account deleted successfully"})
}

func (h *MyResourceHandler) ClearAccountError(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	item, err := h.userResourceService.ClearAccountError(c.Request.Context(), userID, id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	service.RedactAccountForUserResponse(item)
	response.Success(c, item)
}

func (h *MyResourceHandler) SetAccountSchedulable(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	item, err := h.userResourceService.SetAccountSchedulable(c.Request.Context(), userID, id, boolFromAny(payload["schedulable"]))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	service.RedactAccountForUserResponse(item)
	response.Success(c, item)
}

func (h *MyResourceHandler) ExportAccounts(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	result, err := h.userResourceService.ExportAccounts(c.Request.Context(), userID, int64SliceFromAny(c.Query("ids")), boolQuery(c, "include_proxies"))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *MyResourceHandler) ImportAccounts(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	result, err := h.userResourceService.ImportAccounts(c.Request.Context(), userID, payload)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	service.RedactAccountImportResultForUserResponse(result)
	response.Success(c, result)
}

func (h *MyResourceHandler) ImportCodexSessions(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	var input service.UserCodexSessionImportInput
	if err := decodeMyResourcePayload(payload, &input); err != nil {
		response.BadRequest(c, "Invalid Codex session import request")
		return
	}
	result, err := h.userResourceService.ImportCodexSessions(c.Request.Context(), userID, input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	service.RedactAccountImportResultForUserResponse(result)
	response.Success(c, result)
}

func (h *MyResourceHandler) ImportCodexPAT(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	var input service.UserCodexPATImportInput
	if err := decodeMyResourcePayload(payload, &input); err != nil {
		response.BadRequest(c, "Invalid Codex PAT import request")
		return
	}
	item, err := h.userResourceService.ImportCodexPAT(c.Request.Context(), userID, input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	service.RedactAccountForUserResponse(item)
	response.Success(c, item)
}

func (h *MyResourceHandler) GenerateAccountOAuthURL(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	var input service.UserResourceOAuthAuthURLInput
	if err := decodeMyResourcePayload(payload, &input); err != nil {
		response.BadRequest(c, "Invalid OAuth request")
		return
	}
	result, err := h.userResourceService.GenerateAccountOAuthURL(c.Request.Context(), userID, input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *MyResourceHandler) ExchangeAccountOAuthCode(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	var input service.UserResourceOAuthExchangeInput
	if err := decodeMyResourcePayload(payload, &input); err != nil {
		response.BadRequest(c, "Invalid OAuth request")
		return
	}
	result, err := h.userResourceService.ExchangeAccountOAuthCode(c.Request.Context(), userID, input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *MyResourceHandler) ExchangeAccountOAuthCookie(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	var input service.UserResourceOAuthCookieInput
	if err := decodeMyResourcePayload(payload, &input); err != nil {
		response.BadRequest(c, "Invalid OAuth request")
		return
	}
	result, err := h.userResourceService.ExchangeAccountOAuthCookie(c.Request.Context(), userID, input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *MyResourceHandler) BatchUpdateAccounts(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	fields, _ := payload["fields"].(map[string]any)
	if fields == nil {
		fields = map[string]any{}
		for key, value := range payload {
			if key != "ids" {
				fields[key] = value
			}
		}
	}
	result, err := h.userResourceService.BatchUpdateAccounts(c.Request.Context(), userID, int64SliceFromAny(payload["ids"]), fields)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *MyResourceHandler) TestAccount(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	modelID, _ := payload["model_id"].(string)
	result, err := h.userResourceService.TestAccount(c.Request.Context(), userID, id, modelID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *MyResourceHandler) GetAccountTestModels(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	models, err := h.userResourceService.GetAccountTestModels(c.Request.Context(), userID, id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, models)
}

func (h *MyResourceHandler) StreamAccountTest(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	var req struct {
		ModelID      string `json:"model_id"`
		Prompt       string `json:"prompt"`
		Mode         string `json:"mode"`
		ImageDataURL string `json:"image_data_url"`
		AudioDataURL string `json:"audio_data_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && c.Request.ContentLength != 0 {
		response.BadRequest(c, "Invalid test request")
		return
	}
	opts := service.AccountTestOptions{
		ImageDataURL: req.ImageDataURL,
		AudioDataURL: req.AudioDataURL,
	}
	if err := h.userResourceService.StreamAccountTest(c, userID, id, req.ModelID, req.Prompt, req.Mode, opts); err != nil && !c.Writer.Written() {
		response.ErrorFrom(c, err)
	}
}

func (h *MyResourceHandler) RefreshAccount(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	item, err := h.userResourceService.RefreshAccount(c.Request.Context(), userID, id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	service.RedactAccountForUserResponse(item)
	response.Success(c, item)
}

func (h *MyResourceHandler) ListProxies(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	page, err := h.userResourceService.ListProxies(c.Request.Context(), userID, myListOptions(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	service.RedactProxyPageForUserResponse(page)
	response.Success(c, page)
}

func (h *MyResourceHandler) GetProxy(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	item, err := h.userResourceService.GetProxy(c.Request.Context(), userID, id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	service.RedactProxyForUserResponse(item)
	response.Success(c, item)
}

func (h *MyResourceHandler) CreateProxy(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	item, err := h.userResourceService.CreateProxy(c.Request.Context(), userID, payload)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	service.RedactProxyForUserResponse(item)
	response.Created(c, item)
}

func (h *MyResourceHandler) UpdateProxy(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	item, err := h.userResourceService.UpdateProxy(c.Request.Context(), userID, id, payload)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	service.RedactProxyForUserResponse(item)
	response.Success(c, item)
}

func (h *MyResourceHandler) DeleteProxy(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	if err := h.userResourceService.DeleteProxy(c.Request.Context(), userID, id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "Proxy deleted successfully"})
}

func (h *MyResourceHandler) TestProxy(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	result, err := h.userResourceService.TestProxy(c.Request.Context(), userID, id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *MyResourceHandler) ExportProxies(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	result, err := h.userResourceService.ExportProxies(c.Request.Context(), userID, int64SliceFromAny(c.Query("ids")))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *MyResourceHandler) QualityCheckProxy(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	result, err := h.userResourceService.QualityCheckProxy(c.Request.Context(), userID, id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *MyResourceHandler) ListProxySources(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	page, err := h.userResourceService.ListProxySources(c.Request.Context(), userID, myListOptions(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, page)
}

func (h *MyResourceHandler) CreateProxySource(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	item, err := h.userResourceService.CreateProxySource(c.Request.Context(), userID, payload)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, item)
}

func (h *MyResourceHandler) UpdateProxySource(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	item, err := h.userResourceService.UpdateProxySource(c.Request.Context(), userID, id, payload)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

func (h *MyResourceHandler) DeleteProxySource(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	if err := h.userResourceService.DeleteProxySource(c.Request.Context(), userID, id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "Proxy source deleted successfully"})
}

func (h *MyResourceHandler) SyncProxySource(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	result, err := h.userResourceService.SyncProxySource(c.Request.Context(), userID, id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	service.RedactProxySourceSyncResultForUserResponse(result)
	response.Success(c, result)
}

// SyncAllProxySources refreshes every source the caller owns. The aggregate
// result carries counts only, so there is nothing node-shaped left to redact.
func (h *MyResourceHandler) SyncAllProxySources(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	result, err := h.userResourceService.SyncAllProxySources(c.Request.Context(), userID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *MyResourceHandler) ImportProxyNodes(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	isPublic := false
	if rawVisibility, exists := payload["is_public"]; exists {
		var valid bool
		isPublic, valid = rawVisibility.(bool)
		if !valid {
			response.BadRequest(c, "Invalid request: is_public must be a boolean")
			return
		}
	}
	result, err := h.userResourceService.ImportProxyNodes(
		c.Request.Context(),
		userID,
		stringValue(payload["name_prefix"]),
		stringValue(payload["content"]),
		isPublic,
	)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	service.RedactProxyImportResultForUserResponse(result)
	response.Success(c, result)
}

func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func (h *MyResourceHandler) ListRedeemCodes(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	page, err := h.userResourceService.ListRedeemCodes(c.Request.Context(), userID, myListOptions(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, page)
}

func (h *MyResourceHandler) ListRedeemCodeUsages(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	page, err := h.userResourceService.ListRedeemCodeUsages(c.Request.Context(), userID, id, myListOptions(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, page)
}

func (h *MyResourceHandler) GenerateRedeemCodes(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	items, err := h.userResourceService.GenerateRedeemCodes(c.Request.Context(), userID, payload)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, items)
}

func (h *MyResourceHandler) DeleteRedeemCode(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	if err := h.userResourceService.DeleteRedeemCode(c.Request.Context(), userID, id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "Redeem code deleted successfully"})
}

func (h *MyResourceHandler) ExpireRedeemCode(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	item, err := h.userResourceService.ExpireRedeemCode(c.Request.Context(), userID, id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

func (h *MyResourceHandler) BatchDeleteRedeemCodes(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	ids := uniquePositiveIDs(int64SliceFromAny(payload["ids"]))
	if len(ids) > maxMyResourceBatchIDs {
		response.BadRequest(c, "Too many redeem code IDs; maximum is 1000")
		return
	}
	var deleted int
	for _, id := range ids {
		if err := h.userResourceService.DeleteRedeemCode(c.Request.Context(), userID, id); err == nil {
			deleted++
		}
	}
	response.Success(c, gin.H{"deleted": deleted})
}

func (h *MyResourceHandler) BatchExpireRedeemCodes(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	ids := uniquePositiveIDs(int64SliceFromAny(payload["ids"]))
	if len(ids) > maxMyResourceBatchIDs {
		response.BadRequest(c, "Too many redeem code IDs; maximum is 1000")
		return
	}
	var updated int
	for _, id := range ids {
		if _, err := h.userResourceService.ExpireRedeemCode(c.Request.Context(), userID, id); err == nil {
			updated++
		}
	}
	response.Success(c, gin.H{"updated": updated})
}

func (h *MyResourceHandler) RedeemCodeStats(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	stats, err := h.userResourceService.RedeemCodeStats(c.Request.Context(), userID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, stats)
}

func (h *MyResourceHandler) ExportRedeemCodes(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	items, err := h.userResourceService.ExportRedeemCodes(c.Request.Context(), userID, myListOptions(c), int64SliceFromAny(c.Query("ids")))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	writeRedeemCSV(c, items)
}

func (h *MyResourceHandler) BatchUpdateRedeemCodes(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	fields, _ := payload["fields"].(map[string]any)
	if fields == nil {
		fields = map[string]any{}
		for key, value := range payload {
			if key != "ids" {
				fields[key] = value
			}
		}
	}
	result, err := h.userResourceService.BatchUpdateRedeemCodes(c.Request.Context(), userID, int64SliceFromAny(payload["ids"]), fields)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *MyResourceHandler) ListAssignedSubscriptions(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	page, err := h.userResourceService.ListAssignedSubscriptions(c.Request.Context(), userID, myListOptions(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, page)
}

func (h *MyResourceHandler) AssignSubscription(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	item, err := h.userResourceService.AssignSubscription(c.Request.Context(), userID, service.UserSubscriptionAssignInput{
		UserID:       int64FromAny(payload["user_id"]),
		Email:        stringValue(payload["email"]),
		GroupID:      int64FromAny(payload["group_id"]),
		ValidityDays: intFromAny(payload["validity_days"]),
		Notes:        stringValue(payload["notes"]),
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, item)
}

func (h *MyResourceHandler) BulkAssignSubscription(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	result, err := h.userResourceService.BulkAssignSubscription(c.Request.Context(), userID, service.UserSubscriptionBulkAssignInput{
		UserIDs:      int64SliceFromAny(payload["user_ids"]),
		Emails:       stringSliceFromAny(payload["emails"]),
		GroupID:      int64FromAny(payload["group_id"]),
		ValidityDays: intFromAny(payload["validity_days"]),
		Notes:        stringValue(payload["notes"]),
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *MyResourceHandler) ExtendAssignedSubscription(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	payload, ok := bindJSONMap(c)
	if !ok {
		return
	}
	item, err := h.userResourceService.ExtendAssignedSubscription(c.Request.Context(), userID, id, intFromAny(payload["days"]))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

func (h *MyResourceHandler) RevokeAssignedSubscription(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	if err := h.userResourceService.RevokeAssignedSubscription(c.Request.Context(), userID, id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "Subscription revoked successfully"})
}

func (h *MyResourceHandler) RestoreAssignedSubscription(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	item, err := h.userResourceService.RestoreAssignedSubscription(c.Request.Context(), userID, id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

func (h *MyResourceHandler) ResetAssignedSubscriptionUsage(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	item, err := h.userResourceService.ResetAssignedSubscriptionUsage(c.Request.Context(), userID, id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

func (h *MyResourceHandler) ListAccountUsageLogs(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	page, err := h.userResourceService.ListAccountUsageLogs(c.Request.Context(), userID, myListOptions(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, page)
}

func (h *MyResourceHandler) GetAccountUsageStats(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	stats, err := h.userResourceService.GetAccountUsageStats(c.Request.Context(), userID, myListOptions(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, stats)
}

func (h *MyResourceHandler) ExportAccountUsageLogs(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	opts := myListOptions(c)
	opts.Page = 1
	opts.PageSize = 1000
	items := make([]map[string]any, 0, 1000)
	for {
		page, err := h.userResourceService.ListAccountUsageLogs(c.Request.Context(), userID, opts)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		if page.Total > 10000 {
			response.BadRequest(c, "Export exceeds 10000 rows; narrow the date range")
			return
		}
		items = append(items, page.Items...)
		if opts.Page >= page.Pages {
			break
		}
		opts.Page++
	}
	writeAccountUsageCSV(c, items)
}

func (h *MyResourceHandler) ListUpstreamErrors(c *gin.Context) {
	userID, ok := h.currentUser(c)
	if !ok {
		return
	}
	page, err := h.userResourceService.ListUpstreamErrors(c.Request.Context(), userID, myListOptions(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, page)
}

func writeRedeemCSV(c *gin.Context, items []map[string]any) {
	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)
	headers := []string{"id", "code", "type", "status", "group_id", "group_name", "validity_days", "expires_at", "used_by", "used_by_email", "used_at", "notes", "created_at"}
	_ = writer.Write(headers)
	for _, item := range items {
		row := make([]string, 0, len(headers))
		for _, key := range headers {
			row = append(row, csvSafeValue(item[key]))
		}
		_ = writer.Write(row)
	}
	writer.Flush()
	filename := "my-redeem-codes-" + time.Now().UTC().Format("20060102T150405Z") + ".csv"
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Data(http.StatusOK, "text/csv; charset=utf-8", buf.Bytes())
}

func writeAccountUsageCSV(c *gin.Context, items []map[string]any) {
	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)
	headers := []string{
		"id", "created_at", "request_id", "user_id", "api_key_id", "account_id", "account_name",
		"group_id", "group_name", "model", "requested_model", "upstream_model", "input_tokens",
		"output_tokens", "cache_creation_tokens", "cache_read_tokens", "total_cost", "actual_cost",
		"rate_multiplier", "account_rate_multiplier", "billing_type", "stream", "duration_ms", "first_token_ms",
	}
	_ = writer.Write(headers)
	for _, item := range items {
		row := make([]string, 0, len(headers))
		for _, key := range headers {
			row = append(row, csvSafeValue(item[key]))
		}
		_ = writer.Write(row)
	}
	writer.Flush()
	filename := "my-account-usage-" + time.Now().UTC().Format("20060102T150405Z") + ".csv"
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Data(http.StatusOK, "text/csv; charset=utf-8", buf.Bytes())
}

func csvSafeValue(v any) string {
	if v == nil {
		return ""
	}
	var out string
	switch t := v.(type) {
	case time.Time:
		out = t.UTC().Format(time.RFC3339)
	default:
		out = formatCSVScalar(t)
	}
	if out != "" && strings.ContainsAny(out[:1], "=+-@") {
		return "'" + out
	}
	return out
}

func formatCSVScalar(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		raw, err := json.Marshal(t)
		if err == nil && (strings.HasPrefix(string(raw), "{") || strings.HasPrefix(string(raw), "[")) {
			return string(raw)
		}
		if err == nil && strings.HasPrefix(string(raw), `"`) {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				return s
			}
		}
		return strings.Trim(string(raw), `"`)
	}
}

func (h *MyResourceHandler) FeatureStatus(c *gin.Context) {
	enabled := h.settingService != nil && h.settingService.IsUserResourcesEnabled(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": gin.H{"enabled": enabled}})
}
