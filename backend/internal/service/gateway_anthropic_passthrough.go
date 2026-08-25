package service

// 本文件由 gateway_service.go 纯移动拆分而来：Anthropic APIKey 直通
// （passthrough）转发路径及其流式/非流式响应与 usage 解析。仅做代码搬迁，
// 无任何行为变更。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/gin-gonic/gin"
)

type anthropicPassthroughForwardInput struct {
	Body          []byte
	Parsed        *ParsedRequest
	RequestModel  string
	OriginalModel string
	RequestStream bool
	StartTime     time.Time
}

func (s *GatewayService) forwardAnthropicAPIKeyPassthrough(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	reqModel string,
	originalModel string,
	reqStream bool,
	startTime time.Time,
) (*ForwardResult, error) {
	return s.forwardAnthropicAPIKeyPassthroughWithInput(ctx, c, account, anthropicPassthroughForwardInput{
		Body:          body,
		RequestModel:  reqModel,
		OriginalModel: originalModel,
		RequestStream: reqStream,
		StartTime:     startTime,
	})
}

func (s *GatewayService) forwardAnthropicAPIKeyPassthroughWithInput(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	input anthropicPassthroughForwardInput,
) (*ForwardResult, error) {
	token, tokenType, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	if tokenType != "apikey" {
		return nil, fmt.Errorf("anthropic api key passthrough requires apikey token, got: %s", tokenType)
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	logger.LegacyPrintf("service.gateway", "[Anthropic 自动透传] 命中 API Key 透传分支: account=%d name=%s model=%s stream=%v",
		account.ID, account.Name, input.RequestModel, input.RequestStream)

	if c != nil {
		c.Set("anthropic_passthrough", true)
	}
	// Pre-filter: strip empty text blocks (including nested in tool_result) to prevent upstream 400.
	input.Body = StripEmptyTextBlocks(input.Body)
	// Pre-filter: strip web-search history blocks the upstream cannot accept
	// (emulation-synthesized ones always; genuine ones additionally for
	// passback-required third-party upstreams such as GLM/Kimi/DeepSeek,
	// which reject server_tool_use with 400). input.RequestModel 已是映射后的模型 ID。
	input.Body = FilterWebSearchHistoryBlocks(input.Body, input.RequestModel)
	if input.Parsed != nil {
		// 透传分支也会改写实际 wire body，成功 usage hash 依赖这里同步当前 body。
		if err := input.Parsed.ReplaceBody(input.Body); err != nil {
			return nil, err
		}
	}

	var resp *http.Response
	retryStart := time.Now()
	for attempt := 1; attempt <= maxRetryAttempts; attempt++ {
		upstreamCtx, releaseUpstreamCtx := detachStreamUpstreamContext(ctx, input.RequestStream)
		upstreamReq, wireBody, err := s.buildUpstreamRequestAnthropicAPIKeyPassthrough(upstreamCtx, c, account, input.Body, token)
		releaseUpstreamCtx()
		if err != nil {
			return nil, err
		}
		if input.Parsed != nil && !bytes.Equal(wireBody, input.Body) {
			// build 阶段会按 beta 能力清理 body，发送前同步到 ParsedRequest 当前视图。
			if err := input.Parsed.ReplaceBody(wireBody); err != nil {
				return nil, err
			}
			input.Body = input.Parsed.Body.Bytes()
		}

		resp, err = s.httpUpstream.DoWithTLS(upstreamReq, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
		if err != nil {
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			safeErr := sanitizeUpstreamErrorMessage(err.Error())
			setOpsUpstreamError(c, 0, safeErr, "")
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: 0,
				UpstreamURL:        safeUpstreamURL(upstreamReq.URL.String()),
				Passthrough:        true,
				Kind:               "request_error",
				Message:            safeErr,
			})
			if errors.Is(err, context.Canceled) {
				return nil, err
			}
			scheduleOllamaCloudUsageActivity(s.deferredService, account)
			return nil, &UpstreamFailoverError{
				StatusCode: http.StatusBadGateway,
				Reason:     GatewayFailureReason("anthropic_passthrough_transport"),
			}
		}

		// Anthropic safeguards and the provider WAF can report a request-level
		// refusal with statuses that are otherwise treated as ordinary client
		// errors (400/405). Classify only the known refusal fingerprints here so
		// the current request can try another account without changing account
		// health state or broadly retrying every 400.
		if isAnthropicSafetyRefusalStatus(resp.StatusCode) {
			respBody, readErr := s.readUpstreamErrorBody(resp)
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(respBody))
			if readErr == nil && isAnthropicSafetyRefusalResponse(resp.StatusCode, respBody) {
				return nil, s.newAnthropicSafetyFailoverError(c, resp, account, respBody)
			}
		}

		// 透传分支禁止 400 请求体降级重试（该重试会改写请求体）
		if resp.StatusCode >= 400 && resp.StatusCode != 400 && s.shouldRetryUpstreamError(account, resp.StatusCode) {
			if attempt < maxRetryAttempts {
				elapsed := time.Since(retryStart)
				if elapsed >= maxRetryElapsed {
					break
				}

				delay := retryBackoffDelay(attempt)
				remaining := maxRetryElapsed - elapsed
				if delay > remaining {
					delay = remaining
				}
				if delay <= 0 {
					break
				}

				respBody, _ := s.readUpstreamErrorBody(resp)
				_ = resp.Body.Close()
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: resp.StatusCode,
					UpstreamRequestID:  resp.Header.Get("x-request-id"),
					UpstreamURL:        safeUpstreamURL(upstreamReq.URL.String()),
					Passthrough:        true,
					Kind:               "retry",
					Message:            extractUpstreamErrorMessage(respBody),
					Detail: func() string {
						if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
							return truncateString(string(respBody), s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
						}
						return ""
					}(),
				})
				logger.LegacyPrintf("service.gateway", "Anthropic passthrough account %d: upstream error %d, retry %d/%d after %v (elapsed=%v/%v)",
					account.ID, resp.StatusCode, attempt, maxRetryAttempts, delay, elapsed, maxRetryElapsed)
				if err := sleepWithContext(ctx, delay); err != nil {
					return nil, err
				}
				continue
			}
			break
		}

		break
	}
	if resp == nil || resp.Body == nil {
		return nil, errors.New("upstream request failed: empty response")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 && s.shouldRetryUpstreamError(account, resp.StatusCode) {
		if s.shouldFailoverUpstreamError(resp.StatusCode) {
			respBody, _ := s.readUpstreamErrorBody(resp)
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(respBody))

			logger.LegacyPrintf("service.gateway", "[Anthropic Passthrough] Upstream error (retry exhausted, failover): Account=%d(%s) Status=%d RequestID=%s Body=%s",
				account.ID, account.Name, resp.StatusCode, resp.Header.Get("x-request-id"), truncateString(string(respBody), 1000))

			s.handleRetryExhaustedSideEffects(ctx, resp, account)
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				UpstreamRequestID:  resp.Header.Get("x-request-id"),
				Passthrough:        true,
				Kind:               "retry_exhausted_failover",
				Message:            extractUpstreamErrorMessage(respBody),
				Detail: func() string {
					if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
						return truncateString(string(respBody), s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
					}
					return ""
				}(),
			})
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
			}
		}
		return s.handleRetryExhaustedError(ctx, resp, c, account)
	}

	if resp.StatusCode >= 400 && s.shouldFailoverUpstreamError(resp.StatusCode) {
		respBody, _ := s.readUpstreamErrorBody(resp)
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(respBody))

		logger.LegacyPrintf("service.gateway", "[Anthropic Passthrough] Upstream error (failover): Account=%d(%s) Status=%d RequestID=%s Body=%s",
			account.ID, account.Name, resp.StatusCode, resp.Header.Get("x-request-id"), truncateString(string(respBody), 1000))

		s.handleFailoverSideEffects(ctx, resp, account, input.RequestModel)
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			Passthrough:        true,
			Kind:               "failover",
			Message:            extractUpstreamErrorMessage(respBody),
			Detail: func() string {
				if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
					return truncateString(string(respBody), s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
				}
				return ""
			}(),
		})
		return nil, &UpstreamFailoverError{
			StatusCode:             resp.StatusCode,
			ResponseBody:           respBody,
			RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
		}
	}

	if resp.StatusCode >= 400 {
		return s.handleErrorResponse(ctx, resp, c, account, input.RequestModel)
	}

	var usage *ClaudeUsage
	var firstTokenMs *int
	var clientDisconnect bool
	if input.RequestStream {
		streamResult, err := s.handleStreamingResponseAnthropicAPIKeyPassthrough(ctx, resp, c, account, input.StartTime, input.RequestModel)
		if err != nil {
			// 流中断时保留已观测到的 usage 与错误一起返回，避免上游已计量的请求
			// 完全漏记漏计费（issue #5148）。
			if partial := partialStreamUsageResult(c, resp, streamResult, input.OriginalModel, input.RequestModel, input.StartTime, err); partial != nil {
				return partial, err
			}
			return nil, err
		}
		usage = streamResult.usage
		firstTokenMs = streamResult.firstTokenMs
		clientDisconnect = streamResult.clientDisconnect
	} else {
		usage, err = s.handleNonStreamingResponseAnthropicAPIKeyPassthrough(ctx, resp, c, account)
		if err != nil {
			return nil, err
		}
	}
	if usage == nil {
		usage = &ClaudeUsage{}
	}

	return &ForwardResult{
		RequestID:                     resp.Header.Get("x-request-id"),
		Usage:                         *usage,
		Model:                         input.OriginalModel,
		UpstreamModel:                 input.RequestModel,
		UpstreamResponseModel:         observedUpstreamResponseModel(c),
		UpstreamResponseModelConflict: observedUpstreamResponseModelConflict(c),
		UpstreamResponseServiceTier:   observedUpstreamResponseServiceTier(c),
		Stream:                        input.RequestStream,
		Duration:                      time.Since(input.StartTime),
		FirstTokenMs:                  firstTokenMs,
		ClientDisconnect:              clientDisconnect,
	}, nil
}

func isAnthropicSafetyRefusalStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusMethodNotAllowed:
		return true
	default:
		return false
	}
}

func isAnthropicSafetyRefusalResponse(statusCode int, body []byte) bool {
	if len(body) == 0 {
		return false
	}
	if hasAnthropicRefusalStopReason(body) {
		return true
	}
	if !isAnthropicSafetyRefusalStatus(statusCode) {
		return false
	}

	text := strings.ToLower(string(body))
	message := strings.ToLower(strings.TrimSpace(extractUpstreamErrorMessage(body)))
	if message != "" {
		text += "\n" + message
	}

	if strings.Contains(text, "safeguards flagged this message") {
		return true
	}
	if strings.Contains(text, "real-time cyber safeguards") &&
		(strings.Contains(text, "usage policy") || strings.Contains(text, "refusal")) {
		return true
	}
	if strings.Contains(text, "violative cyber") && strings.Contains(text, "usage policy") {
		return true
	}
	if strings.Contains(text, "blocked as it may cause potential threats to the server's security") {
		return true
	}
	return strings.Contains(text, "访问被阻断") && strings.Contains(text, "安全威胁")
}

func hasAnthropicRefusalStopReason(body []byte) bool {
	parsed := gjson.ParseBytes(body)
	for _, path := range []string{"stop_reason", "message.stop_reason", "delta.stop_reason"} {
		if strings.EqualFold(strings.TrimSpace(parsed.Get(path).String()), "refusal") {
			return true
		}
	}
	return false
}

// anthropicSSEPayloadCommitsResponse 判断这一帧 SSE 一旦写给客户端，本次响应是否就被
// 钉死（200 已发出、字节已 flush，再 failover 只会腐化响应）。
//
// 两个例外都是「不含任何用户可见内容」的帧，不能因为它们放弃 failover 窗口：
// 空思考块的 `content_block_start`（`thinking:""`）与随后的 `signature_delta`。
// 上游的安全拒答恰好就是这个形状——空思考块 + `message_delta{stop_reason:"refusal"}`
// （实测 output_tokens=5，正文只有一个空 thinking 与一段 signature）。旧实现在
// `content_block_start` 处就提交，于是携带 refusal 的 `message_delta` 只能被原样透传，
// 客户端据此把整个会话钉死降级（Claude Code 的 model_refusal_fallback 是 scope=session），
// 主号拒答后再也没机会切副号。把这两帧留在 prelude 里，拒答就能在提交前被捕获并真切号；
// 正常响应的第一帧可见增量（text_delta / thinking_delta / input_json_delta）照旧立即
// 提交，客户端观感不变，最坏情况只是纯 signature 响应推迟到 message_stop 才 flush。
func anthropicSSEPayloadCommitsResponse(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if bytes.Equal(trimmed, []byte("[DONE]")) || !json.Valid(trimmed) {
		return true
	}
	parsed := gjson.ParseBytes(trimmed)
	switch parsed.Get("type").String() {
	case "content_block_start":
		return !anthropicContentBlockIsContentlessThinking(parsed.Get("content_block"))
	case "content_block_delta":
		return !strings.EqualFold(strings.TrimSpace(parsed.Get("delta.type").String()), "signature_delta")
	case "message_stop", "error":
		return true
	default:
		return false
	}
}

// anthropicContentBlockIsContentlessThinking 判定 content_block_start 携带的块是否是
// 「尚无任何可见内容的思考块」。redacted_thinking 的载荷在 data 字段而非 thinking。
// 只认这两种类型：text / tool_use 等块的起始帧一律照旧视为提交，不扩大改动面。
func anthropicContentBlockIsContentlessThinking(block gjson.Result) bool {
	switch strings.ToLower(strings.TrimSpace(block.Get("type").String())) {
	case "thinking":
		return strings.TrimSpace(block.Get("thinking").String()) == ""
	case "redacted_thinking":
		return strings.TrimSpace(block.Get("data").String()) == ""
	default:
		return false
	}
}

func (s *GatewayService) newAnthropicSafetyFailoverError(c *gin.Context, resp *http.Response, account *Account, body []byte) *UpstreamFailoverError {
	statusCode := resp.StatusCode
	if statusCode == http.StatusOK {
		statusCode = http.StatusForbidden
	}
	message := strings.TrimSpace(extractUpstreamErrorMessage(body))
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: statusCode,
		UpstreamRequestID:  resp.Header.Get("x-request-id"),
		Kind:               "safety_refusal_failover",
		Message:            sanitizeUpstreamErrorMessage(message),
		Detail: func() string {
			if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
				return truncateString(string(body), s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
			}
			return ""
		}(),
	})
	logger.LegacyPrintf("service.gateway", "[Anthropic Passthrough] safeguards refusal, failover account=%d(%s) status=%d request_id=%s body=%s",
		account.ID, account.Name, statusCode, resp.Header.Get("x-request-id"), truncateString(string(body), 1000))
	return &UpstreamFailoverError{
		StatusCode:      statusCode,
		ResponseBody:    append([]byte(nil), body...),
		ResponseHeaders: resp.Header.Clone(),
		Scope:           GatewayFailureScopeAccount,
		Reason:          GatewayFailureReason("anthropic_safeguards"),
	}
}

// anthropicStopReasonIsHealthy 判定 message_delta 收尾的 stop_reason 是不是一次正常收束。
//
// 协议层的 message_stop 只证明上游"说完了"，不证明它"说完整了"：实测主号故障时
// 会送来带 message_stop 的残缺流，此时 sawTerminalEvent=true，旧判定一路放行，
// 账号不被罚，粘性把下一发重试原样送回同一个坏号——就是「一直断流却从不主副切换」。
//
// 白名单而非黑名单：Anthropic 的正常终止值是这四个，未来新增的正常值宁可先被判成
// 异常（多罚一次 1 分钟冷却，代价可控），也不能把未知故障值放行。空值单独处理：
// 上游没送 stop_reason 时不做判断，交给 output_tokens 那条信号，避免误伤兼容层。
func anthropicStopReasonIsHealthy(stopReason string) bool {
	switch strings.ToLower(strings.TrimSpace(stopReason)) {
	case "end_turn", "max_tokens", "tool_use", "stop_sequence", "pause_turn":
		return true
	default:
		return false
	}
}

// anthropicVisibleDeltaChars 取这一帧可见增量贡献的字符数。
//
// 只认真正会渲染给用户的三种增量，与 anthropicSSEPayloadCommitsResponse 的"提交"
// 口径保持一致：signature_delta 与空思考块不算内容，否则纯 signature 的响应会被
// 误判成有正文。
func anthropicVisibleDeltaChars(parsed gjson.Result) int {
	if strings.TrimSpace(parsed.Get("type").String()) != "content_block_delta" {
		return 0
	}
	switch strings.ToLower(strings.TrimSpace(parsed.Get("delta.type").String())) {
	case "text_delta":
		return len(parsed.Get("delta.text").String())
	case "thinking_delta":
		return len(parsed.Get("delta.thinking").String())
	case "input_json_delta":
		return len(parsed.Get("delta.partial_json").String())
	default:
		return 0
	}
}

