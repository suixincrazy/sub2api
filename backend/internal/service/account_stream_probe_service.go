package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// 本文件把「主号是否已经从流交付失败里恢复」这件事的试错成本，从客户端请求搬到后台。
//
// 为什么需要：流一旦提交（200 已发、首字节已 flush），上游中途截断就再也换不了账号
// ——gateway_anthropic_passthrough.go 那句 "stream truncated after commit, no failover
// possible" 说的就是这个。于是「冷却到点后拿一条真实请求去试主号还行不行」这个动作，
// 本身必然让某个客户端吃一次断流：调度器把主号放回池子，真实请求撞上去，断流，再罚一分钟。
// 观测到的现象就是主号反复「放回来—断流—再罚」，用户得手动发「继续」。
//
// 做法：在冷却到点之前，先用一条后台探测请求替客户端试错。
//   - 探到完整的 message_stop → 清除停调，主号立刻回到调度池（比自然到点还早一点）；
//   - 探不到终止事件         → 把停调时间再往后推一分钟，真实流量继续留在副号，
//                              下一轮再探，如此循环直到主号真的可用。
//
// 于是「切回主号失败就退回副号，如此循环直到主号真的可用」在后台闭环完成，
// 调度器一行都不用改：Account.IsSchedulable() 只看 temp_unschedulable_until 是否已过期
// （account.go:194），把这个时间戳往后推就等于让主号继续留在池外。
//
// 骨架沿用 CNProviderBalanceCheckService 的 Start/Stop/runOnce + ticker，
// 并同样遵守「只清自己认领的停调」——靠 reason 里的 matched_keyword 辨认，
// 绝不碰限流、余额、过载等别的子系统写下的停调。

// 这三个 keyword 由 RateLimitService 的 HandleStreamTruncated / HandleStreamTimeout /
// HandleStreamRefused 写进 temp_unschedulable_reason（见 ratelimit_service.go 里
// handleStreamDeliveryFailure 的调用点）。那边改字面量必须同步改这里，
// 否则探针会认不出哪些冷却是自己该管的，退化成完全不工作。
const (
	streamProbeKeywordTruncated = "stream_truncated"
	streamProbeKeywordTimeout   = "stream_timeout"
	streamProbeKeywordRefusal   = "stream_safety_refusal"
)

const (
	// streamProbeScanInterval 扫描周期。必须明显小于最短冷却（面板默认 1 分钟），
	// 否则冷却会在探针介入之前自然到点，真实流量抢先撞上坏账号。
	streamProbeScanInterval = 20 * time.Second
	// streamProbeLead 剩余冷却小于该值才开探，避免刚罚下去就探、白烧上游额度。
	streamProbeLead = 30 * time.Second
	// streamProbeGuard 探测期间先把停调窗口顶到这么久。
	// 目的是让「探测中」这段时间不可能被真实流量插进来；进程若在探测途中挂掉，
	// 账号也只是多停调这么久，属于 fail-safe 方向。
	streamProbeGuard = 2 * time.Minute
	// streamProbeTimeout 单次探测的整体超时。
	streamProbeTimeout = 45 * time.Second
	// streamProbeConcurrency 同时探测的账号数，避免一轮里对上游造成尖峰。
	streamProbeConcurrency = 2
)

// streamProbeRetryMinutes 探测失败后的续停时长。
// 保持和面板 stream_timeout_settings.temp_unsched_minutes 一样的 1 分钟节奏：
// 探不通就再停一分钟，一分钟后再探，如此循环直到主号真的可用。
// 不做逐级退避——恢复要快，代价是对一个彻底坏掉的账号每分钟烧一次探测额度。
const streamProbeRetryMinutes = 1

// errStreamProbeUnsupported 表示这类账号（Bedrock / Vertex service account 等）
// 的探测路径还没实现。返回它的账号会被直接跳过，冷却按原有方式自然到点，
// 也就是保持改动前的行为，不会因为「探不了」被永久关在池外。
var errStreamProbeUnsupported = errors.New("stream probe unsupported for this account type")

