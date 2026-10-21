//go:build unit

package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type pauseSlotRuleRepository struct {
	service.AccountUserPauseRepository
	paused atomic.Bool
	err    error
}

func (r *pauseSlotRuleRepository) IsPaused(context.Context, int64, int64) (bool, error) {
	return r.paused.Load(), r.err
}
func (r *pauseSlotRuleRepository) PausedAccountIDs(context.Context, int64) (map[int64]struct{}, error) {
	return map[int64]struct{}{}, r.err
}

type pauseSlotAccountRepository struct{ service.AccountRepository }

func (pauseSlotAccountRepository) ListSchedulableByGroupIDAndPlatform(context.Context, int64, string) ([]service.Account, error) {
	return []service.Account{{ID: 1, Platform: service.PlatformOpenAI, Status: service.StatusActive, Schedulable: true}, {ID: 2, Platform: service.PlatformOpenAI, Status: service.StatusActive, Schedulable: true}}, nil
}

type pauseSlotConcurrencyCache struct {
	fakeConcurrencyCache
	calls      atomic.Int64
	releases   atomic.Int64
	waits      atomic.Int64
	decrements atomic.Int64
	queue      bool
	rules      *pauseSlotRuleRepository
}

func (c *pauseSlotConcurrencyCache) AcquireAccountSlot(context.Context, int64, int, string) (bool, error) {
	n := c.calls.Add(1)
	return !c.queue || n > 1, nil
}
func (c *pauseSlotConcurrencyCache) ReleaseAccountSlot(context.Context, int64, string) error {
	c.releases.Add(1)
	return nil
}
func (c *pauseSlotConcurrencyCache) IncrementAccountWaitCount(context.Context, int64, int) (bool, error) {
	c.waits.Add(1)
	c.rules.paused.Store(true)
	return true, nil
}
func (c *pauseSlotConcurrencyCache) DecrementAccountWaitCount(context.Context, int64) error {
	c.decrements.Add(1)
	return nil
}

func newPauseSlotGateway(rules *pauseSlotRuleRepository) *service.OpenAIGatewayService {
	gw := service.NewOpenAIGatewayService(pauseSlotAccountRepository{}, nil, nil, nil, nil, nil, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	gw.SetAccountUserPauseService(service.NewAccountUserPauseService(rules, nil))
	return gw
}

func TestAccountUserPauseSlotRechecksAllThreeAdmissionPaths(t *testing.T) {
	for _, mode := range []string{"scheduler-acquired", "quick-acquired", "queued"} {
		t.Run(mode, func(t *testing.T) {
			rules := &pauseSlotRuleRepository{}
			rules.paused.Store(mode != "queued")
			cache := &pauseSlotConcurrencyCache{queue: mode == "queued", rules: rules}
			gw := newPauseSlotGateway(rules)
			h := &OpenAIGatewayHandler{gatewayService: gw, concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatClaude, 0)}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			ctx := context.WithValue(context.Background(), ctxkey.UserID, int64(42))
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(service.WithOpenAIHTTPAccountPauseScope(ctx))
			selection := &service.AccountSelectionResult{Account: profitSlotTestAccount(1, 0.3), WaitPlan: &service.AccountWaitPlan{AccountID: 1, MaxConcurrency: 2, Timeout: time.Second, MaxWaiting: 2}}
			if mode == "scheduler-acquired" {
				selection.Acquired = true
				selection.ReleaseFunc = func() { cache.releases.Add(1) }
			}
			stream := false
			group := int64(7)
			release, result := h.acquireResponsesAccountSlot(c, &group, "", selection, false, &stream, zap.NewNop())
			require.Nil(t, release)
			require.Equal(t, openAISlotAcquirePauseVetoed, result)
			require.Equal(t, int64(1), cache.releases.Load())
			require.Empty(t, w.Body.String())
			if mode == "queued" {
				require.Equal(t, int64(1), cache.waits.Load())
				require.Equal(t, int64(1), cache.decrements.Load())
			}
		})
	}
}

