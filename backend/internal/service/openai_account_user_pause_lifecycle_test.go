package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type pauseLiveConcurrencyCache struct {
	schedulerTestConcurrencyCache
	leases         []int64
	releasedLeases []int64
}

func (c *pauseLiveConcurrencyCache) AcquireLiveLease(_ context.Context, aid int64, _ int, _ int64, _ int, _ int64, _ string, _ bool) (bool, error) {
	c.leases = append(c.leases, aid)
	return true, nil
}
func (c *pauseLiveConcurrencyCache) RefreshLiveLease(context.Context, int64, int64, int64, string) (bool, error) {
	return true, nil
}
func (c *pauseLiveConcurrencyCache) ReleaseLiveLease(_ context.Context, aid, _, _ int64, _ string) error {
	c.releasedLeases = append(c.releasedLeases, aid)
	return nil
}

func TestAccountUserPauseLiveFinalVetoReleasesBothLeases(t *testing.T) {
	rules := &pauseRoutingRepository{pauseOnCheck: map[int64]bool{1: true, 2: true, 3: true, 4: true}}
	accounts := pauseRoutingAccounts()
	for i := range accounts {
		accounts[i].Type = AccountTypeOAuth
		accounts[i].Credentials = map[string]any{"access_token": "fixture"}
	}
	svc := pauseRoutingService(rules, accounts, false)
	svc.cache = &liveTestStore{GatewayCache: &schedulerTestGatewayCache{}}
	var slotReleases []int64
	cache := &pauseLiveConcurrencyCache{schedulerTestConcurrencyCache: schedulerTestConcurrencyCache{releasedIDs: &slotReleases}}
	svc.concurrencyService = NewConcurrencyService(cache)
	svc.liveAttestation = liveAttestationStub{header: `{"v":1,"t":"fixture"}`}
	svc.liveAttestationCipher = newLiveAttestationCipher(&config.Config{JWT: config.JWTConfig{Secret: "fixture-only"}})
	group := int64(1)
	created, err := svc.CreateLiveCall(pauseRoutingContext(42, true), &LiveCallRequest{SDP: "v=0\r\n", Session: json.RawMessage(`{"model":"gpt-live"}`)}, LiveCallIdentity{UserID: 42, APIKeyID: 7, GroupID: &group}, 10)
	require.Nil(t, created)
	require.Error(t, err)
	require.ElementsMatch(t, []int64{1, 2, 3, 4}, cache.leases)
	require.ElementsMatch(t, cache.leases, cache.releasedLeases)
	require.ElementsMatch(t, cache.leases, slotReleases)
}

type pauseDispatchCache struct {
	dispatchPreviewCoverageCache
	op        *OpenAIDispatchOperationRecord
	onReserve func()
	eligible  []bool
}

func (c *pauseDispatchCache) FindOpenAIDispatchOperation(context.Context, string, string) (*OpenAIDispatchOperationRecord, error) {
	return c.op, nil
}
func (c *pauseDispatchCache) GetOpenAIDispatchPreview(context.Context, string) (*OpenAIDispatchPreviewRecord, error) {
	return c.saved, nil
}
func (c *pauseDispatchCache) ReserveOpenAIDispatchOperation(_ context.Context, _ string, record *OpenAIDispatchOperationRecord) (*OpenAIDispatchOperationRecord, error) {
	c.op = record
	if c.onReserve != nil {
		c.onReserve()
	}
	return record, nil
}
func (c *pauseDispatchCache) GetOpenAIDispatchOperation(context.Context, string) (*OpenAIDispatchOperationRecord, error) {
	return c.op, nil
}
func (c *pauseDispatchCache) RebindOpenAIDispatchSession(_ context.Context, record *OpenAIDispatchOperationRecord, i int, _ time.Duration, _ time.Duration, _ string, eligible bool) error {
	c.eligible = append(c.eligible, eligible)
	item := &record.Operation.Sessions[i]
	item.RebindResult = "rebound"
	if !eligible {
		item.RebindResult = item.Reason
	}
	return nil
}

func TestAccountUserPauseManualDispatchChecksPreviewCreateAndEachItem(t *testing.T) {
	for _, phase := range []string{"preview", "create", "item"} {
		t.Run(phase, func(t *testing.T) {
			rules := &pauseRoutingRepository{paused: map[int64]map[int64]struct{}{42: {}}}
			svc := pauseRoutingService(rules, pauseRoutingAccounts()[:2], false)
			scope := OpenAIStickyScope{GroupID: 1, Hash: "fixture-session"}
			cache := &pauseDispatchCache{dispatchPreviewCoverageCache: dispatchPreviewCoverageCache{scopes: []OpenAIStickyScope{scope}, snapshots: map[observedAccountSession]OpenAIStickySnapshot{{groupID: 1, hash: scope.Hash}: {AccountID: 1, OwnerID: 42, Supported: true}}}}
			svc.cache = cache
			svc.userRepo = dispatchPreviewCoverageUserRepo{}
			svc.rateLimitService = &RateLimitService{settingService: &SettingService{settingRepo: dispatchPreviewCoverageSettingRepo{}}}
			pause := func() { rules.paused[42][2] = struct{}{} }
			if phase == "preview" {
				pause()
			}
			preview, err := svc.PreviewOpenAIUserAccountDispatch(context.Background(), "owner", 42, 1)
			require.NoError(t, err)
			require.Len(t, preview.Targets, 1)
			if phase == "preview" {
				require.False(t, preview.Targets[0].Compatible)
				require.Equal(t, "user_account_paused", preview.Targets[0].Reason)
				return
			}
			require.True(t, preview.Targets[0].Compatible)
			if phase == "create" {
				pause()
			} else {
				cache.onReserve = pause
			}
			op, err := svc.CreateOpenAIUserAccountDispatch(context.Background(), "owner", preview.PreviewID, 2, "fixture-idempotency")
			if phase == "create" {
				require.Error(t, err)
				require.Nil(t, op)
				require.Nil(t, cache.op)
				return
			}
			require.NoError(t, err)
			require.Equal(t, []bool{false}, cache.eligible)
			require.Equal(t, "user_account_paused", op.Sessions[0].Reason)
			require.Equal(t, 0, op.Counts.Rebound)
		})
	}
}
