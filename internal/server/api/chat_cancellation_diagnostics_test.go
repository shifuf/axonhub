package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/stretchr/testify/require"
)

func TestConnectedClientReceivesProcessingCancellationError(t *testing.T) {
	for _, format := range []string{"chat", "responses", "anthropic"} {
		t.Run(format, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
			h := &ChatCompletionHandlers{sseHeartbeatFormat: sseHeartbeatOpenAI}
			if format == "responses" {
				h.streamAdapterFactory = newResponsesStreamAdapter
			}
			if format == "anthropic" {
				h.sseHeartbeatFormat = sseHeartbeatAnthropic
			}
			// The provider/execution stopped, but the downstream caller did not cancel.
			h.writeSSEStream(c, &errorAfterStream{err: fmt.Errorf("execution aborted: %w", context.Canceled)})
			require.NoError(t, c.Request.Context().Err())
			require.Contains(t, w.Body.String(), "event:error")
			require.Contains(t, w.Body.String(), `"code":"stream_processing_canceled"`)
			if format != "chat" {
				require.Contains(t, w.Body.String(), `"type":"error"`)
			}
		})
	}
}

func TestActualClientCancellationDoesNotWriteProcessingError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	WriteSSEStream(c, &errorAfterStream{err: context.Canceled})
	require.NotContains(t, w.Body.String(), "stream_processing_canceled")
	require.NotContains(t, w.Body.String(), "event:error")
}

func TestDeliveredCompletionStillWinsLaterProcessingCancellation(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	WriteSSEStream(c, &errorAfterStream{items: []*httpclient.StreamEvent{{Data: []byte(`[DONE]`)}}, err: context.Canceled})
	require.Contains(t, w.Body.String(), "data: [DONE]")
	require.NotContains(t, w.Body.String(), "event:error")
}
