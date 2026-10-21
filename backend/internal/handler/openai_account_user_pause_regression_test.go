//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type pauseQueuedConcurrencyCache struct {
	fakeConcurrencyCache
	mu                sync.Mutex
	calls             map[int64]int
	releases          map[int64]int
	waits, decrements int
	rules             *pauseHTTPRules
}

func (q *pauseQueuedConcurrencyCache) AcquireAccountSlot(_ context.Context, aid int64, _ int, _ string) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.calls[aid]++
	// A 在选择与快抢时尚满载，真正排队后获得槽位。
	return aid != 1 || q.calls[aid] >= 3, nil
}
func (q *pauseQueuedConcurrencyCache) ReleaseAccountSlot(_ context.Context, aid int64, _ string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.releases[aid]++
	return nil
}
func (q *pauseQueuedConcurrencyCache) IncrementAccountWaitCount(_ context.Context, aid int64, _ int) (bool, error) {
	q.mu.Lock()
	q.waits++
	q.mu.Unlock()
	q.rules.mu.Lock()
	q.rules.paused[42][aid] = true
	q.rules.mu.Unlock()
	return true, nil
}
func (q *pauseQueuedConcurrencyCache) DecrementAccountWaitCount(context.Context, int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.decrements++
	return nil
}

func TestAccountUserPauseQueuedHTTPFinalAdmissionThenActualForward(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run("stream="+strconv.FormatBool(stream), func(t *testing.T) {
			upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/backend-api/codex/responses", r.URL.Path)
				require.Equal(t, "Bearer token-2", r.Header.Get("Authorization"))
				response := queuedCapacityHandlerSuccess()
				w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
				_, _ = io.Copy(w, response.Body)
			}))
			defer upstreamServer.Close()
			base, err := url.Parse(upstreamServer.URL)
			require.NoError(t, err)
			upstream := &pauseHTTPUpstream{base: base}
			h := newOpenAIResponsesFailoverTestHandler(t, upstream)
			h.maxAccountSwitches = 0
			rules := &pauseHTTPRules{paused: map[int64]map[int64]bool{42: {}}}
			cache := &pauseQueuedConcurrencyCache{calls: map[int64]int{}, releases: map[int64]int{}, rules: rules}
			concurrency := service.NewConcurrencyService(cache)
			cfg := &config.Config{RunMode: config.RunModeSimple}
			cfg.Gateway.Scheduling.FallbackWaitTimeout = time.Second
			cfg.Gateway.Scheduling.FallbackMaxWaiting = 3
			accounts := []service.Account{
				{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, Credentials: map[string]any{"access_token": "token-1"}},
				{ID: 2, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: 1, Credentials: map[string]any{"access_token": "token-2"}},
			}
			h.gatewayService = service.NewOpenAIGatewayService(openAIImagesFailoverAccountRepo{accounts: accounts}, nil, nil, nil, nil, nil, nil, cfg, nil, concurrency, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
			h.gatewayService.SetAccountUserPauseService(service.NewAccountUserPauseService(rules, nil))
			h.concurrencyHelper = NewConcurrencyHelper(concurrency, SSEPingFormatComment, 0)
			engine := gin.New()
			engine.POST("/v1/responses", func(c *gin.Context) {
				gid := int64(7)
				c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 101, UserID: 42, GroupID: &gid, User: &service.User{ID: 42, Status: service.StatusActive}, Group: &service.Group{ID: gid, Platform: service.PlatformOpenAI, Status: service.StatusActive}})
				c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 42})
				h.Responses(c)
			})
			server := httptest.NewServer(engine)
			defer server.Close()
			req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-6.1-sol","input":"hello","stream":`+strconv.FormatBool(stream)+`}`))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			res, err := server.Client().Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(res.Body)
			res.Body.Close()
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, res.StatusCode, string(body))
			require.Contains(t, string(body), "recovered")
			require.Equal(t, []int64{2}, upstream.accountIDs, "排队 A 被暂停后必须实际向 B 外呼")
			cache.mu.Lock()
			defer cache.mu.Unlock()
			require.Equal(t, 3, cache.calls[1], "A 必须经过选号抢槽、快速抢槽和真实排队三步")
			require.Equal(t, 1, cache.waits)
			require.Equal(t, cache.waits, cache.decrements)
			require.Equal(t, map[int64]int{1: 1, 2: 1}, cache.releases, "被否决 A 与已准入 B 的槽位各释放一次")
			t.Log("actual HTTP/SSE completed through B after queued A was paused; A never forwarded; wait and account slots balanced; zero upstream switch budget")
		})
	}
}

