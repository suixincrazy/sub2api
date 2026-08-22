package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errStreamProbeTestFailure 代表线上那种「上游把流关了却没送终止事件」的探测失败。
var errStreamProbeTestFailure = errors.New("upstream closed stream without terminal event")

// --- 测试替身 ---

type streamProbeSetCall struct {
	id     int64
	until  time.Time
	reason string
}

type fakeStreamProbeStore struct {
	mu       sync.Mutex
	accounts []Account
	setCalls []streamProbeSetCall
	cleared  []int64
}

func (f *fakeStreamProbeStore) ListByPlatform(context.Context, string) ([]Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Account(nil), f.accounts...), nil
}

func (f *fakeStreamProbeStore) SetTempUnschedulable(_ context.Context, id int64, until time.Time, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls = append(f.setCalls, streamProbeSetCall{id: id, until: until, reason: reason})
	return nil
}

func (f *fakeStreamProbeStore) ClearTempUnschedulable(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, id)
	return nil
}

func (f *fakeStreamProbeStore) snapshot() ([]streamProbeSetCall, []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]streamProbeSetCall(nil), f.setCalls...), append([]int64(nil), f.cleared...)
}

type fakeStreamProber struct {
	mu    sync.Mutex
	err   error
	calls int
	// gotModels 记录每次探测收到的模型，用来断言探针复现的是罚号时那条链路。
	gotModels []string
	// onProbe 在探测发生的瞬间回调，用来断言「探测开始前窗口已经被顶住」。
	onProbe func()
}

func (f *fakeStreamProber) ProbeClaudeStreamHealth(_ context.Context, _ *Account, model string) error {
	f.mu.Lock()
	f.calls++
	f.gotModels = append(f.gotModels, model)
	cb := f.onProbe
	err := f.err
	f.mu.Unlock()
	if cb != nil {
		cb()
	}
	return err
}

func (f *fakeStreamProber) models() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.gotModels...)
}

func (f *fakeStreamProber) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeStreamProbeSettings struct {
	settings *StreamTimeoutSettings
}

func (f *fakeStreamProbeSettings) GetStreamTimeoutSettings(context.Context) (*StreamTimeoutSettings, error) {
	return f.settings, nil
}

// --- 辅助构造 ---

func streamProbeReasonJSON(t *testing.T, keyword string, until time.Time, probeFailures int) string {
	t.Helper()
	raw, err := json.Marshal(&TempUnschedState{
		UntilUnix:      until.Unix(),
		MatchedKeyword: keyword,
		ErrorMessage:   "upstream closed stream without terminal event",
		ProbeFailures:  probeFailures,
	})
	require.NoError(t, err)
	return string(raw)
}

// streamProbeAccount 造一个「因流截断被临时停调」的 Anthropic 主号。
func streamProbeAccount(t *testing.T, until time.Time, keyword string, probeFailures int) *Account {
	t.Helper()
	u := until
	return &Account{
		ID:                      9,
		Platform:                PlatformAnthropic,
		Type:                    AccountTypeAPIKey,
		Status:                  StatusActive,
		Schedulable:             true,
		TempUnschedulableUntil:  &u,
		TempUnschedulableReason: streamProbeReasonJSON(t, keyword, until, probeFailures),
	}
}

func newTestStreamProbeService(store streamProbeAccountStore, prober streamHealthProber) *AccountStreamProbeService {
	return NewAccountStreamProbeService(store, prober, nil, nil, streamProbeScanInterval)
}

// --- streamProbeShouldProbe ---

