//go:build unit

package service

import (
	"context"
	"testing"

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
