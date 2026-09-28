//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func healthProbeTestAccount(id int64, accountType string) *Account {
	return &Account{
		ID:          id,
		Platform:    PlatformOpenAI,
		Type:        accountType,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-access-token", "chatgpt_account_id": "test-chatgpt-account"},
	}
}

func requireHealthProbeNoAccountWrites(t *testing.T, repo *openAIAccountTestRepo) {
	t.Helper()
	require.Nil(t, repo.updatedExtra)
	require.Empty(t, repo.bulkUpdatedIDs)
	require.Zero(t, repo.rateLimitedID)
	require.Zero(t, repo.clearedErrorID)
	require.Zero(t, repo.setErrorID)
}

func TestAccountHealthProbeOAuthTextAndSetupTokenUseExistingRequestShapeWithoutWrites(t *testing.T) {
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
		t.Run(accountType, func(t *testing.T) {
			account := healthProbeTestAccount(89, accountType)
			account.Credentials["model_mapping"] = map[string]any{"display-model": "gpt-5.6-sol"}
			repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}}
			resp := newJSONResponse(http.StatusOK, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"   \"}\n\n"+
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"+
				"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
			resp.Header.Set("x-codex-primary-used-percent", "88")
			upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
			svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}

			result, err := svc.ProbeAccountHealth(context.Background(), account.ID, "display-model")
			require.NoError(t, err)
			require.True(t, result.Success)
			require.Equal(t, account.ID, result.AccountID)
			require.Equal(t, "display-model", result.ModelID)
			require.Equal(t, http.StatusOK, *result.UpstreamStatusCode)
			require.NotNil(t, result.FirstTokenMS)
			require.LessOrEqual(t, *result.FirstTokenMS, result.DurationMS)
			require.Empty(t, result.Error)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, chatgptCodexAPIURL, upstream.requests[0].URL.String())
			require.Equal(t, "chatgpt.com", upstream.requests[0].Host)
			require.Equal(t, "Bearer test-access-token", upstream.requests[0].Header.Get("Authorization"))
			require.Equal(t, "test-chatgpt-account", upstream.requests[0].Header.Get("chatgpt-account-id"))
			require.Equal(t, HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileFromContext(upstream.requests[0].Context()))
			body, err := io.ReadAll(upstream.requests[0].Body)
			require.NoError(t, err)
			require.Equal(t, "gpt-5.6-sol", gjson.GetBytes(body, "model").String())
			require.False(t, gjson.GetBytes(body, "store").Bool())
			requireHealthProbeNoAccountWrites(t, repo)
		})
	}
}

func TestAccountHealthProbeFirstTokenNeedsNonWhitespaceText(t *testing.T) {
	reader, writer := io.Pipe()
	go func() {
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\" \"}\n\n")
		time.Sleep(25 * time.Millisecond)
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\"}\n\n")
		_ = writer.Close()
	}()
	first, message := readAccountHealthProbeStream(reader, time.Now())
	require.Empty(t, message)
	require.NotNil(t, first)
	require.GreaterOrEqual(t, *first, int64(20))
}

func TestAccountHealthProbeCountsTextFromOutputItemBeforeCompleted(t *testing.T) {
	reader, writer := io.Pipe()
	go func() {
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_item.done\",\"item\":{\"content\":[{\"type\":\"output_text\",\"text\":\"hi\"}]}}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\"}\n\n")
		_ = writer.Close()
	}()
	first, message := readAccountHealthProbeStream(reader, time.Now())
	require.Empty(t, message)
	require.NotNil(t, first)
}

func TestAccountHealthProbeFailuresKeepAccountStateAndSecretsPrivate(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantError  string
		firstToken bool
	}{
		{name: "empty completed", status: 200, body: "data: {\"type\":\"response.completed\"}\n\n", wantError: "Upstream completed without text"},
		{name: "truncated after text", status: 200, body: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n", wantError: "Upstream stream ended before completion", firstToken: true},
		{name: "stream error", status: 200, body: "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"access_token=secret\"}}}\n\n", wantError: "Upstream stream failed (server_error)"},
		{name: "401", status: 401, body: `{"error":{"code":"invalid_token","message":"access_token=secret"}}`, wantError: "Upstream returned HTTP 401 (invalid_token)"},
		{name: "429", status: 429, body: `{"error":{"code":"rate_limit_exceeded","message":"access_token=secret"}}`, wantError: "Upstream returned HTTP 429 (rate_limit_exceeded)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := healthProbeTestAccount(90, AccountTypeOAuth)
			repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}}
			resp := newJSONResponse(tc.status, tc.body)
			resp.Header.Set("x-codex-primary-used-percent", "88")
			upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
			svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
			result, err := svc.ProbeAccountHealth(context.Background(), account.ID, "gpt-5.4")
			require.NoError(t, err)
			require.False(t, result.Success)
			require.Equal(t, tc.status, *result.UpstreamStatusCode)
			require.Equal(t, tc.wantError, result.Error)
			require.NotContains(t, result.Error, "secret")
			require.Equal(t, tc.firstToken, result.FirstTokenMS != nil)
			requireHealthProbeNoAccountWrites(t, repo)
		})
	}
}