// anthropicVisibleProseRunes 取这一帧给用户看的**正文**贡献的字符数（rune，不是字节）。
//
// 与 anthropicVisibleDeltaChars 刻意分开的两个理由，都是线上实证打出来的：
//
//  1. 单位。anthropicVisibleDeltaChars 用 len()，数的是 UTF-8 字节。中文一字三字节，
//     于是「200 字符上限」对中文实际只有 66 字。2026-08-23 22:31:21 账号 9 那一发截断
//     正文 131 字（≈289 字节），不但躲过了短回合判定，还因为 289>200 被
//     anthropicTurnProvesUpstreamHealthy 当成「上游把话说完了」的正面证据**清零了连击**，
//     比漏判更糟。日志里 visible_chars=54 对应的是 18 个汉字，54=18×3，就是这个单位。
//
//  2. 口径。thinking_delta 和 input_json_delta 也被 anthropicVisibleDeltaChars 计入，
//     但思考链和工具入参都不是「把话说完」的证据：模型可以想很久然后只吐一句就收尾，
//     那恰恰是要抓的形态。这里只认 text_delta。
//
// anthropicVisibleDeltaChars 保持原样不动：它唯一的用途是
// anthropicStreamLooksIncompleteDespiteTerminal 里的 `== 0` 判零，与单位无关，而那条
// 路径**会罚号**，不能借这次改动改变它的行为。
func anthropicVisibleProseRunes(parsed gjson.Result) int {
	if strings.TrimSpace(parsed.Get("type").String()) != "content_block_delta" {
		return 0
	}
	if !strings.EqualFold(strings.TrimSpace(parsed.Get("delta.type").String()), "text_delta") {
		return 0
	}
	return utf8.RuneCountInString(parsed.Get("delta.text").String())
}

// anthropicThinkingRunes 取这一帧的**思考链**贡献的 rune 数。
//
// 与 anthropicVisibleProseRunes 严格分开：那个只认 text_delta，这个只认 thinking_delta。
// 两个口径都要，才能分辨「想了很久但正文被切掉」——这一形态下 output_tokens 被思考撑得
// 很大，而正文近乎为零，单看任何一个量都判不出来。见
// anthropicTurnLooksSuspiciouslyShort 里的思考分支。
func anthropicThinkingRunes(parsed gjson.Result) int {
	if strings.TrimSpace(parsed.Get("type").String()) != "content_block_delta" {
		return 0
	}
	if !strings.EqualFold(strings.TrimSpace(parsed.Get("delta.type").String()), "thinking_delta") {
		return 0
	}
	return utf8.RuneCountInString(parsed.Get("delta.thinking").String())
}

// anthropicStreamLooksIncompleteDespiteTerminal 是「协议层收尾了但内容不完整」的启发式判定。
//
// 存在的理由：sawTerminalEvent 只证明上游送来了 message_stop，不证明它把话说完。实测主号
// 故障时会送来带 message_stop 的残缺流，旧判定一路放行 → 账号不被罚 → 粘性把下一发重试
// 原样送回同一个坏号，表现为「一直断流却从不主副切换」。
//
// 两条信号，任一成立即判残缺：
//  1. stop_reason 不在正常白名单里（含上游明确送来的故障值）；
//  2. 有 content_block_start（上游确实开了正文块）却零可见字符且 output_tokens<=0
//     ——开了块却什么都没吐出来，是截断的确定形态。
//
// 刻意不做「输出长度突降」的历史基线比对：那需要按账号+模型维护滑动窗口，且用户完全
// 可能问一个只需十几 token 就能答完的问题，误报会把健康账号罚下线。上面第 2 条取的是
// 「零内容」这个无歧义下界，既覆盖突降的极端情形，又不需要基线、不会误伤短回答。
//
// 空 stop_reason 单独放行：兼容层（gemini/openai 转 anthropic）不一定填这个字段，
// 此时只由第 2 条信号判断。
//
// refusal 也必须放行：它已由 reportSafetyRefusalWithoutFailover 用专属 keyword
// （stream_safety_refusal）归因并罚号，这里再判一次会把一次故障记成两次，打乱阈值
// 窗口的计数、也让 ops 看板出现两条指向同一次失败的记录。
func anthropicStreamLooksIncompleteDespiteTerminal(
	stopReason string,
	visibleChars int,
	outputTokens int,
	sawContentBlockStart bool,
) (string, bool) {
	trimmed := strings.TrimSpace(stopReason)
	if strings.EqualFold(trimmed, "refusal") {
		return "", false
	}
	if trimmed != "" && !anthropicStopReasonIsHealthy(trimmed) {
		return "terminal event carried abnormal stop_reason=" + trimmed, true
	}
	if sawContentBlockStart && visibleChars == 0 && outputTokens <= 0 {
		return "terminal event arrived with an opened content block but zero visible output", true
	}
	return "", false
}

// stickyShortTurnStreakStore 是 GatewayCache 的可选扩展：实现了它的缓存支持
// 「该会话连续几发都是可疑短回合」的计数。
//
// 做成可选接口而不是加进 GatewayCache：那个接口被大量测试替身实现，加方法会一次性
// 弄坏所有 mock。真实的 *gatewayCache 结构上满足它，线上正常生效；替身不满足时
// 整个机制静默退化成「不解绑」，也就是改动前的行为，不会把请求引到更差的路径上。
type stickyShortTurnStreakStore interface {
	IncrStickyShortTurnStreak(ctx context.Context, groupID int64, sessionHash string, ttl time.Duration) (int64, error)
	ResetStickyShortTurnStreak(ctx context.Context, groupID int64, sessionHash string) error
}

const (
	// anthropicShortTurnProseRuneLimit 判定「短回合」的正文上限，单位是 rune。
	//
	// 从 200 字节改成 400 rune 的依据：拿三份真实对话里 165 发
	// end_turn + 无 tool_use + output_tokens>0 的回合回放，按字节 200 命中 47 发，
	// 按 rune 400 命中 51 发，且是严格超集——多出来的 4 发正是被漏掉的中文截断
	// （out=58 对 127 字、out=90 对 183 字、out=73 对 150 字、out=70 对 131 字）。
	//
	// 2026-08-24 再从 400 放到 800：400 之上完全免检，而实测截断里正文 446~2325 rune
	// 的有 9 发（12:20:23 prose 529、14:15:48 prose 592、14:20:13 prose 813 等），
	// 全都是「宣布下一步就 end_turn、零 tool_use」的形态。
	//
	// 2026-08-25 再从 800 放到 1300，同时把 anthropicShortTurnOutputTokenLimit 抬到 430。
	// 前一次标定的样本标签是坏的：它把「Stop hook 把任务推回来」也算成截断证据，而 Stop
	// hook 在每一次 assistant 停下时都会触发，与截断无关，于是 168 发里有 140 发被标成正样本，
	// 网格上误报数恒为 7、看不出任何结构。只保留**真人**下一条消息作判据重标之后是
	// 92 正 / 17 负 / 62 无标，网格（漏判/误报）才出现平台：
	//
	//	        P=800  P=900  P=1000  P=1200  P=1300  P=1400  P=1600  P=2000
	//	T=320    6/2    6/2    6/2     6/2     6/2     6/2     6/2     6/2
	//	T=400    5/2    5/2    5/2     4/2     4/2     4/2     4/2     4/2
	//	T=410    5/2    4/2    4/2     3/2     3/2     3/2     3/2     3/2
	//	T=430    5/2    4/2    4/2     3/2     3/2     3/2     3/2     3/2
	//	T=460    5/2    4/2    4/2     3/2     3/2     3/2     3/2     3/2
	//	T=480    5/2    4/3    4/3     3/3     3/3     3/3     3/3     3/3
	//
	// 取 T=430 / P=1300 是平台中点：T 在 [410,460] 内误报不变，480 起才开始吃负样本；
	// P 只要 ≥1200 就够，1300 留一点余量。两个 FP 不是这次放宽引入的——它们在今天的
	// T=320 下**已经**被判可疑（out=42/prose=52，下一条是 `/goal`；out=112/prose=222，
	// 下一条是「版本还是 0.1.179，不变就好」），两发都是真短回答被当成截断，放宽一发未增。
	//
	// 新抓到的三发正是要治的那一类：out=337/prose=675、out=380/prose=1170（17:02:33 断流）、
	// out=402/prose=813（22:20:13 断流）。剩下 3 发漏判（prose 1757/1788/2325）不再追：
	// 它们的下一条真人消息都是接着问实质问题，说明上一发其实答完了，标签本身可疑。
	//
	// 为什么不继续往 P=2000 推：17 发可靠负样本里 15 发的 out 落在 461 以上，正文却铺满
	// 1000~2300 这一带，也就是说这一带的分辨力全部来自 token 闸门、正文上限在这里不提供
	// 任何信息。把 P 推到负样本正文中位数之上只是让判定更依赖 T 单点，不增收益。
	anthropicShortTurnProseRuneLimit = 1300
	// anthropicShortTurnOutputTokenLimit 判定「短回合」的输出 token 上限。
	//
	// 单靠正文长度分不开两类回合，token 数才是干净的分界：同一批样本里所有截断的
	// output_tokens 都 ≤112（19/30/34/58/63/70/73/83/90/104/106/112），而所有正常
	// 短回答都 ≥133（133/138/161/189/227/355/587/792/1796）。128 曾落在这条缝里。
	//
	// 2026-08-24 从 128 放到 320：那条缝是拿无思考、正文偏短的样本标的，对中文长正文
	// 太紧。实测漏判里 out=131/142/143/148/156 配正文 252~394 rune 的有 5 发
	// （12:13:29 out=148/prose=256、12:46:25 out=143/prose=321），密度 0.42~0.62，
	// 落在 128 之上却确确实实是截断。同一张网格：320 把漏判从 27 压到 12，误报仍是 4。
	//
	// 320 对 15 发正常样本仍有余量——它们的 out 是 461~1718 那一簇，最小的 3 发
	// （out=123/42/16）在 128 时就已经判可疑，不是这次放宽新增的。
	//
	// 2026-08-25 再从 320 放到 430。依据是同一次重标之后的网格（完整网格与标签修正过程见
	// anthropicShortTurnProseRuneLimit）：T 在 [410,460] 这一段漏判恒为 3、误报恒为 2，
	// T=480 起误报才涨到 3。430 取这段的中点，两侧各留 20 token 余量。
	//
	// 边界仍然干净：17 发可靠负样本里 15 发的 out ≥461，也就是全部落在 460 之外；余下 2 发
	// （out=42/prose=52、out=112/prose=222）在今天的 320 下**已经**被判可疑，是既有误报，
	// 不是这次抬闸门新增的。新收进来的是 out=337/380/402 那三发实测截断，其中 out=402/
	// prose=813 正是 22:20:13 那一发。
	//
	// 注意比的不是 output_tokens 原值，而是 anthropicVisibleOutputTokens 折算出的
	// 「说出来的那部分」：原值含思考 token，思考一长就把闸门顶穿。标定这个阈值的样本
	// 全是无思考回合，折算在那种回合上恒等于原值，所以标定仍然成立。
	anthropicShortTurnOutputTokenLimit = 430
	// anthropicHealthyTurnMinProseRunes 清零连击所需的最小正文长度，单位 rune。
	//
	// 刻意比 anthropicShortTurnProseRuneLimit 低（1300 vs 200）：两者之间那一段
	// （201~1300 rune 且 token 数不小）既不算可疑、也不算「说完了」，一律不表态。
	// 留这条中间带是因为清零是**否掉已积累证据**的动作，宁可保守。
	anthropicHealthyTurnMinProseRunes = 200
	// anthropicShortTurnStreakThreshold 连续多少发才解绑。
	//
	// 从 2 降到 1 的依据是实测误报率：拿三份真实对话按 message.id 归并成 283 次响应回放，
	// 被判可疑的 ~110 发**全部**后接「继续」这类人工推动或 Stop hook 把任务推回来，未被判
	// 的（out=189/227/792/1796）全是实质回答，误报率为 0。阈值 2 意味着客户端必须先吃满
	// 两发截断才换号；既然误报代价只是丢一次 prompt cache 而漏判代价是用户再吃一发断流，
	// 1 是更划算的一侧。
	anthropicShortTurnStreakThreshold = 1
	// anthropicShortTurnThinkingRuneFloor 判定「想得很久」所需的最小思考链长度，单位 rune。
	//
	// 只用来给下面这条上限解锁，不单独构成判据。取 200 与
	// anthropicHealthyTurnMinProseRunes 同量级：思考短于这个数的回合，output_tokens 不会
	// 被思考显著撑大，anthropicShortTurnOutputTokenLimit 那道闸门本来就有效，不需要绕。
	anthropicShortTurnThinkingRuneFloor = 200
	// anthropicPostThinkingProseRuneCeiling 思考充分的回合里，正文短到这个程度就不可能是
	// 真答案，单位 rune。
	//
	// 刻意取得比 anthropicShortTurnProseRuneLimit(800) 紧得多：那个上限配合 token 闸门
	// 使用，误判有 token 数兜着；这条要绕过 token 闸门，只剩正文长度一个判据，所以必须
	// 收到「几个字，不可能是任何问题的答案」的量级。40 rune 约合 13 个汉字。
	//
	// 2026-08-24 16:42:55 实证（usage_logs id=9514，账号 10）：output_tokens=577 而客户端
	// 只收到 "d." 两个字符。577 里绝大部分是思考 token，于是 outputTokens 越过当时 128 的
	// 闸门让判定直接 return false，这一形态全程免检——窗口调到多大都拦不住，因为压根没被
	// 判为可疑。闸门后来抬到 320 并改比折算值，577 原值仍在 320 之上，所以这条旁路照旧
	// 是这一形态唯一的入口。
	anthropicPostThinkingProseRuneCeiling = 40
	// anthropicShortTurnDiscardBudget 一次客户端请求里，「短但有正文」的可疑回合最多丢弃几次。
	//
	// 必须有上限：如果用户问的问题**本来**就只有一句话的答案，每个账号都会给出同样的短回合，
	// 不设上限就会一路把池子重试穿，最后把一个完好的短回答变成 502。
	//
	// 为什么是 2 而不是 1（2026-08-24 19:2x 实证）：账号 10 连续五发短回合都被成功丢弃
	// （prose 91/138/169/200/200，out 22/34/42/48/49），但其中两发的重试落到账号 9 上之后
	// 就交付了 —— 19:22:03（prose 287 / out 69）与 19:23:58（prose 217 / out 54）都只留下
	// sticky_short_turn_unbound、没有配对的 holdback_failover，正是「额度已被第一个坏号吃光
	// 所以放行」的签名。一次性额度的隐含假设是「第二个号大概率是好的」，而实测同时坏掉两个
	// 号是常态，所以这个假设不成立。
	//
	// 上限仍然远小于池子（当前 8 个可调度透传账号）：最坏情况用掉 3 个号就放行，
	// FailedAccountIDs 不会被掏空，因此额度耗尽退化成放行而不是 502。
	anthropicShortTurnDiscardBudget = 2
	// anthropicEmptyAnswerDiscardBudget 一次客户端请求里，「零正文」回合最多丢弃几次。
	//
	// 刻意比 anthropicShortTurnDiscardBudget 宽：那条上限的全部理由是保护「真的只有一句话
	// 的答案」，而零正文回合不存在这种合法解释（同一判断与对照数据见
	// reportAnthropicEmptyAnswerTurn 的注释），所以也就不存在「额度用尽后要原样放行」的
	// 理由——放行的结果一定是客户端吃到一个没有答案的 200。
	//
	// 2026-08-24 实证：4 条 disposition=delivered（15:04:12 / 15:12:13 / 18:09:58 /
	// 18:14:40）全是账号 12 在账号 9 被丢弃后 5~6 秒落地的空回合。启发式两档共用一次性额度时，
	// 第一个坏号把额度吃光，第二个坏号的空响应就必然放行，窗口调到多大都没用。
	//
	// 仍然留上限而不是无限重试：空回合这一档会走 HandleStreamTruncated 把账号冷却一分钟，
	// 每丢弃一次池子就小一圈，3 次足以跨过连续两个坏号（实测最长连击是 2）。额度耗尽后是
	// 退化成放行（等于今天的行为），不会新增 502；只有池子先被 FailedAccountIDs 掏空才会
	// 走到 502（当前 8 个可调度透传账号，3 次远够不着），那时带明确错误体的 502 也比一个
	// 空的 200 诚实。
	anthropicEmptyAnswerDiscardBudget = 3
)

// anthropicHoldbackDecision 是提交窗口内对「这一帧之后怎么办」的三态判定。
type anthropicHoldbackDecision int

const (
	// anthropicHoldbackKeep 继续持流：证据还不够，不能提交也不能丢弃。
	anthropicHoldbackKeep anthropicHoldbackDecision = iota
	// anthropicHoldbackRelease 放行：把攒下的帧按原样写给客户端，此后正常透传。
	anthropicHoldbackRelease
	// anthropicHoldbackDiscard 丢弃重试：这一回合疑似截断，且提交窗口仍完好，
	// 丢掉缓冲换个账号重来，客户端永远看不到这条截断响应。
	anthropicHoldbackDiscard
)

