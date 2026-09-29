//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type intelligenceHTTPUpstream struct {
	queuedHTTPUpstream
	accountIDs []int64
}

func (u *intelligenceHTTPUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u.accountIDs = append(u.accountIDs, accountID)
	return u.queuedHTTPUpstream.DoWithTLS(req, proxyURL, accountID, concurrency, profile)
}

func TestAccountIntelligenceUsesOnlySelectedOAuthLikeAccountWithoutWrites(t *testing.T) {
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
		t.Run(accountType, func(t *testing.T) {
			selected := healthProbeTestAccount(13, accountType)
			selected.Credentials["model_mapping"] = map[string]any{"public-model": "gpt-5.6-sol"}
			other := healthProbeTestAccount(15, AccountTypeOAuth)
			other.Credentials["access_token"] = "other-account-token"
			repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{selected.ID: selected, other.ID: other}}}
			upstream := &intelligenceHTTPUpstream{queuedHTTPUpstream: queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(http.StatusOK,
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"<!doctype html><html>\"}\n\n"+
					"data: {\"type\":\"response.output_text.delta\",\"delta\":\"done</html>\"}\n\n"+
					"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")}}}
			svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
			prompt := "  Draw one pelican cycling.  "
			result, err := svc.TestAccountIntelligence(context.Background(), selected.ID, "public-model", prompt, "high")
			require.NoError(t, err)
			require.True(t, result.Success)
			require.Equal(t, "<!doctype html><html>done</html>", result.OutputText)
			require.Equal(t, selected.ID, result.AccountID)
			require.Equal(t, "public-model", result.ModelID)
			require.Equal(t, http.StatusOK, *result.UpstreamStatusCode)
			require.Empty(t, result.Error)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, []int64{selected.ID}, upstream.accountIDs)
			require.Equal(t, "Bearer test-access-token", upstream.requests[0].Header.Get("Authorization"))
			body, readErr := io.ReadAll(upstream.requests[0].Body)
			require.NoError(t, readErr)
			require.Equal(t, "gpt-5.6-sol", gjson.GetBytes(body, "model").String())
			require.Equal(t, prompt, gjson.GetBytes(body, "input.0.content.0.text").String())
			require.Equal(t, "high", gjson.GetBytes(body, "reasoning.effort").String())
			require.False(t, gjson.GetBytes(body, "store").Bool())
			require.False(t, gjson.GetBytes(body, "instructions").Exists())
			requireHealthProbeNoAccountWrites(t, repo)
		})
	}
}

func TestAccountIntelligenceDefaultsEffortAndRejectsInvalidInputBeforeUpstream(t *testing.T) {
	account := healthProbeTestAccount(7, AccountTypeOAuth)
	repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(http.StatusOK,
		"data: {\"type\":\"response.output_text.done\",\"text\":\"<html>ok</html>\"}\n\n"+
			"data: {\"type\":\"response.completed\"}\n\n")}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	for _, tc := range []struct{ model, prompt, effort string }{
		{"bad model", "valid", "medium"},
		{"gpt-5.4", " ", "medium"},
		{"gpt-5.4", strings.Repeat("字", maxIntelligencePromptRunes+1), "medium"},
		{"gpt-5.4", "valid", "ultra"},
	} {
		_, err := svc.TestAccountIntelligence(context.Background(), account.ID, tc.model, tc.prompt, tc.effort)
		require.Error(t, err)
	}
	require.Empty(t, upstream.requests)
	result, err := svc.TestAccountIntelligence(context.Background(), account.ID, "gpt-5.4", "Exact prompt", "")
	require.NoError(t, err)
	require.True(t, result.Success)
	body, readErr := io.ReadAll(upstream.requests[0].Body)
	require.NoError(t, readErr)
	require.Equal(t, "medium", gjson.GetBytes(body, "reasoning.effort").String())
	requireHealthProbeNoAccountWrites(t, repo)
}

