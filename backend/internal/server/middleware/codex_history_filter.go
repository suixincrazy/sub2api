package middleware

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/codexhistory"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const codexHistoryFilteredKey = "codex_history_filtered"

type CodexHistoryFilter struct {
	settings *service.SettingService
}

func NewCodexHistoryFilter(settings *service.SettingService) *CodexHistoryFilter {
	return &CodexHistoryFilter{settings: settings}
}

// Prepare bounds and decodes the original body before model allowlist checks.
// Apply runs after those checks so JSON normalization cannot hide duplicate models.
func (f *CodexHistoryFilter) Prepare(c *gin.Context) {
	path := strings.TrimRight(c.Request.URL.Path, "/")
	responses := strings.HasSuffix(path, "/responses")
	compact := strings.HasSuffix(path, "/responses/compact")
	if (!responses && !compact) || (c.Request.Method != http.MethodPost && c.Request.Method != http.MethodGet) {
		c.Next()
		return
	}
	enabled, err := f.settings.CodexHistoryFilterEnabled(c.Request.Context())
	if err != nil {
		f.reject(c, &codexhistory.Error{Code: "filter_settings_unavailable", Message: "Codex history filter settings are temporarily unavailable.", Status: http.StatusServiceUnavailable})
		return
	}
	if !enabled {
		c.Next()
		return
	}
	if compact {
		f.reject(c, &codexhistory.Error{Code: "encrypted_compaction_disabled", Message: "Encrypted Responses compaction is disabled. Use a text summary and start a new session.", Status: http.StatusConflict})
		return
	}
	if c.Request.Method == http.MethodGet {
		f.reject(c, &codexhistory.Error{Code: "websocket_filtering_unsupported", Message: "Use Responses over HTTP/SSE; WebSocket filtering is not supported.", Status: http.StatusUpgradeRequired})
		return
	}
	if c.GetHeader("Origin") != "" || c.GetHeader("Sec-Fetch-Site") != "" {
		f.reject(c, &codexhistory.Error{Code: "browser_request_rejected", Message: "Browser Responses requests are not supported while the Codex history filter is enabled.", Status: http.StatusForbidden})
		return
	}
	encoding := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Encoding")))
	switch encoding {
	case "", "identity", "gzip", "x-gzip", "deflate", "br", "zstd":
	default:
		f.reject(c, &codexhistory.Error{Code: "unsupported_content_encoding", Message: "Unsupported request Content-Encoding.", Status: http.StatusUnsupportedMediaType})
		return
	}
	if c.Request.ContentLength > codexhistory.MaxBodySize {
		f.reject(c, &codexhistory.Error{Code: "request_too_large", Message: "Request exceeds filter size limit.", Status: http.StatusRequestEntityTooLarge})
		return
	}
	if c.Request.Body != nil {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, codexhistory.MaxBodySize)
	}
	body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			f.reject(c, &codexhistory.Error{Code: "request_too_large", Message: "Request exceeds filter size limit.", Status: http.StatusRequestEntityTooLarge})
		} else if encoding != "" && encoding != "identity" {
			f.reject(c, codexhistory.Invalid("invalid_compressed_request", "Cannot decode compressed request."))
		} else {
			f.reject(c, codexhistory.Invalid("invalid_request_json", "Cannot read Responses request."))
		}
		return
	}
	if int64(len(body)) > codexhistory.MaxBodySize {
		f.reject(c, &codexhistory.Error{Code: "request_too_large", Message: "Decoded request exceeds filter size limit.", Status: http.StatusRequestEntityTooLarge})
		return
	}
	requestmodel.ResetRequestBody(c.Request, body)
	c.Request = c.Request.WithContext(codexhistory.WithEnabled(c.Request.Context()))
	c.Next()
	if filtered, _ := c.Get(codexHistoryFilteredKey); filtered == true {
		transportError := false
		if events, ok := c.Get(service.OpsUpstreamErrorsKey); ok {
			if entries, ok := events.([]*service.OpsUpstreamErrorEvent); ok {
				for _, entry := range entries {
					transportError = transportError || (entry != nil && entry.Kind == "request_error" && entry.UpstreamStatusCode == 0)
				}
			}
		}
		f.settings.RecordCodexHistoryResponse(c.Writer.Status(), transportError)
	}
}

func (f *CodexHistoryFilter) Apply(c *gin.Context) {
	if !codexhistory.Enabled(c.Request.Context()) {
		c.Next()
		return
	}
	body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		f.reject(c, codexhistory.Invalid("invalid_request_json", "Cannot read Responses request."))
		return
	}
	result, err := codexhistory.Filter(body)
	if err != nil {
		var filterError *codexhistory.Error
		if errors.As(err, &filterError) {
			f.reject(c, filterError)
		} else {
			f.reject(c, &codexhistory.Error{Code: "local_filter_error", Message: "Cannot filter Responses request.", Status: http.StatusInternalServerError})
		}
		return
	}
	requestmodel.ResetRequestBody(c.Request, result.Body)
	c.Request.Header.Del("Content-Encoding")
	c.Request.Header.Del("Expect")
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.TransferEncoding = nil
	f.settings.RecordCodexHistoryFiltered(result.RemovedReasoningItems, result.RemovedItemIDs)
	c.Set(codexHistoryFilteredKey, true)
	slog.InfoContext(c.Request.Context(), "codex_history_filtered", "removed_reasoning_items", result.RemovedReasoningItems, "removed_item_ids", result.RemovedItemIDs)
	c.Next()
}

func (f *CodexHistoryFilter) reject(c *gin.Context, err *codexhistory.Error) {
	if f.settings != nil {
		f.settings.RecordCodexHistoryBlocked()
	}
	service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalPolicyDenied)
	slog.WarnContext(c.Request.Context(), "codex_history_filter_blocked", "code", err.Code)
	c.AbortWithStatusJSON(err.Status, gin.H{"error": gin.H{"type": "invalid_request_error", "code": err.Code, "message": err.Message}})
}
