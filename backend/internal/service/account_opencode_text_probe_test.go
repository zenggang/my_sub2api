//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type openCodeProbeHTTPUpstream struct {
	queuedHTTPUpstream
	accountID int64
	proxyURL  string
}

func (u *openCodeProbeHTTPUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u.accountID, u.proxyURL = accountID, proxyURL
	return u.queuedHTTPUpstream.DoWithTLS(req, proxyURL, accountID, concurrency, profile)
}

func openCodeProbeService(account *Account, status int, stream string) (*AccountTestService, *openAIAccountTestRepo, *openCodeProbeHTTPUpstream) {
	repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}}
	upstream := &openCodeProbeHTTPUpstream{queuedHTTPUpstream: queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(status, stream)}}}
	return &AccountTestService{accountRepo: repo, httpUpstream: upstream, cfg: &config.Config{}}, repo, upstream
}

func openCodeProbeSuccessStream(protocol string) string {
	switch protocol {
	case APIProtocolAnthropic:
		return "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"secret reasoning\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"<html>answer</html>\"}}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n"
	case APIProtocolResponses:
		return "data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"secret reasoning\"}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"<html>answer</html>\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	default:
		return "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"secret reasoning\"}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"<html>answer</html>\"}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	}
}

func TestOpenCodeAccountHealthAndIntelligenceUseSelectedNativeProtocolWithoutWrites(t *testing.T) {
	for _, tc := range []struct{ model, protocol, path string }{
		{"deepseek-v4-flash", APIProtocolChatCompletions, "/zen/go/v1/chat/completions"},
		{"grok-4.6", APIProtocolResponses, "/zen/go/v1/responses"},
		{"minimax-m3", APIProtocolAnthropic, "/zen/go/v1/messages"},
	} {
		for _, capture := range []bool{false, true} {
			t.Run(tc.protocol+map[bool]string{false: "/health", true: "/intelligence"}[capture], func(t *testing.T) {
				account := openCodeGoTestAccount(31)
				account.Status = "error"
				account.Schedulable = false
				account.Credentials["model_mapping"] = map[string]any{"public-image-name": tc.model}
				proxyID := int64(7)
				account.ProxyID = &proxyID
				account.Proxy = &Proxy{Protocol: "http", Host: "127.0.0.1", Port: 3128}
				svc, repo, upstream := openCodeProbeService(account, http.StatusOK, openCodeProbeSuccessStream(tc.protocol))
				prompt := "  Draw only my requested HTML.  "
				if capture {
					result, err := svc.TestAccountIntelligence(context.Background(), account.ID, "public-image-name", prompt, "high")
					require.NoError(t, err)
					require.True(t, result.Success, result.Error)
					require.Equal(t, "<html>answer</html>", result.OutputText)
				} else {
					result, err := svc.ProbeAccountHealth(context.Background(), account.ID, "public-image-name")
					require.NoError(t, err)
					require.True(t, result.Success, result.Error)
					require.NotNil(t, result.FirstTokenMS)
					prompt = "hi"
				}
				require.Len(t, upstream.requests, 1)
				req := upstream.requests[0]
				require.Equal(t, account.ID, upstream.accountID)
				require.Equal(t, "http://127.0.0.1:3128", upstream.proxyURL)
				require.Equal(t, tc.path, req.URL.Path)
				require.Equal(t, "opencode/1.0.0", req.Header.Get("User-Agent"))
				_, err := uuid.Parse(req.Header.Get("X-OpenCode-Session"))
				require.NoError(t, err)
				body, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				require.Equal(t, tc.model, gjson.GetBytes(body, "model").String())
				require.False(t, gjson.GetBytes(body, "instructions").Exists())
				require.False(t, gjson.GetBytes(body, "system").Exists())
				switch tc.protocol {
				case APIProtocolAnthropic:
					require.Equal(t, "sk-opencode-go-test", req.Header.Get("x-api-key"))
					require.Equal(t, prompt, gjson.GetBytes(body, "messages.0.content.0.text").String())
					if capture {
						require.Equal(t, "high", gjson.GetBytes(body, "output_config.effort").String())
						require.Greater(t, gjson.GetBytes(body, "max_tokens").Int(), gjson.GetBytes(body, "thinking.budget_tokens").Int())
					}
				case APIProtocolResponses:
					require.Equal(t, "Bearer sk-opencode-go-test", req.Header.Get("Authorization"))
					require.Equal(t, prompt, gjson.GetBytes(body, "input.0.content.0.text").String())
					if capture {
						require.Equal(t, "high", gjson.GetBytes(body, "reasoning.effort").String())
					}
				default:
					require.Equal(t, "Bearer sk-opencode-go-test", req.Header.Get("Authorization"))
					require.Equal(t, prompt, gjson.GetBytes(body, "messages.0.content").String())
					if capture {
						require.Equal(t, "high", gjson.GetBytes(body, "reasoning_effort").String())
					}
				}
				requireHealthProbeNoAccountWrites(t, repo)
				require.Equal(t, "error", account.Status)
				require.False(t, account.Schedulable)
			})
		}
	}
}

func TestOpenCodeAccountTestsHonorPinnedProtocolAndHeaderOverrides(t *testing.T) {
	account := openCodeGoTestAccount(31)
	account.Credentials["api_protocol"] = APIProtocolChatCompletions
	account.Credentials["base_url"] = "https://override.example/v1"
	account.Credentials["header_override_enabled"] = true
	account.Credentials["header_overrides"] = map[string]any{"User-Agent": "custom-agent", "X-OpenCode-Session": "stable-session"}
	svc, repo, upstream := openCodeProbeService(account, 200, openCodeProbeSuccessStream(APIProtocolChatCompletions))
	result, err := svc.ProbeAccountHealth(context.Background(), account.ID, "grok-4.6")
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, "https://override.example/v1/chat/completions", upstream.requests[0].URL.String())
	require.Equal(t, "custom-agent", upstream.requests[0].Header.Get("User-Agent"))
	require.Equal(t, "stable-session", upstream.requests[0].Header.Get("X-OpenCode-Session"))
	requireHealthProbeNoAccountWrites(t, repo)
}

