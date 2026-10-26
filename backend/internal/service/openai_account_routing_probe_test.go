//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIAccountRoutingOverrideProbesUseLogicalAccount(t *testing.T) {
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
		for _, probe := range []string{"health", "intelligence", "connection", "compact"} {
			t.Run(accountType+"/"+probe, func(t *testing.T) {
				for _, enabled := range []bool{true, false} {
					parent := healthProbeTestAccount(50, accountType)
					parent.Status = StatusActive
					parent.Extra = map[string]any{openAIAccountRoutingUSCREnabledKey: !enabled}
					selected := healthProbeTestAccount(51, accountType)
					if accountType == AccountTypeOAuth {
						selected.ParentAccountID = &parent.ID
						selected.Credentials = map[string]any{}
					}
					selected.Extra = map[string]any{openAIAccountRoutingUSCREnabledKey: enabled}
					repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{parent.ID: parent, selected.ID: selected}}}
					body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\"}\n\n"
					if probe == "compact" {
						body = compactProbeSSESuccessBody
					}
					upstream := &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(http.StatusOK, body)}}
					svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
					switch probe {
					case "health":
						result, err := svc.ProbeAccountHealth(context.Background(), selected.ID, "gpt-5.4")
						require.NoError(t, err)
						require.True(t, result.Success, result.Error)
						requireHealthProbeNoAccountWrites(t, repo)
					case "intelligence":
						result, err := svc.TestAccountIntelligence(context.Background(), selected.ID, "gpt-5.4", "Return OK", "medium")
						require.NoError(t, err)
						require.True(t, result.Success, result.Error)
						requireHealthProbeNoAccountWrites(t, repo)
					default:
						c, _ := newTestContext()
						mode := ""
						if probe == "compact" {
							mode = AccountTestModeCompact
						}
						require.NoError(t, svc.TestAccountConnection(c, selected.ID, "gpt-5.4", "", mode))
					}
					require.Len(t, upstream.requests, 1)
					want := ""
					if enabled {
						want = "us_cr"
					}
					require.Equal(t, want, upstream.requests[0].Header.Get(openAIAccountRoutingOverrideHeader))
					require.Equal(t, "test-chatgpt-account", upstream.requests[0].Header.Get("chatgpt-account-id"))
				}
			})
		}
	}
}
