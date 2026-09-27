//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestScheduledKeepaliveAutoRecoverHonorsManualSchedulingPause(t *testing.T) {
	for _, pauseAt := range []string{"before_first_probe", "during_first_probe", "between_keepalives", "during_keepalive"} {
		t.Run(pauseAt, func(t *testing.T) {
			runner, plans, results := newKeepaliveTestRunner(t, 1)
			plans.plans[1].AutoRecover = true
			account := &Account{ID: 1, Status: StatusActive, Schedulable: true}
			repo := &rateLimitClearRepoStub{getByIDAccount: account, keepalivePlans: plans}
			runner.rateLimitSvc = &RateLimitService{accountRepo: repo}
			admin := &adminServiceImpl{accountRepo: repo}
			pause := func() {
				_, err := admin.SetAccountSchedulable(context.Background(), account.ID, false)
				require.NoError(t, err)
			}
			probes := 0
			runner.runTest = func(context.Context, int64, string) (*ScheduledTestResult, error) {
				probes++
				if (pauseAt == "during_first_probe" && probes == 1) || (pauseAt == "during_keepalive" && probes == 2) {
					pause()
				}
				return &ScheduledTestResult{Status: "success"}, nil
			}
			if pauseAt == "before_first_probe" {
				pause()
			}
			runner.runOnePlan(context.Background(), plans.plans[1])
			if pauseAt == "between_keepalives" {
				pause()
			}
			for i := 0; i < 3; i++ {
				plans.due(1)
				runner.runOnePlan(context.Background(), plans.plans[1])
				require.False(t, account.Schedulable, "successful keepalives must preserve the administrator's scheduling pause")
			}
			require.GreaterOrEqual(t, results.count(), 3, "manual scheduling pause must not stop subsequent keepalive probes")
			require.True(t, plans.plans[1].Enabled)
			require.False(t, plans.plans[1].AutoRecover)
			require.Equal(t, []bool{false}, repo.setSchedulableCalls)
		})
	}
}

func TestScheduledKeepaliveAutoRecoverHonorsAccountPause(t *testing.T) {
	expired := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name    string
		account Account
	}{
		{name: "disabled", account: Account{Status: StatusDisabled}},
		{name: "expired_active", account: Account{Status: StatusActive, ExpiresAt: &expired, AutoPauseOnExpired: true}},
		{name: "expired_error", account: Account{Status: StatusError, ExpiresAt: &expired, AutoPauseOnExpired: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, plans, _ := newKeepaliveTestRunner(t, 1)
			plans.plans[1].AutoRecover = true
			account := tc.account
			account.ID = 1
			repo := &rateLimitClearRepoStub{getByIDAccount: &account}
			runner.rateLimitSvc = &RateLimitService{accountRepo: repo}
			runner.runTest = func(context.Context, int64, string) (*ScheduledTestResult, error) {
				return &ScheduledTestResult{Status: "success"}, nil
			}

			runner.runOnePlan(context.Background(), plans.plans[1])

			require.False(t, account.Schedulable)
			require.Empty(t, repo.setSchedulableCalls)
			require.Zero(t, repo.clearErrorCalls, "expired or disabled accounts must not be reactivated")
		})
	}
}

func TestScheduledKeepaliveRecoveryClearsCachedRuntimeBlocks(t *testing.T) {
	runner, plans, _ := newKeepaliveTestRunner(t, 1)
	plans.plans[1].AutoRecover = true
	until := time.Now().Add(time.Hour)
	account := &Account{ID: 1, Status: StatusError, TempUnschedulableUntil: &until}
	repo := &rateLimitClearRepoStub{getByIDAccount: account, keepalivePlans: plans}
	cache := &tempUnschedCacheRecorder{}
	blocker := &runtimeBlockRecorder{}
	runner.rateLimitSvc = &RateLimitService{accountRepo: repo, tempUnschedCache: cache, runtimeBlocker: blocker}
	runner.runTest = func(context.Context, int64, string) (*ScheduledTestResult, error) {
		return &ScheduledTestResult{Status: "success"}, nil
	}

	runner.runOnePlan(context.Background(), plans.plans[1])

	require.True(t, account.Schedulable)
	require.Equal(t, []int64{1}, cache.deletedIDs)
	require.Equal(t, []int64{1}, blocker.clearedIDs)
}
