package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type accountPauseAPIRepository struct {
	service.AccountUserPauseRepository
	failure        error
	paused         bool
	summaryFailure error
	summaryPairs   []service.AccountUserPausePair
}

func (r *accountPauseAPIRepository) SetPaused(_ context.Context, aid, uid, actor int64, paused bool) (*service.AccountUserPauseResult, error) {
	if r.failure != nil {
		return nil, r.failure
	}
	changed := r.paused != paused
	r.paused = paused
	result := &service.AccountUserPauseResult{AccountID: aid, UserID: uid, Email: "user@example.com", Paused: paused, Changed: changed}
	if paused {
		now := time.Now()
		result.PausedAt = &now
		result.PausedCount = 1
	}
	return result, nil
}
func (r *accountPauseAPIRepository) List(_ context.Context, aid int64, page, size int) (*service.AccountUserPausePage, error) {
	if r.failure != nil {
		return nil, r.failure
	}
	return &service.AccountUserPausePage{AccountID: aid, Items: []service.AccountUserPauseEntry{}, Page: page, PageSize: size}, nil
}
func (r *accountPauseAPIRepository) Summary(_ context.Context, pairs []service.AccountUserPausePair) (*service.AccountUserPauseSummary, error) {
	r.summaryPairs = pairs
	if r.summaryFailure != nil {
		return nil, r.summaryFailure
	}
	return &service.AccountUserPauseSummary{Available: true, Counts: map[int64]int64{999: 4}, ActivePausedUsers: map[int64][]int64{}}, nil
}

type accountPauseAPIUsers struct{ service.UserRepository }

func (accountPauseAPIUsers) GetByID(_ context.Context, id int64) (*service.User, error) {
	if id != 8 {
		return nil, service.ErrUserNotFound
	}
	return &service.User{ID: 8, Email: "user@example.com"}, nil
}
func (accountPauseAPIUsers) GetByEmail(_ context.Context, email string) (*service.User, error) {
	if email == "ambiguous@example.com" {
		return nil, service.ErrUserEmailAmbiguous
	}
	if email != "user@example.com" {
		return nil, service.ErrUserNotFound
	}
	return &service.User{ID: 8, Email: email}, nil
}

func newAccountPauseAPI(t *testing.T) (*gin.Engine, *accountPauseAPIRepository) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repo := &accountPauseAPIRepository{}
	h := NewOpsHandlerWithAccountUserPauses(&service.OpsService{}, nil, service.NewAccountUserPauseService(repo, accountPauseAPIUsers{}))
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if c.GetHeader("Test-Role") != "absent" {
			role := service.RoleAdmin
			if c.GetHeader("Test-Role") == "user" {
				role = service.RoleUser
			}
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
			c.Set(string(middleware.ContextKeyUserRole), role)
		}
	})
	router.GET("/pauses", h.ListAccountUserPauses)
	router.POST("/pause", h.PauseAccountUser)
	router.POST("/resume", h.ResumeAccountUser)
	router.GET("/observation", h.GetAccountSlotObservation)
	return router, repo
}
func accountPauseAPICall(router *gin.Engine, method, path, body, role string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Test-Role", role)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	return w
}
func accountPauseAPIEnvelope(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var result map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	return result
}

func TestAccountUserPauseAPIIdentityEmailAndIdempotentResults(t *testing.T) {
	router, _ := newAccountPauseAPI(t)
	first := accountPauseAPICall(router, "POST", "/pause", `{"account_id":29,"email":" User@Example.COM "}`, "")
	require.Equal(t, 200, first.Code, first.Body.String())
	data := accountPauseAPIEnvelope(t, first)["data"].(map[string]any)
	require.Equal(t, true, data["paused"])
	require.Equal(t, true, data["changed"])
	require.Equal(t, float64(8), data["user_id"])
	require.Equal(t, service.AccountUserPauseScope, data["scope"])
	second := accountPauseAPICall(router, "POST", "/pause", `{"account_id":29,"user_id":8}`, "")
	require.Equal(t, 200, second.Code)
	require.Equal(t, false, accountPauseAPIEnvelope(t, second)["data"].(map[string]any)["changed"])
	resume := accountPauseAPICall(router, "POST", "/resume", `{"account_id":29,"user_id":8}`, "")
	require.Equal(t, 200, resume.Code)
	data = accountPauseAPIEnvelope(t, resume)["data"].(map[string]any)
	require.Equal(t, false, data["paused"])
	require.Nil(t, data["paused_at"])
}

