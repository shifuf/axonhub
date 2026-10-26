package biz

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/pkg/xcache/live"
	"github.com/looplj/axonhub/llm/httpclient"
)

func newTestChannelService(client *ent.Client) *ChannelService {
	mockSysSvc := &SystemService{
		AbstractService: &AbstractService{
			db: client,
		},
		Cache: xcache.NewFromConfig[ent.System](xcache.Config{Mode: xcache.ModeMemory}),
	}

	svc := &ChannelService{
		AbstractService: &AbstractService{
			db: client,
		},
		SystemService:               mockSysSvc,
		WebhookNotifier:             NewWebhookNotifier(mockSysSvc, httpclient.NewHttpClient()),
		channelPerfMetrics:          make(map[int]*channelMetrics),
		windowedRuleActionsInFlight: make(map[int]map[string]bool),
		cancellationWindow:          make(map[int]map[string][]time.Time),
		cancellationActionsInFlight: make(map[int]map[string]bool),
		perfWindowSeconds:           600,
	}

	svc.enabledChannelsCache = live.NewCache(live.Options[[]*Channel]{
		Name:            "test_enabled_channels",
		InitialValue:    []*Channel{},
		RefreshInterval: time.Hour,
		RefreshFunc:     svc.reloadEnabledChannels,
		OnSwap:          svc.onEnabledChannelsSwap,
	})

	return svc
}

func createTestChannelWithAPIKeys(t *testing.T, client *ent.Client, ctx context.Context, name string, apiKeys []string) *ent.Channel {
	t.Helper()

	creds := objects.ChannelCredentials{
		APIKeys: apiKeys,
	}

	ch, err := client.Channel.Create().
		SetName(name).
		SetType(channel.TypeOpenai).
		SetBaseURL("https://api.openai.com").
		SetCredentials(creds).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetStatus(channel.StatusEnabled).
		Save(ctx)
	require.NoError(t, err)

	return ch
}

func withGlobalAutoDisable(t *testing.T, svc *ChannelService, ctx context.Context, ch *ent.Channel, rules []objects.APIKeyAutoDisableRule) {
	t.Helper()
	require.NoError(t, svc.SystemService.SetRetryPolicy(ctx, &RetryPolicy{
		AutoDisableChannel: AutoDisableChannel{Enabled: true, Rules: rules},
	}))
	svc.SetEnabledChannelsForTest([]*Channel{buildChannel(ch, nil)})
}

func legacyRuleCounterKey(perf *PerformanceRecord, scope autoDisableScope, index int, rule objects.APIKeyAutoDisableRule) string {
	effective := effectiveModelFreezeRule(rule)
	return apiKeyRuleCounterKey(autoDisableRuleIdentity(perf, effective), scope, index, effective)
}

func requireLegacyRuleModelCooldown(t *testing.T, ch *ent.Channel, model string, minutes int) objects.ModelCooldown {
	t.Helper()
	require.Equal(t, channel.StatusEnabled, ch.Status)
	require.Empty(t, ch.DisabledAPIKeys)
	require.Nil(t, ch.AutoDisabledAt)
	require.Nil(t, ch.AutoDisableExpiresAt)
	require.Len(t, ch.ModelCooldowns, 1)
	cooldown := ch.ModelCooldowns[0]
	require.Equal(t, model, cooldown.Model)
	require.True(t, cooldown.IsActive())
	require.NotNil(t, cooldown.ExpiresAt)
	require.WithinDuration(t, time.Now().Add(time.Duration(minutes)*time.Minute), *cooldown.ExpiresAt, 5*time.Second)
	require.True(t, buildChannel(ch, nil).HasActiveModelCooldown(model, time.Now()))
	return cooldown
}

func TestChannelService_GlobalAutoDisableModelSharesAPIKeyFailures(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "test-channel", []string{"key1", "key2", "key3"})
	withGlobalAutoDisable(t, svc, ctx, ch, []objects.APIKeyAutoDisableRule{{
		StatusCodes: []int{401}, Times: 3, Action: objects.APIKeyAutoDisableActionPermanent,
	}})
	for i, key := range []string{"key1", "key2", "key3"} {
		matched, acted := svc.evaluateAutoDisableForFailure(ctx, &PerformanceRecord{
			ChannelID: ch.ID, Model: "gpt-4", APIKey: key, ResponseStatusCode: 401,
		})
		require.True(t, matched)
		require.Equal(t, i == 2, acted)
		updated, err := client.Channel.Get(ctx, ch.ID)
		require.NoError(t, err)
		require.Equal(t, channel.StatusEnabled, updated.Status)
		require.Empty(t, updated.DisabledAPIKeys)
		require.Equal(t, ch.Credentials, updated.Credentials)
		if i < 2 {
			require.Empty(t, updated.ModelCooldowns)
		} else {
			requireLegacyRuleModelCooldown(t, updated, "gpt-4", defaultAutoDisableCooldownMinutes)
		}
	}
	matched, acted := svc.evaluateAutoDisableForFailure(ctx, &PerformanceRecord{
		ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 500,
	})
	require.False(t, matched)
	require.False(t, acted)
}

func TestChannelService_GlobalAutoDisableKeylessModel(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "test-channel-no-keys", []string{})
	withGlobalAutoDisable(t, svc, ctx, ch, []objects.APIKeyAutoDisableRule{{
		StatusCodes: []int{401}, Times: 2, Action: objects.APIKeyAutoDisableActionPermanent,
	}})
	perf := &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", ResponseStatusCode: 401}
	_, acted := svc.evaluateAutoDisableForFailure(ctx, perf)
	require.False(t, acted)
	_, acted = svc.evaluateAutoDisableForFailure(ctx, perf)
	require.True(t, acted)
	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireLegacyRuleModelCooldown(t, updated, "gpt-4", defaultAutoDisableCooldownMinutes)
	require.Empty(t, updated.Credentials.GetAllCredentialRefs())
}

