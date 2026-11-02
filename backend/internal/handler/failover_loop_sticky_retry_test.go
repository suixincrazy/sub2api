package handler

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/stretchr/testify/require"
)

func newStickyTestState(stickyID int64) *FailoverState {
	fs := NewFailoverState(3, true)
	fs.SetStickyAccount(stickyID)
	fs.stickyRetryBaseWait = time.Millisecond
	return fs
}

func TestStickyAccountRetry_Bound503RetriesInPlaceThenSwitches(t *testing.T) {
	mock := &mockTempUnscheduler{}
	fs := newStickyTestState(11)
	err := newTestFailoverErr(503, false, false)

	for i := 1; i <= stickyAccountRetryMax; i++ {
		action := fs.HandleFailoverError(context.Background(), mock, 11, service.PlatformAnthropic, 0, err)
		require.Equal(t, FailoverContinue, action)
		require.Equal(t, i, fs.stickyRetryCount)
		require.NotContains(t, fs.FailedAccountIDs, int64(11), "原地重试期间绑定号不得进失败列表")
		require.Equal(t, 0, fs.SwitchCount)
		require.False(t, fs.ForceCacheBilling, "未换号前不应强制缓存计费")
	}

	action := fs.HandleFailoverError(context.Background(), mock, 11, service.PlatformAnthropic, 0, err)
	require.Equal(t, FailoverContinue, action)
	require.Contains(t, fs.FailedAccountIDs, int64(11), "预算用完后按原逻辑换号")
	require.Equal(t, 1, fs.SwitchCount)
	require.True(t, fs.ForceCacheBilling)
	require.Empty(t, mock.calls)
}

func TestStickyAccountRetry_BudgetExpiredSwitches(t *testing.T) {
	fs := newStickyTestState(11)
	err := newTestFailoverErr(503, false, false)

	require.Equal(t, FailoverContinue, fs.HandleFailoverError(context.Background(), &mockTempUnscheduler{}, 11, service.PlatformAnthropic, 0, err))
	require.Equal(t, 1, fs.stickyRetryCount)

	fs.stickyRetryStarted = time.Now().Add(-stickyAccountRetryBudget - time.Second)
	fs.HandleFailoverError(context.Background(), &mockTempUnscheduler{}, 11, service.PlatformAnthropic, 0, err)
	require.Equal(t, 1, fs.stickyRetryCount, "超出时间预算后不再原地重试")
	require.Contains(t, fs.FailedAccountIDs, int64(11))
	require.Equal(t, 1, fs.SwitchCount)
}

func TestStickyAccountRetry_NotAppliedOutsideScope(t *testing.T) {
	cases := []struct {
		name      string
		stickyID  int64
		accountID int64
		err       *service.UpstreamFailoverError
	}{
		{"无绑定", 0, 11, newTestFailoverErr(503, false, false)},
		{"非绑定号", 11, 15, newTestFailoverErr(503, false, false)},
		{"429交给服务层重发", 11, 11, newTestFailoverErr(429, false, false)},
		{"4xx确定性错误", 11, 11, newTestFailoverErr(403, false, false)},
		{"凭证失败", 11, 11, &service.UpstreamFailoverError{StatusCode: 503, Stage: service.GatewayFailureStageAccountAuth}},
		{"缓存上游透传", 11, 11, &service.UpstreamFailoverError{StatusCode: 503, BoundUpstreamPassthrough: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newStickyTestState(tc.stickyID)
			action := fs.HandleFailoverError(context.Background(), &mockTempUnscheduler{}, tc.accountID, service.PlatformAnthropic, 0, tc.err)
			require.Equal(t, FailoverContinue, action)
			require.Equal(t, 0, fs.stickyRetryCount)
			require.Contains(t, fs.FailedAccountIDs, tc.accountID)
			require.Equal(t, 1, fs.SwitchCount)
		})
	}
}

func TestStickyAccountRetry_PoolModeRetryTakesPrecedence(t *testing.T) {
	fs := newStickyTestState(11)
	err := newTestFailoverErr(503, true, false)

	fs.HandleFailoverError(context.Background(), &mockTempUnscheduler{}, 11, service.PlatformAnthropic, 1, err)
	require.Equal(t, 1, fs.SameAccountRetryCount[11])
	require.Equal(t, 0, fs.stickyRetryCount, "池模式重试优先，粘性预算不消耗")

	fs.HandleFailoverError(context.Background(), &mockTempUnscheduler{}, 11, service.PlatformAnthropic, 1, err)
	require.Equal(t, 1, fs.stickyRetryCount, "池模式预算用完后接粘性原地重试")
	require.NotContains(t, fs.FailedAccountIDs, int64(11))
}

func TestStickyAccountRetry_CanceledDuringWait(t *testing.T) {
	fs := NewFailoverState(3, true)
	fs.SetStickyAccount(11)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	action := fs.HandleFailoverError(ctx, &mockTempUnscheduler{}, 11, service.PlatformAnthropic, 0, newTestFailoverErr(503, false, false))
	require.Equal(t, FailoverCanceled, action)
}

func TestStickyRetryDelaySchedule(t *testing.T) {
	fs := NewFailoverState(3, true)
	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, w := range want {
		require.Equal(t, w, fs.stickyRetryDelay(i+1), "第 %d 次", i+1)
	}
}
