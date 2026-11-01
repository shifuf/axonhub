package orchestrator

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

func TestRateLimitTracking_OnOutboundLlmResponse(t *testing.T) {
	tracker := NewChannelRequestTracker()

	entChannel := &ent.Channel{
		ID:   1,
		Name: "test-channel",
	}
	channel := &biz.Channel{
		Channel: entChannel,
	}

	state := &PersistenceState{
		CurrentCandidate: &ChannelModelsCandidate{
			Channel: channel,
		},
	}
	outbound := &PersistentOutboundTransformer{
		state: state,
	}

	middleware := &rateLimitTracking{
		outbound: outbound,
		tracker:  tracker,
	}

	tests := []struct {
		name           string
		response       *llm.Response
		expectedTokens int64
	}{
		{
			name: "tracks tokens from response",
			response: &llm.Response{
				Usage: &llm.Usage{
					TotalTokens: 150,
				},
			},
			expectedTokens: 150,
		},
		{
			name: "handles nil usage",
			response: &llm.Response{
				Usage: nil,
			},
			expectedTokens: 0,
		},
		{
			name: "handles zero tokens",
			response: &llm.Response{
				Usage: &llm.Usage{
					TotalTokens: 0,
				},
			},
			expectedTokens: 0,
		},
		{
			name:           "handles nil response",
			response:       nil,
			expectedTokens: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset tracker
			tracker = NewChannelRequestTracker()
			middleware.tracker = tracker

			ctx := context.Background()
			result, err := middleware.OnOutboundLlmResponse(ctx, tt.response)

			assert.NoError(t, err)
			assert.Equal(t, tt.response, result)
			assert.Equal(t, tt.expectedTokens, tracker.GetTokenCount(channel.ID))
		})
	}
}

func TestRateLimitTracking_OnOutboundLlmResponse_MultipleChannels(t *testing.T) {
	tracker := NewChannelRequestTracker()

	entChannel1 := &ent.Channel{ID: 1, Name: "channel-1"}
	entChannel2 := &ent.Channel{ID: 2, Name: "channel-2"}

	channel1 := &biz.Channel{Channel: entChannel1}
	channel2 := &biz.Channel{Channel: entChannel2}

	state1 := &PersistenceState{
		CurrentCandidate: &ChannelModelsCandidate{Channel: channel1},
	}
	state2 := &PersistenceState{
		CurrentCandidate: &ChannelModelsCandidate{Channel: channel2},
	}

	outbound1 := &PersistentOutboundTransformer{state: state1}
	outbound2 := &PersistentOutboundTransformer{state: state2}

	middleware1 := &rateLimitTracking{outbound: outbound1, tracker: tracker}
	middleware2 := &rateLimitTracking{outbound: outbound2, tracker: tracker}

	ctx := context.Background()

	// Add tokens for channel 1
	_, _ = middleware1.OnOutboundLlmResponse(ctx, &llm.Response{
		Usage: &llm.Usage{TotalTokens: 100},
	})

	// Add tokens for channel 2
	_, _ = middleware2.OnOutboundLlmResponse(ctx, &llm.Response{
		Usage: &llm.Usage{TotalTokens: 200},
	})

	assert.Equal(t, int64(100), tracker.GetTokenCount(1))
	assert.Equal(t, int64(200), tracker.GetTokenCount(2))
}

func TestRateLimitTracking_OnOutboundLlmStream(t *testing.T) {
	tracker := NewChannelRequestTracker()

	entChannel := &ent.Channel{ID: 1, Name: "test-channel"}
	channel := &biz.Channel{Channel: entChannel}

	state := &PersistenceState{
		CurrentCandidate: &ChannelModelsCandidate{Channel: channel},
	}
	outbound := &PersistentOutboundTransformer{state: state}

	middleware := &rateLimitTracking{
		outbound: outbound,
		tracker:  tracker,
	}

	// Create mock stream with usage in last chunk
	events := []*llm.Response{
		{ID: "1", Usage: nil},
		{ID: "2", Usage: nil},
		{ID: "3", Usage: &llm.Usage{TotalTokens: 250}},
	}

	mockStream := streams.SliceStream(events)

	ctx := context.Background()
	wrappedStream, err := middleware.OnOutboundLlmStream(ctx, mockStream)
	assert.NoError(t, err)

	// Consume the stream
	tokenCount := int64(0)

	for wrappedStream.Next() {
		event := wrappedStream.Current()
		if event != nil && event.Usage != nil {
			tokenCount = event.Usage.TotalTokens
		}
	}

	assert.NoError(t, wrappedStream.Err())
	assert.Equal(t, int64(250), tokenCount)
	assert.Equal(t, int64(250), tracker.GetTokenCount(channel.ID))
}

