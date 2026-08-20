package service

import (
	"testing"
	"time"
)

// 覆盖两个线上真实故障：
//  1. 主号 safeguards 报错回退副号后，主号恢复了却永不切回；
//  2. 同优先级多个副号，只有被绑上的那个被反复使用，另一个永不轮询。
func TestEvaluateStickyPreemption(t *testing.T) {
	now := time.Date(2026, 8, 19, 18, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time {
		ts := now.Add(d)
		return &ts
	}
	const rotateIdle = 5 * time.Minute

	cand := func(id int64, prio int, last *time.Time) stickyPreemptionCandidate {
		return stickyPreemptionCandidate{ID: id, Priority: prio, LastUsedAt: last}
	}

	tests := []struct {
		name        string
		bound       stickyPreemptionCandidate
		candidates  []stickyPreemptionCandidate
		higher      bool
		rotate      bool
		minIdle     time.Duration
		wantPreempt bool
		wantReason  string
		wantBetter  int64
	}{
		{
			// 线上问题2：绑定在副号3(priority=10)，主号5(priority=0)已恢复。
			name:        "higher priority primary recovered preempts secondary",
			bound:       cand(3, 10, at(-30*time.Second)),
			candidates:  []stickyPreemptionCandidate{cand(5, 0, at(-time.Hour)), cand(6, 10, at(-2*time.Hour))},
			higher:      true,
			rotate:      true,
			minIdle:     rotateIdle,
			wantPreempt: true,
			wantReason:  "higher_priority_recovered",
			wantBetter:  5,
		},
		{
			// 主号恢复不受 MinIdle 约束：刚用过副号也要立刻切回。
			name:        "higher priority ignores min idle",
			bound:       cand(3, 10, at(-time.Second)),
			candidates:  []stickyPreemptionCandidate{cand(5, 0, at(-time.Hour))},
			higher:      true,
			rotate:      true,
			minIdle:     rotateIdle,
			wantPreempt: true,
			wantReason:  "higher_priority_recovered",
			wantBetter:  5,
		},
		{
			// 线上问题3：3/6 同为 priority=10，6 比 3 旧出远超 MinIdle。
			name:        "same priority rotates to least recently used",
			bound:       cand(3, 10, at(-10*time.Minute)),
			candidates:  []stickyPreemptionCandidate{cand(6, 10, at(-2*time.Hour))},
			higher:      true,
			rotate:      true,
			minIdle:     rotateIdle,
			wantPreempt: true,
			wantReason:  "same_priority_rotate",
			wantBetter:  6,
		},
		{
			// MinIdle 防抖：候选只比绑定账号旧 1 分钟（< minIdle），不换号，保住 prompt cache。
			// 门控看的是「候选比绑定多饿了多久」，不是绑定账号自身空闲多久：
			// 后者在连续流量下恒为 0，会让轮询永不触发（线上"只请求一个号"的根因）。
			name:        "same priority holds binding when candidate barely older",
			bound:       cand(3, 10, at(-30*time.Second)),
			candidates:  []stickyPreemptionCandidate{cand(6, 10, at(-90*time.Second))},
			higher:      true,
			rotate:      true,
			minIdle:     rotateIdle,
			wantPreempt: false,
		},
		{
			// 连续流量回归：绑定账号刚刚被用过（空闲 0），候选已饿 2 小时。
			// 旧实现用 boundIdle 做门控会在这里拒绝轮询，正是线上故障现场。
			name:        "same priority rotates even when bound just used",
			bound:       cand(3, 10, at(-1*time.Second)),
			candidates:  []stickyPreemptionCandidate{cand(6, 10, at(-2*time.Hour))},
			higher:      false,
			rotate:      true,
			minIdle:     rotateIdle,
			wantPreempt: true,
			wantReason:  "same_priority_rotate",
			wantBetter:  6,
		},
		{
			// 单调性：候选比绑定更近使用过，不能换回去，否则两号来回抖动。
			name:        "no rotation toward more recently used account",
			bound:       cand(6, 10, at(-2*time.Hour)),
			candidates:  []stickyPreemptionCandidate{cand(3, 10, at(-10*time.Minute))},
			higher:      true,
			rotate:      true,
			minIdle:     rotateIdle,
			wantPreempt: false,
		},
		{
			// 从未使用过的账号视为最旧，优先被轮询到。
			name:        "never used candidate wins rotation",
			bound:       cand(3, 10, at(-10*time.Minute)),
			candidates:  []stickyPreemptionCandidate{cand(6, 10, nil)},
			higher:      true,
			rotate:      true,
			minIdle:     rotateIdle,
			wantPreempt: true,
			wantReason:  "same_priority_rotate",
			wantBetter:  6,
		},
		{
			// 绑定在主号上时，更低优先级的副号绝不能抢占。
			name:        "bound primary is never preempted by secondary",
			bound:       cand(5, 0, at(-time.Hour)),
			candidates:  []stickyPreemptionCandidate{cand(3, 10, at(-2*time.Hour)), cand(6, 10, nil)},
			higher:      true,
			rotate:      true,
			minIdle:     rotateIdle,
			wantPreempt: false,
		},
		{
			// 多主号：12 与 5 同为 priority=0，轮询判定与副号走的是同一条 == 分支，
			// 不存在「只有副号才轮询」的档位特例。旧的 case 全用 priority=10，
			// 这条锁住顶层优先级档同样生效。
			name:  "multiple primaries rotate within the top tier",
			bound: cand(12, 0, at(-10*time.Minute)),
			candidates: []stickyPreemptionCandidate{
				cand(5, 0, at(-2*time.Hour)),
				cand(3, 10, at(-5*time.Hour)), // 更旧的副号仍不得被选中
			},
			higher:      true,
			rotate:      true,
			minIdle:     rotateIdle,
			wantPreempt: true,
			wantReason:  "same_priority_rotate",
			wantBetter:  5,
		},
		{
			// 主号档内的 MinIdle 防抖与副号档完全一致：对等主号只旧 60 秒就不换，
			// 且此时也不能被更旧的副号钩下去（否则就变成无故降级到副号）。
			name:  "top tier holds binding when peer primary barely older",
			bound: cand(12, 0, at(-30*time.Second)),
			candidates: []stickyPreemptionCandidate{
				cand(5, 0, at(-90*time.Second)),
				cand(3, 10, at(-5*time.Hour)),
			},
			higher:      true,
			rotate:      true,
			minIdle:     rotateIdle,
			wantPreempt: false,
		},
		{
			// 两个开关全关 => 完全保持旧行为。
			name:        "both switches disabled keeps legacy behavior",
			bound:       cand(3, 10, at(-time.Hour)),
			candidates:  []stickyPreemptionCandidate{cand(5, 0, at(-time.Hour)), cand(6, 10, nil)},
			higher:      false,
			rotate:      false,
			minIdle:     0,
			wantPreempt: false,
		},
		{
			// 只关轮询，切回主号仍生效。
			name:        "rotate disabled still reclaims primary",
			bound:       cand(3, 10, at(-time.Hour)),
			candidates:  []stickyPreemptionCandidate{cand(5, 0, at(-time.Hour)), cand(6, 10, nil)},
			higher:      true,
			rotate:      false,
			minIdle:     0,
			wantPreempt: true,
			wantReason:  "higher_priority_recovered",
			wantBetter:  5,
		},
		{
			// 只关切回，同优先级轮询仍生效。
			name:        "higher disabled still rotates same priority",
			bound:       cand(3, 10, at(-10*time.Minute)),
			candidates:  []stickyPreemptionCandidate{cand(5, 0, at(-time.Hour)), cand(6, 10, nil)},
			higher:      false,
			rotate:      true,
			minIdle:     rotateIdle,
			wantPreempt: true,
			wantReason:  "same_priority_rotate",
			wantBetter:  6,
		},
		{
			name:        "no candidates keeps binding",
			bound:       cand(3, 10, at(-time.Hour)),
			candidates:  nil,
			higher:      true,
			rotate:      true,
			minIdle:     rotateIdle,
			wantPreempt: false,
		},
		{
			// 候选池只剩自己（其它号都被过滤掉了）=> 不抢占，避免空转。
			name:        "only self in candidates keeps binding",
			bound:       cand(3, 10, at(-time.Hour)),
			candidates:  []stickyPreemptionCandidate{cand(3, 10, at(-time.Hour))},
			higher:      true,
			rotate:      true,
			minIdle:     rotateIdle,
			wantPreempt: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluateStickyPreemption(tt.bound, tt.candidates, tt.higher, tt.rotate, tt.minIdle, now)
			if got.preempt != tt.wantPreempt {
				t.Fatalf("preempt = %v, want %v (reason=%q better=%d)", got.preempt, tt.wantPreempt, got.reason, got.betterA)
			}
			if !tt.wantPreempt {
				return
			}
			if got.reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", got.reason, tt.wantReason)
			}
			if got.betterA != tt.wantBetter {
				t.Errorf("betterA = %d, want %d", got.betterA, tt.wantBetter)
			}
		})
	}
}

