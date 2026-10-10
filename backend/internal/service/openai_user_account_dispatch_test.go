package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type dispatchPreviewCoverageCache struct {
	GatewayCache
	OpenAIUserDispatchCache
	scopes    []OpenAIStickyScope
	snapshots map[observedAccountSession]OpenAIStickySnapshot
	saved     *OpenAIDispatchPreviewRecord
	reads     int
}

func (c *dispatchPreviewCoverageCache) ListOpenAIStickyUserBindings(context.Context, int64, int64) ([]OpenAIStickyScope, bool, error) {
	return c.scopes, false, nil
}

func (c *dispatchPreviewCoverageCache) ReadOpenAIStickySnapshot(_ context.Context, scope OpenAIStickyScope, _ string) (*OpenAIStickySnapshot, error) {
	c.reads++
	snapshot := c.snapshots[observedAccountSession{groupID: scope.GroupID, hash: scope.Hash}]
	snapshot.Scope = scope
	return &snapshot, nil
}

func (c *dispatchPreviewCoverageCache) SaveOpenAIDispatchPreview(_ context.Context, record *OpenAIDispatchPreviewRecord) error {
	c.saved = record
	return nil
}

type dispatchPreviewCoverageUserRepo struct{ UserRepository }

func (dispatchPreviewCoverageUserRepo) GetByID(_ context.Context, id int64) (*User, error) {
	return &User{ID: id, Email: "coverage-test"}, nil
}

type dispatchPreviewCoverageSettingRepo struct{ SettingRepository }

func (dispatchPreviewCoverageSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	if key == SettingKeyOpenAIUserAccountDispatchEnabled {
		return "true", nil
	}
	return "", ErrSettingNotFound
}

func TestOpenAIDispatchPreviewShowsUnmatchedInFlightWithoutChangingCASCoverage(t *testing.T) {
	known := OpenAIStickyScope{GroupID: 91, Hash: "known", LegacyHash: "legacy-known"}
	duplicate := known
	duplicate.LegacyHash = "another-legacy-reference"
	skipped := OpenAIStickyScope{GroupID: 91, Hash: "known-skipped", LegacyHash: "legacy-skipped"}
	cache := &dispatchPreviewCoverageCache{scopes: []OpenAIStickyScope{known, duplicate, skipped}, snapshots: map[observedAccountSession]OpenAIStickySnapshot{
		{groupID: 91, hash: "known"}:         {AccountID: 1, OwnerID: 42, Supported: true},
		{groupID: 91, hash: "known-skipped"}: {AccountID: 1, OwnerID: 42, Supported: true, HasWS: true},
	}}
	concurrency := NewConcurrencyService(nil)
	for _, slot := range []struct {
		account, user, group int64
		hash                 string
	}{
		{1, 42, 91, "known"}, {1, 42, 91, "known"},
		{1, 42, 91, "known-skipped"},
		{1, 42, 91, "not-indexed"}, {1, 42, 91, "not-indexed"},
		{1, 42, 0, ""}, {1, 42, 0, ""},
		{1, 42, 92, "known"},
		{1, 43, 91, "known"}, {2, 42, 91, "not-indexed"},
	} {
		release := concurrency.observation.acquireSession(slot.account, slot.user, slot.group, slot.hash)
		t.Cleanup(release)
	}
	svc := &OpenAIGatewayService{
		cache: cache, concurrencyService: concurrency, userRepo: dispatchPreviewCoverageUserRepo{},
		accountRepo:      schedulerTestOpenAIAccountRepo{accounts: []Account{{ID: 1, Name: "A", Platform: PlatformOpenAI}}},
		rateLimitService: &RateLimitService{settingService: &SettingService{settingRepo: dispatchPreviewCoverageSettingRepo{}}},
	}
	for round := 0; round < 2; round++ {
		preview, err := svc.PreviewOpenAIUserAccountDispatch(context.Background(), "admin-test", 42, 1)
		require.NoError(t, err)
		require.Equal(t, 1, preview.Counts.Rebindable)
		require.Equal(t, int64(2), preview.Counts.InFlight, "汇总仍只计可处理会话的关联在途数")
		require.Len(t, preview.Sessions, 1)
		require.Equal(t, int64(2), preview.Sessions[0].InFlight)
		require.Len(t, cache.saved.Bindings, 1, "未登记槽位仅展示，不能进入管理 CAS")
		require.Equal(t, known.Hash, cache.saved.Bindings[0].Scope.Hash)
		require.Equal(t, (round+1)*2, cache.reads, "同物理键重复索引只读取和匹配一次")
		require.Equal(t, 4, preview.Counts.Skipped)
		var totalSkippedInFlight int64
		unmatchedByGroup := map[int64]int64{}
		for _, item := range preview.SkippedSessions {
			totalSkippedInFlight += item.InFlight
			if item.Reason == "protocol_unsupported" {
				require.Equal(t, int64(1), item.InFlight, "已知跳过项也消费槽位，不能再次列为未知")
				require.Equal(t, int64(1), item.CurrentBinding)
				continue
			}
			require.Equal(t, "not_registered_or_unmatched", item.RebindResult)
			require.Equal(t, "not_registered_or_unmatched", item.Reason)
			require.Equal(t, "unknown", item.CurrentBinding, "执行槽位不能推断仍有 A 粘性")
			unmatchedByGroup[item.GroupID] += item.InFlight
		}
		require.Equal(t, int64(6), totalSkippedInFlight, "其他用户和其他账号的槽位不能泄漏")
		require.Equal(t, map[int64]int64{0: 2, 91: 2, 92: 1}, unmatchedByGroup)
	}
}
