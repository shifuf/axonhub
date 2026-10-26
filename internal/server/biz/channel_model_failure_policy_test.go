package biz

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/channelfailureevent"
	"github.com/looplj/axonhub/internal/objects"
)

func TestChannelService_ModelFailurePolicyLegacyActionsNeverDisableKeys(t *testing.T) {
	for _, action := range []objects.APIKeyAutoDisableAction{objects.APIKeyAutoDisableActionTemporary,
		objects.APIKeyAutoDisableActionPermanent, objects.APIKeyAutoDisableActionPermanentDelete, objects.APIKeyAutoDisableActionUntilCron} {
		for _, keyed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/keyed=%v", action, keyed), func(t *testing.T) {
				rule := objects.APIKeyAutoDisableRule{Times: 1, Action: action, DisableUntilCron: "0 0 * * *"}
				if action == objects.APIKeyAutoDisableActionTemporary {
					rule.DisableDurationMinutes = lo.ToPtr(17)
				}
				ctx, svc, ch := newWindowRuleTestService(t, rule)
				perf := &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", ResponseStatusCode: 503, ErrorMessage: "No upstream available"}
				if keyed {
					perf.APIKey = "key1"
				}
				_, acted := svc.evaluateAutoDisableForFailure(ctx, perf)
				require.True(t, acted)
				stored, err := svc.db.Channel.Get(ctx, ch.ID)
				require.NoError(t, err)
				require.Equal(t, channel.StatusEnabled, stored.Status)
				require.Empty(t, stored.DisabledAPIKeys)
				require.Equal(t, []string{"key1", "key2", "key3"}, stored.Credentials.APIKeys)
				require.Len(t, stored.ModelCooldowns, 1)
				require.Equal(t, "gpt-4", stored.ModelCooldowns[0].Model)
				require.NotNil(t, stored.ModelCooldowns[0].ExpiresAt)
				minutes := 30
				if action == objects.APIKeyAutoDisableActionTemporary {
					minutes = 17
				}
				require.WithinDuration(t, time.Now().Add(time.Duration(minutes)*time.Minute), *stored.ModelCooldowns[0].ExpiresAt, 5*time.Second)
				require.Contains(t, stored.ModelCooldowns[0].Reason, "HTTP 503")
				require.Contains(t, stored.ModelCooldowns[0].Reason, "No upstream available")
			})
		}
	}
}

func TestChannelService_ModelFailurePolicySixthFailureEscalatesWithoutConsumingWindow(t *testing.T) {
	rule := objects.APIKeyAutoDisableRule{Times: 2, WindowMinutes: 10, FreezeModel: true,
		Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: lo.ToPtr(17)}
	ctx, svc, ch := newWindowRuleTestService(t, rule)
	var firstExpiry time.Time
	for i := 1; i <= 6; i++ {
		perf := &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", APIKey: fmt.Sprintf("key%d", (i-1)%3+1),
			ResponseStatusCode: 503, ErrorMessage: fmt.Sprintf("provider failure %d", i)}
		svc.evaluateAutoDisableForFailure(ctx, perf)
		// Successful traffic and temporary freezes must not erase the burst.
		svc.clearAutoDisableCountsOnSuccess(perf)
		stored, err := svc.db.Channel.Get(ctx, ch.ID)
		require.NoError(t, err)
		require.Empty(t, stored.DisabledAPIKeys)
		if i < 6 {
			require.Equal(t, channel.StatusEnabled, stored.Status, "failure %d", i)
			if i == 1 {
				require.Empty(t, stored.ModelCooldowns)
			} else {
				require.Len(t, stored.ModelCooldowns, 1)
				if i == 2 {
					firstExpiry = *stored.ModelCooldowns[0].ExpiresAt
				}
				require.True(t, firstExpiry.Equal(*stored.ModelCooldowns[0].ExpiresAt), "later failures cannot extend an active model freeze")
			}
		} else {
			require.Equal(t, channel.StatusEnabled, stored.Status, "the safeguard freezes the model, not the channel")
			require.Nil(t, stored.AutoDisabledAt)
			require.Len(t, stored.ModelCooldowns, 1)
			require.Nil(t, stored.ModelCooldowns[0].ExpiresAt, "safeguard freeze requires manual recovery")
			require.Contains(t, stored.ModelCooldowns[0].Reason, "gpt-4")
			require.Contains(t, stored.ModelCooldowns[0].Reason, "6 failures within 1440 minutes")
			require.Contains(t, stored.ModelCooldowns[0].Reason, "provider failure 6")
		}
	}
}