type healthProbeTimeoutUpstream struct{ queuedHTTPUpstream }

func (u *healthProbeTimeoutUpstream) DoWithTLS(_ *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return nil, context.DeadlineExceeded
}

func TestAccountHealthProbeTimeoutIsFailureWithoutAccountWrites(t *testing.T) {
	account := healthProbeTestAccount(94, AccountTypeOAuth)
	repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: &healthProbeTimeoutUpstream{}}
	result, err := svc.ProbeAccountHealth(context.Background(), account.ID, "gpt-5.4")
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Nil(t, result.UpstreamStatusCode)
	require.Nil(t, result.FirstTokenMS)
	require.Equal(t, "Upstream request timed out", result.Error)
	requireHealthProbeNoAccountWrites(t, repo)
}

func TestAccountHealthProbeRejectsNonTextAndNonOAuthAccountsBeforeUpstream(t *testing.T) {
	account := healthProbeTestAccount(91, AccountTypeOAuth)
	account.Credentials["model_mapping"] = map[string]any{"draw": "gpt-image-2"}
	repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}}
	upstream := &queuedHTTPUpstream{}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	for _, model := range []string{"gpt-image-2", "gpt-realtime", "gpt-5.4-compact", "draw"} {
		_, err := svc.ProbeAccountHealth(context.Background(), account.ID, model)
		require.Error(t, err, model)
	}
	account.Type = AccountTypeAPIKey
	_, err := svc.ProbeAccountHealth(context.Background(), account.ID, "gpt-5.4")
	require.Error(t, err)
	require.Empty(t, upstream.requests)
	requireHealthProbeNoAccountWrites(t, repo)
}

func TestAccountHealthProbeAgentIdentityUsesOnlyExistingTaskAndShadowParent(t *testing.T) {
	_, privateKey := newTestAgentIdentityKey(t)
	parent := healthProbeTestAccount(92, AccountTypeOAuth)
	parent.Credentials = map[string]any{
		"auth_mode":          OpenAIAuthModeAgentIdentity,
		"agent_private_key":  privateKey,
		"agent_runtime_id":   "runtime-test",
		"chatgpt_account_id": "test-chatgpt-account",
	}
	parentID := parent.ID
	shadow := healthProbeTestAccount(93, AccountTypeOAuth)
	shadow.ParentAccountID = &parentID
	shadow.Credentials = map[string]any{"model_mapping": map[string]any{"shadow-model": "gpt-5.4"}}
	repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{parent.ID: parent, shadow.ID: shadow}}}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(200, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\ndata: {\"type\":\"response.completed\"}\n\n")}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}

	missing, err := svc.ProbeAccountHealth(context.Background(), shadow.ID, "shadow-model")
	require.NoError(t, err)
	require.False(t, missing.Success)
	require.Equal(t, "Agent Identity task or key is unavailable", missing.Error)
	require.Empty(t, upstream.requests)
	requireHealthProbeNoAccountWrites(t, repo)

	parent.Credentials["task_id"] = "task-test"
	result, err := svc.ProbeAccountHealth(context.Background(), shadow.ID, "shadow-model")
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, shadow.ID, result.AccountID)
	require.True(t, strings.HasPrefix(upstream.requests[0].Header.Get("Authorization"), "AgentAssertion "))
	require.Equal(t, "test-chatgpt-account", upstream.requests[0].Header.Get("chatgpt-account-id"))
	body, err := io.ReadAll(upstream.requests[0].Body)
	require.NoError(t, err)
	require.Equal(t, "gpt-5.4", gjson.GetBytes(body, "model").String())
	requireHealthProbeNoAccountWrites(t, repo)
}
