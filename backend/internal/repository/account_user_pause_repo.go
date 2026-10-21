package repository

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

type accountUserPauseRepository struct{ db *sql.DB }

func NewAccountUserPauseRepository(db *sql.DB) service.AccountUserPauseRepository {
	if db == nil {
		return nil
	}
	return &accountUserPauseRepository{db: db}
}

func (r *accountUserPauseRepository) PausedAccountIDs(ctx context.Context, userID int64) (map[int64]struct{}, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT p.account_id FROM account_user_scheduling_pauses p
 JOIN accounts a ON a.id=p.account_id AND a.deleted_at IS NULL AND a.platform='openai'
 WHERE p.user_id=$1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int64]struct{})
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result[id] = struct{}{}
	}
	return result, rows.Err()
}

func (r *accountUserPauseRepository) IsPaused(ctx context.Context, accountID, userID int64) (bool, error) {
	var paused bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM account_user_scheduling_pauses
 WHERE account_id=$1 AND user_id=$2)`, accountID, userID).Scan(&paused)
	return paused, err
}

func (r *accountUserPauseRepository) List(ctx context.Context, accountID int64, page, pageSize int) (*service.AccountUserPausePage, error) {
	var platform string
	var deleted sql.NullTime
	if err := r.db.QueryRowContext(ctx, `SELECT platform,deleted_at FROM accounts WHERE id=$1`, accountID).Scan(&platform, &deleted); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrAccountNotFound
		}
		return nil, err
	}
	if platform != service.PlatformOpenAI {
		return nil, service.ErrAccountUserPausePlatform
	}
	result := &service.AccountUserPausePage{AccountID: accountID, Scope: service.AccountUserPauseScope, Items: []service.AccountUserPauseEntry{}, Page: page, PageSize: pageSize}
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_user_scheduling_pauses WHERE account_id=$1`, accountID).Scan(&result.Total); err != nil {
		return nil, err
	}
	result.Pages = (result.Total + int64(pageSize) - 1) / int64(pageSize)
	// 以规则为主读取用户展示信息，软删除与停用用户不能被默认查询过滤后失去清理入口。
	rows, err := r.db.QueryContext(ctx, `SELECT p.user_id,COALESCE(u.email,''),COALESCE(u.username,''),COALESCE(u.status,''),
 (u.id IS NULL OR u.deleted_at IS NOT NULL),p.created_at
 FROM account_user_scheduling_pauses p LEFT JOIN users u ON u.id=p.user_id
 WHERE p.account_id=$1 ORDER BY p.created_at DESC,p.user_id ASC LIMIT $2 OFFSET $3`, accountID, pageSize, int64(page-1)*int64(pageSize))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item service.AccountUserPauseEntry
		if err := rows.Scan(&item.UserID, &item.Email, &item.Username, &item.UserStatus, &item.UserDeleted, &item.PausedAt); err != nil {
			return nil, err
		}
		result.Items = append(result.Items, item)
	}
	return result, rows.Err()
}