// errStreamProbeInconclusive 表示这次探测没能对「账号能不能交付一条完整的流」下结论
// ——凭证缺失、鉴权失败、上游返回非 200 等等。
//
// 为什么必须和真失败区分开：探针失败会自动延长停调，而这里列的这些原因说明
// 出问题的可能是探针自己（比如 access_token 过期——面板那个「测试」按钮同样不刷新
// token，所以这条路径天生有这个盲点），不是账号的流交付能力。真拿这些当失败，
// 一个健康主号会被越关越久，比改动前更糟。
//
// 判据：断流是 200 起头、发到一半没有终止事件的失败。非 200 各有各的子系统
// （限流、过载、余额）负责，探针不越界。
var errStreamProbeInconclusive = errors.New("stream probe inconclusive")

// streamProbeSettingReader 只取本服务需要的那一个配置，便于测试替换。
type streamProbeSettingReader interface {
	GetStreamTimeoutSettings(ctx context.Context) (*StreamTimeoutSettings, error)
}

// streamProbeAccountStore 只声明本服务真正用到的仓储方法。
// 收窄依赖面是为了测试能用十几行的假实现替换，而不必实现整个 AccountRepository；
// 真实的 *accountRepository 结构上就满足它。
type streamProbeAccountStore interface {
	ListByPlatform(ctx context.Context, platform string) ([]Account, error)
	SetTempUnschedulable(ctx context.Context, id int64, until time.Time, reason string) error
	ClearTempUnschedulable(ctx context.Context, id int64) error
}

// streamHealthProber 抽象一次「这个账号现在能不能交付一条完整的流」的探测。
//
// model 是当初把账号罚下线时客户端请求的模型；为空时由实现方回落到默认测试模型。
type streamHealthProber interface {
	ProbeClaudeStreamHealth(ctx context.Context, account *Account, model string) error
}

// AccountStreamProbeService 周期性探测「因流交付失败被临时停调」的 Anthropic 账号，
// 探测通过才放它回调度池，否则把停调时间往后推、让流量继续留在副号。
type AccountStreamProbeService struct {
	accountRepo      streamProbeAccountStore
	prober           streamHealthProber
	settingReader    streamProbeSettingReader
	tempUnschedCache TempUnschedCache
	interval         time.Duration
	stopCh           chan struct{}
	stopOnce         sync.Once
	wg               sync.WaitGroup
}

// NewAccountStreamProbeService 构造探测服务。interval <= 0 时 Start() 直接返回（不启动）。
func NewAccountStreamProbeService(
	accountRepo streamProbeAccountStore,
	prober streamHealthProber,
	settingReader streamProbeSettingReader,
	tempUnschedCache TempUnschedCache,
	interval time.Duration,
) *AccountStreamProbeService {
	return &AccountStreamProbeService{
		accountRepo:      accountRepo,
		prober:           prober,
		settingReader:    settingReader,
		tempUnschedCache: tempUnschedCache,
		interval:         interval,
		stopCh:           make(chan struct{}),
	}
}

func (s *AccountStreamProbeService) Start() {
	if s == nil || s.accountRepo == nil || s.prober == nil {
		return
	}
	if s.interval <= 0 {
		return
	}
	slog.Info("stream_probe_started", "interval", s.interval.String())
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.runOnce()
			case <-s.stopCh:
				return
			}
		}
	}()
}

func (s *AccountStreamProbeService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
}

// runOnce 扫描一轮并对到期临近的账号发起探测。
func (s *AccountStreamProbeService) runOnce() {
	// 复用面板上那个「流超时处理」开关：关掉它就不会再产生这种冷却，
	// 探针也没有存在意义，于是一起停摆，运维只需要记住一个旋钮。
	if s.settingReader != nil {
		settings, err := s.settingReader.GetStreamTimeoutSettings(context.Background())
		if err != nil {
			slog.Warn("stream_probe_get_settings_failed", "error", err)
			return
		}
		if settings == nil || !settings.Enabled || settings.Action != StreamTimeoutActionTempUnsched {
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformAnthropic)
	if err != nil {
		slog.Warn("stream_probe_list_accounts_failed", "error", err)
		return
	}

	now := time.Now()
	type target struct {
		account *Account
		state   *TempUnschedState
	}
	var targets []target
	for i := range accounts {
		account := &accounts[i]
		state, ok := streamProbeShouldProbe(account, now)
		if !ok {
			continue
		}
		targets = append(targets, target{account: account, state: state})
	}
	if len(targets) == 0 {
		return
	}

	sem := make(chan struct{}, streamProbeConcurrency)
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(t target) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.probeOne(ctx, t.account, t.state)
		}(t)
	}
	wg.Wait()
}

