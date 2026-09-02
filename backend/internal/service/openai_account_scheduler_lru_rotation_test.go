package service

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// OpenAI 侧调度的 LRU 轮询。
//
// 背景：线上 group 2（openai）有两个同优先级账号，几天里 100% 的请求都落在账号 ID 较小的
// 那一个上，另一个从不被调用。根因是 isOpenAIAccountCandidateBetter 的比较链末端是
// 「账号 ID 升序」而中间没有 LastUsedAt 这一档 —— Anthropic 侧（gateway_scheduling.go）
// 整套公平性都建立在 LastUsedAt 上，OpenAI 侧完全没有。叠上 lb_top_k=1 把候选裁成一个、
// 加权抽样对单元素直接返回，结果是彻底确定性的「永远选 ID 最小的账号」。
//
// 本文件刻意不带 //go:build unit：默认 go test 就要跑到（见 unit 标签假绿的历史教训）。

func lruCandidate(id int64, lastUsed *time.Time) openAIAccountCandidateScore {
	return openAIAccountCandidateScore{
		account:  &Account{ID: id, Priority: 0, Concurrency: 5, LastUsedAt: lastUsed},
		loadInfo: &AccountLoadInfo{AccountID: id},
	}
}

func lruTimePtr(t time.Time) *time.Time {
	return &t
}

func TestOpenAICandidateComparatorPrefersLeastRecentlyUsed(t *testing.T) {
	now := time.Now()
	// 故意让「刚用过」的那个账号拿更小的 ID：只有 LRU 真正生效，它才会输。
	recent := lruCandidate(1, lruTimePtr(now.Add(-1*time.Minute)))
	stale := lruCandidate(6, lruTimePtr(now.Add(-33*time.Hour)))

	require.True(t, isOpenAIAccountCandidateBetter(stale, recent), "最久未用的账号应当胜出")
	require.False(t, isOpenAIAccountCandidateBetter(recent, stale), "反向必须不成立，否则比较器不是严格弱序")
}

func TestOpenAICandidateComparatorTreatsNeverUsedAsOldest(t *testing.T) {
	now := time.Now()
	neverUsed := lruCandidate(9, nil)
	used := lruCandidate(2, lruTimePtr(now.Add(-time.Hour)))

	require.True(t, isOpenAIAccountCandidateBetter(neverUsed, used), "从未被调度过的账号最优先")
	require.False(t, isOpenAIAccountCandidateBetter(used, neverUsed))
}

// 负载感知仍排在 LRU 之前：一个忙着的老账号不该顶掉一个空闲的新账号，
// 与 Anthropic 侧 Priority -> LoadRate -> LastUsedAt 的次序一致。
func TestOpenAICandidateComparatorLoadRateOutranksLRU(t *testing.T) {
	now := time.Now()
	busyStale := lruCandidate(6, lruTimePtr(now.Add(-33*time.Hour)))
	busyStale.loadInfo.LoadRate = 80
	idleRecent := lruCandidate(1, lruTimePtr(now.Add(-time.Second)))

	require.True(t, isOpenAIAccountCandidateBetter(idleRecent, busyStale), "负载低的账号先胜出")
}

// lastUsedGroupTolerance 是包级可变全局：NewGatewayService 会在构造时按配置刷写它
// （gateway_service.go），零值 config 就把它刷成 0。因此凡是断言依赖容差具体取值的用例，
// 都必须自己钉住取值，不能读环境里的当前值 —— 否则同一个用例在 go test 与
// go test -tags unit 下会因为跑在哪些邻居后面而红绿不同。
func TestOpenAILastUsedRotationBucket(t *testing.T) {
	withLastUsedGroupTolerance(t, time.Minute)
	base := time.Date(2026, 9, 2, 20, 0, 0, 0, time.UTC)

	t.Run("容差窗口内归同一桶", func(t *testing.T) {
		a := &Account{LastUsedAt: lruTimePtr(base)}
		b := &Account{LastUsedAt: lruTimePtr(base.Add(200 * time.Millisecond))}
		require.Equal(t, openAILastUsedRotationBucket(a), openAILastUsedRotationBucket(b))
	})

	t.Run("跨窗口分属不同桶且保持先后", func(t *testing.T) {
		older := &Account{LastUsedAt: lruTimePtr(base.Add(-2 * lastUsedGroupTolerance))}
		newer := &Account{LastUsedAt: lruTimePtr(base)}
		require.Less(t, openAILastUsedRotationBucket(older), openAILastUsedRotationBucket(newer))
	})

	t.Run("nil 是最小桶", func(t *testing.T) {
		require.Less(t, openAILastUsedRotationBucket(&Account{}), openAILastUsedRotationBucket(&Account{LastUsedAt: lruTimePtr(base)}))
		require.Equal(t, openAILastUsedRotationBucket(nil), openAILastUsedRotationBucket(&Account{}))
	})

	t.Run("容差为 0 回到同一秒的旧行为", func(t *testing.T) {
		withLastUsedGroupTolerance(t, 0)

		sameSecond := &Account{LastUsedAt: lruTimePtr(base.Add(700 * time.Millisecond))}
		require.Equal(t, openAILastUsedRotationBucket(&Account{LastUsedAt: lruTimePtr(base)}), openAILastUsedRotationBucket(sameSecond))
		require.NotEqual(t, openAILastUsedRotationBucket(&Account{LastUsedAt: lruTimePtr(base)}), openAILastUsedRotationBucket(&Account{LastUsedAt: lruTimePtr(base.Add(time.Second))}))
	})
}

