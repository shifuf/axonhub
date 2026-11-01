package copilot

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm/oauth"
)

func TestCopilotTokenProviderExposesForceRefresh(t *testing.T) {
	var provider any = &CopilotTokenProvider{}

	refresher, ok := provider.(interface {
		ForceRefresh(context.Context) (*oauth.OAuthCredentials, error)
	})
	require.True(t, ok)
	require.NotNil(t, refresher)
}
