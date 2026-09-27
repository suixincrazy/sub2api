//go:build integration

package repository

import (
	"context"
	"database/sql"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newKeepaliveRecoveryFixture(t *testing.T) (*accountRepository, service.ScheduledTestPlanRepository, *service.ScheduledTestPlan) {
	t.Helper()
	ctx := context.Background()
	repo := newAccountRepositoryWithSQL(integrationEntClient, integrationDB, nil)
	account := mustCreateAccount(t, integrationEntClient, &service.Account{Name: t.Name()})
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id = $1", account.ID)
		require.NoError(t, err)
	})
	// A schedule that was already off when recovery was explicitly enabled may resume.
	require.NoError(t, repo.SetSchedulable(ctx, account.ID, false))
	plans := NewScheduledTestPlanRepository(integrationDB)
	plan, err := service.NewScheduledTestService(plans, nil).CreatePlan(ctx, &service.ScheduledTestPlan{
		AccountID: account.ID, ModelID: "gpt-6-astra", Enabled: true, AutoRecover: true,
	})
	require.NoError(t, err)
	return repo, plans, plan
}

func TestKeepaliveRecoveryAccountEligibility(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     string
		expiration time.Duration
		autoPause  bool
		allowed    bool
	}{
		{name: "active", status: service.StatusActive, allowed: true},
		{name: "error", status: service.StatusError, allowed: true},
		{name: "disabled", status: service.StatusDisabled},
		{name: "expired_active", status: service.StatusActive, expiration: -time.Hour, autoPause: true},
		{name: "expired_error", status: service.StatusError, expiration: -time.Hour, autoPause: true},
		{name: "expiry_ignored", status: service.StatusActive, expiration: -time.Hour, allowed: true},
		{name: "future_expiry", status: service.StatusActive, expiration: time.Hour, autoPause: true, allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, _, plan := newKeepaliveRecoveryFixture(t)
			ctx := context.Background()
			var expiresAt *time.Time
			if tc.expiration != 0 {
				value := time.Now().Add(tc.expiration)
				expiresAt = &value
			}
			_, err := integrationDB.ExecContext(ctx, `UPDATE accounts SET status = $2,
				expires_at = $3, auto_pause_on_expired = $4, error_message = 'original error',
				rate_limited_at = NOW(), rate_limit_reset_at = NOW() + interval '1 hour',
				overload_until = NOW() + interval '1 hour', temp_unschedulable_until = NOW() + interval '1 hour',
				temp_unschedulable_reason = 'original reason',
				extra = '{"model_rate_limits":{"model":true},"antigravity_quota_scopes":{"scope":true},"unrelated":"keep"}'
				WHERE id = $1`, plan.AccountID, tc.status, expiresAt, tc.autoPause)
			require.NoError(t, err)
			result, err := repo.RecoverAfterKeepalive(ctx, plan)
			require.NoError(t, err)
			require.Equal(t, tc.allowed, result.ResumedScheduling)
			require.Equal(t, tc.allowed, result.ClearedRateLimit)
			require.Equal(t, tc.allowed && tc.status == service.StatusError, result.ClearedError)
			account, err := repo.GetByID(ctx, plan.AccountID)
			require.NoError(t, err)
			require.Equal(t, tc.allowed, account.Schedulable)
			require.Equal(t, "keep", account.Extra["unrelated"])
			if tc.allowed {
				require.Equal(t, service.StatusActive, account.Status)
				require.Nil(t, account.RateLimitResetAt)
				require.Nil(t, account.TempUnschedulableUntil)
				require.NotContains(t, account.Extra, "model_rate_limits")
				again, err := repo.RecoverAfterKeepalive(ctx, plan)
				require.NoError(t, err)
				require.Equal(t, &service.SuccessfulTestRecoveryResult{}, again)
			} else {
				require.Equal(t, tc.status, account.Status)
				require.Equal(t, "original error", account.ErrorMessage)
				require.NotNil(t, account.RateLimitResetAt)
			}
		})
	}
}

