package biz

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
)

func withClientCancellationPolicy(
	t *testing.T,
	svc *ChannelService,
	ctx context.Context,
	ch *ent.Channel,
	policy ClientCancellationPolicy,
) {
	t.Helper()

	require.NoError(t, svc.SystemService.SetRetryPolicy(ctx, &RetryPolicy{
		Enabled:                  true,
		ClientCancellationPolicy: policy,
	}))
	svc.SetEnabledChannelsForTest([]*Channel{buildChannel(ch, nil)})
}

func canceledPerf(channelID int, projectID int, apiKey string) *PerformanceRecord {
	perf := &PerformanceRecord{ChannelID: channelID, ProjectID: projectID, Model: "gpt-4", APIKey: apiKey, Stream: true}
	perf.MarkCanceled()
	// Cancellations only count after the upstream produced something; give the
	// record first-token evidence so the cooldown logic applies.
	firstToken := time.Now()
	perf.FirstTokenTime = &firstToken

	return perf
}

func TestNormalizeClientCancellationPolicy_DefaultsAndClamp(t *testing.T) {
	policy := ClientCancellationPolicy{Enabled: true}
	require.NoError(t, normalizeClientCancellationPolicy(&policy))

	require.Equal(t, defaultClientCancellationThreshold, policy.Threshold)
	require.Equal(t, defaultClientCancellationCooldownMinutes, policy.CooldownMinutes)
	require.Equal(t, ClientCancellationActionCooldownModel, policy.Action)
	require.Equal(t, ClientCancellationScopeGlobal, policy.Scope)
	require.NotNil(t, policy.ProjectOverrides)

	policy = ClientCancellationPolicy{
		Enabled:         true,
		Threshold:       -3,
		CooldownMinutes: maxClientCancellationCooldownMinutes + 60,
		Action:          "unknown",
		Scope:           "unknown",
	}
	require.NoError(t, normalizeClientCancellationPolicy(&policy))
	require.Equal(t, defaultClientCancellationThreshold, policy.Threshold)
	require.Equal(t, maxClientCancellationCooldownMinutes, policy.CooldownMinutes)
	require.Equal(t, ClientCancellationActionCooldownModel, policy.Action)
	require.Equal(t, ClientCancellationScopeGlobal, policy.Scope)
}

func TestNormalizeClientCancellationPolicy_LegacyEmptyPolicyStaysEnabled(t *testing.T) {
	// A retry policy stored before this feature existed decodes to zero values.
	policy := ClientCancellationPolicy{}
	require.NoError(t, normalizeClientCancellationPolicy(&policy))
	require.True(t, policy.Enabled)

	// An explicit disable keeps the other normalized fields, so it stays off.
	policy = ClientCancellationPolicy{Enabled: false, Threshold: 2, CooldownMinutes: 30, Action: ClientCancellationActionCooldownOnly}
	require.NoError(t, normalizeClientCancellationPolicy(&policy))
	require.False(t, policy.Enabled)
}

func TestNormalizeClientCancellationPolicy_ProjectOverrides(t *testing.T) {
	policy := ClientCancellationPolicy{
		Enabled:         true,
		Threshold:       3,
		CooldownMinutes: 15,
		Action:          ClientCancellationActionCooldownOnly,
		Scope:           ClientCancellationScopeProjectFirst,
		ProjectOverrides: []ClientCancellationProjectPolicy{
			{ProjectID: 0, Enabled: true},
			{ProjectID: 7, Enabled: true},
			{ProjectID: 7, Enabled: false, Threshold: 9},
			{ProjectID: 11, Enabled: true, Threshold: 1, CooldownMinutes: 5, Action: "invalid"},
		},
	}
	require.NoError(t, normalizeClientCancellationPolicy(&policy))

	require.Len(t, policy.ProjectOverrides, 2)
	require.Equal(t, 7, policy.ProjectOverrides[0].ProjectID)
	require.Equal(t, 3, policy.ProjectOverrides[0].Threshold)
	require.Equal(t, 15, policy.ProjectOverrides[0].CooldownMinutes)
	require.Equal(t, ClientCancellationActionCooldownModel, policy.ProjectOverrides[0].Action)

	require.Equal(t, 11, policy.ProjectOverrides[1].ProjectID)
	require.Equal(t, 1, policy.ProjectOverrides[1].Threshold)
	require.Equal(t, 5, policy.ProjectOverrides[1].CooldownMinutes)
	require.Equal(t, ClientCancellationActionCooldownModel, policy.ProjectOverrides[1].Action)
}

