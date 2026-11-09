package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

func TestFetchChatGPTSubscriptionExpiresAt(t *testing.T) {
	const wantExpiresAt = "2026-06-10T02:52:15Z"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/backend-api/subscriptions", r.URL.Path)
		require.Equal(t, "acc_123", r.URL.Query().Get("account_id"))
		require.Equal(t, "Bearer access-token", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type":    "plus",
			"active_until": wantExpiresAt,
			"will_renew":   true,
			"id":           "sub_123",
		})
	}))
	defer server.Close()

	oldURL := chatGPTSubscriptionsURL
	chatGPTSubscriptionsURL = server.URL + "/backend-api/subscriptions"
	t.Cleanup(func() { chatGPTSubscriptionsURL = oldURL })

	got := fetchChatGPTSubscriptionExpiresAt(context.Background(), func(proxyURL string) (*req.Client, error) {
		return req.C().SetTimeout(5 * time.Second), nil
	}, "access-token", "", "acc_123")

	require.Equal(t, wantExpiresAt, got)
}

func TestFetchChatGPTAccountInfo_SkipsExpiredWorkspaceCandidate(t *testing.T) {
	expiredAt := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/backend-api/accounts/check/v4-2023-04-27", r.URL.Path)
		require.Equal(t, "Bearer access-token", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accounts": map[string]any{
				"org-expired-workspace": map[string]any{
					"account": map[string]any{
						"plan_type":  "self_serve_business_usage_based",
						"is_default": true,
					},
					"entitlement": map[string]any{
						"expires_at": expiredAt,
					},
				},
				"personal-account": map[string]any{
					"account": map[string]any{
						"plan_type": "free",
					},
				},
			},
		})
	}))
	defer server.Close()

	oldURL := chatGPTAccountsCheckURL
	chatGPTAccountsCheckURL = server.URL + "/backend-api/accounts/check/v4-2023-04-27"
	t.Cleanup(func() { chatGPTAccountsCheckURL = oldURL })

	got := fetchChatGPTAccountInfo(context.Background(), func(proxyURL string) (*req.Client, error) {
		return req.C().SetTimeout(5 * time.Second), nil
	}, "access-token", "", "org-expired-workspace")

	require.NotNil(t, got)
	require.Equal(t, "free", got.PlanType)
	require.Empty(t, got.SubscriptionExpiresAt)
}

func TestFetchChatGPTAccountInfo_SkipsDeactivatedWorkspaceCandidate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/backend-api/accounts/check/v4-2023-04-27", r.URL.Path)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accounts": map[string]any{
				"org-deactivated-workspace": map[string]any{
					"account": map[string]any{
						"plan_type":      "self_serve_business_usage_based",
						"is_default":     true,
						"is_deactivated": true,
					},
				},
				"personal-account": map[string]any{
					"account": map[string]any{
						"plan_type": "pro",
					},
				},
			},
		})
	}))
	defer server.Close()

	oldURL := chatGPTAccountsCheckURL
	chatGPTAccountsCheckURL = server.URL + "/backend-api/accounts/check/v4-2023-04-27"
	t.Cleanup(func() { chatGPTAccountsCheckURL = oldURL })

	got := fetchChatGPTAccountInfo(context.Background(), func(proxyURL string) (*req.Client, error) {
		return req.C().SetTimeout(5 * time.Second), nil
	}, "access-token", "", "org-deactivated-workspace")

	require.NotNil(t, got)
	require.Equal(t, "pro", got.PlanType)
}

func TestShouldApplyChatGPTAccountInfoPlanType(t *testing.T) {
	require.False(t, shouldApplyChatGPTAccountInfoPlanType("pro", "self_serve_business_usage_based"))
	require.False(t, shouldApplyChatGPTAccountInfoPlanType("free", "team"))
	require.False(t, shouldApplyChatGPTAccountInfoPlanType("", ""))
	require.True(t, shouldApplyChatGPTAccountInfoPlanType("", "pro"))
}