func TestStreamProbeShouldProbe(t *testing.T) {
	now := time.Now()

	t.Run("流截断冷却即将到点时该探", func(t *testing.T) {
		account := streamProbeAccount(t, now.Add(15*time.Second), streamProbeKeywordTruncated, 0)
		state, ok := streamProbeShouldProbe(account, now)
		require.True(t, ok)
		require.NotNil(t, state)
		assert.Equal(t, streamProbeKeywordTruncated, state.MatchedKeyword)
	})

	t.Run("流超时与安全拒答同属本家族", func(t *testing.T) {
		for _, keyword := range []string{streamProbeKeywordTimeout, streamProbeKeywordRefusal} {
			account := streamProbeAccount(t, now.Add(10*time.Second), keyword, 0)
			_, ok := streamProbeShouldProbe(account, now)
			assert.True(t, ok, keyword)
		}
	})

	t.Run("冷却还早时留到下一轮", func(t *testing.T) {
		account := streamProbeAccount(t, now.Add(5*time.Minute), streamProbeKeywordTruncated, 0)
		_, ok := streamProbeShouldProbe(account, now)
		assert.False(t, ok)
	})

	t.Run("冷却已到点不再罚", func(t *testing.T) {
		// 此时账号已经回到调度池，再写停调等于把正在服务的账号无故踢下线。
		account := streamProbeAccount(t, now.Add(-time.Second), streamProbeKeywordTruncated, 0)
		_, ok := streamProbeShouldProbe(account, now)
		assert.False(t, ok)
	})

	t.Run("别的子系统写的停调不碰", func(t *testing.T) {
		account := streamProbeAccount(t, now.Add(10*time.Second), "rate_limit", 0)
		_, ok := streamProbeShouldProbe(account, now)
		assert.False(t, ok)
	})

	t.Run("reason 是纯文本时不碰", func(t *testing.T) {
		account := streamProbeAccount(t, now.Add(10*time.Second), streamProbeKeywordTruncated, 0)
		account.TempUnschedulableReason = "余额不足，已临时停调"
		_, ok := streamProbeShouldProbe(account, now)
		assert.False(t, ok)
	})

	t.Run("管理员手动停用的账号不恢复", func(t *testing.T) {
		account := streamProbeAccount(t, now.Add(10*time.Second), streamProbeKeywordTruncated, 0)
		account.Schedulable = false
		_, ok := streamProbeShouldProbe(account, now)
		assert.False(t, ok)
	})

	t.Run("限流窗口还在时不探", func(t *testing.T) {
		account := streamProbeAccount(t, now.Add(10*time.Second), streamProbeKeywordTruncated, 0)
		reset := now.Add(time.Hour)
		account.RateLimitResetAt = &reset
		_, ok := streamProbeShouldProbe(account, now)
		assert.False(t, ok)
	})

	t.Run("过载窗口还在时不探", func(t *testing.T) {
		account := streamProbeAccount(t, now.Add(10*time.Second), streamProbeKeywordTruncated, 0)
		overload := now.Add(time.Hour)
		account.OverloadUntil = &overload
		_, ok := streamProbeShouldProbe(account, now)
		assert.False(t, ok)
	})

	t.Run("没有停调窗口的账号不碰", func(t *testing.T) {
		account := streamProbeAccount(t, now.Add(10*time.Second), streamProbeKeywordTruncated, 0)
		account.TempUnschedulableUntil = nil
		_, ok := streamProbeShouldProbe(account, now)
		assert.False(t, ok)
	})
}

// --- probeOne：核心行为 ---

// 探测通过 → 主号回到调度池。
func TestProbeOneClearsCooldownWhenHealthy(t *testing.T) {
	until := time.Now().Add(15 * time.Second)
	account := streamProbeAccount(t, until, streamProbeKeywordTruncated, 2)
	state := parseStreamProbeState(account.TempUnschedulableReason)
	require.NotNil(t, state)

	store := &fakeStreamProbeStore{}
	prober := &fakeStreamProber{err: nil}
	svc := newTestStreamProbeService(store, prober)

	svc.probeOne(context.Background(), account, state)

	_, cleared := store.snapshot()
	assert.Equal(t, []int64{9}, cleared, "探测通过必须清除停调，主号回到调度池")
	assert.Equal(t, 1, prober.callCount())
}

