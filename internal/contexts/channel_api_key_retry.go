package contexts

import "context"

// ExcludeChannelAPIKeyForRetry prevents a failed key from being selected again
// during same-channel retries for this logical request.
func ExcludeChannelAPIKeyForRetry(ctx context.Context, channelID int, apiKey string) {
	if channelID <= 0 || apiKey == "" {
		return
	}

	container := getContainer(ctx)
	container.mu.Lock()
	defer container.mu.Unlock()

	if container.ChannelAPIKeyRetryExclusions == nil {
		container.ChannelAPIKeyRetryExclusions = make(map[int]map[string]struct{})
	}

	excluded := container.ChannelAPIKeyRetryExclusions[channelID]
	if excluded == nil {
		excluded = make(map[string]struct{})
		container.ChannelAPIKeyRetryExclusions[channelID] = excluded
	}

	excluded[apiKey] = struct{}{}
}

// GetChannelAPIKeyRetryExclusions returns a copy of the keys excluded for the
// channel during the current logical request.
func GetChannelAPIKeyRetryExclusions(ctx context.Context, channelID int) map[string]struct{} {
	container := getContainer(ctx)
	container.mu.RLock()
	defer container.mu.RUnlock()

	excluded := container.ChannelAPIKeyRetryExclusions[channelID]
	if len(excluded) == 0 {
		return nil
	}

	result := make(map[string]struct{}, len(excluded))
	for key := range excluded {
		result[key] = struct{}{}
	}

	return result
}

// ClearChannelAPIKeyRetryExclusions resets key exclusions when retry routing
// moves to another channel or another model.
func ClearChannelAPIKeyRetryExclusions(ctx context.Context) {
	container := getContainer(ctx)
	container.mu.Lock()
	defer container.mu.Unlock()

	clear(container.ChannelAPIKeyRetryExclusions)
}