// streamProbeShouldProbe 判断某账号这一轮是否该探，并返回解析出的停调状态。
//
// 只认「本家族写下的、仍在生效的、且即将到点的」停调：
//   - 已经到点的不管：那时账号已经回到池子，再去罚它等于无故把正在服务的账号踢下线；
//   - 还早的不管：留到下一轮，省上游额度；
//   - 限流/过载等更强的阻断还在时不管：那些窗口该由各自的子系统负责。
func streamProbeShouldProbe(account *Account, now time.Time) (*TempUnschedState, bool) {
	if account == nil || account.TempUnschedulableUntil == nil {
		return nil, false
	}
	// 管理员手动停用、或账号本身不是 active 的，恢复它不是本服务的职责。
	if !account.IsActive() || !account.Schedulable {
		return nil, false
	}
	if account.RateLimitResetAt != nil && now.Before(*account.RateLimitResetAt) {
		return nil, false
	}
	if account.OverloadUntil != nil && now.Before(*account.OverloadUntil) {
		return nil, false
	}
	state := parseStreamProbeState(account.TempUnschedulableReason)
	if state == nil || !isStreamDeliveryKeyword(state.MatchedKeyword) {
		return nil, false
	}
	remaining := account.TempUnschedulableUntil.Sub(now)
	if remaining <= 0 || remaining > streamProbeLead {
		return nil, false
	}
	return state, true
}

// probeOne 对单个账号执行「顶住窗口 → 探测 → 放行或退避」。
func (s *AccountStreamProbeService) probeOne(ctx context.Context, account *Account, state *TempUnschedState) {
	// 先把停调窗口顶到 guard 之后再探。
	// 顺序很关键：如果先探后写，探测这几秒里冷却可能自然到点，真实流量就抢先撞上
	// 还没验证过的坏账号——那正是本服务要消灭的现象。
	guardUntil := time.Now().Add(streamProbeGuard)
	if err := s.markTempUnschedulable(ctx, account.ID, state, guardUntil); err != nil {
		slog.Warn("stream_probe_guard_failed", "account_id", account.ID, "error", err)
		return
	}

	probeCtx, cancel := context.WithTimeout(ctx, streamProbeTimeout)
	defer cancel()
	probeErr := s.prober.ProbeClaudeStreamHealth(probeCtx, account, state.Model)

	if errors.Is(probeErr, errStreamProbeUnsupported) || errors.Is(probeErr, errStreamProbeInconclusive) {
		// 探不了、或者探了但下不了结论：把窗口还原成原本的到点时间，
		// 退回改动前的自然到点行为。绝不能算作失败去延长停调——那样探针自身的
		// 毛病（过期 token 之类）会把一个健康主号越关越久。
		originalUntil := time.Unix(state.UntilUnix, 0)
		if state.UntilUnix > 0 && originalUntil.After(time.Now()) {
			if err := s.markTempUnschedulable(ctx, account.ID, state, originalUntil); err != nil {
				slog.Warn("stream_probe_restore_failed", "account_id", account.ID, "error", err)
			}
		} else if err := s.clearTempUnschedulable(ctx, account.ID); err != nil {
			slog.Warn("stream_probe_clear_failed", "account_id", account.ID, "error", err)
		}
		if !errors.Is(probeErr, errStreamProbeUnsupported) {
			slog.Info("stream_probe_inconclusive",
				"account_id", account.ID,
				"keyword", state.MatchedKeyword,
				"error", probeErr.Error())
		}
		return
	}

	if probeErr == nil {
		if err := s.clearTempUnschedulable(ctx, account.ID); err != nil {
			slog.Warn("stream_probe_clear_failed", "account_id", account.ID, "error", err)
			return
		}
		slog.Info("stream_probe_recovered",
			"account_id", account.ID,
			"keyword", state.MatchedKeyword,
			"previous_probe_failures", state.ProbeFailures)
		return
	}

	failures := state.ProbeFailures + 1
	backoff := streamProbeBackoff(failures)
	next := *state
	next.ProbeFailures = failures
	until := time.Now().Add(backoff)
	if err := s.markTempUnschedulable(ctx, account.ID, &next, until); err != nil {
		slog.Warn("stream_probe_extend_failed", "account_id", account.ID, "error", err)
		return
	}
	slog.Info("stream_probe_still_unhealthy",
		"account_id", account.ID,
		"keyword", state.MatchedKeyword,
		"probe_failures", failures,
		"next_probe_after", backoff.String(),
		"until", until,
		"error", probeErr.Error())
}