// 会话内反复调度时，轮询必须真的在两个同优先级副号之间交替，
// 而不是在同一个号上原地打转（线上"只请求一个号"的直接回归）。
func TestStickyRotationAlternatesBetweenSamePriorityAccounts(t *testing.T) {
	const rotateIdle = 5 * time.Minute
	now := time.Date(2026, 8, 19, 18, 0, 0, 0, time.UTC)

	lastUsed := map[int64]time.Time{
		3: now.Add(-time.Hour),
		6: now.Add(-2 * time.Hour),
	}
	bound := int64(3)

	picks := make([]int64, 0, 6)
	for i := 0; i < 6; i++ {
		// 每一轮相隔 MinIdle 以上，模拟持续但不密集的会话流量。
		now = now.Add(rotateIdle + time.Minute)

		boundLast := lastUsed[bound]
		candidates := make([]stickyPreemptionCandidate, 0, len(lastUsed))
		for id, ts := range lastUsed {
			if id == bound {
				continue
			}
			t := ts
			candidates = append(candidates, stickyPreemptionCandidate{ID: id, Priority: 10, LastUsedAt: &t})
		}

		decision := evaluateStickyPreemption(
			stickyPreemptionCandidate{ID: bound, Priority: 10, LastUsedAt: &boundLast},
			candidates, true, true, rotateIdle, now,
		)
		if decision.preempt {
			bound = decision.betterA
		}
		picks = append(picks, bound)
		lastUsed[bound] = now
	}

	seen := map[int64]int{}
	for _, id := range picks {
		seen[id]++
	}
	if len(seen) < 2 {
		t.Fatalf("rotation never switched accounts, picks=%v", picks)
	}
	for id, n := range seen {
		if n == 0 {
			t.Fatalf("account %d never used, picks=%v", id, picks)
		}
	}
	// 严格交替：每一轮都应换到另一个号。
	for i := 1; i < len(picks); i++ {
		if picks[i] == picks[i-1] {
			t.Fatalf("expected alternation, got repeat at %d: %v", i, picks)
		}
	}
}

