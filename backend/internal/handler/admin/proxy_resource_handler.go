package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

const maxAdminProxyJSONBodyBytes int64 = 8 << 20

const (
	invalidAdminProxyImportContentMessage    = "Invalid request: content must be a non-empty string"
	invalidAdminProxyImportNamePrefixMessage = "Invalid request: name_prefix must be a string"
	invalidAdminProxyImportIsPublicMessage   = "Invalid request: is_public must be a boolean"
)

type adminProxyImportRequest struct {
	namePrefix string
	content    string
	isPublic   bool
}

func bindAdminProxyJSONMap(c *gin.Context) (map[string]any, bool) {
	if c.Request == nil || c.Request.Body == nil {
		response.BadRequest(c, "Invalid request: JSON object is required")
		return nil, false
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAdminProxyJSONBodyBytes)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.UseNumber()

	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return nil, false
	}
	if payload == nil {
		response.BadRequest(c, "Invalid request: JSON object is required")
		return nil, false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		response.BadRequest(c, "Invalid request: only one JSON object is allowed")
		return nil, false
	}
	return payload, true
}

func parseAdminProxyImportRequest(c *gin.Context, payload map[string]any) (adminProxyImportRequest, bool) {
	rawContent, exists := payload["content"]
	content, valid := rawContent.(string)
	if !exists || !valid || strings.TrimSpace(content) == "" {
		response.BadRequest(c, invalidAdminProxyImportContentMessage)
		return adminProxyImportRequest{}, false
	}

	namePrefix := ""
	if rawNamePrefix, exists := payload["name_prefix"]; exists {
		namePrefix, valid = rawNamePrefix.(string)
		if !valid {
			response.BadRequest(c, invalidAdminProxyImportNamePrefixMessage)
			return adminProxyImportRequest{}, false
		}
	}

	isPublic := false
	if rawVisibility, exists := payload["is_public"]; exists {
		isPublic, valid = rawVisibility.(bool)
		if !valid {
			response.BadRequest(c, invalidAdminProxyImportIsPublicMessage)
			return adminProxyImportRequest{}, false
		}
	}

	return adminProxyImportRequest{
		namePrefix: namePrefix,
		content:    content,
		isPublic:   isPublic,
	}, true
}

func adminProxySourceListOptions(c *gin.Context) service.UserResourceListOptions {
	page, pageSize := response.ParsePagination(c)
	return service.UserResourceListOptions{
		Page:      page,
		PageSize:  pageSize,
		Search:    c.Query("search"),
		Status:    c.Query("status"),
		SortBy:    c.Query("sort_by"),
		SortOrder: c.Query("sort_order"),
	}
}

func parseAdminProxySourceID(c *gin.Context) (int64, bool) {
	sourceID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || sourceID <= 0 {
		response.BadRequest(c, "Invalid proxy source ID")
		return 0, false
	}
	return sourceID, true
}

func (h *ProxyHandler) systemProxyResourceService(c *gin.Context) (*service.UserResourceService, bool) {
	if h.userResourceService == nil {
		response.InternalError(c, "System proxy resource service is not available")
		return nil, false
	}
	return h.userResourceService, true
}

// ImportProxyNodes imports standard proxies, modern share links, or proxy client configuration.
// POST /api/v1/admin/proxies/import
func (h *ProxyHandler) ImportProxyNodes(c *gin.Context) {
	resourceService, ok := h.systemProxyResourceService(c)
	if !ok {
		return
	}
	payload, ok := bindAdminProxyJSONMap(c)
	if !ok {
		return
	}
	request, ok := parseAdminProxyImportRequest(c, payload)
	if !ok {
		return
	}

	result, err := resourceService.ImportSystemProxyNodes(
		c.Request.Context(),
		request.namePrefix,
		request.content,
		request.isPublic,
	)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	service.RedactProxyImportResultForUserResponse(result)
	response.Success(c, result)
}

// ListProxySources lists system-owned proxy subscription sources.
// GET /api/v1/admin/proxies/sources
func (h *ProxyHandler) ListProxySources(c *gin.Context) {
	resourceService, ok := h.systemProxyResourceService(c)
	if !ok {
		return
	}
	page, err := resourceService.ListSystemProxySources(c.Request.Context(), adminProxySourceListOptions(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, page)
}

// CreateProxySource creates a system-owned proxy subscription source.
// POST /api/v1/admin/proxies/sources
func (h *ProxyHandler) CreateProxySource(c *gin.Context) {
	resourceService, ok := h.systemProxyResourceService(c)
	if !ok {
		return
	}
	payload, ok := bindAdminProxyJSONMap(c)
	if !ok {
		return
	}
	item, err := resourceService.CreateSystemProxySource(c.Request.Context(), payload)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, item)
}

// UpdateProxySource updates a system-owned proxy subscription source.
// PUT /api/v1/admin/proxies/sources/:id
func (h *ProxyHandler) UpdateProxySource(c *gin.Context) {
	resourceService, ok := h.systemProxyResourceService(c)
	if !ok {
		return
	}
	sourceID, ok := parseAdminProxySourceID(c)
	if !ok {
		return
	}
	payload, ok := bindAdminProxyJSONMap(c)
	if !ok {
		return
	}
	item, err := resourceService.UpdateSystemProxySource(c.Request.Context(), sourceID, payload)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

// DeleteProxySource deletes a system-owned proxy subscription source.
// DELETE /api/v1/admin/proxies/sources/:id
func (h *ProxyHandler) DeleteProxySource(c *gin.Context) {
	resourceService, ok := h.systemProxyResourceService(c)
	if !ok {
		return
	}
	sourceID, ok := parseAdminProxySourceID(c)
	if !ok {
		return
	}
	if err := resourceService.DeleteSystemProxySource(c.Request.Context(), sourceID); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "Proxy source deleted successfully"})
}

// SyncProxySource refreshes a system-owned proxy subscription source.
// POST /api/v1/admin/proxies/sources/:id/sync
func (h *ProxyHandler) SyncProxySource(c *gin.Context) {
	resourceService, ok := h.systemProxyResourceService(c)
	if !ok {
		return
	}
	sourceID, ok := parseAdminProxySourceID(c)
	if !ok {
		return
	}
	result, err := resourceService.SyncSystemProxySource(c.Request.Context(), sourceID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	service.RedactProxySourceSyncResultForUserResponse(result)
	response.Success(c, result)
}

// SyncAllProxySources refreshes every system-owned proxy subscription source.
// POST /api/v1/admin/proxies/sources/sync-all
func (h *ProxyHandler) SyncAllProxySources(c *gin.Context) {
	resourceService, ok := h.systemProxyResourceService(c)
	if !ok {
		return
	}
	result, err := resourceService.SyncAllSystemProxySources(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}
