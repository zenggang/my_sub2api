package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type healthProbeHandlerRepo struct {
	service.AccountRepository
	account *service.Account
}

func (r *healthProbeHandlerRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	if r.account != nil && r.account.ID == id {
		return r.account, nil
	}
	return nil, service.ErrAccountNotFound
}

type healthProbeHandlerUpstream struct {
	service.HTTPUpstream
	calls int
}

func (u *healthProbeHandlerUpstream) DoWithTLS(_ *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.calls++
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\ndata: {\"type\":\"response.completed\"}\n\n")),
	}, nil
}

func TestAccountHandlerHealthProbeReturnsStandardResultAndRejectsCompact(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &service.Account{
		ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "test-token"},
	}
	upstream := &healthProbeHandlerUpstream{}
	handler := &AccountHandler{accountTestService: service.NewAccountTestService(
		&healthProbeHandlerRepo{account: account}, nil, nil, nil, nil, upstream, nil, nil,
	)}

	for _, tc := range []struct {
		body       string
		wantStatus int
		wantCalls  int
	}{
		{body: `{"model_id":"gpt-5.4","mode":"compact"}`, wantStatus: http.StatusBadRequest, wantCalls: 0},
		{body: `{"model_id":"gpt-5.4"}`, wantStatus: http.StatusOK, wantCalls: 1},
	} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Params = gin.Params{{Key: "id", Value: "1"}}
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/1/health-probe", bytes.NewBufferString(tc.body))
		c.Request.Header.Set("Content-Type", "application/json")
		handler.HealthProbe(c)
		require.Equal(t, tc.wantStatus, recorder.Code)
		require.Equal(t, tc.wantCalls, upstream.calls)
		if tc.wantStatus == http.StatusOK {
			var envelope struct {
				Code int `json:"code"`
				Data struct {
					AccountID          int64  `json:"account_id"`
					ModelID            string `json:"model_id"`
					Success            bool   `json:"success"`
					UpstreamStatusCode int    `json:"upstream_status_code"`
					FirstTokenMS       *int64 `json:"first_token_ms"`
					DurationMS         int64  `json:"duration_ms"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
			require.Zero(t, envelope.Code)
			require.Equal(t, int64(1), envelope.Data.AccountID)
			require.Equal(t, "gpt-5.4", envelope.Data.ModelID)
			require.True(t, envelope.Data.Success)
			require.Equal(t, http.StatusOK, envelope.Data.UpstreamStatusCode)
			require.NotNil(t, envelope.Data.FirstTokenMS)
			require.LessOrEqual(t, *envelope.Data.FirstTokenMS, envelope.Data.DurationMS)
		}
	}
}

func TestAccountHandlerIntelligenceTestReturnsStandardResult(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &service.Account{
		ID: 13, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "test-token"},
	}
	upstream := &healthProbeHandlerUpstream{}
	handler := &AccountHandler{accountTestService: service.NewAccountTestService(
		&healthProbeHandlerRepo{account: account}, nil, nil, nil, nil, upstream, nil, nil,
	)}
	for _, tc := range []struct {
		body       string
		wantStatus int
		wantCalls  int
	}{
		{body: `{"model_id":"gpt-5.4","prompt":"hello","reasoning_effort":"ultra"}`, wantStatus: http.StatusBadRequest, wantCalls: 0},
		{body: `{"model_id":"gpt-5.4","prompt":"hello","reasoning_effort":"medium"}`, wantStatus: http.StatusOK, wantCalls: 1},
	} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Params = gin.Params{{Key: "id", Value: "13"}}
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/13/intelligence-test", bytes.NewBufferString(tc.body))
		c.Request.Header.Set("Content-Type", "application/json")
		handler.IntelligenceTest(c)
		require.Equal(t, tc.wantStatus, recorder.Code)
		require.Equal(t, tc.wantCalls, upstream.calls)
		if tc.wantStatus == http.StatusOK {
			var envelope struct {
				Code int `json:"code"`
				Data struct {
					AccountID  int64  `json:"account_id"`
					ModelID    string `json:"model_id"`
					Success    bool   `json:"success"`
					OutputText string `json:"output_text"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
			require.Zero(t, envelope.Code)
			require.Equal(t, int64(13), envelope.Data.AccountID)
			require.Equal(t, "gpt-5.4", envelope.Data.ModelID)
			require.True(t, envelope.Data.Success)
			require.Equal(t, "hi", envelope.Data.OutputText)
		}
	}
}