func TestChatGPTAccountInfoBelongsToTokenAccount(t *testing.T) {
	require.False(t, chatGPTAccountInfoBelongsToTokenAccount(
		&OpenAITokenInfo{ChatGPTAccountID: "personal-a"}, &ChatGPTAccountInfo{AccountID: "workspace-b"}))
	require.True(t, chatGPTAccountInfoBelongsToTokenAccount(
		&OpenAITokenInfo{ChatGPTAccountID: "personal-a"}, &ChatGPTAccountInfo{AccountID: "PERSONAL-A"}))
	// 任一侧缺 ID 时无法区分，保持既有行为（采用 accounts/check 的值）。
	require.True(t, chatGPTAccountInfoBelongsToTokenAccount(
		&OpenAITokenInfo{}, &ChatGPTAccountInfo{AccountID: "workspace-b"}))
	require.True(t, chatGPTAccountInfoBelongsToTokenAccount(
		&OpenAITokenInfo{ChatGPTAccountID: "personal-a"}, &ChatGPTAccountInfo{}))
}

// accounts 的 map key 可能是 "default" 这类别名，account.account_id 才是账号标识。
func TestFetchChatGPTAccountInfo_ReportsAccountID(t *testing.T) {
	futureAt := time.Now().Add(720 * time.Hour).UTC().Format(time.RFC3339)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accounts": map[string]any{
				"default": map[string]any{
					"account": map[string]any{
						"account_id": "personal-account-a",
						"plan_type":  "plus",
						"is_default": true,
					},
					"entitlement": map[string]any{"expires_at": futureAt},
				},
			},
		})
	}))
	defer server.Close()

	oldURL := chatGPTAccountsCheckURL
	chatGPTAccountsCheckURL = server.URL + "/backend-api/accounts/check/v4-2023-04-27"
	t.Cleanup(func() { chatGPTAccountsCheckURL = oldURL })

	got := fetchChatGPTAccountInfo(context.Background(), newTestPrivacyClientFactory(), "access-token", "", "")
	require.NotNil(t, got)
	require.Equal(t, "plus", got.PlanType)
	require.Equal(t, "personal-account-a", got.AccountID, "应优先取 account.account_id 而不是 map key")
}

// issue #5459：poid 指向的默认 Personal workspace 与 chatgpt_account_id 是两个不同的
// 标识时，accounts/check 返回的是 workspace 的 entitlement.expires_at。plan_type 那侧
// 已被 shouldApplyChatGPTAccountInfoPlanType 挡住（保留 JWT 里的个人套餐），到期时间
// 这侧原先无条件覆盖，于是显示成「个人 Pro + workspace 到期时间」。
func TestEnrichTokenInfo_WorkspaceEntitlementDoesNotOverridePersonalSubscription(t *testing.T) {
	const (
		personalAccountID   = "personal-account-a"
		workspaceAccountID  = "personal-workspace-b"
		personalActiveUntil = "2027-03-01T00:00:00Z"
	)
	workspaceExpiresAt := time.Now().Add(720 * time.Hour).UTC().Format(time.RFC3339)

	subscriptionCalls := 0
	server := newChatGPTBackendTestServer(t, chatGPTBackendTestServerConfig{
		accountsCheck: map[string]any{
			"accounts": map[string]any{
				workspaceAccountID: map[string]any{
					"account": map[string]any{
						"account_id": workspaceAccountID,
						"plan_type":  "pro",
						"is_default": true,
					},
					"entitlement": map[string]any{"expires_at": workspaceExpiresAt},
				},
			},
		},
		onSubscription: func(accountID string) map[string]any {
			subscriptionCalls++
			require.Equal(t, personalAccountID, accountID,
				"必须用个人 chatgpt_account_id 查订阅，而不是 poid workspace")
			return map[string]any{"plan_type": "pro", "active_until": personalActiveUntil, "will_renew": true}
		},
	})
	defer server.Close()

	tokenInfo := &OpenAITokenInfo{
		AccessToken:      "access-token",
		ChatGPTAccountID: personalAccountID,
		OrganizationID:   workspaceAccountID,
		PlanType:         "pro", // 来自 id_token 的个人套餐
	}
	svc := &OpenAIOAuthService{privacyClientFactory: newTestPrivacyClientFactory()}
	svc.enrichTokenInfo(context.Background(), tokenInfo, "")

	require.Equal(t, "pro", tokenInfo.PlanType)
	require.Equal(t, personalActiveUntil, tokenInfo.SubscriptionExpiresAt,
		"到期时间必须来自个人订阅 active_until，不能是 workspace 的 entitlement.expires_at")
	require.NotEqual(t, workspaceExpiresAt, tokenInfo.SubscriptionExpiresAt)
	require.Equal(t, 1, subscriptionCalls)
}

