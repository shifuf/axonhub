package biz

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
)

func newWindowRuleTestService(t *testing.T, rule objects.APIKeyAutoDisableRule) (context.Context, *ChannelService, *ent.Channel) {
	t.Helper()
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)
	t.Cleanup(svc.Stop)
	ch := createTestChannelWithAPIKeys(t, client, ctx, "window-rule", []string{"key1", "key2", "key3"})
	var err error
	ch, err = client.Channel.UpdateOneID(ch.ID).SetPolicies(objects.ChannelPolicies{
		APIKeyAutoDisableMode:  objects.APIKeyAutoDisableModeCustom,
		APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{rule},
	}).Save(ctx)
	require.NoError(t, err)
	svc.SetEnabledChannelsForTest([]*Channel{buildChannel(ch, nil)})
	return ctx, svc, ch
}

func TestChannelService_WindowFreezeModelIsolatesModelsAndCombinesKeys(t *testing.T) {
	rule := objects.APIKeyAutoDisableRule{Times: 2, WindowMinutes: 10, FreezeModel: true,
		Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: lo.ToPtr(15)}
	ctx, svc, ch := newWindowRuleTestService(t, rule)
	failure := func(model, key string) bool {
		_, acted := svc.evaluateAutoDisableForFailure(ctx, &PerformanceRecord{
			ChannelID: ch.ID, Model: model, APIKey: key, ResponseStatusCode: 500})
		return acted
	}
	require.False(t, failure("gpt-4", "key1"))
	require.False(t, failure("gpt-4o", "key1"), "another model must have its own failure window")
	require.True(t, failure("gpt-4", "key2"), "same model shares a window across channel credentials")
	updated, err := svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, updated.Status)
	require.Empty(t, updated.DisabledAPIKeys)
	require.Len(t, updated.ModelCooldowns, 1)
	require.Equal(t, "gpt-4", updated.ModelCooldowns[0].Model)
	require.Contains(t, updated.ModelCooldowns[0].Reason, "2 failures within 10 minutes")
	require.WithinDuration(t, time.Now().Add(15*time.Minute), *updated.ModelCooldowns[0].ExpiresAt, 5*time.Second)
	cached := buildChannel(updated, nil)
	require.NotNil(t, cached)
	require.True(t, cached.HasActiveModelCooldown("gpt-4", time.Now()))
	require.False(t, cached.HasActiveModelCooldown("gpt-4o", time.Now()))
}

func TestChannelService_WindowLegacyActionsPreserveCredentials(t *testing.T) {
	for _, action := range []objects.APIKeyAutoDisableAction{objects.APIKeyAutoDisableActionTemporary, objects.APIKeyAutoDisableActionPermanent, objects.APIKeyAutoDisableActionUntilCron, objects.APIKeyAutoDisableActionPermanentDelete} {
		t.Run(string(action), func(t *testing.T) {
			rule := objects.APIKeyAutoDisableRule{Times: 1, WindowMinutes: 10, Action: action, DisableDurationMinutes: lo.ToPtr(15), DisableUntilCron: "0 0 * * *"}
			ctx, svc, ch := newWindowRuleTestService(t, rule)
			_, acted := svc.evaluateRules(ctx, &PerformanceRecord{ChannelID: ch.ID, APIKey: "key1", Model: "gpt-4", ResponseStatusCode: 500}, []objects.APIKeyAutoDisableRule{rule}, autoDisableScopeChannel, defaultPermanentModelFailureThreshold, defaultPermanentModelFailureWindowMinutes)
			require.True(t, acted)
			updated, err := svc.db.Channel.Get(ctx, ch.ID)
			require.NoError(t, err)
			require.Equal(t, channel.StatusEnabled, updated.Status)
			require.Empty(t, updated.DisabledAPIKeys)
			require.Equal(t, ch.Credentials.APIKeys, updated.Credentials.APIKeys)
			require.Len(t, updated.ModelCooldowns, 1)
			require.NotNil(t, updated.ModelCooldowns[0].ExpiresAt)
			require.Contains(t, updated.ModelCooldowns[0].Reason, "1 failures within 10 minutes")
		})
	}
}