func TestAccountUserPauseSlotStorageErrorReleasesAndReturns503(t *testing.T) {
	rules := &pauseSlotRuleRepository{err: errors.New("read unavailable")}
	gw := newPauseSlotGateway(rules)
	h := &OpenAIGatewayHandler{gatewayService: gw}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	ctx := service.WithOpenAIHTTPAccountPauseScope(context.WithValue(context.Background(), ctxkey.UserID, int64(42)))
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	var released atomic.Int64
	selection := &service.AccountSelectionResult{Account: profitSlotTestAccount(1, 0.3), Acquired: true, ReleaseFunc: func() { released.Add(1) }}
	group := int64(7)
	stream := false
	_, result := h.acquireResponsesAccountSlot(c, &group, "", selection, false, &stream, zap.NewNop())
	require.Equal(t, openAISlotAcquireFailed, result)
	require.Equal(t, int64(1), released.Load())
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Contains(t, w.Body.String(), "PAUSE_POLICY_UNAVAILABLE")
}

type pauseHTTPRules struct {
	service.AccountUserPauseRepository
	mu               sync.Mutex
	paused           map[int64]map[int64]bool
	pauseAtAdmission bool
}

func (r *pauseHTTPRules) PausedAccountIDs(_ context.Context, uid int64) (map[int64]struct{}, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := map[int64]struct{}{}
	for id, paused := range r.paused[uid] {
		if paused {
			ids[id] = struct{}{}
		}
	}
	return ids, nil
}

func (r *pauseHTTPRules) IsPaused(_ context.Context, aid, uid int64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pauseAtAdmission && aid == 1 && uid == 42 {
		r.paused[uid][aid] = true
	}
	return r.paused[uid][aid], nil
}

type pauseHTTPUpstream struct {
	service.HTTPUpstream
	base       *url.URL
	mu         sync.Mutex
	accountIDs []int64
}

func (u *pauseHTTPUpstream) Do(req *http.Request, _ string, aid int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, aid)
	u.mu.Unlock()
	copy := req.Clone(req.Context())
	copy.URL.Scheme, copy.URL.Host = u.base.Scheme, u.base.Host
	copy.URL.Path = strings.TrimRight(u.base.Path, "/") + req.URL.Path
	copy.Host = u.base.Host
	return http.DefaultClient.Do(copy)
}