// 同一账号的权益期限也可能晚于订阅期限，必须按实际 active_until 构建保存凭据。
func TestEnrichTokenInfo_PersonalSubscriptionPrefersActiveUntil(t *testing.T) {
	const personalAccountID = "personal-account-a"
	entitlementExpiresAt := time.Now().Add(720 * time.Hour).UTC().Format(time.RFC3339)
	activeUntil := time.Now().Add(714 * time.Hour).UTC().Format(time.RFC3339)

	for _, plan := range []string{"free", "go", "plus", "prolite", "pro", "promax", ""} {
		t.Run("plan="+plan, func(t *testing.T) {
			accountPlan := plan
			if accountPlan == "" {
				accountPlan = "promax"
			}
			subscriptionCalls := 0
			server := newChatGPTBackendTestServer(t, chatGPTBackendTestServerConfig{
				accountsCheck: map[string]any{
					"accounts": map[string]any{
						personalAccountID: map[string]any{
							"account":     map[string]any{"account_id": personalAccountID, "plan_type": accountPlan},
							"entitlement": map[string]any{"expires_at": entitlementExpiresAt},
						},
					},
				},
				onSubscription: func(accountID string) map[string]any {
					subscriptionCalls++
					require.Equal(t, personalAccountID, accountID)
					return map[string]any{"plan_type": accountPlan, "active_until": activeUntil}
				},
			})
			defer server.Close()

			tokenInfo := &OpenAITokenInfo{
				AccessToken:           "access-token",
				ChatGPTAccountID:      personalAccountID,
				OrganizationID:        personalAccountID,
				PlanType:              plan,
				SubscriptionExpiresAt: entitlementExpiresAt,
			}
			svc := &OpenAIOAuthService{privacyClientFactory: newTestPrivacyClientFactory()}
			svc.enrichTokenInfo(context.Background(), tokenInfo, "")

			require.Equal(t, accountPlan, tokenInfo.PlanType)
			require.Equal(t, activeUntil, tokenInfo.SubscriptionExpiresAt)
			require.Equal(t, activeUntil, svc.BuildAccountCredentials(tokenInfo)["subscription_expires_at"])
			require.Equal(t, 1, subscriptionCalls)
		})
	}
}