func TestChannelService_WindowExpiresOldFailuresAndSurvivesSuccess(t *testing.T) {
	rule := objects.APIKeyAutoDisableRule{Times: 2, WindowMinutes: 1, Action: objects.APIKeyAutoDisableActionPermanent}
	ctx, svc, ch := newWindowRuleTestService(t, rule)
	perf := &PerformanceRecord{ChannelID: ch.ID, APIKey: "key1", Model: "gpt-4", ResponseStatusCode: 500}
	// Seed one expired failure (outside the one-minute window) as a persisted row.
	insertChannelFailureEventForTest(t, svc, ctx, perf, autoDisableScopeChannel, 0, rule, time.Now().Add(-2*time.Minute))
	_, acted := svc.evaluateAutoDisableForFailure(ctx, perf)
	require.False(t, acted, "expired errors cannot contribute")
	svc.clearAutoDisableCountsOnSuccess(perf)
	_, acted = svc.evaluateAutoDisableForFailure(ctx, perf)
	require.True(t, acted, "a success must not erase failures still inside a rolling window")
	updated, err := svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, updated.Status)
	require.Empty(t, updated.DisabledAPIKeys)
	require.Len(t, updated.ModelCooldowns, 1)
}

func TestChannelService_WindowFreezeModelActions(t *testing.T) {
	for _, window := range []int{0, 10} {
		for _, action := range []objects.APIKeyAutoDisableAction{
			objects.APIKeyAutoDisableActionTemporary, objects.APIKeyAutoDisableActionPermanent, objects.APIKeyAutoDisableActionUntilCron,
		} {
			t.Run(fmt.Sprintf("%s/window=%d", action, window), func(t *testing.T) {
				rule := objects.APIKeyAutoDisableRule{Times: 1, WindowMinutes: window, FreezeModel: true, Action: action,
					DisableDurationMinutes: lo.ToPtr(15), DisableUntilCron: "0 0 * * *", DisableUntilTimezone: "UTC"}
				ctx, svc, ch := newWindowRuleTestService(t, rule)
				_, acted := svc.evaluateAutoDisableForFailure(ctx, &PerformanceRecord{ChannelID: ch.ID,
					APIKey: "key1", Model: "gpt-4", ResponseStatusCode: 500})
				require.True(t, acted)
				updated, err := svc.db.Channel.Get(ctx, ch.ID)
				require.NoError(t, err)
				require.Equal(t, channel.StatusEnabled, updated.Status)
				require.Empty(t, updated.DisabledAPIKeys)
				require.Len(t, updated.ModelCooldowns, 1)
				require.NotNil(t, updated.ModelCooldowns[0].ExpiresAt)
			})
		}
	}
}

func TestChannelService_WindowFreezeMissingModelNeverDisablesChannel(t *testing.T) {
	rule := objects.APIKeyAutoDisableRule{Times: 1, WindowMinutes: 10, FreezeModel: true,
		Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: lo.ToPtr(15)}
	ctx, svc, ch := newWindowRuleTestService(t, rule)
	_, acted := svc.evaluateAutoDisableForFailure(ctx, &PerformanceRecord{ChannelID: ch.ID, APIKey: "key1", ResponseStatusCode: 500})
	require.False(t, acted)
	updated, err := svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, updated.Status)
	require.Empty(t, updated.DisabledAPIKeys)
	require.Empty(t, updated.ModelCooldowns)
}

func TestNormalizeAutoDisableWindow(t *testing.T) {
	for _, window := range []int{-1, 10081} {
		rule := objects.APIKeyAutoDisableRule{Times: 1, WindowMinutes: window, Action: objects.APIKeyAutoDisableActionPermanent}
		_, err := normalizeAutoDisableRules([]objects.APIKeyAutoDisableRule{rule}, true)
		require.Error(t, err)
	}
	normalized, err := normalizeAutoDisableRules([]objects.APIKeyAutoDisableRule{{Times: 1, FreezeModel: true,
		Action: objects.APIKeyAutoDisableActionPermanentDelete}}, true)
	require.NoError(t, err)
	require.Equal(t, objects.APIKeyAutoDisableActionTemporary, normalized[0].Action)
	require.Equal(t, 10, normalized[0].WindowMinutes)
	require.True(t, normalized[0].FreezeModel)
}

