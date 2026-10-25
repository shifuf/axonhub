package biz

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
)

func requireCancellationChannelUnchanged(t *testing.T, before, after *ent.Channel) {
	t.Helper()
	require.Equal(t, before.Status, after.Status, "cancellations must never disable a channel")
	require.Equal(t, before.Credentials, after.Credentials, "cancellations must never delete credentials")
	require.Equal(t, before.DisabledAPIKeys, after.DisabledAPIKeys, "cancellations must never disable credentials")
	require.Equal(t, before.AutoDisabledAt, after.AutoDisabledAt)
	require.Equal(t, before.AutoDisableExpiresAt, after.AutoDisableExpiresAt)
	require.Equal(t, before.ErrorMessage, after.ErrorMessage)
}

func cancellationTestEnvironment(t *testing.T) (*ent.Client, context.Context, *ChannelService) {
	t.Helper()
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	return client, ctx, newTestChannelService(client)
}

func TestNormalizeClientCancellationAction_ModelOnly(t *testing.T) {
	for _, action := range []string{"switch_and_cooldown", "cooldown_only", "cooldown_model", "", "default", "unknown", " cooldown_only "} {
		t.Run(fmt.Sprintf("action=%q", action), func(t *testing.T) {
			require.Equal(t, "cooldown_model", normalizeClientCancellationAction(action))
			policy := ClientCancellationPolicy{
				Enabled: true, Action: action, Scope: ClientCancellationScopeProjectFirst,
				ProjectOverrides: []ClientCancellationProjectPolicy{{ProjectID: 7, Enabled: true, Action: action}},
			}
			require.NoError(t, normalizeClientCancellationPolicy(&policy))
			require.Equal(t, "cooldown_model", policy.Action)
			require.Equal(t, "cooldown_model", policy.ProjectOverrides[0].Action)
		})
	}
}

func TestClientCancellationPolicy_EffectiveLegacyActionsAreModelOnly(t *testing.T) {
	for _, action := range []string{"switch_and_cooldown", "cooldown_only", "cooldown_model", "", "default"} {
		for _, scope := range []string{"global", "project_first"} {
			t.Run(fmt.Sprintf("%s/action=%q", scope, action), func(t *testing.T) {
				// Exercise raw stored/cached policies, without prior normalization.
				policy := ClientCancellationPolicy{
					Enabled: true, Threshold: 3, WindowMinutes: 7, CooldownMinutes: 15,
					Action: action, Scope: scope,
					ProjectOverrides: []ClientCancellationProjectPolicy{
						{ProjectID: 7, Enabled: true, Threshold: 2, WindowMinutes: 4, CooldownMinutes: 5, Action: action},
					},
				}
				effective, source := policy.EffectiveClientCancellationPolicy(7)
				require.Equal(t, "cooldown_model", effective.Action)
				if scope == "global" {
					require.Equal(t, "global", source)
					require.Equal(t, 3, effective.Threshold)
					for _, override := range effective.ProjectOverrides {
						require.Equal(t, "cooldown_model", override.Action)
					}
				} else {
					require.Equal(t, "project", source)
					require.Equal(t, 2, effective.Threshold)
				}
				require.Equal(t, action, policy.Action, "effective lookup must not mutate cached policies")
				require.Equal(t, action, policy.ProjectOverrides[0].Action)
			})
		}
	}
}