// TestAccountUserPauseHTTPRoutesAdmittedAccount 通过真实 HTTP 入站、选号和外呼校验落点；可指向本地 Postman simulation 重放。
func TestAccountUserPauseHTTPRoutesAdmittedAccount(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, late := range []bool{false, true} {
			t.Run("stream="+strconv.FormatBool(stream)+"/pause_at_admission="+strconv.FormatBool(late), func(t *testing.T) {
				upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					require.Equal(t, "/backend-api/codex/responses", r.URL.Path)
					require.Contains(t, []string{"Bearer token-1", "Bearer token-2"}, r.Header.Get("Authorization"))
					response := queuedCapacityHandlerSuccess()
					w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
					_, _ = io.Copy(w, response.Body)
				}))
				defer upstreamServer.Close()
				base := upstreamServer.URL
				if simulated := os.Getenv("TEST_OPENAI_PAUSE_UPSTREAM_URL"); simulated != "" {
					base = simulated
				}
				baseURL, err := url.Parse(base)
				require.NoError(t, err)
				upstream := &pauseHTTPUpstream{base: baseURL}
				h := newOpenAIResponsesFailoverTestHandler(t, upstream)
				h.maxAccountSwitches = 0
				rules := &pauseHTTPRules{paused: map[int64]map[int64]bool{42: {1: !late}}, pauseAtAdmission: late}
				h.gatewayService.SetAccountUserPauseService(service.NewAccountUserPauseService(rules, nil))
				engine := gin.New()
				engine.POST("/v1/responses", func(c *gin.Context) {
					uid, _ := strconv.ParseInt(c.GetHeader("X-Test-User-ID"), 10, 64)
					key, _ := strconv.ParseInt(c.GetHeader("X-Test-Key-ID"), 10, 64)
					gid := int64(7)
					c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: key, UserID: uid, GroupID: &gid, User: &service.User{ID: uid, Status: service.StatusActive}, Group: &service.Group{ID: gid, Platform: service.PlatformOpenAI, Status: service.StatusActive}})
					c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: uid})
					h.Responses(c)
				})
				server := httptest.NewServer(engine)
				defer server.Close()
				for _, identity := range []struct{ uid, key int64 }{{42, 101}, {42, 102}, {43, 103}} {
					req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-6.1-sol","input":"hello","stream":`+strconv.FormatBool(stream)+`}`))
					require.NoError(t, err)
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("X-Test-User-ID", strconv.FormatInt(identity.uid, 10))
					req.Header.Set("X-Test-Key-ID", strconv.FormatInt(identity.key, 10))
					res, err := server.Client().Do(req)
					require.NoError(t, err)
					body, err := io.ReadAll(res.Body)
					res.Body.Close()
					require.NoError(t, err)
					require.Equal(t, http.StatusOK, res.StatusCode, string(body))
					require.Contains(t, string(body), "recovered")
				}
				require.Equal(t, []int64{2, 2, 1}, upstream.accountIDs, "同一用户的两把 key 均避开 A，其他用户仍可使用 A；暂停不消耗上游切号预算")
			})
		}
	}
}

func TestAccountUserPausePolicyFailureKeepsLocalAttribution(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	service.SetOpsUpstreamError(c, http.StatusBadGateway, "previous provider failure", "detail")
	h := &OpenAIGatewayHandler{}
	require.True(t, h.handleOpenAIAccountPausePolicyError(c, service.ErrOpenAIAccountPausePolicyUnavailable, false, false))
	require.Zero(t, c.GetInt(service.OpsUpstreamStatusCodeKey))
	require.Empty(t, c.GetString(service.OpsUpstreamErrorMessageKey))
	errType, code := service.OpsLocalErrorType(c)
	require.Equal(t, "api_error", errType)
	require.Equal(t, "PAUSE_POLICY_UNAVAILABLE", code)
}

func TestAccountUserPauseAsyncImageExecutionReadsCurrentRules(t *testing.T) {
	upstream := &openAIImagesFailoverHTTPUpstream{}
	h := newOpenAIResponsesFailoverTestHandler(t, upstream)
	h.maxAccountSwitches = 0
	rules := &pauseHTTPRules{paused: map[int64]map[int64]bool{42: {}}}
	h.gatewayService.SetAccountUserPauseService(service.NewAccountUserPauseService(rules, nil))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	body := []byte(`{"model":"gpt-image-1","prompt":"fixture image"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations/async", strings.NewReader(string(body))).WithContext(requestCtx)
	c.Request.Header.Set("Content-Type", "application/json")
	group := int64(7)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 9, UserID: 42, GroupID: &group, User: &service.User{ID: 42, Status: service.StatusActive}, Group: &service.Group{ID: group, Platform: service.PlatformOpenAI, AllowImageGeneration: true}})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 42})
	taskCtx, recorder, cancelTask := newAsyncImageContext(c, body, time.Second*5)
	defer cancelTask()
	// 提交上下文分离后才暂停 A，worker 必须读取执行时规则，不能冻结提交时的名单。
	rules.paused[42][1] = true
	cancelRequest()
	require.NoError(t, taskCtx.Request.Context().Err())
	h.Images(taskCtx)
	require.NotEmpty(t, upstream.calls(), recorder.Body.String())
	for _, id := range upstream.calls() {
		require.Equal(t, int64(2), id, "异步执行保留认证身份，并避开提交后新增暂停的账号 A")
	}
}