// 探测仍然失败 → 停调时间往后推，流量继续留在副号。
// 这是用户要求的核心行为：切回主号发现还坏，就退回副号，而不是让客户端吃一次断流。
func TestProbeOneExtendsCooldownWhenStillUnhealthy(t *testing.T) {
	until := time.Now().Add(15 * time.Second)
	account := streamProbeAccount(t, until, streamProbeKeywordTruncated, 0)
	state := parseStreamProbeState(account.TempUnschedulableReason)
	require.NotNil(t, state)

	store := &fakeStreamProbeStore{}
	prober := &fakeStreamProber{err: errStreamProbeTestFailure}
	svc := newTestStreamProbeService(store, prober)

	before := time.Now()
	svc.probeOne(context.Background(), account, state)

	setCalls, cleared := store.snapshot()
	assert.Empty(t, cleared, "探测失败绝不能清除停调")
	require.Len(t, setCalls, 2, "应写两次：探测前顶住窗口 + 探测后按退避续停")

	last := setCalls[1]
	assert.Equal(t, int64(9), last.id)

	// 单档：续停一分钟，一分钟后再探。
	expected := before.Add(streamProbeBackoff(1))
	assert.WithinDuration(t, expected, last.until, 5*time.Second)
	assert.True(t, last.until.After(until), "续停时间必须晚于原到点时间，否则主号会提前回池")

	var persisted TempUnschedState
	require.NoError(t, json.Unmarshal([]byte(last.reason), &persisted))
	assert.Equal(t, 1, persisted.ProbeFailures, "失败次数要落库，下一轮才能继续退避")
	assert.Equal(t, streamProbeKeywordTruncated, persisted.MatchedKeyword, "keyword 必须保留，否则下一轮认不出这是自己的停调")
	assert.Equal(t, last.until.Unix(), persisted.UntilUnix)
}

// 探针必须拿「当初把账号罚下线的那个模型」去探。
//
// 回归的是一个让探针对中转类账号完全失效的缺陷：原先固定用 claude.DefaultTestModel，
// 而中转上游只供应自己那份模型清单，探测请求直接吃 404 model_not_found；404 归入
// inconclusive，于是每一轮都「探了但下不了结论」，账号只能靠自然到点回池——探针
// 白跑、还烧上游额度。线上实测连续 18 轮全是 inconclusive。
func TestProbeOneProbesWithModelThatTriggeredCooldown(t *testing.T) {
	until := time.Now().Add(15 * time.Second)
	account := streamProbeAccount(t, until, streamProbeKeywordTruncated, 0)

	state := parseStreamProbeState(account.TempUnschedulableReason)
	require.NotNil(t, state)
	state.Model = "claude-opus-5"

	store := &fakeStreamProbeStore{}
	prober := &fakeStreamProber{}
	svc := newTestStreamProbeService(store, prober)

	svc.probeOne(context.Background(), account, state)

	require.Equal(t, []string{"claude-opus-5"}, prober.models(),
		"必须复现罚号时那条链路，否则中转号永远探不出结论")
}

// 停调状态里没有模型时（旧数据、或触发源没记）回落到默认测试模型，不能把空串发给上游。
func TestProbeOneFallsBackToDefaultModelWhenStateHasNone(t *testing.T) {
	until := time.Now().Add(15 * time.Second)
	account := streamProbeAccount(t, until, streamProbeKeywordTruncated, 0)
	state := parseStreamProbeState(account.TempUnschedulableReason)
	require.NotNil(t, state)
	require.Empty(t, state.Model)

	store := &fakeStreamProbeStore{}
	prober := &fakeStreamProber{}
	svc := newTestStreamProbeService(store, prober)

	svc.probeOne(context.Background(), account, state)

	// 探针服务原样把空模型交给实现方，由 ProbeClaudeStreamHealth 决定回落，
	// 这样回落规则只有一处。
	require.Equal(t, []string{""}, prober.models())
}