// anthropicHoldbackVerdict 决定持流缓冲的去向。
//
// 为什么需要它：解绑只能减少**重复**暴露——检测发生在 message_delta，而正文早在第一个
// content_block_delta 就 flush 出去了，200 已钉死，第一发截断必然被客户端看到。要做到零
// 暴露，就必须把提交点推后到「能判定」之后。判定量只有 stop_reason + output_tokens，两者
// 都在 message_delta 里，所以持流必须一直撑到那一帧。
//
// 撑到 message_delta 的代价是首字延迟，所以给三条提前放行的出口，任何一条成立就立刻放行：
//  1. 出现 tool_use 块——工具回合永远不是截断形态，没有再等的理由；
//  2. 正文已超过短回合上限——已经不可能被判可疑，再等只是白等；
//  3. 放行截止线到点——静默窗口耗尽、持流总时长撞上限、或客户端总死气吃满预算，三条都拿
//     真实数据标定过，见 GatewayConfig.AnthropicHoldbackWindowMs /
//     AnthropicHoldbackMaxHoldMs / AnthropicHoldbackDeadAirBudgetMs。
//
// heuristicDiscardsUsed 是本次请求已经因为启发式持流判定丢弃过几次上游响应。额度按形态分档，不是一刀切：
// 「短但有正文」丢满 anthropicShortTurnDiscardBudget 次后就原样放行，因为真短答案确实存在；
// 「零正文」可以多丢几次，因为不存在「本来就该是空的答案」这种合法解释。见
// anthropicShortTurnDiscardBudget 与 anthropicEmptyAnswerDiscardBudget。
//
// deadAirExhausted 单独作为一条否决 discard 的闸门，不能只并进 releaseDeadlineElapsed：
// 那个参数只在 stop_reason 还没到时才被读到，而 discard 恰恰发生在 stop_reason 已到的分支里。
// 死气吃满时换号是纯亏——客户端已经等了整个预算，新尝试的三条线全部归零、又要从头等一遍，
// 这正是 14:33:58 那一发跨 3 次尝试攥到 60 秒的成因。此时宁可把疑似截断的内容交出去：
// 截断至少还有 noteAnthropicShortTurnStreak 给下一发解绑兜着，断流则是整发白丢。
//
// blockOrderViolation 是唯一一条**排在所有提前放行出口之前**的判据，而且刻意不受
// deadAirExhausted 否决。理由是这一条与其余判据的性质不同：
//   - 其余判据是启发式，量的是「这一回合看起来是不是没说完」，误判就会误伤健康账号，
//     所以要让位给延迟；块序违规是协议层的确定性矛盾，为它换号不存在误伤成本。
//   - 交付一条块序违规的响应不是「答案短了点」，而是把伪造的工具输出交给客户端。Claude Code
//     这类客户端会当真去用它，用户只能人工发现并纠正（本次就是这么暴露的），代价远高于
//     多等一次尝试。额度封在 anthropicBlockOrderDiscardBudget=2，最坏也只是多等两次。
//
// 它必须排在 sawToolUseBlock 与正文上限这两个提前放行出口之前：违规回合完全可以带工具块，
// 也完全可以带超过 1300 rune 的长正文（21:25:40 那一发 872 rune，同类更长的也见得到），
// 排在后面就会被提前放行短路掉，等于没加。
func anthropicHoldbackVerdict(
	releaseDeadlineElapsed bool,
	deadAirExhausted bool,
	stopReason string,
	proseRunes int,
	outputTokens int,
	sawToolUseBlock bool,
	heuristicDiscardsUsed int,
	blockOrderDiscardsUsed int,
	thinkingRunes int,
	blockOrderViolation bool,
) anthropicHoldbackDecision {
	if blockOrderViolation && blockOrderDiscardsUsed < anthropicBlockOrderDiscardBudget {
		return anthropicHoldbackDiscard
	}
	if sawToolUseBlock {
		return anthropicHoldbackRelease
	}
	if proseRunes > anthropicShortTurnProseRuneLimit {
		return anthropicHoldbackRelease
	}
	// stop_reason 已知时判定是确定的，优先于窗口：窗口只是「等不起了」的兜底，
	// 而这里已经拿到了全部判据。
	if strings.TrimSpace(stopReason) != "" {
		if !deadAirExhausted &&
			anthropicTurnLooksSuspiciouslyShort(stopReason, proseRunes, outputTokens, sawToolUseBlock, thinkingRunes) &&
			heuristicDiscardsUsed < anthropicHoldbackDiscardBudget(stopReason, proseRunes, outputTokens, sawToolUseBlock) {
			return anthropicHoldbackDiscard
		}
		return anthropicHoldbackRelease
	}
	if releaseDeadlineElapsed {
		return anthropicHoldbackRelease
	}
	return anthropicHoldbackKeep
}

// anthropicHoldbackDiscardBudget 给出这一形态在一次客户端请求里允许丢弃的次数。
//
// 两种启发式形态共用同一个计数器、各自比自己的上限，顺序无关：先为短回合丢满 2 次之后，
// 空回合仍能继续丢（2 < 3）；反过来为空回合丢满 3 次之后，短回合那一档立刻判定额度已尽
// （3 >= 2），短回合上限的保护不会因为空回合放宽而失效。块序判据使用独立计数器。
func anthropicHoldbackDiscardBudget(
	stopReason string, proseRunes, outputTokens int, sawToolUseBlock bool,
) int {
	if anthropicTurnIsEmptyAnswer(stopReason, proseRunes, outputTokens, sawToolUseBlock) {
		return anthropicEmptyAnswerDiscardBudget
	}
	return anthropicShortTurnDiscardBudget
}

// anthropicBlockOrderDiscardBudget 是块序违规这一档在一次客户端请求里允许丢弃的次数。
//
// 与另外两档分开计数，各自比自己的上限。给 2 而不是更多：
// 违规是确定性判据、不存在「本来就该这样」的合法解释，理论上一次换号就够；留第二次是为了
// 跨过连续两个坏号（短回合那一档实测最长连击就是 2）。额度耗尽后退化成放行 + 事后归因，
// 不会把池子掏空成 502。
const anthropicBlockOrderDiscardBudget = 2

// anthropicContentBlockOrderTracker 按 content_block_start 的到达顺序判定块序违规。
//
// 判据只有一条：thinking / redacted_thinking 块出现在 text 块**之后**。Anthropic 的
// 扩展思考协议里思考块永远排在正文之前（同一条 assistant 消息内 thinking 先于 text，
// 客户端把 thinking 当作正文的前置推理来渲染），所以「text 之后又冒出 thinking」不是一种
// 合法形态，只能是上游把多次生成的块拼错了序，或者把别的响应的碎片混了进来。
//
// 为什么要专门认这一条：2026-08-25 21:25:40 那一发（账号 9，msg_7d59435a-…，
// stop_reason=end_turn、591 output_tokens、HTTP 200）的块序正是 text / thinking / text，
// 第一个 text 块是伪造的工具输出（"Tool results:\n\n[Bash] SYNTAX OK…"），末块开头是
// 乱码。这种响应在协议层完全合法：有 message_stop、有 stop_reason、正文 872 rune 远超
// 短回合上限，既有的启发式判据（anthropicTurnLooksSuspiciouslyShort / 正文上限）全部放行，
// 网关此前对它一无所知。
//
// 误报面实测：6 份对话共 1443 条 assistant 消息里块序违规只出现 5 次，其中 4 次是已确认的
// 伪造回合，剩下 1 次是 msg_bdrk_* 前缀的合法响应。而 Bedrock 走的是
// handleBedrockStreamingResponse 这条独立链路，两个跟踪点（透传链路的持流观测器 + 常规流式
// 链路的事后归因）都碰不到它，所以那唯一一例合法形态在这两条链路上根本不会出现。判据在它
// 实际生效的范围内是确定性的，不是概率启发式。
type anthropicContentBlockOrderTracker struct {
	sawTextBlock bool
	violated     bool
}

// note 吃下一个 content_block_start 的块类型。大小写不敏感，空串忽略。
func (t *anthropicContentBlockOrderTracker) note(blockType string) {
	switch strings.ToLower(strings.TrimSpace(blockType)) {
	case "text":
		t.sawTextBlock = true
	case "thinking", "redacted_thinking":
		if t.sawTextBlock {
			t.violated = true
		}
	}
}

// violation 报告本条消息是否已经出现块序违规。一旦置位就不再回落。
func (t *anthropicContentBlockOrderTracker) violation() bool {
	return t != nil && t.violated
}

// anthropicHoldbackObserver 在持流期间独立采集判定量。
//
// 必须与 handleStreamingResponseAnthropicAPIKeyPassthrough 里的累加器分开：那些量由
// processLine 累计，而持流期的帧要等 flushPendingPrelude 重放才会过 processLine，
// 共用就会重复计数。
type anthropicHoldbackObserver struct {
	proseRunes         int
	thinkingRunes      int
	outputTokens       int
	stopReason         string
	sawToolUseBlock    bool
	blockOrder         anthropicContentBlockOrderTracker
	firstCommitPointAt time.Time
	// lastContentFrameAt 是最后一帧**有内容**的 SSE data 的到达时刻，窗口靠它度量静默。
	// ping 刻意不计入：它只证明连接活着，不证明上游还在产出，正是要判为「静默」的形态。
	lastContentFrameAt time.Time
	// streamStartedAt 是**客户端请求**的起点（与 duration_ms / first_token_ms 同一口径），
	// 不是上游首帧到达时刻。死气预算靠它度量「客户端到底干等了多久」。
	streamStartedAt time.Time
}

// observe 吃下一帧已解析的 SSE data。commits 是这一帧在旧行为下会不会提交响应，
// now 由调用方传入，便于测试注入时钟。
func (o *anthropicHoldbackObserver) observe(parsed gjson.Result, commits bool, now time.Time) {
	frameType := strings.TrimSpace(parsed.Get("type").String())
	switch frameType {
	case "message_delta":
		if r := strings.TrimSpace(parsed.Get("delta.stop_reason").String()); r != "" {
			o.stopReason = r
		}
	case "content_block_start":
		blockType := strings.TrimSpace(parsed.Get("content_block.type").String())
		o.blockOrder.note(blockType)
		if strings.EqualFold(blockType, "tool_use") {
			o.sawToolUseBlock = true
		}
	}
	o.proseRunes += anthropicVisibleProseRunes(parsed)
	o.thinkingRunes += anthropicThinkingRunes(parsed)
	// output_tokens 在 message_start 里是初始小值、在 message_delta 里才是终值，
	// 两条路径都看并取最大值，避免依赖帧序。
	for _, path := range []string{"usage.output_tokens", "message.usage.output_tokens"} {
		if v := parsed.Get(path); v.Exists() {
			if n := int(v.Int()); n > o.outputTokens {
				o.outputTokens = n
			}
		}
	}
	// firstCommitPointAt 只用来判断「窗口该不该开始计时」，不再是计时的起点：还没到旧行为
	// 下的提交点时，持流没有新增任何延迟，窗口无从谈起。思考模式下这一帧是第一个
	// thinking_delta。
	if commits && o.firstCommitPointAt.IsZero() {
		o.firstCommitPointAt = now
	}
	if frameType != "ping" {
		o.lastContentFrameAt = now
	}
}

// silenceSince 给出「上游静默从什么时候开始算」。返回零值表示窗口还没起算。
//
// 两个下界取晚的那个：lastContentFrameAt 是真正的静默起点，而 firstCommitPointAt 保证
// 在旧行为下本会提交的那一帧之前永不起算——那之前的持流（message_start / ping / 空思考块 /
// signature）本来就存在，不是本机制新增的延迟。
func (o *anthropicHoldbackObserver) silenceSince() time.Time {
	if o.firstCommitPointAt.IsZero() {
		return time.Time{}
	}
	if o.lastContentFrameAt.Before(o.firstCommitPointAt) {
		return o.firstCommitPointAt
	}
	return o.lastContentFrameAt
}

// windowElapsed 判断持流窗口是否已耗尽。
//
// 度量的是**上游静默多久**，不是持流总共多久。这个区别就是 2026-08-25 02:06:53 那一发
// 截断的根因：旧实现从 firstCommitPointAt 起算总时长，而
// anthropicSSEPayloadCommitsResponse 把 thinking_delta 算作提交帧，于是长思考回合的窗口
// 在**思考期间**就走完了。那一发 thinking 3934 rune / 正文 174 rune / output_tokens 1059，
// 折算可见 44 token，判据齐了之后判定必然是「可疑」——但窗口早在 stop_reason 到达前耗尽，
// case <-holdbackCh 已经无条件把缓冲提交给客户端，判定只赶得上给下一发解绑。阈值怎么调都
// 治不了这一类，因为它压根没走到判定那一步。
//
// 改成静默口径之后：帧还在持续到达就说明上游没卡住、判定量还在路上，不放行；只有真的静默
// 满一个窗口（也就是这个机制从一开始就说的「上游吐了几句就长时间静默」）才认定等不起。
//
// 代价是长思考回合在拿到 stop_reason 之前客户端一个字节都收不到（keepalive 在
// !streamCommitted 下不写字节）。这是零暴露的必然价格：任何写给客户端的字节都会钉死
// HTTP 200、断掉换号重试的可能。正文一旦超过 anthropicShortTurnProseRuneLimit 就提前放行，
// 长回答不会一直攥到最后；而这一条本身没有上界（上游一直出帧就一直续期），所以死气的上界
// 由 maxHoldElapsed 那条总时长上限单独兜着——2026-08-25 加，之前只有
// gateway.stream_data_interval_timeout（180s）那一级，实测出现过攥 83.4 秒的回合。
func (o *anthropicHoldbackObserver) windowElapsed(now time.Time, window time.Duration) bool {
	if window <= 0 {
		return false
	}
	since := o.silenceSince()
	if since.IsZero() {
		return false
	}
	return now.Sub(since) >= window
}

// holdbackSilenceDeadline 给出「再静默到什么时刻就该放行」。返回零值表示窗口还没起算。
//
// 给 select 里的定时器分支用：定时器 arm 之后按固定时长走，帧还在到达时也会开火，所以
// 开火后拿这个时刻复核——没到就按差值续期，到了才放行。判据只有这一处口径，和
// windowElapsed 共用 silenceSince，不会出现「定时器认为到点、判定认为没到」的分歧。
func (o *anthropicHoldbackObserver) holdbackSilenceDeadline(window time.Duration) time.Time {
	if window <= 0 {
		return time.Time{}
	}
	since := o.silenceSince()
	if since.IsZero() {
		return time.Time{}
	}
	return since.Add(window)
}

// maxHoldElapsed 判断持流**总时长**是否已到上限。
//
// 与 windowElapsed 的区别是起点：那一条从最后一帧有内容的 data 起算，上游持续出帧就无限
// 续期；这一条从 firstCommitPointAt 起算，一旦开始持流就单调走到底、任何新帧都不能续期。
// 两条各治一种形态——静默窗口治「吐两句就卡住」，这一条治「一直在吐但客户端一个字都看不到」。
//
// 为什么必须有这一条（2026-08-25 实证）：窗口改成静默口径之后，长思考／长回答的持流不再有
// 任何上界，08:28:10 那一发 out=3677 攥了 83.4 秒、first_token_ms 83394 ≈ duration_ms
// 83423，客户端全程零字节。分 band 量下来死气全压在 output_tokens>430 这一批上（p90 首字节
// 42s→113s），而这一批在判据上结构性不可能被判可疑，攥着纯亏。完整标定见
// GatewayConfig.AnthropicHoldbackMaxHoldMs。
func (o *anthropicHoldbackObserver) maxHoldElapsed(now time.Time, maxHold time.Duration) bool {
	if maxHold <= 0 {
		return false
	}
	if o.firstCommitPointAt.IsZero() {
		return false
	}
	return now.Sub(o.firstCommitPointAt) >= maxHold
}

// holdbackMaxHoldDeadline 给出持流总时长到顶的时刻。返回零值表示还没起算或没配上限。
func (o *anthropicHoldbackObserver) holdbackMaxHoldDeadline(maxHold time.Duration) time.Time {
	if maxHold <= 0 {
		return time.Time{}
	}
	if o.firstCommitPointAt.IsZero() {
		return time.Time{}
	}
	return o.firstCommitPointAt.Add(maxHold)
}

// deadAirElapsed 判断**客户端总死气**是否已经吃满预算。
//
// 与前两条的区别是锚点，而这个区别决定了它治的是另一类故障：windowElapsed 从最后一帧有内容
// 的 data 起算、maxHoldElapsed 从 firstCommitPointAt 起算，两者都只度量「本次上游尝试」。
// 而客户端感知的死气是跨 failover **累加**的——一次 discard 换号之后，新尝试的 observer 是
// 全新的，那两条线连同 10 秒上限一起归零，客户端却还在同一个 HTTP 请求上继续干等。
//
// 2026-08-25 实证，5 次断流全同一签名（13:23:21 / 13:50:52 / 14:10:30 / 14:26:26 /
// 14:33:58）：14:26:26 那一发跨 2 次尝试、客户端零字节 30.76 秒；14:33:58 那一发跨 3 次
// 尝试、约 60 秒。单次尝试的 duration_ms 分别只记到 19.4s / 23.6s，所以
// usage_logs.first_token_ms/duration_ms 会**系统性低估**真实死气，别拿它当验证口径——
// 要么按 gateway_check_start 到 Client disconnected 的跨度量，要么直接数断连事件。
//
// streamStartedAt 由 anthropicClientRequestStartedAt 从 gin.Context 上取，跨尝试是同一个
// 值，所以这一条是三条里唯一不会被换号重置的。
func (o *anthropicHoldbackObserver) deadAirElapsed(now time.Time, budget time.Duration) bool {
	if budget <= 0 || o.streamStartedAt.IsZero() {
		return false
	}
	return now.Sub(o.streamStartedAt) >= budget
}