func TestAccountUserPauseAPIRejectsUnknownAmbiguousAndInvalidInputs(t *testing.T) {
	router, _ := newAccountPauseAPI(t)
	for _, tc := range []struct {
		path, body string
		status     int
		reason     string
	}{
		{"/pause", `{"account_id":29,"user_id":8,"email":"user@example.com"}`, 400, "INVALID_REQUEST"},
		{"/pause", `{"account_id":29,"user_id":null,"email":"user@example.com"}`, 400, "INVALID_REQUEST"},
		{"/pause", `{"account_id":29,"user_id":0}`, 400, "INVALID_REQUEST"},
		{"/pause", `{"account_id":29,"user_id":8,"actor_id":999}`, 400, "INVALID_REQUEST"},
		{"/pause", `{"account_id":29,"email":"missing@example.com"}`, 404, "USER_NOT_FOUND"},
		{"/pause", `{"account_id":29,"email":"ambiguous@example.com"}`, 409, "EMAIL_AMBIGUOUS"},
		{"/pause", `{"account_id":29,"email":"not-email"}`, 400, "INVALID_REQUEST"},
		{"/pause?user_id=9", `{"account_id":29,"user_id":8}`, 400, "INVALID_REQUEST"},
		{"/resume", `{"account_id":29,"user_id":8} {}`, 400, "INVALID_REQUEST"},
	} {
		t.Run(tc.path+tc.body, func(t *testing.T) {
			w := accountPauseAPICall(router, "POST", tc.path, tc.body, "")
			require.Equal(t, tc.status, w.Code, w.Body.String())
			require.Equal(t, tc.reason, accountPauseAPIEnvelope(t, w)["reason"])
		})
	}
}

func TestAccountUserPauseAPIValidatesPaginationAndAdmin(t *testing.T) {
	router, _ := newAccountPauseAPI(t)
	for _, query := range []string{"", "account_id=0", "account_id=29&page=0", "account_id=29&page_size=101", "account_id=29&page=9999999999999999999999", "account_id=29&page=2&page=3", "account_id=29&user_id=8", "account_id=29&page=", "account_id=29;page=1"} {
		w := accountPauseAPICall(router, "GET", "/pauses?"+query, "", "")
		require.Equal(t, 400, w.Code, query+" "+w.Body.String())
	}
	w := accountPauseAPICall(router, "GET", "/pauses?account_id=29&page=2&page_size=30", "", "")
	require.Equal(t, 200, w.Code)
	require.Equal(t, float64(30), accountPauseAPIEnvelope(t, w)["data"].(map[string]any)["page_size"])
	for _, role := range []string{"absent", "user"} {
		w = accountPauseAPICall(router, "POST", "/pause", `{"account_id":29,"user_id":8}`, role)
		require.Equal(t, http.StatusForbidden, w.Code)
	}
}

func TestAccountUserPauseAPIPropagatesUnknownWriteResult(t *testing.T) {
	router, repo := newAccountPauseAPI(t)
	repo.failure = service.ErrAccountUserPauseResultUnknown.WithCause(errors.New("lost commit reply"))
	w := accountPauseAPICall(router, "POST", "/pause", `{"account_id":29,"user_id":8}`, "")
	require.Equal(t, 503, w.Code)
	require.Equal(t, "RESULT_UNKNOWN", accountPauseAPIEnvelope(t, w)["reason"])
}

func TestAccountUserPauseObservationKeepsIdleCountsAndUnknownFailure(t *testing.T) {
	router, repo := newAccountPauseAPI(t)
	w := accountPauseAPICall(router, "GET", "/observation", "", "")
	require.Equal(t, 200, w.Code)
	data := accountPauseAPIEnvelope(t, w)["data"].(map[string]any)
	summary := data["pause_summary"].(map[string]any)
	require.Equal(t, true, summary["available"])
	require.Equal(t, float64(4), summary["counts"].(map[string]any)["999"])
	require.Empty(t, repo.summaryPairs)
	repo.summaryFailure = errors.New("database offline")
	w = accountPauseAPICall(router, "GET", "/observation", "", "")
	require.Equal(t, 200, w.Code)
	data = accountPauseAPIEnvelope(t, w)["data"].(map[string]any)
	require.Contains(t, data, "accounts")
	summary = data["pause_summary"].(map[string]any)
	require.Equal(t, false, summary["available"])
	require.NotContains(t, summary, "counts")
}