func TestChannelService_LegacyAutoDisableWithoutModelDoesNotAct(t *testing.T) {
	for _, key := range []string{"key1", ""} {
		t.Run(fmt.Sprintf("keyed=%v", key != ""), func(t *testing.T) {
			rule := objects.APIKeyAutoDisableRule{StatusCodes: []int{401}, Times: 1, Action: objects.APIKeyAutoDisableActionPermanentDelete}
			ctx, svc, ch := newWindowRuleTestService(t, rule)
			for range 6 {
				matched, acted := svc.checkAndHandleChannelAPIKeyRules(ctx, &PerformanceRecord{
					ChannelID: ch.ID, APIKey: key, ResponseStatusCode: 401,
				})
				require.True(t, matched)
				require.False(t, acted)
			}
			updated, err := svc.db.Channel.Get(ctx, ch.ID)
			require.NoError(t, err)
			require.Equal(t, channel.StatusEnabled, updated.Status)
			require.Empty(t, updated.DisabledAPIKeys)
			require.Empty(t, updated.ModelCooldowns)
			require.Equal(t, ch.Credentials, updated.Credentials)
			require.Zero(t, countChannelFailureEventsByChannelForTest(t, svc, ch.ID))
		})
	}
}

func TestChannelService_markChannelUnavailable_RefreshesStaleLocalCacheWhenAlreadyDisabled(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	svc := newTestChannelService(client)
	defer svc.enabledChannelsCache.Stop()

	ch := createTestChannelWithAPIKeys(t, client, ctx, "stale-cache-channel", []string{"key1"})

	require.NoError(t, svc.enabledChannelsCache.Load(ctx, true))
	require.NotNil(t, svc.GetEnabledChannel(ch.ID), "precondition: local cache should contain enabled channel")

	_, err := client.Channel.UpdateOneID(ch.ID).
		SetStatus(channel.StatusDisabled).
		SetErrorMessage("disabled elsewhere").
		Save(ctx)
	require.NoError(t, err)

	svc.markChannelUnavailable(ctx, ch.ID, 401, 2, 2, nil, "")

	require.Nil(t, svc.GetEnabledChannel(ch.ID), "local cache should be refreshed even when DB row was already disabled")
}

func TestChannelService_DisableAllAPIKeysDisablesChannel(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	svc := newTestChannelService(client)

	// Create a channel with 2 API keys
	ch := createTestChannelWithAPIKeys(t, client, ctx, "test-channel-2-keys", []string{"key1", "key2"})

	// Disable first key
	err := svc.DisableAPIKey(ctx, ch.ID, "key1", 401, "Test reason 1")
	require.NoError(t, err)

	// Verify channel is still enabled
	updatedCh, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, updatedCh.Status)
	require.Len(t, updatedCh.DisabledAPIKeys, 1)

	// Disable second key - should disable the entire channel
	err = svc.DisableAPIKey(ctx, ch.ID, "key2", 401, "Test reason 2")
	require.NoError(t, err)

	// Verify channel is now disabled
	updatedCh, err = client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusDisabled, updatedCh.Status)
	require.Len(t, updatedCh.DisabledAPIKeys, 2)
	require.NotNil(t, updatedCh.ErrorMessage)
	require.NotNil(t, updatedCh.AutoDisabledAt)
	require.Nil(t, updatedCh.AutoDisableExpiresAt, "all-keys-unavailable must not set a channel expiry")
}

func TestChannelService_DisableAllAPIKeysNotifiesWebhook(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	type webhookRequest struct {
		body string
		err  error
	}
	notifications := make(chan webhookRequest, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		notifications <- webhookRequest{body: string(body), err: err}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := WebhookNotifierConfig{
		Targets: []WebhookTarget{
			{
				Name:      "default",
				Enabled:   true,
				URL:       server.URL,
				TimeoutMs: 1000,
				Body:      `{"event":"{{.Event}}","channel_id":{{.Channel.ID}},"channel_name":"{{.Channel.Name}}","provider":"{{.Channel.Provider}}","base_url":"{{.Channel.BaseURL}}","status":"{{.Channel.Status}}","status_code":{{.Trigger.StatusCode}},"reason":"{{.Trigger.Reason}}"}`,
			},
		},
		Subscriptions: []WebhookSubscription{
			{Event: EventChannelAutoDisabled, TargetNames: []string{"default"}},
		},
	}

	svc := newTestChannelService(client)
	svc.SystemService = newTestSystemServiceWithWebhookConfig(t, client, cfg)
	svc.WebhookNotifier = NewWebhookNotifier(svc.SystemService, httpclient.NewHttpClient())

	ch := createTestChannelWithAPIKeys(t, client, ctx, "test-channel-webhook", []string{"key1", "key2"})

	require.NoError(t, svc.DisableAPIKey(ctx, ch.ID, "key1", 500, "first key failed"))

	select {
	case notification := <-notifications:
		t.Fatalf("received webhook before channel was disabled: %s", notification.body)
	case <-time.After(100 * time.Millisecond):
	}

	require.NoError(t, svc.DisableAPIKey(ctx, ch.ID, "key2", 500, "second key failed"))

	select {
	case notification := <-notifications:
		require.NoError(t, notification.err)
		require.JSONEq(t, fmt.Sprintf(`{
			"event": "channel.auto_disabled",
			"channel_id": %d,
			"channel_name": "test-channel-webhook",
			"provider": "openai",
			"base_url": "https://api.openai.com",
			"status": "disabled",
			"status_code": 500,
			"reason": "All API keys disabled (last error: 500)"
		}`, ch.ID), notification.body)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel auto-disabled webhook")
	}
}

func TestChannelService_SuccessResetsErrorWindows(t *testing.T) {
	rule := objects.APIKeyAutoDisableRule{StatusCodes: []int{401}, Times: 3, Action: objects.APIKeyAutoDisableActionPermanent}
	ctx, svc, ch := newWindowRuleTestService(t, rule)
	base := time.Now().Add(-20 * time.Second)
	perf := &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 401, EndTime: base}
	for range 2 {
		_, acted := svc.evaluateAutoDisableForFailure(ctx, perf)
		require.False(t, acted)
	}
	successAt := base.Add(time.Second)
	svc.IncrementChannelSelection(ch.ID)
	svc.RecordPerformance(ctx, &PerformanceRecord{
		ChannelID: ch.ID, Model: "gpt-4", APIKey: "key2", Success: true,
		RequestCompleted: true, StartTime: successAt, EndTime: successAt,
	})
	perf.EndTime = base.Add(2 * time.Second)
	for range 2 {
		_, acted := svc.evaluateAutoDisableForFailure(ctx, perf)
		require.False(t, acted, "the successful model requires three new failures to reach its threshold")
	}
	_, acted := svc.evaluateAutoDisableForFailure(ctx, perf)
	require.True(t, acted)
	updated, err := svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireLegacyRuleModelCooldown(t, updated, "gpt-4", defaultAutoDisableCooldownMinutes)
}

