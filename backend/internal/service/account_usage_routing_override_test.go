package service

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	httppool "github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/stretchr/testify/require"
)

func TestAccountUsageService_ProbeOpenAICodexSnapshotUsesAccountRoutingOverride(t *testing.T) {
	requests := make(chan *http.Request, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(context.Background())
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	host, port, err := net.SplitHostPort(target.Host)
	require.NoError(t, err)
	portNumber, err := strconv.Atoi(port)
	require.NoError(t, err)
	proxy := &Proxy{ID: 987654, Protocol: "http", Host: host, Port: portNumber}
	// 唯一本地端口使此缓存键只属于本测试，不接触默认客户端或任何真实代理配置。
	client, err := httppool.GetClient(httppool.Options{ProxyURL: proxy.URL(), Timeout: 15 * time.Second, ResponseHeaderTimeout: 10 * time.Second})
	require.NoError(t, err)
	originalTransport := client.Transport
	localTransport := server.Client().Transport
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		localRequest := request.Clone(request.Context())
		localRequest.URL.Scheme = target.Scheme
		localRequest.URL.Host = target.Host
		return localTransport.RoundTrip(localRequest)
	})
	defer func() {
		client.Transport = originalTransport
		client.CloseIdleConnections()
	}()

	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
		for _, tc := range []struct {
			name  string
			extra map[string]any
			want  string
		}{
			{name: "missing defaults off"},
			{name: "enabled", extra: map[string]any{openAIAccountRoutingUSCREnabledKey: true}, want: "us_cr"},
			{name: "disabled", extra: map[string]any{openAIAccountRoutingUSCREnabledKey: false}},
			{name: "wrong type", extra: map[string]any{openAIAccountRoutingUSCREnabledKey: "true"}},
		} {
			t.Run(accountType+"/"+tc.name, func(t *testing.T) {
				account := &Account{ID: 91, Platform: PlatformOpenAI, Type: accountType, Credentials: map[string]any{"access_token": "local-probe-token"}, Extra: tc.extra, ProxyID: &proxy.ID, Proxy: proxy}
				updates, err := (&AccountUsageService{}).probeOpenAICodexSnapshot(context.Background(), account)
				require.NoError(t, err)
				require.Empty(t, updates)
				select {
				case request := <-requests:
					require.Equal(t, http.MethodPost, request.Method)
					require.Equal(t, "/backend-api/codex/responses", request.URL.Path)
					require.Equal(t, "chatgpt.com", request.Host)
					require.Equal(t, "Bearer local-probe-token", request.Header.Get("Authorization"))
					require.Equal(t, tc.want, request.Header.Get(openAIAccountRoutingOverrideHeader))
				case <-time.After(time.Second):
					t.Fatal("本地 TLS 服务未收到用量探针请求")
				}
			})
		}
	}
}
