package service

import (
	"context"
	"time"
)

// TempUnschedState 临时不可调度状态
type TempUnschedState struct {
	UntilUnix            int64  `json:"until_unix"`                       // 解除时间（Unix 时间戳）
	TriggeredAtUnix      int64  `json:"triggered_at_unix"`                // 触发时间（Unix 时间戳）
	StatusCode           int    `json:"status_code"`                      // 触发的错误码
	MatchedKeyword       string `json:"matched_keyword"`                  // 匹配的关键词
	RuleIndex            int    `json:"rule_index"`                       // 触发的规则索引
	ErrorMessage         string `json:"error_message"`                    // 错误消息
	TriggerCount         int64  `json:"trigger_count,omitempty"`          // 本次触发累计命中次数
	TriggerThreshold     int    `json:"trigger_threshold,omitempty"`      // 触发阈值
	TriggerWindowMinutes int    `json:"trigger_window_minutes,omitempty"` // 计数窗口（分钟）
	// ProbeFailures 后台探针连续探测失败的次数，由 AccountStreamProbeService 写入。
	// 只有「流交付失败」家族的冷却会带这个字段，用来做冷却退避；其它触发源不写。
	ProbeFailures int `json:"probe_failures,omitempty"`
	// Model 记录触发这次停调时客户端请求的模型，供后台探针复现同一条链路。
	//
	// 为什么非记不可：探针原先固定用 claude.DefaultTestModel 去探，而中转类账号的
	// 上游只供应自己那一份模型清单，探测请求会直接吃 404 model_not_found。404 归入
	// inconclusive，于是每一轮都「探了但下不了结论」，探针对这类账号等于完全不工作。
	// 用当初把账号罚下线的那个模型去探，既能保证上游一定认（刚刚才在服务它），
	// 又让探测结论和真实流量说的是同一件事。
	Model string `json:"model,omitempty"`
}

// TempUnschedCache 临时不可调度缓存接口
type TempUnschedCache interface {
	SetTempUnsched(ctx context.Context, accountID int64, state *TempUnschedState) error
	GetTempUnsched(ctx context.Context, accountID int64) (*TempUnschedState, error)
	DeleteTempUnsched(ctx context.Context, accountID int64) error
}

// OpenAIAPIKeyHealthCache is an optional TempUnschedCache extension used to
// aggregate pool API-key failures across gateway instances.
type OpenAIAPIKeyHealthCache interface {
	RecordOpenAIAPIKeyHealthFailure(ctx context.Context, accountID int64, windowMinutes, threshold int) (count int64, tripped bool, err error)
}

// TimeoutCounterCache 超时计数器缓存接口
type TimeoutCounterCache interface {
	// IncrementTimeoutCount 增加账户的超时计数，返回当前计数值
	// windowMinutes 是计数窗口时间（分钟），超过此时间计数器会自动重置
	IncrementTimeoutCount(ctx context.Context, accountID int64, windowMinutes int) (int64, error)
	// GetTimeoutCount 获取账户当前的超时计数
	GetTimeoutCount(ctx context.Context, accountID int64) (int64, error)
	// ResetTimeoutCount 重置账户的超时计数
	ResetTimeoutCount(ctx context.Context, accountID int64) error
	// GetTimeoutCountTTL 获取计数器剩余过期时间
	GetTimeoutCountTTL(ctx context.Context, accountID int64) (time.Duration, error)
}