// markTempUnschedulable 同时写库和缓存，保持与 triggerStreamTimeoutTempUnsched 一致：
// 调度判定读库里的字段，但缓存也有别处在读，两边不同步会出现诡异的半恢复状态。
func (s *AccountStreamProbeService) markTempUnschedulable(
	ctx context.Context, accountID int64, state *TempUnschedState, until time.Time,
) error {
	next := *state
	next.UntilUnix = until.Unix()

	reason := next.ErrorMessage
	if raw, err := json.Marshal(&next); err == nil {
		reason = string(raw)
	}
	if err := s.accountRepo.SetTempUnschedulable(ctx, accountID, until, reason); err != nil {
		return err
	}
	if s.tempUnschedCache != nil {
		if err := s.tempUnschedCache.SetTempUnsched(ctx, accountID, &next); err != nil {
			slog.Warn("stream_probe_cache_set_failed", "account_id", accountID, "error", err)
		}
	}
	return nil
}

func (s *AccountStreamProbeService) clearTempUnschedulable(ctx context.Context, accountID int64) error {
	if err := s.accountRepo.ClearTempUnschedulable(ctx, accountID); err != nil {
		return err
	}
	if s.tempUnschedCache != nil {
		if err := s.tempUnschedCache.DeleteTempUnsched(ctx, accountID); err != nil {
			slog.Warn("stream_probe_cache_delete_failed", "account_id", accountID, "error", err)
		}
	}
	return nil
}

func streamProbeBackoff(failures int) time.Duration {
	// 单档：不管失败多少次都只续停一分钟，下一轮继续探。
	// failures 仅用于日志和观测，不再影响时长。
	return streamProbeRetryMinutes * time.Minute
}

func isStreamDeliveryKeyword(keyword string) bool {
	switch keyword {
	case streamProbeKeywordTruncated, streamProbeKeywordTimeout, streamProbeKeywordRefusal:
		return true
	default:
		return false
	}
}

// parseStreamProbeState 解析 temp_unschedulable_reason。
// 早期/其它来源的 reason 可能是纯文本而不是 JSON，那种解析失败即视为「不是本家族的」。
func parseStreamProbeState(reason string) *TempUnschedState {
	reason = strings.TrimSpace(reason)
	if reason == "" || !strings.HasPrefix(reason, "{") {
		return nil
	}
	var state TempUnschedState
	if err := json.Unmarshal([]byte(reason), &state); err != nil {
		return nil
	}
	return &state
}

