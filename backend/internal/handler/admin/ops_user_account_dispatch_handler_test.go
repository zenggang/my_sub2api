package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type dispatchAPIAccountRepo struct {
	service.AccountRepository
	accounts []service.Account
}

func (r dispatchAPIAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			return &r.accounts[i], nil
		}
	}
	return nil, fmt.Errorf("missing account")
}
func (r dispatchAPIAccountRepo) ListSchedulableByPlatform(context.Context, string) ([]service.Account, error) {
	return r.accounts, nil
}
func (r dispatchAPIAccountRepo) ListSchedulableByGroupIDAndPlatform(context.Context, int64, string) ([]service.Account, error) {
	return r.accounts, nil
}
func (r dispatchAPIAccountRepo) ListSchedulableUngroupedByPlatform(context.Context, string) ([]service.Account, error) {
	return r.accounts, nil
}

type dispatchAPIUserRepo struct{ service.UserRepository }

func (dispatchAPIUserRepo) GetByID(context.Context, int64) (*service.User, error) {
	return &service.User{ID: 42, Email: "user-test"}, nil
}

func newDispatchAPI(t *testing.T) (*gin.Engine, *service.OpenAIGatewayService, service.GatewayCache) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	cfg.Gateway.OpenAIWS.SessionHashReadOldFallback = true
	cfg.Gateway.OpenAIWS.SessionHashDualWriteOld = true
	accounts := dispatchAPIAccountRepo{accounts: []service.Account{{ID: 1, Name: "A", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 10, Priority: 0, GroupIDs: []int64{91}}, {ID: 2, Name: "B", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 10, Priority: 1, GroupIDs: []int64{91}}}}
	settings := service.NewSettingService(newTestSettingRepo(), cfg)
	rate := service.NewRateLimitService(accounts, nil, cfg, nil, nil)
	rate.SetSettingService(settings)
	cache := repository.NewGatewayCache(rdb)
	concurrency := service.NewConcurrencyService(repository.NewConcurrencyCache(rdb, 30, 120))
	gateway := service.NewOpenAIGatewayService(accounts, nil, nil, dispatchAPIUserRepo{}, nil, nil, cache, cfg, nil, concurrency, nil, rate, nil, nil, nil, nil, nil, nil, nil, nil, settings, nil)
	h := NewOpsHandlerWithDispatch(nil, gateway)
	router := gin.New()
	// Harness 实际调用 Admin handler；只用本地固定身份代替外部 JWT 登录和数据库。
	router.Use(func(c *gin.Context) {
		if c.GetHeader("Test-Admin") != "absent" {
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
			c.Set(string(middleware.ContextKeyUserRole), service.RoleAdmin)
		}
	})
	router.POST("/preview", h.PreviewUserAccountDispatch)
	router.POST("/create", h.CreateUserAccountDispatch)
	router.GET("/status", h.GetUserAccountDispatchStatus)
	router.GET("/settings", h.GetUserAccountDispatchSettings)
	router.PUT("/settings", h.UpdateUserAccountDispatchSettings)
	return router, gateway, cache
}