func TestKeepaliveRecoveryManualStopAndExplicitRearm(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		name := "single"
		if bulk {
			name = "bulk"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			repo, plans, plan := newKeepaliveRecoveryFixture(t)
			second := *plan
			second.ID = 0
			second.Enabled = false
			secondPlan, err := plans.Create(ctx, &second)
			require.NoError(t, err)
			_, _, otherPlan := newKeepaliveRecoveryFixture(t)
			result, err := repo.RecoverAfterKeepalive(ctx, plan)
			require.NoError(t, err)
			require.True(t, result.ResumedScheduling)
			if bulk {
				off := false
				count, err := repo.BulkUpdate(ctx, []int64{plan.AccountID}, service.AccountBulkUpdate{Schedulable: &off})
				require.NoError(t, err)
				require.EqualValues(t, 1, count)
			} else {
				require.NoError(t, repo.SetSchedulable(ctx, plan.AccountID, false))
			}
			for _, stale := range []*service.ScheduledTestPlan{plan, secondPlan} {
				current, err := plans.GetByID(ctx, stale.ID)
				require.NoError(t, err)
				require.False(t, current.AutoRecover)
				require.Equal(t, stale.Enabled, current.Enabled)
				require.False(t, stale.UpdatedAt.Equal(current.UpdatedAt))
				result, err := repo.RecoverAfterKeepalive(ctx, stale)
				require.NoError(t, err)
				require.False(t, result.ResumedScheduling)
				_, err = plans.Update(ctx, stale)
				require.ErrorIs(t, err, sql.ErrNoRows, "a stale plan edit must not restore auto recovery")
			}
			other, err := plans.GetByID(ctx, otherPlan.ID)
			require.NoError(t, err)
			require.True(t, other.AutoRecover)
			// A new repository instance must observe the durable manual stop.
			restarted := newAccountRepositoryWithSQL(integrationEntClient, integrationDB, nil)
			current, err := plans.GetByID(ctx, plan.ID)
			require.NoError(t, err)
			result, err = restarted.RecoverAfterKeepalive(ctx, current)
			require.NoError(t, err)
			require.False(t, result.ResumedScheduling)
			current.AutoRecover = true
			current, err = plans.Update(ctx, current)
			require.NoError(t, err)
			result, err = restarted.RecoverAfterKeepalive(ctx, current)
			require.NoError(t, err)
			require.True(t, result.ResumedScheduling)
		})
	}
}

func TestKeepaliveRecoveryRechecksConcurrentAccountChanges(t *testing.T) {
	for _, change := range []string{"manual_stop", "disable", "expire"} {
		t.Run(change, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			repo, _, plan := newKeepaliveRecoveryFixture(t)
			tx, err := integrationEntClient.Tx(ctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			switch change {
			case "manual_stop":
				writer := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
				require.NoError(t, writer.SetSchedulable(dbent.NewTxContext(ctx, tx), plan.AccountID, false))
			case "disable":
				_, err = tx.ExecContext(ctx, "UPDATE accounts SET status = 'disabled' WHERE id = $1", plan.AccountID)
			case "expire":
				_, err = tx.ExecContext(ctx, "UPDATE accounts SET auto_pause_on_expired = true, expires_at = NOW() - interval '1 hour' WHERE id = $1", plan.AccountID)
			}
			require.NoError(t, err)
			type outcome struct {
				result *service.SuccessfulTestRecoveryResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := repo.RecoverAfterKeepalive(ctx, plan)
				done <- outcome{result, err}
			}()
			require.Eventually(t, func() bool {
				var waiting bool
				err := integrationDB.QueryRowContext(ctx, `SELECT EXISTS (
					SELECT 1 FROM pg_stat_activity WHERE datname = current_database()
					AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'
					AND query LIKE '%WITH candidate AS MATERIALIZED%')`).Scan(&waiting)
				return err == nil && waiting
			}, 3*time.Second, 10*time.Millisecond, "recovery must wait for the account writer")
			require.NoError(t, tx.Commit())
			select {
			case outcome := <-done:
				require.NoError(t, outcome.err)
				require.Equal(t, &service.SuccessfulTestRecoveryResult{}, outcome.result)
			case <-ctx.Done():
				t.Fatal("recovery did not finish after the account write committed")
			}
			account, err := repo.GetByID(ctx, plan.AccountID)
			require.NoError(t, err)
			require.False(t, account.Schedulable)
		})
	}
}