func TestChannelService_WindowConcurrentFailuresArePreservedAndBounded(t *testing.T) {
	rule := objects.APIKeyAutoDisableRule{Times: 2, WindowMinutes: 10, FreezeModel: true,
		Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: lo.ToPtr(15)}
	ctx, svc, ch := newWindowRuleTestService(t, rule)
	started := make(chan struct{})
	release := make(chan struct{})
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
	perf := &PerformanceRecord{ChannelID: ch.ID, APIKey: "key1", Model: "gpt-4", ResponseStatusCode: 500}
	_, acted := svc.evaluateAutoDisableForFailure(ctx, perf)
	require.False(t, acted)
	done := make(chan bool, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- false
			}
		}()
		_, acted := svc.evaluateAutoDisableForFailure(ctx, perf)
		done <- acted
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("freeze action did not start")
	}
	for range 100 {
		_, acted := svc.evaluateAutoDisableForFailure(ctx, perf)
		require.False(t, acted, "one burst must have only one in-flight action")
	}
	svc.clearAutoDisableCountsOnSuccess(perf)
	close(release)
	released = true
	select {
	case acted := <-done:
		require.True(t, acted)
	case <-time.After(5 * time.Second):
		t.Fatal("freeze action did not finish")
	}
	key := legacyRuleCounterKey(perf, autoDisableScopeChannel, 0, rule)
	pending, _ := countChannelFailureEventsForTest(t, svc, key, channelFailureWindowKeyForTest(perf, rule), time.Now().Add(-time.Duration(effectiveModelFreezeRule(rule).WindowMinutes)*time.Minute))
	svc.apiKeyErrorCountsLock.Lock()
	inFlight := len(svc.windowedRuleActionsInFlight[ch.ID])
	svc.apiKeyErrorCountsLock.Unlock()
	require.GreaterOrEqual(t, pending, defaultPermanentModelFailureThreshold, "every persisted failure stays available for the permanent guard")
	updated, err := svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, updated.Status, "the safeguard freezes the model, not the channel")
	require.Nil(t, updated.AutoDisabledAt)
	require.Len(t, updated.ModelCooldowns, 1)
	require.Nil(t, updated.ModelCooldowns[0].ExpiresAt, "safeguard freeze requires manual recovery")
	require.Zero(t, inFlight)
}

func TestChannelService_WindowActionFailureCanRetry(t *testing.T) {
	rule := objects.APIKeyAutoDisableRule{Times: 2, WindowMinutes: 10, FreezeModel: true,
		Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: lo.ToPtr(15)}
	ctx, svc, ch := newWindowRuleTestService(t, rule)
	failures := 0
	svc.db.Channel.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, mutation ent.Mutation) (ent.Value, error) {
			failures++
			return nil, fmt.Errorf("temporary persistence failure")
		})
	})
	perf := &PerformanceRecord{ChannelID: ch.ID, APIKey: "key1", Model: "gpt-4", ResponseStatusCode: 500}
	for range 3 {
		_, acted := svc.evaluateAutoDisableForFailure(ctx, perf)
		require.False(t, acted)
	}
	require.Equal(t, 2, failures, "a failed action must be retried on the next matching error")
	key := legacyRuleCounterKey(perf, autoDisableScopeChannel, 0, rule)
	pending, _ := countChannelFailureEventsForTest(t, svc, key, channelFailureWindowKeyForTest(perf, rule), time.Now().Add(-time.Duration(effectiveModelFreezeRule(rule).WindowMinutes)*time.Minute))
	require.Equal(t, 3, pending)
	require.Empty(t, svc.windowedRuleActionsInFlight[ch.ID])
}

