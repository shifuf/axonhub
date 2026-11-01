package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
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

	// DefaultResponseHeaderTimeout bounds the wait for upstream response headers.
	// It is intentionally independent from the stream idle timeout: once headers
	// arrive a long-lived stream may remain open indefinitely as long as bytes
	// continue to arrive.
	DefaultResponseHeaderTimeout = 2 * time.Minute
)

// ErrStreamIdleTimeout reports that an upstream stream stayed silent for longer
// than the configured adjacent-read idle window.
var ErrStreamIdleTimeout = errors.New("stream idle timeout")

// idleTimeoutReadCloser measures inactivity at the raw HTTP body boundary.
// Resetting at the event decoder boundary is insufficient because SSE comment
// heartbeats are valid keepalives but are not emitted as events by go-sse.
type idleTimeoutReadCloser struct {
	body     io.ReadCloser
	timeout  time.Duration
	timer    *time.Timer
	timedOut atomic.Bool
	closed   atomic.Bool

	closeOnce sync.Once
	closeErr  error
}

func newIdleTimeoutReadCloser(body io.ReadCloser, timeout time.Duration) *idleTimeoutReadCloser {
	if body == nil || timeout <= 0 {
		return nil
	}

	reader := &idleTimeoutReadCloser{
		body:    body,
		timeout: timeout,
	}
	reader.timer = time.AfterFunc(timeout, reader.expire)

	return reader
}

func (r *idleTimeoutReadCloser) expire() {
	if !r.closed.CompareAndSwap(false, true) {
		return
	}

	// Mark the cause before closing the body so a concurrently unblocked
	// decoder observes the timeout rather than a generic transport close.
	r.timedOut.Store(true)
	r.closeUnderlying()
}

func (r *idleTimeoutReadCloser) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if n > 0 && !r.closed.Load() && r.timer != nil {
		r.timer.Reset(r.timeout)
	}
	if r.timedOut.Load() {
		return n, fmt.Errorf("%w after %s", ErrStreamIdleTimeout, r.timeout)
	}

	return n, err
}

func (r *idleTimeoutReadCloser) Close() error {
	r.closed.Store(true)
	if r.timer != nil {
		r.timer.Stop()
	}
	r.closeUnderlying()

	return r.closeErr
}

func (r *idleTimeoutReadCloser) closeUnderlying() {
	r.closeOnce.Do(func() {
		r.closeErr = r.body.Close()
	})
}

func (r *idleTimeoutReadCloser) TimedOut() bool {
	return r.timedOut.Load()
}

type idleTimeoutStream struct {
	ctx     context.Context
	stream  streams.Stream[*StreamEvent]
	timeout time.Duration
	body    *idleTimeoutReadCloser
	err     error
}

func withIdleTimeout(
	ctx context.Context,
	stream streams.Stream[*StreamEvent],
	body *idleTimeoutReadCloser,
) streams.Stream[*StreamEvent] {
	if stream == nil || body == nil {
		return stream
	}

	return &idleTimeoutStream{
		ctx:     ctx,
		stream:  stream,
		timeout: body.timeout,
		body:    body,
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

	hasNext := s.stream.Next()

	// Cancellation always wins over the idle guard: closing the decoder may
	// unblock Next around the same time as the request context is canceled.
	if err := s.ctx.Err(); err != nil {
		s.err = err
		return false
	}
	if s.body.TimedOut() {
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