func (r *accountUserPauseRepository) SetPaused(ctx context.Context, accountID, userID, adminID int64, paused bool) (*service.AccountUserPauseResult, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var platform string
	var deleted sql.NullTime
	// 空组合也先锁账号行，固定账号→关系的锁序；否则并发恢复可能漏掉尚未提交的暂停插入。
	err = tx.QueryRowContext(ctx, `SELECT platform,deleted_at FROM accounts WHERE id=$1 FOR NO KEY UPDATE`, accountID).Scan(&platform, &deleted)
	if errors.Is(err, sql.ErrNoRows) && !paused {
		if err := tx.Commit(); err != nil {
			return nil, service.ErrAccountUserPauseResultUnknown.WithCause(err)
		}
		return &service.AccountUserPauseResult{AccountID: accountID, UserID: userID, Scope: service.AccountUserPauseScope}, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrAccountNotFound
	}
	if err != nil {
		return nil, err
	}
	if paused {
		if deleted.Valid {
			return nil, service.ErrAccountNotFound
		}
		if platform != service.PlatformOpenAI {
			return nil, service.ErrAccountUserPausePlatform
		}
		var userDeleted sql.NullTime
		// 用户软删除会更新非主键列；共享行锁让“用户仍存在”的复核保持到本次提交。
		if err := tx.QueryRowContext(ctx, `SELECT deleted_at FROM users WHERE id=$1 FOR SHARE`, userID).Scan(&userDeleted); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, service.ErrUserNotFound
			}
			return nil, err
		}
		if userDeleted.Valid {
			return nil, service.ErrUserNotFound
		}
	}
	var write sql.Result
	if paused {
		write, err = tx.ExecContext(ctx, `INSERT INTO account_user_scheduling_pauses (account_id,user_id,created_at,created_by_admin_id)
 VALUES ($1,$2,clock_timestamp(),NULLIF($3::bigint,0)) ON CONFLICT (account_id,user_id) DO NOTHING`, accountID, userID, adminID)
	} else {
		write, err = tx.ExecContext(ctx, `DELETE FROM account_user_scheduling_pauses WHERE account_id=$1 AND user_id=$2`, accountID, userID)
	}
	if err != nil {
		return nil, err
	}
	affected, err := write.RowsAffected()
	if err != nil {
		return nil, err
	}
	result := &service.AccountUserPauseResult{AccountID: accountID, UserID: userID, Changed: affected > 0, Scope: service.AccountUserPauseScope}
	var pausedAt sql.NullTime
	// 在锁内用同一语句回读组合与计数，成功响应描述本次已提交操作；不借分页列表猜 pair 状态。
	err = tx.QueryRowContext(ctx, `SELECT p.created_at,COALESCE(u.email,''),
 (SELECT COUNT(*) FROM account_user_scheduling_pauses WHERE account_id=$1)
 FROM (SELECT 1) seed LEFT JOIN account_user_scheduling_pauses p ON p.account_id=$1 AND p.user_id=$2
 LEFT JOIN users u ON u.id=$2`, accountID, userID).Scan(&pausedAt, &result.Email, &result.PausedCount)
	if err != nil {
		return nil, err
	}
	result.Paused = pausedAt.Valid
	if pausedAt.Valid {
		t := pausedAt.Time.UTC()
		result.PausedAt = &t
	}
	if err := tx.Commit(); err != nil {
		return nil, service.ErrAccountUserPauseResultUnknown.WithCause(err)
	}
	return result, nil
}

func (r *accountUserPauseRepository) Summary(ctx context.Context, pairs []service.AccountUserPausePair) (*service.AccountUserPauseSummary, error) {
	result := &service.AccountUserPauseSummary{Available: true, Counts: map[int64]int64{}, ActivePausedUsers: map[int64][]int64{}}
	// 计数不依赖进程内活跃账号集合，完全空闲的账号也必须显示持久名单人数。
	rows, err := r.db.QueryContext(ctx, `SELECT p.account_id,COUNT(*) FROM account_user_scheduling_pauses p
 JOIN accounts a ON a.id=p.account_id AND a.deleted_at IS NULL AND a.platform='openai' GROUP BY p.account_id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var accountID, count int64
		if err := rows.Scan(&accountID, &count); err != nil {
			rows.Close()
			return nil, err
		}
		result.Counts[accountID] = count
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(pairs) == 0 {
		return result, nil
	}
	accountIDs, userIDs := make([]int64, 0, len(pairs)), make([]int64, 0, len(pairs))
	seen := map[service.AccountUserPausePair]struct{}{}
	for _, pair := range pairs {
		if pair.AccountID <= 0 || pair.UserID <= 0 {
			continue
		}
		if _, ok := seen[pair]; ok {
			continue
		}
		seen[pair] = struct{}{}
		accountIDs, userIDs = append(accountIDs, pair.AccountID), append(userIDs, pair.UserID)
	}
	if len(accountIDs) == 0 {
		return result, nil
	}
	// unnest 成对输入保留账号与用户的对应关系，避免分别 IN 查询产生交叉组合或逐卡查询。
	rows, err = r.db.QueryContext(ctx, `SELECT p.account_id,p.user_id FROM account_user_scheduling_pauses p
 JOIN unnest($1::bigint[],$2::bigint[]) requested(account_id,user_id) ON p.account_id=requested.account_id AND p.user_id=requested.user_id
 JOIN accounts a ON a.id=p.account_id AND a.deleted_at IS NULL AND a.platform='openai'
 ORDER BY p.account_id,p.user_id`, pq.Array(accountIDs), pq.Array(userIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var accountID, userID int64
		if err := rows.Scan(&accountID, &userID); err != nil {
			return nil, err
		}
		result.ActivePausedUsers[accountID] = append(result.ActivePausedUsers[accountID], userID)
	}
	for _, ids := range result.ActivePausedUsers {
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	}
	return result, rows.Err()
}

var _ service.AccountUserPauseRepository = (*accountUserPauseRepository)(nil)
