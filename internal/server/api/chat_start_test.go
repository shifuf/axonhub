package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/openai/codex"
	"github.com/stretchr/testify/require"
)

func TestAwaitChatCompletion_HeartbeatWhileWaiting(t *testing.T) {
	for _, format := range []sseHeartbeatFormat{sseHeartbeatOpenAI, sseHeartbeatAnthropic} {
		t.Run(sseHeartbeatFormatName(format), func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", nil)
			h := &ChatCompletionHandlers{sseKeepAlive: SSEKeepAliveConfig{Enabled: true, Interval: time.Millisecond}, sseHeartbeatFormat: format}
			response := &httpclient.Response{StatusCode: 200}
			result, err := h.awaitChatCompletion(c, &httpclient.Request{Path: "/v1/chat/completions", Body: []byte(`{"stream":true}`)}, cancel, func() (orchestrator.ChatCompletionResult, error) {
				time.Sleep(15 * time.Millisecond)
				return orchestrator.ChatCompletionResult{ChatCompletion: response}, nil
			})
			require.NoError(t, err)
			require.Same(t, response, result.ChatCompletion)
			require.True(t, c.Writer.Written())
			require.Equal(t, "no", w.Header().Get("X-Accel-Buffering"))
			if format == sseHeartbeatOpenAI {
				require.Contains(t, w.Body.String(), ": keep-alive\n\n")
			} else {
				require.Contains(t, w.Body.String(), "event: ping\n")
			}
		})
	}
}

func TestAwaitChatCompletion_FastErrorPreservesStatus(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	h := &ChatCompletionHandlers{sseKeepAlive: SSEKeepAliveConfig{Enabled: true, Interval: time.Hour}, sseHeartbeatFormat: sseHeartbeatOpenAI}
	expected := &httpclient.Error{StatusCode: 429, Body: []byte(`{"error":{"message":"rate limited"}}`)}
	_, err := h.awaitChatCompletion(c, &httpclient.Request{Path: "/v1/chat/completions", Body: []byte(`{"stream":true}`)}, func() {}, func() (orchestrator.ChatCompletionResult, error) {
		return orchestrator.ChatCompletionResult{}, expected
	})
	require.ErrorIs(t, err, expected)
	require.False(t, c.Writer.Written())
	require.Empty(t, w.Body.String())
}

