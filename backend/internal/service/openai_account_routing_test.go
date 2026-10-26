package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIAccountRoutingOverrideBuilders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, platform, accountType string
		extra                       map[string]any
		want                        string
	}{
		{name: "missing defaults off", platform: PlatformOpenAI, accountType: AccountTypeOAuth},
		{name: "enabled OAuth", platform: PlatformOpenAI, accountType: AccountTypeOAuth, extra: map[string]any{openAIAccountRoutingUSCREnabledKey: true}, want: "us_cr"},
		{name: "disabled OAuth", platform: PlatformOpenAI, accountType: AccountTypeOAuth, extra: map[string]any{openAIAccountRoutingUSCREnabledKey: false}},
		{name: "string true is disabled", platform: PlatformOpenAI, accountType: AccountTypeOAuth, extra: map[string]any{openAIAccountRoutingUSCREnabledKey: "true"}},
		{name: "numeric true is disabled", platform: PlatformOpenAI, accountType: AccountTypeOAuth, extra: map[string]any{openAIAccountRoutingUSCREnabledKey: 1}},
		{name: "enabled setup token", platform: PlatformOpenAI, accountType: AccountTypeSetupToken, extra: map[string]any{openAIAccountRoutingUSCREnabledKey: true}, want: "us_cr"},
		{name: "API key unaffected", platform: PlatformOpenAI, accountType: AccountTypeAPIKey, extra: map[string]any{openAIAccountRoutingUSCREnabledKey: true}},
		{name: "other platform unaffected", platform: PlatformAnthropic, accountType: AccountTypeOAuth, extra: map[string]any{openAIAccountRoutingUSCREnabledKey: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{ID: 12, Platform: tc.platform, Type: tc.accountType, Extra: tc.extra}
			svc := &OpenAIGatewayService{}
			for _, transport := range []string{"http", "http_passthrough", "alpha_responses", "ws"} {
				t.Run(transport, func(t *testing.T) {
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					c.Request.Header.Set(openAIAccountRoutingOverrideHeader, "client-spoof")
					var headers http.Header
					if transport == "ws" {
						var err error
						headers, _, err = svc.buildOpenAIWSHeaders(context.Background(), c, account, "token", OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2}, true, "", "", "", "gpt-5.6", "")
						require.NoError(t, err)
					} else {
						var req *http.Request
						var err error
						if transport == "http_passthrough" {
							req, err = svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, []byte(`{"model":"gpt-5.6"}`), "token")
						} else if transport == "alpha_responses" {
							req, err = svc.buildOpenAIAlphaSearchResponsesWebSearchRequest(context.Background(), c, account, []byte(`{}`), []byte(`{"model":"gpt-5.6"}`), "token")
						} else {
							req, err = svc.buildUpstreamRequest(context.Background(), c, account, []byte(`{"model":"gpt-5.6"}`), "token", true, "", true)
						}
						require.NoError(t, err)
						headers = req.Header
					}
					require.Equal(t, tc.want, headers.Get(openAIAccountRoutingOverrideHeader))
					require.Equal(t, "client-spoof", c.Request.Header.Get(openAIAccountRoutingOverrideHeader), "构建出站不能改写客户端原始请求")
				})
			}
		})
	}
}

func TestOpenAIAccountRoutingOverrideShadowUsesSelectedAccount(t *testing.T) {
	parent := &Account{ID: 30, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Credentials: map[string]any{"chatgpt_account_id": "parent-org"}}
	shadow := &Account{ID: 31, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &parent.ID}
	svc := &OpenAIGatewayService{accountRepo: &stubChatGPTHeadersRepo{byID: map[int64]*Account{parent.ID: parent}}}
	for _, selectedEnabled := range []bool{false, true} {
		parent.Extra = map[string]any{openAIAccountRoutingUSCREnabledKey: !selectedEnabled}
		shadow.Extra = map[string]any{openAIAccountRoutingUSCREnabledKey: selectedEnabled}
		for _, transport := range []string{"http", "http_passthrough", "alpha_responses", "ws"} {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			var headers http.Header
			if transport == "ws" {
				var err error
				headers, _, err = svc.buildOpenAIWSHeaders(context.Background(), c, shadow, "token", OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2}, true, "", "", "", "gpt-5.6", "")
				require.NoError(t, err)
			} else {
				var req *http.Request
				var err error
				if transport == "http_passthrough" {
					req, err = svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, shadow, []byte(`{"model":"gpt-5.6"}`), "token")
				} else if transport == "alpha_responses" {
					req, err = svc.buildOpenAIAlphaSearchResponsesWebSearchRequest(context.Background(), c, shadow, []byte(`{}`), []byte(`{"model":"gpt-5.6"}`), "token")
				} else {
					req, err = svc.buildUpstreamRequest(context.Background(), c, shadow, []byte(`{"model":"gpt-5.6"}`), "token", true, "", true)
				}
				require.NoError(t, err)
				headers = req.Header
			}
			want := ""
			if selectedEnabled {
				want = "us_cr"
			}
			require.Equal(t, want, headers.Get(openAIAccountRoutingOverrideHeader), transport)
			require.Equal(t, "parent-org", headers.Get("chatgpt-account-id"), transport)
		}
	}
}