func TestChannelService_ClientCancellationExecuteNeverDisablesChannelOrKeys(t *testing.T) {
	client, ctx, svc := cancellationTestEnvironment(t)
	// Observe real Ent writes, including attempted forbidden changes that might
	// otherwise return an error and leave the final stored state unchanged.
	client.Channel.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, mutation ent.Mutation) (ent.Value, error) {
			if mutation.Op().Is(ent.OpUpdate | ent.OpUpdateOne) {
				for _, field := range append(mutation.Fields(), mutation.ClearedFields()...) {
					switch field {
					case channel.FieldCredentials, channel.FieldDisabledAPIKeys, channel.FieldStatus,
						channel.FieldAutoDisabledAt, channel.FieldAutoDisableExpiresAt, channel.FieldErrorMessage:
						t.Errorf("cancellation attempted forbidden channel mutation: %s", field)
					}
				}
			}
			return next.Mutate(ctx, mutation)
		})
	})
	credentials := []struct {
		name string
		keys []string
		key  string
	}{
		{name: "single-key", keys: []string{"key1"}, key: "key1"},
		{name: "multiple-keys", keys: []string{"key1", "key2"}, key: "key1"},
		{name: "keyless-perf", keys: []string{"key1", "key2"}},
		{name: "keyless-channel"},
	}
	for _, action := range []string{"switch_and_cooldown", "cooldown_only", "cooldown_model", "", "default"} {
		for _, credential := range credentials {
			for _, model := range []string{"public-model", ""} {
				t.Run(fmt.Sprintf("action=%q/%s/model=%q", action, credential.name, model), func(t *testing.T) {
					ch := createTestChannelWithAPIKeys(t, client, ctx, t.Name(), credential.keys)
					svc.SetEnabledChannelsForTest([]*Channel{buildChannel(ch, nil)})
					perf := canceledPerf(ch.ID, 7, credential.key)
					perf.Model = model
					before := time.Now()
					acted := svc.executeClientCancellationCooldown(ctx, perf, 2, 10, 5, action)
					updated, err := client.Channel.Get(ctx, ch.ID)
					require.NoError(t, err)
					requireCancellationChannelUnchanged(t, ch, updated)
					if model == "" {
						require.False(t, acted, "missing model must never broaden the cooldown target")
						require.Empty(t, updated.ModelCooldowns)
						return
					}
					require.True(t, acted)
					require.Len(t, updated.ModelCooldowns, 1)
					freeze := updated.ModelCooldowns[0]
					require.Equal(t, "public-model", freeze.Model, "freeze the original request model, not an upstream alias")
					require.NotNil(t, freeze.ExpiresAt, "cancellations must not create permanent freezes")
					require.WithinDuration(t, before.Add(5*time.Minute), *freeze.ExpiresAt, time.Second)
					cached := buildChannel(updated, nil)
					require.True(t, cached.HasActiveModelCooldown("public-model", time.Now()))
					require.False(t, cached.HasActiveModelCooldown("other-model", time.Now()))
				})
			}
		}
	}
}

func TestChannelService_ClientCancellationEligibilityDoesNotCreateCounters(t *testing.T) {
	client, ctx, svc := cancellationTestEnvironment(t)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "eligibility", []string{"key1"})
	withClientCancellationPolicy(t, svc, ctx, ch, ClientCancellationPolicy{
		Enabled: true, Threshold: 1, WindowMinutes: 10, CooldownMinutes: 5, Action: "switch_and_cooldown",
	})
	for _, tc := range []struct {
		name string
		edit func(*PerformanceRecord)
	}{
		{name: "nonstream-abort", edit: func(p *PerformanceRecord) { p.Stream = false }},
		{name: "no-first-token", edit: func(p *PerformanceRecord) { p.FirstTokenTime = nil }},
		{name: "not-canceled", edit: func(p *PerformanceRecord) { p.Canceled = false }},
		{name: "missing-model", edit: func(p *PerformanceRecord) { p.Model = "" }},
		{name: "whitespace-model", edit: func(p *PerformanceRecord) { p.Model = "  " }},
		{name: "invalid-channel", edit: func(p *PerformanceRecord) { p.ChannelID = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			perf := canceledPerf(ch.ID, 7, "key1")
			tc.edit(perf)
			require.False(t, svc.evaluateClientCancellationForFailure(ctx, perf))
			require.Empty(t, svc.cancellationWindow)
			require.Empty(t, svc.cancellationActionsInFlight)
		})
	}
	require.False(t, svc.evaluateClientCancellationForFailure(ctx, nil))
	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireCancellationChannelUnchanged(t, ch, updated)
	require.Empty(t, updated.ModelCooldowns)
}

func TestChannelService_ClientCancellationAggregatesAcrossKeys(t *testing.T) {
	client, ctx, svc := cancellationTestEnvironment(t)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "across-keys", []string{"secret-key-1", "secret-key-2"})
	withClientCancellationPolicy(t, svc, ctx, ch, ClientCancellationPolicy{
		Enabled: true, Threshold: 3, WindowMinutes: 10, CooldownMinutes: 5, Action: "cooldown_model",
	})
	require.False(t, svc.evaluateClientCancellationForFailure(ctx, canceledPerf(ch.ID, 7, "secret-key-1")))
	require.False(t, svc.evaluateClientCancellationForFailure(ctx, canceledPerf(ch.ID, 7, "secret-key-2")))
	require.True(t, svc.evaluateClientCancellationForFailure(ctx, canceledPerf(ch.ID, 7, "")))
	require.Len(t, svc.cancellationWindow[ch.ID], 1)
	for key, timestamps := range svc.cancellationWindow[ch.ID] {
		require.NotContains(t, key, "secret-key")
		require.Len(t, timestamps, 3, "cooldown must not consume the rolling evidence")
	}
	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireCancellationChannelUnchanged(t, ch, updated)
	require.Len(t, updated.ModelCooldowns, 1)
}