func TestOpenAITokenRefresher_SubscriptionLookupFailurePreservesSavedExpiry(t *testing.T) {
	const personalAccountID = "personal-account-a"
	savedExpiry := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	entitlementExpiresAt := time.Now().Add(30 * time.Hour).UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		name        string
		status      int
		activeUntil string
	}{
		{name: "upstream unavailable", status: http.StatusServiceUnavailable},
		{name: "missing active_until"},
		{name: "invalid active_until", activeUntil: "not-a-date"},
		{name: "missing timezone", activeUntil: "2027-01-01T15:11:19"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newChatGPTBackendTestServer(t, chatGPTBackendTestServerConfig{
				accountsCheck: map[string]any{"accounts": map[string]any{
					personalAccountID: map[string]any{
						"account":     map[string]any{"account_id": personalAccountID, "plan_type": "promax"},
						"entitlement": map[string]any{"expires_at": entitlementExpiresAt},
					},
				}},
				subscriptionStatus: tc.status,
				onSubscription: func(string) map[string]any {
					return map[string]any{"plan_type": "promax", "active_until": tc.activeUntil}
				},
			})
			defer server.Close()
			svc := &OpenAIOAuthService{privacyClientFactory: newTestPrivacyClientFactory()}
			refresher := NewOpenAITokenRefresher(svc, nil)
			account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{
				"access_token": "access-token", "chatgpt_account_id": personalAccountID,
				"organization_id": personalAccountID, "plan_type": "promax",
				"subscription_expires_at": savedExpiry,
			}}

			credentials, err := refresher.Refresh(context.Background(), account)
			require.NoError(t, err, "订阅信息补全失败不应使令牌刷新失败")
			require.Equal(t, savedExpiry, credentials["subscription_expires_at"])

			delete(account.Credentials, "subscription_expires_at")
			credentials, err = refresher.Refresh(context.Background(), account)
			require.NoError(t, err)
			require.NotContains(t, credentials, "subscription_expires_at", "首次获取失败时不能用权益时间冒充订阅时间")
		})
	}
}

type openaiSubscriptionRefreshClient struct {
	openaiOAuthClientRefreshStub
	response *openai.TokenResponse
}

func (s *openaiSubscriptionRefreshClient) RefreshTokenWithClientID(context.Context, string, string, string) (*openai.TokenResponse, error) {
	return s.response, nil
}

func TestOpenAITokenRefresher_RefreshPersistsSubscriptionExpiry(t *testing.T) {
	const personalAccountID = "personal-account-a"
	savedExpiry := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	activeUntil := time.Now().Add(48 * time.Hour).In(time.FixedZone("UTC+8", 8*60*60)).Format(time.RFC3339)
	entitlementExpiresAt := time.Now().Add(54 * time.Hour).UTC().Format(time.RFC3339)
	for _, success := range []bool{true, false} {
		t.Run(map[bool]string{true: "subscription available", false: "subscription unavailable"}[success], func(t *testing.T) {
			server := newChatGPTBackendTestServer(t, chatGPTBackendTestServerConfig{
				accountsCheck: map[string]any{"accounts": map[string]any{
					personalAccountID: map[string]any{
						"account":     map[string]any{"account_id": personalAccountID, "plan_type": "promax"},
						"entitlement": map[string]any{"expires_at": entitlementExpiresAt},
					},
				}},
				onSubscription: func(accountID string) map[string]any {
					require.Equal(t, personalAccountID, accountID)
					if !success {
						return map[string]any{}
					}
					return map[string]any{"plan_type": "promax", "active_until": activeUntil}
				},
			})
			defer server.Close()
			claims := `{"https://api.openai.com/auth":{"chatgpt_account_id":"personal-account-a","chatgpt_plan_type":"promax"}}`
			client := &openaiSubscriptionRefreshClient{response: &openai.TokenResponse{
				AccessToken: "new-access-token", RefreshToken: "new-refresh-token", ExpiresIn: 3600,
				IDToken: "e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".signature",
			}}
			svc := &OpenAIOAuthService{oauthClient: client, privacyClientFactory: newTestPrivacyClientFactory()}
			account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{
				"access_token": "old-access-token", "refresh_token": "old-refresh-token",
				"subscription_expires_at": savedExpiry, "model_mapping": map[string]string{"source": "target"},
			}}

			credentials, err := NewOpenAITokenRefresher(svc, nil).Refresh(context.Background(), account)
			require.NoError(t, err)
			wantExpiry := savedExpiry
			if success {
				wantExpiry = activeUntil
			}
			require.Equal(t, wantExpiry, credentials["subscription_expires_at"])
			require.Equal(t, "new-access-token", credentials["access_token"])
			require.Equal(t, "new-refresh-token", credentials["refresh_token"])
			require.Equal(t, account.Credentials["model_mapping"], credentials["model_mapping"])
		})
	}
}

