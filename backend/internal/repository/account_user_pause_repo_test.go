package repository

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountUserPauseSetPausedLocksBeforeMutationAndReadback(t *testing.T) {
	stamp := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		paused  bool
		changed bool
		deleted any
	}{
		{"pause", true, true, nil}, {"duplicate_pause", true, false, nil}, {"resume_deleted_account", false, true, stamp}, {"duplicate_resume", false, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			repo := NewAccountUserPauseRepository(db)
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT platform,deleted_at FROM accounts WHERE id=\$1 FOR NO KEY UPDATE`).WithArgs(int64(29)).WillReturnRows(sqlmock.NewRows([]string{"platform", "deleted_at"}).AddRow("openai", tc.deleted))
			var affected int64
			if tc.changed {
				affected = 1
			}
			if tc.paused {
				mock.ExpectQuery(`SELECT deleted_at FROM users WHERE id=\$1 FOR SHARE`).WithArgs(int64(8)).WillReturnRows(sqlmock.NewRows([]string{"deleted_at"}).AddRow(nil))
				mock.ExpectExec(`INSERT INTO account_user_scheduling_pauses .*`).WithArgs(int64(29), int64(8), int64(7)).WillReturnResult(sqlmock.NewResult(0, affected))
			} else {
				mock.ExpectExec(`DELETE FROM account_user_scheduling_pauses WHERE account_id=\$1 AND user_id=\$2`).WithArgs(int64(29), int64(8)).WillReturnResult(sqlmock.NewResult(0, affected))
			}
			var pausedAt any
			count := int64(2)
			if tc.paused {
				pausedAt = stamp
				count = 3
			}
			mock.ExpectQuery(`SELECT p.created_at,COALESCE\(u.email,''\),`).WithArgs(int64(29), int64(8)).WillReturnRows(sqlmock.NewRows([]string{"created_at", "email", "count"}).AddRow(pausedAt, "user@example.com", count))
			mock.ExpectCommit()
			got, err := repo.SetPaused(context.Background(), 29, 8, 7, tc.paused)
			require.NoError(t, err)
			require.Equal(t, tc.paused, got.Paused)
			require.Equal(t, tc.changed, got.Changed)
			require.Equal(t, count, got.PausedCount)
			if tc.paused {
				require.Equal(t, &stamp, got.PausedAt)
			} else {
				require.Nil(t, got.PausedAt)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestAccountUserPauseResumePhysicallyDeletedAccountIsIdempotent(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT platform,deleted_at FROM accounts WHERE id=\$1 FOR NO KEY UPDATE`).WithArgs(int64(29)).WillReturnError(sql.ErrNoRows)
	mock.ExpectCommit()
	got, err := NewAccountUserPauseRepository(db).SetPaused(context.Background(), 29, 8, 7, false)
	require.NoError(t, err)
	require.False(t, got.Paused)
	require.False(t, got.Changed)
	require.Zero(t, got.PausedCount)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountUserPauseDoesNotCreateForDeletedOrOtherPlatform(t *testing.T) {
	for _, platform := range []string{"claude", "openai"} {
		t.Run(platform, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT platform,deleted_at FROM accounts WHERE id=\$1 FOR NO KEY UPDATE`).WillReturnRows(sqlmock.NewRows([]string{"platform", "deleted_at"}).AddRow(platform, nil))
			if platform == "openai" {
				mock.ExpectQuery(`SELECT deleted_at FROM users WHERE id=\$1 FOR SHARE`).WillReturnRows(sqlmock.NewRows([]string{"deleted_at"}).AddRow(time.Now()))
			}
			mock.ExpectRollback()
			_, err = NewAccountUserPauseRepository(db).SetPaused(context.Background(), 29, 8, 7, true)
			if platform == "openai" {
				require.ErrorIs(t, err, service.ErrUserNotFound)
			} else {
				require.ErrorIs(t, err, service.ErrAccountUserPausePlatform)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestAccountUserPauseCommitFailureIsUnknown(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT platform,deleted_at FROM accounts WHERE id=\$1 FOR NO KEY UPDATE`).WillReturnRows(sqlmock.NewRows([]string{"platform", "deleted_at"}).AddRow("openai", nil))
	mock.ExpectExec(`DELETE FROM account_user_scheduling_pauses`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT p.created_at,COALESCE`).WillReturnRows(sqlmock.NewRows([]string{"created_at", "email", "count"}).AddRow(nil, "user@example.com", 0))
	mock.ExpectCommit().WillReturnError(errors.New("commit acknowledgement lost"))
	_, err = NewAccountUserPauseRepository(db).SetPaused(context.Background(), 29, 8, 7, false)
	require.ErrorIs(t, err, service.ErrAccountUserPauseResultUnknown)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountUserPauseSummaryCountsIdleAccountsAndMatchesOnlyActivePairs(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`SELECT p.account_id,COUNT\(\*\)`).WillReturnRows(sqlmock.NewRows([]string{"account_id", "count"}).AddRow(29, 3).AddRow(999, 4))
	mock.ExpectQuery(`SELECT p.account_id,p.user_id .*JOIN unnest`).WithArgs("{29,30}", "{8,9}").WillReturnRows(sqlmock.NewRows([]string{"account_id", "user_id"}).AddRow(29, 8))
	got, err := NewAccountUserPauseRepository(db).Summary(context.Background(), []service.AccountUserPausePair{{AccountID: 29, UserID: 8}, {AccountID: 30, UserID: 9}, {AccountID: 29, UserID: 8}})
	require.NoError(t, err)
	require.True(t, got.Available)
	require.Equal(t, int64(4), got.Counts[999])
	require.Equal(t, []int64{8}, got.ActivePausedUsers[29])
	require.NotContains(t, got.ActivePausedUsers, int64(999))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountUserPauseListIncludesDeletedUsers(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`SELECT platform,deleted_at FROM accounts`).WithArgs(int64(29)).WillReturnRows(sqlmock.NewRows([]string{"platform", "deleted_at"}).AddRow("openai", nil))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM account_user_scheduling_pauses`).WithArgs(int64(29)).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT p.user_id,COALESCE.*LEFT JOIN users`).WithArgs(int64(29), 20, int64(0)).WillReturnRows(sqlmock.NewRows([]string{"user_id", "email", "username", "status", "deleted", "created_at"}).AddRow(8, "old@example.com", "old", "inactive", true, time.Now()))
	got, err := NewAccountUserPauseRepository(db).List(context.Background(), 29, 1, 20)
	require.NoError(t, err)
	require.Len(t, got.Items, 1)
	require.True(t, got.Items[0].UserDeleted)
	require.Equal(t, "old@example.com", got.Items[0].Email)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountUserPauseReadsPropagateDatabaseFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	failure := errors.New("database unavailable")
	mock.ExpectQuery(`SELECT p.account_id FROM account_user_scheduling_pauses`).WithArgs(int64(8)).WillReturnError(failure)
	ids, err := NewAccountUserPauseRepository(db).PausedAccountIDs(context.Background(), 8)
	require.Nil(t, ids)
	require.ErrorIs(t, err, failure)
	mock.ExpectQuery(`SELECT EXISTS`).WithArgs(int64(29), int64(8)).WillReturnError(failure)
	_, err = NewAccountUserPauseRepository(db).IsPaused(context.Background(), 29, 8)
	require.ErrorIs(t, err, failure)
	require.NoError(t, mock.ExpectationsWereMet())
}