func TestChannelService_SuccessResetsOnlyMatchingModelAcrossKeys(t *testing.T) {
	rule := objects.APIKeyAutoDisableRule{StatusCodes: []int{429}, Times: 3, Action: objects.APIKeyAutoDisableActionPermanent}
	ctx, svc, ch := newWindowRuleTestService(t, rule)
	base := time.Now().Add(-20 * time.Second)
	keyed := &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 429, EndTime: base}
	keyless := &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4o", ResponseStatusCode: 429, EndTime: base}
	for range 2 {
		for _, perf := range []*PerformanceRecord{keyed, keyless} {
			_, acted := svc.evaluateAutoDisableForFailure(ctx, perf)
			require.False(t, acted)
		}
	}
	successAt := base.Add(time.Second)
	svc.IncrementChannelSelection(ch.ID)
	svc.RecordPerformance(ctx, &PerformanceRecord{
		ChannelID: ch.ID, Model: "gpt-4", Success: true, RequestCompleted: true, StartTime: successAt, EndTime: successAt,
	})
	keyed.EndTime = base.Add(2 * time.Second)
	for range 2 {
		_, acted := svc.evaluateAutoDisableForFailure(ctx, keyed)
		require.False(t, acted, "success resets this model's count across all keys")
	}
	_, keyedActed := svc.evaluateAutoDisableForFailure(ctx, keyed)
	require.True(t, keyedActed)
	_, keylessActed := svc.evaluateAutoDisableForFailure(ctx, keyless)
	require.True(t, keylessActed, "a success for gpt-4 does not clear gpt-4o failures")
	updated, err := svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, updated.Status)
	require.Empty(t, updated.DisabledAPIKeys)
	require.Len(t, updated.ModelCooldowns, 2)
	for _, model := range []string{"gpt-4", "gpt-4o"} {
		var cooldown *objects.ModelCooldown
		for i := range updated.ModelCooldowns {
			if updated.ModelCooldowns[i].Model == model {
				cooldown = &updated.ModelCooldowns[i]
				break
			}
		}
		require.NotNil(t, cooldown)
		require.True(t, cooldown.IsActive())
		require.NotNil(t, cooldown.ExpiresAt)
		require.True(t, buildChannel(updated, nil).HasActiveModelCooldown(model, time.Now()))
	}
}

func TestChannelService_MultipleStatusCodes(t *testing.T) {
	for _, tt := range []struct{ code, threshold int }{{401, 2}, {403, 1}} {
		t.Run(fmt.Sprint(tt.code), func(t *testing.T) {
			client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
			defer client.Close()
			ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
			svc := newTestChannelService(client)
			ch := createTestChannelWithAPIKeys(t, client, ctx, "status-rules", []string{"key1", "key2"})
			withGlobalAutoDisable(t, svc, ctx, ch, []objects.APIKeyAutoDisableRule{
				{StatusCodes: []int{401}, Times: 2, Action: objects.APIKeyAutoDisableActionPermanent},
				{StatusCodes: []int{403}, Times: 1, Action: objects.APIKeyAutoDisableActionPermanent},
			})
			for i := 1; i <= tt.threshold; i++ {
				_, acted := svc.evaluateAutoDisableForFailure(ctx, &PerformanceRecord{
					ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: tt.code,
				})
				require.Equal(t, i == tt.threshold, acted)
			}
			updated, err := client.Channel.Get(ctx, ch.ID)
			require.NoError(t, err)
			requireLegacyRuleModelCooldown(t, updated, "gpt-4", defaultAutoDisableCooldownMinutes)
		})
	}
}

func TestChannelService_ConcurrentErrorTracking(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	svc := newTestChannelService(client)

	ch := createTestChannelWithAPIKeys(t, client, ctx, "test-channel", []string{"key1", "key2", "key3"})
	withGlobalAutoDisable(t, svc, ctx, ch, []objects.APIKeyAutoDisableRule{{
		StatusCodes: []int{401},
		Times:       5,
		Action:      objects.APIKeyAutoDisableActionPermanent,
	}})

	// Simulate concurrent error reporting
	var wg sync.WaitGroup

	numGoroutines := 5
	panics := make(chan any, numGoroutines)

	for i := range numGoroutines {
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panics <- r
				}
			}()

			perf := &PerformanceRecord{
				ChannelID: ch.ID, Model: "gpt-4",
				APIKey:             fmt.Sprintf("key%d", idx%3+1),
				ResponseStatusCode: 401,
				Success:            false,
			}
			svc.evaluateAutoDisableForFailure(ctx, perf)
		}(i)
	}

	wg.Wait()

	close(panics)
	for recovered := range panics {
		t.Fatalf("concurrent failure tracking panicked: %v", recovered)
	}
	// Five shared failures trigger the normal rule, not the sixth-failure guard.
	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireLegacyRuleModelCooldown(t, updated, "gpt-4", defaultAutoDisableCooldownMinutes)
	require.Equal(t, ch.Credentials, updated.Credentials)
}

func TestChannelService_DisableAPIKeyIdempotent(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	svc := newTestChannelService(client)

	ch := createTestChannelWithAPIKeys(t, client, ctx, "test-channel", []string{"key1", "key2"})

	// Disable key1 first time
	err := svc.DisableAPIKey(ctx, ch.ID, "key1", 401, "Reason 1")
	require.NoError(t, err)

	// Disable key1 second time - should be idempotent
	err = svc.DisableAPIKey(ctx, ch.ID, "key1", 401, "Reason 2")
	require.NoError(t, err)

	// Verify only one entry
	updatedCh, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Len(t, updatedCh.DisabledAPIKeys, 1)
}

func TestChannelService_DisableAPIKeyNotFound(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	svc := newTestChannelService(client)

	ch := createTestChannelWithAPIKeys(t, client, ctx, "test-channel", []string{"key1", "key2"})

	// Try to disable a key that doesn't exist - should be ignored
	err := svc.DisableAPIKey(ctx, ch.ID, "nonexistent-key", 401, "Reason")
	require.NoError(t, err)

	// Verify no keys are disabled
	updatedCh, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Len(t, updatedCh.DisabledAPIKeys, 0)
}

