package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type pauseChurningRules struct {
	AccountUserPauseRepository
	checks []int64
}

func (r *pauseChurningRules) PausedAccountIDs(context.Context, int64) (map[int64]struct{}, error) {
	return map[int64]struct{}{}, nil
}
func (r *pauseChurningRules) IsPaused(_ context.Context, aid, _ int64) (bool, error) {
	r.checks = append(r.checks, aid)
	return aid <= 4, nil
}

type pauseChangingGroupAccounts struct {
	schedulerTestOpenAIAccountRepo
	lists int
}

func (r *pauseChangingGroupAccounts) GetByID(ctx context.Context, id int64) (*Account, error) {
	if id == 5 && r.lists >= 3 {
		added := r.accounts[0]
		added.ID = 5
		added.Priority = -2
		return &added, nil
	}
	return r.schedulerTestOpenAIAccountRepo.GetByID(ctx, id)
}
func (r *pauseChangingGroupAccounts) ListSchedulableByGroupIDAndPlatform(ctx context.Context, gid int64, platform string) ([]Account, error) {
	r.lists++
	accounts := append([]Account(nil), r.accounts...)
	if r.lists >= 3 {
		added := accounts[0]
		added.ID = 5
		added.Priority = -2
		accounts = append(accounts, added)
	}
	var result []Account
	for _, a := range accounts {
		if a.Platform == platform && openAIStickyAccountMatchesGroup(&a, &gid) {
			result = append(result, a)
		}
	}
	return result, nil
}

func TestAccountUserPauseFiniteChurnAndGroupCannotExpand(t *testing.T) {
	rules := &pauseChurningRules{}
	accounts := pauseRoutingAccounts()
	other := accounts[0]
	other.ID = 9
	other.Priority = -3
	other.GroupIDs = []int64{2}
	accounts = append(accounts, other)
	repo := &pauseChangingGroupAccounts{schedulerTestOpenAIAccountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}}
	svc := pauseRoutingService(&pauseRoutingRepository{}, accounts, false)
	svc.accountRepo = repo
	svc.accountUserPauses = NewAccountUserPauseService(rules, nil)
	ctx := pauseRoutingContext(42, true)
	group := int64(1)
	a, err := svc.SelectAccountForModelWithExclusions(ctx, &group, "", "", nil)
	require.Nil(t, a)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.Equal(t, []int64{1, 2, 3, 4}, rules.checks, "pause/resume churn must progress over fixed group candidates, excluding newly-added 5 and foreign group 9")
	otherGroup := int64(2)
	a, err = svc.SelectAccountForModelWithExclusions(ctx, &otherGroup, "", "", nil)
	require.Nil(t, a)
	require.ErrorIs(t, err, ErrNoAvailableAccounts, "a pause reselection cannot switch to a different group")
	a, err = svc.SelectAccountForModelWithExclusions(pauseRoutingContext(42, true), &group, "", "", nil)
	require.NoError(t, err)
	require.Equal(t, int64(5), a.ID, "a new request can use new dynamic candidates")
	t.Log("all four final vetoes terminate despite empty/churning snapshot; group cannot expand; a new request sees newly-added candidate 5")
}

func TestAccountUserPauseCompatibleProvidersHaveNoPolicyDependency(t *testing.T) {
	for _, platform := range domain.PlatformIDsWhere(func(spec domain.PlatformSpec) bool {
		return spec.Gateway == domain.PlatformGatewayOpenAI && spec.ID != PlatformOpenAI
	}) {
		t.Run(platform, func(t *testing.T) {
			rules := &pauseRoutingRepository{listErr: errors.New("unavailable"), checkErr: errors.New("unavailable")}
			a := pauseRoutingAccounts()[0]
			a.Platform = platform
			svc := pauseRoutingService(rules, []Account{a}, false)
			group := int64(1)
			selected, err := svc.SelectAccountForTokenCount(pauseRoutingContext(42, true), &group, "", "", "", platform)
			require.NoError(t, err)
			require.Equal(t, a.ID, selected.ID)
			require.Zero(t, rules.reads)
			require.Empty(t, rules.checks)
		})
	}
}

func TestAccountUserPausePinnedFinalReadFailureWins(t *testing.T) {
	rules := &pauseRoutingRepository{checkErr: errors.New("database lost after snapshot")}
	svc := pauseRoutingService(rules, pauseRoutingAccounts(), false)
	group := &Group{ID: 1, Platform: PlatformOpenAI}
	group.CodexModelsManifestConfig.Enabled = true
	group.CodexModelsManifestConfig.AccountIDs = []int64{1, 2}
	group.CodexModelsManifestConfig.FallbackToScheduler = true
	var calls atomic.Int64
	_, err := svc.fetchPinnedOpenAIModels(pauseRoutingContext(42, true), group, func(context.Context, *Account) (*OpenAIModelsResponse, error) {
		calls.Add(1)
		return &OpenAIModelsResponse{Body: []byte(`{"data":[]}`)}, nil
	})
	require.True(t, IsOpenAIAccountPausePolicyError(err))
	require.Zero(t, calls.Load())
	_, _, err = svc.FetchPinnedOpenAIModelsList(pauseRoutingContext(42, true), group, 3, "")
	require.True(t, IsOpenAIAccountPausePolicyError(err))
	require.Equal(t, 2, rules.reads, "each call performs exactly one initial snapshot; policy failure never enters scheduler fallback")
	t.Log("pinned per-source final read failure prevents external calls and remains PAUSE_POLICY_UNAVAILABLE with scheduler fallback enabled")
}

type pauseLiveFailingUpstream struct {
	HTTPUpstream
	rules *pauseRoutingRepository
	calls int
}

