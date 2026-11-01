package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	entrequest "github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

type startupHeartbeatRecorder struct {
	*httptest.ResponseRecorder
	upstreamStarted <-chan struct{}
	heartbeat       chan struct{}
	once            sync.Once
}

func (w *startupHeartbeatRecorder) Flush() {
	w.ResponseRecorder.Flush()
	if strings.Contains(w.Body.String(), ": keep-alive\n\n") {
		select {
		case <-w.upstreamStarted:
			w.once.Do(func() { close(w.heartbeat) })
		default:
		}
	}
}

// Exercise real net/http + the retry pre-read + API writer + persistence, not
// only an artificially delayed function. The provider cannot progress until a
// downstream heartbeat is flushed while it is waiting for headers or content.
func TestChatCompletion_KeepAliveThroughRealPipeline(t *testing.T) {
	for _, phase := range []string{"headers", "content", "upstream-error"} {
		t.Run(phase, func(t *testing.T) {
			heartbeat := make(chan struct{})
			upstreamStarted := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(upstreamStarted)
				if phase == "content" {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					fmt.Fprint(w, ": upstream ping\n\ndata: {\"id\":\"chatcmpl-wait\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n")
					w.(http.Flusher).Flush()
				}
				select {
				case <-heartbeat:
				case <-r.Context().Done():
					return
				}
				if phase == "upstream-error" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					fmt.Fprint(w, `{"error":{"message":"provider rate limited","type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"id\":\"chatcmpl-wait\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"upstream result\"},\"finish_reason\":null}]}\n\n")
				fmt.Fprint(w, "data: {\"id\":\"chatcmpl-wait\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer upstream.Close()
			client := enttest.NewEntClient(t, "sqlite3", "file:startup-pipeline?mode=memory&_fk=0")
			defer client.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			ctx = ent.NewContext(authz.WithTestBypass(ctx), client)
			row, err := client.Channel.Create().SetType(channel.TypeOpenai).SetName("startup-test").SetBaseURL(upstream.URL + "/v1").SetCredentials(objects.ChannelCredentials{APIKey: "test"}).SetSupportedModels([]string{"gpt-4"}).SetDefaultTestModel("gpt-4").SetStatus(channel.StatusEnabled).Save(ctx)
			require.NoError(t, err)
			channelSvc, requestSvc, systemSvc, usageSvc := setupSpeechAPITestServices(t, client)
			require.NoError(t, systemSvc.SetRetryPolicy(ctx, &biz.RetryPolicy{Enabled: true, MaxChannelRetries: 1, MaxSingleChannelRetries: 0, StreamFirstEventTimeoutSeconds: 0}))
			require.NoError(t, systemSvc.SetStoragePolicy(ctx, &biz.StoragePolicy{StoreRequestBody: true, StoreResponseBody: true, StoreChunks: true}))
			outbound, err := openai.NewOutboundTransformer(row.BaseURL, "test")
			require.NoError(t, err)
			selector := &speechAPISelector{candidates: []*orchestrator.ChannelModelsCandidate{{Channel: &biz.Channel{Channel: row, Outbound: outbound}, Models: []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4", Source: "direct"}}}}}
			protection := biz.NewPromptProtectionRuleService(biz.PromptProtectionRuleServiceParams{CacheConfig: xcache.Config{Mode: xcache.ModeMemory}, Ent: client})
			defer protection.Stop()
			orch := orchestrator.NewChatCompletionOrchestrator(channelSvc, nil, requestSvc, httpclient.NewHttpClient(), openai.NewInboundTransformer(), systemSvc, usageSvc, nil, nil, protection, biz.NewLiveStreamRegistry(), orchestrator.NewChannelLimiterManager(), nil).WithChannelSelector(selector)
			h := &ChatCompletionHandlers{ChatCompletionOrchestrator: orch, sseKeepAlive: SSEKeepAliveConfig{Enabled: true, Interval: time.Millisecond}, sseHeartbeatFormat: sseHeartbeatOpenAI}
			w := &startupHeartbeatRecorder{ResponseRecorder: httptest.NewRecorder(), upstreamStarted: upstreamStarted, heartbeat: heartbeat}
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
			c.Request.Header.Set("Content-Type", "application/json")
			h.ChatCompletion(c)
			require.NoError(t, ctx.Err(), "provider was never released by a startup heartbeat")
			require.Equal(t, http.StatusOK, w.Code)
			require.Contains(t, w.Body.String(), ": keep-alive\n\n")
			records, err := client.Request.Query().All(ctx)
			require.NoError(t, err)
			require.Len(t, records, 1)
			if phase == "upstream-error" {
				require.Contains(t, w.Body.String(), "event:error")
				require.Contains(t, w.Body.String(), "rate_limit_exceeded")
				require.Equal(t, entrequest.StatusFailed, records[0].Status)
				executions, err := client.RequestExecution.Query().All(ctx)
				require.NoError(t, err)
				require.NotEmpty(t, executions)
				require.NotNil(t, executions[0].ResponseStatusCode)
				require.Equal(t, http.StatusTooManyRequests, *executions[0].ResponseStatusCode)
			} else {
				require.Contains(t, w.Body.String(), "upstream result")
				require.NotContains(t, w.Body.String(), "event:error")
				require.Equal(t, entrequest.StatusCompleted, records[0].Status)
			}
		})
	}
}
