package orchestrator

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

// RetrySafe reports whether a failed attempt may be replayed on the same
// channel (including another API key) or on another channel.
//
// The safety matrix mirrors Meta-Gateway's proxy contract:
//
//	path                 default  with Idempotency-Key
//	images/*             no       no
//	audio/*              no       yes
//	videos/*, video/*    no       yes
//	music/*              no       yes
//	responses*           no       yes
//	everything else      yes      yes
//
// Images stay non-replayable even with an Idempotency-Key because the key may
// be scoped to one upstream and cannot deduplicate across channels. Read-like
// methods remain retryable regardless of path.
func (p *PersistentOutboundTransformer) RetrySafe() bool {
	if p == nil || p.state == nil {
		return true
	}

	request := p.state.RawRequest
	if request == nil && p.state.LlmRequest != nil {
		request = p.state.LlmRequest.RawRequest
	}

	method := ""
	path := ""
	headers := http.Header(nil)
	requestType := llm.RequestType("")
	apiFormat := llm.APIFormat("")

	if request != nil {
		method = strings.ToUpper(strings.TrimSpace(request.Method))
		path = requestPath(request)
		headers = request.Headers
		requestType = llm.RequestType(request.RequestType)
		apiFormat = llm.APIFormat(request.APIFormat)
	}

	if p.state.LlmRequest != nil {
		if requestType == "" {
			requestType = p.state.LlmRequest.RequestType
		}
		if apiFormat == "" {
			apiFormat = p.state.LlmRequest.APIFormat
		}
		if path == "" && p.state.LlmRequest.RawRequest != nil {
			path = requestPath(p.state.LlmRequest.RawRequest)
		}
	}

	if method != http.MethodPost && method != http.MethodPut && method != http.MethodPatch {
		return true
	}

	normalizedPath := normalizeRetryPath(path)

	// Image generation/edit/variation is billed on acceptance. Never replay it.
	if requestType == llm.RequestTypeImage ||
		isImageAPIFormat(apiFormat) ||
		strings.HasPrefix(normalizedPath, "images/") ||
		normalizedPath == "images" {
		return false
	}

	if hasIdempotencyKey(headers) {
		return true
	}

	if requestType == llm.RequestTypeSpeech ||
		requestType == llm.RequestTypeTranscription ||
		requestType == llm.RequestTypeTranslation ||
		requestType == llm.RequestTypeVideo {
		return false
	}

	if isArtifactAPIFormat(apiFormat) {
		return false
	}

	if strings.HasPrefix(normalizedPath, "audio/") ||
		hasRetryPathPrefix(normalizedPath, "videos/", "video/", "music/") {
		return false
	}

	return normalizedPath != "responses" && !strings.HasPrefix(normalizedPath, "responses/")
}

func requestPath(request *httpclient.Request) string {
	if request == nil {
		return ""
	}
	if path := strings.TrimSpace(request.Path); path != "" {
		return path
	}
	if request.URL == "" {
		return ""
	}

	parsed, err := url.Parse(request.URL)
	if err != nil {
		return ""
	}

	return parsed.Path
}

func normalizeRetryPath(path string) string {
	path = strings.ToLower(strings.TrimSpace(path))
	path = strings.TrimPrefix(path, "/")

	for strings.HasPrefix(path, "v1/") {
		path = strings.TrimPrefix(path, "v1/")
	}

	return path
}

func hasRetryPathPrefix(path string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}

	return false
}

func hasIdempotencyKey(headers http.Header) bool {
	for key, values := range headers {
		if !strings.EqualFold(key, "Idempotency-Key") {
			continue
		}

		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				return true
			}
		}
	}

	return false
}

func isImageAPIFormat(format llm.APIFormat) bool {
	switch format {
	case llm.APIFormatOpenAIImageGeneration,
		llm.APIFormatOpenAIImageEdit,
		llm.APIFormatOpenAIImageVariation,
		llm.APIFormatModelScopeImage:
		return true
	default:
		return false
	}
}

func isArtifactAPIFormat(format llm.APIFormat) bool {
	switch format {
	case llm.APIFormatOpenAISpeech,
		llm.APIFormatOpenAITranscription,
		llm.APIFormatOpenAITranslation,
		llm.APIFormatOpenAIVideo,
		llm.APIFormatOpenAIResponse,
		llm.APIFormatOpenAIResponseWebSocket,
		llm.APIFormatOpenAIResponseCompact,
		llm.APIFormatSeedanceVideo,
		llm.APIFormatZenmuxVideo:
		return true
	default:
		return false
	}
}