func TestChannelService_DisableAPIKeyEmptyKey(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	svc := newTestChannelService(client)

	ch := createTestChannelWithAPIKeys(t, client, ctx, "test-channel", []string{"key1"})

	// Try to disable an empty key - should return error
	err := svc.DisableAPIKey(ctx, ch.ID, "", 401, "Reason")
	require.Error(t, err)
}

func TestMatchesAPIKeyRule(t *testing.T) {
	tests := []struct {
		name string
		rule objects.APIKeyAutoDisableRule
		perf *PerformanceRecord
		want bool
	}{
		{
			name: "status code only",
			rule: objects.APIKeyAutoDisableRule{StatusCodes: []int{401}},
			perf: &PerformanceRecord{ResponseStatusCode: 401},
			want: true,
		},
		{
			name: "keyword only is case insensitive",
			rule: objects.APIKeyAutoDisableRule{KeywordPatterns: []string{"quota exceeded"}},
			perf: &PerformanceRecord{ResponseStatusCode: 503, ErrorMessage: "QUOTA EXCEEDED for this account"},
			want: true,
		},
		{
			name: "regular expression",
			rule: objects.APIKeyAutoDisableRule{KeywordPatterns: []string{`account (was )?disabled`}},
			perf: &PerformanceRecord{ResponseStatusCode: 400, ErrorMessage: "Account was disabled"},
			want: true,
		},
		{
			name: "invalid regular expression falls back to literal keyword",
			rule: objects.APIKeyAutoDisableRule{KeywordPatterns: []string{"quota["}},
			perf: &PerformanceRecord{ResponseStatusCode: 429, ErrorMessage: "Provider QUOTA[ exceeded"},
			want: true,
		},
		{
			name: "status and keyword must both match",
			rule: objects.APIKeyAutoDisableRule{StatusCodes: []int{401}, KeywordPatterns: []string{"invalid key"}},
			perf: &PerformanceRecord{ResponseStatusCode: 403, ErrorMessage: "invalid key"},
			want: false,
		},
		{
			name: "missing message does not match keyword",
			rule: objects.APIKeyAutoDisableRule{KeywordPatterns: []string{"quota"}},
			perf: &PerformanceRecord{ResponseStatusCode: 429},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, matchesAPIKeyRule(tt.rule, tt.perf))
		})
	}
}

func TestNormalizeAPIKeyAutoDisableRules(t *testing.T) {
	duration := 30
	policies := &objects.ChannelPolicies{APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{
		{
			StatusCodes:            []int{429, 401, 429},
			KeywordPatterns:        []string{" quota ", "quota", ""},
			Times:                  2,
			Action:                 objects.APIKeyAutoDisableActionTemporary,
			DisableDurationMinutes: &duration,
		},
	}}

	require.NoError(t, NormalizeAPIKeyAutoDisableRules(policies))
	require.Equal(t, []int{401, 429}, policies.APIKeyAutoDisableRules[0].StatusCodes)
	require.Equal(t, []string{"quota"}, policies.APIKeyAutoDisableRules[0].KeywordPatterns)
	require.True(t, policies.APIKeyAutoDisableRules[0].FreezeModel)
	require.Equal(t, defaultAutoDisableWindowMinutes, policies.APIKeyAutoDisableRules[0].WindowMinutes)
	defaults := &objects.ChannelPolicies{APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{{Times: 1, Action: objects.APIKeyAutoDisableActionTemporary}}}
	require.NoError(t, NormalizeAPIKeyAutoDisableRules(defaults))
	require.NotNil(t, defaults.APIKeyAutoDisableRules[0].DisableDurationMinutes)
	require.Equal(t, defaultAutoDisableCooldownMinutes, *defaults.APIKeyAutoDisableRules[0].DisableDurationMinutes)

	invalidDuration := 0
	tests := []objects.APIKeyAutoDisableRule{
		{Times: 0, Action: objects.APIKeyAutoDisableActionTemporary},
		{StatusCodes: []int{99}, Times: 1, Action: objects.APIKeyAutoDisableActionTemporary},
		{Times: 1, Action: "unsupported"},
		{Times: 1, Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: &invalidDuration},
	}
	for _, rule := range tests {
		require.Error(t, NormalizeAPIKeyAutoDisableRules(&objects.ChannelPolicies{
			APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{rule},
		}))
	}
}

func TestChannelService_ChannelAPIKeyRuleTemporaryModelCooldownAfterThreshold(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)
	duration := 30
	ch := createTestChannelWithAPIKeys(t, client, ctx, "temporary-rule", []string{"key1", "key2"})
	ch, err := client.Channel.UpdateOneID(ch.ID).
		SetPolicies(objects.ChannelPolicies{APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{
			{
				StatusCodes:            []int{429},
				Times:                  2,
				Action:                 objects.APIKeyAutoDisableActionTemporary,
				DisableDurationMinutes: &duration,
			},
		}}).
		Save(ctx)
	require.NoError(t, err)
	svc.SetEnabledChannelsForTest([]*Channel{buildChannel(ch, nil)})

	perf := &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 429}
	matched, acted := svc.checkAndHandleChannelAPIKeyRules(ctx, perf)
	require.True(t, matched)
	require.False(t, acted)
	matched, acted = svc.checkAndHandleChannelAPIKeyRules(ctx, perf)
	require.True(t, matched)
	require.True(t, acted)

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireLegacyRuleModelCooldown(t, updated, "gpt-4", duration)
	require.Equal(t, ch.Credentials, updated.Credentials)
}

func TestChannelService_ChannelAPIKeyRuleLegacyDeleteKeepsLastKeyAndFreezesModel(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "permanent-rule", []string{"only-key"})
	ch, err := client.Channel.UpdateOneID(ch.ID).
		SetPolicies(objects.ChannelPolicies{APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{
			{
				StatusCodes: []int{401},
				Times:       1,
				Action:      objects.APIKeyAutoDisableActionPermanentDelete,
			},
		}}).
		Save(ctx)
	require.NoError(t, err)
	svc.SetEnabledChannelsForTest([]*Channel{buildChannel(ch, nil)})

	matched, acted := svc.checkAndHandleChannelAPIKeyRules(ctx, &PerformanceRecord{
		ChannelID: ch.ID, Model: "gpt-4", APIKey: "only-key", ResponseStatusCode: 401,
	})
	require.True(t, matched)
	require.True(t, acted)

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireLegacyRuleModelCooldown(t, updated, "gpt-4", defaultAutoDisableCooldownMinutes)
	require.Equal(t, ch.Credentials, updated.Credentials)
}