func TestChannelService_WindowSuccessPreservesBothModelHistories(t *testing.T) {
	rule := objects.APIKeyAutoDisableRule{Times: 2, FreezeModel: true,
		Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: lo.ToPtr(15)}
	ctx, svc, ch := newWindowRuleTestService(t, rule)
	a := &PerformanceRecord{ChannelID: ch.ID, APIKey: "key1", Model: "gpt-4", ResponseStatusCode: 500}
	b := &PerformanceRecord{ChannelID: ch.ID, APIKey: "key2", Model: "gpt-4o", ResponseStatusCode: 500}
	_, acted := svc.evaluateAutoDisableForFailure(ctx, a)
	require.False(t, acted)
	_, acted = svc.evaluateAutoDisableForFailure(ctx, b)
	require.False(t, acted)
	svc.clearAutoDisableCountsOnSuccess(a)
	_, acted = svc.evaluateAutoDisableForFailure(ctx, a)
	require.True(t, acted, "same model history survives success")
	_, acted = svc.evaluateAutoDisableForFailure(ctx, b)
	require.True(t, acted, "other model streak stays independent")
}

func TestChannelService_WindowKeylessSuccessDoesNotClearOtherModelHistory(t *testing.T) {
	rule := objects.APIKeyAutoDisableRule{Times: 2, FreezeModel: true,
		Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: lo.ToPtr(15)}
	ctx, svc, ch := newWindowRuleTestService(t, rule)
	a := &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", ResponseStatusCode: 500}
	b := &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4o", ResponseStatusCode: 500}
	_, acted := svc.evaluateAutoDisableForFailure(ctx, a)
	require.False(t, acted)
	_, acted = svc.evaluateAutoDisableForFailure(ctx, b)
	require.False(t, acted)
	svc.clearAutoDisableCountsOnSuccess(a)
	_, acted = svc.evaluateAutoDisableForFailure(ctx, b)
	require.True(t, acted, "success on another model cannot reset this model's failure streak")
}

func TestChannelService_WindowGlobalRulesAndManualRecovery(t *testing.T) {
	rule := objects.APIKeyAutoDisableRule{Times: 2, WindowMinutes: 10, FreezeModel: true,
		Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: lo.ToPtr(15)}
	ctx, svc, ch := newWindowRuleTestService(t, rule)
	var err error
	ch, err = svc.db.Channel.UpdateOneID(ch.ID).SetPolicies(objects.ChannelPolicies{
		APIKeyAutoDisableMode: objects.APIKeyAutoDisableModeInherit,
	}).Save(ctx)
	require.NoError(t, err)
	withGlobalAutoDisable(t, svc, ctx, ch, []objects.APIKeyAutoDisableRule{rule})
	perf := &PerformanceRecord{ChannelID: ch.ID, APIKey: "key1", Model: "gpt-4", ResponseStatusCode: 500}
	_, acted := svc.evaluateAutoDisableForFailure(ctx, perf)
	require.False(t, acted)
	_, acted = svc.evaluateAutoDisableForFailure(ctx, perf)
	require.True(t, acted)
	updated, err := svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Len(t, updated.ModelCooldowns, 1)
	expiry := *updated.ModelCooldowns[0].ExpiresAt
	require.False(t, buildChannel(updated, nil).HasActiveModelCooldown("gpt-4", expiry.Add(time.Second)))
	updated, err = svc.ClearChannelModelCooldown(ctx, ch.ID, "gpt-4")
	require.NoError(t, err)
	require.Empty(t, updated.ModelCooldowns)
	// A permanent manual freeze survives subsequent automatic failures unchanged.
	updated, err = svc.FreezeChannelModel(ctx, ch.ID, "gpt-4", 0)
	require.NoError(t, err)
	manual := updated.ModelCooldowns[0]
	for range 2 {
		svc.evaluateAutoDisableForFailure(ctx, perf)
	}
	updated, err = svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Len(t, updated.ModelCooldowns, 1)
	require.Equal(t, manual.Reason, updated.ModelCooldowns[0].Reason)
	require.Nil(t, updated.ModelCooldowns[0].ExpiresAt)
}
