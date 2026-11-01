package contexts

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChannelAPIKeyRetryExclusions(t *testing.T) {
	ctx := EnsureContainer(t.Context())

	ExcludeChannelAPIKeyForRetry(ctx, 10, "key-1")
	ExcludeChannelAPIKeyForRetry(ctx, 10, "key-2")
	ExcludeChannelAPIKeyForRetry(ctx, 20, "other-key")

	require.Equal(t, map[string]struct{}{
		"key-1": {},
		"key-2": {},
	}, GetChannelAPIKeyRetryExclusions(ctx, 10))
	require.Equal(t, map[string]struct{}{
		"other-key": {},
	}, GetChannelAPIKeyRetryExclusions(ctx, 20))

	ClearChannelAPIKeyRetryExclusions(ctx)
	require.Empty(t, GetChannelAPIKeyRetryExclusions(ctx, 10))
	require.Empty(t, GetChannelAPIKeyRetryExclusions(ctx, 20))
}

func TestClearChannelAPIKey(t *testing.T) {
	ctx := EnsureContainer(t.Context())
	ctx = WithChannelAPIKey(ctx, "key-1")

	key, ok := GetChannelAPIKey(ctx)
	require.True(t, ok)
	require.Equal(t, "key-1", key)

	ClearChannelAPIKey(ctx)
	_, ok = GetChannelAPIKey(ctx)
	require.False(t, ok)
}
