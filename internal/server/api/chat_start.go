package api

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/openai/codex"
)

type chatStreamAbortKey struct{}

func abortChatStream(ctx context.Context) {
	if abort, ok := ctx.Value(chatStreamAbortKey{}).(context.CancelFunc); ok {
		abort()
	}
}

// awaitChatCompletion keeps a slow header wait, failover and first-content probe
// alive without giving any worker access to Gin's response writer. A heartbeat
// commits HTTP 200; subsequent failures must be encoded as protocol SSE errors.
func (h *ChatCompletionHandlers) awaitChatCompletion(
	c *gin.Context,
	request *httpclient.Request,
	abort context.CancelFunc,
	process func() (orchestrator.ChatCompletionResult, error),
) (orchestrator.ChatCompletionResult, error) {
	if !h.keepAliveDuringStart(request) {
		return process()
	}

	ctx := c.Request.Context()
	if err := ctx.Err(); err != nil {
		return orchestrator.ChatCompletionResult{}, err
	}
	type outcome struct {
		result orchestrator.ChatCompletionResult
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		var out outcome
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Error(ctx, "Panic while preparing chat stream", log.Any("panic", recovered))
				out.err = errors.New("chat stream preparation failed")
			}
			finished <- out
		}()
		out.result, out.err = process()
	}()

	// Joining before cleanup is important: Close must never race a prefix Next.
	stop := func(cause error) (orchestrator.ChatCompletionResult, error) {
		abort()
		out := <-finished
		closeChatCompletionStream(ctx, out.result.ChatCompletionStream)
		return orchestrator.ChatCompletionResult{}, cause
	}
	timer := time.NewTicker(h.sseKeepAlive.Interval)
	defer timer.Stop()
	for {
		select {
		case out := <-finished:
			return out.result, out.err
		case <-ctx.Done():
			return stop(ctx.Err())
		case <-timer.C:
			// A ready response/error wins over a heartbeat, preserving its HTTP status.
			select {
			case out := <-finished:
				return out.result, out.err
			default:
			}
			if ctx.Err() != nil {
				return stop(ctx.Err())
			}
			setSSEHeaders(c)
			c.Header("Access-Control-Allow-Origin", "*")
			if err := writeSSEHeartbeatEvent(ctx, c.Writer, h.sseHeartbeatFormat); err != nil {
				return stop(err)
			}
		}
	}
}

func (h *ChatCompletionHandlers) keepAliveDuringStart(request *httpclient.Request) bool {
	if request == nil || !h.sseKeepAlive.Enabled || h.sseKeepAlive.Interval <= 0 || h.sseHeartbeatFormat == sseHeartbeatNone ||
		gjson.GetBytes(request.Body, "stream").Type != gjson.True {
		return false
	}
	// Do not turn binary audio, multipart or compact responses into SSE based on
	// a stray stream field. These are the JSON SSE APIs handled by this handler.
	for _, suffix := range []string{"/chat/completions", "/completions", "/responses", "/messages"} {
		if strings.HasSuffix(request.Path, suffix) {
			// Codex Responses needs the upstream turn-state/reasoning headers on
			// the final HTTP response. Once an SSE heartbeat commits the response,
			// those headers can no longer be forwarded.
			if strings.HasSuffix(request.Path, "/responses") && codex.GetSessionIDFromHeaders(request.Headers) != "" {
				return false
			}
			return true
		}
	}
	return false
}

func closeChatCompletionStream(ctx context.Context, stream streams.Stream[*httpclient.StreamEvent]) {
	if stream == nil {
		return
	}
	err := stream.Close()
	if waiter, ok := stream.(streams.FinalizationWaiter); ok {
		waiter.WaitForFinalization()
	}
	if err != nil {
		log.Error(ctx, "Error closing chat stream", log.Cause(err))
	}
}

func (h *ChatCompletionHandlers) writeStartedChatError(c *gin.Context, err error) {
	// Go through the same error-policy/Responses adapter as an established
	// stream. Never append a JSON HTTP error to a committed SSE response.
	stream := streams.MapErr(streams.SliceStream([]*httpclient.StreamEvent{nil}), func(*httpclient.StreamEvent) (*httpclient.StreamEvent, error) { return nil, err })
	if h.ChatCompletionOrchestrator != nil {
		stream = newUpstreamErrorStream(c.Request.Context(), stream, h.ChatCompletionOrchestrator.SystemService)
	}
	h.writeSSEStream(c, stream)
}

// prepareChatStream keeps ownership of a live stream until wrappers and the
// Responses prefix probe finish. Its caller may recover a preparation panic;
// the original stream must still be canceled, closed and finalized in that case.
func prepareChatStream(
	ctx context.Context,
	source streams.Stream[*httpclient.StreamEvent],
	abort context.CancelFunc,
	prepare func(streams.Stream[*httpclient.StreamEvent]) streams.Stream[*httpclient.StreamEvent],
) streams.Stream[*httpclient.StreamEvent] {
	transferred := false
	defer func() {
		if !transferred {
			abort()
			closeChatCompletionStream(ctx, source)
		}
	}()
	stream := prepare(source)
	transferred = true
	return stream
}

// Internal upstream cancellation must not cancel error delivery to a client
// that is still connected (e.g. safeguards or a failed prefix preparation).
func newChatProcessingContext(c *gin.Context) (context.Context, context.CancelFunc) {
	caller := c.Request.Context()
	upstream, cancel := context.WithCancel(caller)
	c.Request = c.Request.WithContext(context.WithValue(caller, chatStreamAbortKey{}, cancel))
	return upstream, cancel
}