// holdbackDeadAirDeadline 给出客户端死气预算耗尽的时刻。返回零值表示没配预算或没有锚点。
func (o *anthropicHoldbackObserver) holdbackDeadAirDeadline(budget time.Duration) time.Time {
	if budget <= 0 || o.streamStartedAt.IsZero() {
		return time.Time{}
	}
	return o.streamStartedAt.Add(budget)
}

// holdbackReleaseDeadline 给出「无论上游再说什么都必须放行」的时刻：三条截止线里先到的那个。
// 返回零值表示三条都还没起算。
//
// 定时器分支只认这一个口径，保证不会出现「定时器认为到点、判定认为没到」的分歧——判定侧
// 走 releaseDeadlineElapsed，两者由同样的三条截止线导出。
func (o *anthropicHoldbackObserver) holdbackReleaseDeadline(window, maxHold, deadAir time.Duration) time.Time {
	earliest := time.Time{}
	for _, candidate := range []time.Time{
		o.holdbackSilenceDeadline(window),
		o.holdbackMaxHoldDeadline(maxHold),
		o.holdbackDeadAirDeadline(deadAir),
	} {
		if candidate.IsZero() {
			continue
		}
		if earliest.IsZero() || candidate.Before(earliest) {
			earliest = candidate
		}
	}
	return earliest
}

// releaseDeadlineElapsed 判断三条放行截止线有没有任意一条到点。
func (o *anthropicHoldbackObserver) releaseDeadlineElapsed(now time.Time, window, maxHold, deadAir time.Duration) bool {
	return o.windowElapsed(now, window) || o.maxHoldElapsed(now, maxHold) || o.deadAirElapsed(now, deadAir)
}

// anthropicTurnLooksSuspiciouslyShort 判定这一回合是否为「协议合法但疑似没把话说完」。
//
// 与 anthropicStreamLooksIncompleteDespiteTerminal 的分工：那个判的是能确定的残缺
// （异常 stop_reason、开了块却零内容），确定就罚号；这个判的是**不能确定**的可疑形态，
// 只用来累计连击、到阈值后解绑，绝不罚号。两者必须分开，否则可疑形态会被当成确定故障
// 把健康账号罚下线——那正是当初否掉「输出长度突降基线」的理由。
//
// 前三个条件是门槛，之后分「空」与「短」两类：
//  1. stop_reason 恰好是 end_turn。tool_use/max_tokens/stop_sequence 都说明模型
//     确实干了活或是被限制截断，pause_turn 是协议级续传，都不算。
//  2. 没有开过 tool_use 块。开了工具块的回合无论正文多短都是正常的 agent 行为。
//  3. 上游确实报了正的 output_tokens。为 0 的情形归 anthropicStreamLooksIncomplete-
//     DespiteTerminal 管，这里放行避免同一次故障被两套逻辑各记一次。
//
// 空回合（proseRunes == 0）：end_turn 收尾却一个字的正文都没有。这**不受 output_tokens
// 上限约束** —— 上限的作用是区分「短答案」与「真答案」，而没有正文时压根不存在答案。
// 思考 token 也计入 output_tokens，所以「想了很久、一句话都没说」会是大 output_tokens +
// 零正文，用上限去卡它只会漏掉。
//
// 短回合（0 < proseRunes <= 上限）：仍要求 output_tokens 不超过上限，这是主判据，
// 见 anthropicShortTurnOutputTokenLimit；正文上限兜住 token 计费口径异常的情形。
//
// 为什么必须把空回合收进来（2026-08-24 实证）：账号 12 sotamodel 的主力故障形态是
// message_start 之后一路到 message_stop 全程无正文、message_delta 报 output_tokens=1，
// 协议完整、stop_reason=end_turn。旧实现两侧都判不住 —— anthropicStreamLooksIncomplete-
// DespiteTerminal 要求 output_tokens <= 0（拿到的是 1），这里又要求 proseRunes > 0
// （拿到的是 0），48 小时里 51/142 发（35.9%）从缝里漏过去，客户端看到空响应后自己重试，
// 粘性把每一次重试都送回同一个坏号。对照组同期 output_tokens=1 占比：账号 5 是 0.1%、
// 9 是 0.1%、10/11 是 0% —— 空回合是明确的上游故障信号，不是合法回答形态。
func anthropicTurnLooksSuspiciouslyShort(
	stopReason string,
	proseRunes int,
	outputTokens int,
	sawToolUseBlock bool,
	thinkingRunes int,
) bool {
	if !strings.EqualFold(strings.TrimSpace(stopReason), "end_turn") {
		return false
	}
	if sawToolUseBlock {
		return false
	}
	// output_tokens<=0 且正文也是空的：那是 anthropicStreamLooksIncompleteDespiteTerminal
	// 的确定形态（开了块却零输出），让给它罚号，避免同一次故障记两次。
	//
	// 但 output_tokens<=0 **配着有正文**是另一回事：上游把 usage 报成 0 而实际吐了字节，
	// 两套判定都不认它——残缺判定要求 visibleChars==0，这里旧代码又直接 return false。
	// 2026-08-24 17:02:33 实证（usage_logs id=9544，账号 10）：output_tokens=0、
	// first_token_ms=10808 之后还跑了 2680ms，说明内容块和字节都出去了，却全程零信号、
	// 零记账。所以这一档必须留在短回合判据里，由正文长度说话。
	if outputTokens <= 0 && proseRunes == 0 {
		return false
	}
	if proseRunes == 0 {
		return true
	}
	// 想了很久却只吐出几个字：这一档必须在 token 闸门**之前**判，因为思考 token 计入
	// output_tokens，闸门在这里恒为假。见 anthropicPostThinkingProseRuneCeiling 上的实证。
	if thinkingRunes >= anthropicShortTurnThinkingRuneFloor &&
		proseRunes <= anthropicPostThinkingProseRuneCeiling {
		return true
	}
	if anthropicVisibleOutputTokens(outputTokens, proseRunes, thinkingRunes) >
		anthropicShortTurnOutputTokenLimit {
		return false
	}
	return proseRunes <= anthropicShortTurnProseRuneLimit
}

// anthropicVisibleOutputTokens 从 output_tokens 里估算「说出来的那部分」占多少 token。
//
// 为什么要折算：output_tokens 是思考与正文的合计，而 anthropicShortTurnOutputTokenLimit
// 那道闸门想问的一直是「模型到底说了多少」。思考一长合计就把闸门顶穿、闸门恒为假，于是
// 「想了很久却只说半句」这一整类截断全程免检——上面那条 anthropicPostThinkingProseRuneCeiling
// 旁路是拿正文 2 rune 的极端形态标定的，兜不住正文上百 rune 的同类形态。
//
// 2026-08-24 19:46:49 实证：thinking 454 rune / 正文 114 rune / output_tokens 286，
// stop_reason=end_turn 且无 tool_use，正文内容是宣布下一步（「先把上游状态和技能副本对齐」）
// 之后就收尾，思考里明说要并行推进几个方向。286 越过当时 128 的闸门让它免检，折算后是 57，
// 回到闸门的有效区间内。
//
// 按 rune 数比例切分：思考与正文出自同一次生成、同一种语言，token/rune 密度可以当成相同，
// 所以 visible ≈ out × prose/(prose+thinking)。这是估算而非精确值——上游不单独报正文
// token（那一发的 output_tokens_details.thinking_tokens 是 0），拿不到精确切分。作为闸门
// 输入够用：折算误差远小于闸门本身的余量。
//
// 折算与 anthropicPostThinkingProseRuneCeiling 那条旁路共用同一道下限
// anthropicShortTurnThinkingRuneFloor，理由也是同一条：思考短于下限时 output_tokens 没被
// 显著撑大，闸门本来就有效，此时折算只会凭空削弱它。下限以下返回原值，所以无思考
// （以及思考很短）的回合行为与改动前逐位相同，闸门背后那批样本的标定不受影响。
//
// 折算后闸门的实际语义（闸门 320、中文 token/rune 密度约 0.6）：320 token 对应正文约
// 533 rune，也就是思考很长时「正文没到五百来字」都算可疑。举几组：thinking 900/正文 300/
// out 1000 折算 250 判可疑，thinking 900/正文 500/out 1000 折算 357 放行，
// thinking 3000/正文 600/out 2000 折算 333 放行；正文再长则先被
// anthropicShortTurnProseRuneLimit(800) 拦下。
//
// 这道闸门与 anthropicTurnProvesUpstreamHealthy 那侧共用常量、也共用这个折算函数，
// 两者由此构成直接互否，见那边的注释。
func anthropicVisibleOutputTokens(outputTokens, proseRunes, thinkingRunes int) int {
	if outputTokens <= 0 || proseRunes <= 0 {
		return outputTokens
	}
	if thinkingRunes < anthropicShortTurnThinkingRuneFloor {
		return outputTokens
	}
	return outputTokens * proseRunes / (proseRunes + thinkingRunes)
}

// anthropicTurnIsEmptyAnswer 从可疑形态里再切出「空回合」这一档。
//
// 与短回合分开的理由是代价不同：短回合可能真的只是个一句话答案，误判要留退路（持流只丢弃
// 一次、绝不罚号）；空回合不存在「本来就该是空的答案」这种可能，所以可以按上游故障处理，
// 走 HandleStreamTruncated 让账号进冷却，下一发自然落到别的号。判据与
// anthropicTurnLooksSuspiciouslyShort 的空回合分支必须一致，所以直接复用它做前置。
func anthropicTurnIsEmptyAnswer(
	stopReason string,
	proseRunes int,
	outputTokens int,
	sawToolUseBlock bool,
) bool {
	// 刻意传 thinkingRunes=0：空回合走的是 proseRunes==0 那条短路，与思考长度无关。
	// 传 0 保证这里判的严格是「空」，不会把「想很久+几个字」也算成空回合去罚号——
	// 那一档只解绑、只丢弃，不进冷却。
	return proseRunes == 0 &&
		anthropicTurnLooksSuspiciouslyShort(stopReason, proseRunes, outputTokens, sawToolUseBlock, 0)
}

// anthropicTurnProvesUpstreamHealthy 判定这一回合能否作为「上游把话说完了」的正面证据，
// 也就是能不能拿它去清零短回合连击数。
//
// 为什么不能直接用 !anthropicTurnLooksSuspiciouslyShort 当清零条件：agent 流量里每次
// 截断后面必然跟着一串 tool_use 回合（客户端去读日志、抓数据、再问一次），而 tool_use
// 回合在 anthropicTurnLooksSuspiciouslyShort 里直接 return false，于是落进 else 分支
// 把连击清零。结果 streak 永远停在 1，永远到不了阈值 2——解绑在真实 agent 流量下仍是
// 死代码，两条链路都一样。线上实证：账号 9 在 21:23:19（out=63/chars=132）与 21:26:34
// （out=34/chars=81）各截断一次，中间隔着几发 tool_use，日志里只留下 streak=1。
//
// 所以清零要的是正面证据：这一回合确实产出了成段正文，**且** output_tokens 也确实上了
// 量。两个都要，是因为 22:31:21 那一发就是只满足了「字节数够多」（131 个汉字 ≈289 字节）
// 就把连击清零的——单看长度会被中文的字节膨胀骗过去，加上 token 数才骗不过。
//
// token 那道闸门必须与 anthropicTurnLooksSuspiciouslyShort **共用同一个常量、且同样比
// 折算值**，这不是巧合而是三态互斥的构造：可疑要求 folded <= 上限，清零要求 folded > 上限，
// 同一个常量下两者是直接互否，不可能同时成立。一旦拆成两个数（试过 128/320），out 落在
// 129~320 且正文 201~800 的回合会同时命中两个判定，调用点 if/else-if 的书写顺序就变成
// 隐式优先级，改动顺序会静默改变行为——TestShortTurnPredicatesAreMutuallyExclusive 正是
// 为了钉死这一点。
//
// 同理必须收 thinkingRunes：可疑侧自 2026-08-24 起比的是 anthropicVisibleOutputTokens
// 折算值，这一侧要是继续比原值，互斥就已经破了。prose 300 / thinking 900 / out 1000
// 折算成 250 判可疑，原值 1000 又判成健康，两边同时为真。互斥测试当时枚举 thinking 恒为 0，
// 折算在那种回合上恒等于原值，所以看不见这个洞。
func anthropicTurnProvesUpstreamHealthy(
	stopReason string,
	proseRunes int,
	outputTokens int,
	sawToolUseBlock bool,
	thinkingRunes int,
) bool {
	// 白名单刻意比 anthropicStopReasonIsHealthy 窄。那个白名单答的是「这条流算不算残缺」，
	// 含 tool_use 和 pause_turn；这里答的是「这一回合把话说完了没有」，那两个恰恰都表示
	// 回合还没说完——tool_use 要等工具结果再续，pause_turn 是协议级续传。把它们当成正面
	// 证据就会重演「tool_use 清零连击」这个 bug。
	switch strings.ToLower(strings.TrimSpace(stopReason)) {
	case "end_turn", "max_tokens", "stop_sequence":
	default:
		return false
	}
	if sawToolUseBlock {
		return false
	}
	if anthropicVisibleOutputTokens(outputTokens, proseRunes, thinkingRunes) <=
		anthropicShortTurnOutputTokenLimit {
		return false
	}
	return proseRunes > anthropicHealthyTurnMinProseRunes
}

// noteAnthropicShortTurnStreak 累计可疑短回合，达到阈值就解除本会话的粘性绑定，
// 并返回这一次是否真的解绑了。
//
// 为什么先解绑而不是直接罚号：罚号会把账号从所有会话的调度池里摘掉，而这里的判据是
// 启发式的，每一次观测都罚的误判代价太大——单发短回答有「问题本来就只有一句话答案」
// 这种合法解释。解绑只影响这一条会话的账号亲和性，误判代价仅是丢一次 prompt cache。
//
// 返回值给调用方用来补一次账号冷却：解绑本身治不了「坏号正好是首选号」的情况，见
// reportAnthropicShortTurnUnbind。未达阈值的观测返回 false，仍然零惩罚。
func (s *GatewayService) noteAnthropicShortTurnStreak(
	ctx context.Context, account *Account, model string, proseRunes, outputTokens int,
) bool {
	groupID, sessionKey, ok := StickySessionScopeFromContext(ctx)
	if !ok {
		return false
	}
	store, ok := s.cache.(stickyShortTurnStreakStore)
	if !ok {
		return false
	}
	streak, err := store.IncrStickyShortTurnStreak(ctx, groupID, sessionKey, stickySessionTTL)
	if err != nil {
		slog.Warn("sticky_short_turn_incr_failed", "account_id", account.ID, "error", err)
		return false
	}
	if streak < anthropicShortTurnStreakThreshold {
		slog.Info("sticky_short_turn_observed",
			"account_id", account.ID, "model", model, "streak", streak,
			"prose_runes", proseRunes, "output_tokens", outputTokens)
		return false
	}
	if err := s.cache.DeleteSessionAccountID(ctx, groupID, sessionKey); err != nil {
		slog.Warn("sticky_short_turn_unbind_failed", "account_id", account.ID, "error", err)
		return false
	}
	// 解绑后清零：否则下一发换到好账号上答了一句短话就又达标，会来回解绑。
	if err := store.ResetStickyShortTurnStreak(ctx, groupID, sessionKey); err != nil {
		slog.Warn("sticky_short_turn_reset_failed", "account_id", account.ID, "error", err)
	}
	slog.Warn("sticky_short_turn_unbound",
		"account_id", account.ID, "model", model, "streak", streak,
		"prose_runes", proseRunes, "output_tokens", outputTokens,
		"reason", "consecutive protocol-legal but suspiciously short turns")
	return true
}

// clearAnthropicShortTurnStreak 在一次正常回合后清零连击数。
func (s *GatewayService) clearAnthropicShortTurnStreak(ctx context.Context) {
	groupID, sessionKey, ok := StickySessionScopeFromContext(ctx)
	if !ok {
		return
	}
	store, ok := s.cache.(stickyShortTurnStreakStore)
	if !ok {
		return
	}
	if err := store.ResetStickyShortTurnStreak(ctx, groupID, sessionKey); err != nil {
		slog.Warn("sticky_short_turn_reset_failed", "error", err)
	}
}

const anthropicEmptyStreamUpstreamMessage = "Anthropic upstream returned an empty SSE stream with no terminal event"
const anthropicIncompleteStreamUpstreamMessage = "Anthropic upstream delivered a terminal event but the content was incomplete"

const anthropicTruncatedStreamUpstreamMessage = "Anthropic upstream truncated the SSE stream after it was committed to the client"

const anthropicEmptyAnswerTurnUpstreamMessage = "Anthropic upstream ended the turn with no visible answer"

const anthropicShortTurnUnbindUpstreamMessage = "Anthropic upstream repeatedly ended turns after unusually short replies"

const anthropicUnfailedOverRefusalUpstreamMessage = "Anthropic upstream returned a safeguards refusal that could no longer be failed over"

func anthropicEmptyStreamErrorBody() []byte {
	body, err := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "upstream_error",
			"code":    "anthropic_empty_stream",
			"message": anthropicEmptyStreamUpstreamMessage,
		},
	})
	if err != nil {
		return []byte(`{"type":"error","error":{"type":"upstream_error","code":"anthropic_empty_stream","message":"Anthropic upstream returned an empty SSE stream with no terminal event"}}`)
	}
	return body
}

