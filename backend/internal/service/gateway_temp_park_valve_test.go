package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// tempParkValveRepo 只实现阀门用到的两个方法，其余走内嵌的 AccountRepository
// （nil 内嵌：被测路径之外的调用会 panic，正好暴露"阀门碰了不该碰的东西"）。
type tempParkValveRepo struct {
	AccountRepository

	parked      []Account
	listErr     error
	clearedIDs  []int64
	clearErr    error
	listCallCnt int
}

func (r *tempParkValveRepo) ListTempParkedByGroupIDAndPlatforms(_ context.Context, _ int64, _ []string) ([]Account, error) {
	r.listCallCnt++
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.parked, nil
}

func (r *tempParkValveRepo) ClearTempUnschedulable(_ context.Context, id int64) error {
	if r.clearErr != nil {
		return r.clearErr
	}
	r.clearedIDs = append(r.clearedIDs, id)
	return nil
}

func parkedAccount(id int64, until time.Time) Account {
	u := until
	return Account{
		ID:                     id,
		Platform:               PlatformAnthropic,
		Status:                 StatusActive,
		Schedulable:            true,
		TempUnschedulableUntil: &u,
	}
}

func valveService(repo AccountRepository, enabled bool) *GatewayService {
	cfg := &config.Config{}
	cfg.Gateway.TempParkReleaseValveEnabled = enabled
	return &GatewayService{accountRepo: repo, cfg: cfg}
}

func valveGroupID(id int64) *int64 { return &id }

func TestTempParkValve_PicksEarliestExpiringPark(t *testing.T) {
	now := time.Now()
	repo := &tempParkValveRepo{parked: []Account{
		parkedAccount(9, now.Add(50*time.Second)),
		parkedAccount(10, now.Add(5*time.Second)), // 最早到点，应当被选中
		parkedAccount(11, now.Add(30*time.Second)),
	}}
	s := valveService(repo, true)

	id, outcome := s.releaseEarliestTempParkedAccount(context.Background(), valveGroupID(8), PlatformAnthropic, true, "", nil)

	require.Equal(t, tempParkValveReleased, outcome)
	require.Equal(t, int64(10), id)
	require.Equal(t, []int64{10}, repo.clearedIDs, "只放行一个号，不能批量解除")
}

func TestTempParkValve_SkipsAccountsAlreadyFailedThisRequest(t *testing.T) {
	now := time.Now()
	repo := &tempParkValveRepo{parked: []Account{
		parkedAccount(10, now.Add(5*time.Second)),
		parkedAccount(11, now.Add(30*time.Second)),
	}}
	s := valveService(repo, true)

	// 10 号本次请求已经失败过：放它回来只会原地再失败一次。
	excluded := map[int64]struct{}{10: {}}
	id, outcome := s.releaseEarliestTempParkedAccount(context.Background(), valveGroupID(8), PlatformAnthropic, true, "", excluded)

	require.Equal(t, tempParkValveReleased, outcome)
	require.Equal(t, int64(11), id)
	require.Equal(t, []int64{11}, repo.clearedIDs)
}

func TestTempParkValve_DisabledByConfig(t *testing.T) {
	repo := &tempParkValveRepo{parked: []Account{parkedAccount(10, time.Now().Add(time.Second))}}
	s := valveService(repo, false)

	id, outcome := s.releaseEarliestTempParkedAccount(context.Background(), valveGroupID(8), PlatformAnthropic, true, "", nil)

	require.Equal(t, tempParkValveDisabled, outcome)
	require.Zero(t, id)
	require.Zero(t, repo.listCallCnt, "关闭时不应该发查询")
	require.Empty(t, repo.clearedIDs)
}

func TestTempParkValve_NoParkedCandidates(t *testing.T) {
	repo := &tempParkValveRepo{parked: nil}
	s := valveService(repo, true)

	id, outcome := s.releaseEarliestTempParkedAccount(context.Background(), valveGroupID(8), PlatformAnthropic, true, "", nil)

	require.Equal(t, tempParkValveNoCandidate, outcome)
	require.Zero(t, id)
	require.Empty(t, repo.clearedIDs)
}

func TestTempParkValve_AllCandidatesExcluded(t *testing.T) {
	now := time.Now()
	repo := &tempParkValveRepo{parked: []Account{
		parkedAccount(10, now.Add(5*time.Second)),
		parkedAccount(11, now.Add(30*time.Second)),
	}}
	s := valveService(repo, true)

	excluded := map[int64]struct{}{10: {}, 11: {}}
	id, outcome := s.releaseEarliestTempParkedAccount(context.Background(), valveGroupID(8), PlatformAnthropic, true, "", excluded)

	require.Equal(t, tempParkValveNoCandidate, outcome)
	require.Zero(t, id)
	require.Empty(t, repo.clearedIDs, "全部试过就不该再放行，避免同一发原地打转")
}

