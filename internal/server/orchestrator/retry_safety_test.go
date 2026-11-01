package orchestrator

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestPersistentOutboundTransformer_RetrySafe(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		path    string
		format  llm.APIFormat
		headers http.Header
		safe    bool
	}{
		{name: "chat is safe", method: http.MethodPost, path: "/v1/chat/completions", safe: true},
		{name: "completion is safe", method: http.MethodPost, path: "/v1/completions", safe: true},
		{name: "embedding is safe", method: http.MethodPost, path: "/v1/embeddings", safe: true},
		{name: "image generation is never safe", method: http.MethodPost, path: "/v1/images/generations", safe: false},
		{name: "image edit is never safe", method: http.MethodPost, path: "/v1/images/edits", safe: false},
		{name: "image variation is never safe", method: http.MethodPost, path: "/v1/images/variations", safe: false},
		{name: "image with idempotency key is still unsafe", method: http.MethodPost, path: "/v1/images/generations", headers: http.Header{"Idempotency-Key": {"image-op"}}, safe: false},
		{name: "audio speech needs idempotency key", method: http.MethodPost, path: "/v1/audio/speech", safe: false},
		{name: "audio speech with idempotency key is safe", method: http.MethodPost, path: "/v1/audio/speech", headers: http.Header{"Idempotency-Key": {"audio-op"}}, safe: true},
		{name: "video generation needs idempotency key", method: http.MethodPost, path: "/v1/videos/generations", safe: false},
		{name: "video generation with idempotency key is safe", method: http.MethodPost, path: "/v1/videos/generations", headers: http.Header{"idempotency-key": {"video-op"}}, safe: true},
		{name: "music generation needs idempotency key", method: http.MethodPost, path: "/v1/music/generations", safe: false},
		{name: "responses need idempotency key", method: http.MethodPost, path: "/v1/responses", format: llm.APIFormatOpenAIResponse, safe: false},
		{name: "responses with idempotency key are safe", method: http.MethodPost, path: "/v1/responses", format: llm.APIFormatOpenAIResponse, headers: http.Header{"Idempotency-Key": {"response-op"}}, safe: true},
		{name: "image request type without a path is unsafe", method: http.MethodPost, format: llm.APIFormatOpenAIImageGeneration, safe: false},
		{name: "video request type without a path is unsafe", method: http.MethodPost, format: llm.APIFormatOpenAIVideo, safe: false},
		{name: "read requests are always safe", method: http.MethodGet, path: "/v1/images/generations", safe: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := &httpclient.Request{
				Method:  tt.method,
				Path:    tt.path,
				Headers: tt.headers,
			}
			if tt.format != "" {
				request.APIFormat = tt.format.String()
			}

			outbound := &PersistentOutboundTransformer{state: &PersistenceState{
				RawRequest: request,
				LlmRequest: &llm.Request{
					APIFormat:  tt.format,
					RawRequest: request,
				},
			}}

			require.Equal(t, tt.safe, outbound.RetrySafe())
		})
	}
}