// 需求：主号故障切到副号后，多个副号之间轮换；轮换过程中主号一恢复就要伺机切回。
//
// 关键在于两条判定的先后与门控差异：
//   - 主号恢复（更高优先级）不受 MinIdle 约束，任何一轮都能立刻切回；
//   - 同级副号轮换受 MinIdle 约束，只按轮换周期换。
// 若顺序颠倒或给切回也加上 MinIdle，主号恢复后仍要继续消耗副号额度。
func TestStickyPreemptionSwitchesBackToPrimaryDuringSecondaryRotation(t *testing.T) {
	const rotateIdle = 5 * time.Minute
	now := time.Date(2026, 8, 19, 18, 0, 0, 0, time.UTC)

	primary := int64(5) // Priority 0
	secondaries := map[int64]time.Time{
		3: now.Add(-time.Hour),
		6: now.Add(-2 * time.Hour),
		7: now.Add(-3 * time.Hour),
	}
	priority := map[int64]int{5: 0, 3: 10, 6: 10, 7: 10}
	lastUsed := map[int64]time.Time{5: now.Add(-30 * time.Minute)}
	for id, ts := range secondaries {
		lastUsed[id] = ts
	}

	bound := int64(3) // 主号故障期间落到了某个副号
	primaryHealthy := false
	picks := make([]int64, 0, 8)

	for round := 0; round < 8; round++ {
		now = now.Add(rotateIdle + time.Minute)
		// 第 5 轮主号恢复（例如 429 冷却结束、safeguards 解除）。
		if round == 5 {
			primaryHealthy = true
		}

		// 候选池模拟 gatewayStickyPreemptionCandidates：故障中的主号过不了可调度性链，
		// 因此恢复之前根本不会出现在候选里，不存在「误判切回」。
		candidates := make([]stickyPreemptionCandidate, 0, len(lastUsed))
		for id := range lastUsed {
			if id == bound {
				continue
			}
			if id == primary && !primaryHealthy {
				continue
			}
			ts := lastUsed[id]
			candidates = append(candidates, stickyPreemptionCandidate{ID: id, Priority: priority[id], LastUsedAt: &ts})
		}

		boundLast := lastUsed[bound]
		decision := evaluateStickyPreemption(
			stickyPreemptionCandidate{ID: bound, Priority: priority[bound], LastUsedAt: &boundLast},
			candidates, true, true, rotateIdle, now,
		)
		if decision.preempt {
			bound = decision.betterA
		}
		picks = append(picks, bound)
		lastUsed[bound] = now
	}

	// 恢复之前只在副号之间轮换，且确实换过号。
	before := picks[:5]
	seenBefore := map[int64]struct{}{}
	for _, id := range before {
		if id == primary {
			t.Fatalf("主号仍在故障中就被切回了，picks=%v", picks)
		}
		seenBefore[id] = struct{}{}
	}
	if len(seenBefore) < 2 {
		t.Fatalf("副号之间没有轮换，picks=%v", picks)
	}

	// 恢复之后立刻切回主号，并在其后保持在主号上（更高优先级不再被同级轮换抢走）。
	for i, id := range picks[5:] {
		if id != primary {
			t.Fatalf("主号恢复后第 %d 轮未切回主号，picks=%v", i, picks)
		}
	}
}

