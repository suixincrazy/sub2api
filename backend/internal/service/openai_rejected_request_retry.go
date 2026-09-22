package service

import (
	"context"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// 上游（或其前置 ALB / 网关）偶发地把一次完全合法的请求打回成 400，且不给任何
// 可定位字段：要么是 ALB 自己那张 HTML 400 错误页，要么是只有一句
// "Upstream rejected the request" 的 JSON，要么连 body 都是空的。生产 ops_error_logs
// 里这类记录的共同形状是 kind=http_error、每次只有一个 upstream event、响应里没有
// error.param / error.code，重放同一份请求体立刻就能成功。
//
// 这类「通用拒绝」与 isOpenAIDeterministicClientError 覆盖的真 400 语义相反：
// 后者换账号重试多少次都失败（Schema 违规、非法字段），前者是链路抖动，重试即好。
// 因此这里把它接进既有的 429 重试预算（1 次初始 + upstream429MaxRetries 次重试），
// 用尽后才按 429 收口并施加一次冷却——形状照 isOpenAIRequestScopedCapacityShed：
// 请求级瞬态、先在同号上有界重试、重试期间不冻结账号。

// openAIRejectedRequestFallbackMessages 是「通用拒绝」的兜底文案闭集。
// 只认网关自己的兜底串与上游那句无信息量的拒绝，不做模糊包含匹配，避免把带有
// 真实原因的 400 误判成可重试。
var openAIRejectedRequestFallbackMessages = []string{
	strings.ToLower(openAIUpstreamClientErrorFallbackMessage),
	"upstream rejected request",
	"bad request",
	"400 bad request",
}

// isOpenAIGenericRejectionText 判断一段错误文案是否只是「被拒了」而不含任何原因。
func isOpenAIGenericRejectionText(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return true
	}
	for _, candidate := range openAIRejectedRequestFallbackMessages {
		if lower == candidate {
			return true
		}
	}
	return false
}

// isOpenAIALBRejectionPage 判断 body 是否为负载均衡器直出的 HTML 400 错误页。
// 这类响应从不经过模型，判定只看「非 JSON + HTML 标记 + 400 字样」，不匹配
// 具体厂商串（alb / nginx / envoy 都是同一类链路故障）。
func isOpenAIALBRejectionPage(body []byte) bool {
	if len(body) == 0 || gjson.ValidBytes(body) {
		return false
	}
	lower := strings.ToLower(string(body))
	if !strings.Contains(lower, "<html") && !strings.Contains(lower, "<body") {
		return false
	}
	return strings.Contains(lower, "400") || strings.Contains(lower, "bad request")
}

// isOpenAIGenericUpstreamRejection 判断一个 400 响应是否属于「通用拒绝」。
//
// 排他顺序很重要：任何带有可定位信息的 400 都必须先被排除，只剩下「无原因的拒绝」
// 才返回 true。判据如下（任一成立即认为携带了真实原因，不可重试）：
//   - error.param 存在：客户端要靠它定位非法字段（形如 input[8].tools[1].parameters）
//   - error.code 存在：客户端要靠它判断是否值得重试
//   - error.message 含实质内容：不在兜底文案闭集里就是真原因
//
// 另外，已被现有分类器认领的 400 一律让行，避免双重归类改写既有语义。
func isOpenAIGenericUpstreamRejection(statusCode int, upstreamMsg string, body []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	// 已有明确语义的 400 各有归属，不在这里改判。
	if isOpenAIContextWindowError(upstreamMsg, body) ||
		isOpenAITransientProcessingError(statusCode, upstreamMsg, body) ||
		isOpenAIRequestScopedCapacityShed(upstreamMsg, body) ||
		isOpenAIInstructionsRequiredError(statusCode, upstreamMsg, body) ||
		isOpenAICompatibleModelNotFound400(body) {
		return false
	}
	if hit, _, _ := detectOpenAICyberPolicy(body); hit {
		return false
	}
	if isOpenAIALBRejectionPage(body) {
		return true
	}
	if len(body) == 0 || len(strings.TrimSpace(string(body))) == 0 {
		return isOpenAIGenericRejectionText(upstreamMsg)
	}
	if !gjson.ValidBytes(body) {
		// 非 JSON 且不是 HTML 错误页：只有整段文本本身无信息量时才算通用拒绝。
		return isOpenAIGenericRejectionText(string(body)) && isOpenAIGenericRejectionText(upstreamMsg)
	}
	// 结构化 JSON：带 param / code 的都是可定位错误，必须原样回客户端。
	if strings.TrimSpace(gjson.GetBytes(body, "error.param").String()) != "" {
		return false
	}
	if strings.TrimSpace(extractUpstreamErrorCode(body)) != "" {
		return false
	}
	if !isOpenAIGenericRejectionText(upstreamMsg) {
		return false
	}
	return isOpenAIGenericRejectionText(gjson.GetBytes(body, "error.message").String())
}

