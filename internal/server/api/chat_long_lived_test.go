package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/stretchr/testify/require"
)

type longLivedTestStream struct {
	ctx       context.Context
	remaining int
	interval  time.Duration
	current   *httpclient.StreamEvent
	done      bool
}

func (s *longLivedTestStream) Next() bool {
	if s.done {
		return false
	}
	timer := time.NewTimer(s.interval)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		s.done = true
		return false
	case <-timer.C:
	}
	if s.remaining == 0 {
		s.current = &httpclient.StreamEvent{Data: []byte(`[DONE]`)}
		s.done = true
		return true
	}
	s.remaining--
	s.current = &httpclient.StreamEvent{Data: []byte(fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":"part-%d"},"finish_reason":null}]}`, s.remaining))}
	return true
}
func (s *longLivedTestStream) Current() *httpclient.StreamEvent { return s.current }
func (s *longLivedTestStream) Err() error                       { return s.ctx.Err() }
func (s *longLivedTestStream) Close() error                     { return nil }

// Simulated time advances real Go timers by more than two hours. This is not
// a claim of a two-hour wall-clock network/production soak test.
func TestChatStreamSurvivesTwoHoursSimulated(t *testing.T) {
	for _, interval := range []time.Duration{20 * time.Second, 5 * time.Minute} {
		t.Run(interval.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/chat/completions", nil)
				count := int(2*time.Hour/interval) + 1
				stream := &longLivedTestStream{ctx: ctx, remaining: count, interval: interval}
				started := time.Now()
				writeSSEStream(c, stream, FormatStreamError, SSEKeepAliveConfig{Enabled: true, Interval: 15 * time.Second}, sseHeartbeatOpenAI)
				require.Greater(t, time.Since(started), 2*time.Hour)
				require.NoError(t, ctx.Err())
				require.Zero(t, stream.remaining)
				require.Equal(t, count, strings.Count(w.Body.String(), `"content":"part-`))
				require.Contains(t, w.Body.String(), ": keep-alive\n\n")
				require.Contains(t, w.Body.String(), "data: [DONE]")
				require.NotContains(t, w.Body.String(), "event:error")
			})
		})
	}
}
