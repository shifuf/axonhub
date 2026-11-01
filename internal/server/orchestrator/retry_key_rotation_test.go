package orchestrator

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
)

func TestPersistentOutboundTransformer_ExcludeCurrentRetryAPIKey(t *testing.T) {
	t.Run("rotates to another enabled key", func(t *testing.T) {
		ctx := contexts.EnsureContainer(t.Context())
		ctx = contexts.WithChannelAPIKey(ctx, "key-1")

		channel := &biz.Channel{Channel: &ent.Channel{
			ID:          7,
			Credentials: objects.ChannelCredentials{APIKeys: []string{"key-1", "key-2"}},
		}}
		outbound := &PersistentOutboundTransformer{state: &PersistenceState{
			CurrentCandidate: &ChannelModelsCandidate{Channel: channel},
		}}

		require.NoError(t, outbound.excludeCurrentRetryAPIKey(ctx))
		require.Contains(t, contexts.GetChannelAPIKeyRetryExclusions(ctx, 7), "key-1")
	})

	t.Run("does not rotate OAuth credentials", func(t *testing.T) {
		ctx := contexts.EnsureContainer(t.Context())
		ctx = contexts.WithChannelAPIKey(ctx, "oauth-ref")

		channel := &biz.Channel{Channel: &ent.Channel{
			ID:          8,
			Credentials: objects.ChannelCredentials{OAuth: &objects.OAuthCredentials{AccessToken: "oauth-token"}},
		}}
		outbound := &PersistentOutboundTransformer{state: &PersistenceState{
			CurrentCandidate: &ChannelModelsCandidate{Channel: channel},
		}}

		require.NoError(t, outbound.excludeCurrentRetryAPIKey(ctx))
		require.Empty(t, contexts.GetChannelAPIKeyRetryExclusions(ctx, 8))
	})

	t.Run("stops after every enabled key failed", func(t *testing.T) {
		ctx := contexts.EnsureContainer(t.Context())
		ctx = contexts.WithChannelAPIKey(ctx, "key-2")
		contexts.ExcludeChannelAPIKeyForRetry(ctx, 9, "key-1")

		channel := &biz.Channel{Channel: &ent.Channel{
			ID:          9,
			Credentials: objects.ChannelCredentials{APIKeys: []string{"key-1", "key-2"}},
		}}
		outbound := &PersistentOutboundTransformer{state: &PersistenceState{
			CurrentCandidate: &ChannelModelsCandidate{Channel: channel},
		}}

		require.ErrorIs(t, outbound.excludeCurrentRetryAPIKey(ctx), errChannelUnavailableForRetry)
	})
}

func TestPersistentOutboundTransformer_CanFailover(t *testing.T) {
	channel := &biz.Channel{Channel: &ent.Channel{
		Credentials: objects.ChannelCredentials{APIKeys: []string{"key-1"}},
	}}
	outbound := &PersistentOutboundTransformer{state: &PersistenceState{
		CurrentCandidate: &ChannelModelsCandidate{Channel: channel},
	}}

	require.False(t, outbound.CanFailover(&httpclient.Error{StatusCode: http.StatusBadRequest}))
	require.True(t, outbound.CanFailover(&httpclient.Error{StatusCode: http.StatusUnauthorized}))
	require.True(t, outbound.CanFailover(&httpclient.Error{StatusCode: http.StatusRequestTimeout}))
	require.True(t, outbound.CanFailover(&httpclient.Error{StatusCode: http.StatusTooManyRequests}))
	require.True(t, outbound.CanFailover(&httpclient.Error{StatusCode: http.StatusServiceUnavailable}))
	require.True(t, outbound.CanFailover(pipeline.ErrStreamFirstEventTimeout))
	require.True(t, outbound.CanFailover(pipeline.ErrNonStreamResponseTimeout))
	require.False(t, outbound.CanFailover(context.Canceled))
}

func TestPersistentOutboundTransformer_CredentialFailuresPreferSameChannelKeyRotation(t *testing.T) {
	channel := &biz.Channel{Channel: &ent.Channel{
		ID:          11,
		Credentials: objects.ChannelCredentials{APIKeys: []string{"key-1", "key-2"}},
	}}
	outbound := &PersistentOutboundTransformer{state: &PersistenceState{
		CurrentCandidate: &ChannelModelsCandidate{
			Channel: channel,
			Models: []biz.ChannelModelEntry{
				{RequestModel: "gpt-4", ActualModel: "gpt-4"},
				{RequestModel: "gpt-4", ActualModel: "gpt-4-fallback"},
			},
		},
	}}

	require.True(t, outbound.CanRetry(&httpclient.Error{StatusCode: http.StatusUnauthorized}))
	require.True(t, outbound.preferAPIKeyRetry)

	outbound.preferAPIKeyRetry = false
	require.True(t, outbound.CanRetry(&httpclient.Error{StatusCode: http.StatusTooManyRequests}))
	require.True(t, outbound.preferAPIKeyRetry)
}

func TestPersistentOutboundTransformer_SingleKeyCredentialFailureFailsOver(t *testing.T) {
	channel := &biz.Channel{Channel: &ent.Channel{
		ID:          12,
		Credentials: objects.ChannelCredentials{APIKeys: []string{"key-1"}},
	}}
	outbound := &PersistentOutboundTransformer{state: &PersistenceState{
		CurrentCandidate: &ChannelModelsCandidate{
			Channel: channel,
			Models: []biz.ChannelModelEntry{
				{RequestModel: "gpt-4", ActualModel: "gpt-4"},
				{RequestModel: "gpt-4", ActualModel: "gpt-4-fallback"},
			},
		},
	}}

	require.False(t, outbound.CanRetry(&httpclient.Error{StatusCode: http.StatusUnauthorized}))
	require.False(t, outbound.CanRetry(&httpclient.Error{StatusCode: http.StatusTooManyRequests}))
	require.False(t, outbound.preferAPIKeyRetry)
}