// 同一个 LRU 桶内由每请求随机名次决定，避免并发请求读到同一份快照时全部挤向同一个账号。
// tieBreak 为 0（未赋值）时退回按 ID，保证管理端分数快照一类非调度路径行为不变。
func TestOpenAICandidateComparatorUsesTieBreakWithinSameBucket(t *testing.T) {
	now := time.Now()
	low := lruCandidate(1, lruTimePtr(now))
	high := lruCandidate(6, lruTimePtr(now.Add(100*time.Millisecond)))
	require.Equal(t, openAILastUsedRotationBucket(low.account), openAILastUsedRotationBucket(high.account),
		"前置条件：两者必须落在同一个 LRU 桶")

	require.True(t, isOpenAIAccountCandidateBetter(low, high), "tieBreak 未赋值时按 ID 升序")

	low.tieBreak = 900
	high.tieBreak = 100
	require.True(t, isOpenAIAccountCandidateBetter(high, low), "tieBreak 更小者胜出，顶掉 ID 偏好")
}

// 比较器同时喂给最小堆和 sort.Slice，必须是严格弱序：任意一对不得双向成立，
// 排序结果也必须自洽。容差判定之所以用「量化成桶」而不是成对的 |a-b| <= tol，
// 就是因为后者不满足传递性。
func TestOpenAICandidateComparatorIsStrictWeakOrdering(t *testing.T) {
	withLastUsedGroupTolerance(t, time.Minute)
	now := time.Now()
	candidates := []openAIAccountCandidateScore{
		lruCandidate(1, lruTimePtr(now)),
		lruCandidate(2, lruTimePtr(now.Add(-lastUsedGroupTolerance/3))),
		lruCandidate(3, lruTimePtr(now.Add(-2*lastUsedGroupTolerance/3))),
		lruCandidate(4, lruTimePtr(now.Add(-5*lastUsedGroupTolerance))),
		lruCandidate(5, nil),
	}
	candidates[2].score = 1
	candidates[3].loadInfo.WaitingCount = 2

	for i := range candidates {
		for j := range candidates {
			if i == j {
				require.False(t, isOpenAIAccountCandidateBetter(candidates[i], candidates[j]), "自反位置不得成立")
				continue
			}
			if isOpenAIAccountCandidateBetter(candidates[i], candidates[j]) {
				require.False(t, isOpenAIAccountCandidateBetter(candidates[j], candidates[i]),
					"账号 %d 与 %d 双向都更优", candidates[i].account.ID, candidates[j].account.ID)
			}
		}
	}

	ranked := selectTopKOpenAICandidates(candidates, len(candidates))
	require.True(t, sort.SliceIsSorted(ranked, func(i, j int) bool {
		return isOpenAIAccountCandidateBetter(ranked[i], ranked[j])
	}))
}

// 复现线上形态：lb_top_k=1 把候选裁成一个，因此轮询只能来自比较器本身。
func TestSelectTopKOpenAICandidatesTopOneRotatesByLastUsed(t *testing.T) {
	now := time.Now()

	first := selectTopKOpenAICandidates([]openAIAccountCandidateScore{
		lruCandidate(1, lruTimePtr(now.Add(-time.Minute))),
		lruCandidate(6, lruTimePtr(now.Add(-33*time.Hour))),
	}, 1)
	require.Len(t, first, 1)
	require.Equal(t, int64(6), first[0].account.ID, "topK=1 时也要选最久未用的账号")

	// 账号 6 刚被用过之后，下一次必须换回账号 1 —— 这就是轮询。
	second := selectTopKOpenAICandidates([]openAIAccountCandidateScore{
		lruCandidate(1, lruTimePtr(now.Add(-time.Minute))),
		lruCandidate(6, lruTimePtr(now)),
	}, 1)
	require.Len(t, second, 1)
	require.Equal(t, int64(1), second[0].account.ID)
}

