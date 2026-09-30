//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func TestAccountSlotObservationTracksExactUsersAndRelease(t *testing.T) {
	cache := &stubConcurrencyCacheForTest{acquireResult: true}
	svc := NewConcurrencyService(cache)
	ctx := context.WithValue(context.Background(), ctxkey.UserID, int64(42))

	first, err := svc.AcquireAccountSlot(ctx, 7, 2)
	require.NoError(t, err)
	second, err := svc.AcquireAccountSlot(ctx, 7, 2)
	require.NoError(t, err)

	accounts, _ := svc.SnapshotAccountSlotObservation()
	require.Equal(t, int64(2), accounts[7].Active)
	require.Equal(t, int64(2), accounts[7].Users["42"])

	first.ReleaseFunc()
	accounts, _ = svc.SnapshotAccountSlotObservation()
	require.Equal(t, int64(1), accounts[7].Active)
	require.Equal(t, int64(1), accounts[7].Users["42"])

	second.ReleaseFunc()
	accounts, _ = svc.SnapshotAccountSlotObservation()
	_, exists := accounts[7]
	require.False(t, exists)
}

func TestAccountSlotObservationTracksUnlimitedAccounts(t *testing.T) {
	cache := &stubConcurrencyCacheForTest{}
	svc := NewConcurrencyService(cache)
	ctx := context.WithValue(context.Background(), ctxkey.UserID, int64(9))

	result, err := svc.AcquireAccountSlot(ctx, 11, 0)
	require.NoError(t, err)
	accounts, _ := svc.SnapshotAccountSlotObservation()
	require.Equal(t, int64(1), accounts[11].Active)
	require.Equal(t, int64(1), accounts[11].Users["9"])

	result.ReleaseFunc()
	accounts, _ = svc.SnapshotAccountSlotObservation()
	_, exists := accounts[11]
	require.False(t, exists)
}

func TestAccountSlotObservationTracksWaitingQueue(t *testing.T) {
	cache := &stubConcurrencyCacheForTest{waitAllowed: true}
	svc := NewConcurrencyService(cache)

	ok, err := svc.IncrementAccountWaitCount(context.Background(), 13, 3)
	require.NoError(t, err)
	require.True(t, ok)
	accounts, _ := svc.SnapshotAccountSlotObservation()
	require.Equal(t, int64(1), accounts[13].Waiting)

	svc.DecrementAccountWaitCount(context.Background(), 13)
	accounts, _ = svc.SnapshotAccountSlotObservation()
	_, exists := accounts[13]
	require.False(t, exists)
}

func TestAccountSlotObservationTracksWaitingUsersIndependently(t *testing.T) {
	svc := NewConcurrencyService(&stubConcurrencyCacheForTest{waitAllowed: true})
	first := context.WithValue(context.Background(), ctxkey.UserID, int64(42))
	second := context.WithValue(context.Background(), ctxkey.UserID, int64(43))

	for _, ctx := range []context.Context{first, first, second, context.Background()} {
		ok, err := svc.IncrementAccountWaitCount(ctx, 13, 5)
		require.NoError(t, err)
		require.True(t, ok)
	}
	ok, err := svc.IncrementAccountWaitCount(first, 14, 5)
	require.NoError(t, err)
	require.True(t, ok)
	accounts, _ := svc.SnapshotAccountSlotObservation()
	require.Equal(t, int64(4), accounts[13].Waiting)
	require.Equal(t, map[string]int64{"42": 2, "43": 1}, accounts[13].WaitingUsers)
	require.Empty(t, accounts[13].Users)
	require.Equal(t, int64(1), accounts[14].WaitingUsers["42"])

	// HTTP 响应序列化后的副本不能反向改写正在等待的真实请求。
	accounts[13].WaitingUsers["42"] = 100
	svc.DecrementAccountWaitCount(first, 13)
	accounts, _ = svc.SnapshotAccountSlotObservation()
	require.Equal(t, int64(3), accounts[13].Waiting)
	require.Equal(t, int64(1), accounts[13].WaitingUsers["42"])
	require.Equal(t, int64(1), accounts[14].WaitingUsers["42"])

	svc.DecrementAccountWaitCount(context.Background(), 13)
	svc.DecrementAccountWaitCount(first, 13)
	svc.DecrementAccountWaitCount(first, 13)
	accounts, _ = svc.SnapshotAccountSlotObservation()
	require.Equal(t, int64(1), accounts[13].Waiting)
	require.Equal(t, map[string]int64{"43": 1}, accounts[13].WaitingUsers)

	svc.DecrementAccountWaitCount(second, 13)
	svc.DecrementAccountWaitCount(first, 14)
	accounts, _ = svc.SnapshotAccountSlotObservation()
	require.Empty(t, accounts)
}

