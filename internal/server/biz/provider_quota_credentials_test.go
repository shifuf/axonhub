package biz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/providerquotastatus"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz/provider_quota"
)

func TestChannelWithQuotaCredential(t *testing.T) {
	channel := &ent.Channel{
		Credentials: objects.ChannelCredentials{
			APIKey:           "inference-key",
			APIKeys:          []string{"inference-key-2"},
			QuotaAPIKey:      " quota-key ",
			ManagementAPIKey: "management-key",
		},
	}

	quotaChannel := channelWithQuotaCredential(channel)

	require.Equal(t, "quota-key", quotaChannel.Credentials.APIKey)
	require.Empty(t, quotaChannel.Credentials.APIKeys)
	require.Empty(t, quotaChannel.Credentials.ManagementAPIKey)
	require.Equal(t, "inference-key", channel.Credentials.APIKey)
	require.Equal(t, []string{"inference-key-2"}, channel.Credentials.APIKeys)
}

func TestChannelWithQuotaCredential_EmptyCredentialSkips(t *testing.T) {
	channel := &ent.Channel{Credentials: objects.ChannelCredentials{APIKey: "inference-key"}}

	require.Nil(t, channelWithQuotaCredential(channel))
}

func TestHasCredentialsForProvider_RequiresDedicatedQuotaToken(t *testing.T) {
	for _, channelType := range []channel.Type{channel.TypeNanogpt, channel.TypeMinimax} {
		t.Run(string(channelType), func(t *testing.T) {
			ch := &ent.Channel{
				Type:        channelType,
				Credentials: objects.ChannelCredentials{APIKey: "inference-key"},
			}
			require.False(t, hasCredentialsForProvider(ch))
			ch.Credentials.QuotaAPIKey = "quota-key"
			require.True(t, hasCredentialsForProvider(ch))
		})
	}
	require.False(t, hasCredentialsForProvider(&ent.Channel{
		Type:        channel.TypeOllama,
		Credentials: objects.ChannelCredentials{APIKey: "inference-key"},
	}))
}

func TestQuotaTokenProviderTypes(t *testing.T) {
	require.True(t, supportsQuotaAPIKey(channel.TypeNanogpt))
	require.True(t, supportsQuotaAPIKey(channel.TypeZhipu))
	require.False(t, supportsQuotaAPIKey(channel.TypeCodex))
	require.False(t, supportsQuotaAPIKey(channel.TypeZenmux))
	require.False(t, supportsQuotaAPIKey(channel.TypeOllama))
}

func TestProviderQuotaService_CheckChannelQuota_EmptyQuotaTokenDoesNotCallCheckerOrInvalidateStatus(t *testing.T) {
	service, _, ctx, client := setupProviderQuotaCollectionService(t)
	defer client.Close()
	checker := &credentialCaptureQuotaChecker{}
	service.checkers["nanogpt"] = checker
	channelEntity := createProviderQuotaCollectionChannel(t, ctx, client, "NanoGPT", channel.TypeNanogpt)
	service.quotaCache.Store(channelEntity.ID, &QuotaChannelStatus{ProviderType: "nanogpt", Ready: true})

	service.checkChannelQuota(ctx, quotaCheckGroup{channels: []*ent.Channel{channelEntity}}, time.Now())

	require.Equal(t, 0, checker.calls)
	_, ok := service.quotaCache.Load(channelEntity.ID)
	require.True(t, ok)
}

type credentialCaptureQuotaChecker struct {
	calls       int
	quotaAPIKey string
}

func (c *credentialCaptureQuotaChecker) CheckQuota(_ context.Context, ch *ent.Channel) (provider_quota.QuotaData, error) {
	c.calls++
	c.quotaAPIKey = ch.Credentials.APIKey
	return provider_quota.QuotaData{Status: "available", Ready: true}, nil
}

func (c *credentialCaptureQuotaChecker) SupportsChannel(*ent.Channel) bool { return true }

func TestProviderQuotaService_CheckChannelQuota_UsesDedicatedQuotaToken(t *testing.T) {
	service, _, ctx, client := setupProviderQuotaCollectionService(t)
	defer client.Close()
	channelEntity, err := client.Channel.Create().
		SetName("NanoGPT").
		SetType(channel.TypeNanogpt).
		SetStatus(channel.StatusEnabled).
		SetCredentials(objects.ChannelCredentials{APIKey: "inference-key", QuotaAPIKey: "quota-key"}).
		SetSupportedModels([]string{"test-model"}).
		SetDefaultTestModel("test-model").
		Save(ctx)
	require.NoError(t, err)
	checker := &credentialCaptureQuotaChecker{}
	service.checkers["nanogpt"] = checker

	service.checkChannelQuota(ctx, quotaCheckGroup{channels: []*ent.Channel{channelEntity}}, time.Now())

	require.Equal(t, 1, checker.calls)
	require.Equal(t, "quota-key", checker.quotaAPIKey)
}

func TestProviderQuotaService_TestQuotaUsesUnsavedToken(t *testing.T) {
	service, _, _, client := setupProviderQuotaCollectionService(t)
	defer client.Close()
	checker := &credentialCaptureQuotaChecker{}
	service.checkers["nanogpt"] = checker

	quota, err := service.TestQuota(t.Context(), channel.TypeNanogpt, "https://api.nanogpt.com", "temporary-token")

	require.NoError(t, err)
	require.Equal(t, "available", quota.Status)
	require.Equal(t, "temporary-token", checker.quotaAPIKey)
	count, countErr := client.Channel.Query().Count(authz.WithTestBypass(ent.NewContext(t.Context(), client)))
	require.NoError(t, countErr)
	require.Zero(t, count)
}

