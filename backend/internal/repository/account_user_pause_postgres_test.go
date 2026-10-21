package repository

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func newAccountPausePostgres(t *testing.T) (*sql.DB, *sql.DB, string, string) {
	t.Helper()
	dsn := os.Getenv("TEST_ACCOUNT_USER_PAUSE_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_ACCOUNT_USER_PAUSE_DATABASE_URL for isolated PostgreSQL validation")
	}
	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"postgres", "postgresql"}, parsed.Scheme)
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, admin.PingContext(ctx))
	schema := "account_pause_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.ExecContext(ctx, "CREATE SCHEMA "+pq.QuoteIdentifier(schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, cleanupErr := admin.ExecContext(cleanupCtx, "DROP SCHEMA "+pq.QuoteIdentifier(schema)+" CASCADE")
		require.NoError(t, cleanupErr)
	})
	query := parsed.Query()
	query.Set("search_path", schema)
	query.Set("application_name", schema)
	parsed.RawQuery = query.Encode()
	scopedDSN := parsed.String()
	db, err := sql.Open("postgres", scopedDSN)
	require.NoError(t, err)
	db.SetMaxOpenConns(8)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `CREATE TABLE users (id BIGINT PRIMARY KEY,email TEXT NOT NULL,username TEXT NOT NULL DEFAULT '',status TEXT NOT NULL DEFAULT 'active',deleted_at TIMESTAMPTZ NULL);
 CREATE TABLE accounts (id BIGINT PRIMARY KEY,platform TEXT NOT NULL,deleted_at TIMESTAMPTZ NULL);
 INSERT INTO users(id,email) VALUES(7,'admin@example.com'),(8,'user@example.com'),(9,'other@example.com'),(10,'new@example.com');
 INSERT INTO accounts(id,platform) VALUES(29,'openai'),(30,'openai'),(999,'openai'),(31,'anthropic');`)
	require.NoError(t, err)
	migration, err := os.ReadFile("../../migrations/246_account_user_scheduling_pauses.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	return db, admin, scopedDSN, schema
}

