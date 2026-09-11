package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newAttestationHTTPTestContext(value ...string) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	for _, item := range value {
		c.Request.Header.Add(openAIAttestationHeader, item)
	}
	return c
}

func TestOpenAIAttestationHTTPForwardingModeAndTarget(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	for _, tc := range []struct {
		name       string
		mode       string
		target     string
		wantHeader string
	}{
		{name: "off", mode: config.OpenAIAttestationModeOff, target: chatgptCodexURL},
		{name: "observe", mode: config.OpenAIAttestationModeObserve, target: chatgptCodexURL},
		{name: "http codex", mode: config.OpenAIAttestationModeHTTP, target: chatgptCodexURL, wantHeader: `{"v":1,"s":0,"t":"v1.test"}`},
		{name: "api key target", mode: config.OpenAIAttestationModeHTTP, target: openaiPlatformAPIURL},
		{name: "third party target", mode: config.OpenAIAttestationModeHTTP, target: "https://api.example.test/backend-api/codex/responses"},
		{name: "lookalike codex path", mode: config.OpenAIAttestationModeHTTP, target: "https://chatgpt.com/backend-api/codex/responses-archive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newAttestationHTTPTestContext(`{"v":1,"s":0,"t":"v1.test"}`)
			req := httptest.NewRequest(http.MethodPost, tc.target, nil)
			svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
				OpenAIAttestation: config.GatewayOpenAIAttestationConfig{Mode: tc.mode},
			}}}
			require.NoError(t, svc.applyOpenAIAttestationHTTPForwarding(c, req, account, tc.target))
			require.Equal(t, tc.wantHeader, req.Header.Get(openAIAttestationHeader))
		})
	}
}

func TestOpenAIAttestationHTTPForwardingRejectsMalformedInput(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		OpenAIAttestation: config.GatewayOpenAIAttestationConfig{Mode: config.OpenAIAttestationModeHTTP},
	}}}

	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "control", value: "v1\x00bad"},
		{name: "too long", value: strings.Repeat("a", openAIAttestationMaxBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newAttestationHTTPTestContext(tc.value)
			req := httptest.NewRequest(http.MethodPost, chatgptCodexURL, nil)
			err := svc.applyOpenAIAttestationHTTPForwarding(c, req, account, chatgptCodexURL)
			require.Error(t, err)
			require.Empty(t, req.Header.Get(openAIAttestationHeader))
		})
	}

	c := newAttestationHTTPTestContext(`{"v":1}`, `{"v":1}`)
	req := httptest.NewRequest(http.MethodPost, chatgptCodexURL, nil)
	require.Error(t, svc.applyOpenAIAttestationHTTPForwarding(c, req, account, chatgptCodexURL))
}

func TestOpenAIAttestationHTTPModeDoesNotEnableWSBridge(t *testing.T) {
	c := newAttestationHTTPTestContext(`{"v":1,"s":0}`)
	c.Set("openai_ws_http_bridge", true)
	req := httptest.NewRequest(http.MethodPost, chatgptCodexURL, nil)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		OpenAIAttestation: config.GatewayOpenAIAttestationConfig{Mode: config.OpenAIAttestationModeHTTP},
	}}}
	require.NoError(t, svc.applyOpenAIAttestationHTTPForwarding(c, req, &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, chatgptCodexURL))
	require.Empty(t, req.Header.Get(openAIAttestationHeader))
}
