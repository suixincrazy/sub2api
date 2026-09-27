package repository

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.AccountKeepaliveRecoveryRepository = (*accountRepository)(nil)

func (r *accountRepository) RecoverAfterKeepalive(ctx context.Context, plan *service.ScheduledTestPlan) (*service.SuccessfulTestRecoveryResult, error) {
	// Lock the account before the plan, matching manual scheduling changes.
	// The locking reads recheck current values after a concurrent writer commits.
	rows, err := r.sql.QueryContext(ctx, `
		WITH candidate AS MATERIALIZED (
			SELECT id, status = 'error' AS cleared_error,
				NOT schedulable AS resumed_scheduling,
				(rate_limited_at IS NOT NULL OR rate_limit_reset_at IS NOT NULL
				 OR overload_until IS NOT NULL OR temp_unschedulable_until IS NOT NULL
				 OR COALESCE(extra -> 'model_rate_limits' NOT IN ('null'::jsonb, '{}'::jsonb, '[]'::jsonb), false)
				 OR COALESCE(extra -> 'antigravity_quota_scopes' NOT IN ('null'::jsonb, '{}'::jsonb, '[]'::jsonb), false)
				) AS cleared_rate_limit
			FROM accounts
			WHERE id = $1 AND deleted_at IS NULL AND status IN ('active', 'error')
				AND NOT (auto_pause_on_expired AND expires_at IS NOT NULL AND expires_at <= clock_timestamp())
			FOR UPDATE
		), recovery AS MATERIALIZED (
			SELECT candidate.* FROM candidate
			JOIN scheduled_test_plans p ON p.account_id = candidate.id
			WHERE p.id = $2 AND p.enabled AND p.auto_recover AND p.updated_at = $3
			FOR UPDATE OF p
		)
		UPDATE accounts a
		SET schedulable = true, status = 'active',
			error_message = CASE WHEN recovery.cleared_error THEN '' ELSE a.error_message END,
			rate_limited_at = NULL, rate_limit_reset_at = NULL, overload_until = NULL,
			temp_unschedulable_until = NULL,
			temp_unschedulable_reason = CASE WHEN recovery.cleared_rate_limit THEN NULL ELSE a.temp_unschedulable_reason END,
			extra = CASE WHEN recovery.cleared_rate_limit
				THEN COALESCE(a.extra, '{}'::jsonb) - 'model_rate_limits' - 'antigravity_quota_scopes'
				ELSE a.extra END,
			updated_at = NOW()
		FROM recovery
		WHERE a.id = recovery.id
			AND NOT (a.auto_pause_on_expired AND a.expires_at IS NOT NULL AND a.expires_at <= clock_timestamp())
			AND (recovery.cleared_error OR recovery.cleared_rate_limit OR recovery.resumed_scheduling)
		RETURNING recovery.cleared_error, recovery.cleared_rate_limit, recovery.resumed_scheduling
	`, plan.AccountID, plan.ID, plan.UpdatedAt)
	if err != nil {
		return nil, err
	}
	result := &service.SuccessfulTestRecoveryResult{}
	if rows.Next() {
		if err := rows.Scan(&result.ClearedError, &result.ClearedRateLimit, &result.ResumedScheduling); err != nil {
			_ = rows.Close()
			return nil, err
		}
	}
	readErr := rows.Err()
	closeErr := rows.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if result.ClearedError || result.ClearedRateLimit || result.ResumedScheduling {
		if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &plan.AccountID, nil, nil); err != nil {
			logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue keepalive recovery failed: account=%d err=%v", plan.AccountID, err)
		}
		r.syncSchedulerAccountSnapshot(ctx, plan.AccountID)
	}
	return result, nil
}
