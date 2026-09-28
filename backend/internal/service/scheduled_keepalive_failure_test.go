//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func (r *rateLimitClearRepoStub) PauseAfterKeepaliveFailure(ctx context.Context, plan *ScheduledTestPlan) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if r.keepalivePlans != nil {
		current, err := r.keepalivePlans.GetByID(ctx, plan.ID)
		if err != nil || !current.Enabled || !current.AutoRecover || !current.UpdatedAt.Equal(plan.UpdatedAt) {
			return false, nil
		}
	}
	if !r.getByIDAccount.Schedulable {
		return false, nil
	}
	r.getByIDAccount.Schedulable = false
	r.setSchedulableCalls = append(r.setSchedulableCalls, false)
	return true, nil
}

func TestScheduledKeepaliveFailurePausesUntilSuccessfulProbe(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed_response", true: "request_timeout"}[timeout], func(t *testing.T) {
			runner, plans, _ := newKeepaliveTestRunner(t, 1)
			plans.plans[1].AutoRecover = true
			account := &Account{ID: 1, Status: StatusActive, Schedulable: true}
			repo := &rateLimitClearRepoStub{getByIDAccount: account, keepalivePlans: plans}
			runner.rateLimitSvc = &RateLimitService{accountRepo: repo}
			for _, success := range []bool{false, false, true, true} {
				plans.due(1)
				runner.runTest = func(context.Context, int64, string) (*ScheduledTestResult, error) {
					if success {
						return &ScheduledTestResult{Status: "success"}, nil
					}
					if timeout {
						return nil, context.DeadlineExceeded
					}
					return &ScheduledTestResult{Status: "failed", HTTPStatus: 503}, nil
				}
				runner.runOnePlan(context.Background(), plans.plans[1])
				require.Equal(t, success, account.Schedulable)
				require.True(t, plans.plans[1].AutoRecover, "automatic pause must retain recovery opt-in")
				if !success {
					require.WithinDuration(t, time.Now().Add(2*time.Second), *plans.plans[1].NextRunAt, time.Second)
				}
			}
			require.Equal(t, []bool{false, true}, repo.setSchedulableCalls)
		})
	}
}

func TestScheduledKeepaliveFailureDoesNotPauseOptOutOrCancelledRun(t *testing.T) {
	for _, action := range []string{"opt_out", "cancel", "edit"} {
		t.Run(action, func(t *testing.T) {
			runner, plans, _ := newKeepaliveTestRunner(t, 1)
			plans.plans[1].AutoRecover = action != "opt_out"
			account := &Account{ID: 1, Status: StatusActive, Schedulable: true}
			repo := &rateLimitClearRepoStub{getByIDAccount: account, keepalivePlans: plans}
			runner.rateLimitSvc = &RateLimitService{accountRepo: repo}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runner.runTest = func(context.Context, int64, string) (*ScheduledTestResult, error) {
				if action == "cancel" {
					cancel()
				}
				if action == "edit" {
					plans.plans[1].UpdatedAt = plans.plans[1].UpdatedAt.Add(time.Second)
				}
				return &ScheduledTestResult{Status: "failed"}, nil
			}
			runner.runOnePlan(ctx, plans.plans[1])
			require.True(t, account.Schedulable)
			require.Empty(t, repo.setSchedulableCalls)
		})
	}
}