// 反向不变式：套餐本身就取自 accounts/check（JWT 没有 plan_type）时，到期时间必须
// 跟着取同一条记录，否则会变成「workspace 套餐 + 个人到期时间」的另一种错配。
func TestEnrichTokenInfo_WorkspacePlanTypeKeepsItsOwnExpiry(t *testing.T) {
	const workspaceAccountID = "workspace-b"
	workspaceExpiresAt := time.Now().Add(720 * time.Hour).UTC().Format(time.RFC3339)

	subscriptionCalls := 0
	server := newChatGPTBackendTestServer(t, chatGPTBackendTestServerConfig{
		accountsCheck: map[string]any{
			"accounts": map[string]any{
				workspaceAccountID: map[string]any{
					"account": map[string]any{
						"account_id": workspaceAccountID,
						"plan_type":  "self_serve_business_usage_based",
						"is_default": true,
					},
					"entitlement": map[string]any{"expires_at": workspaceExpiresAt},
				},
			},
		},
		onSubscription: func(string) map[string]any {
			subscriptionCalls++
			return map[string]any{}
		},
	})
	defer server.Close()

	tokenInfo := &OpenAITokenInfo{
		AccessToken:      "access-token",
		ChatGPTAccountID: "personal-account-a",
		OrganizationID:   workspaceAccountID,
		// id_token 没带 chatgpt_plan_type
	}
	svc := &OpenAIOAuthService{privacyClientFactory: newTestPrivacyClientFactory()}
	svc.enrichTokenInfo(context.Background(), tokenInfo, "")

	require.Equal(t, "self_serve_business_usage_based", tokenInfo.PlanType)
	require.Equal(t, workspaceExpiresAt, tokenInfo.SubscriptionExpiresAt,
		"套餐与到期时间必须来自同一条记录")
	require.Zero(t, subscriptionCalls)
}

type chatGPTBackendTestServerConfig struct {
	accountsCheck      map[string]any
	onSubscription     func(accountID string) map[string]any
	subscriptionStatus int
}

// newChatGPTBackendTestServer 同时接管 accounts/check 与 subscriptions 两个端点，
// 并在 t.Cleanup 里还原包级 URL 变量。
func newChatGPTBackendTestServer(t *testing.T, cfg chatGPTBackendTestServerConfig) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/backend-api/accounts/check/v4-2023-04-27":
			_ = json.NewEncoder(w).Encode(cfg.accountsCheck)
		case "/backend-api/subscriptions":
			if cfg.subscriptionStatus != 0 {
				w.WriteHeader(cfg.subscriptionStatus)
			}
			body := map[string]any{}
			if cfg.onSubscription != nil {
				body = cfg.onSubscription(r.URL.Query().Get("account_id"))
			}
			_ = json.NewEncoder(w).Encode(body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	oldAccounts, oldSubscriptions := chatGPTAccountsCheckURL, chatGPTSubscriptionsURL
	chatGPTAccountsCheckURL = server.URL + "/backend-api/accounts/check/v4-2023-04-27"
	chatGPTSubscriptionsURL = server.URL + "/backend-api/subscriptions"
	t.Cleanup(func() {
		chatGPTAccountsCheckURL, chatGPTSubscriptionsURL = oldAccounts, oldSubscriptions
	})
	return server
}

// 本组测试使用真实 HTTP 客户端访问本地替身，隐私设置等其他请求在出站前拒绝。
func newTestPrivacyClientFactory() PrivacyClientFactory {
	return func(string) (*req.Client, error) {
		return req.C().SetTimeout(time.Second).OnBeforeRequest(func(_ *req.Client, request *req.Request) error {
			target, err := url.Parse(request.RawURL)
			if err != nil || target.Hostname() != "127.0.0.1" {
				return errors.New("external requests disabled in subscription tests")
			}
			return nil
		}), nil
	}
}