// 只有报错/不可用才会导致主副切换：抢占判定永远不会从高优先级降到低优先级。
func TestStickyPreemptionNeverDemotesToLowerPriority(t *testing.T) {
	now := time.Date(2026, 8, 19, 18, 0, 0, 0, time.UTC)
	boundLast := now.Add(-time.Hour)
	staleSecondary := now.Add(-10 * time.Hour) // 比主号旧得多，仍不得被选中

	decision := evaluateStickyPreemption(
		stickyPreemptionCandidate{ID: 5, Priority: 0, LastUsedAt: &boundLast},
		[]stickyPreemptionCandidate{
			{ID: 3, Priority: 10, LastUsedAt: &staleSecondary},
			{ID: 6, Priority: 20, LastUsedAt: &staleSecondary},
		},
		true, true, 5*time.Minute, now,
	)
	if decision.preempt {
		t.Fatalf("绑定在主号上时不得因低优先级副号更旧而降级切换，decision=%+v", decision)
	}
}

// 需求：有多个主号时，主号之间要和副号之间有完全一样的轮询机制。
//
// 线上背景（2026-08-20 实证）：调度看的是 accounts.priority，
// 长期只有一个 priority=0 的主号，其余都是 priority=10 的副号，
// 于是所有轮询实测都发生在副号档，主号档从未有过覆盖。
// 加入第二个 priority=0 主号后，两个主号必须像两个副号一样按 MinIdle 交替，
// 并且整个过程中都不许把请求降级给更旧的副号。
func TestStickyRotationAlternatesBetweenMultiplePrimaries(t *testing.T) {
	const rotateIdle = 5 * time.Minute
	now := time.Date(2026, 8, 20, 22, 0, 0, 0, time.UTC)

	priority := map[int64]int{5: 0, 12: 0, 3: 10, 11: 10}
	lastUsed := map[int64]time.Time{
		5:  now.Add(-time.Hour),
		12: now.Add(-2 * time.Hour),
		3:  now.Add(-10 * time.Hour), // 副号比两个主号都旧得多，仍不得被选中
		11: now.Add(-12 * time.Hour),
	}
	bound := int64(5)

	picks := make([]int64, 0, 6)
	for i := 0; i < 6; i++ {
		now = now.Add(rotateIdle + time.Minute)

		candidates := make([]stickyPreemptionCandidate, 0, len(lastUsed))
		for id := range lastUsed {
			if id == bound {
				continue
			}
			ts := lastUsed[id]
			candidates = append(candidates, stickyPreemptionCandidate{ID: id, Priority: priority[id], LastUsedAt: &ts})
		}

		boundLast := lastUsed[bound]
		decision := evaluateStickyPreemption(
			stickyPreemptionCandidate{ID: bound, Priority: priority[bound], LastUsedAt: &boundLast},
			candidates, true, true, rotateIdle, now,
		)
		if decision.preempt {
			if decision.reason != "same_priority_rotate" {
				t.Fatalf("主号档轮换的原因应为 same_priority_rotate，实际 %q（picks=%v）", decision.reason, picks)
			}
			bound = decision.betterA
		}
		picks = append(picks, bound)
		lastUsed[bound] = now
	}

	for i, id := range picks {
		if priority[id] != 0 {
			t.Fatalf("第 %d 轮把请求降级给了副号 %d，picks=%v", i, id, picks)
		}
	}
	if picks[0] == picks[1] {
		t.Fatalf("两个主号之间没有发生轮换，picks=%v", picks)
	}
	for i := 1; i < len(picks); i++ {
		if picks[i] == picks[i-1] {
			t.Fatalf("主号档应严格交替，第 %d 轮重复，picks=%v", i, picks)
		}
	}
}