func TestChannelService_ModelFailurePolicyDifferentRulesSharePairWindowEscalation(t *testing.T) {
	first := objects.APIKeyAutoDisableRule{Times: 4, WindowMinutes: 10, Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: lo.ToPtr(5), StatusCodes: []int{401}}
	ctx, svc, ch := newWindowRuleTestService(t, first)
	second := first
	second.StatusCodes = []int{429}
	for i := 0; i < 6; i++ {
		code := 401
		if i%2 == 1 {
			code = 429
		}
		svc.evaluateRules(ctx, &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: code},
			[]objects.APIKeyAutoDisableRule{first, second}, autoDisableScopeChannel, defaultPermanentModelFailureThreshold, defaultPermanentModelFailureWindowMinutes)
	}
	stored, err := svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, stored.Status, "the safeguard freezes the model, not the channel")
	require.Len(t, stored.ModelCooldowns, 1)
	require.Equal(t, "gpt-4", stored.ModelCooldowns[0].Model)
	require.Nil(t, stored.ModelCooldowns[0].ExpiresAt)
	require.Empty(t, stored.DisabledAPIKeys)
}

func TestChannelService_ModelFailurePolicyIsolatesModelsAndChannels(t *testing.T) {
	rule := objects.APIKeyAutoDisableRule{Times: 2, WindowMinutes: 10, Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: lo.ToPtr(5)}
	ctx, svc, ch := newWindowRuleTestService(t, rule)
	other := createTestChannelWithAPIKeys(t, svc.db, ctx, "other-window-channel", []string{"other-key"})
	other, err := svc.db.Channel.UpdateOne(other).SetPolicies(ch.Policies).Save(ctx)
	require.NoError(t, err)
	svc.SetEnabledChannelsForTest([]*Channel{buildChannel(ch, nil), buildChannel(other, nil)})
	for i := 0; i < 5; i++ {
		for _, perf := range []*PerformanceRecord{{ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1"}, {ChannelID: ch.ID, Model: "gpt-4o", APIKey: "key2"}, {ChannelID: other.ID, Model: "gpt-4", APIKey: "other-key"}} {
			perf.ResponseStatusCode = 503
			svc.evaluateAutoDisableForFailure(ctx, perf)
		}
	}
	svc.evaluateAutoDisableForFailure(ctx, &PerformanceRecord{ChannelID: other.ID, Model: "gpt-4", APIKey: "other-key", ResponseStatusCode: 503})
	stored, err := svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, stored.Status)
	otherStored, err := svc.db.Channel.Get(ctx, other.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, otherStored.Status, "the safeguard freezes models, never channels")
	require.Len(t, otherStored.ModelCooldowns, 1)
	require.Equal(t, "gpt-4", otherStored.ModelCooldowns[0].Model)
	require.Nil(t, otherStored.ModelCooldowns[0].ExpiresAt)
	require.Empty(t, stored.DisabledAPIKeys)
	require.Empty(t, otherStored.DisabledAPIKeys)
}

func TestChannelService_ModelFailurePolicyOldQueuedFailuresDoNotCount(t *testing.T) {
	rule := objects.APIKeyAutoDisableRule{Times: 2, WindowMinutes: 1, Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: lo.ToPtr(5)}
	ctx, svc, ch := newWindowRuleTestService(t, rule)
	for range 6 {
		svc.evaluateAutoDisableForFailure(ctx, &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 503, EndTime: time.Now().Add(-2 * time.Minute)})
	}
	svc.evaluateAutoDisableForFailure(ctx, &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 503})
	stored, err := svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, stored.Status)
	require.Empty(t, stored.ModelCooldowns)
	require.Empty(t, stored.DisabledAPIKeys)
}

func TestChannelService_ModelFailurePolicyMissingModelNeverBroadensAction(t *testing.T) {
	rule := objects.APIKeyAutoDisableRule{Times: 1, Action: objects.APIKeyAutoDisableActionPermanentDelete}
	ctx, svc, ch := newWindowRuleTestService(t, rule)
	for range 6 {
		_, acted := svc.evaluateAutoDisableForFailure(ctx, &PerformanceRecord{ChannelID: ch.ID, APIKey: "key1", ResponseStatusCode: 503})
		require.False(t, acted)
	}
	stored, err := svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, stored.Status)
	require.Empty(t, stored.DisabledAPIKeys)
	require.Equal(t, ch.Credentials.APIKeys, stored.Credentials.APIKeys)
}

