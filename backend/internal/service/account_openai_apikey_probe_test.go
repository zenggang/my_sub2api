//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIAPIKeyAccountProbesUseConfiguredEndpointWithoutWrites(t *testing.T) {
	for _, protocol := range []string{APIProtocolResponses, APIProtocolChatCompletions} {
		for _, passthrough := range []bool{false, true} {
			for _, capture := range []bool{false, true} {
				t.Run(protocol+map[bool]string{false: "/mapped", true: "/passthrough"}[passthrough]+map[bool]string{false: "/health", true: "/intelligence"}[capture], func(t *testing.T) {
					account := healthProbeTestAccount(37, AccountTypeAPIKey)
					account.Status, account.Schedulable = StatusDisabled, false
					account.Credentials["api_key"] = "test-api-key"
					account.Credentials["base_url"] = "https://api.example.com/custom/v1"
					account.Credentials["model_mapping"] = map[string]any{"public-model": "gpt-5.4"}
					account.Credentials["header_override_enabled"] = true
					account.Credentials["header_overrides"] = map[string]any{"User-Agent": "custom-agent"}
					account.Extra = map[string]any{"openai_passthrough": passthrough, openai_compat.ExtraKeyResponsesSupported: protocol == APIProtocolResponses}
					proxyID := int64(8)
					account.ProxyID, account.Proxy = &proxyID, &Proxy{Protocol: "http", Host: "127.0.0.1", Port: 3128}
					svc, repo, upstream := openCodeProbeService(account, http.StatusOK, openCodeProbeSuccessStream(protocol))
					prompt := "  Return only my requested HTML.  "
					if capture {
						result, err := svc.TestAccountIntelligence(context.Background(), account.ID, "public-model", prompt, "high")
						require.NoError(t, err)
						require.True(t, result.Success, result.Error)
						require.Equal(t, "<html>answer</html>", result.OutputText)
					} else {
						result, err := svc.ProbeAccountHealth(context.Background(), account.ID, "public-model")
						require.NoError(t, err)
						require.True(t, result.Success, result.Error)
						require.NotNil(t, result.FirstTokenMS)
						require.LessOrEqual(t, *result.FirstTokenMS, result.DurationMS)
						prompt = "hi"
					}
					require.Len(t, upstream.requests, 1)
					req := upstream.requests[0]
					path := "/custom/v1/responses"
					if protocol == APIProtocolChatCompletions {
						path = "/custom/v1/chat/completions"
					}
					require.Equal(t, "api.example.com", req.URL.Host)
					require.Equal(t, path, req.URL.Path)
					require.Equal(t, "Bearer test-api-key", req.Header.Get("Authorization"))
					require.Empty(t, req.Header.Get("ChatGPT-Account-ID"))
					require.Empty(t, req.Header.Get("X-OpenCode-Session"))
					require.Equal(t, "custom-agent", req.Header.Get("User-Agent"))
					require.Equal(t, account.ID, upstream.accountID)
					require.Equal(t, "http://127.0.0.1:3128", upstream.proxyURL)
					body, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					model := "gpt-5.4"
					if passthrough {
						model = "public-model"
					}
					require.Equal(t, model, gjson.GetBytes(body, "model").String())
					promptPath, effortPath := "input.0.content.0.text", "reasoning.effort"
					if protocol == APIProtocolChatCompletions {
						promptPath, effortPath = "messages.0.content", "reasoning_effort"
					} else {
						require.True(t, gjson.GetBytes(body, "store").Exists())
						require.False(t, gjson.GetBytes(body, "store").Bool())
					}
					require.Equal(t, prompt, gjson.GetBytes(body, promptPath).String())
					if capture {
						require.Equal(t, "high", gjson.GetBytes(body, effortPath).String())
					}
					require.False(t, gjson.GetBytes(body, "instructions").Exists())
					require.Equal(t, StatusDisabled, account.Status)
					require.False(t, account.Schedulable)
					requireHealthProbeNoAccountWrites(t, repo)
				})
			}
		}
	}
}

func TestOpenAIAPIKeyAccountProbesRejectInvalidModelsAndMissingKeys(t *testing.T) {
	account := healthProbeTestAccount(38, AccountTypeAPIKey)
	account.Credentials["model_mapping"] = map[string]any{"text-alias": "gpt-image-2", "public-model": "gpt-5.4"}
	svc, repo, upstream := openCodeProbeService(account, http.StatusOK, "")
	for _, model := range []string{"text-alias", "unknown", "invalid model"} {
		_, err := svc.ProbeAccountHealth(context.Background(), account.ID, model)
		require.Error(t, err)
		_, err = svc.TestAccountIntelligence(context.Background(), account.ID, model, "My test prompt", "high")
		require.Error(t, err)
	}
	health, err := svc.ProbeAccountHealth(context.Background(), account.ID, "public-model")
	require.NoError(t, err)
	require.False(t, health.Success)
	require.Equal(t, "API key is missing", health.Error)
	intelligence, err := svc.TestAccountIntelligence(context.Background(), account.ID, "public-model", "My test prompt", "high")
	require.NoError(t, err)
	require.False(t, intelligence.Success)
	require.Equal(t, "API key is missing", intelligence.Error)
	require.Empty(t, upstream.requests)
	requireHealthProbeNoAccountWrites(t, repo)
}

func TestOpenAIAPIKeyAccountProbeAuthAndRateLimitFailuresKeepState(t *testing.T) {
	for _, protocol := range []string{APIProtocolResponses, APIProtocolChatCompletions} {
		for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
			account := healthProbeTestAccount(38, AccountTypeAPIKey)
			account.Credentials["api_key"] = "test-api-key"
			account.Extra = map[string]any{openai_compat.ExtraKeyResponsesSupported: protocol == APIProtocolResponses}
			account.Status, account.Schedulable = StatusDisabled, false
			for _, capture := range []bool{false, true} {
				svc, repo, upstream := openCodeProbeService(account, status, `{"error":{"code":"invalid_request","message":"Bearer test-api-key"}}`)
				if capture {
					result, err := svc.TestAccountIntelligence(context.Background(), account.ID, "gpt-5.4", "My test prompt", "high")
					require.NoError(t, err)
					require.False(t, result.Success)
					require.Empty(t, result.OutputText)
					require.Equal(t, status, *result.UpstreamStatusCode)
					require.NotContains(t, result.Error, "test-api-key")
				} else {
					result, err := svc.ProbeAccountHealth(context.Background(), account.ID, "gpt-5.4")
					require.NoError(t, err)
					require.False(t, result.Success)
					require.Equal(t, status, *result.UpstreamStatusCode)
					require.NotContains(t, result.Error, "test-api-key")
				}
				require.Len(t, upstream.requests, 1)
				require.Equal(t, StatusDisabled, account.Status)
				require.False(t, account.Schedulable)
				requireHealthProbeNoAccountWrites(t, repo)
			}
		}
	}
}
