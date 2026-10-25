package orchestrator

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/pipeline/stream"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

// These tests drive the real ChatCompletionOrchestrator pipeline so the client
// cancellation counter, the cooldown write, and the candidate-pool reload are all
// exercised end to end instead of calling the ChannelService helpers directly.

type cancellationTestEnv struct {
	ctx            context.Context
	client         *ent.Client
	project        *ent.Project
	channelService *biz.ChannelService
	systemService  *biz.SystemService
	orchestrator   *ChatCompletionOrchestrator
	channelIDs     []int
}

func createCancellationTestChannel(t *testing.T, ctx context.Context, client *ent.Client, name string, keys []string) *ent.Channel {
	t.Helper()

	row, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName(name).
		SetBaseURL("https://api.openai.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKeys: keys}).
		SetSupportedModels([]string{"gpt-4", "gpt-4o"}).
		SetDefaultTestModel("gpt-4").
		SetStatus(channel.StatusEnabled).
		Save(ctx)
	require.NoError(t, err)

	return row
}

// cancelAfterFirstTokenExecutor streams one content chunk (so the performance
// record sees first-token evidence) and then aborts with context.Canceled,
// mirroring "the client hung up after generation had already started".
func cancelAfterFirstTokenExecutor() *mockExecutorWithErrorStream {
	return &mockExecutorWithErrorStream{
		events: []*httpclient.StreamEvent{
			{Data: []byte(`{"id":"chatcmpl-cancel","object":"chat.completion.chunk","model":"gpt-4","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}]}`)},
		},
		streamErr: context.Canceled,
	}
}
func newCancellationTestEnv(t *testing.T, executor pipeline.Executor, keys []string) *cancellationTestEnv {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	t.Cleanup(func() { _ = client.Close() })

	ctx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
	project := createTestProject(t, ctx, client)

	rowA := createCancellationTestChannel(t, ctx, client, "cancel-channel-a", keys)
	rowB := createCancellationTestChannel(t, ctx, client, "cancel-channel-b", []string{"other-key"})

	systemService := newTestSystemService(client)
	channelService := biz.NewChannelService(biz.ChannelServiceParams{
		CacheConfig:     xcache.Config{Mode: xcache.ModeMemory},
		Ent:             client,
		SystemService:   systemService,
		HttpClient:      httpclient.NewHttpClient(),
		WebhookNotifier: biz.NewWebhookNotifier(systemService, httpclient.NewHttpClient()),
	})
	dataStorageService := &biz.DataStorageService{
		AbstractService: &biz.AbstractService{},
		SystemService:   systemService,
		Cache:           xcache.NewFromConfig[ent.DataStorage](xcache.Config{Mode: xcache.ModeMemory}),
	}
	usageLogService := biz.NewUsageLogService(client, systemService, channelService)
	requestService := biz.NewRequestService(client, systemService.CacheConfig, systemService, usageLogService, dataStorageService, biz.NewLiveStreamRegistry())

	require.NotEmpty(t, keys)
	outboundA, err := openai.NewOutboundTransformer(rowA.BaseURL, keys[0])
	require.NoError(t, err)
	outboundB, err := openai.NewOutboundTransformer(rowB.BaseURL, "other-key")
	require.NoError(t, err)

	candidates := channelsToTestCandidates([]*biz.Channel{
		{Channel: rowA, Outbound: outboundA},
		{Channel: rowB, Outbound: outboundB},
	}, "gpt-4")
	require.Len(t, candidates, 2)

	orchestrator := &ChatCompletionOrchestrator{
		channelSelector:       &staticChannelSelector{candidates: candidates},
		Inbound:               openai.NewInboundTransformer(),
		RequestService:        requestService,
		ChannelService:        channelService,
		PromptProvider:        &stubPromptProvider{},
		SystemService:         systemService,
		UsageLogService:       usageLogService,
		PipelineFactory:       pipeline.NewFactory(executor),
		ModelMapper:           NewModelMapper(),
		channelLimiterManager: NewChannelLimiterManager(),
		Middlewares:           []pipeline.Middleware{stream.EnsureUsage()},
	}

	return &cancellationTestEnv{
		ctx:            ctx,
		client:         client,
		project:        project,
		channelService: channelService,
		systemService:  systemService,
		orchestrator:   orchestrator,
		channelIDs:     []int{rowA.ID, rowB.ID},
	}
}

func (e *cancellationTestEnv) useExecutor(executor pipeline.Executor) {
	e.orchestrator.PipelineFactory = pipeline.NewFactory(executor)
}

func (e *cancellationTestEnv) setPolicy(t *testing.T, policy biz.ClientCancellationPolicy) {
	t.Helper()

	require.NoError(t, e.systemService.SetRetryPolicy(e.ctx, &biz.RetryPolicy{
		Enabled:                  true,
		MaxChannelRetries:        1,
		MaxSingleChannelRetries:  0,
		RetryDelayMs:             0,
		ClientCancellationPolicy: policy,
	}))
}

func (e *cancellationTestEnv) process(projectID int, apiKey string) error {
	ctx := contexts.WithProjectID(e.ctx, projectID)
	if apiKey != "" {
		ctx = contexts.WithChannelAPIKey(ctx, apiKey)
	}

	result, err := e.orchestrator.Process(ctx, buildTestRequest("gpt-4", "hello", true))
	if err != nil {
		return err
	}

	// Drain the event stream so the pipeline finalizes and records performance;
	// a real HTTP writer would consume it the same way.
	if result.ChatCompletionStream != nil {
		for result.ChatCompletionStream.Next() {
		}
		_ = result.ChatCompletionStream.Err()
		_ = result.ChatCompletionStream.Close()
	}

	return nil
}

func (e *cancellationTestEnv) getChannel(t *testing.T, id int) *ent.Channel {
	t.Helper()

	row, err := e.client.Channel.Get(e.ctx, id)
	require.NoError(t, err)

	return row
}

// waitForModelCooldown observes the async performance worker's model-scoped
// write. A channel remaining enabled alone is not evidence of a cooldown.
func (e *cancellationTestEnv) waitForModelCooldown(t *testing.T, before *ent.Channel, model string, minutes int) *ent.Channel {
	t.Helper()

	var row *ent.Channel
	var freeze objects.ModelCooldown
	require.Eventually(t, func() bool {
		current, err := e.client.Channel.Get(e.ctx, before.ID)
		if err != nil {
			return false
		}
		for _, cooldown := range current.ModelCooldowns {
			if cooldown.Model == model && cooldown.IsActive() {
				row, freeze = current, cooldown
				return true
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond)

	require.Equal(t, channel.StatusEnabled, row.Status)
	require.Equal(t, before.Credentials, row.Credentials, "cancellations must not delete or replace keys")
	require.Equal(t, before.DisabledAPIKeys, row.DisabledAPIKeys, "cancellations must not disable keys")
	require.Nil(t, row.AutoDisabledAt)
	require.Nil(t, row.AutoDisableExpiresAt)
	require.Nil(t, row.ErrorMessage)
	require.Len(t, row.ModelCooldowns, 1)
	require.NotNil(t, freeze.ExpiresAt, "client cancellation freezes must be temporary")
	require.WithinDuration(t, time.Now().Add(time.Duration(minutes)*time.Minute), *freeze.ExpiresAt, 2*time.Second)
	require.Contains(t, freeze.Reason, "Client cancellation cooldown")

	return row
}

func (e *cancellationTestEnv) requireModelOnlyCandidateSelection(t *testing.T, frozenChannelID int) {
	t.Helper()

	// The serving cache must retain both enabled channels and receive the model
	// freeze written by the worker; channel disappearance is never acceptable.
	require.Eventually(t, func() bool {
		for _, ch := range e.channelService.GetEnabledChannels() {
			if ch.ID == frozenChannelID {
				return ch.HasActiveModelCooldown("gpt-4", time.Now())
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond)

	enabledIDs := make([]int, 0, len(e.channelIDs))
	for _, ch := range e.channelService.GetEnabledChannels() {
		require.Equal(t, channel.StatusEnabled, ch.Status)
		enabledIDs = append(enabledIDs, ch.ID)
	}
	require.ElementsMatch(t, e.channelIDs, enabledIDs)

	// Exercise the real default selector's channel-model candidate path against
	// that refreshed cache, not the static selector used to pin the canceling
	// stream to its original channel.
	selector := &DefaultSelector{ChannelService: e.channelService}
	sameModel, err := selector.selectChannelCadidates(e.ctx, &llm.Request{Model: "gpt-4"})
	require.NoError(t, err)
	require.Len(t, sameModel, 1)
	require.Equal(t, e.channelIDs[1], sameModel[0].Channel.ID)
	require.NotEqual(t, frozenChannelID, sameModel[0].Channel.ID)

	otherModel, err := selector.selectChannelCadidates(e.ctx, &llm.Request{Model: "gpt-4o"})
	require.NoError(t, err)
	otherModelIDs := make([]int, 0, len(otherModel))
	for _, candidate := range otherModel {
		otherModelIDs = append(otherModelIDs, candidate.Channel.ID)
	}
	require.ElementsMatch(t, e.channelIDs, otherModelIDs, "other models must still use the frozen model's channel")
}

func cancellationSwitchAndCooldownPolicy(threshold, cooldownMinutes int) biz.ClientCancellationPolicy {
	return biz.ClientCancellationPolicy{
		Enabled:         true,
		Threshold:       threshold,
		CooldownMinutes: cooldownMinutes,
		Action:          biz.ClientCancellationActionSwitchAndCooldown,
		Scope:           biz.ClientCancellationScopeGlobal,
	}
}

func TestClientCancellationCooldown_ThresholdFreezesOnlyModel(t *testing.T) {
	env := newCancellationTestEnv(t, cancelAfterFirstTokenExecutor(), []string{"key-1"})
	env.setPolicy(t, cancellationSwitchAndCooldownPolicy(2, 3))

	channelID := env.channelIDs[0]
	before := env.getChannel(t, channelID)

	// First cancellation only increments the independent window. The mock
	// stream emits content before context.Canceled, preserving first-token
	// evidence; draining it in process finalizes the performance record.
	require.NoError(t, env.process(env.project.ID, "key-1"))
	time.Sleep(200 * time.Millisecond)
	first := env.getChannel(t, channelID)
	require.Equal(t, channel.StatusEnabled, first.Status)
	require.Empty(t, first.ModelCooldowns)
	require.Equal(t, before.Credentials, first.Credentials)
	require.Empty(t, first.DisabledAPIKeys)

	// The second eligible cancellation freezes only the requested model.
	require.NoError(t, env.process(env.project.ID, "key-1"))
	frozen := env.waitForModelCooldown(t, before, "gpt-4", 3)
	require.Contains(t, frozen.ModelCooldowns[0].Reason, "2 cancellations")
	env.requireModelOnlyCandidateSelection(t, channelID)
}

func TestClientCancellationCooldown_SuccessPreservesWindow(t *testing.T) {
	env := newCancellationTestEnv(t, cancelAfterFirstTokenExecutor(), []string{"key-1"})
	env.setPolicy(t, cancellationSwitchAndCooldownPolicy(2, 3))

	channelID := env.channelIDs[0]
	before := env.getChannel(t, channelID)

	require.NoError(t, env.process(env.project.ID, "key-1"))
	time.Sleep(200 * time.Millisecond)
	require.Empty(t, env.getChannel(t, channelID).ModelCooldowns)

	// A successful request does not erase unexpired cancellations. Streaming success needs a
	// full chunk sequence ending with finish_reason so the aggregator completes.
	env.useExecutor(&mockExecutor{streamEvents: []*httpclient.StreamEvent{
		{Data: []byte(`{"id":"chatcmpl-ok","object":"chat.completion.chunk","model":"gpt-4","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`)},
		{Data: []byte(`{"id":"chatcmpl-ok","object":"chat.completion.chunk","model":"gpt-4","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`)},
		{Data: []byte(`{"id":"chatcmpl-ok","object":"chat.completion.chunk","model":"gpt-4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)},
	}})
	require.NoError(t, env.process(env.project.ID, "key-1"))

	// One more eligible cancellation reaches two within the rolling window,
	// despite the success between them.
	env.useExecutor(cancelAfterFirstTokenExecutor())
	require.NoError(t, env.process(env.project.ID, "key-1"))
	frozen := env.waitForModelCooldown(t, before, "gpt-4", 3)
	require.Contains(t, frozen.ModelCooldowns[0].Reason, "2 cancellations")
	env.requireModelOnlyCandidateSelection(t, channelID)
}

func TestClientCancellationCooldown_LegacyCooldownOnlyPreservesCredentials(t *testing.T) {
	env := newCancellationTestEnv(t, cancelAfterFirstTokenExecutor(), []string{"key-1", "key-2"})
	env.setPolicy(t, biz.ClientCancellationPolicy{
		Enabled:         true,
		Threshold:       1,
		CooldownMinutes: 5,
		Action:          biz.ClientCancellationActionCooldownOnly,
		Scope:           biz.ClientCancellationScopeGlobal,
	})

	channelID := env.channelIDs[0]
	before := env.getChannel(t, channelID)
	require.NoError(t, env.process(env.project.ID, "key-1"))
	frozen := env.waitForModelCooldown(t, before, "gpt-4", 5)
	require.Equal(t, []string{"key-1", "key-2"}, frozen.Credentials.APIKeys)
	require.Empty(t, frozen.DisabledAPIKeys)
	env.requireModelOnlyCandidateSelection(t, channelID)
}

func TestClientCancellationCooldown_ProjectFirstOverride(t *testing.T) {
	env := newCancellationTestEnv(t, cancelAfterFirstTokenExecutor(), []string{"key-1"})
	env.setPolicy(t, biz.ClientCancellationPolicy{
		Enabled:         true,
		Threshold:       5,
		CooldownMinutes: 30,
		Action:          biz.ClientCancellationActionSwitchAndCooldown,
		Scope:           biz.ClientCancellationScopeProjectFirst,
		ProjectOverrides: []biz.ClientCancellationProjectPolicy{
			{ProjectID: 42, Enabled: true, Threshold: 1, CooldownMinutes: 1},
		},
	})

	channelID := env.channelIDs[0]
	before := env.getChannel(t, channelID)

	// A project without an override keeps the global threshold of 5.
	require.NoError(t, env.process(env.project.ID, "key-1"))
	time.Sleep(200 * time.Millisecond)
	first := env.getChannel(t, channelID)
	require.Equal(t, channel.StatusEnabled, first.Status)
	require.Empty(t, first.ModelCooldowns)

	// The overriding project freezes the model on its first eligible cancel,
	// using the override's one-minute duration instead of the global 30.
	require.NoError(t, env.process(42, "key-1"))
	frozen := env.waitForModelCooldown(t, before, "gpt-4", 1)
	require.Contains(t, frozen.ModelCooldowns[0].Reason, "1 cancellations")
	env.requireModelOnlyCandidateSelection(t, channelID)
}

func TestClientCancellationCooldown_StatusCodeFailuresAreNotCancellations(t *testing.T) {
	failing := &mockExecutor{err: &httpclient.Error{
		Method:     http.MethodPost,
		URL:        "https://api.openai.com/v1/chat/completions",
		StatusCode: http.StatusInternalServerError,
		Status:     "500 Internal Server Error",
		Body:       []byte(`{"error":"boom"}`),
	}}
	env := newCancellationTestEnv(t, failing, []string{"key-1"})
	// A threshold of one makes any wrongly counted failure immediately visible.
	env.setPolicy(t, cancellationSwitchAndCooldownPolicy(1, 3))

	channelID := env.channelIDs[0]

	require.Error(t, env.process(env.project.ID, "key-1"))
	time.Sleep(300 * time.Millisecond)

	failed := env.getChannel(t, channelID)
	require.Equal(t, channel.StatusEnabled, failed.Status)
	require.Empty(t, failed.ModelCooldowns)
	require.Empty(t, failed.DisabledAPIKeys)
	// A 500 must still fail over to the next channel: the cancellation guard only
	// covers client cancellations.
	require.GreaterOrEqual(t, failing.requestCalls.Load(), int64(2))

	// A pre-first-token abort must also leave the cancellation window untouched;
	// it has no upstream-output evidence and cannot freeze a model.
	env.setPolicy(t, cancellationSwitchAndCooldownPolicy(2, 3))
	env.useExecutor(&mockExecutor{err: context.Canceled})
	require.Error(t, env.process(env.project.ID, "key-1"))
	time.Sleep(300 * time.Millisecond)

	aborted := env.getChannel(t, channelID)
	require.Equal(t, channel.StatusEnabled, aborted.Status)
	require.Empty(t, aborted.ModelCooldowns)
	require.Empty(t, aborted.DisabledAPIKeys)
}
