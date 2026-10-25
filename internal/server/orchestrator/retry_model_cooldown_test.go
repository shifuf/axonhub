package orchestrator

import (
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
)

func TestRefreshChannelBeforeRetry_ModelCooldown(t *testing.T) {
	for _, tt := range []struct {
		name           string
		cooldowns      []objects.ModelCooldown
		credentialless bool
		wantErr        bool
	}{
		{
			name:      "newly frozen original alias is unavailable",
			cooldowns: []objects.ModelCooldown{{Model: "public-alias"}}, wantErr: true,
		},
		{
			name:      "expired original alias freeze does not prevent retry",
			cooldowns: []objects.ModelCooldown{{Model: "public-alias", ExpiresAt: lo.ToPtr(time.Now().Add(-time.Hour))}},
		},
		{
			name:      "upstream model is not the selection model",
			cooldowns: []objects.ModelCooldown{{Model: "test-model"}},
		},
		{
			name: "credentialless channel still observes a model freeze", credentialless: true,
			cooldowns: []objects.ModelCooldown{{Model: "public-alias"}}, wantErr: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, client := setupTest(t)
			credentials := objects.ChannelCredentials{APIKey: "test-key"}
			channelType := channel.TypeOpenai
			if tt.credentialless {
				credentials = objects.ChannelCredentials{}
				channelType = channel.TypeOllama
			}
			ch, err := client.Channel.Create().
				SetType(channelType).
				SetName("retry-model-cooldown").
				SetBaseURL("https://retry.invalid").
				SetCredentials(credentials).
				SetSupportedModels([]string{"test-model"}).
				SetDefaultTestModel("test-model").
				SetStatus(channel.StatusEnabled).
				Save(ctx)
			require.NoError(t, err)
			service := newTestChannelServiceForChannels(client)
			t.Cleanup(service.Stop)
			snapshot := retryGuardSnapshot(t, ctx, service, ch.ID)
			outbound := newRetryGuardTransformer(service, snapshot, credentials.APIKey)
			outbound.state.OriginalModel = "public-alias"
			outbound.state.LlmRequest = &llm.Request{Model: "test-model"}

			// Candidates are already pinned when the new freeze is committed.
			_, err = client.Channel.UpdateOneID(ch.ID).SetModelCooldowns(tt.cooldowns).Save(ctx)
			require.NoError(t, err)
			require.Empty(t, snapshot.ModelCooldowns)
			err = outbound.refreshChannelBeforeRetry(ctx)
			if tt.wantErr {
				require.ErrorIs(t, err, errChannelUnavailableForRetry)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, "public-alias", outbound.state.OriginalModel)
			require.Same(t, snapshot, outbound.GetCurrentChannel())
		})
	}
}