func TestChannelService_ModelFailurePolicySixthDuringInflightCooldownStillEscalates(t *testing.T) {
	rule := objects.APIKeyAutoDisableRule{Times: 2, WindowMinutes: 10, FreezeModel: true, Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: lo.ToPtr(5)}
	ctx, svc, ch := newWindowRuleTestService(t, rule)
	started, release := make(chan struct{}), make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	var once sync.Once
	svc.db.Channel.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, mutation ent.Mutation) (ent.Value, error) {
			once.Do(func() { close(started); <-release })
			return next.Mutate(ctx, mutation)
		})
	})
	perf := &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 503, ErrorMessage: "failure1"}
	svc.evaluateAutoDisableForFailure(ctx, perf)
	result := make(chan bool, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Errorf("action panic: %v", recovered)
				result <- false
			}
		}()
		_, acted := svc.evaluateAutoDisableForFailure(ctx, perf)
		result <- acted
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("cooldown did not start")
	}
	for i := 3; i <= 6; i++ {
		svc.evaluateAutoDisableForFailure(ctx, &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 503, ErrorMessage: fmt.Sprintf("failure%d", i)})
	}
	close(release)
	released = true
	select {
	case acted := <-result:
		require.True(t, acted)
	case <-time.After(5 * time.Second):
		t.Fatal("cooldown did not complete")
	}
	stored, err := svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, stored.Status, "sixth failure cannot be lost while another action owns the claim")
	require.Len(t, stored.ModelCooldowns, 1)
	require.Nil(t, stored.ModelCooldowns[0].ExpiresAt, "escalated freeze requires manual recovery")
	require.Empty(t, stored.DisabledAPIKeys)
	require.Contains(t, stored.ModelCooldowns[0].Reason, "failure6")
}

func TestSystemService_ModelFailurePolicyValidatesBeforeLegacyUpgrade(t *testing.T) {
	ctx, svc, _ := newWindowRuleTestService(t, objects.APIKeyAutoDisableRule{Times: 1, Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: lo.ToPtr(5)})
	for _, rule := range []objects.APIKeyAutoDisableRule{
		{Times: 1, WindowMinutes: -1, Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: lo.ToPtr(5)},
		{Times: 1, WindowMinutes: 10, Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: lo.ToPtr(0)},
	} {
		require.Error(t, svc.SystemService.SetRetryPolicy(ctx, &RetryPolicy{AutoDisableChannel: AutoDisableChannel{Enabled: true, Rules: []objects.APIKeyAutoDisableRule{rule}}}))
	}
}

func TestChannelService_ModelFailurePolicyBlankModelNeverBroadensAction(t *testing.T) {
	ctx, svc, ch := newWindowRuleTestService(t, objects.APIKeyAutoDisableRule{Times: 1, Action: objects.APIKeyAutoDisableActionPermanentDelete})
	for range 6 {
		_, acted := svc.evaluateAutoDisableForFailure(ctx, &PerformanceRecord{ChannelID: ch.ID, Model: " \t ", APIKey: "key1", ResponseStatusCode: 503})
		require.False(t, acted)
	}
	stored, err := svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, stored.Status)
	require.Empty(t, stored.ModelCooldowns)
	require.Empty(t, stored.DisabledAPIKeys)
}

func TestDefaultRetryPolicyUsesModelOnlyCancellation(t *testing.T) {
	require.Equal(t, ClientCancellationActionCooldownModel, defaultRetryPolicy.ClientCancellationPolicy.Action)
	require.Equal(t, defaultClientCancellationWindowMinutes, defaultRetryPolicy.ClientCancellationPolicy.WindowMinutes)
}

