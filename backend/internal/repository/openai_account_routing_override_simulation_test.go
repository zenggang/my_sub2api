//go:build unit && simulation

package repository

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type routingSimulationRepo struct {
	service.AccountRepository
	account *service.Account
}

func (r *routingSimulationRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	if id != r.account.ID {
		return nil, service.ErrAccountNotFound
	}
	return r.account, nil
}

type routingSimulationUpstream struct {
	service.HTTPUpstream
	t        *testing.T
	base     *url.URL
	caseName string
}

func (u *routingSimulationUpstream) Do(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	return u.HTTPUpstream.Do(u.localRequest(req), proxy, id, concurrency)
}

func (u *routingSimulationUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return u.HTTPUpstream.DoWithTLS(u.localRequest(req), proxy, id, concurrency, profile)
}

func (u *routingSimulationUpstream) localRequest(req *http.Request) *http.Request {
	require.Equal(u.t, "https://chatgpt.com/backend-api/codex/responses", req.URL.String())
	// 仅替换测试依赖地址；探针生成、路由头、真实仓储HTTP传输与SSE解析均运行正式代码。
	local := req.Clone(req.Context())
	target := *req.URL
	target.Scheme, target.Host = u.base.Scheme, u.base.Host
	target.Path = strings.TrimRight(u.base.Path, "/") + req.URL.Path
	local.URL, local.Host = &target, ""
	local.Header.Set("X-Routing-Fixture-Case", u.caseName)
	return local
}

func TestOpenAIAccountRoutingOverridePostmanSimulation(t *testing.T) {
	base := strings.TrimRight(os.Getenv("TEST_OPENAI_ACCOUNT_ROUTING_SIMULATION_URL"), "/")
	require.NotEmpty(t, base, "start the openai-account-routing Postman simulation first")
	parsed, err := url.Parse(base)
	require.NoError(t, err)
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	upstream := &routingSimulationUpstream{HTTPUpstream: NewHTTPUpstream(cfg), t: t, base: parsed}
	want := map[string]string{}
	for _, accountType := range []string{service.AccountTypeOAuth, service.AccountTypeSetupToken} {
		account := &service.Account{
			ID: 101, Platform: service.PlatformOpenAI, Type: accountType, Concurrency: 1,
			Credentials: map[string]any{"access_token": "synthetic-token", "chatgpt_account_id": "synthetic-account"},
		}
		repo := &routingSimulationRepo{account: account}
		probes := service.NewAccountTestService(repo, nil, nil, nil, nil, upstream, cfg, nil)
		for _, state := range []struct {
			name    string
			value   any
			routing string
		}{
			{"missing", nil, ""}, {"enabled", true, "us_cr"}, {"disabled_after_enabled", false, ""}, {"wrong_type", "true", ""},
		} {
			t.Run(accountType+"/"+state.name, func(t *testing.T) {
				upstream.t = t
				account.Extra = map[string]any{}
				if state.value != nil {
					account.Extra["openai_account_routing_us_cr_enabled"] = state.value
				}
				upstream.caseName = accountType + "/" + state.name
				want[upstream.caseName] = state.routing
				health, err := probes.ProbeAccountHealth(context.Background(), account.ID, "gpt-6.1-sol")
				require.NoError(t, err)
				require.True(t, health.Success, health.Error)
				intelligence, err := probes.TestAccountIntelligence(context.Background(), account.ID, "gpt-6.1-sol", "Return the requested HTML.", "low")
				require.NoError(t, err)
				require.True(t, intelligence.Success, intelligence.Error)
				require.Equal(t, "<html>routing fixture</html>", intelligence.OutputText)
			})
		}
	}
	response, err := http.Get(base + "/_requests")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var captured []struct {
		Case    string `json:"case"`
		Routing string `json:"routing"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&captured))
	require.Len(t, captured, 16)
	counts := map[string]int{}
	for _, request := range captured {
		expected, exists := want[request.Case]
		require.True(t, exists, request.Case)
		require.Equal(t, expected, request.Routing, request.Case)
		counts[request.Case]++
	}
	for name := range want {
		require.Equal(t, 2, counts[name], name)
	}
}