func TestClientCancellationPolicy_EffectiveGlobalScope(t *testing.T) {
	policy := ClientCancellationPolicy{
		Enabled:         true,
		Threshold:       2,
		CooldownMinutes: 30,
		Action:          ClientCancellationActionSwitchAndCooldown,
		Scope:           ClientCancellationScopeGlobal,
		ProjectOverrides: []ClientCancellationProjectPolicy{
			{ProjectID: 5, Enabled: true, Threshold: 1, CooldownMinutes: 5, Action: ClientCancellationActionCooldownOnly},
		},
	}
	require.NoError(t, normalizeClientCancellationPolicy(&policy))

	effective, source := policy.EffectiveClientCancellationPolicy(5)
	require.Equal(t, "global", source)
	require.Equal(t, 2, effective.Threshold)
	require.Equal(t, 30, effective.CooldownMinutes)
}

func TestClientCancellationPolicy_EffectiveProjectFirst(t *testing.T) {
	policy := ClientCancellationPolicy{
		Enabled:         true,
		Threshold:       2,
		CooldownMinutes: 30,
		Action:          ClientCancellationActionSwitchAndCooldown,
		Scope:           ClientCancellationScopeProjectFirst,
		ProjectOverrides: []ClientCancellationProjectPolicy{
			{ProjectID: 5, Enabled: true, Threshold: 4, CooldownMinutes: 10, Action: ClientCancellationActionCooldownOnly},
			{ProjectID: 6, Enabled: false, Threshold: 1},
		},
	}
	require.NoError(t, normalizeClientCancellationPolicy(&policy))

	effective, source := policy.EffectiveClientCancellationPolicy(5)
	require.Equal(t, "project", source)
	require.Equal(t, 4, effective.Threshold)
	require.Equal(t, 10, effective.CooldownMinutes)
	require.Equal(t, ClientCancellationActionCooldownModel, effective.Action)

	// A disabled override falls back to the global settings.
	effective, source = policy.EffectiveClientCancellationPolicy(6)
	require.Equal(t, "global", source)
	require.Equal(t, 2, effective.Threshold)

	// Projects without an override use the global settings too.
	effective, source = policy.EffectiveClientCancellationPolicy(99)
	require.Equal(t, "global", source)
	require.Equal(t, 2, effective.Threshold)
}

func TestChannelService_ClientCancellationLegacySwitchFreezesOnlyModel(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "cancel-channel", []string{"key1", "key2"})
	withClientCancellationPolicy(t, svc, ctx, ch, ClientCancellationPolicy{
		Enabled: true, Threshold: 2, WindowMinutes: 10, CooldownMinutes: 10,
		Action: ClientCancellationActionSwitchAndCooldown, Scope: ClientCancellationScopeGlobal,
	})

	require.False(t, svc.evaluateClientCancellationForFailure(ctx, canceledPerf(ch.ID, 0, "key1")))
	before := time.Now()
	require.True(t, svc.evaluateClientCancellationForFailure(ctx, canceledPerf(ch.ID, 0, "key1")))

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireCancellationChannelUnchanged(t, ch, updated)
	require.Len(t, updated.ModelCooldowns, 1)
	require.Equal(t, "gpt-4", updated.ModelCooldowns[0].Model)
	require.NotNil(t, updated.ModelCooldowns[0].ExpiresAt)
	require.WithinDuration(t, before.Add(10*time.Minute), *updated.ModelCooldowns[0].ExpiresAt, time.Second)
	require.Contains(t, updated.ModelCooldowns[0].Reason, "2 cancellations within 10 minutes")
}

func TestChannelService_ClientCancellationLegacyCredentialActionFreezesOnlyModel(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "cancel-channel-keys", []string{"key1", "key2"})
	withClientCancellationPolicy(t, svc, ctx, ch, ClientCancellationPolicy{
		Enabled: true, Threshold: 1, WindowMinutes: 10, CooldownMinutes: 5,
		Action: ClientCancellationActionCooldownOnly, Scope: ClientCancellationScopeGlobal,
	})

	require.True(t, svc.evaluateClientCancellationForFailure(ctx, canceledPerf(ch.ID, 0, "key1")))

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireCancellationChannelUnchanged(t, ch, updated)
	require.Len(t, updated.ModelCooldowns, 1)
	require.Equal(t, "gpt-4", updated.ModelCooldowns[0].Model)
	require.NotNil(t, updated.ModelCooldowns[0].ExpiresAt)
}

