package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 需求：多个同优先级账号在「都能用」的情况下也要随机轮换，而不是永远先挑同一个。
//
// 原实现 sameLastUsedAt 要求两个 LastUsedAt 精确落在同一秒（a.Unix() == b.Unix()），
// 而线上 LastUsedAt 是毫秒级真实时间戳，同一秒几乎不可能命中，于是排序组恒为单元素、
// 两个 shuffle 函数从不生效，Layer 2 退化成完全确定性的 LRU。
// rotate_last_used_bucket 引入时间窗口，让「同样陈旧」的同级账号进入同一组参与随机打散。
func TestRotateLastUsedBucket_GroupsNearbyLastUsedForShuffle(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	t.Run("same-second requirement makes millisecond timestamps distinct", func(t *testing.T) {
		withLastUsedGroupTolerance(t, 0)
		a := base.Add(500 * time.Millisecond)
		b := base.Add(1400 * time.Millisecond) // 相差 900ms 但跨了秒边界：真实流量里非常常见
		require.False(t, sameLastUsedAt(&a, &b), "旧口径下毫秒级差异会跨秒，分组恒为单元素")
	})

	t.Run("tolerance window groups equally stale accounts", func(t *testing.T) {
		withLastUsedGroupTolerance(t, time.Minute)
		a := base
		b := base.Add(900 * time.Millisecond)
		c := base.Add(30 * time.Second)
		outside := base.Add(2 * time.Minute)
		require.True(t, sameLastUsedAt(&a, &b))
		require.True(t, sameLastUsedAt(&a, &c))
		require.False(t, sameLastUsedAt(&a, &outside), "窗口之外仍按 LRU 先后区分，主序不变")
	})

	t.Run("nil means never used and stays its own group boundary", func(t *testing.T) {
		withLastUsedGroupTolerance(t, time.Minute)
		a := base
		require.True(t, sameLastUsedAt(nil, nil))
		require.False(t, sameLastUsedAt(nil, &a))
		require.False(t, sameLastUsedAt(&a, nil))
	})
}

// 端到端口径：同优先级、同负载、LastUsedAt 相差毫秒级的账号，
// 多次排序后不能总是同一个排在最前——否则线上就是「只请求一个号」。
func TestRotateLastUsedBucket_SamePriorityAccountsRotate(t *testing.T) {
	withLastUsedGroupTolerance(t, time.Minute)
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	newSet := func() []accountWithLoad {
		t1, t2, t3 := base, base.Add(120*time.Millisecond), base.Add(1500*time.Millisecond)
		return []accountWithLoad{
			{account: &Account{ID: 1, Priority: 0, LastUsedAt: &t1}, loadInfo: &AccountLoadInfo{AccountID: 1}},
			{account: &Account{ID: 2, Priority: 0, LastUsedAt: &t2}, loadInfo: &AccountLoadInfo{AccountID: 2}},
			{account: &Account{ID: 3, Priority: 0, LastUsedAt: &t3}, loadInfo: &AccountLoadInfo{AccountID: 3}},
		}
	}

	firsts := map[int64]int{}
	for i := 0; i < 200; i++ {
		set := newSet()
		shuffleWithinSortGroups(set)
		firsts[set[0].account.ID]++
	}
	require.Len(t, firsts, 3, "三个同级同负载账号都应有机会被排在最前，实际只出现 %v", firsts)
	for id, n := range firsts {
		require.Greater(t, n, 10, "账号 %d 只被选中 %d/200 次，分布过于集中", id, n)
	}
}

// 窗口之外的账号不参与打散：更久未使用的账号必须稳定排在前面，保证 LRU 公平性不被随机性吃掉。
func TestRotateLastUsedBucket_OutsideWindowKeepsLRUOrder(t *testing.T) {
	withLastUsedGroupTolerance(t, time.Minute)
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	oldest, recent := base, base.Add(10*time.Minute)

	for i := 0; i < 50; i++ {
		set := []accountWithLoad{
			{account: &Account{ID: 1, Priority: 0, LastUsedAt: &oldest}, loadInfo: &AccountLoadInfo{AccountID: 1}},
			{account: &Account{ID: 2, Priority: 0, LastUsedAt: &recent}, loadInfo: &AccountLoadInfo{AccountID: 2}},
		}
		shuffleWithinSortGroups(set)
		require.Equal(t, int64(1), set[0].account.ID, "窗口外的最久未使用账号必须稳定优先")
	}
}

// 主号档（priority 更小）与副号档必须各自成组打散，随机性绝不能把副号顶到主号前面。
// 有多个主号时这条尤其关键：主号之间要轮询，但轮询不得越过档位把请求降级给副号。
func TestRotateLastUsedBucket_ShuffleNeverCrossesPriorityTiers(t *testing.T) {
	withLastUsedGroupTolerance(t, time.Minute)
	base := time.Date(2026, 8, 20, 22, 0, 0, 0, time.UTC)

	seenFirst := map[int64]int{}
	for i := 0; i < 200; i++ {
		// 四个号 LastUsedAt 全落在同一个容差窗口内，只有优先级不同。
		p1, p2, s1, s2 := base, base.Add(200*time.Millisecond), base.Add(400*time.Millisecond), base.Add(600*time.Millisecond)
		set := []accountWithLoad{
			{account: &Account{ID: 5, Priority: 0, LastUsedAt: &p1}, loadInfo: &AccountLoadInfo{AccountID: 5}},
			{account: &Account{ID: 12, Priority: 0, LastUsedAt: &p2}, loadInfo: &AccountLoadInfo{AccountID: 12}},
			{account: &Account{ID: 3, Priority: 10, LastUsedAt: &s1}, loadInfo: &AccountLoadInfo{AccountID: 3}},
			{account: &Account{ID: 11, Priority: 10, LastUsedAt: &s2}, loadInfo: &AccountLoadInfo{AccountID: 11}},
		}
		shuffleWithinSortGroups(set)
		require.Equal(t, 0, set[0].account.Priority, "副号被打散到了主号之前：%v", set[0].account.ID)
		require.Equal(t, 0, set[1].account.Priority, "第二位也必须仍是主号档")
		seenFirst[set[0].account.ID]++
	}
	require.Len(t, seenFirst, 2, "两个主号都应有机会排在最前，实际 %v", seenFirst)
	for id, n := range seenFirst {
		require.Greater(t, n, 20, "主号 %d 只排到最前 %d/200 次，主号档内轮询过于集中", id, n)
	}
}

func withLastUsedGroupTolerance(t *testing.T, d time.Duration) {
	t.Helper()
	prev := lastUsedGroupTolerance
	SetLastUsedGroupTolerance(d)
	t.Cleanup(func() { lastUsedGroupTolerance = prev })
}
