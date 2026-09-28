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
	first, _, message := readAccountHealthProbeStream(reader, time.Now())
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
	first, _, message := readAccountHealthProbeStream(reader, time.Now())
	require.Empty(t, message)
	require.NotNil(t, first)
}

func TestAccountHealthProbeConcatenatedStreamEvents(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantError  string
		firstToken bool
	}{
		{name: "delta and completed", body: `data: {"type":"response.output_text.delta","delta":"hi"}{"type":"response.completed"}` + "\n\n", firstToken: true},
		{name: "failure after completed on same line", body: `data: {"type":"response.output_text.delta","delta":"hi"}{"type":"response.completed"}{"type":"response.failed","response":{"error":{"code":"server_error"}}}` + "\n\n", wantError: "Upstream stream failed (server_error)", firstToken: true},
		{name: "completed ends before next line", body: `data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" + `data: {"type":"response.completed"}` + "\n\n" + `data: {"type":"response.failed"}` + "\n\n", firstToken: true},
		{name: "malformed tail after completed", body: `data: {"type":"response.output_text.delta","delta":"hi"}{"type":"response.completed"}{bad` + "\n\n", wantError: "Invalid upstream stream event"},
		{name: "completed then done", body: `data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" + `data: {"type":"response.completed"}` + "\n\n" + "data: [DONE]\n\n", firstToken: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first, _, message := readAccountHealthProbeStream(strings.NewReader(tc.body), time.Now())
			require.Equal(t, tc.wantError, message)
			require.Equal(t, tc.firstToken, first != nil)
		})
	}
}

func TestAccountHealthProbeCompletionDoesNotWaitForStreamClose(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantError  string
		firstToken bool
	}{
		{name: "text completed", body: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\ndata: {\"type\":\"response.completed\"}\n\n", firstToken: true},
		{name: "empty completed", body: "data: {\"type\":\"response.completed\"}\n\n", wantError: "Upstream completed without text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			type probeResult struct {
				firstToken *int64
				message    string
			}
			resultCh := make(chan probeResult, 1)
			go func() {
				first, _, message := readAccountHealthProbeStream(reader, time.Now())
				resultCh <- probeResult{firstToken: first, message: message}
			}()
			_, err := io.WriteString(writer, tc.body)
			require.NoError(t, err)
			select {
			case result := <-resultCh:
				require.Equal(t, tc.wantError, result.message)
				require.Equal(t, tc.firstToken, result.firstToken != nil)
			case <-time.After(2 * time.Second):
				t.Fatal("health probe waited for stream close after response.completed")
			}
		})
	}
}