func TestOpenCodeAccountTestsRejectUnsupportedModelsBeforeRequest(t *testing.T) {
	for _, target := range []string{"gemini-3-flash", "opencode/jev-latest", "gpt-image-2", "grok-video"} {
		account := openCodeGoTestAccount(31)
		account.Credentials["model_mapping"] = map[string]any{"public-model": target}
		svc, repo, upstream := openCodeProbeService(account, 200, "")
		_, err := svc.ProbeAccountHealth(context.Background(), account.ID, "public-model")
		require.Error(t, err)
		_, err = svc.TestAccountIntelligence(context.Background(), account.ID, "public-model", "Exact prompt", "medium")
		require.Error(t, err)
		require.Empty(t, upstream.requests)
		requireHealthProbeNoAccountWrites(t, repo)
	}
}

func TestOpenCodeAccountTextStreamRequiresAnswerAndNormalCompletion(t *testing.T) {
	for _, tc := range []struct{ name, protocol, stream, wantError string }{
		{"chat early eof", APIProtocolChatCompletions, `data: {"choices":[{"delta":{"content":"partial"}}]}` + "\n\n", "Upstream stream ended before completion"},
		{"chat thinking only", APIProtocolChatCompletions, `data: {"choices":[{"delta":{"reasoning_content":"thinking"},"finish_reason":"stop"}]}` + "\n\n", "Upstream completed without text"},
		{"chat truncated", APIProtocolChatCompletions, `data: {"choices":[{"delta":{"content":"partial"},"finish_reason":"length"}]}` + "\n\n", "Upstream response did not complete"},
		{"chat done only", APIProtocolChatCompletions, "data: [DONE]\n\n", "Upstream stream ended before completion"},
		{"chat malformed", APIProtocolChatCompletions, "data: {bad}\n\n", "Invalid upstream stream event"},
		{"anthropic early eof", APIProtocolAnthropic, `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}` + "\n\n", "Upstream stream ended before completion"},
		{"anthropic thinking only", APIProtocolAnthropic, `data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"thinking"}}` + "\n\ndata: {\"type\":\"message_stop\"}\n\n", "Upstream completed without text"},
		{"anthropic truncated", APIProtocolAnthropic, `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}` + "\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n", "Upstream response did not complete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, capture := range []bool{false, true} {
				text, _, _, failure := readOpenCodeAccountTextStream(strings.NewReader(tc.stream), tc.protocol, time.Now(), capture)
				require.Equal(t, tc.wantError, failure)
				require.Empty(t, text)
			}
		})
	}
	for _, protocol := range []string{APIProtocolChatCompletions, APIProtocolAnthropic} {
		_, first, status, failure := readOpenCodeAccountTextStream(strings.NewReader("data: {\"type\":\"error\",\"error\":{\"status_code\":503,\"code\":\"server_error\",\"message\":\"sk-secret\"}}\n\n"), protocol, time.Now(), true)
		require.EqualValues(t, 503, *status)
		require.Nil(t, first)
		require.Equal(t, "Upstream stream failed (server_error)", failure)
	}
}

