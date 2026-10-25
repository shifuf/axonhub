package orchestrator

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

// Exercise model association selection and the real retry loop together. An
// enabled channel is not necessarily eligible for the requested model: disabled
// channels, model freezes and unassociated channels must not receive attempts.
func TestChatCompletionOrchestrator_Process_FailoverCandidateEligibility(t *testing.T) {
	const requestedModel = "retry-model"
	expired := time.Now().Add(-time.Hour)

	for _, tt := range []struct {
		name                 string
		disabledRetries      bool
		maxChannelRetries    int
		sameChannelRetries   int
		firstChannelCooldown []objects.ModelCooldown
		disableSecondChannel bool
		failures             int
		freezeOnFirstRequest []int
		wantCandidates       []int
		wantAttempts         []int
		wantError            bool
	}{
		{
			name: "exhausts all four eligible channels", maxChannelRetries: 6, failures: 4,
			wantCandidates: []int{0, 1, 2, 3}, wantAttempts: []int{0, 1, 2, 3}, wantError: true,
		},
		{
			name: "recovers on third eligible channel", maxChannelRetries: 6, failures: 2,
			wantCandidates: []int{0, 1, 2, 3}, wantAttempts: []int{0, 1, 2},
		},
		{
			name: "recovers on fourth eligible channel", maxChannelRetries: 6, failures: 3,
			wantCandidates: []int{0, 1, 2, 3}, wantAttempts: []int{0, 1, 2, 3},
		},
		{
			name: "one configured channel switch permits only two attempts", maxChannelRetries: 1, failures: 2,
			wantCandidates: []int{0, 1}, wantAttempts: []int{0, 1}, wantError: true,
		},
		{
			name: "zero channel switches permits only the initial attempt", failures: 1,
			wantCandidates: []int{0}, wantAttempts: []int{0}, wantError: true,
		},
		{
			name: "disabled retry policy permits only the initial attempt", disabledRetries: true, maxChannelRetries: 6, failures: 1,
			wantCandidates: []int{0}, wantAttempts: []int{0}, wantError: true,
		},
		{
			name: "model freeze and disabled channel leave only two eligible candidates", maxChannelRetries: 6, failures: 2,
			firstChannelCooldown: []objects.ModelCooldown{{Model: requestedModel, Reason: "Manually frozen by user"}},
			disableSecondChannel: true,
			wantCandidates:       []int{2, 3}, wantAttempts: []int{2, 3}, wantError: true,
		},
		{
			name: "expired model freeze restores the candidate", maxChannelRetries: 6, failures: 4,
			firstChannelCooldown: []objects.ModelCooldown{{Model: requestedModel, ExpiresAt: &expired}},
			wantCandidates:       []int{0, 1, 2, 3}, wantAttempts: []int{0, 1, 2, 3}, wantError: true,
		},
		{
			name: "freeze on another model does not exhaust candidates", maxChannelRetries: 6, failures: 4,
			firstChannelCooldown: []objects.ModelCooldown{{Model: "another-model"}},
			wantCandidates:       []int{0, 1, 2, 3}, wantAttempts: []int{0, 1, 2, 3}, wantError: true,
		},
		{
			name: "same channel retry skips a newly frozen selection model", maxChannelRetries: 3, sameChannelRetries: 2, failures: 1,
			freezeOnFirstRequest: []int{0},
			wantCandidates:       []int{0, 1, 2, 3}, wantAttempts: []int{0, 1},
		},
		{
			name: "failover skips models frozen after candidate selection", maxChannelRetries: 3, failures: 1,
			freezeOnFirstRequest: []int{1, 2},
			wantCandidates:       []int{0, 1, 2, 3}, wantAttempts: []int{0, 3},
		},
		{
			name: "failover stops when all remaining candidates became frozen", maxChannelRetries: 3, failures: 1,
			freezeOnFirstRequest: []int{1, 2, 3},
			wantCandidates:       []int{0, 1, 2, 3}, wantAttempts: []int{0}, wantError: true,
		},
		{
			name: "same channel retry budget resets on every channel switch", maxChannelRetries: 3, sameChannelRetries: 1, failures: 8,
			wantCandidates: []int{0, 1, 2, 3}, wantAttempts: []int{0, 0, 1, 1, 2, 2, 3, 3}, wantError: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, client := setupTest(t)
			project := createTestProject(t, ctx, client)
			ctx = contexts.WithProjectID(ctx, project.ID)

			// Five channels support the upstream model, but only the first four
			// are associated with the public model. Never fall back to the fifth.
			channels := make([]*ent.Channel, 0, 5)
			associations := make([]*objects.ModelAssociation, 0, 4)
			for i := range 5 {
				builder := client.Channel.Create().
					SetType(channel.TypeOpenai).
					SetName(fmt.Sprintf("Retry channel %d", i)).
					SetBaseURL(fmt.Sprintf("https://retry-channel-%d.invalid/v1", i)).
					SetCredentials(objects.ChannelCredentials{APIKey: "test-key"}).
					SetSupportedModels([]string{"gpt-4"}).
					SetDefaultTestModel("gpt-4").
					SetStatus(channel.StatusEnabled)
				if i == 0 {
					builder.SetModelCooldowns(tt.firstChannelCooldown)
				}
				if i == 1 && tt.disableSecondChannel {
					builder.SetStatus(channel.StatusDisabled)
				}
				ch, err := builder.Save(ctx)
				require.NoError(t, err)
				channels = append(channels, ch)
				if i < 4 {
					associations = append(associations, &objects.ModelAssociation{
						Type: "channel_model", Priority: i,
						ChannelModel: &objects.ChannelModelAssociation{ChannelID: ch.ID, ModelID: "gpt-4"},
					})
				}
			}
			_, err := client.Model.Create().
				SetModelID(requestedModel).
				SetName("Retry model").
				SetDeveloper("openai").
				SetIcon("openai").
				SetGroup("test").
				SetModelCard(&objects.ModelCard{}).
				SetStatus("enabled").
				SetSettings(&objects.ModelSettings{Associations: associations}).
				Save(ctx)
			require.NoError(t, err)

			_, requestService, systemService, usageLogService := setupTestServices(t, client)
			require.NoError(t, systemService.SetRetryPolicy(ctx, &biz.RetryPolicy{
				Enabled:                 !tt.disabledRetries,
				MaxChannelRetries:       tt.maxChannelRetries,
				MaxSingleChannelRetries: tt.sameChannelRetries,
				LoadBalancerStrategy:    biz.LoadBalancerStrategyRoundRobin,
				TraceStickyMode:         biz.TraceStickyDisabled,
			}))
			channelService := newTestChannelServiceForChannels(client)
			t.Cleanup(channelService.Stop)
			selector := NewDefaultSelector(channelService, newTestModelService(client), systemService)
			loadBalancer := NewLoadBalancer(systemService, nil, NewRoundRobinStrategy(channelService))
			balanced := WithLoadBalancedSelector(selector, loadBalancer, systemService)
			candidates, err := balanced.Select(ctx, &llm.Request{Model: requestedModel})
			require.NoError(t, err)
			candidateIDs := lo.Map(candidates, func(c *ChannelModelsCandidate, _ int) int {
				// The executor below is the only transport; no upstream calls.
				require.Nil(t, c.Channel.HTTPClient)
				return c.Channel.ID
			})
			wantCandidateIDs := lo.Map(tt.wantCandidates, func(i int, _ int) int { return channels[i].ID })
			require.Equal(t, wantCandidateIDs, candidateIDs)

			executor := &sequenceExecutor{}
			for range tt.failures {
				executor.steps = append(executor.steps, executorStep{err: &httpclient.Error{
					StatusCode: http.StatusServiceUnavailable,
					Body:       []byte(`{"error":{"message":"upstream unavailable","type":"api_error"}}`),
				}})
			}
			// An extra success detects attempts beyond candidate or policy limits.
			executor.steps = append(executor.steps, executorStep{resp: &httpclient.Response{
				StatusCode: http.StatusOK,
				Body:       buildMockOpenAIResponse("chatcmpl-retry", "gpt-4", "Recovered", 10, 20),
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
			}})
			transport := &firstAttemptActionExecutor{Executor: executor, action: func(attemptCtx context.Context) {
				for _, i := range tt.freezeOnFirstRequest {
					_, err := channelService.FreezeChannelModel(attemptCtx, channels[i].ID, requestedModel, 30)
					require.NoError(t, err)
				}
			}}
			orchestrator := &ChatCompletionOrchestrator{
				channelSelector:        selector,
				roundRobinLoadBalancer: loadBalancer,
				Inbound:                openai.NewInboundTransformer(),
				RequestService:         requestService,
				ChannelService:         channelService,
				PromptProvider:         &stubPromptProvider{},
				SystemService:          systemService,
				UsageLogService:        usageLogService,
				PipelineFactory:        pipeline.NewFactory(transport),
				ModelMapper:            NewModelMapper(),
				channelLimiterManager:  NewChannelLimiterManager(),
			}
			result, err := orchestrator.Process(ctx, buildTestRequest(requestedModel, "Hello!", false))
			if tt.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.NotNil(t, result.ChatCompletion)
			}

			wantAttemptIDs := lo.Map(tt.wantAttempts, func(i int, _ int) int { return channels[i].ID })
			executions, err := client.RequestExecution.Query().Order(ent.Asc("id")).All(ctx)
			require.NoError(t, err)
			attemptIDs := lo.Map(executions, func(e *ent.RequestExecution, _ int) int { return e.ChannelID })
			require.Equal(t, wantAttemptIDs, attemptIDs, "persist every attempted channel in retry order")
			wantURLs := lo.Map(tt.wantAttempts, func(i int, _ int) string {
				return channels[i].BaseURL + "/chat/completions"
			})
			attemptURLs := lo.Map(executor.requests, func(r *httpclient.Request, _ int) string { return r.URL })
			require.Equal(t, wantURLs, attemptURLs, "send attempts only to eligible channels within the retry budget")
		})
	}
}

// Run a synchronous action after candidates have been pinned, just before the
// first upstream failure. Only the transport is replaced; selection and retry
// decisions still run through the production orchestrator.
type firstAttemptActionExecutor struct {
	pipeline.Executor
	action func(context.Context)
}

func (e *firstAttemptActionExecutor) Do(ctx context.Context, req *httpclient.Request) (*httpclient.Response, error) {
	if action := e.action; action != nil {
		e.action = nil
		action(ctx)
	}
	return e.Executor.Do(ctx, req)
}
