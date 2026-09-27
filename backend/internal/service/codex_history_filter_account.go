package service

import (
	"context"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/codexhistory"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
)

const CodexHistoryFilterAccountExtraKey = "codex_history_filter_enabled"
const CodexHistoryFilterRequestKey = "codex_history_filter_request"

// Account selection must happen before filtering so retries with a different
// account can independently apply its override to the original request.
type CodexHistoryFilterRequest struct {
	DefaultEnabled bool
	PolicyError    *codexhistory.Error
	Filtered       *codexhistory.Result
	Blocked        bool
}

func CodexHistoryFilterRequestFromContext(c *gin.Context) *CodexHistoryFilterRequest {
	if c == nil {
		return nil
	}
	value, _ := c.Get(CodexHistoryFilterRequestKey)
	state, _ := value.(*CodexHistoryFilterRequest)
	return state
}

func CodexHistoryFilterEnabledForAccount(ctx context.Context, c *gin.Context, account *Account) bool {
	enabled := codexhistory.Enabled(ctx)
	if state := CodexHistoryFilterRequestFromContext(c); state != nil {
		enabled = state.DefaultEnabled
	}
	if account != nil && account.Platform == PlatformOpenAI {
		if override, ok := account.Extra[CodexHistoryFilterAccountExtraKey].(bool); ok {
			return override
		}
	}
	return enabled
}

func ValidateCodexHistoryFilterAccountExtra(platform string, extra map[string]any) error {
	if platform != PlatformOpenAI || extra[CodexHistoryFilterAccountExtraKey] == nil {
		return nil
	}
	if _, ok := extra[CodexHistoryFilterAccountExtraKey].(bool); !ok {
		return infraerrors.BadRequest("INVALID_CODEX_HISTORY_FILTER_ENABLED", "codex_history_filter_enabled must be a boolean or null")
	}
	return nil
}

func CodexHistoryFilterTransportError(c *gin.Context, compact bool) *codexhistory.Error {
	if compact {
		return &codexhistory.Error{Code: "encrypted_compaction_disabled", Message: "Encrypted Responses compaction is disabled. Use a text summary and start a new session.", Status: http.StatusConflict}
	}
	if c.Request.Method == http.MethodGet {
		return &codexhistory.Error{Code: "websocket_filtering_unsupported", Message: "Use Responses over HTTP/SSE; WebSocket filtering is not supported.", Status: http.StatusUpgradeRequired}
	}
	if c.GetHeader("Origin") != "" || c.GetHeader("Sec-Fetch-Site") != "" {
		return &codexhistory.Error{Code: "browser_request_rejected", Message: "Browser Responses requests are not supported while the Codex history filter is enabled.", Status: http.StatusForbidden}
	}
	switch strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Encoding"))) {
	case "", "identity", "gzip", "x-gzip", "deflate", "br", "zstd":
	default:
		return &codexhistory.Error{Code: "unsupported_content_encoding", Message: "Unsupported request Content-Encoding.", Status: http.StatusUnsupportedMediaType}
	}
	if c.Request.ContentLength > codexhistory.MaxBodySize {
		return &codexhistory.Error{Code: "request_too_large", Message: "Request exceeds filter size limit.", Status: http.StatusRequestEntityTooLarge}
	}
	return nil
}

func ApplyCodexHistoryFilterForAccount(ctx context.Context, c *gin.Context, account *Account, body []byte) (context.Context, []byte, error) {
	state := CodexHistoryFilterRequestFromContext(c)
	// Other protocols converted internally to Responses are outside this filter.
	if state == nil && !codexhistory.Enabled(ctx) {
		return ctx, body, nil
	}
	ctx = codexhistory.WithFilterEnabled(ctx, CodexHistoryFilterEnabledForAccount(ctx, c, account))
	if !codexhistory.Enabled(ctx) {
		return ctx, body, nil
	}
	if state != nil && state.PolicyError != nil {
		return ctx, nil, state.PolicyError
	}
	if int64(len(body)) > codexhistory.MaxBodySize {
		return ctx, nil, &codexhistory.Error{Code: "request_too_large", Message: "Decoded request exceeds filter size limit.", Status: http.StatusRequestEntityTooLarge}
	}
	result, err := codexhistory.Filter(body)
	if err != nil {
		return ctx, nil, err
	}
	if state != nil && state.Filtered == nil {
		state.Filtered = &codexhistory.Result{
			RemovedReasoningItems:    result.RemovedReasoningItems,
			RemovedItemIDs:           result.RemovedItemIDs,
			NormalizedAgentTextParts: result.NormalizedAgentTextParts,
		}
	}
	return ctx, result.Body, nil
}

func MarkCodexHistoryFilterBlocked(c *gin.Context) {
	if state := CodexHistoryFilterRequestFromContext(c); state != nil {
		state.Blocked = true
	}
	MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
}
