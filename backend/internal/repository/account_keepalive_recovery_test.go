//go:build unit

package repository

import (
	"context"
	"errors"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

func TestManualSchedulingPauseRollsBackIfRecoveryRevocationFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)
	wantErr := errors.New("cannot persist recovery opt-out")
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE accounts SET schedulable = \$1, updated_at = NOW\(\)`).
		WithArgs(false, "{7}").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE scheduled_test_plans`).WithArgs("{7}").WillReturnError(wantErr)
	mock.ExpectRollback()

	err = repo.SetSchedulable(context.Background(), 7, false)

	require.ErrorIs(t, err, wantErr)
	require.NoError(t, mock.ExpectationsWereMet())
}