func TestAwaitChatCompletion_DoesNotHeartbeatNonSSE(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		enabled          bool
	}{
		{"nonstream", "/v1/chat/completions", `{"stream":false}`, true},
		{"missing-stream", "/v1/chat/completions", `{}`, true},
		{"binary", "/v1/audio/speech", `{"stream":true}`, true},
		{"compact", "/v1/responses/compact", `{"stream":true}`, true},
		{"disabled", "/v1/chat/completions", `{"stream":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, nil)
			h := &ChatCompletionHandlers{sseKeepAlive: SSEKeepAliveConfig{Enabled: tc.enabled, Interval: time.Millisecond}, sseHeartbeatFormat: sseHeartbeatOpenAI}
			_, err := h.awaitChatCompletion(c, &httpclient.Request{Path: tc.path, Body: []byte(tc.body)}, func() {}, func() (orchestrator.ChatCompletionResult, error) {
				time.Sleep(5 * time.Millisecond)
				return orchestrator.ChatCompletionResult{}, nil
			})
			require.NoError(t, err)
			require.False(t, c.Writer.Written())
			require.Empty(t, w.Body.String())
		})
	}
}

func TestAwaitChatCompletion_CodexHeadersCanBeCommittedAfterStreamStarts(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set(codex.TurnMetadataHeader, `{"session_id":"session-1"}`)
	request := &httpclient.Request{
		Path:    "/v1/responses",
		Body:    []byte(`{"stream":true}`),
		Headers: c.Request.Header.Clone(),
	}
	h := &ChatCompletionHandlers{sseKeepAlive: SSEKeepAliveConfig{Enabled: true, Interval: time.Millisecond}, sseHeartbeatFormat: sseHeartbeatOpenAI}
	called := false
	_, err := h.awaitChatCompletion(c, request, func() {}, func() (orchestrator.ChatCompletionResult, error) {
		time.Sleep(10 * time.Millisecond)
		called = true
		return orchestrator.ChatCompletionResult{}, nil
	})
	require.NoError(t, err)
	require.True(t, called, "no startup heartbeat should run before Codex response headers are available")
	require.False(t, c.Writer.Written())
	require.Empty(t, w.Body.String())
}

func TestAwaitChatCompletion_WriteFailureCancelsAndJoins(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w := &flushFailingResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: errors.New("broken pipe")}
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", nil)
	h := &ChatCompletionHandlers{sseKeepAlive: SSEKeepAliveConfig{Enabled: true, Interval: time.Millisecond}, sseHeartbeatFormat: sseHeartbeatOpenAI}
	joined := make(chan struct{})
	stream := &startCleanupStream{Stream: streams.SliceStream([]*httpclient.StreamEvent{})}
	_, err := h.awaitChatCompletion(c, &httpclient.Request{Path: "/v1/chat/completions", Body: []byte(`{"stream":true}`)}, cancel, func() (orchestrator.ChatCompletionResult, error) {
		<-ctx.Done()
		close(joined)
		return orchestrator.ChatCompletionResult{ChatCompletionStream: stream}, nil
	})
	require.ErrorContains(t, err, "broken pipe")
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	select {
	case <-joined:
	default:
		t.Fatal("processing goroutine not joined")
	}
	require.True(t, stream.closed)
}

type startCleanupStream struct {
	streams.Stream[*httpclient.StreamEvent]
	closed bool
}

func (s *startCleanupStream) Close() error { s.closed = true; return s.Stream.Close() }

func TestAwaitChatCompletion_PanicBecomesError(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	h := &ChatCompletionHandlers{sseKeepAlive: SSEKeepAliveConfig{Enabled: true, Interval: time.Hour}, sseHeartbeatFormat: sseHeartbeatOpenAI}
	_, err := h.awaitChatCompletion(c, &httpclient.Request{Path: "/v1/responses", Body: []byte(`{"stream":true}`)}, func() {}, func() (orchestrator.ChatCompletionResult, error) { panic("test") })
	require.Error(t, err)
	require.NotContains(t, err.Error(), "test")
}

func TestChatCompletionStartedError_UsesProtocolSSE(t *testing.T) {
	for _, responses := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "responses"}[responses], func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			h := &ChatCompletionHandlers{sseHeartbeatFormat: sseHeartbeatOpenAI}
			if responses {
				h.streamAdapterFactory = newResponsesStreamAdapter
			}
			setSSEHeaders(c)
			require.NoError(t, writeSSEHeartbeatEvent(c.Request.Context(), c.Writer, sseHeartbeatOpenAI))
			h.writeStartedChatError(c, &httpclient.Error{StatusCode: 429, Body: []byte(`{"error":{"message":"rate limited","code":"rate_limit_exceeded"}}`)})
			require.Equal(t, http.StatusOK, w.Code)
			require.Contains(t, w.Body.String(), "event:error")
			require.Contains(t, w.Body.String(), "rate_limit_exceeded")
			if responses {
				require.Contains(t, w.Body.String(), `"type":"error"`)
				require.Contains(t, w.Body.String(), `"sequence_number":0`)
			}
			require.False(t, strings.Contains(w.Body.String(), "data: [DONE]"))
		})
	}
}

func TestAwaitChatCompletion_AlreadyCanceledDoesNotStartProvider(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses", nil)
	h := &ChatCompletionHandlers{sseKeepAlive: SSEKeepAliveConfig{Enabled: true, Interval: time.Hour}, sseHeartbeatFormat: sseHeartbeatOpenAI}
	called := false
	_, err := h.awaitChatCompletion(c, &httpclient.Request{Path: "/v1/responses", Body: []byte(`{"stream":true}`)}, cancel, func() (orchestrator.ChatCompletionResult, error) {
		called = true
		return orchestrator.ChatCompletionResult{}, ctx.Err()
	})
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, called)
	require.Empty(t, w.Body.String())
}

type startupContextStream struct{ ctx context.Context }

func (s *startupContextStream) Next() bool                       { <-s.ctx.Done(); return false }
func (s *startupContextStream) Current() *httpclient.StreamEvent { return nil }
func (s *startupContextStream) Err() error                       { return s.ctx.Err() }
func (s *startupContextStream) Close() error                     { return nil }

func TestWriteSSEStream_WriteFailureAbortsProviderBeforeJoiningReader(t *testing.T) {
	deadline, stop := context.WithTimeout(t.Context(), 2*time.Second)
	defer stop()
	ctx, cancel := context.WithCancel(deadline)
	defer cancel()
	ctx = context.WithValue(ctx, chatStreamAbortKey{}, cancel)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	c.Writer = &heartbeatFailingResponseWriter{ResponseWriter: c.Writer, err: errors.New("broken pipe"), failed: make(chan struct{}, 1)}
	before := time.Now()
	writeSSEStreamWithHeartbeat(c, &startupContextStream{ctx: ctx}, FormatStreamError, time.Millisecond, sseHeartbeatOpenAI)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.Less(t, time.Since(before), time.Second, "writer failure must not wait for provider timeout")
}

func TestChatCompletionStartedError_AnthropicEnvelope(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	h := &ChatCompletionHandlers{sseHeartbeatFormat: sseHeartbeatAnthropic}
	setSSEHeaders(c)
	require.NoError(t, writeSSEHeartbeatEvent(c.Request.Context(), c.Writer, sseHeartbeatAnthropic))
	h.writeStartedChatError(c, &httpclient.Error{StatusCode: 429, Body: []byte(`{"error":{"message":"provider rate limited","type":"rate_limit_error"},"request_id":"upstream-123"}`)})
	require.Contains(t, w.Body.String(), `"type":"error"`)
	require.Contains(t, w.Body.String(), `"type":"rate_limit_error"`)
	require.Contains(t, w.Body.String(), `"request_id":"upstream-123"`)
	require.Contains(t, w.Body.String(), "event:error")
}

type panicPrimingStream struct {
	startCleanupStream
	finalized bool
}

func (s *panicPrimingStream) Next() bool           { panic("prefix read panic") }
func (s *panicPrimingStream) WaitForFinalization() { s.finalized = true }

func TestPrepareChatStream_PanicClosesOriginalAndFinalizes(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	source := &panicPrimingStream{startCleanupStream: startCleanupStream{Stream: streams.SliceStream([]*httpclient.StreamEvent{})}}
	require.Panics(t, func() {
		prepareChatStream(ctx, source, cancel, func(stream streams.Stream[*httpclient.StreamEvent]) streams.Stream[*httpclient.StreamEvent] {
			return primeCodexTurnStateHeader(make(http.Header), stream)
		})
	})
	require.True(t, source.closed)
	require.True(t, source.finalized)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
}
func TestPrepareChatStream_SuccessTransfersOwnership(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	source := &startCleanupStream{Stream: streams.SliceStream([]*httpclient.StreamEvent{{Data: []byte(`[DONE]`)}})}
	stream := prepareChatStream(ctx, source, cancel, func(stream streams.Stream[*httpclient.StreamEvent]) streams.Stream[*httpclient.StreamEvent] {
		return primeCodexTurnStateHeader(make(http.Header), stream)
	})
	require.False(t, source.closed)
	require.NoError(t, ctx.Err())
	require.True(t, stream.Next())
	require.Equal(t, []byte(`[DONE]`), stream.Current().Data)
}

type panicAfterHeartbeatStream struct {
	panicPrimingStream
	heartbeat <-chan struct{}
}

func (s *panicAfterHeartbeatStream) Next() bool {
	<-s.heartbeat
	panic("private upstream panic detail")
}

func TestChatStart_PanicAfterHeartbeatStillDeliversError(t *testing.T) {
	caller, cancelCaller := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelCaller()
	ready := make(chan struct{})
	close(ready)
	w := &startupHeartbeatRecorder{ResponseRecorder: httptest.NewRecorder(), upstreamStarted: ready, heartbeat: make(chan struct{})}
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(caller, http.MethodPost, "/v1/responses", nil)
	processCtx, abort := newChatProcessingContext(c)
	defer abort()
	source := &panicAfterHeartbeatStream{panicPrimingStream: panicPrimingStream{startCleanupStream: startCleanupStream{Stream: streams.SliceStream([]*httpclient.StreamEvent{})}}, heartbeat: w.heartbeat}
	h := &ChatCompletionHandlers{sseKeepAlive: SSEKeepAliveConfig{Enabled: true, Interval: time.Millisecond}, sseHeartbeatFormat: sseHeartbeatOpenAI, streamAdapterFactory: newResponsesStreamAdapter}
	_, err := h.awaitChatCompletion(c, &httpclient.Request{Path: "/v1/responses", Body: []byte(`{"stream":true}`)}, abort, func() (orchestrator.ChatCompletionResult, error) {
		prepareChatStream(processCtx, source, abort, func(stream streams.Stream[*httpclient.StreamEvent]) streams.Stream[*httpclient.StreamEvent] {
			return primeCodexTurnStateHeader(make(http.Header), stream)
		})
		return orchestrator.ChatCompletionResult{}, nil
	})
	require.Error(t, err)
	require.NotErrorIs(t, err, context.Canceled)
	require.True(t, source.closed)
	require.True(t, source.finalized)
	require.ErrorIs(t, processCtx.Err(), context.Canceled)
	require.NoError(t, c.Request.Context().Err(), "upstream abort must not masquerade as a client disconnect")
	h.writeStartedChatError(c, err)
	require.Contains(t, w.Body.String(), ": keep-alive\n\n")
	require.Contains(t, w.Body.String(), "event:error")
	require.Contains(t, w.Body.String(), `"type":"error"`)
	require.NotContains(t, w.Body.String(), "private upstream panic detail")
}
