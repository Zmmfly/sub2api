package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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

	got := fetchChatGPTAccountInfo(context.Background(), newTestPrivacyClientFactory(t, server.URL, "access-token"), "access-token", "", "")
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
	svc := &OpenAIOAuthService{privacyClientFactory: newTestPrivacyClientFactory(t, server.URL, "access-token")}
	svc.enrichTokenInfo(context.Background(), tokenInfo, "")
	require.Equal(t, PrivacyModeTrainingOff, tokenInfo.PrivacyMode)

	require.Equal(t, "pro", tokenInfo.PlanType)
	require.Equal(t, personalActiveUntil, tokenInfo.SubscriptionExpiresAt,
		"到期时间必须来自个人订阅 active_until，不能是 workspace 的 entitlement.expires_at")
	require.NotEqual(t, workspaceExpiresAt, tokenInfo.SubscriptionExpiresAt)
	require.Equal(t, 1, subscriptionCalls)
}

// 单个人账号（poid == chatgpt_account_id）是绝大多数情况，行为必须保持不变：
// 直接用 accounts/check 的 entitlement，不额外打订阅端点。
func TestEnrichTokenInfo_KeepsEntitlementWhenAccountMatches(t *testing.T) {
	const personalAccountID = "personal-account-a"
	entitlementExpiresAt := time.Now().Add(720 * time.Hour).UTC().Format(time.RFC3339)

	subscriptionCalls := 0
	server := newChatGPTBackendTestServer(t, chatGPTBackendTestServerConfig{
		accountsCheck: map[string]any{
			"accounts": map[string]any{
				personalAccountID: map[string]any{
					"account": map[string]any{
						"account_id": personalAccountID,
						"plan_type":  "plus",
						"is_default": true,
					},
					"entitlement": map[string]any{"expires_at": entitlementExpiresAt},
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
		ChatGPTAccountID: personalAccountID,
		OrganizationID:   personalAccountID,
		PlanType:         "plus",
	}
	svc := &OpenAIOAuthService{privacyClientFactory: newTestPrivacyClientFactory(t, server.URL, "access-token")}
	svc.enrichTokenInfo(context.Background(), tokenInfo, "")
	require.Equal(t, PrivacyModeTrainingOff, tokenInfo.PrivacyMode)

	require.Equal(t, entitlementExpiresAt, tokenInfo.SubscriptionExpiresAt)
	require.Zero(t, subscriptionCalls, "账号一致时不应额外请求订阅端点")
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
	svc := &OpenAIOAuthService{privacyClientFactory: newTestPrivacyClientFactory(t, server.URL, "access-token")}
	svc.enrichTokenInfo(context.Background(), tokenInfo, "")
	require.Equal(t, PrivacyModeTrainingOff, tokenInfo.PrivacyMode)

	require.Equal(t, "self_serve_business_usage_based", tokenInfo.PlanType)
	require.Equal(t, workspaceExpiresAt, tokenInfo.SubscriptionExpiresAt,
		"套餐与到期时间必须来自同一条记录")
	require.Zero(t, subscriptionCalls)
}

type chatGPTBackendTestServerConfig struct {
	accountsCheck  map[string]any
	onSubscription func(accountID string) map[string]any
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

// newTestPrivacyClientFactory intercepts the privacy PATCH and permits only the
// exact local backend fixture origin. Unexpected requests fail before transport.
func newTestPrivacyClientFactory(t *testing.T, serverURL, accessToken string) PrivacyClientFactory {
	t.Helper()
	localURL, err := url.Parse(serverURL)
	require.NoError(t, err)
	return func(proxyURL string) (*req.Client, error) {
		if proxyURL != "" {
			t.Error("unexpected proxy in backend fixture")
			return nil, errors.New("unexpected proxy in backend fixture")
		}
		client := req.C().SetTimeout(time.Second)
		client.Transport.WrapRoundTripFunc(func(next http.RoundTripper) req.HttpRoundTripFunc {
			return func(r *http.Request) (*http.Response, error) {
				reject := func() (*http.Response, error) {
					t.Error("unexpected backend fixture request")
					return nil, errors.New("unexpected backend fixture request")
				}
				// Compare without printing the supplied Authorization value on failure.
				if r.Header.Get("Authorization") != "Bearer "+accessToken || r.URL.User != nil {
					return reject()
				}
				if r.URL.Scheme == "https" && r.URL.Host == "chatgpt.com" && r.URL.Path == "/backend-api/settings/account_user_setting" {
					query, err := url.ParseQuery(r.URL.RawQuery)
					if err != nil || r.Method != http.MethodPatch || r.URL.RawPath != "" || r.URL.Fragment != "" ||
						len(query) != 2 || len(query["feature"]) != 1 || query.Get("feature") != "training_allowed" ||
						len(query["value"]) != 1 || query.Get("value") != "false" {
						return reject()
					}
					if r.Body != nil {
						body, err := io.ReadAll(r.Body)
						_ = r.Body.Close()
						if err != nil || len(body) != 0 {
							return reject()
						}
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{}`)),
						Request:    r,
					}, nil
				}
				if r.URL.Scheme != localURL.Scheme || r.URL.Host != localURL.Host || r.Method != http.MethodGet ||
					(r.URL.Path != "/backend-api/accounts/check/v4-2023-04-27" && r.URL.Path != "/backend-api/subscriptions") {
					return reject()
				}
				return next.RoundTrip(r)
			}
		})
		return client, nil
	}
}