// reportStreamTruncatedAfterCommit 记录「流已提交给客户端后被上游截断」。
//
// 这类失败无法在本次请求内 failover：字节已经 flush，200 已经钉死，改写只会腐化
// 响应（handler 层的 c.Writer.Size() 守卫正是为此）。但它仍然是确定的上游故障，
// 必须留下两个痕迹，否则客户端紧接着发来的重试会被粘性送回同一个坏账号：
//  1. ops 错误看板可见的归因记录——否则 ops_error_logs 只有一行 upstream_status_code=null
//     的空壳，运维无法判断故障方；
//  2. 账号健康度惩罚，与空闲超时同一套 stream_timeout_settings 开关/阈值，
//     达阈后账号临时不可调度，下一次请求自然落到别的账号。
//
// 调用方必须已排除客户端侧原因（clientDisconnected / ctx 取消）与本地配置原因
// （bufio.ErrTooLong）：那些换号无用，罚账号只会误伤。
func (s *GatewayService) reportStreamTruncatedAfterCommit(ctx context.Context, c *gin.Context, resp *http.Response, account *Account, model, reason string) {
	if account == nil {
		return
	}
	upstreamRequestID := ""
	if resp != nil {
		upstreamRequestID = resp.Header.Get("x-request-id")
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: http.StatusBadGateway,
		UpstreamRequestID:  upstreamRequestID,
		Kind:               "stream_truncated_after_commit",
		Message:            anthropicTruncatedStreamUpstreamMessage,
		Detail:             reason,
	})
	logger.LegacyPrintf("service.gateway", "[Anthropic Passthrough] stream truncated after commit, no failover possible: account=%d(%s) model=%s request_id=%s reason=%s",
		account.ID, account.Name, model, upstreamRequestID, reason)
	if s.rateLimitService != nil {
		s.rateLimitService.HandleStreamTruncated(ctx, account, model)
	}
}

// reportAnthropicEmptyAnswerTurn 归因并冷却「协议完整但零正文」的回合。
//
// 与 reportStreamTruncatedAfterCommit 共用 stream_timeout_settings 那一套开关/阈值/
// 计数窗口（运维只需一个旋钮），只在 Kind 与文案上区分 —— 对上游来说这两件事是同一类
// 故障：这一发没有产出可用输出。线上该设置是 threshold_count=1 / temp_unsched 1 分钟，
// 所以第一次空回合就把账号冷却一分钟，下一发自然落到别的号。
//
// 为什么这一档一次就罚，而短回合要等连击到阈值：短回合有「用户问的问题本来就只有一句话
// 答案」这种合法解释，逐发罚会把健康账号摘下线；空回合没有对应的合法解释。实测对照也支持
// 这个判断，见 anthropicTurnLooksSuspiciouslyShort 的注释。短回合那一档的口径见
// reportAnthropicShortTurnUnbind。
//
// 调用点两处，覆盖「拦下来了」和「没拦住」两种结局：持流丢弃分支（客户端零暴露）与
// reportIfTerminalButIncomplete（持流关闭/窗口耗尽/重试额度已用，响应已交付）。
// 两处都与 reportAnthropicShortTurnUnbind 二选一，不能对同一故障重复归因。
func (s *GatewayService) reportAnthropicEmptyAnswerTurn(
	ctx context.Context, c *gin.Context, resp *http.Response, account *Account, model string,
	outputTokens int, stopReason, disposition string,
) {
	if account == nil {
		return
	}
	upstreamRequestID := ""
	if resp != nil {
		upstreamRequestID = resp.Header.Get("x-request-id")
	}
	detail := fmt.Sprintf("stop_reason=%s output_tokens=%d prose_runes=0 disposition=%s",
		stopReason, outputTokens, disposition)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: http.StatusBadGateway,
		UpstreamRequestID:  upstreamRequestID,
		Kind:               "empty_answer_turn",
		Message:            anthropicEmptyAnswerTurnUpstreamMessage,
		Detail:             detail,
	})
	slog.Warn("anthropic_empty_answer_turn",
		"account_id", account.ID, "account_name", account.Name, "model", model,
		"stop_reason", stopReason, "output_tokens", outputTokens,
		"disposition", disposition, "upstream_request_id", upstreamRequestID)
	if s.rateLimitService != nil {
		s.rateLimitService.HandleStreamTruncated(ctx, account, model)
	}
}

// reportAnthropicShortTurnUnbind 归因并冷却「连击到阈值、刚刚被解绑」的账号。
//
// 为什么解绑之后还要罚号：解绑只是删掉会话到账号的亲和记录，下一发要落到哪由调度决定，
// 而调度是按 priority 升序取的（account_repo.go 的 ORDER BY ... priority ASC）。坏号
// 如果正好是本组 priority 最小的那个，删掉绑定后下一发照旧选回它 —— 解绑成了空操作。
// 线上实测到过这一幕：解绑后紧接着的那一发又回到同一个号上，继续断。
//
// 罚号的口径刻意收在「真的解绑了」这一刻，而不是每次可疑观测：单发短回合有「问题本来
// 就只有一句话答案」这种合法解释，逐发罚会把健康账号摘下线；连击到阈值意味着这条会话
// 上已经连续出现，合法解释站不住了。未达阈值的观测仍然零惩罚，见
// noteAnthropicShortTurnStreak。
//
// 与空回合那一档共用 stream_timeout_settings（运维一个旋钮），只在 Kind 与文案上区分。
// 线上配置 threshold_count=1 / temp_unsched 1 分钟，所以解绑那一刻账号冷却一分钟，
// 足够让下一发绕开它，又不至于长时间少一个号。
//
// 调用方必须与 reportAnthropicEmptyAnswerTurn 二选一：空回合已经自带冷却，两个都调
// 会让同一次故障在计数窗口里记两次。
func (s *GatewayService) reportAnthropicShortTurnUnbind(
	ctx context.Context, c *gin.Context, resp *http.Response, account *Account, model string,
	proseRunes, outputTokens int, stopReason, disposition string,
) {
	if account == nil {
		return
	}
	upstreamRequestID := ""
	if resp != nil {
		upstreamRequestID = resp.Header.Get("x-request-id")
	}
	detail := fmt.Sprintf("stop_reason=%s prose_runes=%d output_tokens=%d disposition=%s",
		stopReason, proseRunes, outputTokens, disposition)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: http.StatusBadGateway,
		UpstreamRequestID:  upstreamRequestID,
		Kind:               "short_turn_streak_unbind",
		Message:            anthropicShortTurnUnbindUpstreamMessage,
		Detail:             detail,
	})
	slog.Warn("anthropic_short_turn_streak_unbind",
		"account_id", account.ID, "account_name", account.Name, "model", model,
		"stop_reason", stopReason, "prose_runes", proseRunes, "output_tokens", outputTokens,
		"disposition", disposition, "upstream_request_id", upstreamRequestID)
	if s.rateLimitService != nil {
		s.rateLimitService.HandleStreamTruncated(ctx, account, model)
	}
}

const anthropicBlockOrderViolationUpstreamMessage = "Anthropic upstream emitted content blocks out of protocol order"

// unbindStickySessionNow 立刻删掉会话到账号的亲和记录，不看短回合连击阈值。
//
// 与 noteAnthropicShortTurnStreak 的分工：那个是给启发式判据用的，单发可疑不足以定罪，
// 所以要先攒连击到 anthropicShortTurnStreakThreshold 才解绑。确定性判据（块序违规）没有
// 「攒证据」这一步，一次就该解绑，不能受那个阈值常量的取值影响——今天它是 1、两者行为恰好
// 相同，但把确定性判据挂在一个随时可能被调大的启发式阈值上是错的耦合。
//
// 解绑后同样把连击计数清零：坏号已经被摘掉，下一发落到好号上答一句短话不该立刻又达标。
func (s *GatewayService) unbindStickySessionNow(ctx context.Context, account *Account, model, reason string) bool {
	if account == nil || s.cache == nil {
		return false
	}
	groupID, sessionKey, ok := StickySessionScopeFromContext(ctx)
	if !ok {
		return false
	}
	if err := s.cache.DeleteSessionAccountID(ctx, groupID, sessionKey); err != nil {
		slog.Warn("sticky_unbind_failed", "account_id", account.ID, "reason", reason, "error", err)
		return false
	}
	if store, ok := s.cache.(stickyShortTurnStreakStore); ok {
		if err := store.ResetStickyShortTurnStreak(ctx, groupID, sessionKey); err != nil {
			slog.Warn("sticky_short_turn_reset_failed", "account_id", account.ID, "error", err)
		}
	}
	slog.Warn("sticky_unbound",
		"account_id", account.ID, "account_name", account.Name, "model", model, "reason", reason)
	return true
}

// reportAnthropicBlockOrderViolation 归因并冷却「块序违规」的账号。
//
// 判据见 anthropicContentBlockOrderTracker：thinking 块出现在 text 块之后，协议上不可能。
//
// 为什么一次就罚，与空回合同档而不是走短回合的连击：块序违规是确定性矛盾，不存在「用户问的
// 问题本来就该这样答」这类合法解释，攒连击只会让第二发也吃到伪造输出。实测误报面 1/5，
// 且那 1 次是 msg_bdrk_* 前缀的另一个上游实现，不在本网关的坏号池里。
//
// 走同一个 HandleStreamTruncated / stream_timeout_settings：对客户端的后果与截断同类
// （拿到一条不能用的响应），运维只需一个旋钮，ops 看板上用 Kind 区分归因。
//
// 调用点两处，覆盖「拦下来了」和「没拦住」两种结局：持流丢弃分支（disposition=discarded，
// 客户端零暴露）与 reportIfTerminalButIncomplete（disposition=delivered，持流关闭/窗口
// 耗尽/丢弃额度已用，响应已经交付）。两处都与另外两个报告器互斥，同一次故障不能记两次；
// 块序丢弃额度与启发式额度独立维护。
func (s *GatewayService) reportAnthropicBlockOrderViolation(
	ctx context.Context, c *gin.Context, resp *http.Response, account *Account, model string,
	proseRunes, outputTokens int, stopReason, disposition string,
) {
	if account == nil {
		return
	}
	upstreamRequestID := ""
	if resp != nil {
		upstreamRequestID = resp.Header.Get("x-request-id")
	}
	detail := fmt.Sprintf("stop_reason=%s prose_runes=%d output_tokens=%d disposition=%s violation=thinking_after_text",
		stopReason, proseRunes, outputTokens, disposition)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: http.StatusBadGateway,
		UpstreamRequestID:  upstreamRequestID,
		Kind:               "block_order_violation",
		Message:            anthropicBlockOrderViolationUpstreamMessage,
		Detail:             detail,
	})
	slog.Warn("anthropic_block_order_violation",
		"account_id", account.ID, "account_name", account.Name, "model", model,
		"stop_reason", stopReason, "prose_runes", proseRunes, "output_tokens", outputTokens,
		"disposition", disposition, "upstream_request_id", upstreamRequestID)
	if s.rateLimitService != nil {
		s.rateLimitService.HandleStreamTruncated(ctx, account, model)
	}
}

// reportStreamIncompleteAfterCommit 记录「协议层收尾了但内容不完整」。
//
// 与 reportStreamTruncatedAfterCommit 并列，区别只在判定依据：那边是流断在半路
// （连 message_stop 都没来），这边是 message_stop 到了、但 stop_reason 异常或正文为空。
// 后者过去完全没有信号——sawTerminalEvent=true 就直接 return nil，账号不罚，粘性把
// 下一发重试送回同一个坏号，于是「一直断流却从不主副切换」。
//
// 走同一个 HandleStreamTruncated：两者对客户端的后果一样（拿到一条用不了的响应），
// 运维也只需一个旋钮（stream_timeout_settings）。ops 看板上用 Kind 区分归因。
func (s *GatewayService) reportStreamIncompleteAfterCommit(ctx context.Context, c *gin.Context, resp *http.Response, account *Account, model, reason string) {
	if account == nil {
		return
	}
	upstreamRequestID := ""
	if resp != nil {
		upstreamRequestID = resp.Header.Get("x-request-id")
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: http.StatusBadGateway,
		UpstreamRequestID:  upstreamRequestID,
		Kind:               "stream_incomplete_after_commit",
		Message:            anthropicIncompleteStreamUpstreamMessage,
		Detail:             reason,
	})
	logger.LegacyPrintf("service.gateway", "[Anthropic Passthrough] terminal event but incomplete content, no failover possible: account=%d(%s) model=%s request_id=%s reason=%s",
		account.ID, account.Name, model, upstreamRequestID, reason)
	if s.rateLimitService != nil {
		s.rateLimitService.HandleStreamTruncated(ctx, account, model)
	}
}

// reportSafetyRefusalWithoutFailover 记录「安全拒答到达时已经切不了号」。
//
// 两个调用现场：透传流在 prelude 提交之后才收到 refusal 帧（此时 200 已钉死，
// newAnthropicSafetyFailoverError 那条真切号的路走不了了）；以及非透传流——它压根没有
// 提交前拒答检测，任何位置的 refusal 都只能事后归因。
//
// 必须留痕的理由与 reportStreamTruncatedAfterCommit 完全一致：不罚账号的话，客户端紧接着
// 的重试会被粘性原样送回同一个坏账号，表现为「一直报 safeguards 却从不主副切换」。实测拒答
// 是**账号特有**的（主号每帧被拒的同时，副号的同一请求成功），所以罚号换号确实有效。
// 沿用 stream_timeout_settings 的开关/阈值/窗口（运维只需一个旋钮），只在 keyword 与文案上
// 区分；线上阈值为 1 = 第一次拒答就把该账号冷却 1 分钟，下一发自然落到副号。
//
// 极端情形（提示词本身违规导致所有账号都拒）会让各账号各自冷却一个阈值窗口，代价可接受：
// 冷却是分钟级临时态，且客户端此时无论换到哪个号都拿不到有效响应。
func (s *GatewayService) reportSafetyRefusalWithoutFailover(ctx context.Context, c *gin.Context, resp *http.Response, account *Account, model, reason string, body []byte) {
	if account == nil {
		return
	}
	statusCode := http.StatusForbidden
	upstreamRequestID := ""
	if resp != nil {
		upstreamRequestID = resp.Header.Get("x-request-id")
		if resp.StatusCode != 0 && resp.StatusCode != http.StatusOK {
			statusCode = resp.StatusCode
		}
	}
	message := strings.TrimSpace(extractUpstreamErrorMessage(body))
	if message == "" {
		message = anthropicUnfailedOverRefusalUpstreamMessage
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: statusCode,
		UpstreamRequestID:  upstreamRequestID,
		Kind:               "safety_refusal_no_failover",
		Message:            sanitizeUpstreamErrorMessage(message),
		Detail:             reason,
	})
	logger.LegacyPrintf("service.gateway", "[Anthropic] safeguards refusal, too late to failover: account=%d(%s) model=%s request_id=%s reason=%s body=%s",
		account.ID, account.Name, model, upstreamRequestID, reason, truncateString(string(body), 1000))
	if s.rateLimitService != nil {
		s.rateLimitService.HandleStreamRefused(ctx, account, model)
	}
}

// newAnthropicEmptyStreamFailoverError 把「上游 200 但整条 SSE 只送来非提交性前奏
// （message_start / ping / message_delta）后就断流」标记为可 failover 的上游异常。
//
// 这类响应对客户端完全无用——Claude Code 只会报 "API returned an empty or malformed
// response (HTTP 200)"。而它必然发生在 prelude 还没写出客户端之前（keepalive 在
// !streamCommitted 时只重置定时器不写字节，响应头也只落在 c.Header() 的 map 里），
// 所以此刻 failover 窗口仍然完好，必须切号而不是把半条流 flush 给客户端钉死成 200。
//
// 不设 RetryableOnSameAccount：空流往往伴随上游长时间挂起，在同一账号上重试只会
// 把停顿时间成倍放大；Scope=Account 会照常把这次失败计入调度健康度。
func (s *GatewayService) newAnthropicEmptyStreamFailoverError(c *gin.Context, resp *http.Response, account *Account, reason string) *UpstreamFailoverError {
	upstreamRequestID := ""
	headers := http.Header{}
	if resp != nil {
		upstreamRequestID = resp.Header.Get("x-request-id")
		headers = resp.Header.Clone()
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: http.StatusBadGateway,
		UpstreamRequestID:  upstreamRequestID,
		Kind:               "empty_stream_failover",
		Message:            anthropicEmptyStreamUpstreamMessage,
		Detail:             reason,
	})
	logger.LegacyPrintf("service.gateway", "[Anthropic Passthrough] empty stream before commit, failover account=%d(%s) request_id=%s reason=%s",
		account.ID, account.Name, upstreamRequestID, reason)
	return &UpstreamFailoverError{
		StatusCode:      http.StatusBadGateway,
		ResponseBody:    anthropicEmptyStreamErrorBody(),
		ResponseHeaders: headers,
		Scope:           GatewayFailureScopeAccount,
		Reason:          GatewayFailureReason("anthropic_empty_stream"),
	}
}

// anthropicHoldbackDiscardsKey 记录本次客户端请求已经因为持流判定丢弃过几次上游响应。
// 放在 gin.Context 上而不是 GatewayService 上：作用域必须是「一次客户端请求」，
// 跨账号重试共享同一个 gin.Context，正是需要的粒度。
//
// 是计数而不是布尔：布尔只能表达「额度用过了」，而额度按形态分档（空回合比短回合宽）。
// 共用一个布尔时，第一个坏号无论什么形态都把额度吃光，第二个坏号的空响应必然放行——
// 那正是 4 条 disposition=delivered 的成因，见 anthropicEmptyAnswerDiscardBudget。
const anthropicHoldbackDiscardsKey = "anthropic_holdback_discards"