func TestTempParkValve_UngroupedRequestIsNotEligible(t *testing.T) {
	repo := &tempParkValveRepo{parked: []Account{parkedAccount(10, time.Now().Add(time.Second))}}
	s := valveService(repo, true)

	id, outcome := s.releaseEarliestTempParkedAccount(context.Background(), nil, PlatformAnthropic, true, "", nil)

	require.Equal(t, tempParkValveNotEligible, outcome)
	require.Zero(t, id)
	require.Zero(t, repo.listCallCnt)
}

func TestTempParkValve_QueryFailureDoesNotRelease(t *testing.T) {
	repo := &tempParkValveRepo{listErr: errors.New("db down")}
	s := valveService(repo, true)

	id, outcome := s.releaseEarliestTempParkedAccount(context.Background(), valveGroupID(8), PlatformAnthropic, true, "", nil)

	require.Equal(t, tempParkValveNoCandidate, outcome)
	require.Zero(t, id)
	require.Empty(t, repo.clearedIDs)
}

func TestTempParkValve_ClearFailureReportsNoRelease(t *testing.T) {
	repo := &tempParkValveRepo{
		parked:   []Account{parkedAccount(10, time.Now().Add(5*time.Second))},
		clearErr: errors.New("write failed"),
	}
	s := valveService(repo, true)

	id, outcome := s.releaseEarliestTempParkedAccount(context.Background(), valveGroupID(8), PlatformAnthropic, true, "", nil)

	require.Equal(t, tempParkValveClearFailed, outcome)
	require.Zero(t, id, "park 没清掉就必须报告未放行，否则调用方会白重选一次")
}

func TestTempParkValve_SkipsAntigravityWithoutMixedScheduling(t *testing.T) {
	now := time.Now()
	ag := parkedAccount(20, now.Add(2*time.Second))
	ag.Platform = PlatformAntigravity
	native := parkedAccount(10, now.Add(40*time.Second))
	repo := &tempParkValveRepo{parked: []Account{ag, native}}
	s := valveService(repo, true)

	// Antigravity 账号没开混合调度，即使 park 到点更早也不能被 Anthropic 请求选中。
	id, outcome := s.releaseEarliestTempParkedAccount(context.Background(), valveGroupID(8), PlatformAnthropic, true, "", nil)

	require.Equal(t, tempParkValveReleased, outcome)
	require.Equal(t, int64(10), id)
}

func TestTempParkValve_NilTempUntilIsSkipped(t *testing.T) {
	// 读到过期快照时可能出现 park 字段为空的行，必须跳过而不是当成"最早到点"。
	stale := parkedAccount(10, time.Now().Add(5*time.Second))
	stale.TempUnschedulableUntil = nil
	repo := &tempParkValveRepo{parked: []Account{stale}}
	s := valveService(repo, true)

	id, outcome := s.releaseEarliestTempParkedAccount(context.Background(), valveGroupID(8), PlatformAnthropic, true, "", nil)

	require.Equal(t, tempParkValveNoCandidate, outcome)
	require.Zero(t, id)
}

func TestIsNoAvailableAccountsErr(t *testing.T) {
	require.False(t, isNoAvailableAccountsErr(nil))
	require.False(t, isNoAvailableAccountsErr(errors.New("boom")))
	require.True(t, isNoAvailableAccountsErr(ErrNoAvailableAccounts))
	// 多个选号返回点会包一层原因说明，阀门必须照样识别。
	require.True(t, isNoAvailableAccountsErr(
		fmt.Errorf("%w supporting model: %s (channel pricing restriction)", ErrNoAvailableAccounts, "claude-opus-5")))
}

func TestAccountIsMixedSchedulableForGateway(t *testing.T) {
	var nilAcc *Account
	require.False(t, nilAcc.IsMixedSchedulableForGateway(true))

	native := &Account{Platform: PlatformAnthropic}
	require.True(t, native.IsMixedSchedulableForGateway(true))
	require.True(t, native.IsMixedSchedulableForGateway(false), "原生平台账号与混合调度开关无关")

	ag := &Account{Platform: PlatformAntigravity}
	require.False(t, ag.IsMixedSchedulableForGateway(true), "未开混合调度的 antigravity 账号不参与")
	require.False(t, ag.IsMixedSchedulableForGateway(false))
}
