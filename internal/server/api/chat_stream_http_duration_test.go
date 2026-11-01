package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/llm"
)

// Default runs quickly across a deliberately short inherited socket deadline.
// AXONHUB_STREAM_SOAK_DURATION=2h runs a real two-hour localhost HTTP soak;
// it does not call or charge an external provider or touch the running server.
func TestChatStreamRealHTTPDuration(t *testing.T) {
	duration := 350 * time.Millisecond
	interval := 80 * time.Millisecond
	heartbeat := 5 * time.Millisecond
	if value := os.Getenv("AXONHUB_STREAM_SOAK_DURATION"); value != "" {
		parsed, err := time.ParseDuration(value)
		require.NoError(t, err)
		require.GreaterOrEqual(t, parsed, time.Second)
		require.LessOrEqual(t, parsed, 3*time.Hour)
		duration = parsed
		interval = min(20*time.Second, parsed/3)
		heartbeat = min(15*time.Second, interval/2)
	}
	for _, passthrough := range []bool{false, true} {
		name := "transformed"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				send := func(i int) bool {
					_, err := fmt.Fprintf(w, "data: {\"id\":\"chat-soak\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"part-%d \"},\"finish_reason\":null}]}\n\n", i)
					if err != nil {
						return false
					}
					w.(http.Flusher).Flush()
					return true
				}
				if !send(0) {
					return
				}
				end := time.NewTimer(duration)
				defer end.Stop()
				tick := time.NewTicker(interval)
				defer tick.Stop()
				for i := 1; ; i++ {
					select {
					case <-r.Context().Done():
						return
					case <-tick.C:
						if !send(i) {
							return
						}
					case <-end.C:
						fmt.Fprint(w, "data: {\"id\":\"chat-soak\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":34,\"total_tokens\":46}}\n\ndata: [DONE]\n\n")
						w.(http.Flusher).Flush()
						return
					}
				}
			}))
			defer upstream.Close()
			f := newStreamDeliveryE2EFixture(t, llm.APIFormatOpenAIChatCompletion, "", true)
			f.passthrough = passthrough
			row, err := f.client.Channel.UpdateOne(f.channel).SetBaseURL(upstream.URL).SetEndpoints([]objects.ChannelEndpoint{{APIFormat: llm.APIFormatOpenAIChatCompletion.String(), BaseURL: upstream.URL, Transport: objects.ChannelEndpointTransportHTTP}}).Save(f.ctx)
			require.NoError(t, err)
			f.channel = row
			h := f.handler(t)
			h.StreamWriter = nil
			h.sseKeepAlive.Interval = heartbeat
			router := gin.New()
			router.Use(middleware.WithTimeout(0), f.testContext)
			handlerDone := make(chan struct{})
			router.POST("/v1/chat/completions", func(c *gin.Context) { defer close(handlerDone); h.ChatCompletion(c) })
			downstream := httptest.NewUnstartedServer(router)
			downstream.Config.WriteTimeout = 25 * time.Millisecond
			downstream.Start()
			defer downstream.Close()
			ctx, cancel := context.WithTimeout(t.Context(), duration+10*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, downstream.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"long stream"}]}`))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			started := time.Now()
			resp, err := downstream.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.GreaterOrEqual(t, time.Since(started), duration)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Contains(t, string(body), "part-0")
			require.Contains(t, string(body), "part-1")
			require.Contains(t, string(body), ": keep-alive\n\n")
			require.Contains(t, string(body), "data: [DONE]")
			require.NotContains(t, string(body), "event:error")
			select {
			case <-handlerDone:
			case <-ctx.Done():
				t.Fatal("handler did not finish and persist the completed response")
			}
			f.assertDatabase(t, request.StatusCompleted, true)
			t.Logf("real HTTP %s stream completed in %s, bytes=%d, heartbeats=%d", name, time.Since(started), len(body), strings.Count(string(body), ": keep-alive\n\n"))
		})
	}
}
