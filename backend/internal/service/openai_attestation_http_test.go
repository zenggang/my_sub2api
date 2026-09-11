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

func TestOpenAIAttestationHTTPForwardingRemovesUnscopedExistingHeader(t *testing.T) {
	c := newAttestationHTTPTestContext(`{"v":1,"s":0,"t":"v1.client"}`)
	req := httptest.NewRequest(http.MethodPost, openaiPlatformAPIURL, nil)
	req.Header.Set(openAIAttestationHeader, "override-must-not-leak")
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		OpenAIAttestation: config.GatewayOpenAIAttestationConfig{Mode: config.OpenAIAttestationModeHTTP},
	}}}
	require.NoError(t, svc.applyOpenAIAttestationHTTPForwarding(c, req, &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, openaiPlatformAPIURL))
	require.Empty(t, req.Header.Get(openAIAttestationHeader))
}

func TestOpenAIAttestationHTTPForwardingRemovesNonCanonicalExistingHeader(t *testing.T) {
	c := newAttestationHTTPTestContext(`{"v":1,"s":0,"t":"v1.client"}`)
	req := httptest.NewRequest(http.MethodPost, openaiPlatformAPIURL, nil)
	req.Header["x-oai-attestation"] = []string{"override-must-not-leak"}
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		OpenAIAttestation: config.GatewayOpenAIAttestationConfig{Mode: config.OpenAIAttestationModeHTTP},
	}}}
	require.NoError(t, svc.applyOpenAIAttestationHTTPForwarding(c, req, &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, openaiPlatformAPIURL))
	for key := range req.Header {
		require.False(t, strings.EqualFold(key, openAIAttestationHeader), "unexpected attestation key %q", key)
	}
}

func TestOpenAIAttestationAllModeEnablesWSBridgeHTTPTurn(t *testing.T) {
	c := newAttestationHTTPTestContext(`{"v":1,"s":0,"t":"v1.bridge"}`)
	c.Set("openai_ws_http_bridge", true)
	req := httptest.NewRequest(http.MethodPost, chatgptCodexURL, nil)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		OpenAIAttestation: config.GatewayOpenAIAttestationConfig{Mode: config.OpenAIAttestationModeAll},
	}}}
	require.NoError(t, svc.applyOpenAIAttestationHTTPForwarding(c, req, &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, chatgptCodexURL))
	require.Equal(t, `{"v":1,"s":0,"t":"v1.bridge"}`, req.Header.Get(openAIAttestationHeader))
}

func TestOpenAIAttestationWSValidationUsesAllModeAndOAuthGate(t *testing.T) {
	cfg := &config.Config{Gateway: config.GatewayConfig{
		OpenAIAttestation: config.GatewayOpenAIAttestationConfig{Mode: config.OpenAIAttestationModeAll},
	}}
	oauth := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	apiKey := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	require.NoError(t, validateOpenAIAttestationForWS(cfg, oauth, http.Header{}))
	require.NoError(t, validateOpenAIAttestationForWS(cfg, oauth, http.Header{
		openAIAttestationHeader: []string{`{"v":1,"s":0,"t":"v1.test"}`},
	}))
	require.Error(t, validateOpenAIAttestationForWS(cfg, oauth, http.Header{
		openAIAttestationHeader: []string{"one", "two"},
	}))
	require.Error(t, validateOpenAIAttestationForWS(cfg, oauth, http.Header{
		openAIAttestationHeader: []string{"bad\x00value"},
	}))
	// API-key and non-all modes remain outside the attestation contract.
	require.NoError(t, validateOpenAIAttestationForWS(cfg, apiKey, http.Header{
		openAIAttestationHeader: []string{"bad\x00value", "second"},
	}))
	cfg.Gateway.OpenAIAttestation.Mode = config.OpenAIAttestationModeHTTP
	require.NoError(t, validateOpenAIAttestationForWS(cfg, oauth, http.Header{
		openAIAttestationHeader: []string{"bad\x00value", "second"},
	}))
}

func TestOpenAIAttestationFailoverGuardStopsOAuthAuthSwitchOnly(t *testing.T) {
	cfg := &config.Config{Gateway: config.GatewayConfig{
		OpenAIAttestation: config.GatewayOpenAIAttestationConfig{Mode: config.OpenAIAttestationModeHTTP},
	}}
	c := newAttestationHTTPTestContext(`{"v":1,"s":0,"t":"v1.guard"}`)
	svc := &OpenAIGatewayService{cfg: cfg}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	authErr := svc.guardOpenAIAttestationFailover(c, account, &UpstreamFailoverError{StatusCode: http.StatusForbidden, RetryableOnSameAccount: true})
	require.Equal(t, NextAccountStop, authErr.NextAccountAction)
	require.Equal(t, openAIAttestationFailoverReason, authErr.Reason)
	require.False(t, authErr.ShouldRetryNextAccount())

	capacityErr := svc.guardOpenAIAttestationFailover(c, account, &UpstreamFailoverError{StatusCode: http.StatusServiceUnavailable})
	require.True(t, capacityErr.ShouldRetryNextAccount(), "503 capacity behavior remains unchanged")
	SetOpenAIAttestationSwitchCount(c, 1)
	capacityAfterSwitch := svc.guardOpenAIAttestationFailover(c, account, &UpstreamFailoverError{StatusCode: http.StatusServiceUnavailable})
	require.False(t, capacityAfterSwitch.ShouldRetryNextAccount(), "a proof-bearing request gets at most one new OAuth account attempt")
}