func anthropicHoldbackDiscardsUsed(c *gin.Context) int {
	if c == nil {
		return 0
	}
	v, ok := c.Get(anthropicHoldbackDiscardsKey)
	if !ok {
		return 0
	}
	used, _ := v.(int)
	return used
}

func noteAnthropicHoldbackDiscard(c *gin.Context) {
	if c != nil {
		c.Set(anthropicHoldbackDiscardsKey, anthropicHoldbackDiscardsUsed(c)+1)
	}
}

const anthropicBlockOrderDiscardsKey = "anthropic_block_order_discards"

func anthropicBlockOrderDiscardsUsed(c *gin.Context) int {
	if c == nil {
		return 0
	}
	v, ok := c.Get(anthropicBlockOrderDiscardsKey)
	if !ok {
		return 0
	}
	used, _ := v.(int)
	return used
}

func noteAnthropicBlockOrderDiscard(c *gin.Context) {
	if c != nil {
		c.Set(anthropicBlockOrderDiscardsKey, anthropicBlockOrderDiscardsUsed(c)+1)
	}
}

// anthropicClientRequestStartKey 记录**本次客户端请求**的起点，跨 failover 尝试共享。
//
// 为什么不能用每次尝试自己的 startTime：那个每换一次号就重新取一次，导致以它为锚的死气
// 上限跟着归零。客户端在同一个 HTTP 请求上感知的是累加值，锚点必须和它同寿命。
// gin.Context 的生命周期正好是一次客户端请求，与 anthropicHoldbackDiscardsKey 同理。
const anthropicClientRequestStartKey = "anthropic_client_request_start"

// anthropicClientRequestStartedAt 取本次客户端请求的起点；首次调用用 fallback 播种。
//
// 首次尝试拿自己的 startTime 播种，之后每次 failover 尝试读到的都是同一个值。c 为 nil
// （单测直接构造 observer）时退化成 fallback，等价于「只有一次尝试」，不影响判据。
func anthropicClientRequestStartedAt(c *gin.Context, fallback time.Time) time.Time {
	if c == nil {
		return fallback
	}
	if v, ok := c.Get(anthropicClientRequestStartKey); ok {
		if seeded, ok := v.(time.Time); ok && !seeded.IsZero() {
			return seeded
		}
	}
	c.Set(anthropicClientRequestStartKey, fallback)
	return fallback
}

const anthropicShortTurnHoldbackMessage = "Anthropic upstream ended the turn after an unusually short reply"

func anthropicShortTurnHoldbackErrorBody() []byte {
	body, err := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "upstream_error",
			"code":    "anthropic_short_turn_holdback",
			"message": anthropicShortTurnHoldbackMessage,
		},
	})
	if err != nil {
		return []byte(`{"type":"error","error":{"type":"upstream_error","code":"anthropic_short_turn_holdback","message":"Anthropic upstream ended the turn after an unusually short reply"}}`)
	}
	return body
}

// newAnthropicShortTurnFailoverError 把「协议合法但疑似截断，且提交窗口仍完好」标记为
// 可 failover 的上游异常，让这条响应在写给客户端之前就被换号重试掉。
//
// 与 noteAnthropicShortTurnStreak 的分工：那个是事后解绑，只能让**下一发**落到别的账号，
// 治不了当前这一发；这个发生在提交之前，客户端根本看不到截断内容——这才是零暴露。
//
// 刻意不罚账号：判据是启发式的，Scope=Request + RequestScopedTransient 是这个仓库里
// 「故障与账号健康无关」的既有标记，会让 TempUnscheduleRetryableError 直接 return，
// 也让 ShouldReportAccountScheduleFailure 不把它算进调度健康度。换号本身由
// FailedAccountIDs 保证——本次请求不会再选回这个账号。
//
// 不设 RetryableOnSameAccount：同一个坏中转上重试只会再截断一次。
func (s *GatewayService) newAnthropicShortTurnFailoverError(
	c *gin.Context, resp *http.Response, account *Account, model string, proseRunes, outputTokens int, stopReason string,
) *UpstreamFailoverError {
	noteAnthropicHoldbackDiscard(c)
	upstreamRequestID := ""
	headers := http.Header{}
	if resp != nil {
		upstreamRequestID = resp.Header.Get("x-request-id")
		headers = resp.Header.Clone()
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: http.StatusBadGateway,
		UpstreamRequestID:  upstreamRequestID,
		Kind:               "short_turn_holdback_failover",
		Message:            anthropicShortTurnHoldbackMessage,
		Detail: fmt.Sprintf("stop_reason=%s prose_runes=%d output_tokens=%d",
			stopReason, proseRunes, outputTokens),
	})
	slog.Warn("anthropic_short_turn_holdback_failover",
		"account_id", account.ID, "account_name", account.Name, "model", model,
		"stop_reason", stopReason, "prose_runes", proseRunes, "output_tokens", outputTokens,
		"upstream_request_id", upstreamRequestID)
	return &UpstreamFailoverError{
		StatusCode:             http.StatusBadGateway,
		ResponseBody:           anthropicShortTurnHoldbackErrorBody(),
		ResponseHeaders:        headers,
		Scope:                  GatewayFailureScopeRequest,
		RequestScopedTransient: true,
		Reason:                 GatewayFailureReason("anthropic_short_turn_holdback"),
	}
}

func anthropicBlockOrderHoldbackErrorBody() []byte {
	body, err := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "upstream_error",
			"code":    "anthropic_block_order_violation",
			"message": anthropicBlockOrderViolationUpstreamMessage,
		},
	})
	if err != nil {
		return []byte(`{"type":"error","error":{"type":"upstream_error","code":"anthropic_block_order_violation","message":"Anthropic upstream emitted content blocks out of protocol order"}}`)
	}
	return body
}

// newAnthropicBlockOrderFailoverError 把「块序违规且提交窗口仍完好」标记为可 failover 的
// 上游异常，让这条响应在写给客户端之前就被换号重试掉。
//
// 与 newAnthropicShortTurnFailoverError 同一套语义（请求域瞬时失败、带上游响应头、
// 记一次 holdback discard），只在 Kind / code / 文案上区分，好让 ops 看板能把「伪造回合」
// 这一类单独统计出来——它和短回合的根因不同，混在一起就看不出某个上游是不是在拼错块序。
func (s *GatewayService) newAnthropicBlockOrderFailoverError(
	c *gin.Context, resp *http.Response, account *Account, model string, proseRunes, outputTokens int, stopReason string,
) *UpstreamFailoverError {
	noteAnthropicBlockOrderDiscard(c)
	upstreamRequestID := ""
	headers := http.Header{}
	if resp != nil {
		upstreamRequestID = resp.Header.Get("x-request-id")
		headers = resp.Header.Clone()
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: http.StatusBadGateway,
		UpstreamRequestID:  upstreamRequestID,
		Kind:               "block_order_holdback_failover",
		Message:            anthropicBlockOrderViolationUpstreamMessage,
		Detail: fmt.Sprintf("stop_reason=%s prose_runes=%d output_tokens=%d violation=thinking_after_text",
			stopReason, proseRunes, outputTokens),
	})
	slog.Warn("anthropic_block_order_holdback_failover",
		"account_id", account.ID, "account_name", account.Name, "model", model,
		"stop_reason", stopReason, "prose_runes", proseRunes, "output_tokens", outputTokens,
		"upstream_request_id", upstreamRequestID)
	return &UpstreamFailoverError{
		StatusCode:             http.StatusBadGateway,
		ResponseBody:           anthropicBlockOrderHoldbackErrorBody(),
		ResponseHeaders:        headers,
		Scope:                  GatewayFailureScopeRequest,
		RequestScopedTransient: true,
		Reason:                 GatewayFailureReason("anthropic_block_order_violation"),
	}
}

func (s *GatewayService) buildUpstreamRequestAnthropicAPIKeyPassthrough(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	token string,
) (*http.Request, []byte, error) {
	body = stripDeferredToolCacheControl(body)
	targetURL := claudeAPIURL
	baseURL := account.GetBaseURL()
	if baseURL != "" {
		validatedURL, err := s.validateUpstreamBaseURL(baseURL)
		if err != nil {
			return nil, nil, err
		}
		targetURL = validatedURL + "/v1/messages?beta=true"
	}

	// 能力维度 body sanitize：透传路径上 anthropic-beta header 原样透传客户端值，
	// 依此决定是否保留 body 中的 context_management。避免“客户端 body 带字段但
	// header 忘记带 beta token”的客户端 bug 在透传场景下让上游 400。
	clientBeta := ""
	if c != nil && c.Request != nil {
		clientBeta = getHeaderRaw(c.Request.Header, "anthropic-beta")
	}
	// 账号覆写了 anthropic-beta 时，覆写值即最终上游值：净化以覆写值为准
	if beta, ok := account.HeaderOverrideValue("anthropic-beta"); ok {
		clientBeta = beta
	}
	if sanitized, changed := sanitizeAnthropicBodyForBetaTokens(body, clientBeta); changed {
		body = sanitized
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}

	if c != nil && c.Request != nil {
		for key, values := range c.Request.Header {
			lowerKey := strings.ToLower(strings.TrimSpace(key))
			if !allowedHeaders[lowerKey] {
				continue
			}
			wireKey := resolveWireCasing(key)
			for _, v := range values {
				addHeaderRaw(req.Header, wireKey, v)
			}
		}
	}

	// 覆盖入站鉴权残留，并注入上游认证
	req.Header.Del("authorization")
	req.Header.Del("x-api-key")
	req.Header.Del("x-goog-api-key")
	req.Header.Del("cookie")
	setAnthropicAPIKeyAuthHeader(req.Header, account, token)

	if getHeaderRaw(req.Header, "content-type") == "" {
		setHeaderRaw(req.Header, "content-type", "application/json")
	}
	if getHeaderRaw(req.Header, "anthropic-version") == "" {
		setHeaderRaw(req.Header, "anthropic-version", "2023-06-01")
	}

	// 账号级请求头覆写（最终生效，覆盖上面所有来源的同名头）
	account.ApplyHeaderOverrides(req.Header)

	return req, body, nil
}

