package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

type pauseRoutingRepository struct {
	AccountUserPauseRepository
	mu           sync.Mutex
	paused       map[int64]map[int64]struct{}
	pauseOnCheck map[int64]bool
	listErr      error
	checkErr     error
	reads        int
	checks       []int64
}

func (r *pauseRoutingRepository) PausedAccountIDs(_ context.Context, uid int64) (map[int64]struct{}, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads++
	ids := make(map[int64]struct{})
	for id := range r.paused[uid] {
		ids[id] = struct{}{}
	}
	return ids, r.listErr
}

func (r *pauseRoutingRepository) IsPaused(_ context.Context, aid, uid int64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks = append(r.checks, aid)
	if r.pauseOnCheck[aid] {
		if r.paused == nil {
			r.paused = make(map[int64]map[int64]struct{})
		}
		if r.paused[uid] == nil {
			r.paused[uid] = make(map[int64]struct{})
		}
		r.paused[uid][aid] = struct{}{}
	}
	_, paused := r.paused[uid][aid]
	return paused, r.checkErr
}

type pauseRoutingAccountRepository struct{ schedulerTestOpenAIAccountRepo }

func (r pauseRoutingAccountRepository) ListByGroup(context.Context, int64) ([]Account, error) {
	return append([]Account(nil), r.accounts...), nil
}

func pauseRoutingContext(uid int64, http bool) context.Context {
	ctx := context.WithValue(context.Background(), ctxkey.UserID, uid)
	if http {
		ctx = WithOpenAIHTTPAccountPauseScope(ctx)
	}
	return ctx
}

func pauseRoutingAccounts() []Account {
	accounts := make([]Account, 4)
	for i := range accounts {
		accounts[i] = Account{ID: int64(i + 1), Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 2, Priority: i, GroupIDs: []int64{1}, Extra: map[string]any{"openai_apikey_responses_websockets_v2_enabled": true}}
	}
	return accounts
}

func pauseRoutingService(repo *pauseRoutingRepository, accounts []Account, advanced bool) *OpenAIGatewayService {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.StickyResponseIDTTLSeconds = 3600
	flag := "false"
	if advanced {
		flag = "true"
	}
	return &OpenAIGatewayService{
		accountRepo:       pauseRoutingAccountRepository{schedulerTestOpenAIAccountRepo{accounts: accounts}},
		accountUserPauses: NewAccountUserPauseService(repo, nil),
		cfg:               cfg, cache: &schedulerTestGatewayCache{},
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService(flag),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
}

func TestAccountUserPauseHTTPSelectionCoversPreviousStickyAndBothSchedulers(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "advanced"}[advanced], func(t *testing.T) {
			repo := &pauseRoutingRepository{paused: map[int64]map[int64]struct{}{42: {1: {}, 2: {}, 3: {}}}}
			svc := pauseRoutingService(repo, pauseRoutingAccounts(), advanced)
			ctx := pauseRoutingContext(42, true)
			group := int64(1)
			require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(ctx, group, "resp_paused", 1, time.Hour))
			require.NoError(t, svc.cache.SetSessionAccountID(ctx, group, "session-paused", 2, time.Hour))
			excluded := map[int64]struct{}{99: {}}
			selection, _, err := svc.SelectAccountWithScheduler(ctx, &group, "resp_paused", "session-paused", "", excluded, OpenAIUpstreamTransportAny, false)
			require.NoError(t, err)
			require.Equal(t, int64(4), selection.Account.ID)
			require.Equal(t, map[int64]struct{}{99: {}}, excluded, "must not mutate caller's upstream failure set")
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
			require.Equal(t, 1, repo.reads, "one snapshot is reused inside the selection stack")
		})
	}
}

