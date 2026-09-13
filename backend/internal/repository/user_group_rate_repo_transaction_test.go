package repository

import (
	"context"
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestSyncGroupRateMultipliersRollsBackPartialClear(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE user_group_rate_multipliers`).
		WithArgs(int64(55)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`DELETE FROM user_group_rate_multipliers`).
		WithArgs(int64(55)).
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	repo := NewUserGroupRateRepository(db)
	if err := repo.SyncGroupRateMultipliers(context.Background(), 55, nil); err == nil {
		t.Fatal("expected partial rate clear to roll back")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestClearGroupRPMOverridesCommitsAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE user_group_rate_multipliers`).
		WithArgs(int64(55)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`DELETE FROM user_group_rate_multipliers`).
		WithArgs(int64(55)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	repo := NewUserGroupRateRepository(db)
	if err := repo.ClearGroupRPMOverrides(context.Background(), 55); err != nil {
		t.Fatalf("clear RPM overrides: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}