func (s *GatewayService) handleStreamingResponseAnthropicAPIKeyPassthrough(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	startTime time.Time,
	model string,
) (*streamingResult, error) {
	observer := upstreamResponseModelObserverFromContext(c)
	if observer == nil {
		observer = beginUpstreamResponseModelObservation(c)
	}
	if s.rateLimitService != nil {
		s.rateLimitService.UpdateSessionWindow(ctx, account, resp.Header)
	}

	writeAnthropicPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)

	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "text/event-stream"
	}
	c.Header("Content-Type", contentType)
	if c.Writer.Header().Get("Cache-Control") == "" {
		c.Header("Cache-Control", "no-cache")
	}
	if c.Writer.Header().Get("Connection") == "" {
		c.Header("Connection", "keep-alive")
	}
	c.Header("X-Accel-Buffering", "no")
	if v := resp.Header.Get("x-request-id"); v != "" {
		c.Header("x-request-id", v)
	}

	w := c.Writer
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming not supported")
	}

	usage := &ClaudeUsage{}
	var firstTokenMs *int
	clientDisconnected := false
	sawTerminalEvent := false
	// 启发式截断判定用的三个观测量：协议层收尾了不等于内容完整，见
	// reportStreamIncompleteAfterCommit 的说明。
	sawStopReason := ""
	visibleChars := 0
	// proseRunes 与 visibleChars 分开累计：前者只数正文 rune，供短回合解绑判定用；
	// 后者含思考链与工具入参、单位是字节，只供会罚号的残缺判定判零。见
	// anthropicVisibleProseRunes 的说明。
	proseRunes := 0
	// thinkingRunes 单独累计：用来识别「想了很久却只吐几个字」，那一形态下 output_tokens
	// 被思考撑大，token 闸门恒为假。见 anthropicPostThinkingProseRuneCeiling。
	thinkingRunes := 0
	sawContentBlockStart := false
	sawToolUseBlock := false
	// blockOrder 与 holdback.blockOrder 是两份独立的跟踪器，理由同 anthropicHoldbackObserver
	// 的注释：持流期的帧要等 flushPendingPrelude 重放才过 processLine，共用会重复观测。
	// 这一份负责「没被持流拦住」的结局——持流关了、窗口耗尽、或丢弃额度已用完时，响应已经
	// 交付，只能事后解绑 + 罚号，让下一发绕开这个号。
	var blockOrder anthropicContentBlockOrderTracker

	scanner := bufio.NewScanner(resp.Body)
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	scanBuf := getSSEScannerBuf64K()
	scanner.Buffer(scanBuf[:0], maxLineSize)

	type scanEvent struct {
		line string
		err  error
	}
	events := make(chan scanEvent, 16)
	done := make(chan struct{})
	sendEvent := func(ev scanEvent) bool {
		select {
		case events <- ev:
			return true
		case <-done:
			return false
		}
	}
	var lastReadAt int64
	atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
	go func(scanBuf *sseScannerBuf64K) {
		defer putSSEScannerBuf64K(scanBuf)
		defer close(events)
		for scanner.Scan() {
			atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
			if !sendEvent(scanEvent{line: scanner.Text()}) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			_ = sendEvent(scanEvent{err: err})
		}
	}(scanBuf)
	defer close(done)

	streamInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamDataIntervalTimeout > 0 {
		streamInterval = time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
	}
	var intervalTicker *time.Ticker
	if streamInterval > 0 {
		intervalTicker = time.NewTicker(streamInterval)
		defer intervalTicker.Stop()
	}
	var intervalCh <-chan time.Time
	if intervalTicker != nil {
		intervalCh = intervalTicker.C
	}

	keepaliveInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamKeepaliveInterval > 0 {
		keepaliveInterval = time.Duration(s.cfg.Gateway.StreamKeepaliveInterval) * time.Second
	}
	var keepaliveTimer *time.Timer
	if keepaliveInterval > 0 {
		keepaliveTimer = time.NewTimer(keepaliveInterval)
		defer keepaliveTimer.Stop()
	}
	var keepaliveCh <-chan time.Time
	if keepaliveTimer != nil {
		keepaliveCh = keepaliveTimer.C
	}
	lastDataAt := time.Now()
	resetKeepaliveTimer := func() {
		if keepaliveTimer == nil {
			return
		}
		if !keepaliveTimer.Stop() {
			select {
			case <-keepaliveTimer.C:
			default:
			}
		}
		keepaliveTimer.Reset(keepaliveInterval)
	}
	inPartialEvent := false
	pendingPreludeLines := make([]string, 0, 12)
	streamCommitted := c.Writer.Written()
	refusalReported := false

	// 零暴露持流：把原本会提交响应的帧继续攒在 pendingPreludeLines 里，直到能判定这一
	// 回合是不是疑似截断。见 anthropicHoldbackVerdict。窗口配成 0 就完全退化成旧行为。
	holdbackWindow := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.AnthropicHoldbackWindowMs > 0 {
		holdbackWindow = time.Duration(s.cfg.Gateway.AnthropicHoldbackWindowMs) * time.Millisecond
	}
	holdbackActive := holdbackWindow > 0
	// 持流总时长上限，与静默窗口取先到者。窗口配 0（整个机制关掉）时这一条也不参与，
	// 免得上限把已经显式关掉的机制又拉起来。见 GatewayConfig.AnthropicHoldbackMaxHoldMs。
	holdbackMaxHold := time.Duration(0)
	if holdbackActive && s.cfg != nil && s.cfg.Gateway.AnthropicHoldbackMaxHoldMs > 0 {
		holdbackMaxHold = time.Duration(s.cfg.Gateway.AnthropicHoldbackMaxHoldMs) * time.Millisecond
	}
	holdbackDiscardsUsed := anthropicHoldbackDiscardsUsed(c)
	blockOrderDiscardsUsed := anthropicBlockOrderDiscardsUsed(c)
	// 客户端死气预算：三条截止线里唯一跨 failover 不归零的一条，见 deadAirElapsed。
	// 窗口配 0（整个机制关掉）时同样不参与，理由和 maxHold 一致。
	holdbackDeadAir := time.Duration(0)
	if holdbackActive && s.cfg != nil && s.cfg.Gateway.AnthropicHoldbackDeadAirBudgetMs > 0 {
		holdbackDeadAir = time.Duration(s.cfg.Gateway.AnthropicHoldbackDeadAirBudgetMs) * time.Millisecond
	}
	holdback := &anthropicHoldbackObserver{
		streamStartedAt: anthropicClientRequestStartedAt(c, startTime),
	}
	// 独立定时器，不复用 keepalive：窗口是毫秒级而 keepalive 默认 10 秒，靠它兜底会让
	// 「上游吐了两句就长时间静默」的流白等十秒。
	//
	// 定时器只是**唤醒器**，判据由 holdback.holdbackReleaseDeadline 单独持有：静默窗口量的
	// 是静默时长、会随新帧不断续期，而定时器一旦 arm 就按固定时长走，帧还在到达时它照样会
	// 开火。所以开火之后必须复核，复核不通过就按剩余时间续期，见 case <-holdbackCh。
	//
	// arm 的时长直接取 holdbackReleaseDeadline（三条截止线里先到的那个）到现在的差值，不再
	// 自己 min 一遍：口径只留一处，免得定时器与判定各算一套。
	//
	// 刻意不再要求 firstCommitPointAt 已就位：配了死气预算时，「上游只发了 message_start /
	// ping 这类不提交的帧然后彻底卡住」也必须能被唤醒。2026-08-25 14:10:30 那一发正是这个
	// 形态——out=0、客户端零字节 420 秒，前两条线全都没起算，唯一兜得住的是死气那一条。
	var holdbackTimer *time.Timer
	var holdbackCh <-chan time.Time
	armHoldbackTimer := func() {
		if !holdbackActive || holdbackTimer != nil {
			return
		}
		deadline := holdback.holdbackReleaseDeadline(holdbackWindow, holdbackMaxHold, holdbackDeadAir)
		if deadline.IsZero() {
			return
		}
		wake := time.Until(deadline)
		if wake <= 0 {
			wake = time.Millisecond
		}
		holdbackTimer = time.NewTimer(wake)
		holdbackCh = holdbackTimer.C
	}
	defer func() {
		if holdbackTimer != nil {
			holdbackTimer.Stop()
		}
	}()

	processLine := func(line string) {
		if data, ok := extractAnthropicSSEDataLine(line); ok {
			trimmed := strings.TrimSpace(data)
			observer.ObserveAnthropic([]byte(trimmed))
			// 提交之后才到的拒答：本次请求已无法切号（字节已 flush、200 已钉死），但仍必须
			// 留下归因与账号惩罚，否则客户端的下一发重试会被粘性送回同一个坏账号。提交之前
			// 到的拒答不会走到这里——它在 !streamCommitted 分支就 return 成真 failover 了。
			if !refusalReported && trimmed != "" && trimmed != "[DONE]" &&
				isAnthropicSafetyRefusalResponse(http.StatusForbidden, []byte(trimmed)) {
				refusalReported = true
				s.reportSafetyRefusalWithoutFailover(ctx, c, resp, account, model, "refusal arrived after stream commit", []byte(trimmed))
			}
			if anthropicStreamEventIsTerminal("", trimmed) {
				sawTerminalEvent = true
			}
			// 采集启发式判定所需的三个量。放在终止判定之后、写客户端之前，
			// 保证 message_delta 的 stop_reason 一定先于流结束被记下。
			if trimmed != "" && trimmed != "[DONE]" {
				parsedFrame := gjson.Parse(trimmed)
				switch strings.TrimSpace(parsedFrame.Get("type").String()) {
				case "message_delta":
					if r := strings.TrimSpace(parsedFrame.Get("delta.stop_reason").String()); r != "" {
						sawStopReason = r
					}
				case "content_block_start":
					sawContentBlockStart = true
					blockType := strings.TrimSpace(parsedFrame.Get("content_block.type").String())
					blockOrder.note(blockType)
					if strings.EqualFold(blockType, "tool_use") {
						sawToolUseBlock = true
					}
				}
				visibleChars += anthropicVisibleDeltaChars(parsedFrame)
				proseRunes += anthropicVisibleProseRunes(parsedFrame)
				thinkingRunes += anthropicThinkingRunes(parsedFrame)
			}
			if firstTokenMs == nil && trimmed != "" && trimmed != "[DONE]" {
				ms := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &ms
			}
			parseSSEUsagePassthrough(data, usage)
		} else {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "event:") && anthropicStreamEventIsTerminal(strings.TrimSpace(strings.TrimPrefix(trimmed, "event:")), "") {
				sawTerminalEvent = true
			}
		}

		if clientDisconnected {
			return
		}
		restored := string(reverseToolNamesIfPresent(c, []byte(line)))
		if _, err := io.WriteString(w, restored); err != nil {
			clientDisconnected = true
			logger.LegacyPrintf("service.gateway", "[Anthropic passthrough] Client disconnected during streaming, continue draining upstream for usage: account=%d", account.ID)
		} else if _, err := io.WriteString(w, "\n"); err != nil {
			clientDisconnected = true
			logger.LegacyPrintf("service.gateway", "[Anthropic passthrough] Client disconnected during streaming, continue draining upstream for usage: account=%d", account.ID)
		} else if line == "" {
			// 按 SSE 事件边界刷出，减少每行 flush 带来的 syscall 开销。
			flusher.Flush()
			lastDataAt = time.Now()
			resetKeepaliveTimer()
			inPartialEvent = false
		} else {
			inPartialEvent = true
		}
	}

	flushPendingPrelude := func() {
		for _, line := range pendingPreludeLines {
			processLine(line)
		}
		pendingPreludeLines = pendingPreludeLines[:0]
		streamCommitted = true
	}

	// reportIfTerminalButIncomplete 在「协议层收尾了」的成功返回点上再做一次内容完整性
	// 启发式判定。客户端自己断开、请求被取消的情形一律不判——那时残缺是客户端造成的，
	// 罚账号只会误伤。已按拒答归因过的流也不再判：同一次故障不能记两次。
	reportIfTerminalButIncomplete := func() {
		if clientDisconnected || refusalReported || c.Request.Context().Err() != nil {
			return
		}
		if blockOrder.violation() {
			s.unbindStickySessionNow(ctx, account, model, "content block order violation")
			s.reportAnthropicBlockOrderViolation(ctx, c, resp, account, model,
				proseRunes, usage.OutputTokens, sawStopReason, "delivered")
			return
		}
		reason, incomplete := anthropicStreamLooksIncompleteDespiteTerminal(
			sawStopReason, visibleChars, usage.OutputTokens, sawContentBlockStart)
		if !incomplete {
			// 确定性判定放行之后，再看这一回合是否属于「协议合法但疑似没把话说完」。
			// 三态：可疑 -> 累计连击；有正面证据 -> 清零；其余（典型是 tool_use 中间回合）
			// -> 不表态，保留连击。见 anthropicTurnProvesUpstreamHealthy。
			if anthropicTurnLooksSuspiciouslyShort(sawStopReason, proseRunes, usage.OutputTokens, sawToolUseBlock, thinkingRunes) {
				unbound := s.noteAnthropicShortTurnStreak(ctx, account, model, proseRunes, usage.OutputTokens)
				// 走到这里说明这一发没被持流拦住（窗口配 0、窗口耗尽、或本请求的重试额度
				// 已经用掉），客户端已经吃到一个残缺的 200。解绑只管下一发落在哪，账号本身
				// 还在池子里，所以要额外冷却账号。两个报告器只能二选一，避免同一次故障重复计数。
				switch {
				case anthropicTurnIsEmptyAnswer(sawStopReason, proseRunes, usage.OutputTokens, sawToolUseBlock):
					s.reportAnthropicEmptyAnswerTurn(ctx, c, resp, account, model,
						usage.OutputTokens, sawStopReason, "delivered")
				case unbound:
					s.reportAnthropicShortTurnUnbind(ctx, c, resp, account, model,
						proseRunes, usage.OutputTokens, sawStopReason, "delivered")
				}
			} else if anthropicTurnProvesUpstreamHealthy(sawStopReason, proseRunes, usage.OutputTokens, sawToolUseBlock, thinkingRunes) {
				s.clearAnthropicShortTurnStreak(ctx)
			}
			return
		}
		s.reportStreamIncompleteAfterCommit(ctx, c, resp, account, model, reason)
	}

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				// 上游把流关了却一个语义事件都没送到：此刻 prelude 还没写出客户端，
				// failover 窗口完好，必须先切号。一旦 flushPendingPrelude() 把
				// message_start 写出去，200 就钉死在客户端上，只能报 empty/malformed。
				// 客户端已经走了就别切了：留给下面的 flush 去发现断开并保住已观测的 usage。
				if !sawTerminalEvent && !streamCommitted && !clientDisconnected &&
					c.Request.Context().Err() == nil {
					pendingPreludeLines = pendingPreludeLines[:0]
					reason := "missing terminal event"
					// 持流期间上游中途死掉：提交窗口仍然完好，所以照样切号，但别把它
					// 说成空流——正文其实已经吐了一部分，只是还没写给客户端。
					if holdbackActive && !holdback.firstCommitPointAt.IsZero() {
						reason = fmt.Sprintf("stream ended mid-content during holdback (prose_runes=%d output_tokens=%d)",
							holdback.proseRunes, holdback.outputTokens)
					}
					return nil, s.newAnthropicEmptyStreamFailoverError(c, resp, account, reason)
				}
				flushPendingPrelude()
				if !clientDisconnected {
					// 兜底补刷，确保最后一个未以空行结尾的事件也能及时送达客户端。
					flusher.Flush()
				}
				if !sawTerminalEvent {
					if clientDisconnected && streamInterval > 0 {
						lastRead := time.Unix(0, atomic.LoadInt64(&lastReadAt))
						if time.Since(lastRead) >= streamInterval {
							return &streamingResult{usage: usage, firstTokenMs: firstTokenMs, clientDisconnect: true}, fmt.Errorf("stream usage incomplete after timeout")
						}
					}
					if !clientDisconnected && c.Request.Context().Err() == nil {
						s.reportStreamTruncatedAfterCommit(ctx, c, resp, account, model, "upstream closed stream without terminal event")
					}
					return &streamingResult{usage: usage, firstTokenMs: firstTokenMs, clientDisconnect: clientDisconnected}, fmt.Errorf("stream usage incomplete: missing terminal event")
				}
				reportIfTerminalButIncomplete()
				return &streamingResult{usage: usage, firstTokenMs: firstTokenMs, clientDisconnect: clientDisconnected}, nil
			}
			if ev.err != nil {
				// 上游还没吐出任何一行就读失败：这是明确的死上游，且 prelude 为空，
				// 切号最安全。已经收到 message_start 的中途断流不在此列——那时故障方
				// 可能是客户端，仍走下面的 flush 以便通过写失败发现客户端已断开并保住计量。
				// 另排除三类换号无用的情形：客户端取消/超时与账号无关，行超长是本地
				// MaxLineSize 配置问题。
				if !sawTerminalEvent && !streamCommitted && !clientDisconnected &&
					(len(pendingPreludeLines) == 0 || (holdbackActive && !holdback.firstCommitPointAt.IsZero())) &&
					c.Request.Context().Err() == nil &&
					!errors.Is(ev.err, context.Canceled) &&
					!errors.Is(ev.err, context.DeadlineExceeded) &&
					!errors.Is(ev.err, bufio.ErrTooLong) {
					readErrReason := fmt.Sprintf("stream read error before any data: %v", ev.err)
					if holdbackActive && !holdback.firstCommitPointAt.IsZero() {
						readErrReason = fmt.Sprintf("stream read error during holdback (prose_runes=%d output_tokens=%d): %v",
							holdback.proseRunes, holdback.outputTokens, ev.err)
					}
					return nil, s.newAnthropicEmptyStreamFailoverError(c, resp, account, readErrReason)
				}
				flushPendingPrelude()
				if sawTerminalEvent {
					reportIfTerminalButIncomplete()
					return &streamingResult{usage: usage, firstTokenMs: firstTokenMs, clientDisconnect: clientDisconnected}, nil
				}
				if clientDisconnected {
					return &streamingResult{usage: usage, firstTokenMs: firstTokenMs, clientDisconnect: true}, fmt.Errorf("stream usage incomplete after disconnect: %w", ev.err)
				}
				if errors.Is(ev.err, context.Canceled) || errors.Is(ev.err, context.DeadlineExceeded) {
					return &streamingResult{usage: usage, firstTokenMs: firstTokenMs, clientDisconnect: true}, fmt.Errorf("stream usage incomplete: %w", ev.err)
				}
				if errors.Is(ev.err, bufio.ErrTooLong) {
					logger.LegacyPrintf("service.gateway", "[Anthropic passthrough] SSE line too long: account=%d max_size=%d error=%v", account.ID, maxLineSize, ev.err)
					return &streamingResult{usage: usage, firstTokenMs: firstTokenMs}, ev.err
				}
				s.reportStreamTruncatedAfterCommit(ctx, c, resp, account, model, "stream read error after commit: "+sanitizeStreamError(ev.err))
				return &streamingResult{usage: usage, firstTokenMs: firstTokenMs}, fmt.Errorf("stream read error: %w", ev.err)
			}

			line := ev.line
			trimmedLine := strings.TrimSpace(line)
			if !streamCommitted {
				pendingPreludeLines = append(pendingPreludeLines, line)
				commitPrelude := false
				// holdEligible 排除终止帧与错误帧：message_stop / error / [DONE] 到了就说明
				// 没什么可等的了，继续攥着只是白拖延迟；错误帧更不能压。
				holdEligible := false
				var holdFrame gjson.Result
				if data, ok := extractAnthropicSSEDataLine(line); ok {
					data = strings.TrimSpace(data)
					if data != "" && isAnthropicSafetyRefusalResponse(http.StatusForbidden, []byte(data)) {
						pendingPreludeLines = pendingPreludeLines[:0]
						return nil, s.newAnthropicSafetyFailoverError(c, resp, account, []byte(data))
					}
					commitPrelude = data != "" && anthropicSSEPayloadCommitsResponse([]byte(data))
					if holdbackActive && data != "" && data != "[DONE]" && json.Valid([]byte(data)) {
						holdFrame = gjson.Parse(data)
						switch strings.TrimSpace(holdFrame.Get("type").String()) {
						case "message_stop", "error":
						default:
							holdEligible = true
						}
					}
				} else if strings.HasPrefix(strings.ToLower(trimmedLine), "event:") {
					commitPrelude = anthropicStreamEventIsTerminal(strings.TrimSpace(strings.TrimPrefix(trimmedLine, "event:")), "")
				}
				if line == "" && !commitPrelude {
					for _, pendingLine := range pendingPreludeLines {
						pendingLine = strings.TrimSpace(pendingLine)
						if strings.HasPrefix(strings.ToLower(pendingLine), "event:") &&
							strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(pendingLine, "event:")), "error") {
							commitPrelude = true
							break
						}
					}
				}
				// 零暴露持流：每一帧都要过判定，不能只看会提交的帧——stop_reason 落在
				// message_delta 上，而它在 anthropicSSEPayloadCommitsResponse 里返回 false，
				// 只在提交帧上判就永远拿不到判据。
				if holdEligible {
					now := time.Now()
					holdback.observe(holdFrame, commitPrelude, now)
					armHoldbackTimer()
					switch anthropicHoldbackVerdict(
						holdback.releaseDeadlineElapsed(now, holdbackWindow, holdbackMaxHold, holdbackDeadAir),
						holdback.deadAirElapsed(now, holdbackDeadAir),
						holdback.stopReason, holdback.proseRunes, holdback.outputTokens,
						holdback.sawToolUseBlock, holdbackDiscardsUsed, blockOrderDiscardsUsed, holdback.thinkingRunes,
						holdback.blockOrder.violation(),
					) {
					case anthropicHoldbackKeep:
						commitPrelude = false
					case anthropicHoldbackRelease:
						commitPrelude = true
					case anthropicHoldbackDiscard:
						pendingPreludeLines = pendingPreludeLines[:0]
						// 块序违规单独一条出口：判据是确定性的，所以解绑不走短回合的连击阈值，
						// 归因也不能记成短回合/空回合——那两档的根因是「没把话说完」，这一档是
						// 「块拼错了序，正文可能整段是伪造的」，混记就看不出某个上游在犯哪种错。
						if holdback.blockOrder.violation() && blockOrderDiscardsUsed < anthropicBlockOrderDiscardBudget {
							s.unbindStickySessionNow(ctx, account, model, "content block order violation")
							s.reportAnthropicBlockOrderViolation(ctx, c, resp, account, model,
								holdback.proseRunes, holdback.outputTokens,
								holdback.stopReason, "discarded")
							return nil, s.newAnthropicBlockOrderFailoverError(
								c, resp, account, model,
								holdback.proseRunes, holdback.outputTokens, holdback.stopReason)
						}
						// 丢弃只治这一发。粘性绑定还指着这个坏号，而重试那一发不会改写它
						// （非破坏性绑定：已有绑定指向别的账号时不覆盖），所以下一发客户端
						// 请求照旧从这个号起手，每发白烧一次上游调用。这里主动解绑，阈值 1
						// 下这一次 note 就直接删绑定，重试那一发随即绑到新号上。
						// 代价只是丢一次 prompt cache —— 见 noteAnthropicShortTurnStreak。
						// 解绑成功还要补一次冷却：坏号若是本组 priority 最小的那个，
						// 光删绑定下一发照旧选回它。见 reportAnthropicShortTurnUnbind。
						unbound := s.noteAnthropicShortTurnStreak(ctx, account, model,
							holdback.proseRunes, holdback.outputTokens)
						switch {
						case anthropicTurnIsEmptyAnswer(holdback.stopReason, holdback.proseRunes,
							holdback.outputTokens, holdback.sawToolUseBlock):
							s.reportAnthropicEmptyAnswerTurn(ctx, c, resp, account, model,
								holdback.outputTokens, holdback.stopReason, "discarded")
						case unbound:
							s.reportAnthropicShortTurnUnbind(ctx, c, resp, account, model,
								holdback.proseRunes, holdback.outputTokens,
								holdback.stopReason, "discarded")
						}
						return nil, s.newAnthropicShortTurnFailoverError(
							c, resp, account, model,
							holdback.proseRunes, holdback.outputTokens, holdback.stopReason)
					}
				}
				if commitPrelude {
					flushPendingPrelude()
				} else {
					inPartialEvent = len(pendingPreludeLines) > 0
				}
				continue
			}
			processLine(line)

		case <-holdbackCh:
			// 定时器只是唤醒器，判据在 holdback.holdbackReleaseDeadline 手里：静默窗口量的
			// 是**静默**时长、会随新帧续期，而定时器 arm 之后按固定时长走，帧还在源源到达时
			// 它照样开火。所以开火先复核，三条截止线都没到就按剩余时间续期、继续持流；只有
			// 静默真的满了一个窗口（判定要的 stop_reason 始终没来）、持流总时长撞到上限
			// （上游一直在吐但客户端一个字节都还没拿到）、或者客户端总死气吃满预算（跨
			// failover 累加，见 deadAirElapsed），才认定等不起，原样放行。
			holdbackCh = nil
			now := time.Now()
			deadline := holdback.holdbackReleaseDeadline(holdbackWindow, holdbackMaxHold, holdbackDeadAir)
			switch {
			case streamCommitted:
				// 已经提交过，窗口没有可做的事。
			case !deadline.IsZero() && now.Before(deadline):
				if holdbackTimer != nil {
					// 定时器已经开火、通道已被取空，Reset 前不需要再 Stop+drain。
					holdbackTimer.Reset(deadline.Sub(now))
					holdbackCh = holdbackTimer.C
				}
			default:
				flushPendingPrelude()
				if !clientDisconnected {
					flusher.Flush()
				}
			}

		case <-intervalCh:
			lastRead := time.Unix(0, atomic.LoadInt64(&lastReadAt))
			if time.Since(lastRead) < streamInterval {
				continue
			}
			flushPendingPrelude()
			if clientDisconnected {
				return &streamingResult{usage: usage, firstTokenMs: firstTokenMs, clientDisconnect: true}, fmt.Errorf("stream usage incomplete after timeout")
			}
			logger.LegacyPrintf("service.gateway", "[Anthropic passthrough] Stream data interval timeout: account=%d model=%s interval=%s", account.ID, model, streamInterval)
			if s.rateLimitService != nil {
				s.rateLimitService.HandleStreamTimeout(ctx, account, model)
			}
			return &streamingResult{usage: usage, firstTokenMs: firstTokenMs}, fmt.Errorf("stream data interval timeout")

		case <-keepaliveCh:
			if !streamCommitted {
				resetKeepaliveTimer()
				continue
			}
			if clientDisconnected {
				continue
			}
			if inPartialEvent {
				resetKeepaliveTimer()
				continue
			}
			if time.Since(lastDataAt) < keepaliveInterval {
				resetKeepaliveTimer()
				continue
			}
			if _, err := fmt.Fprint(w, "event: ping\ndata: {\"type\": \"ping\"}\n\n"); err != nil {
				clientDisconnected = true
				logger.LegacyPrintf("service.gateway", "[Anthropic passthrough] Client disconnected during keepalive ping, continue draining upstream for usage: account=%d", account.ID)
				continue
			}
			flusher.Flush()
			lastDataAt = time.Now()
			resetKeepaliveTimer()
		}
	}
}

