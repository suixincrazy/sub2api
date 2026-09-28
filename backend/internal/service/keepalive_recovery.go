package service

import (
	"context"
	"fmt"
	"log/slog"
)

func (s *RateLimitService) PauseAccountAfterKeepaliveFailure(ctx context.Context, plan *ScheduledTestPlan) (bool, error) {
	repo, ok := s.accountRepo.(AccountKeepaliveRecoveryRepository)
	if !ok {
		return false, fmt.Errorf("account repository does not support keepalive recovery")
	}
	return repo.PauseAfterKeepaliveFailure(ctx, plan)
}

// RecoverAccountAfterKeepalive never uses the manual recovery path: a completed
// request must not overwrite a newer scheduling pause or plan edit.
func (s *RateLimitService) RecoverAccountAfterKeepalive(ctx context.Context, plan *ScheduledTestPlan) (*SuccessfulTestRecoveryResult, error) {
	repo, ok := s.accountRepo.(AccountKeepaliveRecoveryRepository)
	if !ok {
		return nil, fmt.Errorf("account repository does not support keepalive recovery")
	}
	result, err := repo.RecoverAfterKeepalive(ctx, plan)
	if err != nil || result == nil {
		return result, err
	}
	if result.ClearedRateLimit && s.tempUnschedCache != nil {
		if err := s.tempUnschedCache.DeleteTempUnsched(ctx, plan.AccountID); err != nil {
			slog.Warn("temp_unsched_cache_delete_failed", "account_id", plan.AccountID, "error", err)
		}
	}
	if result.ClearedError || result.ClearedRateLimit || result.ResumedScheduling {
		s.ResetOpenAI403Counter(ctx, plan.AccountID)
		s.notifyAccountSchedulingBlockCleared(plan.AccountID)
	}
	return result, nil
}