func TestChannelService_ClientCancellationPairProjectAndWindowIsolation(t *testing.T) {
	client, ctx, svc := cancellationTestEnvironment(t)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "isolation-1", []string{"key1", "key2"})
	other := createTestChannelWithAPIKeys(t, client, ctx, "isolation-2", []string{"key1", "key2"})
	withClientCancellationPolicy(t, svc, ctx, ch, ClientCancellationPolicy{
		Enabled: true, Threshold: 2, WindowMinutes: 10, CooldownMinutes: 5, Action: "cooldown_only",
	})
	svc.SetEnabledChannelsForTest([]*Channel{buildChannel(ch, nil), buildChannel(other, nil)})
	cancel := func(channelID, projectID int, model, key string) bool {
		perf := canceledPerf(channelID, projectID, key)
		perf.Model = model
		return svc.evaluateClientCancellationForFailure(ctx, perf)
	}
	require.False(t, cancel(ch.ID, 7, "model-a", "key1"))
	require.False(t, cancel(ch.ID, 7, "model-b", "key1"))
	require.False(t, cancel(ch.ID, 8, "model-a", "key1"))
	require.False(t, cancel(other.ID, 7, "model-a", "key1"))
	require.True(t, cancel(ch.ID, 7, "model-a", "key2"))
	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireCancellationChannelUnchanged(t, ch, updated)
	require.Len(t, updated.ModelCooldowns, 1)
	require.Equal(t, "model-a", updated.ModelCooldowns[0].Model)
	untouched, err := client.Channel.Get(ctx, other.ID)
	require.NoError(t, err)
	requireCancellationChannelUnchanged(t, other, untouched)
	require.Empty(t, untouched.ModelCooldowns)

	// A changed duration/threshold is still the same pair/project/window, but
	// changing the window starts a separate window rather than mixing evidence.
	withClientCancellationPolicy(t, svc, ctx, other, ClientCancellationPolicy{
		Enabled: true, Threshold: 2, WindowMinutes: 1, CooldownMinutes: 5, Action: "switch_and_cooldown",
	})
	require.False(t, cancel(other.ID, 7, "model-a", "key2"))
	require.True(t, cancel(other.ID, 7, "model-a", "key1"))
}

func TestChannelService_ClientCancellationSuccessOnlyPrunesExpiredEvidence(t *testing.T) {
	client, ctx, svc := cancellationTestEnvironment(t)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "window-expiry", []string{"key1", "key2"})
	withClientCancellationPolicy(t, svc, ctx, ch, ClientCancellationPolicy{
		Enabled: true, Threshold: 3, WindowMinutes: 1, CooldownMinutes: 5, Action: "cooldown_model",
	})
	perf := canceledPerf(ch.ID, 7, "key1")
	require.False(t, svc.evaluateClientCancellationForFailure(ctx, perf))
	var key string
	for k := range svc.cancellationWindow[ch.ID] {
		key = k
	}
	recent := time.Now().Add(-10 * time.Second)
	svc.cancellationWindow[ch.ID][key] = []time.Time{time.Now().Add(-2 * time.Minute), recent}
	success := &PerformanceRecord{ChannelID: ch.ID, ProjectID: 7, Model: "gpt-4", APIKey: "key2"}
	success.MarkSuccess()
	svc.RecordPerformance(ctx, success)
	require.Equal(t, []time.Time{recent}, svc.cancellationWindow[ch.ID][key])
	require.False(t, svc.evaluateClientCancellationForFailure(ctx, canceledPerf(ch.ID, 7, "key2")))
	require.True(t, svc.evaluateClientCancellationForFailure(ctx, canceledPerf(ch.ID, 7, "key1")))
}

