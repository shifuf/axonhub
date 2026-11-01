package httpclient

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type blockingStream struct {
	ctx      context.Context
	events   []*StreamEvent
	index    int
	closed   chan struct{}
	closeErr error
	closeOne sync.Once
}

func newBlockingStream(ctx context.Context, events ...*StreamEvent) *blockingStream {
	return &blockingStream{
		ctx:    ctx,
		events: events,
		closed: make(chan struct{}),
	}
}

func (s *blockingStream) Next() bool {
	if s.index < len(s.events) {
		s.index++
		return true
	}

	select {
	case <-s.closed:
		return false
	case <-s.ctx.Done():
		return false
	}
}

func (s *blockingStream) Current() *StreamEvent {
	if s.index == 0 || s.index > len(s.events) {
		return nil
	}

	return s.events[s.index-1]
}

func (s *blockingStream) Err() error {
	return nil
}

func (s *blockingStream) Close() error {
	s.closeOne.Do(func() {
		close(s.closed)
	})

	return s.closeErr
}

func TestIdleTimeoutStream_TimeoutAfterLastEvent(t *testing.T) {
	ctx := context.Background()
	stream := newBlockingStream(ctx, &StreamEvent{Data: []byte("first")})
	wrapped := withIdleTimeout(ctx, stream, 20*time.Millisecond)

	require.True(t, wrapped.Next())
	require.Equal(t, "first", string(wrapped.Current().Data))

	require.False(t, wrapped.Next())
	require.ErrorIs(t, wrapped.Err(), ErrStreamIdleTimeout)
}

func TestIdleTimeoutStream_ClientCancellationIsNotIdleTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stream := newBlockingStream(ctx)
	wrapped := withIdleTimeout(ctx, stream, time.Second)

	done := make(chan struct{})
	go func() {
		defer close(done)
		require.False(t, wrapped.Next())
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream did not stop after client cancellation")
	}

	require.ErrorIs(t, wrapped.Err(), context.Canceled)
	require.NotErrorIs(t, wrapped.Err(), ErrStreamIdleTimeout)
}

func TestIdleTimeoutStream_DisabledTimeout(t *testing.T) {
	ctx := context.Background()
	stream := newBlockingStream(ctx)

	require.Same(t, stream, withIdleTimeout(ctx, stream, 0))
}