// 续停时模型必须一起保住：丢了下一轮又会退回默认模型去探，缺陷复现。
func TestProbeOneKeepsModelWhenExtendingCooldown(t *testing.T) {
	until := time.Now().Add(15 * time.Second)
	account := streamProbeAccount(t, until, streamProbeKeywordTruncated, 0)
	state := parseStreamProbeState(account.TempUnschedulableReason)
	require.NotNil(t, state)
	state.Model = "claude-opus-5"

	store := &fakeStreamProbeStore{}
	prober := &fakeStreamProber{err: errStreamProbeTestFailure}
	svc := newTestStreamProbeService(store, prober)

	svc.probeOne(context.Background(), account, state)

	setCalls, _ := store.snapshot()
	require.Len(t, setCalls, 2)
	for i, call := range setCalls {
		var persisted TempUnschedState
		require.NoError(t, json.Unmarshal([]byte(call.reason), &persisted))
		assert.Equal(t, "claude-opus-5", persisted.Model, "第 %d 次写入丢了模型", i+1)
	}
}

// 连续失败也只续停一分钟——单档，不做逐级退避，主号恢复要快。
func TestProbeOneKeepsFlatOneMinuteBackoff(t *testing.T) {
	for _, priorFailures := range []int{0, 1, 3, 9, 50} {
		until := time.Now().Add(15 * time.Second)
		account := streamProbeAccount(t, until, streamProbeKeywordTruncated, priorFailures)
		state := parseStreamProbeState(account.TempUnschedulableReason)
		require.NotNil(t, state)

		store := &fakeStreamProbeStore{}
		svc := newTestStreamProbeService(store, &fakeStreamProber{err: errStreamProbeTestFailure})

		before := time.Now()
		svc.probeOne(context.Background(), account, state)

		setCalls, _ := store.snapshot()
		require.Len(t, setCalls, 2)
		assert.WithinDuration(t, before.Add(time.Minute), setCalls[1].until, 5*time.Second,
			"prior_failures=%d 也只续停一分钟", priorFailures)

		var persisted TempUnschedState
		require.NoError(t, json.Unmarshal([]byte(setCalls[1].reason), &persisted))
		assert.Equal(t, priorFailures+1, persisted.ProbeFailures, "次数仍要累计，用于观测")
	}
}

func TestStreamProbeBackoff(t *testing.T) {
	for _, failures := range []int{0, 1, 2, 3, 4, 100} {
		assert.Equal(t, time.Minute, streamProbeBackoff(failures),
			"failures=%d 单档一分钟", failures)
	}
}

// 探测开始前窗口必须已经顶住：否则 1 分钟冷却可能在探测这几秒里自然到点，
// 真实流量抢先撞上还没验证过的坏账号——正是本服务要消灭的现象。
func TestProbeOneGuardsWindowBeforeProbing(t *testing.T) {
	until := time.Now().Add(2 * time.Second) // 故意让冷却马上就要到点
	account := streamProbeAccount(t, until, streamProbeKeywordTruncated, 0)
	state := parseStreamProbeState(account.TempUnschedulableReason)
	require.NotNil(t, state)

	store := &fakeStreamProbeStore{}
	var guardAtProbeTime []streamProbeSetCall
	probeStart := time.Time{}
	prober := &fakeStreamProber{err: errStreamProbeTestFailure}
	prober.onProbe = func() {
		probeStart = time.Now()
		guardAtProbeTime, _ = store.snapshot()
	}
	svc := newTestStreamProbeService(store, prober)

	svc.probeOne(context.Background(), account, state)

	require.Len(t, guardAtProbeTime, 1, "探测发生时必须已经写过一次停调（guard）")
	guard := guardAtProbeTime[0]
	assert.Equal(t, int64(9), guard.id)
	assert.True(t, guard.until.After(probeStart.Add(time.Minute)),
		"guard 窗口要覆盖整段探测时间，实际 until=%s probeStart=%s", guard.until, probeStart)
	assert.WithinDuration(t, probeStart.Add(streamProbeGuard), guard.until, 5*time.Second)
}

