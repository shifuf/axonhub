package orchestrator

import (
	"context"
	"slices"

	"github.com/looplj/axonhub/internal/contexts"
)

// excludeCurrentRetryAPIKey rotates a same-channel retry to another enabled key.
// OAuth credentials are not API keys and are intentionally left to their own
// refresh protocol.
func (p *PersistentOutboundTransformer) excludeCurrentRetryAPIKey(ctx context.Context) error {
	if p == nil || p.state == nil || p.state.CurrentCandidate == nil || p.state.CurrentCandidate.Channel == nil {
		return nil
	}

	candidate := p.state.CurrentCandidate
	allKeys := candidate.Channel.Credentials.GetAllAPIKeys()
	if len(allKeys) < 2 {
		return nil
	}

	currentKey := currentRetryCredentialRef(ctx, p)
	if currentKey == "" || !slices.Contains(allKeys, currentKey) {
		return nil
	}

	enabled := candidate.Channel.Credentials.GetEnabledAPIKeys(candidate.Channel.DisabledAPIKeys)
	excluded := contexts.GetChannelAPIKeyRetryExclusions(ctx, candidate.Channel.ID)

	hasAlternative := false
	for _, key := range enabled {
		if key == currentKey {
			continue
		}
		if _, alreadyTried := excluded[key]; alreadyTried {
			continue
		}

		hasAlternative = true
		break
	}

	if !hasAlternative {
		if len(enabled) > 1 {
			return errChannelUnavailableForRetry
		}

		// A single-key channel may still recover from a transient transport
		// failure; there is simply no key to rotate to.
		return nil
	}

	contexts.ExcludeChannelAPIKeyForRetry(ctx, candidate.Channel.ID, currentKey)

	return nil
}
