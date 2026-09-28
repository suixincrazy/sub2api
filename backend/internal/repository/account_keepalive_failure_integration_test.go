//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestKeepaliveFailurePausesAndPreservesRecovery(t *testing.T) {
	ctx := context.Background()
	repo, plans, plan := newKeepaliveRecoveryFixture(t)
	require.NoError(t, repo.SetSchedulable(ctx, plan.AccountID, true))
	cache := &schedulerCacheRecorder{}
	repo.schedulerCache = cache
	before, err := repo.GetByID(ctx, plan.AccountID)
	require.NoError(t, err)
	paused, err := repo.PauseAfterKeepaliveFailure(ctx, plan)
	require.NoError(t, err)
	require.True(t, paused)
	require.Len(t, cache.setAccounts, 1)
	require.False(t, cache.setAccounts[0].Schedulable)
	after, err := repo.GetByID(ctx, plan.AccountID)
	require.NoError(t, err)
	require.False(t, after.Schedulable)
	require.Equal(t, before.Status, after.Status)
	require.Equal(t, before.Extra, after.Extra)
	current, err := plans.GetByID(ctx, plan.ID)
	require.NoError(t, err)
	require.True(t, current.AutoRecover)
	require.Equal(t, plan.UpdatedAt, current.UpdatedAt)
	paused, err = repo.PauseAfterKeepaliveFailure(ctx, plan)
	require.NoError(t, err)
	require.False(t, paused)
	recovered, err := repo.RecoverAfterKeepalive(ctx, plan)
	require.NoError(t, err)
	require.True(t, recovered.ResumedScheduling)
	require.Len(t, cache.setAccounts, 2)
	require.True(t, cache.setAccounts[1].Schedulable)
	var events int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM scheduler_outbox WHERE account_id = $1 AND event_type = $2`, plan.AccountID, service.SchedulerOutboxEventAccountChanged).Scan(&events))
	require.Positive(t, events, "pending scheduling notifications are deduplicated")
}

func TestKeepaliveFailureRechecksPlan(t *testing.T) {
	for _, change := range []string{"opt_out", "disable", "edit", "delete", "wrong_account"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			repo, plans, plan := newKeepaliveRecoveryFixture(t)
			require.NoError(t, repo.SetSchedulable(ctx, plan.AccountID, true))
			current := *plan
			switch change {
			case "opt_out":
				current.AutoRecover = false
			case "disable":
				current.Enabled = false
			case "edit":
				current.ModelID = "gpt-6-sol"
			case "delete":
				require.NoError(t, plans.Delete(ctx, plan.ID))
			case "wrong_account":
				_, _, other := newKeepaliveRecoveryFixture(t)
				plan.ID = other.ID
			}
			if change == "opt_out" || change == "disable" || change == "edit" {
				_, err := plans.Update(ctx, &current)
				require.NoError(t, err)
			}
			paused, err := repo.PauseAfterKeepaliveFailure(ctx, plan)
			require.NoError(t, err)
			require.False(t, paused)
			account, err := repo.GetByID(ctx, plan.AccountID)
			require.NoError(t, err)
			require.True(t, account.Schedulable)
		})
	}
}

func TestKeepaliveFailureWaitsForConcurrentOptOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	repo, _, plan := newKeepaliveRecoveryFixture(t)
	require.NoError(t, repo.SetSchedulable(ctx, plan.AccountID, true))
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `UPDATE scheduled_test_plans SET auto_recover = false WHERE id = $1`, plan.ID)
	require.NoError(t, err)
	type outcome struct {
		paused bool
		err    error
	}
	done := make(chan outcome, 1)
	go func() { paused, err := repo.PauseAfterKeepaliveFailure(ctx, plan); done <- outcome{paused, err} }()
	require.Eventually(t, func() bool {
		var waiting bool
		err := integrationDB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'
			AND query LIKE '%pause AS MATERIALIZED%')`).Scan(&waiting)
		return err == nil && waiting
	}, 3*time.Second, 10*time.Millisecond)
	require.NoError(t, tx.Commit())
	result := <-done
	require.NoError(t, result.err)
	require.False(t, result.paused)
	account, err := repo.GetByID(ctx, plan.AccountID)
	require.NoError(t, err)
	require.True(t, account.Schedulable)
}
