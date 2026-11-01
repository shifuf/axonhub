package httpclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIdleTimeoutStream_SSECommentsResetRawReadIdleWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	reader, writer := io.Pipe()
	body := newIdleTimeoutReadCloser(reader, 40*time.Millisecond)
	stream := NewDefaultSSEDecoder(ctx, body)
	wrapped := withIdleTimeout(ctx, stream, body)

	writeDone := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				writeDone <- fmt.Errorf("heartbeat writer panicked: %v", recovered)
			}
		}()
		for range 8 {
			if _, err := fmt.Fprint(writer, ": keep-alive\n\n"); err != nil {
				writeDone <- err
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		writeDone <- writer.Close()
	}()

	require.False(t, wrapped.Next())
	require.NoError(t, <-writeDone)
	require.NoError(t, wrapped.Err())
	require.False(t, body.TimedOut())
	require.NoError(t, wrapped.Close())
}

func TestIdleTimeoutStream_TimeoutAfterSilentRawBody(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	reader, writer := io.Pipe()
	defer writer.Close()
	body := newIdleTimeoutReadCloser(reader, 30*time.Millisecond)
	stream := NewDefaultSSEDecoder(ctx, body)
	wrapped := withIdleTimeout(ctx, stream, body)

	start := time.Now()
	require.False(t, wrapped.Next())
	require.ErrorIs(t, wrapped.Err(), ErrStreamIdleTimeout)
	require.Less(t, time.Since(start), 500*time.Millisecond)
	require.True(t, body.TimedOut())
	require.NoError(t, wrapped.Close())
}

func TestIdleTimeoutStream_ClientCancellationIsNotIdleTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	body := newIdleTimeoutReadCloser(reader, time.Second)
	stream := NewDefaultSSEDecoder(ctx, body)
	wrapped := withIdleTimeout(ctx, stream, body)

	done := make(chan struct{})
	go func() {
		defer close(done)
		require.False(t, wrapped.Next())
	}()

	cancel()
	require.NoError(t, body.Close())
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream did not stop after client cancellation")
	}

	require.ErrorIs(t, wrapped.Err(), context.Canceled)
	require.NotErrorIs(t, wrapped.Err(), ErrStreamIdleTimeout)
	require.NoError(t, writer.Close())
}

func TestIdleTimeoutStream_DisabledTimeout(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()

	body := newIdleTimeoutReadCloser(reader, 0)
	stream := NewDefaultSSEDecoder(t.Context(), reader)

	require.Nil(t, body)
	require.Same(t, stream, withIdleTimeout(t.Context(), stream, body))
}

func TestHttpClients_SetBoundedResponseHeaderWait(t *testing.T) {
	for name, client := range map[string]*HttpClient{
		"default": NewHttpClient(),
		"proxy":   NewHttpClientWithProxy(nil),
	} {
		t.Run(name, func(t *testing.T) {
			transport, ok := client.GetNativeClient().Transport.(*http.Transport)
			require.True(t, ok)
			require.Equal(t, DefaultResponseHeaderTimeout, transport.ResponseHeaderTimeout)
		})
	}
}