func TestRateLimitTracking_OnOutboundRawRequest_DoesNotIncrementRequests(t *testing.T) {
	tracker := NewChannelRequestTracker()

	entChannel := &ent.Channel{ID: 1, Name: "test-channel"}
	channel := &biz.Channel{Channel: entChannel}

	state := &PersistenceState{
		CurrentCandidate: &ChannelModelsCandidate{Channel: channel},
	}
	outbound := &PersistentOutboundTransformer{state: state}

	middleware := &rateLimitTracking{
		outbound: outbound,
		tracker:  tracker,
	}

	ctx := context.Background()

	for range 5 {
		_, err := middleware.OnOutboundRawRequest(ctx, nil)
		assert.NoError(t, err)
	}

	assert.Equal(t, int64(0), tracker.GetRequestCount(channel.ID))
}

func TestRateLimitTracking_CombinedTokenOnly(t *testing.T) {
	tracker := NewChannelRequestTracker()

	entChannel := &ent.Channel{ID: 1, Name: "test-channel"}
	channel := &biz.Channel{Channel: entChannel}

	state := &PersistenceState{
		CurrentCandidate: &ChannelModelsCandidate{Channel: channel},
	}
	outbound := &PersistentOutboundTransformer{state: state}

	middleware := &rateLimitTracking{
		outbound: outbound,
		tracker:  tracker,
	}

	ctx := context.Background()

	// Simulate a request flow. RPM admission is handled before this middleware;
	// tracking only records TPM and upstream cooldown signals.
	_, _ = middleware.OnOutboundRawRequest(ctx, nil)
	_, _ = middleware.OnOutboundRawRequest(ctx, nil)

	_, _ = middleware.OnOutboundLlmResponse(ctx, &llm.Response{
		Usage: &llm.Usage{TotalTokens: 100},
	})

	_, _ = middleware.OnOutboundRawRequest(ctx, nil)

	_, _ = middleware.OnOutboundLlmResponse(ctx, &llm.Response{
		Usage: &llm.Usage{TotalTokens: 50},
	})

	assert.Equal(t, int64(0), tracker.GetRequestCount(channel.ID))
	assert.Equal(t, int64(150), tracker.GetTokenCount(channel.ID))
}

func TestNoopRateLimitTracking(t *testing.T) {
	middleware := &noopRateLimitTracking{}

	ctx := context.Background()

	// Should return response unchanged
	resp := &llm.Response{ID: "test"}
	result, err := middleware.OnOutboundLlmResponse(ctx, resp)
	assert.NoError(t, err)
	assert.Equal(t, resp, result)

	// Should return stream unchanged
	stream := streams.SliceStream([]*llm.Response{{ID: "1"}})
	wrappedStream, err := middleware.OnOutboundLlmStream(ctx, stream)
	assert.NoError(t, err)
	assert.Equal(t, stream, wrappedStream)
}

// ========== OnOutboundRawError Tests (429 Cooldown) ==========

func TestRateLimitTracking_OnOutboundRawError_429(t *testing.T) {
	tracker := NewChannelRequestTracker()

	entChannel := &ent.Channel{ID: 1, Name: "test-channel"}
	channel := &biz.Channel{Channel: entChannel}

	state := &PersistenceState{
		CurrentCandidate: &ChannelModelsCandidate{Channel: channel},
	}
	outbound := &PersistentOutboundTransformer{state: state}

	middleware := &rateLimitTracking{
		outbound: outbound,
		tracker:  tracker,
	}

	ctx := context.Background()

	// Simulate 429 error with Retry-After header
	httpErr := &httpclient.Error{
		StatusCode: http.StatusTooManyRequests,
		Headers:    http.Header{"Retry-After": []string{"30"}},
	}

	middleware.OnOutboundRawError(ctx, httpErr)

	// Verify channel is in cooldown
	assert.True(t, tracker.IsCoolingDown(channel.ID))
}

func TestRateLimitTracking_OnOutboundRawError_QueueErrorIgnored(t *testing.T) {
	tracker := NewChannelRequestTracker()

	channel := &biz.Channel{Channel: &ent.Channel{ID: 7, Name: "kimi"}}

	state := &PersistenceState{
		CurrentCandidate: &ChannelModelsCandidate{Channel: channel},
	}
	outbound := &PersistentOutboundTransformer{state: state}

	middleware := &rateLimitTracking{
		outbound: outbound,
		tracker:  tracker,
	}

	queueErr := asChannelQueueError(channel, ErrChannelQueueFull)
	require.NotNil(t, queueErr)

	middleware.OnOutboundRawError(context.Background(), queueErr)

	assert.False(t, tracker.IsCoolingDown(channel.ID),
		"local queue rejection must not trigger upstream-style cooldown")
}