type pauseDynamicHTTPAccountRepository struct {
	service.AccountRepository
	mu       sync.Mutex
	accounts []service.Account
}

func (r *pauseDynamicHTTPAccountRepository) GetByID(_ context.Context, id int64) (*service.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.accounts {
		if a.ID == id {
			copy := a
			return &copy, nil
		}
	}
	return nil, service.ErrNoAvailableAccounts
}
func (r *pauseDynamicHTTPAccountRepository) ListSchedulableByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []service.Account
	for _, a := range r.accounts {
		if a.Platform == platform {
			result = append(result, a)
		}
	}
	return result, nil
}
func (r *pauseDynamicHTTPAccountRepository) ListSchedulableByGroupIDAndPlatform(ctx context.Context, _ int64, platform string) ([]service.Account, error) {
	return r.ListSchedulableByPlatform(ctx, platform)
}
func (r *pauseDynamicHTTPAccountRepository) ListSchedulableUngroupedByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	return r.ListSchedulableByPlatform(ctx, platform)
}

func TestAccountUserPauseAdmittedThenRealFailoverUsesNewCandidate(t *testing.T) {
	accounts := &pauseDynamicHTTPAccountRepository{accounts: []service.Account{
		{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Priority: 0, Credentials: map[string]any{"access_token": "token-1"}},
		{ID: 2, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Priority: 1, Credentials: map[string]any{"access_token": "token-2"}},
	}}
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer token-2" {
			accounts.mu.Lock()
			if len(accounts.accounts) == 2 {
				added := accounts.accounts[1]
				added.ID = 3
				added.Priority = -1
				added.Credentials = map[string]any{"access_token": "token-3"}
				accounts.accounts = append(accounts.accounts, added)
			}
			accounts.mu.Unlock()
			w.WriteHeader(520)
			_, _ = w.Write([]byte("upstream fixture failure"))
			return
		}
		require.Equal(t, "Bearer token-3", r.Header.Get("Authorization"))
		response := queuedCapacityHandlerSuccess()
		w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
		_, _ = io.Copy(w, response.Body)
	}))
	defer upstreamServer.Close()
	base, err := url.Parse(upstreamServer.URL)
	require.NoError(t, err)
	upstream := &pauseHTTPUpstream{base: base}
	h := newOpenAIResponsesFailoverTestHandler(t, upstream)
	h.maxAccountSwitches = 1
	cfg := &config.Config{RunMode: config.RunModeSimple}
	h.gatewayService = service.NewOpenAIGatewayService(accounts, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
	rules := &pauseHTTPRules{paused: map[int64]map[int64]bool{42: {}}, pauseAtAdmission: true}
	h.gatewayService.SetAccountUserPauseService(service.NewAccountUserPauseService(rules, nil))
	c, w := newOpenAIResponsesFailoverTestContext(t, context.Background())
	apiKey, _ := middleware2.GetAPIKeyFromContext(c)
	apiKey.UserID = 42
	apiKey.User.ID = 42
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 42})
	h.Responses(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "recovered")
	require.Equal(t, []int64{2, 3}, upstream.accountIDs, "A 被终检否决、B 实际外呼失败后，新 C 在一次上游切号预算内成功")
	t.Log("real B HTTP 520 created a new higher-priority C; C actually forwarded after pause reselection completed; final veto did not consume the one upstream failover budget")
}