func TestAccountSlotObservationWaitingBecomesActive(t *testing.T) {
	svc := NewConcurrencyService(&stubConcurrencyCacheForTest{waitAllowed: true, acquireResult: true})
	ctx := context.WithValue(context.Background(), ctxkey.UserID, int64(42))
	ok, err := svc.IncrementAccountWaitCount(ctx, 13, 3)
	require.NoError(t, err)
	require.True(t, ok)

	slot, err := svc.AcquireAccountSlot(ctx, 13, 2)
	require.NoError(t, err)
	svc.DecrementAccountWaitCount(ctx, 13)
	accounts, _ := svc.SnapshotAccountSlotObservation()
	require.Zero(t, accounts[13].Waiting)
	require.Empty(t, accounts[13].WaitingUsers)
	require.Equal(t, int64(1), accounts[13].Active)
	require.Equal(t, int64(1), accounts[13].Users["42"])

	slot.ReleaseFunc()
	accounts, _ = svc.SnapshotAccountSlotObservation()
	require.Empty(t, accounts)
}

func TestAccountSlotObservationWaitingCleanupAfterCancellation(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "cancelled"
		if deadline {
			name = "timed_out"
		}
		t.Run(name, func(t *testing.T) {
			svc := NewConcurrencyService(&stubConcurrencyCacheForTest{waitAllowed: true})
			identity := context.WithValue(context.Background(), ctxkey.UserID, int64(42))
			ctx, cancel := context.WithCancel(identity)
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(identity, 10*time.Millisecond)
			}
			defer cancel()
			ok, err := svc.IncrementAccountWaitCount(ctx, 13, 3)
			require.NoError(t, err)
			require.True(t, ok)
			if deadline {
				<-ctx.Done()
				require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
			} else {
				cancel()
				require.ErrorIs(t, ctx.Err(), context.Canceled)
			}
			svc.DecrementAccountWaitCount(ctx, 13)
			accounts, _ := svc.SnapshotAccountSlotObservation()
			require.Empty(t, accounts)
		})
	}
}

func TestAccountSlotObservationWaitingPreservesAdmissionBehavior(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		cache   ConcurrencyCache
		allowed bool
	}{
		{name: "queue_full", cache: &stubConcurrencyCacheForTest{}, allowed: false},
		{name: "cache_unavailable", cache: &stubConcurrencyCacheForTest{waitErr: errors.New("offline")}, allowed: true},
		{name: "no_cache", allowed: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			svc := NewConcurrencyService(scenario.cache)
			ctx := context.WithValue(context.Background(), ctxkey.UserID, int64(42))
			ok, err := svc.IncrementAccountWaitCount(ctx, 13, 3)
			require.NoError(t, err)
			require.Equal(t, scenario.allowed, ok)
			accounts, _ := svc.SnapshotAccountSlotObservation()
			if !scenario.allowed {
				require.Empty(t, accounts)
				return
			}
			require.Equal(t, int64(1), accounts[13].WaitingUsers["42"])
			svc.DecrementAccountWaitCount(ctx, 13)
			accounts, _ = svc.SnapshotAccountSlotObservation()
			require.Empty(t, accounts)
		})
	}
}

func TestAccountSlotObservationWaitingSnapshotsAreRaceSafe(t *testing.T) {
	store := newAccountSlotObservationStore()
	var workers sync.WaitGroup
	for userID := int64(0); userID < 8; userID++ {
		workers.Add(1)
		go func(id int64) {
			defer workers.Done()
			for i := 0; i < 100; i++ {
				store.incrementWaiting(13, id)
				store.snapshot()
				store.decrementWaiting(13, id)
			}
		}(userID)
	}
	workers.Wait()
	require.Empty(t, store.snapshot())
}
