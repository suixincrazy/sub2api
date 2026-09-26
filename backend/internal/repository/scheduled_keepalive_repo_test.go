//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestScheduledKeepaliveUpdateDoesNotReviveEditedOrPausedPlan(t *testing.T) {
	for _, count := range []int64{0, 1} {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		repo := NewScheduledTestPlanRepository(db)
		now := time.Now()
		plan := &service.ScheduledTestPlan{ID: 8, UpdatedAt: now.Add(-time.Minute)}
		mock.ExpectExec(`UPDATE scheduled_test_plans SET last_run_at = \$2, next_run_at = \$3, last_status = \$4, consecutive_failures = \$5 WHERE id = \$1 AND enabled = true AND updated_at = \$6`).
			WithArgs(int64(8), now, now.Add(time.Minute), "success", 0, plan.UpdatedAt).
			WillReturnResult(sqlmock.NewResult(0, count))
		applied, err := repo.UpdateAfterRun(context.Background(), plan, now, now.Add(time.Minute), "success", 0)
		require.NoError(t, err)
		require.Equal(t, count == 1, applied)
		require.NoError(t, mock.ExpectationsWereMet())
		_ = db.Close()
	}
}

func TestScheduledKeepaliveReadsPersistedIntervalsAndState(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	now := time.Now()
	cols := []string{"id", "account_id", "model_id", "cron_expression", "enabled", "max_results", "auto_recover", "last_run_at", "next_run_at", "created_at", "updated_at", "probe_interval_seconds", "keepalive_interval_seconds", "keepalive_max_interval_seconds", "last_status", "consecutive_failures"}
	mock.ExpectQuery(`FROM scheduled_test_plans WHERE enabled = true`).WillReturnRows(sqlmock.NewRows(cols).
		AddRow(1, 8, "gpt-6-astra", "", true, 100, true, now, now.Add(time.Minute), now, now, 2, 60, 90, "success", 0))
	plans, err := NewScheduledTestPlanRepository(db).ListEnabled(context.Background())
	require.NoError(t, err)
	require.Len(t, plans, 1)
	require.Equal(t, 2, plans[0].ProbeIntervalSeconds)
	require.Equal(t, 60, plans[0].KeepaliveIntervalSeconds)
	require.Equal(t, 90, plans[0].KeepaliveMaxIntervalSeconds)
	require.Equal(t, "success", plans[0].LastStatus)
	require.True(t, plans[0].NextRunAt.After(now))
	require.NoError(t, mock.ExpectationsWereMet())
}
