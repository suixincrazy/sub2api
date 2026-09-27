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

// OpenAI requests retain the original body until account selection. Other
// providers are decoded here and filtered after model admission and routing.
func (f *CodexHistoryFilter) Prepare(c *gin.Context) {
	route := c.FullPath()
	if !strings.HasSuffix(route, "/responses") && !strings.Contains(route, "/responses/") {
		c.Next()
		return
	}
	suffix, valid := service.OpenAIResponsesRequestPathSuffix(c)
	responses := valid && suffix == ""
	compact := valid && (suffix == "/compact" || strings.HasPrefix(suffix, "/compact/"))
	if (!responses && !compact) || (c.Request.Method != http.MethodPost && c.Request.Method != http.MethodGet) {
		c.Next()
		return
	}
	enabled, err := f.settings.CodexHistoryFilterEnabled(c.Request.Context())
	if err != nil {
		f.reject(c, &codexhistory.Error{Code: "filter_settings_unavailable", Message: "Codex history filter settings are temporarily unavailable.", Status: http.StatusServiceUnavailable})
		return
	}
	if codexHistoryNeedsAccountPolicy(c) {
		state := &service.CodexHistoryFilterRequest{DefaultEnabled: enabled, PolicyError: service.CodexHistoryFilterTransportError(c, compact)}
		c.Set(service.CodexHistoryFilterRequestKey, state)
		c.Next()
		if state.Blocked {
			f.settings.RecordCodexHistoryBlocked()
		}
		if state.Filtered != nil {
			f.recordFiltered(c, *state.Filtered)
		}
		f.recordResponse(c)
		return
	}
	if !enabled {
		c.Next()
		return
	}
	if !f.prepareEnabledRequest(c, compact) {
		return
	}
	c.Next()
	f.recordResponse(c)
}

func (f *CodexHistoryFilter) prepareEnabledRequest(c *gin.Context, compact bool) bool {
	if policyError := service.CodexHistoryFilterTransportError(c, compact); policyError != nil {
		f.reject(c, policyError)
		return false
	}
	encoding := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Encoding")))
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
		return false
	}
	if int64(len(body)) > codexhistory.MaxBodySize {
		f.reject(c, &codexhistory.Error{Code: "request_too_large", Message: "Decoded request exceeds filter size limit.", Status: http.StatusRequestEntityTooLarge})
		return false
	}
	requestmodel.ResetRequestBody(c.Request, body)
	c.Request = c.Request.WithContext(codexhistory.WithEnabled(c.Request.Context()))
	return true
}

func (f *CodexHistoryFilter) recordResponse(c *gin.Context) {
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
	if state := service.CodexHistoryFilterRequestFromContext(c); state != nil {
		if codexHistoryNeedsAccountPolicy(c) || !state.DefaultEnabled {
			c.Next()
			return
		}
		// Composite routing selected another provider; apply the gateway
		// default before that provider converts its Responses request.
		if state.PolicyError != nil {
			f.reject(c, state.PolicyError)
			return
		}
		if !f.prepareEnabledRequest(c, false) {
			return
		}
		c.Set(service.CodexHistoryFilterRequestKey, nil)
	}
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
	f.recordFiltered(c, result)
	c.Next()
}

func (f *CodexHistoryFilter) recordFiltered(c *gin.Context, result codexhistory.Result) {
	f.settings.RecordCodexHistoryFiltered(result.RemovedReasoningItems, result.RemovedItemIDs, result.NormalizedAgentTextParts)
	c.Set(codexHistoryFilteredKey, true)
	slog.InfoContext(c.Request.Context(), "codex_history_filtered", "removed_reasoning_items", result.RemovedReasoningItems, "removed_item_ids", result.RemovedItemIDs, "normalized_agent_text_parts", result.NormalizedAgentTextParts)
}

func codexHistoryNeedsAccountPolicy(c *gin.Context) bool {
	apiKey, ok := GetAPIKeyFromContext(c)
	if !ok || apiKey == nil || apiKey.Group == nil {
		return false
	}
	platform := apiKey.Group.Platform
	if platform == service.PlatformComposite {
		if target, resolved := service.ResolvedTargetPlatformFromContext(c.Request.Context()); resolved {
			platform = target
		}
	}
	return platform == service.PlatformOpenAI || platform == service.PlatformComposite
}

func (f *CodexHistoryFilter) reject(c *gin.Context, err *codexhistory.Error) {
	if f.settings != nil {
		f.settings.RecordCodexHistoryBlocked()
	}
	service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalPolicyDenied)
	slog.WarnContext(c.Request.Context(), "codex_history_filter_blocked", "code", err.Code)
	c.AbortWithStatusJSON(err.Status, gin.H{"error": gin.H{"type": "invalid_request_error", "code": err.Code, "message": err.Message}})
}
