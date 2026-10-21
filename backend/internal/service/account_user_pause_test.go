package service

import (
	"context"
	"errors"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type accountPauseServiceRepo struct {
	AccountUserPauseRepository
	ids     func(context.Context, int64) (map[int64]struct{}, error)
	check   func(context.Context, int64, int64) (bool, error)
	set     func(context.Context, int64, int64, int64, bool) (*AccountUserPauseResult, error)
	summary func(context.Context, []AccountUserPausePair) (*AccountUserPauseSummary, error)
}

func (r accountPauseServiceRepo) PausedAccountIDs(ctx context.Context, id int64) (map[int64]struct{}, error) {
	return r.ids(ctx, id)
}
func (r accountPauseServiceRepo) IsPaused(ctx context.Context, aid, uid int64) (bool, error) {
	return r.check(ctx, aid, uid)
}
func (r accountPauseServiceRepo) SetPaused(ctx context.Context, aid, uid, actor int64, paused bool) (*AccountUserPauseResult, error) {
	return r.set(ctx, aid, uid, actor, paused)
}
func (r accountPauseServiceRepo) Summary(ctx context.Context, pairs []AccountUserPausePair) (*AccountUserPauseSummary, error) {
	return r.summary(ctx, pairs)
}

type accountPauseServiceUsers struct {
	UserRepository
	byEmail func(context.Context, string) (*User, error)
	byID    func(context.Context, int64) (*User, error)
}

func (r accountPauseServiceUsers) GetByEmail(ctx context.Context, email string) (*User, error) {
	return r.byEmail(ctx, email)
}
func (r accountPauseServiceUsers) GetByID(ctx context.Context, id int64) (*User, error) {
	return r.byID(ctx, id)
}

func TestAccountUserPauseServiceFailsClosedWhenUnavailable(t *testing.T) {
	for _, svc := range []*AccountUserPauseService{nil, NewAccountUserPauseService(nil, nil)} {
		ids, err := svc.PausedAccountIDs(context.Background(), 8)
		require.Nil(t, ids)
		require.ErrorIs(t, err, ErrAccountUserPauseUnavailable)
		_, err = svc.IsPaused(context.Background(), 29, 8)
		require.ErrorIs(t, err, ErrAccountUserPauseUnavailable)
	}
	failure := errors.New("database offline")
	svc := NewAccountUserPauseService(accountPauseServiceRepo{
		ids:   func(context.Context, int64) (map[int64]struct{}, error) { return nil, failure },
		check: func(context.Context, int64, int64) (bool, error) { return false, failure },
	}, nil)
	ids, err := svc.PausedAccountIDs(context.Background(), 8)
	require.Nil(t, ids)
	require.Equal(t, "PAUSE_STORE_UNAVAILABLE", infraerrors.Reason(err))
	require.ErrorIs(t, err, failure)
	_, err = svc.IsPaused(context.Background(), 29, 8)
	require.ErrorIs(t, err, ErrAccountUserPauseUnavailable)
}

func TestAccountUserPauseServiceUsesNormalizedEmailAndAuthenticatedActor(t *testing.T) {
	users := accountPauseServiceUsers{byEmail: func(_ context.Context, email string) (*User, error) {
		require.Equal(t, "user@example.com", email)
		return &User{ID: 8, Email: email, Status: "inactive"}, nil
	}}
	repo := accountPauseServiceRepo{set: func(_ context.Context, aid, uid, actor int64, paused bool) (*AccountUserPauseResult, error) {
		require.Equal(t, int64(29), aid)
		require.Equal(t, int64(8), uid)
		require.Equal(t, int64(7), actor)
		require.True(t, paused)
		return &AccountUserPauseResult{AccountID: aid, UserID: uid, Paused: true, Changed: true}, nil
	}}
	got, err := NewAccountUserPauseService(repo, users).Pause(context.Background(), 29, 0, " User@Example.COM ", 7)
	require.NoError(t, err)
	require.Equal(t, AccountUserPauseScope, got.Scope)
}

func TestAccountUserPauseServicePreservesEmailLookupErrors(t *testing.T) {
	for _, failure := range []error{ErrUserNotFound, ErrUserEmailAmbiguous, errors.New("query failed")} {
		svc := NewAccountUserPauseService(accountPauseServiceRepo{}, accountPauseServiceUsers{byEmail: func(context.Context, string) (*User, error) { return nil, failure }})
		_, err := svc.Pause(context.Background(), 29, 0, "user@example.com", 7)
		require.ErrorIs(t, err, failure)
		if !errors.Is(failure, ErrUserNotFound) && !errors.Is(failure, ErrUserEmailAmbiguous) {
			require.ErrorIs(t, err, ErrAccountUserPauseUnavailable)
		}
	}
}

func TestAccountUserPauseServiceRejectsAmbiguousIdentityAndInvalidEmail(t *testing.T) {
	svc := NewAccountUserPauseService(accountPauseServiceRepo{}, accountPauseServiceUsers{})
	for _, tc := range []struct {
		uid   int64
		email string
	}{{8, "user@example.com"}, {0, ""}, {-1, "user@example.com"}, {0, "Name <user@example.com>"}, {0, "not-an-email"}} {
		_, err := svc.Pause(context.Background(), 29, tc.uid, tc.email, 7)
		require.Equal(t, "INVALID_REQUEST", infraerrors.Reason(err))
	}
}

func TestAccountUserPauseResumeDoesNotRequireExistingUser(t *testing.T) {
	svc := NewAccountUserPauseService(accountPauseServiceRepo{set: func(_ context.Context, aid, uid, actor int64, paused bool) (*AccountUserPauseResult, error) {
		require.False(t, paused)
		return &AccountUserPauseResult{AccountID: aid, UserID: uid}, nil
	}}, nil)
	got, err := svc.Resume(context.Background(), 29, 8, 7)
	require.NoError(t, err)
	require.False(t, got.Paused)
}

func TestAccountUserPauseSummaryOnlyChecksActivePairsAndKeepsIdleCounts(t *testing.T) {
	svc := NewAccountUserPauseService(accountPauseServiceRepo{summary: func(_ context.Context, pairs []AccountUserPausePair) (*AccountUserPauseSummary, error) {
		require.Equal(t, []AccountUserPausePair{{AccountID: 29, UserID: 8}, {AccountID: 30, UserID: 9}}, pairs)
		return &AccountUserPauseSummary{Available: true, Counts: map[int64]int64{999: 4}}, nil
	}}, nil)
	got, err := svc.Summary(context.Background(), map[int64]AccountSlotObservation{29: {Users: map[string]int64{"8": 1, "invalid": 1}, WaitingUsers: map[string]int64{"10": 1}}, 30: {Users: map[string]int64{"9": 1}}})
	require.NoError(t, err)
	require.Equal(t, int64(4), got.Counts[999])
}

func TestAccountUserPauseServiceReadDoesNotTurnNilSnapshotIntoEmpty(t *testing.T) {
	svc := NewAccountUserPauseService(accountPauseServiceRepo{ids: func(context.Context, int64) (map[int64]struct{}, error) { return nil, nil }}, nil)
	ids, err := svc.PausedAccountIDs(context.Background(), 8)
	require.Nil(t, ids)
	require.ErrorIs(t, err, ErrAccountUserPauseUnavailable)
}