func dispatchAPICall(t *testing.T, router *gin.Engine, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	router.ServeHTTP(recorder, request)
	return recorder
}
func dispatchAPIData(t *testing.T, recorder *httptest.ResponseRecorder, target any) {
	t.Helper()
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.NoError(t, json.Unmarshal(envelope.Data, target))
}
func dispatchRequest(t *testing.T, gateway *service.OpenAIGatewayService, sid, protocol string) (*gin.Context, string) {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("session_id", sid)
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.UserID, int64(42)))
	service.PrepareOpenAIStickyRequest(c, protocol)
	return c, gateway.GenerateSessionHash(c, nil)
}
func dispatchSelect(t *testing.T, gateway *service.OpenAIGatewayService, c *gin.Context, hash, previous string) *service.AccountSelectionResult {
	t.Helper()
	group := int64(91)
	selection, _, err := gateway.SelectAccountWithScheduler(c.Request.Context(), &group, previous, hash, "", nil, service.OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.True(t, selection.Acquired)
	return selection
}

func TestUserAccountDispatchAPIMainFlowIncludesIdleAndInFlightSessions(t *testing.T) {
	router, gateway, _ := newDispatchAPI(t)
	require.Equal(t, http.StatusConflict, dispatchAPICall(t, router, "POST", "/preview", `{"user_id":42,"source_account_id":1}`, nil).Code)
	var setting struct {
		Enabled bool `json:"enabled"`
	}
	dispatchAPIData(t, dispatchAPICall(t, router, "GET", "/settings", "", nil), &setting)
	require.False(t, setting.Enabled)
	dispatchAPIData(t, dispatchAPICall(t, router, "PUT", "/settings", `{"enabled":true}`, nil), &setting)
	require.True(t, setting.Enabled)
	firstSID, secondSID := uuid.NewString(), uuid.NewString()
	first, hash := dispatchRequest(t, gateway, firstSID, "http")
	firstSlot := dispatchSelect(t, gateway, first, hash, "")
	require.Equal(t, int64(1), firstSlot.Account.ID)
	second, secondHash := dispatchRequest(t, gateway, secondSID, "http")
	idleSlot := dispatchSelect(t, gateway, second, secondHash, "")
	idleSlot.ReleaseFunc()
	var preview service.OpenAIDispatchPreview
	dispatchAPIData(t, dispatchAPICall(t, router, "POST", "/preview", `{"user_id":42,"source_account_id":1}`, nil), &preview)
	require.Equal(t, 2, preview.Counts.Rebindable)
	require.Equal(t, int64(1), preview.Counts.InFlight)
	idempotency := uuid.NewString()
	body := fmt.Sprintf(`{"preview_id":%q,"target_account_id":2,"idempotency_key":%q}`, preview.PreviewID, idempotency)
	var op service.OpenAIDispatchOperation
	dispatchAPIData(t, dispatchAPICall(t, router, "POST", "/create", body, nil), &op)
	require.True(t, op.Accepted)
	require.Equal(t, 2, op.Counts.Rebound)
	// 点击前的 A 请求迟到写 C，不得覆盖 B；其准入回调也不能消费新操作。
	group := int64(91)
	require.NoError(t, gateway.BindStickySession(first.Request.Context(), &group, hash, 1))
	gateway.ObserveOpenAIDispatchAdmission(first.Request.Context(), 1, false)
	firstSlot.ReleaseFunc()
	var status service.OpenAIDispatchOperation
	dispatchAPIData(t, dispatchAPICall(t, router, "GET", "/status?operation_id="+op.OperationID, "", nil), &status)
	require.Equal(t, 2, status.Counts.Awaiting)
	for _, item := range status.Sessions {
		require.Equal(t, float64(2), item.CurrentBinding)
		require.NotEmpty(t, item.RebindRevision)
	}
	next, nextHash := dispatchRequest(t, gateway, firstSID, "http")
	nextSlot := dispatchSelect(t, gateway, next, nextHash, "")
	require.Equal(t, int64(2), nextSlot.Account.ID)
	gateway.ObserveOpenAIDispatchAdmission(next.Request.Context(), 2, false)
	nextSlot.ReleaseFunc()
	dispatchAPIData(t, dispatchAPICall(t, router, "POST", "/preview", `{"user_id":42,"source_account_id":2}`, nil), &preview)
	require.Equal(t, 2, preview.Counts.Rebindable, "空闲改绑会话立即进入目标索引")
	reverse := fmt.Sprintf(`{"preview_id":%q,"target_account_id":1,"idempotency_key":%q}`, preview.PreviewID, uuid.NewString())
	var op2 service.OpenAIDispatchOperation
	dispatchAPIData(t, dispatchAPICall(t, router, "POST", "/create", reverse, nil), &op2)
	require.Equal(t, 2, op2.Counts.Rebound)
	dispatchAPIData(t, dispatchAPICall(t, router, "PUT", "/settings", `{"enabled":false}`, nil), &setting)
	var retried service.OpenAIDispatchOperation
	dispatchAPIData(t, dispatchAPICall(t, router, "POST", "/create", body, nil), &retried)
	require.Equal(t, op.OperationID, retried.OperationID)
	require.Equal(t, 1, retried.Counts.AssignedTarget)
	for _, item := range retried.Sessions {
		require.Equal(t, float64(1), item.CurrentBinding, "重试必须保留反向操作后的当前绑定")
	}
}

func TestUserAccountDispatchAPIAuthenticationAndStrictInput(t *testing.T) {
	router, _, _ := newDispatchAPI(t)
	for _, scenario := range []struct {
		method, path, body string
		headers            map[string]string
		status             int
	}{
		{"GET", "/settings", "", map[string]string{"Test-Admin": "absent", "X-Quick-Ops-Owner": strings.Repeat("a", 64)}, http.StatusForbidden},
		{"GET", "/settings", "", map[string]string{"X-Quick-Ops-Owner": "invalid"}, http.StatusBadRequest},
		{"PUT", "/settings", `{"enabled":true,"account_id":2}`, nil, http.StatusBadRequest},
		{"PUT", "/settings", `{}`, nil, http.StatusBadRequest},
		{"PUT", "/settings", `{"enabled":true} {}`, nil, http.StatusBadRequest},
	} {
		recorder := dispatchAPICall(t, router, scenario.method, scenario.path, scenario.body, scenario.headers)
		require.Equal(t, scenario.status, recorder.Code, recorder.Body.String())
	}
}

func TestUserAccountDispatchGuardianSameSIDDoesNotConsumeMainObservation(t *testing.T) {
	router, gateway, _ := newDispatchAPI(t)
	require.NoError(t, gateway.SetOpenAIDispatchEnabled(context.Background(), true))
	sid := uuid.NewString()
	c, hash := dispatchRequest(t, gateway, sid, "http")
	slot := dispatchSelect(t, gateway, c, hash, "")
	slot.ReleaseFunc()
	var preview service.OpenAIDispatchPreview
	dispatchAPIData(t, dispatchAPICall(t, router, "POST", "/preview", `{"user_id":42,"source_account_id":1}`, nil), &preview)
	var op service.OpenAIDispatchOperation
	dispatchAPIData(t, dispatchAPICall(t, router, "POST", "/create", fmt.Sprintf(`{"preview_id":%q,"target_account_id":2,"idempotency_key":%q}`, preview.PreviewID, uuid.NewString()), nil), &op)
	guardian, guardianHash := dispatchRequest(t, gateway, sid, "http")
	guardian.Request.Header.Set("x-openai-subagent", "guardian")
	guardian.Request.Header.Set("x-codex-parent-thread-id", sid)
	guardian.Request = guardian.Request.WithContext(service.WithOpenAIGuardianParentAffinity(guardian.Request.Context(), guardian, nil, "codex-auto-review"))
	guardianSlot := dispatchSelect(t, gateway, guardian, guardianHash, "")
	gateway.ObserveOpenAIDispatchAdmission(guardian.Request.Context(), guardianSlot.Account.ID, false)
	guardianSlot.ReleaseFunc()
	var status service.OpenAIDispatchOperation
	dispatchAPIData(t, dispatchAPICall(t, router, "GET", "/status?operation_id="+op.OperationID, "", nil), &status)
	require.Equal(t, 1, status.Counts.Awaiting)
	main, mainHash := dispatchRequest(t, gateway, sid, "http")
	mainSlot := dispatchSelect(t, gateway, main, mainHash, "")
	gateway.ObserveOpenAIDispatchAdmission(main.Request.Context(), mainSlot.Account.ID, false)
	mainSlot.ReleaseFunc()
	dispatchAPIData(t, dispatchAPICall(t, router, "GET", "/status?operation_id="+op.OperationID, "", nil), &status)
	require.Equal(t, 1, status.Counts.AssignedTarget)
}

func TestUserAccountDispatchPreviousResponseCapturesBeforeEarlyReturn(t *testing.T) {
	router, gateway, cache := newDispatchAPI(t)
	require.NoError(t, gateway.SetOpenAIDispatchEnabled(context.Background(), true))
	sid := uuid.NewString()
	c, hash := dispatchRequest(t, gateway, sid, "http")
	slot := dispatchSelect(t, gateway, c, hash, "")
	slot.ReleaseFunc()
	owner := map[string]string{"X-Quick-Ops-Owner": strings.Repeat("a", 64)}
	var preview service.OpenAIDispatchPreview
	dispatchAPIData(t, dispatchAPICall(t, router, "POST", "/preview", `{"user_id":42,"source_account_id":1}`, owner), &preview)
	var op service.OpenAIDispatchOperation
	idempotency := uuid.NewString()
	body := fmt.Sprintf(`{"preview_id":%q,"target_account_id":2,"idempotency_key":%q}`, preview.PreviewID, idempotency)
	dispatchAPIData(t, dispatchAPICall(t, router, "POST", "/create", body, owner), &op)
	require.Equal(t, http.StatusForbidden, dispatchAPICall(t, router, "GET", "/status?operation_id="+op.OperationID, "", map[string]string{"X-Quick-Ops-Owner": strings.Repeat("b", 64)}).Code)
	conflict := fmt.Sprintf(`{"preview_id":%q,"target_account_id":1,"idempotency_key":%q}`, preview.PreviewID, idempotency)
	require.Equal(t, http.StatusConflict, dispatchAPICall(t, router, "POST", "/create", conflict, owner).Code)
	require.NoError(t, service.NewOpenAIWSStateStore(cache).BindResponseAccount(context.Background(), 91, "resp_before_rebind", 1, time.Hour))
	next, nextHash := dispatchRequest(t, gateway, sid, "http")
	selection := dispatchSelect(t, gateway, next, nextHash, "resp_before_rebind")
	require.Equal(t, int64(1), selection.Account.ID, "previous 优先级保持既有行为")
	gateway.ObserveOpenAIDispatchAdmission(next.Request.Context(), 1, false)
	selection.ReleaseFunc()
	var status service.OpenAIDispatchOperation
	dispatchAPIData(t, dispatchAPICall(t, router, "GET", "/status?operation_id="+op.OperationID, "", owner), &status)
	require.Equal(t, 1, status.Counts.AssignedOther, "提前返回也要观察自己读到的改绑身份")
	require.Equal(t, float64(1), status.Sessions[0].CurrentBinding)
}

func TestUserAccountDispatchKnownOwnerConflictSkipsMarkerCapture(t *testing.T) {
	router, gateway, _ := newDispatchAPI(t)
	require.NoError(t, gateway.SetOpenAIDispatchEnabled(context.Background(), true))
	sid := uuid.NewString()
	c, hash := dispatchRequest(t, gateway, sid, "http")
	slot := dispatchSelect(t, gateway, c, hash, "")
	slot.ReleaseFunc()
	var preview service.OpenAIDispatchPreview
	dispatchAPIData(t, dispatchAPICall(t, router, "POST", "/preview", `{"user_id":42,"source_account_id":1}`, nil), &preview)
	var op service.OpenAIDispatchOperation
	dispatchAPIData(t, dispatchAPICall(t, router, "POST", "/create", fmt.Sprintf(`{"preview_id":%q,"target_account_id":2,"idempotency_key":%q}`, preview.PreviewID, uuid.NewString()), nil), &op)
	foreign, foreignHash := dispatchRequest(t, gateway, sid, "http")
	foreign.Request = foreign.Request.WithContext(context.WithValue(foreign.Request.Context(), ctxkey.UserID, int64(43)))
	foreignSlot := dispatchSelect(t, gateway, foreign, foreignHash, "")
	gateway.ObserveOpenAIDispatchAdmission(foreign.Request.Context(), foreignSlot.Account.ID, false)
	foreignSlot.ReleaseFunc()
	var status service.OpenAIDispatchOperation
	dispatchAPIData(t, dispatchAPICall(t, router, "GET", "/status?operation_id="+op.OperationID, "", nil), &status)
	require.Equal(t, 1, status.Counts.Awaiting)
	require.Nil(t, status.Sessions[0].AssignedAccountID)
}
