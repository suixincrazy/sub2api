package service

import (
	"context"
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// gatewayTransportFailoverBody 是传输层失败（代理/DNS/TCP/TLS，没拿到任何 HTTP
// 状态码）时挂在 UpstreamFailoverError 上的 Anthropic 格式错误体。内容与历史上
// 内联写出的 502 完全一致，因此 failover 真正耗尽时客户端看到的载荷不变。
var gatewayTransportFailoverBody = []byte(`{"type":"error","error":{"type":"upstream_error","message":"Upstream request failed"}}`)

// transportFailoverSpec 是每条转发链路必须自己决定、公共处理函数无法代为决定的两项。
//
// 之所以做成必填参数而不是在公共函数里写死：这两项都随调用点变化，写死一次就会在
// 新增链路时静默取到错误的值。做成参数后，新增调用点不填就编译不过，逼着显式决策。
type transportFailoverSpec struct {
	// Reason 是本条链路稳定可辨的失败归因。粘性解绑与账号惩罚的归因测试按它断言，
	// 运维侧也靠它区分究竟是哪条链路的代理挂了。留空会让这些区分全部塌成一个值。
	Reason GatewayFailureReason

	// ResponseBody 是 failover 耗尽后客户端看到的错误体，必须与本链路对客户端输出的
	// 协议形状一致：ForwardAsChatCompletions / ForwardAsResponses 输出 OpenAI 形状，
	// 且错误透传规则可能把 ResponseBody 原样吐给客户端，挂 Anthropic 形状会让严格
	// 客户端反序列化失败。留空时回落到 Anthropic 形状（多数链路的正确值）。
	ResponseBody []byte
}

// handleUpstreamTransportError handles a transport-level upstream failure on
// the Anthropic/Bedrock forward paths (Do/DoWithTLS returned a non-HTTP error:
// proxy / DNS / TCP / TLS). It:
//  1. records the failure in Ops error logs (status 0, kind=request_error) —
//     the caller passes path-specific fields (UpstreamURL, Passthrough) via
//     event; identity, proxy attribution and classification fields are
//     filled here from the same account snapshot that built the transport;
//  2. for durable faults (expired/rejected proxy creds, dead proxy,
//     DNS/routing) emits a stable warn event that alert rules can key on —
//     see logUpstreamTransportPersistentFault for why this does not touch
//     account scheduling state;
//  3. returns an error that is *UpstreamFailoverError (so the handler fails
//     over to a healthy account) for all non-canceled errors, or the original
//     error for context.Canceled (client gone — no failover).
//
// It deliberately does NOT write to the response: the handler owns the
// response (failover, or a protocol-correct error once failover is exhausted).
func (s *GatewayService) handleUpstreamTransportError(ctx context.Context, c *gin.Context, account *Account, err error, event OpsUpstreamErrorEvent, spec transportFailoverSpec) error {
	safeErr := sanitizeUpstreamErrorMessage(err.Error())
	setOpsUpstreamError(c, 0, safeErr, "")
	event.ProxyID, event.ProxyName = opsUpstreamProxyAttribution(account)
	event.Platform = account.Platform
	event.AccountID = account.ID
	event.AccountName = account.Name
	event.UpstreamStatusCode = 0
	event.Kind = "request_error"
	event.Message = safeErr
	appendOpsUpstreamError(c, event)

	// Client disconnected: do NOT fail over to another account — the upstream
	// never had a chance to exhibit a fault.
	if errors.Is(err, context.Canceled) || (errors.Is(err, context.DeadlineExceeded) && errors.Is(ctx.Err(), context.DeadlineExceeded)) {
		return err
	}

	// Transport attempt left local validation; count Ollama Cloud activity.
	scheduleOllamaCloudUsageActivity(s.deferredService, account)

	if classifyUpstreamTransportError(err).Persistent {
		logUpstreamTransportPersistentFault("service.gateway", "gateway.upstream_transport_error_persistent", account, safeErr)
	}

	body := spec.ResponseBody
	if len(body) == 0 {
		body = gatewayTransportFailoverBody
	}
	return &UpstreamFailoverError{
		StatusCode:   http.StatusBadGateway,
		ResponseBody: body,
		Reason:       spec.Reason,
	}
}

// logUpstreamTransportPersistentFault 记录一次被判定为持久性的传输层故障。
//
// 只记录，不改账号状态。传输层故障（代理挂了、DNS 不通、TCP 被拒）说明的是**这条
// 出网路径**坏了，不是账号本身没额度：把账号摘出调度池 10 分钟等于让一次代理抖动
// 连带废掉一个健康账号，而 failover 本身已经把这一发救回来了。摘除只留给额度耗尽
// 这类确实指向账号自身的信号。
//
// 分类结果仍然要报出来：代理凭证过期这种故障会持续复现，运维需要一个可挂告警规则的
// 稳定事件名，否则只能从一堆 request_error 里翻。
func logUpstreamTransportPersistentFault(component, event string, account *Account, safeErr string) {
	if account == nil {
		return
	}
	logger.L().With(zap.String("component", component)).Warn(
		event,
		zap.Int64("account_id", account.ID),
		zap.String("account_name", account.Name),
		zap.String("platform", account.Platform),
		zap.String("reason", "upstream transport error (proxy/network): "+safeErr),
	)
}