func (u *pauseLiveFailingUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	u.rules.mu.Lock()
	u.rules.listErr = errors.New("policy unavailable after upstream failure")
	u.rules.mu.Unlock()
	return &http.Response{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("fixture upstream unavailable"))}, nil
}
func (u *pauseLiveFailingUpstream) DoWithTLS(r *http.Request, p string, aid int64, c int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(r, p, aid, c)
}

func TestAccountUserPauseLivePolicyErrorBeforeAndAfterUpstream(t *testing.T) {
	for _, phase := range []string{"snapshot", "admission", "after_upstream_failure"} {
		t.Run(phase, func(t *testing.T) {
			rules := &pauseRoutingRepository{}
			if phase == "snapshot" {
				rules.listErr = errors.New("fixture unavailable")
			}
			if phase == "admission" {
				rules.checkErr = errors.New("fixture unavailable")
			}
			accounts := pauseRoutingAccounts()
			for i := range accounts {
				accounts[i].Type = AccountTypeOAuth
				accounts[i].Credentials = map[string]any{"access_token": "fixture"}
			}
			svc := pauseRoutingService(rules, accounts, false)
			svc.cache = &liveTestStore{GatewayCache: &schedulerTestGatewayCache{}}
			var releases []int64
			concurrency := &pauseLiveConcurrencyCache{schedulerTestConcurrencyCache: schedulerTestConcurrencyCache{releasedIDs: &releases}}
			svc.concurrencyService = NewConcurrencyService(concurrency)
			svc.liveAttestation = liveAttestationStub{header: `{"v":1,"t":"fixture"}`}
			svc.liveAttestationCipher = newLiveAttestationCipher(&config.Config{JWT: config.JWTConfig{Secret: "fixture-only"}})
			upstream := &pauseLiveFailingUpstream{rules: rules}
			svc.httpUpstream = upstream
			group := int64(1)
			created, err := svc.CreateLiveCall(pauseRoutingContext(42, true), &LiveCallRequest{SDP: "v=0\r\n", Session: json.RawMessage(`{"model":"gpt-live"}`)}, LiveCallIdentity{UserID: 42, APIKeyID: 7, GroupID: &group}, 10)
			require.Nil(t, created)
			require.True(t, IsOpenAIAccountPausePolicyError(err))
			require.ElementsMatch(t, concurrency.leases, concurrency.releasedLeases)
			require.ElementsMatch(t, concurrency.leases, releases)
			if phase == "after_upstream_failure" {
				require.Equal(t, 1, upstream.calls)
			} else {
				require.Zero(t, upstream.calls)
			}
			t.Logf("phase=%s policy 503 wins; upstream calls=%d; leases and slots balanced", phase, upstream.calls)
		})
	}
}

type pausePartialDispatchCache struct {
	pauseDispatchCache
	rules *pauseRoutingRepository
}

func (c *pausePartialDispatchCache) RebindOpenAIDispatchSession(ctx context.Context, record *OpenAIDispatchOperationRecord, i int, ttl, legacy time.Duration, rev string, eligible bool) error {
	err := c.pauseDispatchCache.RebindOpenAIDispatchSession(ctx, record, i, ttl, legacy, rev, eligible)
	if i == 0 {
		c.rules.checkErr = errors.New("fixture lost rules after first committed item")
	}
	return err
}

func TestAccountUserPauseDispatchPolicyFailureRetainsPriorResult(t *testing.T) {
	rules := &pauseRoutingRepository{}
	svc := pauseRoutingService(rules, pauseRoutingAccounts()[:2], false)
	scope1 := OpenAIStickyScope{GroupID: 1, Hash: "round2-session-1"}
	scope2 := OpenAIStickyScope{GroupID: 1, Hash: "round2-session-2"}
	cache := &pausePartialDispatchCache{pauseDispatchCache: pauseDispatchCache{dispatchPreviewCoverageCache: dispatchPreviewCoverageCache{
		scopes: []OpenAIStickyScope{scope1, scope2}, snapshots: map[observedAccountSession]OpenAIStickySnapshot{
			{groupID: 1, hash: scope1.Hash}: {AccountID: 1, OwnerID: 42, Supported: true},
			{groupID: 1, hash: scope2.Hash}: {AccountID: 1, OwnerID: 42, Supported: true},
		},
	}}, rules: rules}
	svc.cache = cache
	svc.userRepo = dispatchPreviewCoverageUserRepo{}
	svc.rateLimitService = &RateLimitService{settingService: &SettingService{settingRepo: dispatchPreviewCoverageSettingRepo{}}}
	preview, err := svc.PreviewOpenAIUserAccountDispatch(context.Background(), "owner", 42, 1)
	require.NoError(t, err)
	require.Len(t, preview.Sessions, 2)
	op, err := svc.CreateOpenAIUserAccountDispatch(context.Background(), "owner", preview.PreviewID, 2, "round2-failure")
	require.Nil(t, op)
	var dispatchErr *OpenAIDispatchError
	require.ErrorAs(t, err, &dispatchErr)
	require.Equal(t, 503, dispatchErr.Status)
	require.Equal(t, "DISPATCH_STORE_UNAVAILABLE", dispatchErr.Code)
	require.NotEmpty(t, dispatchErr.OperationID)
	require.Equal(t, []bool{true}, cache.eligible, "no next mutation may occur after a rule read failure")
	status, err := svc.StatusOpenAIUserAccountDispatch(context.Background(), "owner", dispatchErr.OperationID)
	require.NoError(t, err)
	require.Equal(t, 1, status.Counts.Rebound)
	require.Equal(t, "rebound", status.Sessions[0].RebindResult)
	require.Equal(t, "not_processed", status.Sessions[1].RebindResult)
	t.Log("dispatch policy failure prevents second write and returns original operation id; status still reports completed first item")
}