func TestChannelService_PermanentWindowAggregatesRuleWindowsAndExpiresBurst(t *testing.T) {
	rule := objects.APIKeyAutoDisableRule{Times: 100, WindowMinutes: 1, Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: lo.ToPtr(5)}
	ctx, svc, ch := newWindowRuleTestService(t, rule)
	perf := &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 503}
	for range 5 {
		svc.evaluateAutoDisableForFailure(ctx, perf)
	}
	// Seed expired evidence instead of sleeping for a minute.
	old := time.Now().Add(-25 * time.Hour)
	_, err := svc.db.ChannelFailureEvent.Update().
		Where(channelfailureevent.ChannelID(ch.ID)).
		SetFailedAt(old).
		Save(ctx)
	require.NoError(t, err)
	svc.evaluateAutoDisableForFailure(ctx, perf)
	stored, err := svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, stored.Status)
	require.Empty(t, stored.ModelCooldowns)
	longer := rule
	longer.WindowMinutes = 10
	for range 4 {
		svc.evaluateRules(ctx, perf, []objects.APIKeyAutoDisableRule{longer}, autoDisableScopeChannel, defaultPermanentModelFailureThreshold, defaultPermanentModelFailureWindowMinutes)
	}
	stored, err = svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, stored.Status)
	require.Empty(t, stored.ModelCooldowns)

	// One current failure in the 1-minute rule and five in the 10-minute rule
	// jointly reach the configured 24-hour permanent-freeze threshold.
	svc.evaluateRules(ctx, perf, []objects.APIKeyAutoDisableRule{longer}, autoDisableScopeChannel, defaultPermanentModelFailureThreshold, defaultPermanentModelFailureWindowMinutes)
	stored, err = svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, stored.Status, "permanent freeze must not disable the whole channel")
	require.Len(t, stored.ModelCooldowns, 1)
	require.Nil(t, stored.ModelCooldowns[0].ExpiresAt)
}

func TestChannelService_ModelFailurePolicyEscalationRetriesAfterDatabaseFailure(t *testing.T) {
	rule := objects.APIKeyAutoDisableRule{Times: 2, WindowMinutes: 10, Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: lo.ToPtr(5)}
	ctx, svc, ch := newWindowRuleTestService(t, rule)
	rejected := false
	svc.db.Channel.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, mutation ent.Mutation) (ent.Value, error) {
			if change, ok := mutation.(*ent.ChannelMutation); ok {
				if cooldowns, set := change.ModelCooldowns(); set && !rejected {
					for _, mc := range cooldowns {
						if strings.Contains(mc.Reason, "Permanently frozen") {
							rejected = true
							return nil, fmt.Errorf("database temporarily unavailable")
						}
					}
				}
			}
			return next.Mutate(ctx, mutation)
		})
	})
	perf := &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 503}
	for range 6 {
		svc.evaluateAutoDisableForFailure(ctx, perf)
	}
	require.True(t, rejected)
	stored, err := svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, stored.Status)
	require.Empty(t, svc.windowedRuleActionsInFlight[ch.ID])
	svc.evaluateAutoDisableForFailure(ctx, perf)
	stored, err = svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Len(t, stored.ModelCooldowns, 1)
	require.Nil(t, stored.ModelCooldowns[0].ExpiresAt)
	require.Contains(t, stored.ModelCooldowns[0].Reason, "Permanently frozen")
	require.Empty(t, stored.DisabledAPIKeys)
}

func TestChannelService_ModelFailurePolicyLatchesSixthDuringInflightCooldown(t *testing.T) {
	rule := objects.APIKeyAutoDisableRule{Times: 2, WindowMinutes: 1, Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: lo.ToPtr(5)}
	ctx, svc, ch := newWindowRuleTestService(t, rule)
	started, release := make(chan struct{}), make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	var once sync.Once
	svc.db.Channel.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, mutation ent.Mutation) (ent.Value, error) {
			once.Do(func() { close(started); <-release })
			return next.Mutate(ctx, mutation)
		})
	})
	perf := func(number int) *PerformanceRecord {
		return &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 503, ErrorMessage: fmt.Sprintf("failure%d", number)}
	}
	_, acted := svc.evaluateAutoDisableForFailure(ctx, perf(1))
	require.False(t, acted)
	done := make(chan bool, 1)
	go func() { _, acted := svc.evaluateAutoDisableForFailure(ctx, perf(2)); done <- acted }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model cooldown did not start")
	}
	for number := 3; number <= 6; number++ {
		_, acted = svc.evaluateAutoDisableForFailure(ctx, perf(number))
		require.False(t, acted, "the in-flight cooldown owns the action claim")
	}
	// The sixth event qualified while the action was in flight. It must remain
	// latched even if the persistence call returns after the rolling window.
	time.Sleep(1100 * time.Millisecond)
	close(release)
	released = true
	select {
	case acted = <-done:
		require.True(t, acted)
	case <-time.After(5 * time.Second):
		t.Fatal("model cooldown did not finish")
	}
	stored, err := svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, stored.Status)
	require.Len(t, stored.ModelCooldowns, 1)
	require.Nil(t, stored.ModelCooldowns[0].ExpiresAt)
	require.Empty(t, stored.DisabledAPIKeys)
	require.Contains(t, stored.ModelCooldowns[0].Reason, "6 failures within 1440 minutes")
	require.Contains(t, stored.ModelCooldowns[0].Reason, "failure6")
}
