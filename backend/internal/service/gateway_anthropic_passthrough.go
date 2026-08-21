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
	"net/http"
	"strings"
	"sync/atomic"
	"time"

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

const anthropicEmptyStreamUpstreamMessage = "Anthropic upstream returned an empty SSE stream with no terminal event"

const anthropicTruncatedStreamUpstreamMessage = "Anthropic upstream truncated the SSE stream after it was committed to the client"

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
					return nil, s.newAnthropicEmptyStreamFailoverError(c, resp, account, "missing terminal event")
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
				return &streamingResult{usage: usage, firstTokenMs: firstTokenMs, clientDisconnect: clientDisconnected}, nil
			}
			if ev.err != nil {
				// 上游还没吐出任何一行就读失败：这是明确的死上游，且 prelude 为空，
				// 切号最安全。已经收到 message_start 的中途断流不在此列——那时故障方
				// 可能是客户端，仍走下面的 flush 以便通过写失败发现客户端已断开并保住计量。
				// 另排除三类换号无用的情形：客户端取消/超时与账号无关，行超长是本地
				// MaxLineSize 配置问题。
				if !sawTerminalEvent && !streamCommitted && !clientDisconnected &&
					len(pendingPreludeLines) == 0 &&
					c.Request.Context().Err() == nil &&
					!errors.Is(ev.err, context.Canceled) &&
					!errors.Is(ev.err, context.DeadlineExceeded) &&
					!errors.Is(ev.err, bufio.ErrTooLong) {
					return nil, s.newAnthropicEmptyStreamFailoverError(c, resp, account, fmt.Sprintf("stream read error before any data: %v", ev.err))
				}
				flushPendingPrelude()
				if sawTerminalEvent {
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
				if data, ok := extractAnthropicSSEDataLine(line); ok {
					data = strings.TrimSpace(data)
					if data != "" && isAnthropicSafetyRefusalResponse(http.StatusForbidden, []byte(data)) {
						pendingPreludeLines = pendingPreludeLines[:0]
						return nil, s.newAnthropicSafetyFailoverError(c, resp, account, []byte(data))
					}
					commitPrelude = data != "" && anthropicSSEPayloadCommitsResponse([]byte(data))
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
				if commitPrelude {
					flushPendingPrelude()
				} else {
					inPartialEvent = len(pendingPreludeLines) > 0
				}
				continue
			}
			processLine(line)

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
			if cc5m.Exists() && cc5m.Int() > 0 {
				usage.CacheCreation5mTokens = int(cc5m.Int())
			}
			if cc1h.Exists() && cc1h.Int() > 0 {
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