// ProbeClaudeStreamHealth 无 gin 上下文的 Claude 流健康探测。
//
// 只判断一件事：这条 SSE 能不能一路走到 message_stop。
// 断流的根因正是「上游把流关了却没送终止事件」，所以终止事件本身就是健康信号；
// 只看 HTTP 200 会漏报——观测到的断流全都是 200 起头的。
//
// 返回值分三类，调用方据此决定是否延长停调：
//   - nil                          → 账号健康，可以放回调度池；
//   - errStreamProbeInconclusive   → 下不了结论（凭证缺失、非 200 等），维持原窗口；
//   - 其它 error                    → 确实交付不出完整的流，按退避继续停调。
//
// 请求构造刻意与 testClaudeAccountConnection 保持一致（同样的 URL 规范化、
// claude.DefaultHeaders、oauth/apikey 的 beta 头差异、账号级头覆写、代理与 TLS 指纹），
// 但不写任何 SSE 给客户端。那边是面板「测试」按钮的入口、绑死了 *gin.Context，
// 这里没有去动它：那条路径是日常在用的，为后台探测重构它不划算。
// 两处的头/鉴权逻辑因此有重复，改一边时请同步另一边。
//
// 已知盲点：和面板那个按钮一样，这里不刷新 OAuth access_token。token 过期会得到
// 401，而 401 归入 inconclusive，所以最坏结果只是探不出结论、退回自然到点，
// 不会误伤账号。
//
// model 为当初触发停调的客户端请求模型，空则回落到 claude.DefaultTestModel。
// 必须优先用前者：中转类账号的上游只供应自己那份模型清单，拿默认模型去探会吃
// 404 model_not_found，而 404 归入 inconclusive，探针对这类账号就永远下不了结论。
func (s *AccountTestService) ProbeClaudeStreamHealth(ctx context.Context, account *Account, model string) error {
	if s == nil || account == nil {
		return fmt.Errorf("%w: nil service or account", errStreamProbeInconclusive)
	}
	// Bedrock / Vertex service account 的鉴权链路完全不同，这里不实现，
	// 交回 errStreamProbeUnsupported 让调用方跳过、保持原有的自然到点行为。
	if account.IsBedrock() || account.Type == AccountTypeServiceAccount {
		return errStreamProbeUnsupported
	}

	modelID := strings.TrimSpace(model)
	if modelID == "" {
		modelID = claude.DefaultTestModel
	}
	var apiURL, authToken string

	switch {
	case account.IsOAuth():
		apiURL = testClaudeAPIURL
		authToken = account.GetCredential("access_token")
		if authToken == "" {
			return fmt.Errorf("%w: no access token available", errStreamProbeInconclusive)
		}
	case account.Type == AccountTypeAPIKey:
		authToken = account.GetCredential("api_key")
		if authToken == "" {
			return fmt.Errorf("%w: no api key available", errStreamProbeInconclusive)
		}
		modelID = account.GetMappedModel(modelID)
		baseURL := account.GetBaseURL()
		if baseURL == "" {
			baseURL = "https://api.anthropic.com"
		}
		normalized, err := s.validateUpstreamBaseURL(baseURL)
		if err != nil {
			return fmt.Errorf("%w: invalid base url: %v", errStreamProbeInconclusive, err)
		}
		apiURL = strings.TrimSuffix(normalized, "/") + "/v1/messages?beta=true"
	default:
		return errStreamProbeUnsupported
	}

	payload, err := createTestPayload(modelID)
	if err != nil {
		return fmt.Errorf("%w: build payload: %v", errStreamProbeInconclusive, err)
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("%w: marshal payload: %v", errStreamProbeInconclusive, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, strings.NewReader(string(payloadBytes)))
	if err != nil {
		return fmt.Errorf("%w: build request: %v", errStreamProbeInconclusive, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	for key, value := range claude.DefaultHeaders {
		req.Header.Set(key, value)
	}
	if account.IsOAuth() {
		req.Header.Set("anthropic-beta", claude.DefaultBetaHeader)
		req.Header.Set("Authorization", "Bearer "+authToken)
	} else {
		req.Header.Set("anthropic-beta", claude.APIKeyBetaHeader)
		setAnthropicAPIKeyAuthHeader(req.Header, account, authToken)
	}
	account.ApplyHeaderOverrides(req.Header)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	var tlsProfile *tlsfingerprint.Profile
	if s.tlsFPProfileService != nil {
		tlsProfile = s.tlsFPProfileService.ResolveTLSProfile(account)
	}

	resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, tlsProfile)
	if err != nil {
		return fmt.Errorf("stream probe: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// 非 200 不是断流的形态，交给限流/过载/余额各自的子系统处理，探针不越界。
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%w: upstream returned %d: %s",
			errStreamProbeInconclusive, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return probeReadClaudeStream(resp.Body)
}

// probeReadClaudeStream 读 SSE 直到拿到终止事件。
// 没等到 message_stop / [DONE] 就先 EOF —— 那正是线上断流的形态，必须算失败。
func probeReadClaudeStream(body io.Reader) error {
	reader := bufio.NewReader(body)
	for {
		line, readErr := reader.ReadString('\n')
		if line != "" {
			if done, eventErr := probeInspectSSELine(line); done {
				return eventErr
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return errors.New("upstream closed stream without terminal event")
			}
			return fmt.Errorf("stream read error: %w", readErr)
		}
	}
}

// probeInspectSSELine 检查一行 SSE。done=true 表示这行已经给出最终结论。
func probeInspectSSELine(line string) (bool, error) {
	line = strings.TrimSpace(line)
	if line == "" || !sseDataPrefix.MatchString(line) {
		return false, nil
	}
	jsonStr := sseDataPrefix.ReplaceAllString(line, "")
	if jsonStr == "[DONE]" {
		return true, nil
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
		return false, nil
	}
	eventType, _ := data["type"].(string)
	switch eventType {
	case "message_stop":
		return true, nil
	case "error":
		message := "unknown upstream error"
		if errData, ok := data["error"].(map[string]any); ok {
			if msg, ok := errData["message"].(string); ok && msg != "" {
				message = msg
			}
		}
		return true, fmt.Errorf("upstream error event: %s", message)
	default:
		return false, nil
	}
}