func TestChannelService_ChannelAPIKeyRuleCountsAlternatingStatusesTogether(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)
	duration := 30
	ch := createTestChannelWithAPIKeys(t, client, ctx, "multi-status-rule", []string{"key1", "key2"})
	ch, err := client.Channel.UpdateOneID(ch.ID).
		SetPolicies(objects.ChannelPolicies{APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{
			{
				StatusCodes:            []int{401, 403},
				Times:                  2,
				Action:                 objects.APIKeyAutoDisableActionTemporary,
				DisableDurationMinutes: &duration,
			},
		}}).
		Save(ctx)
	require.NoError(t, err)
	svc.SetEnabledChannelsForTest([]*Channel{buildChannel(ch, nil)})

	matched, acted := svc.checkAndHandleChannelAPIKeyRules(ctx, &PerformanceRecord{
		ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 401,
	})
	require.True(t, matched)
	require.False(t, acted)

	matched, acted = svc.checkAndHandleChannelAPIKeyRules(ctx, &PerformanceRecord{
		ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 403,
	})
	require.True(t, matched)
	require.True(t, acted)

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireLegacyRuleModelCooldown(t, updated, "gpt-4", duration)
	require.Equal(t, ch.Credentials, updated.Credentials)
}

func TestChannelService_ChannelAPIKeyRuleKeepsWindowAfterNonMatchingFailure(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)
	duration := 30
	ch := createTestChannelWithAPIKeys(t, client, ctx, "reset-non-match", []string{"key1", "key2"})
	ch, err := client.Channel.UpdateOneID(ch.ID).
		SetPolicies(objects.ChannelPolicies{APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{
			{
				StatusCodes:            []int{401},
				Times:                  2,
				Action:                 objects.APIKeyAutoDisableActionTemporary,
				DisableDurationMinutes: &duration,
			},
		}}).
		Save(ctx)
	require.NoError(t, err)
	svc.SetEnabledChannelsForTest([]*Channel{buildChannel(ch, nil)})

	matched, acted := svc.checkAndHandleChannelAPIKeyRules(ctx, &PerformanceRecord{
		ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 401,
	})
	require.True(t, matched)
	require.False(t, acted)

	matched, acted = svc.checkAndHandleChannelAPIKeyRules(ctx, &PerformanceRecord{
		ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 500,
	})
	require.False(t, matched)
	require.False(t, acted)

	matched, acted = svc.checkAndHandleChannelAPIKeyRules(ctx, &PerformanceRecord{
		ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 401,
	})
	require.True(t, matched)
	require.True(t, acted)

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireLegacyRuleModelCooldown(t, updated, "gpt-4", duration)
}

func TestChannelService_ChannelAPIKeyRuleKeepsIndependentCounters(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)
	duration := 30
	ch := createTestChannelWithAPIKeys(t, client, ctx, "reset-owned-failure", []string{"key1", "key2"})
	ch, err := client.Channel.UpdateOneID(ch.ID).
		SetPolicies(objects.ChannelPolicies{APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{
			{
				StatusCodes:            []int{401},
				Times:                  2,
				Action:                 objects.APIKeyAutoDisableActionTemporary,
				DisableDurationMinutes: &duration,
			},
			{
				StatusCodes:            []int{401, 403},
				Times:                  2,
				Action:                 objects.APIKeyAutoDisableActionTemporary,
				DisableDurationMinutes: &duration,
			},
		}}).
		Save(ctx)
	require.NoError(t, err)
	svc.SetEnabledChannelsForTest([]*Channel{buildChannel(ch, nil)})

	matched, acted := svc.checkAndHandleChannelAPIKeyRules(ctx, &PerformanceRecord{
		ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 403,
	})
	require.True(t, matched)
	require.False(t, acted)

	matched, acted = svc.checkAndHandleChannelAPIKeyRules(ctx, &PerformanceRecord{
		ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 401,
	})
	require.True(t, matched)
	require.False(t, acted)

	matched, acted = svc.checkAndHandleChannelAPIKeyRules(ctx, &PerformanceRecord{
		ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 403,
	})
	require.True(t, matched)
	require.True(t, acted)

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireLegacyRuleModelCooldown(t, updated, "gpt-4", duration)
}

func TestChannelService_ChannelAPIKeyRuleEditStartsNewWindow(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)
	duration := 30
	ch := createTestChannelWithAPIKeys(t, client, ctx, "edited-rule", []string{"key1", "key2"})
	ch, err := client.Channel.UpdateOneID(ch.ID).
		SetPolicies(objects.ChannelPolicies{APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{
			{
				StatusCodes:            []int{429},
				Times:                  2,
				Action:                 objects.APIKeyAutoDisableActionTemporary,
				DisableDurationMinutes: &duration,
			},
		}}).
		Save(ctx)
	require.NoError(t, err)
	svc.SetEnabledChannelsForTest([]*Channel{buildChannel(ch, nil)})

	perf := &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 429}
	matched, acted := svc.checkAndHandleChannelAPIKeyRules(ctx, perf)
	require.True(t, matched)
	require.False(t, acted)

	editedDuration := 60
	ch, err = client.Channel.UpdateOneID(ch.ID).
		SetPolicies(objects.ChannelPolicies{APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{
			{
				StatusCodes:            []int{429},
				Times:                  2,
				Action:                 objects.APIKeyAutoDisableActionTemporary,
				DisableDurationMinutes: &editedDuration,
			},
		}}).
		Save(ctx)
	require.NoError(t, err)
	svc.SetEnabledChannelsForTest([]*Channel{buildChannel(ch, nil)})

	matched, acted = svc.checkAndHandleChannelAPIKeyRules(ctx, perf)
	require.True(t, matched)
	require.False(t, acted)

	matched, acted = svc.checkAndHandleChannelAPIKeyRules(ctx, perf)
	require.True(t, matched)
	require.True(t, acted)
	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireLegacyRuleModelCooldown(t, updated, "gpt-4", editedDuration)
}

