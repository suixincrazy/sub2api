package service

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

var openAIRejectionHTMLTags = regexp.MustCompile(`<[^>]*>`)

func isOpenAIGenericRejectionText(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "", "upstream rejected the request", "upstream rejected request", "bad request", "400 bad request":
		return true
	default:
		return false
	}
}

// Recognize the observed ALB default page, not HTML 400s with real diagnostics.
func isOpenAIALBRejectionPage(body []byte) bool {
	text := strings.ToLower(strings.TrimSpace(string(body)))
	if !strings.Contains(text, "<html") || !strings.Contains(text, "<center>alb</center>") {
		return false
	}
	plain := strings.Join(strings.Fields(openAIRejectionHTMLTags.ReplaceAllString(text, " ")), " ")
	return plain == "400 bad request 400 bad request alb"
}

// Only uninformative refusals enter the 429 budget. An explicit code, parameter,
// authentication type or diagnostic message retains its existing handling.
func isOpenAIGenericUpstreamRejection(statusCode int, upstreamMsg string, body []byte) bool {
	if statusCode != http.StatusBadRequest || !isOpenAIGenericRejectionText(upstreamMsg) {
		return false
	}
	if !gjson.ValidBytes(body) {
		return isOpenAIGenericRejectionText(string(body)) || isOpenAIALBRejectionPage(body)
	}
	for _, path := range []string{"error.param", "response.error.param", "param", "error.code", "response.error.code", "code"} {
		if strings.TrimSpace(gjson.GetBytes(body, path).String()) != "" {
			return false
		}
	}
	for _, path := range []string{"error.type", "response.error.type", "type"} {
		switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, path).String())) {
		case "", "error", "response.failed", "invalid_request_error", "upstream_error", "api_error", "server_error":
		default:
			return false
		}
	}
	for _, path := range []string{"error.status_code", "response.error.status_code", "status_code"} {
		if status := gjson.GetBytes(body, path).Int(); status != 0 && status != http.StatusBadRequest {
			return false
		}
	}
	for _, path := range []string{"error.message", "response.error.message", "message", "detail"} {
		if !isOpenAIGenericRejectionText(gjson.GetBytes(body, path).String()) {
			return false
		}
	}
	return true
}

func isOpenAIGenericStreamRejection(payload []byte, message string) bool {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return false
	}
	status := openAIStreamFailedEventSemanticStatus(payload, message)
	if status != http.StatusBadRequest && !strings.EqualFold(strings.TrimSpace(extractOpenAISSEErrorMessage(payload)), openAIUpstreamClientErrorFallbackMessage) {
		return false
	}
	return isOpenAIGenericUpstreamRejection(http.StatusBadRequest, message, payload)
}

func openAIAccountStreamRateLimit(account *Account, payload []byte, message string) bool {
	return openAIStreamFailedEventSemanticStatus(payload, message) == http.StatusTooManyRequests ||
		(account != nil && account.Platform == PlatformOpenAI && isOpenAIGenericStreamRejection(payload, message))
}

func openAIAccountStreamShouldFailover(account *Account, payload []byte, message, eventType string) bool {
	if account != nil && account.Platform == PlatformOpenAI && isOpenAIGenericStreamRejection(payload, message) {
		return true
	}
	if eventType == "error" {
		return openAIStreamErrorEventShouldFailover(payload, message)
	}
	return openAIStreamFailedEventShouldFailover(payload, message)
}

// Classify before custom 400 health policies or downstream writes, but retain
// the actual upstream status/body in ops logs for every failed attempt.
func (s *OpenAIGatewayService) retryOpenAIRejectedHTTPResponse(ctx context.Context, c *gin.Context, account *Account, resp *http.Response, body []byte, message, model string) *UpstreamFailoverError {
	if !isOpenAIGenericUpstreamRejection(resp.StatusCode, message, body) || IsResponseCommitted(c) {
		return nil
	}
	failure := s.newOpenAIGenericRejectionRetryError(ctx, account, resp.Header, body, model)
	if failure == nil {
		return nil
	}
	detail := ""
	if s != nil && s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		detail = truncateString(string(s.redactAgentIdentitySensitiveBody(ctx, account, body)), maxBytes)
	}
	message = sanitizeUpstreamErrorMessage(message)
	if message == "" {
		message = openAIUpstreamClientErrorFallbackMessage
	}
	setOpsUpstreamError(c, resp.StatusCode, message, detail)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account),
		Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
		UpstreamStatusCode: resp.StatusCode, UpstreamRequestID: resp.Header.Get("x-request-id"),
		Kind: "failover", Message: message, Detail: detail,
	})
	return failure
}

// A scope owns the six retries and postpones account cooling until exhaustion.
// Direct callers without that scope retain the original client-error behavior.
func (s *OpenAIGatewayService) newOpenAIGenericRejectionRetryError(ctx context.Context, account *Account, responseHeaders http.Header, responseBody []byte, canonicalModel ...string) *UpstreamFailoverError {
	if account == nil || account.Platform != PlatformOpenAI || upstream429State(ctx, account) == nil {
		return nil
	}
	headers := responseHeaders.Clone()
	deferUpstream429SideEffects(ctx, account, http.StatusTooManyRequests, headers, func(ctx context.Context) {
		if s != nil {
			s.handleOpenAIAccountUpstreamError(ctx, account, http.StatusTooManyRequests, headers, responseBody, canonicalModel...)
		}
	})
	return &UpstreamFailoverError{
		StatusCode: http.StatusTooManyRequests, ResponseBody: responseBody,
		ResponseHeaders: headers, RetryableOnSameAccount: true, RequestScopedTransient: true,
	}
}