func extractAnthropicSSEDataLine(line string) (string, bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	start := len("data:")
	for start < len(line) {
		if line[start] != ' ' && line[start] != '\t' {
			break
		}
		start++
	}
	return line[start:], true
}

// parseSSEUsagePassthrough 从 Anthropic SSE data 行提取 usage（包级函数：
// Anthropic 平台 passthrough 与国产供应商原生 Anthropic 直通共用）。
func parseSSEUsagePassthrough(data string, usage *ClaudeUsage) {
	if usage == nil || data == "" || data == "[DONE]" {
		return
	}

	parsed := gjson.Parse(data)
	switch parsed.Get("type").String() {
	case "message_start":
		msgUsage := parsed.Get("message.usage")
		if msgUsage.Exists() {
			usage.InputTokens = int(msgUsage.Get("input_tokens").Int())
			usage.CacheCreationInputTokens = int(msgUsage.Get("cache_creation_input_tokens").Int())
			usage.CacheReadInputTokens = int(msgUsage.Get("cache_read_input_tokens").Int())

			// 保持与通用解析一致：message_start 允许覆盖 5m/1h 明细（包括 0）。
			cc5m := msgUsage.Get("cache_creation.ephemeral_5m_input_tokens")
			cc1h := msgUsage.Get("cache_creation.ephemeral_1h_input_tokens")
			if cc5m.Exists() || cc1h.Exists() {
				usage.CacheCreation5mTokens = int(cc5m.Int())
				usage.CacheCreation1hTokens = int(cc1h.Int())
			}
		}
	case "message_delta":
		deltaUsage := parsed.Get("usage")
		if deltaUsage.Exists() {
			if v := deltaUsage.Get("input_tokens").Int(); v > 0 {
				usage.InputTokens = int(v)
			}
			if v := deltaUsage.Get("output_tokens").Int(); v > 0 {
				usage.OutputTokens = int(v)
			}
			if v := deltaUsage.Get("cache_creation_input_tokens").Int(); v > 0 {
				usage.CacheCreationInputTokens = int(v)
			}
			if v := deltaUsage.Get("cache_read_input_tokens").Int(); v > 0 {
				usage.CacheReadInputTokens = int(v)
			}

			cc5m := deltaUsage.Get("cache_creation.ephemeral_5m_input_tokens")
			cc1h := deltaUsage.Get("cache_creation.ephemeral_1h_input_tokens")
			if cc5m.Exists() {
				usage.CacheCreation5mTokens = int(cc5m.Int())
			}
			if cc1h.Exists() {
				usage.CacheCreation1hTokens = int(cc1h.Int())
			}
		}
	}

	if usage.CacheReadInputTokens == 0 {
		if cached := parsed.Get("message.usage.cached_tokens").Int(); cached > 0 {
			usage.CacheReadInputTokens = int(cached)
		}
		if cached := parsed.Get("usage.cached_tokens").Int(); usage.CacheReadInputTokens == 0 && cached > 0 {
			usage.CacheReadInputTokens = int(cached)
		}
	}
	if usage.CacheCreationInputTokens == 0 {
		cc5m := parsed.Get("message.usage.cache_creation.ephemeral_5m_input_tokens").Int()
		cc1h := parsed.Get("message.usage.cache_creation.ephemeral_1h_input_tokens").Int()
		if cc5m == 0 && cc1h == 0 {
			cc5m = parsed.Get("usage.cache_creation.ephemeral_5m_input_tokens").Int()
			cc1h = parsed.Get("usage.cache_creation.ephemeral_1h_input_tokens").Int()
		}
		total := cc5m + cc1h
		if total > 0 {
			usage.CacheCreationInputTokens = int(total)
		}
	}

	// Kimi's Anthropic-compatible stream uses input_tokens with two meanings:
	// message_start reports total prompt input, while message_delta reports only
	// uncached input. prompt_tokens remains the total in both events. Normalize
	// to ClaudeUsage's mutually-exclusive buckets so downstream billing does not
	// subtract cache tokens from an already-uncached value.
	usageNode := parsed.Get("usage")
	if parsed.Get("type").String() == "message_start" {
		usageNode = parsed.Get("message.usage")
	}
	normalizeAnthropicCompatiblePromptUsage(usageNode, usage)
}

// normalizeAnthropicCompatiblePromptUsage converts provider-native OpenAI-style
// prompt/cache fields into Claude's mutually-exclusive usage buckets. Native
// Anthropic responses do not expose these aliases and are left alone.
func normalizeAnthropicCompatiblePromptUsage(usageNode gjson.Result, usage *ClaudeUsage) bool {
	if usage == nil || !usageNode.Exists() {
		return false
	}
	promptTokens := usageNode.Get("prompt_tokens")
	promptCacheHitTokens := usageNode.Get("prompt_cache_hit_tokens")
	promptCacheMissTokens := usageNode.Get("prompt_cache_miss_tokens")
	if (!promptTokens.Exists() || promptTokens.Int() <= 0) &&
		!promptCacheHitTokens.Exists() && !promptCacheMissTokens.Exists() {
		return false
	}

	cacheReadTokens := usage.CacheReadInputTokens
	if v := usageNode.Get("cache_read_input_tokens"); v.Exists() {
		cacheReadTokens = int(v.Int())
	}
	if cacheReadTokens == 0 {
		if v := usageNode.Get("cached_tokens"); v.Exists() {
			cacheReadTokens = int(v.Int())
		}
	}
	if cacheReadTokens == 0 {
		if v := usageNode.Get("prompt_tokens_details.cached_tokens"); v.Exists() {
			cacheReadTokens = int(v.Int())
		}
	}
	if cacheReadTokens == 0 && promptCacheHitTokens.Exists() {
		cacheReadTokens = max(int(promptCacheHitTokens.Int()), 0)
	}

	cacheCreationTokens := usage.CacheCreationInputTokens
	if v := usageNode.Get("cache_creation_input_tokens"); v.Exists() {
		cacheCreationTokens = int(v.Int())
	}
	if cacheCreationTokens == 0 {
		cc5m := usageNode.Get("cache_creation.ephemeral_5m_input_tokens").Int()
		cc1h := usageNode.Get("cache_creation.ephemeral_1h_input_tokens").Int()
		if cc5m > 0 || cc1h > 0 {
			cacheCreationTokens = int(cc5m + cc1h)
		}
	}

	if promptCacheMissTokens.Exists() {
		usage.InputTokens = max(int(promptCacheMissTokens.Int()), 0)
	} else {
		usage.InputTokens = max(int(promptTokens.Int())-cacheReadTokens-cacheCreationTokens, 0)
	}
	usage.CacheReadInputTokens = cacheReadTokens
	usage.CacheCreationInputTokens = cacheCreationTokens
	return true
}

func parseClaudeUsageFromResponseBody(body []byte) *ClaudeUsage {
	usage := &ClaudeUsage{}
	if len(body) == 0 {
		return usage
	}

	parsed := gjson.ParseBytes(body)
	usageNode := parsed.Get("usage")
	if !usageNode.Exists() {
		return usage
	}

	usage.InputTokens = int(usageNode.Get("input_tokens").Int())
	usage.OutputTokens = int(usageNode.Get("output_tokens").Int())
	usage.CacheCreationInputTokens = int(usageNode.Get("cache_creation_input_tokens").Int())
	usage.CacheReadInputTokens = int(usageNode.Get("cache_read_input_tokens").Int())

	cc5m := usageNode.Get("cache_creation.ephemeral_5m_input_tokens").Int()
	cc1h := usageNode.Get("cache_creation.ephemeral_1h_input_tokens").Int()
	if cc5m > 0 || cc1h > 0 {
		usage.CacheCreation5mTokens = int(cc5m)
		usage.CacheCreation1hTokens = int(cc1h)
	}
	if usage.CacheCreationInputTokens == 0 && (cc5m > 0 || cc1h > 0) {
		usage.CacheCreationInputTokens = int(cc5m + cc1h)
	}
	if usage.CacheReadInputTokens == 0 {
		if cached := usageNode.Get("cached_tokens").Int(); cached > 0 {
			usage.CacheReadInputTokens = int(cached)
		}
	}
	normalizeAnthropicCompatiblePromptUsage(usageNode, usage)
	return usage
}

// invalidNonStreamingJSONFailoverError 把"上游 2xx 返回非 JSON body"归一为
// failover 错误（包级函数：Anthropic 平台 passthrough 与国产供应商原生
// Anthropic 直通共用）。
func invalidNonStreamingJSONFailoverError(
	ctx context.Context,
	rateLimitService *RateLimitService,
	resp *http.Response,
	account *Account,
	body []byte,
	parseErr error,
	requestedModel ...string,
) error {
	const statusCode = http.StatusBadGateway

	accountID := int64(0)
	accountName := ""
	retryableOnSameAccount := false
	if account != nil {
		accountID = account.ID
		accountName = account.Name
		retryableOnSameAccount = account.IsPoolMode() && account.IsPoolModeRetryableStatus(statusCode)
	}

	logger.LegacyPrintf(
		"service.gateway",
		"Account %d(%s): upstream returned non-JSON 2xx response, attempting failover: status=%d request_id=%s error=%v",
		accountID,
		accountName,
		resp.StatusCode,
		resp.Header.Get("x-request-id"),
		parseErr,
	)

	if rateLimitService != nil && account != nil {
		if len(requestedModel) > 0 {
			rateLimitService.HandleUpstreamError(ctx, account, statusCode, resp.Header, body, requestedModel[0])
		} else {
			rateLimitService.HandleUpstreamError(ctx, account, statusCode, resp.Header, body)
		}
	}

	return &UpstreamFailoverError{
		StatusCode:             statusCode,
		ResponseBody:           body,
		ResponseHeaders:        resp.Header,
		RetryableOnSameAccount: retryableOnSameAccount,
	}
}

func (s *GatewayService) handleNonStreamingResponseAnthropicAPIKeyPassthrough(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
) (*ClaudeUsage, error) {
	if s.rateLimitService != nil {
		s.rateLimitService.UpdateSessionWindow(ctx, account, resp.Header)
	}

	body, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, anthropicTooLargeError)
	if err != nil {
		return nil, err
	}
	observer := upstreamResponseModelObserverFromContext(c)
	if observer == nil {
		observer = beginUpstreamResponseModelObservation(c)
	}
	observer.ObserveAnthropic(body)

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		var raw json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, invalidNonStreamingJSONFailoverError(ctx, s.rateLimitService, resp, account, body, err)
		}
		if isAnthropicSafetyRefusalResponse(resp.StatusCode, body) {
			return nil, s.newAnthropicSafetyFailoverError(c, resp, account, body)
		}
	}

	usage := parseClaudeUsageFromResponseBody(body)
	if IsForceCacheBilling(ctx) && usage.InputTokens > 0 {
		body, err = classifyAnthropicResponseInputAsCacheRead(body, usage)
		if err != nil {
			return nil, err
		}
	}

	writeAnthropicPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	contentType := normalizeJSONBodyContentType(c.Writer.Header(), resp.Header.Get("Content-Type"), body)
	body = reverseToolNamesIfPresent(c, body)
	c.Data(resp.StatusCode, contentType, body)
	return usage, nil
}

// normalizeJSONBodyContentType 修正上游对 JSON 响应体的错误 Content-Type 声明。
//
// 透传分支原样复制上游 Content-Type，但中转类上游（oneapi/自建 relay）常把
// /v1/messages 的 JSON 响应标成 text/plain，严格客户端会直接判定
// "API returned an empty or malformed response (HTTP 200)" 并丢弃整个响应——
// 哪怕响应体本身是完好的 Anthropic message。非透传分支早就默认 application/json
// （见 handleNonStreamingResponse），这里补齐同样的保证。
//
// 只在响应体确实是 JSON 且上游声明不是 JSON 媒体类型时改写：非 JSON 体（如上游
// 直接吐纯文本错误）保持原声明，不掩盖上游真实行为。dst 非空时必须一并覆盖，
// 因为 WriteFilteredHeaders 已把上游值写进 header map，而 gin 的 writeContentType
// 只在 header 缺失时才会写入。
func normalizeJSONBodyContentType(dst http.Header, upstreamContentType string, body []byte) string {
	contentType := strings.TrimSpace(upstreamContentType)
	// 上游已声明 JSON：原样保留（含 charset 等参数）。
	if isJSONMediaType(contentType) {
		return contentType
	}
	// 响应体不是 JSON：不掩盖上游的真实声明，仅在其缺失时兜底。
	if !bodyLooksLikeJSON(body) {
		if contentType == "" {
			return "application/json"
		}
		return contentType
	}
	if dst != nil {
		dst.Set("Content-Type", "application/json")
	}
	return "application/json"
}

// isJSONMediaType 判断媒体类型是否为 JSON 家族（含 application/*+json 与 text/json）。
func isJSONMediaType(contentType string) bool {
	mediaType := strings.ToLower(strings.TrimSpace(contentType))
	if idx := strings.IndexByte(mediaType, ';'); idx >= 0 {
		mediaType = strings.TrimSpace(mediaType[:idx])
	}
	return mediaType == "application/json" ||
		mediaType == "text/json" ||
		strings.HasSuffix(mediaType, "+json")
}

// bodyLooksLikeJSON 只做首字符判定 + json.Valid 校验，避免对大响应体反复反序列化。
func bodyLooksLikeJSON(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return false
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return false
	}
	return json.Valid(trimmed)
}

func classifyAnthropicResponseInputAsCacheRead(body []byte, usage *ClaudeUsage) ([]byte, error) {
	classified, err := sjson.SetBytes(body, "usage.input_tokens", 0)
	if err != nil {
		return nil, fmt.Errorf("classify forced cache billing input tokens: %w", err)
	}
	classified, err = sjson.SetBytes(classified, "usage.cache_read_input_tokens", usage.CacheReadInputTokens+usage.InputTokens)
	if err != nil {
		return nil, fmt.Errorf("classify forced cache billing cache read tokens: %w", err)
	}
	return classified, nil
}

func writeAnthropicPassthroughResponseHeaders(dst http.Header, src http.Header, filter *responseheaders.CompiledHeaderFilter) {
	if dst == nil || src == nil {
		return
	}
	if filter != nil {
		responseheaders.WriteFilteredHeaders(dst, src, filter)
		return
	}
	if v := strings.TrimSpace(src.Get("Content-Type")); v != "" {
		dst.Set("Content-Type", v)
	}
	if v := strings.TrimSpace(src.Get("x-request-id")); v != "" {
		dst.Set("x-request-id", v)
	}
}