func waitAccountPauseRowLock(t *testing.T, observer *sql.DB, schema string) {
	t.Helper()
	// 等待真实 pg_stat_activity 的锁等待证据，不用单次睡眠猜另一个写入是否已经到达账号锁。
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var count int
		err := observer.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_stat_activity
 WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE '%FOR NO KEY UPDATE%'`, schema).Scan(&count)
		return err == nil && count > 0
	}, 5*time.Second, 10*time.Millisecond)
}

func TestAccountUserPausePostgresOppositeWritesWaitForAccountLock(t *testing.T) {
	db, observer, _, schema := newAccountPausePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	repo := NewAccountUserPauseRepository(db)
	for _, uncommittedPause := range []bool{true, false} {
		t.Run(map[bool]string{true: "pause_then_resume", false: "resume_then_pause"}[uncommittedPause], func(t *testing.T) {
			if !uncommittedPause {
				_, err := repo.SetPaused(ctx, 29, 8, 7, true)
				require.NoError(t, err)
			}
			tx, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer tx.Rollback()
			var platform string
			require.NoError(t, tx.QueryRowContext(ctx, `SELECT platform FROM accounts WHERE id=29 FOR NO KEY UPDATE`).Scan(&platform))
			if uncommittedPause {
				_, err = tx.ExecContext(ctx, `INSERT INTO account_user_scheduling_pauses(account_id,user_id,created_by_admin_id) VALUES(29,8,7)`)
			} else {
				_, err = tx.ExecContext(ctx, `DELETE FROM account_user_scheduling_pauses WHERE account_id=29 AND user_id=8`)
			}
			require.NoError(t, err)
			type outcome struct {
				result *service.AccountUserPauseResult
				err    error
			}
			completed := make(chan outcome, 1)
			go func() {
				result, writeErr := repo.SetPaused(ctx, 29, 8, 7, !uncommittedPause)
				completed <- outcome{result, writeErr}
			}()
			waitAccountPauseRowLock(t, observer, schema)
			select {
			case premature := <-completed:
				t.Fatalf("opposite write escaped account lock: %+v", premature)
			default:
			}
			require.NoError(t, tx.Commit())
			select {
			case got := <-completed:
				require.NoError(t, got.err)
				require.Equal(t, !uncommittedPause, got.result.Paused)
				require.True(t, got.result.Changed)
				state, readErr := repo.IsPaused(ctx, 29, 8)
				require.NoError(t, readErr)
				require.Equal(t, !uncommittedPause, state)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			_, err = repo.SetPaused(ctx, 29, 8, 7, false)
			require.NoError(t, err)
		})
	}
}

func TestAccountUserPausePostgresPersistenceLifecycleAndIdleSummary(t *testing.T) {
	db, _, dsn, _ := newAccountPausePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	repo := NewAccountUserPauseRepository(db)
	first, err := repo.SetPaused(ctx, 29, 8, 7, true)
	require.NoError(t, err)
	require.True(t, first.Changed)
	again, err := repo.SetPaused(ctx, 29, 8, 7, true)
	require.NoError(t, err)
	require.False(t, again.Changed)
	require.Equal(t, first.PausedAt, again.PausedAt)
	_, err = db.ExecContext(ctx, `UPDATE users SET email='changed@example.com',status='inactive',deleted_at=NOW() WHERE id=8`)
	require.NoError(t, err)
	page, err := repo.List(ctx, 29, 1, 20)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, "changed@example.com", page.Items[0].Email)
	require.True(t, page.Items[0].UserDeleted)
	resumed, err := repo.SetPaused(ctx, 29, 8, 7, false)
	require.NoError(t, err)
	require.True(t, resumed.Changed)
	require.False(t, resumed.Paused)
	_, err = repo.SetPaused(ctx, 29, 8, 7, true)
	require.ErrorIs(t, err, service.ErrUserNotFound)
	_, err = repo.SetPaused(ctx, 31, 9, 7, true)
	require.ErrorIs(t, err, service.ErrAccountUserPausePlatform)
	_, err = repo.SetPaused(ctx, 999, 9, 7, true)
	require.NoError(t, err)
	// 新连接与新仓储必须从持久库恢复，不依赖原服务或进程内名单。
	reconnected, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer reconnected.Close()
	freshRepo := NewAccountUserPauseRepository(reconnected)
	paused, err := freshRepo.IsPaused(ctx, 999, 9)
	require.NoError(t, err)
	require.True(t, paused)
	ids, err := freshRepo.PausedAccountIDs(ctx, 9)
	require.NoError(t, err)
	require.Contains(t, ids, int64(999))
	ids, err = freshRepo.PausedAccountIDs(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, ids)
	summary, err := freshRepo.Summary(ctx, []service.AccountUserPausePair{{AccountID: 29, UserID: 8}})
	require.NoError(t, err)
	require.Equal(t, int64(1), summary.Counts[999])
	require.Empty(t, summary.ActivePausedUsers)
	_, err = db.ExecContext(ctx, `UPDATE accounts SET deleted_at=NOW() WHERE id=999`)
	require.NoError(t, err)
	summary, err = freshRepo.Summary(ctx, nil)
	require.NoError(t, err)
	require.NotContains(t, summary.Counts, int64(999))
	resumed, err = repo.SetPaused(ctx, 999, 9, 7, false)
	require.NoError(t, err)
	require.True(t, resumed.Changed)
	_, err = repo.SetPaused(ctx, 30, 9, 7, true)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM users WHERE id=7`)
	require.NoError(t, err)
	paused, err = repo.IsPaused(ctx, 30, 9)
	require.NoError(t, err)
	require.True(t, paused)
	var actor sql.NullInt64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT created_by_admin_id FROM account_user_scheduling_pauses WHERE account_id=30 AND user_id=9`).Scan(&actor))
	require.False(t, actor.Valid)
	_, err = db.ExecContext(ctx, `DELETE FROM users WHERE id=9`)
	require.NoError(t, err)
	paused, err = repo.IsPaused(ctx, 30, 9)
	require.NoError(t, err)
	require.False(t, paused)
	resumed, err = repo.SetPaused(ctx, 123456, 9, 7, false)
	require.NoError(t, err)
	require.False(t, resumed.Changed)
}