// 探不了的账号型别 → 还原原窗口，退回改动前的自然到点行为，不因「探不了」被永久关在池外。
func TestProbeOneRestoresWindowForUnsupportedAccount(t *testing.T) {
	assertProbeRestoresWindow(t, errStreamProbeUnsupported)
}

// 探测下不了结论（凭证缺失、鉴权失败、上游非 200）→ 同样只还原窗口，绝不延长停调。
// 这条最要紧：这些失败说明出问题的可能是探针自己（比如 access_token 过期），
// 真当成账号失败去退避，一个健康主号会被越关越久，比改动前更糟。
func TestProbeOneRestoresWindowWhenInconclusive(t *testing.T) {
	assertProbeRestoresWindow(t, errStreamProbeInconclusive)
	// 包装过的也要认出来（实际代码用 %w 带上下文）。
	assertProbeRestoresWindow(t, fmt.Errorf("%w: upstream returned 401: invalid api key", errStreamProbeInconclusive))
}

func assertProbeRestoresWindow(t *testing.T, probeErr error) {
	t.Helper()
	until := time.Now().Add(20 * time.Second)
	account := streamProbeAccount(t, until, streamProbeKeywordTruncated, 0)
	state := parseStreamProbeState(account.TempUnschedulableReason)
	require.NotNil(t, state)

	store := &fakeStreamProbeStore{}
	svc := newTestStreamProbeService(store, &fakeStreamProber{err: probeErr})

	svc.probeOne(context.Background(), account, state)

	setCalls, cleared := store.snapshot()
	assert.Empty(t, cleared)
	require.Len(t, setCalls, 2, "guard 之后要把窗口还原回去")
	assert.WithinDuration(t, until, setCalls[1].until, time.Second,
		"必须还原成原到点时间，而不是往后推")

	var persisted TempUnschedState
	require.NoError(t, json.Unmarshal([]byte(setCalls[1].reason), &persisted))
	assert.Equal(t, 0, persisted.ProbeFailures, "下不了结论不算失败，不该累计退避")
}

// --- runOnce ---

func TestRunOnceSkipsWhenPanelSwitchOff(t *testing.T) {
	until := time.Now().Add(10 * time.Second)
	store := &fakeStreamProbeStore{accounts: []Account{*streamProbeAccount(t, until, streamProbeKeywordTruncated, 0)}}
	prober := &fakeStreamProber{err: errStreamProbeTestFailure}

	for name, settings := range map[string]*StreamTimeoutSettings{
		"开关关闭":   {Enabled: false, Action: StreamTimeoutActionTempUnsched},
		"动作不是停调": {Enabled: true, Action: "log"},
		"配置为空":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			svc := NewAccountStreamProbeService(store, prober, &fakeStreamProbeSettings{settings: settings}, nil, streamProbeScanInterval)
			svc.runOnce()
			assert.Zero(t, prober.callCount(), "面板开关没开时探针应完全停摆")
		})
	}
}

func TestRunOnceProbesEligibleAccountOnly(t *testing.T) {
	now := time.Now()
	eligible := *streamProbeAccount(t, now.Add(10*time.Second), streamProbeKeywordTruncated, 0)
	tooEarly := *streamProbeAccount(t, now.Add(10*time.Minute), streamProbeKeywordTruncated, 0)
	tooEarly.ID = 11
	otherSubsystem := *streamProbeAccount(t, now.Add(10*time.Second), "rate_limit", 0)
	otherSubsystem.ID = 12

	store := &fakeStreamProbeStore{accounts: []Account{eligible, tooEarly, otherSubsystem}}
	prober := &fakeStreamProber{err: nil}
	settings := &fakeStreamProbeSettings{settings: &StreamTimeoutSettings{
		Enabled: true, Action: StreamTimeoutActionTempUnsched,
	}}
	svc := NewAccountStreamProbeService(store, prober, settings, nil, streamProbeScanInterval)

	svc.runOnce()

	assert.Equal(t, 1, prober.callCount())
	_, cleared := store.snapshot()
	assert.Equal(t, []int64{9}, cleared, "只有到期临近且属于本家族的账号被处理")
}