func TestAccountHealthProbeFailuresKeepAccountStateAndSecretsPrivate(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		body         string
		wantError    string
		firstToken   bool
		streamStatus int
	}{
		{name: "empty completed", status: 200, body: "data: {\"type\":\"response.completed\"}\n\n", wantError: "Upstream completed without text"},
		{name: "truncated after text", status: 200, body: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n", wantError: "Upstream stream ended before completion", firstToken: true},
		{name: "stream error", status: 200, body: "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"access_token=secret\"}}}\n\n", wantError: "Upstream stream failed (server_error)"},
		{name: "stream 503", status: 200, body: `data: {"type":"response.failed","response":{"error":{"status_code":503,"code":"server_is_overloaded","message":"access_token=secret"}}}` + "\n\n", wantError: "Upstream stream failed (server_is_overloaded)", streamStatus: 503},
		{name: "stream 504", status: 200, body: `data: {"type":"error","error":{"status":"504","code":"server_error"}}` + "\n\n", wantError: "Upstream stream failed (server_error)", streamStatus: 504},
		{name: "numeric stream error code", status: 200, body: `data: {"type":"response.failed","response":{"error":{"code":"503"}}}` + "\n\n", wantError: "Upstream stream failed", streamStatus: 503},
		{name: "semantic code without numeric status", status: 200, body: `data: {"type":"response.failed","response":{"error":{"code":"slow_down"}}}` + "\n\n", wantError: "Upstream stream failed (slow_down)"},
		{name: "invalid stream status", status: 200, body: `data: {"type":"response.failed","response":{"error":{"status_code":200,"code":"server_error"}}}` + "\n\n", wantError: "Upstream stream failed (server_error)"},
		{name: "401", status: 401, body: `{"error":{"code":"invalid_token","message":"access_token=secret"}}`, wantError: "Upstream returned HTTP 401 (invalid_token)"},
		{name: "429", status: 429, body: `{"error":{"code":"rate_limit_exceeded","message":"access_token=secret"}}`, wantError: "Upstream returned HTTP 429 (rate_limit_exceeded)"},
		{name: "untrusted HTTP error code", status: 503, body: `{"error":{"code":"sk-test-token-123","message":"access_token=secret"}}`, wantError: "Upstream returned HTTP 503"},
		{name: "untrusted stream error code", status: 200, body: "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"sk-test-token-123\"}}}\n\n", wantError: "Upstream stream failed"},
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
			if tc.streamStatus == 0 {
				require.Nil(t, result.StreamErrorStatusCode)
			} else {
				require.Equal(t, tc.streamStatus, *result.StreamErrorStatusCode)
			}
			require.Equal(t, tc.wantError, result.Error)
			require.NotContains(t, result.Error, "secret")
			require.NotContains(t, result.Error, "sk-test-token-123")
			require.Equal(t, tc.firstToken, result.FirstTokenMS != nil)
			requireHealthProbeNoAccountWrites(t, repo)
		})
	}
}

func TestAccountHealthProbeResolvesTextCapabilityFromUpstreamModel(t *testing.T) {
	cases := []struct {
		name          string
		model         string
		mappingTarget string
		passthrough   bool
		wantUpstream  string
		wantError     bool
	}{
		{name: "image named alias maps to text", model: "gpt-image-alias", mappingTarget: "gpt-5.4", wantUpstream: "gpt-5.4"},
		{name: "text named alias maps to image", model: "text-alias", mappingTarget: "gpt-image-2", wantError: true},
		{name: "passthrough ignores text named alias image mapping", model: "text-alias", mappingTarget: "gpt-image-2", passthrough: true, wantUpstream: "text-alias"},
		{name: "passthrough rejects image named alias text mapping", model: "gpt-image-alias", mappingTarget: "gpt-5.4", passthrough: true, wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := healthProbeTestAccount(95, AccountTypeOAuth)
			account.Credentials["model_mapping"] = map[string]any{tc.model: tc.mappingTarget}
			if tc.passthrough {
				account.Extra = map[string]any{"openai_passthrough": true}
			}
			repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}}
			upstream := &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(http.StatusOK, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\ndata: {\"type\":\"response.completed\"}\n\n")}}
			svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
			result, err := svc.ProbeAccountHealth(context.Background(), account.ID, tc.model)
			if tc.wantError {
				require.Error(t, err)
				require.Empty(t, upstream.requests)
			} else {
				require.NoError(t, err)
				require.True(t, result.Success)
				require.Len(t, upstream.requests, 1)
				body, readErr := io.ReadAll(upstream.requests[0].Body)
				require.NoError(t, readErr)
				require.Equal(t, tc.wantUpstream, gjson.GetBytes(body, "model").String())
			}
			requireHealthProbeNoAccountWrites(t, repo)
		})
	}
}

func TestAccountHealthProbeRejectsSyntheticFixtureWithoutUpstreamOrWrites(t *testing.T) {
	account := healthProbeTestAccount(96, AccountTypeOAuth)
	account.Extra = map[string]any{"synthetic_ui_test": true}
	repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}}
	upstream := &queuedHTTPUpstream{}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}

	result, err := svc.ProbeAccountHealth(context.Background(), account.ID, "gpt-5.4")
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Equal(t, "Synthetic test account cannot be health-probed", result.Error)
	require.Nil(t, result.UpstreamStatusCode)
	require.Empty(t, upstream.requests)
	requireHealthProbeNoAccountWrites(t, repo)
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