// 三个以上主号时，轮询必须覆盖到每一个，不能在其中两个之间打转。
// LRU 单调方向保证每轮都换到「最久没用」的那个，等价于顶层档位的轮转。
func TestStickyRotationCoversEveryPrimaryInTopTier(t *testing.T) {
	const rotateIdle = 5 * time.Minute
	now := time.Date(2026, 8, 20, 22, 0, 0, 0, time.UTC)

	primaries := []int64{5, 12, 13}
	lastUsed := map[int64]time.Time{
		5:  now.Add(-time.Hour),
		12: now.Add(-2 * time.Hour),
		13: now.Add(-3 * time.Hour),
	}
	bound := int64(5)

	seen := map[int64]int{}
	for i := 0; i < 9; i++ {
		now = now.Add(rotateIdle + time.Minute)

		candidates := make([]stickyPreemptionCandidate, 0, len(primaries))
		for _, id := range primaries {
			if id == bound {
				continue
			}
			ts := lastUsed[id]
			candidates = append(candidates, stickyPreemptionCandidate{ID: id, Priority: 0, LastUsedAt: &ts})
		}

		boundLast := lastUsed[bound]
		decision := evaluateStickyPreemption(
			stickyPreemptionCandidate{ID: bound, Priority: 0, LastUsedAt: &boundLast},
			candidates, true, true, rotateIdle, now,
		)
		if decision.preempt {
			bound = decision.betterA
		}
		seen[bound]++
		lastUsed[bound] = now
	}

	if len(seen) != len(primaries) {
		t.Fatalf("三个主号应全部参与轮询，实际分布 %v", seen)
	}
	for _, id := range primaries {
		if seen[id] < 2 {
			t.Fatalf("主号 %d 只被选中 %d 次，轮询不均匀：%v", id, seen[id], seen)
		}
	}
}