func TestStartNoopWithoutInterval(t *testing.T) {
	svc := NewAccountStreamProbeService(&fakeStreamProbeStore{}, &fakeStreamProber{}, nil, nil, 0)
	svc.Start()
	svc.Stop() // interval<=0 时没有 goroutine，Stop 不该阻塞
}

// --- SSE 判定 ---

func TestProbeReadClaudeStream(t *testing.T) {
	t.Run("走到 message_stop 算健康", func(t *testing.T) {
		body := "event: message_start\n" +
			"data: {\"type\":\"message_start\"}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n"
		assert.NoError(t, probeReadClaudeStream(strings.NewReader(body)))
	})

	t.Run("[DONE] 也算终止", func(t *testing.T) {
		assert.NoError(t, probeReadClaudeStream(strings.NewReader("data: [DONE]\n\n")))
	})

	// 线上断流的真实形态：200 起头、内容发了一半、上游把连接关了却没送终止事件。
	t.Run("没有终止事件就 EOF 算断流", func(t *testing.T) {
		body := "data: {\"type\":\"message_start\"}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n"
		err := probeReadClaudeStream(strings.NewReader(body))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "without terminal event")
	})

	t.Run("空流算断流", func(t *testing.T) {
		require.Error(t, probeReadClaudeStream(strings.NewReader("")))
	})

	t.Run("error 事件按上游报错处理", func(t *testing.T) {
		body := "data: {\"type\":\"error\",\"error\":{\"message\":\"overloaded_error\"}}\n\n"
		err := probeReadClaudeStream(strings.NewReader(body))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "overloaded_error")
	})
}

func TestProbeInspectSSELine(t *testing.T) {
	done, err := probeInspectSSELine("data: {\"type\":\"message_stop\"}")
	assert.True(t, done)
	assert.NoError(t, err)

	done, err = probeInspectSSELine("data: {\"type\":\"content_block_delta\"}")
	assert.False(t, done)
	assert.NoError(t, err)

	done, err = probeInspectSSELine("event: message_stop")
	assert.False(t, done, "非 data: 行不做判定")
	assert.NoError(t, err)

	done, err = probeInspectSSELine("data: 这不是 JSON")
	assert.False(t, done, "解析不了的行跳过而不是当成失败")
	assert.NoError(t, err)

	done, err = probeInspectSSELine("")
	assert.False(t, done)
	assert.NoError(t, err)
}

func TestIsStreamDeliveryKeyword(t *testing.T) {
	assert.True(t, isStreamDeliveryKeyword(streamProbeKeywordTruncated))
	assert.True(t, isStreamDeliveryKeyword(streamProbeKeywordTimeout))
	assert.True(t, isStreamDeliveryKeyword(streamProbeKeywordRefusal))
	assert.False(t, isStreamDeliveryKeyword("rate_limit"))
	assert.False(t, isStreamDeliveryKeyword(""))
}

func TestParseStreamProbeState(t *testing.T) {
	assert.Nil(t, parseStreamProbeState(""))
	assert.Nil(t, parseStreamProbeState("余额不足"))
	assert.Nil(t, parseStreamProbeState("{坏 JSON"))

	state := parseStreamProbeState(`{"matched_keyword":"stream_truncated","probe_failures":3}`)
	require.NotNil(t, state)
	assert.Equal(t, streamProbeKeywordTruncated, state.MatchedKeyword)
	assert.Equal(t, 3, state.ProbeFailures)
}