func TestOpenCodeAccountTestsFailuresNeverMutateAccountOrExposeCredentials(t *testing.T) {
	for _, protocol := range []string{APIProtocolChatCompletions, APIProtocolResponses, APIProtocolAnthropic} {
		for _, status := range []int{401, 403, 429, 503} {
			account := openCodeGoTestAccount(31)
			account.Credentials["api_protocol"] = protocol
			svc, repo, _ := openCodeProbeService(account, status, `{"error":{"code":"sk-secret-token","message":"api_key=secret"}}`)
			result, err := svc.TestAccountIntelligence(context.Background(), account.ID, "glm-5.3", "Exact prompt", "medium")
			require.NoError(t, err)
			require.False(t, result.Success)
			require.Empty(t, result.OutputText)
			require.Equal(t, status, *result.UpstreamStatusCode)
			require.NotContains(t, result.Error, "secret")
			requireHealthProbeNoAccountWrites(t, repo)
		}
	}
}

func TestOpenCodeAccountTestsBoundOutputAndRecognizeCancellation(t *testing.T) {
	for _, protocol := range []string{APIProtocolChatCompletions, APIProtocolAnthropic} {
		stream := "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("x", maxIntelligenceOutputBytes+1) + "\"}}]}\n\n"
		if protocol == APIProtocolAnthropic {
			stream = "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"" + strings.Repeat("x", maxIntelligenceOutputBytes+1) + "\"}}\n\n"
		}
		output, _, _, failure := readOpenCodeAccountTextStream(strings.NewReader(stream), protocol, time.Now(), true)
		require.Empty(t, output)
		require.Equal(t, "Response text exceeds 1 MiB limit", failure)
	}
	account := openCodeGoTestAccount(31)
	svc, repo, _ := openCodeProbeService(account, 200, openCodeProbeSuccessStream(APIProtocolChatCompletions))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := svc.TestAccountIntelligence(ctx, account.ID, "glm-5.3", "Exact prompt", "medium")
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Empty(t, result.OutputText)
	require.Equal(t, "Test request canceled", result.Error)
	requireHealthProbeNoAccountWrites(t, repo)
	svc.httpUpstream = &healthProbeTimeoutUpstream{}
	health, err := svc.ProbeAccountHealth(context.Background(), account.ID, "glm-5.3")
	require.NoError(t, err)
	require.Equal(t, "Upstream request timed out", health.Error)
	requireHealthProbeNoAccountWrites(t, repo)
}