func TestChannelService_ClientCancellationPrunesIdleWindows(t *testing.T) {
	client, ctx, svc := cancellationTestEnvironment(t)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "idle-windows", []string{"key1"})
	withClientCancellationPolicy(t, svc, ctx, ch, ClientCancellationPolicy{
		Enabled: true, Threshold: 2, WindowMinutes: 1, CooldownMinutes: 5, Action: "cooldown_model",
	})
	first := canceledPerf(ch.ID, 7, "key1")
	require.False(t, svc.evaluateClientCancellationForFailure(ctx, first))
	for key := range svc.cancellationWindow[ch.ID] {
		svc.cancellationWindow[ch.ID][key] = []time.Time{time.Now().Add(-2 * time.Minute)}
	}
	other := createTestChannelWithAPIKeys(t, client, ctx, "active-window", []string{"key1"})
	require.False(t, svc.evaluateClientCancellationForFailure(ctx, canceledPerf(other.ID, 7, "key1")))
	require.NotContains(t, svc.cancellationWindow, ch.ID, "idle identities must expire, even if they never cancel again")
	require.Len(t, svc.cancellationWindow[other.ID], 1)
}

func TestChannelService_ClientCancellationCooldownPreservesWindowAndActiveExpiry(t *testing.T) {
	client, ctx, svc := cancellationTestEnvironment(t)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "keep-window", []string{"key1", "key2"})
	withClientCancellationPolicy(t, svc, ctx, ch, ClientCancellationPolicy{
		Enabled: true, Threshold: 2, WindowMinutes: 10, CooldownMinutes: 5, Action: "cooldown_model",
	})
	require.False(t, svc.evaluateClientCancellationForFailure(ctx, canceledPerf(ch.ID, 7, "key1")))
	require.True(t, svc.evaluateClientCancellationForFailure(ctx, canceledPerf(ch.ID, 7, "key2")))
	// Enable ordinary upstream-error rules too: aborted streams must not feed
	// either these counters or the sixth-actual-failure channel breaker.
	require.NoError(t, svc.SystemService.SetRetryPolicy(ctx, &RetryPolicy{
		Enabled: true,
		ClientCancellationPolicy: ClientCancellationPolicy{
			Enabled: true, Threshold: 2, WindowMinutes: 10, CooldownMinutes: 5, Action: "cooldown_model",
		},
		AutoDisableChannel: AutoDisableChannel{
			Enabled: true,
			Rules: []objects.APIKeyAutoDisableRule{{
				StatusCodes: []int{500}, Times: 1, WindowMinutes: 10, Action: objects.APIKeyAutoDisableActionTemporary,
			}},
		},
	}))
	frozen, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Len(t, frozen.ModelCooldowns, 1)
	before := frozen.ModelCooldowns[0]

	// More than six aborts must never enter the ordinary-error permanent breaker.
	for i := range 16 {
		perf := canceledPerf(ch.ID, 7, []string{"key1", "key2"}[i%2])
		perf.ResponseStatusCode = 500
		svc.RecordPerformance(ctx, perf)
	}
	stillFrozen, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireCancellationChannelUnchanged(t, ch, stillFrozen)
	require.Equal(t, []objects.ModelCooldown{before}, stillFrozen.ModelCooldowns, "active freeze must not be extended or made permanent")
	require.Empty(t, svc.cancellationActionsInFlight)
	require.Len(t, svc.cancellationWindow[ch.ID], 1)
	for key, timestamps := range svc.cancellationWindow[ch.ID] {
		require.Len(t, timestamps, 2, "retain only the latest threshold samples, not an unbounded burst")
		require.NotContains(t, key, "key1")
		require.NotContains(t, key, "key2")
	}

	// Simulate expiry, not a new policy window. The unconsumed recent evidence
	// must still allow the next cancellation to freeze this same model again.
	expired := time.Now().Add(-time.Second)
	stillFrozen.ModelCooldowns[0].ExpiresAt = &expired
	require.NoError(t, client.Channel.UpdateOneID(ch.ID).SetModelCooldowns(stillFrozen.ModelCooldowns).Exec(ctx))
	require.True(t, svc.evaluateClientCancellationForFailure(ctx, canceledPerf(ch.ID, 7, "key1")))
	repeated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireCancellationChannelUnchanged(t, ch, repeated)
	require.Len(t, repeated.ModelCooldowns, 1)
	require.NotNil(t, repeated.ModelCooldowns[0].ExpiresAt)
	require.True(t, repeated.ModelCooldowns[0].ExpiresAt.After(time.Now()))
}

