package biz

import (
	"context"
	"fmt"
	"github.com/samber/lo"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/channelfailureevent"
	"github.com/looplj/axonhub/internal/objects"
)

// insertChannelFailureEventForTest persists one failure row exactly the way
// evaluateWindowedRule does, so tests can pre-fill a rolling window.
func insertChannelFailureEventForTest(
	t *testing.T,
	svc *ChannelService,
	ctx context.Context,
	perf *PerformanceRecord,
	scope autoDisableScope,
	ruleIndex int,
	rule objects.APIKeyAutoDisableRule,
	failedAt time.Time,
) string {
	t.Helper()
	effective := effectiveModelFreezeRule(rule)
	ruleKey := apiKeyRuleCounterKey(autoDisableRuleIdentity(perf, effective), scope, ruleIndex, effective)
	windowKey := fmt.Sprintf("%s:window:%d", autoDisableRuleIdentity(perf, effective), effective.WindowMinutes)
	err := svc.db.ChannelFailureEvent.Create().
		SetChannelID(perf.ChannelID).
		SetModel(perf.Model).
		SetRuleKey(ruleKey).
		SetRuleKeyHash(channelFailureEventHash(ruleKey)).
		SetWindowKey(windowKey).
		SetWindowKeyHash(channelFailureEventHash(windowKey)).
		SetStatusCode(perf.ResponseStatusCode).
		SetErrorDetail(autoDisableErrorDetail(perf)).
		SetFailedAt(failedAt).
		Exec(ctx)
	require.NoError(t, err)
	return ruleKey
}

// countChannelFailureEventsForTest mirrors the persisted counting query.
func countChannelFailureEventsForTest(t *testing.T, svc *ChannelService, ruleKey, windowKey string, cutoff time.Time) (int, int) {
	t.Helper()
	ruleCount, err := svc.db.ChannelFailureEvent.Query().
		Where(channelfailureevent.RuleKeyHash(channelFailureEventHash(ruleKey)), channelfailureevent.FailedAtGT(cutoff)).
		Count(context.Background())
	require.NoError(t, err)
	windowTotal, err := svc.db.ChannelFailureEvent.Query().
		Where(channelfailureevent.WindowKeyHash(channelFailureEventHash(windowKey)), channelfailureevent.FailedAtGT(cutoff)).
		Count(context.Background())
	require.NoError(t, err)
	return ruleCount, windowTotal
}

func countChannelFailureEventsByChannelForTest(t *testing.T, svc *ChannelService, channelID int) int {
	t.Helper()
	count, err := svc.db.ChannelFailureEvent.Query().
		Where(channelfailureevent.ChannelID(channelID)).
		Count(context.Background())
	require.NoError(t, err)
	return count
}

// channelFailureWindowKeyForTest rebuilds the shared window key for a rule.
func channelFailureWindowKeyForTest(perf *PerformanceRecord, rule objects.APIKeyAutoDisableRule) string {
	effective := effectiveModelFreezeRule(rule)
	return fmt.Sprintf("%s:window:%d", autoDisableRuleIdentity(perf, effective), effective.WindowMinutes)
}

// TestChannelService_WindowCountersMergeAcrossInstances proves the persisted
// failure window is shared: a second service instance (another process or a
// restarted server) sees the first instance's failures and triggers the rule.
func TestChannelService_WindowCountersMergeAcrossInstances(t *testing.T) {
	rule := objects.APIKeyAutoDisableRule{Times: 2, WindowMinutes: 10, FreezeModel: true,
		Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: lo.ToPtr(15)}
	ctx, svcA, ch := newWindowRuleTestService(t, rule)
	perf := &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 500}
	_, acted := svcA.evaluateAutoDisableForFailure(ctx, perf)
	require.False(t, acted)

	// A fresh service on the same database simulates either another server
	// instance or a restart of the current one.
	svcB := newTestChannelService(svcA.db)
	t.Cleanup(svcB.Stop)
	svcB.SetEnabledChannelsForTest([]*Channel{buildChannel(ch, nil)})
	_, acted = svcB.evaluateAutoDisableForFailure(ctx, perf)
	require.True(t, acted, "the second instance must count the first instance's failure")

	updated, err := svcB.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Len(t, updated.ModelCooldowns, 1)
	require.Equal(t, "gpt-4", updated.ModelCooldowns[0].Model)
	require.Contains(t, updated.ModelCooldowns[0].Reason, "2 failures within 10 minutes")
}

// TestChannelService_PurgeExpiredChannelFailureEvents keeps the event table
// bounded: rows older than the longest possible window are deleted, recent
// rows survive.
func TestChannelService_PurgeExpiredChannelFailureEvents(t *testing.T) {
	rule := objects.APIKeyAutoDisableRule{Times: 2, WindowMinutes: 10, FreezeModel: true,
		Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: lo.ToPtr(15)}
	ctx, svc, ch := newWindowRuleTestService(t, rule)
	perf := &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 500}
	insertChannelFailureEventForTest(t, svc, ctx, perf, autoDisableScopeChannel, 0, rule, time.Now().Add(-10*time.Duration(channelFailureEventRetention)*time.Minute))
	insertChannelFailureEventForTest(t, svc, ctx, perf, autoDisableScopeChannel, 0, rule, time.Now())

	svc.purgeExpiredChannelFailureEvents(ctx, time.Now())

	require.Equal(t, 1, countChannelFailureEventsByChannelForTest(t, svc, ch.ID))
}

// TestChannelService_CustomPermanentModelFreezeThreshold proves the safeguard
// threshold is configurable: with the retry policy set to 3, the third failure
// in the window permanently freezes the model without disabling the channel.
func TestChannelService_CustomPermanentModelFreezeThreshold(t *testing.T) {
	rule := objects.APIKeyAutoDisableRule{Times: 2, WindowMinutes: 10, FreezeModel: true,
		Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: lo.ToPtr(15)}
	ctx, svc, ch := newWindowRuleTestService(t, rule)
	require.NoError(t, svc.SystemService.SetRetryPolicy(ctx, &RetryPolicy{
		AutoDisableChannel: AutoDisableChannel{PermanentFailureThreshold: 3},
	}))
	perf := &PerformanceRecord{ChannelID: ch.ID, Model: "gpt-4", APIKey: "key1", ResponseStatusCode: 500}

	_, acted := svc.evaluateAutoDisableForFailure(ctx, perf)
	require.False(t, acted)
	_, acted = svc.evaluateAutoDisableForFailure(ctx, perf)
	require.True(t, acted, "the rule threshold still triggers the temporary freeze")
	stored, err := svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.ModelCooldowns[0].ExpiresAt, "the rule action stays temporary")

	_, acted = svc.evaluateAutoDisableForFailure(ctx, perf)
	require.True(t, acted, "the third failure crosses the custom safeguard threshold")
	stored, err = svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, stored.Status, "the safeguard freezes the model, not the channel")
	require.Len(t, stored.ModelCooldowns, 1)
	require.Nil(t, stored.ModelCooldowns[0].ExpiresAt, "the safeguard upgrade is permanent")
	require.Contains(t, stored.ModelCooldowns[0].Reason, "threshold 3")
}