func TestChannelService_ChannelAPIKeyRuleKeepsWindowWhenActionFails(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)
	duration := 30
	ch := createTestChannelWithAPIKeys(t, client, ctx, "failed-action", []string{"key1", "key2"})
	ch, err := client.Channel.UpdateOneID(ch.ID).
		SetPolicies(objects.ChannelPolicies{APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{
			{
				StatusCodes:            []int{429},
				Times:                  1,
				Action:                 objects.APIKeyAutoDisableActionTemporary,
				DisableDurationMinutes: &duration,
			},
		}}).
		Save(ctx)
	require.NoError(t, err)
	svc.SetEnabledChannelsForTest([]*Channel{buildChannel(ch, nil)})
	// Fail only the cooldown write, not the failure-event persistence: the
	// window must survive a failed action and be retried on the next failure.
	svc.db.Channel.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(mutationCtx context.Context, mutation ent.Mutation) (ent.Value, error) {
			return nil, fmt.Errorf("simulated cooldown write failure")
		})
	})

	matched, acted := svc.checkAndHandleChannelAPIKeyRules(ctx, &PerformanceRecord{
		ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 429,
	})
	require.True(t, matched)
	require.False(t, acted)

	perf := &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 429}
	rule0 := ch.Policies.APIKeyAutoDisableRules[0]
	key := legacyRuleCounterKey(perf, autoDisableScopeChannel, 0, rule0)
	count, _ := countChannelFailureEventsForTest(t, svc, key, channelFailureWindowKeyForTest(perf, rule0), time.Now().Add(-time.Duration(effectiveModelFreezeRule(rule0).WindowMinutes)*time.Minute))
	require.Equal(t, 1, count)
	svc.apiKeyErrorCountsLock.Lock()
	defer svc.apiKeyErrorCountsLock.Unlock()
	require.Empty(t, svc.windowedRuleActionsInFlight[ch.ID])
}

func TestChannelService_RemovedAPIKeyRuleDoesNotCountUnmatchedFailures(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)
	duration := 30
	rule := objects.APIKeyAutoDisableRule{
		StatusCodes:            []int{429},
		Times:                  2,
		Action:                 objects.APIKeyAutoDisableActionTemporary,
		DisableDurationMinutes: &duration,
	}
	ch := createTestChannelWithAPIKeys(t, client, ctx, "removed-rule", []string{"key1", "key2"})
	ch, err := client.Channel.UpdateOneID(ch.ID).
		SetPolicies(objects.ChannelPolicies{APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{rule}}).
		Save(ctx)
	require.NoError(t, err)
	svc.SetEnabledChannelsForTest([]*Channel{buildChannel(ch, nil)})

	perf := &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 429}
	matched, acted := svc.checkAndHandleChannelAPIKeyRules(ctx, perf)
	require.True(t, matched)
	require.False(t, acted)

	withoutRulesEntity := *ch
	withoutRulesEntity.Policies.APIKeyAutoDisableRules = nil
	withoutRules := buildChannel(&withoutRulesEntity, nil)
	svc.SetEnabledChannelsForTest([]*Channel{withoutRules})
	matched, acted = svc.checkAndHandleChannelAPIKeyRules(ctx, perf)
	require.False(t, matched)
	require.False(t, acted)

	svc.SetEnabledChannelsForTest([]*Channel{buildChannel(ch, nil)})
	matched, acted = svc.checkAndHandleChannelAPIKeyRules(ctx, perf)
	require.True(t, matched)
	require.True(t, acted)

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireLegacyRuleModelCooldown(t, updated, "gpt-4", duration)
	key := legacyRuleCounterKey(perf, autoDisableScopeChannel, 0, rule)
	count, _ := countChannelFailureEventsForTest(t, svc, key, channelFailureWindowKeyForTest(perf, rule), time.Now().Add(-time.Duration(effectiveModelFreezeRule(rule).WindowMinutes)*time.Minute))
	require.Equal(t, 2, count, "removed rules must not count unmatched failures")
}

func TestChannelService_ChannelAPIKeyRuleSuccessResetPreventsDelayedAction(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)
	duration := 30
	rule := objects.APIKeyAutoDisableRule{
		StatusCodes:            []int{429},
		Times:                  1,
		Action:                 objects.APIKeyAutoDisableActionTemporary,
		DisableDurationMinutes: &duration,
	}
	ch := createTestChannelWithAPIKeys(t, client, ctx, "concurrent-action", []string{"key1", "key2"})
	ch, err := client.Channel.UpdateOneID(ch.ID).
		SetPolicies(objects.ChannelPolicies{APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{rule}}).
		Save(ctx)
	require.NoError(t, err)
	svc.SetEnabledChannelsForTest([]*Channel{buildChannel(ch, nil)})

	perf := &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 429, EndTime: time.Now().Add(-time.Second)}
	effective := effectiveModelFreezeRule(rule)
	ruleKey := legacyRuleCounterKey(perf, autoDisableScopeChannel, 0, rule)
	windowKey := fmt.Sprintf("%s:window:%d", autoDisableRuleIdentity(perf, effective), effective.WindowMinutes)
	svc.windowedRuleActionsInFlight = map[int]map[string]bool{ch.ID: {windowKey: true}}
	matched, acted := svc.checkAndHandleChannelAPIKeyRules(ctx, perf)
	require.True(t, matched)
	require.False(t, acted)
	pending, _ := countChannelFailureEventsForTest(t, svc, ruleKey, windowKey, time.Now().Add(-time.Duration(effective.WindowMinutes)*time.Minute))
	svc.apiKeyErrorCountsLock.Lock()
	inFlight := svc.windowedRuleActionsInFlight[ch.ID][windowKey]
	svc.apiKeyErrorCountsLock.Unlock()
	require.Equal(t, 1, pending)
	require.True(t, inFlight)

	now := time.Now()
	svc.IncrementChannelSelection(ch.ID)
	svc.RecordPerformance(ctx, &PerformanceRecord{
		ChannelID: ch.ID, Model: "gpt-4", APIKey: "key2", StartTime: now, EndTime: now,
		Success: true, RequestCompleted: true,
	})
	pending, _ = countChannelFailureEventsForTest(t, svc, ruleKey, windowKey, time.Now().Add(-time.Duration(effective.WindowMinutes)*time.Minute))
	svc.apiKeyErrorCountsLock.Lock()
	inFlight = svc.windowedRuleActionsInFlight[ch.ID][windowKey]
	delete(svc.windowedRuleActionsInFlight[ch.ID], windowKey)
	svc.apiKeyErrorCountsLock.Unlock()
	require.Equal(t, 1, pending, "the raw failure event remains stored, but the success marker excludes it from the streak")
	require.True(t, inFlight)
	matched, acted = svc.checkAndHandleChannelAPIKeyRules(ctx, perf)
	require.True(t, matched)
	require.False(t, acted, "a failure completed before the success cannot trigger a delayed freeze")
	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Empty(t, updated.ModelCooldowns)
}