func TestRateLimitTracking_OnOutboundRawError_429WithoutRetryAfter(t *testing.T) {
	tracker := NewChannelRequestTracker()

	entChannel := &ent.Channel{ID: 1, Name: "test-channel"}
	channel := &biz.Channel{Channel: entChannel}

	state := &PersistenceState{
		CurrentCandidate: &ChannelModelsCandidate{Channel: channel},
	}
	outbound := &PersistentOutboundTransformer{state: state}

	middleware := &rateLimitTracking{
		outbound: outbound,
		tracker:  tracker,
	}

	ctx := context.Background()

	httpErr := &httpclient.Error{
		StatusCode: http.StatusTooManyRequests,
		Headers:    http.Header{},
	}

	middleware.OnOutboundRawError(ctx, httpErr)

	until, ok := tracker.GetCooldownUntil(channel.ID)
	require.True(t, ok)
	require.WithinDuration(t, time.Now().Add(time.Minute), until, 5*time.Second)
}

func TestRateLimitTracking_OnOutboundRawError_Retryable500UsesBaseCooldown(t *testing.T) {
	tracker := NewChannelRequestTracker()

	entChannel := &ent.Channel{ID: 1, Name: "test-channel"}
	channel := &biz.Channel{Channel: entChannel}

	state := &PersistenceState{
		CurrentCandidate: &ChannelModelsCandidate{Channel: channel},
	}
	outbound := &PersistentOutboundTransformer{state: state}

	middleware := &rateLimitTracking{
		outbound: outbound,
		tracker:  tracker,
	}

	ctx := context.Background()

	httpErr := &httpclient.Error{
		StatusCode: http.StatusInternalServerError,
		Headers:    http.Header{"Retry-After": []string{"30"}},
	}

	middleware.OnOutboundRawError(ctx, httpErr)

	until, ok := tracker.GetCooldownUntil(channel.ID)
	require.True(t, ok)
	require.WithinDuration(t, time.Now().Add(time.Minute), until, 5*time.Second)
}

func TestRateLimitTracking_OnOutboundRawError_NonRetryable400Ignored(t *testing.T) {
	tracker := NewChannelRequestTracker()
	channel := &biz.Channel{Channel: &ent.Channel{ID: 1, Name: "test-channel"}}
	state := &PersistenceState{
		CurrentCandidate: &ChannelModelsCandidate{Channel: channel},
	}
	outbound := &PersistentOutboundTransformer{state: state}
	middleware := &rateLimitTracking{outbound: outbound, tracker: tracker}

	middleware.OnOutboundRawError(context.Background(), &httpclient.Error{
		StatusCode: http.StatusBadRequest,
		Headers:    http.Header{"Retry-After": []string{"300"}},
	})

	assert.False(t, tracker.IsCoolingDown(channel.ID))
}

func TestRateLimitTracking_OnOutboundRawError_RetryAfterOnlyExtendsAndCaps(t *testing.T) {
	tests := []struct {
		name       string
		retryAfter string
		want       time.Duration
	}{
		{name: "shorter header does not shrink base", retryAfter: "5", want: time.Minute},
		{name: "longer header extends base", retryAfter: "120", want: 2 * time.Minute},
		{name: "header is capped", retryAfter: "3600", want: 5 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewChannelRequestTracker()
			channel := &biz.Channel{Channel: &ent.Channel{ID: 1, Name: "test-channel"}}
			state := &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{Channel: channel},
			}
			middleware := &rateLimitTracking{
				outbound: &PersistentOutboundTransformer{state: state},
				tracker:  tracker,
			}
			now := time.Now()

			middleware.OnOutboundRawError(context.Background(), &httpclient.Error{
				StatusCode: http.StatusTooManyRequests,
				Headers:    http.Header{"Retry-After": []string{tt.retryAfter}},
			})

			until, ok := tracker.GetCooldownUntil(channel.ID)
			require.True(t, ok)
			require.WithinDuration(t, now.Add(tt.want), until, 2*time.Second)
		})
	}
}

func TestRateLimitTracking_OnOutboundRawError_NoChannel(t *testing.T) {
	tracker := NewChannelRequestTracker()

	state := &PersistenceState{
		CurrentCandidate: nil, // No current channel
	}
	outbound := &PersistentOutboundTransformer{state: state}

	middleware := &rateLimitTracking{
		outbound: outbound,
		tracker:  tracker,
	}

	ctx := context.Background()

	// Simulate 429 error
	httpErr := &httpclient.Error{
		StatusCode: http.StatusTooManyRequests,
		Headers:    http.Header{"Retry-After": []string{"30"}},
	}

	// Should not panic
	middleware.OnOutboundRawError(ctx, httpErr)
}

func TestRateLimitTracking_OnOutboundRawError_NilChannel(t *testing.T) {
	tracker := NewChannelRequestTracker()

	outbound := &PersistentOutboundTransformer{}

	middleware := &rateLimitTracking{
		outbound: outbound,
		tracker:  tracker,
	}

	ctx := context.Background()

	// Simulate 429 error
	httpErr := &httpclient.Error{
		StatusCode: http.StatusTooManyRequests,
		Headers:    http.Header{"Retry-After": []string{"30"}},
	}

	// Should not panic
	middleware.OnOutboundRawError(ctx, httpErr)
}

// ========== parseRetryAfter tests moved to llm/httpclient/errors_test.go ==========
