//go:build unit && simulation

package repository

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type openCodeProbeSimulationRepo struct {
	service.AccountRepository
	account *service.Account
}

func (r *openCodeProbeSimulationRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	if id != r.account.ID {
		return nil, service.ErrAccountNotFound
	}
	return r.account, nil
}

// 仅账户读取由本地夹具提供；探针与正式 HTTP 传输真实运行，任何意外账户写入会命中未实现接口并失败。
func TestOpenCodeProbePostmanSimulation(t *testing.T) {
	base := strings.TrimRight(os.Getenv("TEST_OPENCODE_PROBE_SIMULATION_URL"), "/")
	require.NotEmpty(t, base, "start the opencode-account-tests Postman simulation first")
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	upstream := NewHTTPUpstream(cfg)
	for _, protocol := range []string{service.APIProtocolChatCompletions, service.APIProtocolResponses, service.APIProtocolAnthropic} {
		t.Run(protocol, func(t *testing.T) {
			for _, scenario := range []string{"ok", "empty", "truncated", "http503", "stream503"} {
				t.Run(scenario, func(t *testing.T) {
					model := "fixture-" + scenario
					account := &service.Account{
						ID: 31, Name: "OpenCode simulation", Platform: service.PlatformOpenCodeGo, Type: service.AccountTypeAPIKey,
						Status: service.StatusDisabled, Schedulable: false, Concurrency: 1,
						Credentials: map[string]any{
							"api_key": "synthetic-opencode-key", "account_mode": "go", "api_protocol": protocol,
							"base_url":      base + "/v1",
							"api_base_urls": map[string]any{protocol: base + "/v1"},
							"model_mapping": map[string]any{"public-model": model},
						},
					}
					repo := &openCodeProbeSimulationRepo{account: account}
					svc := service.NewAccountTestService(repo, nil, nil, nil, nil, upstream, cfg, nil)
					health, err := svc.ProbeAccountHealth(context.Background(), account.ID, "public-model")
					require.NoError(t, err)
					require.Equal(t, scenario == "ok", health.Success, health.Error)
					require.NotNil(t, health.UpstreamStatusCode)
					if scenario == "ok" {
						require.NotNil(t, health.FirstTokenMS)
						require.LessOrEqual(t, *health.FirstTokenMS, health.DurationMS)
					} else {
						require.NotEmpty(t, health.Error)
					}
					intelligence, err := svc.TestAccountIntelligence(context.Background(), account.ID, "public-model", "Return the requested test page.", "high")
					require.NoError(t, err)
					require.Equal(t, scenario == "ok", intelligence.Success, intelligence.Error)
					if scenario == "ok" {
						require.Equal(t, "<html>fixture answer</html>", intelligence.OutputText)
					} else {
						require.Empty(t, intelligence.OutputText)
						require.NotEmpty(t, intelligence.Error)
					}
					if scenario == "http503" {
						require.Equal(t, 503, *health.UpstreamStatusCode)
						require.Equal(t, 503, *intelligence.UpstreamStatusCode)
					}
					if scenario == "stream503" {
						require.NotNil(t, health.StreamErrorStatusCode)
						require.Equal(t, 503, *health.StreamErrorStatusCode)
						require.NotNil(t, intelligence.StreamErrorStatusCode)
						require.Equal(t, 503, *intelligence.StreamErrorStatusCode)
					}
					require.NotContains(t, health.Error+intelligence.Error, "synthetic-opencode-key")
					require.Equal(t, service.StatusDisabled, account.Status)
					require.False(t, account.Schedulable)
				})
			}
		})
	}
}