func TestProviderQuotaService_TestQuotaUsesCompatibleCheckerForUnknownOpenAIURL(t *testing.T) {
	service, _, _, client := setupProviderQuotaCollectionService(t)
	defer client.Close()
	checker := &credentialCaptureQuotaChecker{}
	service.checkers["compatible"] = checker

	quota, err := service.TestQuota(t.Context(), channel.TypeOpenai, "https://unrecognized.example/v1", "temporary-token")

	require.NoError(t, err)
	require.Equal(t, "available", quota.Status)
	require.Equal(t, 1, checker.calls)
	require.Equal(t, "temporary-token", checker.quotaAPIKey)
}

func TestProviderQuotaService_TestQuotaRedactsTokenFromCheckerErrors(t *testing.T) {
	service, _, _, client := setupProviderQuotaCollectionService(t)
	defer client.Close()
	service.checkers["nanogpt"] = quotaErrorChecker{err: errors.New("upstream rejected token temporary-secret")}

	_, err := service.TestQuota(t.Context(), channel.TypeNanogpt, "https://api.nanogpt.com", " temporary-secret ")

	require.Error(t, err)
	require.NotContains(t, err.Error(), "temporary-secret")
}

func TestProviderQuotaService_CheckChannelQuota_IgnoreModeSkipsCheckerAndClearsStatus(t *testing.T) {
	service, _, ctx, client := setupProviderQuotaCollectionService(t)
	defer client.Close()
	checker := &credentialCaptureQuotaChecker{}
	service.checkers["minimax"] = checker
	channelEntity, err := client.Channel.Create().
		SetName("Ignored MiniMax").
		SetType(channel.TypeMinimax).
		SetStatus(channel.StatusEnabled).
		SetCredentials(objects.ChannelCredentials{QuotaAPIKey: "quota-key"}).
		SetSettings(&objects.ChannelSettings{QuotaRoutingMode: objects.QuotaRoutingModeIgnoreQuota}).
		SetSupportedModels([]string{"test-model"}).
		SetDefaultTestModel("test-model").
		Save(ctx)
	require.NoError(t, err)
	_, err = client.ProviderQuotaStatus.Create().
		SetChannelID(channelEntity.ID).
		SetProviderType(providerquotastatus.ProviderTypeMinimax).
		SetStatus(providerquotastatus.StatusAvailable).
		SetReady(true).
		SetQuotaData(map[string]any{"old": "quota"}).
		SetNextCheckAt(time.Now().Add(-time.Minute)).
		Save(ctx)
	require.NoError(t, err)

	service.checkChannelQuota(ctx, quotaCheckGroup{channels: []*ent.Channel{channelEntity}}, time.Now())

	require.Zero(t, checker.calls)
	_, err = client.ProviderQuotaStatus.Query().
		Where(providerquotastatus.ChannelIDEQ(channelEntity.ID)).
		Only(ctx)
	require.True(t, ent.IsNotFound(err))
}

func TestProviderQuotaService_CheckChannelQuota_MixedGroupDoesNotSaveIgnoredMember(t *testing.T) {
	service, _, ctx, client := setupProviderQuotaCollectionService(t)
	defer client.Close()
	checker := &credentialCaptureQuotaChecker{}
	service.checkers["minimax"] = checker
	ignored, err := client.Channel.Create().
		SetName("Ignored MiniMax").
		SetType(channel.TypeMinimax).
		SetStatus(channel.StatusEnabled).
		SetCredentials(objects.ChannelCredentials{QuotaAPIKey: "ignored-key"}).
		SetSettings(&objects.ChannelSettings{QuotaRoutingMode: objects.QuotaRoutingModeIgnoreQuota}).
		SetSupportedModels([]string{"test-model"}).
		SetDefaultTestModel("test-model").
		Save(ctx)
	require.NoError(t, err)
	active, err := client.Channel.Create().
		SetName("Active MiniMax").
		SetType(channel.TypeMinimax).
		SetStatus(channel.StatusEnabled).
		SetCredentials(objects.ChannelCredentials{QuotaAPIKey: "active-key"}).
		SetSettings(&objects.ChannelSettings{QuotaRoutingMode: objects.QuotaRoutingModeBackpressure}).
		SetSupportedModels([]string{"test-model"}).
		SetDefaultTestModel("test-model").
		Save(ctx)
	require.NoError(t, err)

	service.checkChannelQuota(ctx, quotaCheckGroup{
		channels:   []*ent.Channel{ignored, active},
		accountKey: "shared-account",
	}, time.Now())

	require.Equal(t, 1, checker.calls)
	require.Equal(t, "active-key", checker.quotaAPIKey)
	_, err = client.ProviderQuotaStatus.Query().
		Where(providerquotastatus.ChannelIDEQ(ignored.ID)).
		Only(ctx)
	require.True(t, ent.IsNotFound(err))
	status, err := client.ProviderQuotaStatus.Query().
		Where(providerquotastatus.ChannelIDEQ(active.ID)).
		Only(ctx)
	require.NoError(t, err)
	require.Equal(t, providerquotastatus.StatusAvailable, status.Status)
}

type quotaErrorChecker struct{ err error }

func (c quotaErrorChecker) CheckQuota(context.Context, *ent.Channel) (provider_quota.QuotaData, error) {
	return provider_quota.QuotaData{}, c.err
}

func (quotaErrorChecker) SupportsChannel(*ent.Channel) bool { return true }