func TestEffectiveAutoDisableMode(t *testing.T) {
	require.Equal(t, objects.APIKeyAutoDisableModeOff, objects.ChannelPolicies{APIKeyAutoDisableMode: objects.APIKeyAutoDisableModeOff}.EffectiveAutoDisableMode())
	require.Equal(t, objects.APIKeyAutoDisableModeOff, objects.ChannelPolicies{
		APIKeyAutoDisableMode:  objects.APIKeyAutoDisableModeOff,
		APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{{Times: 1, Action: objects.APIKeyAutoDisableActionPermanent}},
	}.EffectiveAutoDisableMode())
	require.Equal(t, objects.APIKeyAutoDisableModeInherit, objects.ChannelPolicies{APIKeyAutoDisableMode: objects.APIKeyAutoDisableModeCustom}.EffectiveAutoDisableMode())
	require.Equal(t, objects.APIKeyAutoDisableModeCustom, objects.ChannelPolicies{
		APIKeyAutoDisableMode:  objects.APIKeyAutoDisableModeCustom,
		APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{{Times: 1, Action: objects.APIKeyAutoDisableActionPermanent}},
	}.EffectiveAutoDisableMode())
	require.Equal(t, objects.APIKeyAutoDisableModeInherit, objects.ChannelPolicies{}.EffectiveAutoDisableMode())
	require.Equal(t, objects.APIKeyAutoDisableModeCustom, objects.ChannelPolicies{
		APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{{Times: 1, Action: objects.APIKeyAutoDisableActionPermanent}},
	}.EffectiveAutoDisableMode())
}

func TestNormalizeAPIKeyAutoDisableRules_Mode(t *testing.T) {
	inherit := &objects.ChannelPolicies{
		APIKeyAutoDisableMode:  objects.APIKeyAutoDisableModeInherit,
		APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{{Times: 1, Action: objects.APIKeyAutoDisableActionPermanent}},
	}
	require.NoError(t, NormalizeAPIKeyAutoDisableRules(inherit))
	require.Empty(t, inherit.APIKeyAutoDisableRules)

	emptyCustom := &objects.ChannelPolicies{APIKeyAutoDisableMode: objects.APIKeyAutoDisableModeCustom}
	require.NoError(t, NormalizeAPIKeyAutoDisableRules(emptyCustom))
	require.Equal(t, objects.APIKeyAutoDisableModeInherit, emptyCustom.APIKeyAutoDisableMode)

	off := &objects.ChannelPolicies{
		APIKeyAutoDisableMode:  objects.APIKeyAutoDisableModeOff,
		APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{{Times: 1, Action: objects.APIKeyAutoDisableActionPermanent}},
	}
	require.NoError(t, NormalizeAPIKeyAutoDisableRules(off))
	require.Equal(t, objects.APIKeyAutoDisableModeOff, off.APIKeyAutoDisableMode)
	require.Len(t, off.APIKeyAutoDisableRules, 1)
}

func TestChannelService_InheritTemporaryModelCooldownAndCleanup(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "inherit-temp", []string{"key1", "key2"})
	duration := 30
	withGlobalAutoDisable(t, svc, ctx, ch, []objects.APIKeyAutoDisableRule{{
		StatusCodes:            []int{429},
		Times:                  2,
		Action:                 objects.APIKeyAutoDisableActionTemporary,
		DisableDurationMinutes: &duration,
	}})

	now := time.Now()
	perf := &PerformanceRecord{
		ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 429,
		RequestCompleted: true, StartTime: now, EndTime: now,
	}
	svc.IncrementChannelSelection(ch.ID)
	svc.RecordPerformance(ctx, perf)
	svc.IncrementChannelSelection(ch.ID)
	svc.RecordPerformance(ctx, perf)

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	cooldown := requireLegacyRuleModelCooldown(t, updated, "gpt-4", duration)
	elapsed := time.Now().Add(-time.Minute)
	cooldown.ExpiresAt = &elapsed
	_, err = client.Channel.UpdateOneID(ch.ID).SetModelCooldowns([]objects.ModelCooldown{cooldown}).Save(ctx)
	require.NoError(t, err)
	svc.cleanupExpiredDisabledAPIKeys(ctx)
	recovered, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Empty(t, recovered.DisabledAPIKeys)
	require.Empty(t, recovered.ModelCooldowns)
	require.Equal(t, channel.StatusEnabled, recovered.Status)
	require.Equal(t, ch.Credentials, recovered.Credentials)
}