func TestAccountIntelligenceFailureDiscardsPartialOutputAndKeepsAccountState(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantError string
		status                int
		streamStatus          int
	}{
		{"upstream 429", `{"error":{"code":"rate_limit_exceeded","message":"access_token=secret"}}`, "Upstream returned HTTP 429 (rate_limit_exceeded)", 429, 0},
		{"stream failure", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n" +
			"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"status_code\":503,\"message\":\"access_token=secret\"}}}\n\n", "Upstream stream failed (server_error)", 200, 503},
		{"early EOF", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", "Upstream stream ended before completion", 200, 0},
		{"empty completion", "data: {\"type\":\"response.completed\"}\n\n", "Upstream completed without text", 200, 0},
		{"output limit", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"" + strings.Repeat("x", maxIntelligenceOutputBytes+1) + "\"}\n\n" +
			"data: {\"type\":\"response.completed\"}\n\n", "Response text exceeds 1 MiB limit", 200, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := healthProbeTestAccount(15, AccountTypeOAuth)
			repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}}
			upstream := &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(tc.status, tc.body)}}
			svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
			result, err := svc.TestAccountIntelligence(context.Background(), account.ID, "gpt-5.4", "prompt", "medium")
			require.NoError(t, err)
			require.False(t, result.Success)
			require.Empty(t, result.OutputText)
			require.Equal(t, tc.wantError, result.Error)
			require.Equal(t, tc.status, *result.UpstreamStatusCode)
			if tc.streamStatus != 0 {
				require.Equal(t, tc.streamStatus, *result.StreamErrorStatusCode)
			} else {
				require.Nil(t, result.StreamErrorStatusCode)
			}
			require.Len(t, upstream.requests, 1)
			requireHealthProbeNoAccountWrites(t, repo)
		})
	}
}

func TestAccountIntelligenceStreamUsesCompletedOutputFallback(t *testing.T) {
	output, status, err := readAccountIntelligenceStream(strings.NewReader(
		`data: {"type":"response.completed","response":{"status":"completed","output":[{"content":[{"type":"output_text","text":"<html>fallback</html>"}]}]}}` + "\n\n"))
	require.Empty(t, err)
	require.Nil(t, status)
	require.Equal(t, "<html>fallback</html>", output)

	output, status, err = readAccountIntelligenceStream(strings.NewReader(
		`data: {"type":"response.output_text.delta","delta":"<html>draft</html>"}` + "\n\n" +
			`data: {"type":"response.completed","response":{"status":"completed","output":[{"content":[{"type":"output_text","text":"<html>final</html>"}]}]}}` + "\n\n"))
	require.Empty(t, err)
	require.Nil(t, status)
	require.Equal(t, "<html>final</html>", output)

	output, status, err = readAccountIntelligenceStream(strings.NewReader(
		`data: {"type":"response.output_text.delta","delta":"` + strings.Repeat("x", maxIntelligenceOutputBytes+1) + `"}` + "\n\n" +
			`data: {"type":"response.completed","response":{"status":"completed","output":[{"content":[{"type":"output_text","text":"<html>final</html>"}]}]}}` + "\n\n"))
	require.Empty(t, err)
	require.Nil(t, status)
	require.Equal(t, "<html>final</html>", output)

	output, status, err = readAccountIntelligenceStream(strings.NewReader(
		`data: {"type":"response.output_text.delta","delta":"<html>draft</html>"}` + "\n\n" +
			`data: {"type":"response.completed","response":{"status":"completed","output":[{"content":[{"type":"output_text","text":"` + strings.Repeat("x", maxIntelligenceOutputBytes+1) + `"}]}]}}` + "\n\n"))
	require.Empty(t, output)
	require.Nil(t, status)
	require.Equal(t, "Response text exceeds 1 MiB limit", err)

	output, status, err = readAccountIntelligenceStream(strings.NewReader(
		`data: {"type":"response.content_part.done","part":{"type":"output_text","text":"<html>part</html>"}}` + "\n\n" +
			`data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n"))
	require.Empty(t, err)
	require.Nil(t, status)
	require.Equal(t, "<html>part</html>", output)

	output, status, err = readAccountIntelligenceStream(strings.NewReader(
		`data: {"type":"response.output_text.delta","delta":"partial"}{"type":"response.completed"}{"type":"response.failed","response":{"error":{"code":"server_error"}}}` + "\n\n"))
	require.Empty(t, output)
	require.Nil(t, status)
	require.Equal(t, "Upstream stream failed (server_error)", err)

	output, status, err = readAccountIntelligenceStream(strings.NewReader(
		`data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n" +
			`data: {"type":"response.done"}` + "\n\n"))
	require.Empty(t, output)
	require.Nil(t, status)
	require.Equal(t, "Upstream stream ended before completion", err)
}

func TestAccountIntelligenceCanceledRequestDoesNotReturnCompletedText(t *testing.T) {
	account := healthProbeTestAccount(7, AccountTypeOAuth)
	repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(http.StatusOK,
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"<html>done</html>\"}\n\n"+
			"data: {\"type\":\"response.completed\"}\n\n")}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := svc.TestAccountIntelligence(ctx, account.ID, "gpt-5.4", "prompt", "medium")
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Empty(t, result.OutputText)
	require.Equal(t, "Test request canceled", result.Error)
	requireHealthProbeNoAccountWrites(t, repo)
}