func TestChannelService_ClientCancellationSuccessPreservesWindow(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "cancel-reset", []string{"key1"})
	withClientCancellationPolicy(t, svc, ctx, ch, ClientCancellationPolicy{
		Enabled:         true,
		Threshold:       2,
		CooldownMinutes: 10,
		Action:          ClientCancellationActionSwitchAndCooldown,
		Scope:           ClientCancellationScopeGlobal,
	})

	require.False(t, svc.evaluateClientCancellationForFailure(ctx, canceledPerf(ch.ID, 0, "key1")))

	success := &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1"}
	success.MarkSuccess()
	svc.RecordPerformance(ctx, success)

	// A success does not erase a still-unexpired cancellation in this window.
	require.True(t, svc.evaluateClientCancellationForFailure(ctx, canceledPerf(ch.ID, 0, "key1")))

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, updated.Status)
}

func TestChannelService_ClientCancellationDisabled(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "cancel-disabled", []string{"key1"})
	withClientCancellationPolicy(t, svc, ctx, ch, ClientCancellationPolicy{
		Enabled:         false,
		Threshold:       1,
		CooldownMinutes: 10,
		Action:          ClientCancellationActionSwitchAndCooldown,
		Scope:           ClientCancellationScopeGlobal,
	})

	require.False(t, svc.evaluateClientCancellationForFailure(ctx, canceledPerf(ch.ID, 0, "key1")))
	require.False(t, svc.evaluateClientCancellationForFailure(ctx, canceledPerf(ch.ID, 0, "key1")))

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, updated.Status)
}

func TestChannelService_ClientCancellationProjectOverride(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "cancel-project", []string{"key1"})
	withClientCancellationPolicy(t, svc, ctx, ch, ClientCancellationPolicy{
		Enabled:         true,
		Threshold:       5,
		CooldownMinutes: 30,
		Action:          ClientCancellationActionSwitchAndCooldown,
		Scope:           ClientCancellationScopeProjectFirst,
		ProjectOverrides: []ClientCancellationProjectPolicy{
			{ProjectID: 42, Enabled: true, Threshold: 1, CooldownMinutes: 1},
		},
	})

	// Project 42 uses the aggressive override and cools down on the first cancel.
	require.True(t, svc.evaluateClientCancellationForFailure(ctx, canceledPerf(ch.ID, 42, "key1")))

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireCancellationChannelUnchanged(t, ch, updated)
	require.Len(t, updated.ModelCooldowns, 1)
	require.NotNil(t, updated.ModelCooldowns[0].ExpiresAt)

	// Other projects inherit the global threshold of 5.
	_, err = svc.ClearChannelModelCooldown(ctx, ch.ID, "gpt-4")
	require.NoError(t, err)
	for range 4 {
		require.False(t, svc.evaluateClientCancellationForFailure(ctx, canceledPerf(ch.ID, 7, "key1")))
	}
	require.True(t, svc.evaluateClientCancellationForFailure(ctx, canceledPerf(ch.ID, 7, "key1")))
}
func TestChannelService_ClientCancellationModelRecoversAfterExpiry(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "cancel-recover", []string{"key1", "key2"})
	withClientCancellationPolicy(t, svc, ctx, ch, ClientCancellationPolicy{
		Enabled: true, Threshold: 1, WindowMinutes: 10, CooldownMinutes: 5,
		Action: ClientCancellationActionCooldownOnly, Scope: ClientCancellationScopeGlobal,
	})

	require.True(t, svc.evaluateClientCancellationForFailure(ctx, canceledPerf(ch.ID, 0, "key1")))

	frozen, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireCancellationChannelUnchanged(t, ch, frozen)
	require.Len(t, frozen.ModelCooldowns, 1)

	// Only the model freeze expires; channel and credentials were never disabled.
	expired := time.Now().Add(-time.Minute)
	frozen.ModelCooldowns[0].ExpiresAt = &expired
	require.NoError(t, client.Channel.UpdateOneID(ch.ID).SetModelCooldowns(frozen.ModelCooldowns).Exec(ctx))
	changed, removed, err := svc.cleanupChannelExpiredModelCooldowns(ctx, ch.ID)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, 1, removed)

	recovered, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	requireCancellationChannelUnchanged(t, ch, recovered)
	require.Empty(t, recovered.ModelCooldowns)
}