func TestAccountUserPauseIsUserScopedAndDoesNotApplyToWebSocket(t *testing.T) {
	for _, tc := range []struct {
		name  string
		uid   int64
		http  bool
		want  int64
		reads int
	}{
		{"HTTP paused user", 42, true, 2, 1}, {"other user's key", 43, true, 1, 1}, {"client WebSocket", 42, false, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &pauseRoutingRepository{paused: map[int64]map[int64]struct{}{42: {1: {}}}}
			svc := pauseRoutingService(repo, pauseRoutingAccounts(), false)
			group := int64(1)
			account, err := svc.SelectAccountForModelWithExclusions(pauseRoutingContext(tc.uid, tc.http), &group, "", "", nil)
			require.NoError(t, err)
			require.Equal(t, tc.want, account.ID)
			require.Equal(t, tc.reads, repo.reads)
		})
	}
	ctx := pauseRoutingContext(42, true)
	svc := &OpenAIGatewayService{}
	require.NoError(t, svc.CheckOpenAIAccountUserPause(ctx, &Account{ID: 1, Platform: PlatformGrok}), "other providers do not acquire a pause dependency")
}

func TestAccountUserPauseFinalRecheckIsFiniteAndPreservesLaterDynamicSelection(t *testing.T) {
	repo := &pauseRoutingRepository{pauseOnCheck: map[int64]bool{1: true, 2: true, 3: true}}
	accounts := pauseRoutingAccounts()
	svc := pauseRoutingService(repo, accounts, false)
	ctx := pauseRoutingContext(42, true)
	group := int64(1)
	account, err := svc.SelectAccountForModelWithExclusions(ctx, &group, "", "", nil)
	require.NoError(t, err)
	require.Equal(t, int64(4), account.ID)
	require.Equal(t, []int64{1, 2, 3, 4}, repo.checks, "three concurrent pause vetoes must still find the fourth account")
	added := accounts[0]
	added.ID = 5
	added.Priority = -1
	svc.accountRepo = pauseRoutingAccountRepository{schedulerTestOpenAIAccountRepo{accounts: append(accounts, added)}}
	account, err = svc.SelectAccountForModelWithExclusions(ctx, &group, "", "", nil)
	require.NoError(t, err)
	require.Equal(t, int64(5), account.ID, "a completed pause stage cannot freeze later ordinary retries")
}

func TestAccountUserPauseStorageErrorsNeverBecomeCapacityOrFailOpen(t *testing.T) {
	for _, onCheck := range []bool{false, true} {
		repo := &pauseRoutingRepository{}
		if onCheck {
			repo.checkErr = errors.New("database unavailable")
		} else {
			repo.listErr = errors.New("database unavailable")
		}
		svc := pauseRoutingService(repo, pauseRoutingAccounts(), false)
		group := int64(1)
		account, err := svc.SelectAccountForModelWithExclusions(pauseRoutingContext(42, true), &group, "", "", nil)
		require.Nil(t, account)
		require.True(t, IsOpenAIAccountPausePolicyError(err))
		require.NotErrorIs(t, err, ErrNoAvailableAccounts)
	}
	svc := pauseRoutingService(&pauseRoutingRepository{}, pauseRoutingAccounts(), false)
	group := int64(1)
	_, err := svc.SelectAccountForModelWithExclusions(pauseRoutingContext(0, true), &group, "", "", nil)
	require.True(t, IsOpenAIAccountPausePolicyError(err), "marked HTTP requests cannot silently lose their authenticated identity")
}

func TestAccountUserPausePinnedSourcesAreFilteredAndReadFailureCannotFallback(t *testing.T) {
	repo := &pauseRoutingRepository{paused: map[int64]map[int64]struct{}{42: {1: {}}}}
	svc := pauseRoutingService(repo, pauseRoutingAccounts(), false)
	group := &Group{ID: 1, Platform: PlatformOpenAI}
	group.CodexModelsManifestConfig.Enabled = true
	group.CodexModelsManifestConfig.AccountIDs = []int64{1, 2}
	group.CodexModelsManifestConfig.FallbackToScheduler = true
	var fetched []int64
	results, err := svc.fetchPinnedOpenAIModels(pauseRoutingContext(42, true), group, func(_ context.Context, a *Account) (*OpenAIModelsResponse, error) {
		fetched = append(fetched, a.ID)
		return &OpenAIModelsResponse{Body: []byte(`{"data":[]}`)}, nil
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, []int64{2}, fetched)
	repo.listErr = errors.New("pause table read failed")
	_, _, err = svc.FetchPinnedOpenAIModelsList(pauseRoutingContext(42, true), group, 3, "")
	require.True(t, IsOpenAIAccountPausePolicyError(err))
}
