package orchestrator

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestMaybeEvaluateChannelAPIKeyRulesOnFailure_OriginalModel(t *testing.T) {
	for _, keyFromPerf := range []bool{false, true} {
		name := "context credential before performance recording"
		if keyFromPerf {
			name = "incomplete performance credential"
		}
		t.Run(name, func(t *testing.T) {
			ctx, client := setupTest(t)
			ch := createRetryGuardChannel(t, ctx, client, "failure-rules", objects.ChannelCredentials{APIKey: "test-key"})
			_, err := client.Channel.UpdateOne(ch).SetPolicies(objects.ChannelPolicies{
				APIKeyAutoDisableMode: objects.APIKeyAutoDisableModeCustom,
				APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{{
					Times: 1, WindowMinutes: 10, FreezeModel: true,
					Action: objects.APIKeyAutoDisableActionPermanent,
				}},
			}).Save(ctx)
			require.NoError(t, err)
			service := newTestChannelServiceForChannels(client)
			t.Cleanup(service.Stop)
			outbound := newRetryGuardTransformer(service, retryGuardSnapshot(t, ctx, service, ch.ID), "")
			outbound.state.OriginalModel = "public-alias"
			outbound.state.LlmRequest = &llm.Request{Model: "test-model"}
			if keyFromPerf {
				outbound.state.Perf = &biz.PerformanceRecord{APIKey: "test-key", Model: "test-model"}
			} else {
				ctx = contexts.WithChannelAPIKey(ctx, "test-key")
			}

			maybeEvaluateChannelAPIKeyRulesOnFailure(ctx, outbound, &httpclient.Error{StatusCode: http.StatusServiceUnavailable})

			updated, err := client.Channel.Get(ctx, ch.ID)
			require.NoError(t, err)
			require.Equal(t, channel.StatusEnabled, updated.Status)
			require.Empty(t, updated.DisabledAPIKeys, "a model freeze must not disable the credential")
			require.Len(t, updated.ModelCooldowns, 1)
			require.Equal(t, "public-alias", updated.ModelCooldowns[0].Model, "freeze the selection model, not the upstream model")
		})
	}
}

func TestMaybeEvaluateChannelAPIKeyRulesOnFailure_CompletedPerformanceIsNotCountedAgain(t *testing.T) {
	ctx, client := setupTest(t)
	ch := createRetryGuardChannel(t, ctx, client, "completed-failure-rules", objects.ChannelCredentials{APIKey: "test-key"})
	_, err := client.Channel.UpdateOne(ch).SetPolicies(objects.ChannelPolicies{
		APIKeyAutoDisableMode: objects.APIKeyAutoDisableModeCustom,
		APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{{
			Times: 2, WindowMinutes: 10, FreezeModel: true,
			Action: objects.APIKeyAutoDisableActionPermanent,
		}},
	}).Save(ctx)
	require.NoError(t, err)
	service := newTestChannelServiceForChannels(client)
	t.Cleanup(service.Stop)
	outbound := newRetryGuardTransformer(service, retryGuardSnapshot(t, ctx, service, ch.ID), "test-key")
	outbound.state.OriginalModel = "public-alias"
	outbound.state.Perf.RequestCompleted = true
	failure := &httpclient.Error{StatusCode: http.StatusServiceUnavailable}

	maybeEvaluateChannelAPIKeyRulesOnFailure(ctx, outbound, failure)
	outbound.state.Perf.RequestCompleted = false
	maybeEvaluateChannelAPIKeyRulesOnFailure(ctx, outbound, failure)
	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Empty(t, updated.ModelCooldowns, "completed performance must not consume another failure in the window")

	maybeEvaluateChannelAPIKeyRulesOnFailure(ctx, outbound, failure)
	updated, err = client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Len(t, updated.ModelCooldowns, 1, "two unrecorded failures should still reach the threshold")
	require.Equal(t, "public-alias", updated.ModelCooldowns[0].Model)
}

func TestMaybeEvaluateChannelAPIKeyRulesOnFailure_KeylessModelPolicy(t *testing.T) {
	ctx, client := setupTest(t)
	ch, err := client.Channel.Create().SetType(channel.TypeOllama).SetName("keyless-model-policy").
		SetBaseURL("http://retry.invalid").SetCredentials(objects.ChannelCredentials{}).
		SetSupportedModels([]string{"test-model"}).SetDefaultTestModel("test-model").SetStatus(channel.StatusEnabled).
		SetPolicies(objects.ChannelPolicies{APIKeyAutoDisableMode: objects.APIKeyAutoDisableModeCustom,
			APIKeyAutoDisableRules: []objects.APIKeyAutoDisableRule{{Times: 1, Action: objects.APIKeyAutoDisableActionPermanentDelete}}}).Save(ctx)
	require.NoError(t, err)
	service := newTestChannelServiceForChannels(client)
	t.Cleanup(service.Stop)
	outbound := newRetryGuardTransformer(service, retryGuardSnapshot(t, ctx, service, ch.ID), "")
	outbound.state.OriginalModel = "public-alias"
	outbound.state.Perf = nil
	maybeEvaluateChannelAPIKeyRulesOnFailure(ctx, outbound, &httpclient.Error{StatusCode: http.StatusServiceUnavailable})
	stored, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, stored.Status)
	require.Empty(t, stored.DisabledAPIKeys)
	require.Len(t, stored.ModelCooldowns, 1)
	require.Equal(t, "public-alias", stored.ModelCooldowns[0].Model)
	require.NotNil(t, stored.ModelCooldowns[0].ExpiresAt)
}
