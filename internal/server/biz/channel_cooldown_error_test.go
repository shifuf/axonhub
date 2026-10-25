package biz

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/objects"
)

func TestChannelService_ModelFreezeReasonIncludesSafeUpstreamError(t *testing.T) {
	tests := []struct {
		name, message    string
		status           int
		contains, absent []string
	}{
		{name: "provider message and status", status: 503, message: "No available upstream channel for this model\n{\"error\":{\"message\":\"No available upstream channel for this model\",\"apiKey\":\"key1\"}}",
			contains: []string{"HTTP 503", "No available upstream channel for this model", "within 10 minutes", "auto-recover after 15 minutes"}, absent: []string{"key1", `"apiKey"`}},
		{name: "empty primary message does not persist raw body", status: 503, message: "  \n{\"privatePayload\":\"raw-body-must-not-be-persisted\"}",
			contains: []string{"HTTP 503", "Service Unavailable"}, absent: []string{"privatePayload", "raw-body-must-not-be-persisted"}},
		{name: "status fallback", status: 429, contains: []string{"HTTP 429", "Too Many Requests"}},
		{name: "credential redaction", status: 401, message: "invalid key1 Authorization: Bearer provider-secret-token",
			contains: []string{"HTTP 401", "invalid", "[REDACTED]"}, absent: []string{"key1", "provider-secret-token"}},
		{name: "URL credentials are redacted", status: 502, message: "GET https://provider.invalid?api_key=query-secret&access_token=access-secret failed",
			contains: []string{"HTTP 502", "failed", "[REDACTED]"}, absent: []string{"query-secret", "access-secret"}},
		{name: "bounded unicode message", status: 502, message: strings.Repeat("错误内容", 3000), contains: []string{"HTTP 502", "错误内容"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := objects.APIKeyAutoDisableRule{Times: 1, WindowMinutes: 10, FreezeModel: true,
				Action: objects.APIKeyAutoDisableActionTemporary, DisableDurationMinutes: lo.ToPtr(15)}
			ctx, svc, ch := newWindowRuleTestService(t, rule)
			_, acted := svc.evaluateAutoDisableForFailure(ctx, &PerformanceRecord{ChannelID: ch.ID, APIKey: "key1", Model: "gpt-4",
				ResponseStatusCode: tt.status, ErrorMessage: tt.message})
			require.True(t, acted)
			stored, err := svc.db.Channel.Get(ctx, ch.ID)
			require.NoError(t, err)
			require.Len(t, stored.ModelCooldowns, 1)
			reason := stored.ModelCooldowns[0].Reason
			for _, text := range tt.contains {
				require.Contains(t, reason, text)
			}
			for _, secret := range tt.absent {
				require.NotContains(t, reason, secret)
			}
			require.True(t, utf8.ValidString(reason))
			require.LessOrEqual(t, len(reason), 2300, fmt.Sprintf("reason length: %d", len(reason)))
		})
	}
}
