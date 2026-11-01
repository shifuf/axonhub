package httpclient

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/looplj/axonhub/llm/streams"
)

const (
	// DefaultStreamIdleTimeout bounds the gap between upstream stream reads.
	// Keepalive events and all normal SSE/binary chunks reset it. A long-lived
	// stream has no total deadline; only a permanently silent connection is
	// interrupted.
	DefaultStreamIdleTimeout = 2 * time.Minute
)

// ErrStreamIdleTimeout reports that an upstream stream stayed silent for longer
// than the configured adjacent-read idle window.
var ErrStreamIdleTimeout = errors.New("stream idle timeout")

type idleTimeoutStream struct {
	ctx     context.Context
	stream  streams.Stream[*StreamEvent]
	timeout time.Duration
	err     error
}

func withIdleTimeout(
	ctx context.Context,
	stream streams.Stream[*StreamEvent],
	timeout time.Duration,
) streams.Stream[*StreamEvent] {
	if stream == nil || timeout <= 0 {
		return stream
	}

	return &idleTimeoutStream{
		ctx:     ctx,
		stream:  stream,
		timeout: timeout,
	}
}

func (s *idleTimeoutStream) Next() bool {
	if s.err != nil {
		return false
	}
	if err := s.ctx.Err(); err != nil {
		s.err = err
		return false
	}

	var timedOut atomic.Bool
	timer := time.AfterFunc(s.timeout, func() {
		if timedOut.CompareAndSwap(false, true) {
			_ = s.stream.Close()
		}
	})

	hasNext := s.stream.Next()
	timer.Stop()

	// Cancellation always wins over the idle guard: closing the decoder may
	// unblock Next around the same time as the request context is canceled.
	if err := s.ctx.Err(); err != nil {
		s.err = err
		return false
	}
	if timedOut.Load() {
		s.err = fmt.Errorf("%w after %s", ErrStreamIdleTimeout, s.timeout)
		return false
	}

	return hasNext
}

func (s *idleTimeoutStream) Current() *StreamEvent {
	return s.stream.Current()
}

func (s *idleTimeoutStream) Err() error {
	if s.err != nil {
		return s.err
	}

	return s.stream.Err()
}

func (s *idleTimeoutStream) Close() error {
	return s.stream.Close()
}

func (s *idleTimeoutStream) AcknowledgeDelivery() {
	if observer, ok := s.stream.(streams.DeliveryAcknowledger); ok {
		observer.AcknowledgeDelivery()
	}
}

func (s *idleTimeoutStream) DeliveryAcknowledged() bool {
	observer, ok := s.stream.(streams.DeliveryAcknowledger)
	return ok && observer.DeliveryAcknowledged()
}

func (s *idleTimeoutStream) Deliver(ctx context.Context, write func() (bool, error)) error {
	boundary, ok := s.stream.(streams.DeliveryBoundary)
	if ok {
		return boundary.Deliver(ctx, write)
	}

	terminal, err := write()
	if err == nil && terminal {
		s.AcknowledgeDelivery()
	}

	return err
}

func (s *idleTimeoutStream) WaitForFinalization() {
	if waiter, ok := s.stream.(streams.FinalizationWaiter); ok {
		waiter.WaitForFinalization()
	}
}

func (s *idleTimeoutStream) ExpectedStreamChoices() int {
	if provider, ok := s.stream.(interface{ ExpectedStreamChoices() int }); ok {
		return provider.ExpectedStreamChoices()
	}

	return 0
}
