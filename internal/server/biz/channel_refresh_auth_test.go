package biz

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChannelRefreshAuth(t *testing.T) {
	t.Run("static credentials are not refreshable", func(t *testing.T) {
		channel := &Channel{}

		refreshed, err := channel.RefreshAuth(t.Context())
		require.NoError(t, err)
		require.False(t, refreshed)
	})

	t.Run("oauth force refresh callback", func(t *testing.T) {
		calls := 0
		channel := &Channel{
			refreshToken: func(ctx context.Context) error {
				calls++
				return nil
			},
		}

		refreshed, err := channel.RefreshAuth(t.Context())
		require.NoError(t, err)
		require.True(t, refreshed)
		require.Equal(t, 1, calls)
	})

	t.Run("refresh error is propagated", func(t *testing.T) {
		wantErr := errors.New("token endpoint unavailable")
		channel := &Channel{
			refreshToken: func(ctx context.Context) error {
				return wantErr
			},
		}

		refreshed, err := channel.RefreshAuth(t.Context())
		require.ErrorIs(t, err, wantErr)
		require.True(t, refreshed)
	})
}