func TestChannelService_CustomAndGlobalCountersAreIndependent(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "overlay-counters", []string{"key1", "key2"})
	ch, err := client.Channel.UpdateOneID(ch.ID).
		SetPolicies(objects.ChannelPolicies{
			APIKeyAutoDisableMode: objects.APIKeyAutoDisableModeCustom,
			APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{{
				KeywordPatterns:  []string{"quota"},
				Times:            2,
				Action:           objects.APIKeyAutoDisableActionUntilCron,
				DisableUntilCron: "0 0 * * *",
			}},
		}).
		Save(ctx)
	require.NoError(t, err)
	require.NoError(t, svc.SystemService.SetRetryPolicy(ctx, &RetryPolicy{
		AutoDisableChannel: AutoDisableChannel{
			Enabled: true,
			Rules: []objects.APIKeyAutoDisableRule{{
				StatusCodes: []int{401},
				Times:       3,
				Action:      objects.APIKeyAutoDisableActionPermanent,
			}},
		},
	}))
	svc.SetEnabledChannelsForTest([]*Channel{buildChannel(ch, nil)})

	quotaPerf := &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 429, ErrorMessage: "quota exceeded"}
	matched, acted := svc.evaluateAutoDisableForFailure(ctx, quotaPerf)
	require.True(t, matched)
	require.False(t, acted)

	authPerf := &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 401}
	matched, acted = svc.evaluateAutoDisableForFailure(ctx, authPerf)
	require.True(t, matched)
	require.False(t, acted)

	channelKey := legacyRuleCounterKey(quotaPerf, autoDisableScopeChannel, 0, ch.Policies.APIKeyAutoDisableRules[0])
	globalRule := objects.APIKeyAutoDisableRule{StatusCodes: []int{401}, Times: 3, Action: objects.APIKeyAutoDisableActionPermanent}
	globalKey := legacyRuleCounterKey(authPerf, autoDisableScopeGlobal, 0, globalRule)
	channelRule := ch.Policies.APIKeyAutoDisableRules[0]
	channelCount, _ := countChannelFailureEventsForTest(t, svc, channelKey, channelFailureWindowKeyForTest(quotaPerf, channelRule), time.Now().Add(-time.Duration(effectiveModelFreezeRule(channelRule).WindowMinutes)*time.Minute))
	globalCount, _ := countChannelFailureEventsForTest(t, svc, globalKey, channelFailureWindowKeyForTest(authPerf, globalRule), time.Now().Add(-time.Duration(effectiveModelFreezeRule(globalRule).WindowMinutes)*time.Minute))
	require.Equal(t, 1, channelCount)
	require.Equal(t, 1, globalCount)

	now := time.Now()
	svc.IncrementChannelSelection(ch.ID)
	svc.RecordPerformance(ctx, &PerformanceRecord{
		ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", Success: true, RequestCompleted: true, StartTime: now, EndTime: now,
	})
	channelCount, _ = countChannelFailureEventsForTest(t, svc, channelKey, channelFailureWindowKeyForTest(quotaPerf, channelRule), time.Now().Add(-time.Duration(effectiveModelFreezeRule(channelRule).WindowMinutes)*time.Minute))
	globalCount, _ = countChannelFailureEventsForTest(t, svc, globalKey, channelFailureWindowKeyForTest(authPerf, globalRule), time.Now().Add(-time.Duration(effectiveModelFreezeRule(globalRule).WindowMinutes)*time.Minute))
	require.Equal(t, 1, channelCount, "raw channel-scope events are retained for diagnostics")
	require.Equal(t, 1, globalCount, "raw global-scope events are retained for diagnostics")

	// This failure matches the custom rule; the model success reset means it is
	// only the first post-success channel-scope failure and cannot freeze yet.
	matched, acted = svc.evaluateAutoDisableForFailure(ctx, &PerformanceRecord{
		ChannelID: ch.ID, Model: "gpt-4", APIKey: "key2", ResponseStatusCode: 401, ErrorMessage: "quota exceeded",
	})
	require.True(t, matched)
	require.False(t, acted)
	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Empty(t, updated.ModelCooldowns)
}

func TestChannelService_OffModeDoesNotCountOrDisable(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "off-mode", []string{"key1", "key2"})
	ch, err := client.Channel.UpdateOneID(ch.ID).
		SetPolicies(objects.ChannelPolicies{
			APIKeyAutoDisableMode: objects.APIKeyAutoDisableModeOff,
			APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{{
				StatusCodes: []int{401},
				Times:       1,
				Action:      objects.APIKeyAutoDisableActionPermanent,
			}},
		}).
		Save(ctx)
	require.NoError(t, err)
	require.NoError(t, svc.SystemService.SetRetryPolicy(ctx, &RetryPolicy{
		AutoDisableChannel: AutoDisableChannel{
			Enabled: true,
			Rules: []objects.APIKeyAutoDisableRule{{
				StatusCodes: []int{401},
				Times:       1,
				Action:      objects.APIKeyAutoDisableActionPermanent,
			}},
		},
	}))
	svc.SetEnabledChannelsForTest([]*Channel{buildChannel(ch, nil)})

	matched, acted := svc.evaluateAutoDisableForFailure(ctx, &PerformanceRecord{
		ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 401,
	})
	require.False(t, matched)
	require.False(t, acted)
	require.Zero(t, countChannelFailureEventsByChannelForTest(t, svc, ch.ID))

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Empty(t, updated.DisabledAPIKeys)
	require.Empty(t, updated.ModelCooldowns)
	require.Equal(t, channel.StatusEnabled, updated.Status)
}

func TestChannelService_KeylessTemporaryModelCooldownAndCleanup(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "keyless-temp", []string{})
	duration := 30
	withGlobalAutoDisable(t, svc, ctx, ch, []objects.APIKeyAutoDisableRule{{
		StatusCodes:            []int{429},
		Times:                  1,
		Action:                 objects.APIKeyAutoDisableActionTemporary,
		DisableDurationMinutes: &duration,
	}})

	now := time.Now()
	svc.IncrementChannelSelection(ch.ID)
	svc.RecordPerformance(ctx, &PerformanceRecord{
		ChannelID: ch.ID, Model: "gpt-4", ResponseStatusCode: 429, RequestCompleted: true, StartTime: now, EndTime: now,
	})

	frozen, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	cooldown := requireLegacyRuleModelCooldown(t, frozen, "gpt-4", duration)
	elapsed := time.Now().Add(-time.Minute)
	cooldown.ExpiresAt = &elapsed
	_, err = client.Channel.UpdateOneID(ch.ID).SetModelCooldowns([]objects.ModelCooldown{cooldown}).Save(ctx)
	require.NoError(t, err)
	svc.cleanupExpiredDisabledAPIKeys(ctx)
	recovered, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, recovered.Status)
	require.Empty(t, recovered.ModelCooldowns)
	require.Empty(t, recovered.DisabledAPIKeys)
	require.Nil(t, recovered.AutoDisabledAt)
	require.Nil(t, recovered.AutoDisableExpiresAt)
	require.Nil(t, recovered.ErrorMessage)

	manual := createTestChannelWithAPIKeys(t, client, ctx, "manual-disabled", []string{})
	_, err = client.Channel.UpdateOneID(manual.ID).
		SetStatus(channel.StatusDisabled).
		SetErrorMessage("operator").
		SetAutoDisableExpiresAt(elapsed).
		Save(ctx)
	require.NoError(t, err)
	svc.cleanupExpiredDisabledAPIKeys(ctx)
	still, err := client.Channel.Get(ctx, manual.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusDisabled, still.Status)
	require.Nil(t, still.AutoDisabledAt)
}