// isOpenAIGenericStreamRejection 判断 HTTP 200 流内的 error / response.failed 终态
// 是否属于「通用拒绝」。上游偶发地在 200 之后立刻推一个无原因的 invalid_request_error
// 就收尾，语义与 HTTP 400 通用拒绝完全一致，同样应复用 429 重试预算。
func isOpenAIGenericStreamRejection(payload []byte, message string) bool {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return false
	}
	if openAIStreamFailedEventSemanticStatus(payload, message) != http.StatusBadRequest {
		return false
	}
	// 复用 HTTP 400 的排他判据；流内事件把错误对象嵌在 response.error 下，
	// 先归一成 error.* 形状再判。
	normalized := payload
	if gjson.GetBytes(payload, "response.error").Exists() && !gjson.GetBytes(payload, "error").Exists() {
		normalized = []byte(`{"error":` + gjson.GetBytes(payload, "response.error").Raw + `}`)
	}
	return isOpenAIGenericUpstreamRejection(http.StatusBadRequest, message, normalized)
}

// newOpenAIGenericRejectionRetryError 把「通用拒绝」包装成 429 failover 错误，
// 使其落入 retryUpstream429 的重试闸门。预算用尽后 upstream429RetryState.retry
// 会把 RetryableOnSameAccount 归零，外层 handler 不会再叠加预算。
//
// 返回 nil 表示当前调用栈没有重试预算可用，调用方必须落回原有的确定性错误分支。
// 判据是 ctx 里有没有 upstream429RetryState：该作用域只由 retryUpstream429 →
// beginUpstream429Retry 装入（openai_gateway_forward.go:23 的 Forward 最外层，
// 流式侧同时改写 c.Request 让终态处理器看到同一个作用域）。没有作用域时
// deferUpstream429SideEffects 会静默丢弃冷却回调，合成的 429 也无人重试 —— 那就
// 成了「把确定性 400 翻译成客户端看不懂的 429」，正是 #5479 要避免的形状。
//
// 冷却必须在这里显式登记：deferUpstream429SideEffects 只在上游真的返回 429 时
// 才被调用方触达（handleOpenAIAccountUpstreamError / HandleUpstreamError 的入口
// 都以 status==429 为前提），而这里的 429 是网关按「链路抖动」语义合成的，上游
// 实际返回的是 400 或 HTTP 200 流内失败。不登记回调的话预算耗尽时
// upstream429RetryState.apply 为 nil，冷却会被整个跳过。
func (s *OpenAIGatewayService) newOpenAIGenericRejectionRetryError(
	ctx context.Context,
	account *Account,
	responseHeaders http.Header,
	responseBody []byte,
	canonicalModel ...string,
) *UpstreamFailoverError {
	if upstream429State(ctx, account) == nil {
		return nil
	}
	headers := responseHeaders.Clone()
	if headers == nil {
		headers = http.Header{}
	}
	// 推迟到预算耗尽再执行：重试期间账号必须保持可调度。
	deferUpstream429SideEffects(ctx, account, http.StatusTooManyRequests, headers, func(ctx context.Context) {
		if s == nil {
			return
		}
		// 走 OpenAI 侧既有的 429 处理链（Spark 限流 / 临时不可调度 /
		// RateLimitService.HandleUpstreamError → handle429 → SetRateLimited）。
		// 此刻 upstream429RetryState.applying 为 true，链路里两处
		// deferUpstream429SideEffects 都会直接放行，不会自我递归。
		s.handleOpenAIAccountUpstreamError(ctx, account, http.StatusTooManyRequests, headers, responseBody, canonicalModel...)
	})
	return &UpstreamFailoverError{
		StatusCode:             http.StatusTooManyRequests,
		ResponseBody:           responseBody,
		ResponseHeaders:        headers,
		RetryableOnSameAccount: true,
		RequestScopedTransient: true,
	}
}