// 端到端到 buildOpenAIAccountLoadPlan：用线上那套设置（高级调度器开、lb_top_k=1、
// 除 priority=1 外全部权重为 0），验证选择顺序真的按 LRU 轮转，而不是钉在账号 1。
func TestBuildOpenAIAccountLoadPlanRotatesUnderProductionSettings(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	defer resetOpenAIAdvancedSchedulerSettingCacheForTest()

	cfg := &config.Config{}
	repo := &openAIAdvancedSchedulerSettingRepoStub{values: map[string]string{
		openAIAdvancedSchedulerSettingKey:                       "true",
		SettingKeyOpenAIAdvancedSchedulerStickyWeightedEnabled:  "true",
		SettingKeyOpenAIAdvancedSchedulerLBTopK:                 "1",
		SettingKeyOpenAIAdvancedSchedulerWeightPriority:         "1",
		SettingKeyOpenAIAdvancedSchedulerWeightLoad:             "0",
		SettingKeyOpenAIAdvancedSchedulerWeightQueue:            "0",
		SettingKeyOpenAIAdvancedSchedulerWeightErrorRate:        "0",
		SettingKeyOpenAIAdvancedSchedulerWeightTTFT:             "0",
		SettingKeyOpenAIAdvancedSchedulerWeightReset:            "0",
		SettingKeyOpenAIAdvancedSchedulerWeightQuotaHeadroom:    "0",
		SettingKeyOpenAIAdvancedSchedulerWeightUpstreamCost:     "0",
		SettingKeyOpenAIAdvancedSchedulerWeightPreviousResponse: "0",
		SettingKeyOpenAIAdvancedSchedulerWeightSessionSticky:    "0",
	}}
	svc := &OpenAIGatewayService{
		cfg:              cfg,
		rateLimitService: &RateLimitService{settingService: NewSettingService(repo, cfg)},
	}
	scheduler := &defaultOpenAIAccountScheduler{service: svc}
	ctx := context.Background()
	require.Equal(t, 1, svc.openAIWSLBTopKForRequest(ctx), "前置条件：复现线上的 lb_top_k=1")

	groupID := int64(2)
	// 稳定的会话锚点：deriveOpenAISelectionSeed 只在没有锚点时才掺入时间熵，
	// 所以这里的种子是固定的 —— 轮转必须完全来自 LRU，不能依赖随机。
	req := OpenAIAccountScheduleRequest{
		GroupID:        &groupID,
		Platform:       PlatformOpenAI,
		RequestedModel: "gpt-5.6",
		SessionHash:    "stable-session-anchor",
		StickyWeighted: true,
	}

	now := time.Now()
	accRecent := &Account{ID: 1, Priority: 0, Concurrency: 5, LastUsedAt: lruTimePtr(now.Add(-time.Minute))}
	accStale := &Account{ID: 6, Priority: 0, Concurrency: 5, LastUsedAt: lruTimePtr(now.Add(-33 * time.Hour))}
	loadMap := map[int64]*AccountLoadInfo{
		1: {AccountID: 1},
		6: {AccountID: 6},
	}

	plan := scheduler.buildOpenAIAccountLoadPlan(ctx, req, []*Account{accRecent, accStale}, loadMap)
	require.Equal(t, 1, plan.topK)
	require.Len(t, plan.candidates, 2)
	require.Equal(t, plan.candidates[0].score, plan.candidates[1].score, "同优先级下两者分数必须相同，否则本用例证不到 LRU")
	require.NotZero(t, plan.candidates[0].tieBreak, "调度路径必须给候选赋随机名次")
	require.Len(t, plan.selectionOrder, 1)
	require.Equal(t, int64(6), plan.selectionOrder[0].account.ID, "久未使用的账号 6 必须被选中")

	// 账号 6 用过之后翻转
	accStale.LastUsedAt = lruTimePtr(now)
	plan = scheduler.buildOpenAIAccountLoadPlan(ctx, req, []*Account{accRecent, accStale}, loadMap)
	require.Len(t, plan.selectionOrder, 1)
	require.Equal(t, int64(1), plan.selectionOrder[0].account.ID, "轮到账号 1")
}