func TestChannelService_ClientCancellationPolicyShapeDoesNotConsumeWindow(t *testing.T) {
	client, ctx, svc := cancellationTestEnvironment(t)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "policy-shape", []string{"key1", "key2"})
	policy := ClientCancellationPolicy{
		Enabled: true, Threshold: 3, WindowMinutes: 10, CooldownMinutes: 5, Action: "cooldown_only",
	}
	withClientCancellationPolicy(t, svc, ctx, ch, policy)
	require.False(t, svc.evaluateClientCancellationForFailure(ctx, canceledPerf(ch.ID, 7, "key1")))
	policy.Threshold = 2
	policy.CooldownMinutes = 8
	policy.Action = "switch_and_cooldown"
	withClientCancellationPolicy(t, svc, ctx, ch, policy)
	require.True(t, svc.evaluateClientCancellationForFailure(ctx, canceledPerf(ch.ID, 7, "key2")))
	require.Len(t, svc.cancellationWindow[ch.ID], 1)
	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireCancellationChannelUnchanged(t, ch, updated)
	require.Len(t, updated.ModelCooldowns, 1)
	require.Contains(t, updated.ModelCooldowns[0].Reason, "auto-recover after 8 minutes")
}

func TestChannelService_ClientCancellationConcurrentKeysShareOneModelWindow(t *testing.T) {
	client, ctx, svc := cancellationTestEnvironment(t)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "concurrent-keys", []string{"key1", "key2"})
	withClientCancellationPolicy(t, svc, ctx, ch, ClientCancellationPolicy{
		Enabled: true, Threshold: 2, WindowMinutes: 10, CooldownMinutes: 5, Action: "cooldown_model",
	})
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic evaluating cancellation: %v", r)
				}
			}()
			svc.evaluateClientCancellationForFailure(ctx, canceledPerf(ch.ID, 7, []string{"key1", "key2"}[i%2]))
		}()
	}
	wg.Wait()
	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireCancellationChannelUnchanged(t, ch, updated)
	require.Len(t, updated.ModelCooldowns, 1)
	require.NotNil(t, updated.ModelCooldowns[0].ExpiresAt)
	require.Empty(t, svc.cancellationActionsInFlight)
	require.Len(t, svc.cancellationWindow[ch.ID], 1)
	for key, timestamps := range svc.cancellationWindow[ch.ID] {
		require.Len(t, timestamps, 2)
		require.False(t, strings.Contains(key, "key1") || strings.Contains(key, "key2"))
		require.True(t, slices.IsSortedFunc(timestamps, time.Time.Compare))
	}
}

func TestChannelService_ClientCancellationNonstreamAndPreTokenAbortsAreNotFailures(t *testing.T) {
	client, ctx, svc := cancellationTestEnvironment(t)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "abort-not-failure", []string{"key1"})
	withClientCancellationPolicy(t, svc, ctx, ch, ClientCancellationPolicy{
		Enabled: true, Threshold: 1, WindowMinutes: 10, CooldownMinutes: 5, Action: "switch_and_cooldown",
	})
	require.NoError(t, svc.SystemService.SetRetryPolicy(ctx, &RetryPolicy{
		Enabled: true,
		ClientCancellationPolicy: ClientCancellationPolicy{
			Enabled: true, Threshold: 1, WindowMinutes: 10, CooldownMinutes: 5, Action: "switch_and_cooldown",
		},
		AutoDisableChannel: AutoDisableChannel{
			Enabled: true,
			Rules: []objects.APIKeyAutoDisableRule{{
				Times: 1, WindowMinutes: 10, Action: objects.APIKeyAutoDisableActionTemporary,
			}},
		},
	}))
	for i := range 16 {
		perf := canceledPerf(ch.ID, 7, "key1")
		if i%2 == 0 {
			perf.Stream = false
		} else {
			perf.FirstTokenTime = nil
		}
		perf.ResponseStatusCode = 500
		svc.RecordPerformance(ctx, perf)
	}
	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireCancellationChannelUnchanged(t, ch, updated)
	require.Empty(t, updated.ModelCooldowns)
	require.Empty(t, svc.cancellationWindow)
	require.Empty(t, svc.cancellationActionsInFlight)
	metrics := svc.channelPerfMetrics[ch.ID].aggregatedMetrics
	require.Zero(t, metrics.FailureCount)
	require.Zero(t, metrics.ConsecutiveFailures)
}
