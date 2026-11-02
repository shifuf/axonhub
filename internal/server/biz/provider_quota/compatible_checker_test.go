package provider_quota

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestCompatibleQuotaChecker_NewAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/usage/token/", r.URL.Path)
		require.Equal(t, "Bearer temporary-token", r.Header.Get("Authorization"))
		writeCompatibleJSON(t, w, map[string]any{
			"data": map[string]any{
				"total_granted":   1_000_000,
				"total_used":      250_000,
				"total_available": 750_000,
				"expires_at":      1_900_000_000,
			},
		})
	}))
	defer server.Close()

	quota, err := NewCompatibleQuotaChecker(httpclient.NewHttpClient()).CheckQuota(t.Context(), compatibleTestChannel(t, server.URL+"/v1"))

	require.NoError(t, err)
	require.Equal(t, "compatible", quota.ProviderType)
	require.Equal(t, "available", quota.Status)
	require.Equal(t, compatibleSourceNewAPI, quota.RawData["source"])
	require.InDelta(t, 1.5, quota.RawData["total_available_usd"], 1e-9)
	require.Len(t, quota.Limits, 1)
	require.InDelta(t, 0.25, quota.Limits[0].UsageRatio, 1e-9)
}

func TestCompatibleQuotaChecker_FallsBackToSub2API(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/usage/token/":
			http.NotFound(w, r)
		case "/v1/usage":
			writeCompatibleJSON(t, w, map[string]any{
				"data": map[string]any{
					"balance": 12.5,
					"usage": map[string]any{
						"today": map[string]any{"cost": 0.75, "requests": 12},
						"total": map[string]any{"cost": 8.25},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	quota, err := NewCompatibleQuotaChecker(httpclient.NewHttpClient()).CheckQuota(t.Context(), compatibleTestChannel(t, server.URL))

	require.NoError(t, err)
	require.Equal(t, compatibleSourceSub2API, quota.RawData["source"])
	require.InDelta(t, 12.5, quota.RawData["balance_usd"], 1e-9)
	require.InDelta(t, 0.75, quota.RawData["today_cost_usd"], 1e-9)
	require.Equal(t, "available", quota.Status)
}

func TestCompatibleQuotaChecker_OpenAIBilling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/dashboard/billing/subscription":
			writeCompatibleJSON(t, w, map[string]any{"hard_limit_usd": 100})
		case "/v1/dashboard/billing/usage":
			require.NotEmpty(t, r.URL.Query().Get("start_date"))
			require.NotEmpty(t, r.URL.Query().Get("end_date"))
			writeCompatibleJSON(t, w, map[string]any{"total_usage": 2500})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	quota, err := NewCompatibleQuotaChecker(httpclient.NewHttpClient()).CheckQuota(t.Context(), compatibleTestChannel(t, server.URL+"/v1"))

	require.NoError(t, err)
	require.Equal(t, compatibleSourceOpenAIBilling, quota.RawData["source"])
	require.InDelta(t, 75.0, quota.RawData["balance_usd"], 1e-9)
	require.Equal(t, "available", quota.Status)
}

func TestCompatibleQuotaChecker_DeepSeekMultiCurrency(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/balance":
			writeCompatibleJSON(t, w, map[string]any{
				"is_available": true,
				"balance_infos": []any{
					map[string]any{
						"currency":          "CNY",
						"total_balance":     "88.50",
						"granted_balance":   "10.00",
						"topped_up_balance": "78.50",
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	quota, err := NewCompatibleQuotaChecker(httpclient.NewHttpClient()).CheckQuota(t.Context(), compatibleTestChannel(t, server.URL))

	require.NoError(t, err)
	require.Equal(t, compatibleSourceDeepSeek, quota.RawData["source"])
	require.Equal(t, "available", quota.Status)
	balances, ok := quota.RawData["balances"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, balances, 1)
	require.Equal(t, "CNY", balances[0]["currency"])
	require.InDelta(t, 88.5, balances[0]["amount"], 1e-9)
}

func TestCompatibleQuotaChecker_RejectsUnusableResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeCompatibleJSON(t, w, map[string]any{"ok": true})
	}))
	defer server.Close()

	_, err := NewCompatibleQuotaChecker(httpclient.NewHttpClient()).CheckQuota(t.Context(), compatibleTestChannel(t, server.URL))

	require.Error(t, err)
	require.Contains(t, err.Error(), "no compatible balance endpoint")
}

func TestCompatibleQuotaChecker_SupportsGenericChannels(t *testing.T) {
	checker := NewCompatibleQuotaChecker(httpclient.NewHttpClient())

	for _, channelType := range []channel.Type{
		channel.TypeOpenai,
		channel.TypeOpenaiResponses,
		channel.TypeDeepseek,
		channel.TypeDeepseekAnthropic,
	} {
		require.True(t, checker.SupportsChannel(&ent.Channel{
			Type:    channelType,
			BaseURL: "https://relay.example.com/v1",
			Credentials: objects.ChannelCredentials{
				APIKey: "quota-token",
			},
		}))
	}
	require.False(t, checker.SupportsChannel(&ent.Channel{
		Type:    channel.TypeAnthropic,
		BaseURL: "https://relay.example.com/v1",
		Credentials: objects.ChannelCredentials{
			APIKey: "quota-token",
		},
	}))
}

func compatibleTestChannel(t *testing.T, baseURL string) *ent.Channel {
	t.Helper()
	return &ent.Channel{
		Type:    channel.TypeOpenai,
		BaseURL: baseURL,
		Credentials: objects.ChannelCredentials{
			APIKey: "temporary-token",
		},
	}
}

func writeCompatibleJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(value))
}
