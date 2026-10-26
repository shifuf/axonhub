package biz

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aptible/supercronic/cronexpr"

	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcontext"
)

var compiledAPIKeyRuleRegexes sync.Map

type autoDisableScope string

const (
	autoDisableScopeChannel autoDisableScope = "channel"
	autoDisableScopeGlobal  autoDisableScope = "global"
)

const (
	defaultAutoDisableWindowMinutes   = 10
	defaultAutoDisableCooldownMinutes = 30
	// defaultPermanentModelFailureThreshold is the fallback safeguard threshold:
	// this many failures for one channel×model inside the configured rolling window
	// permanently freeze the model on that channel. Configurable through the
	// retry policy (AutoDisableChannel.PermanentFailureThreshold).
	defaultPermanentModelFailureThreshold     = 6
	maxPermanentModelFailureThreshold         = 1000
	defaultPermanentModelFailureWindowMinutes = 24 * 60
	maxPermanentModelFailureWindowMinutes     = 7 * 24 * 60
)

// The rolling failure window is persisted in the channel_failure_events table
// (see channel_failure_window.go). Rows are shared by every server instance, so
// a restart or a multi-instance deployment keeps counting the same failures.

// effectiveModelFreezeRule upgrades every legacy action/target in memory too,
// so old persisted settings cannot continue disabling or deleting credentials.
func effectiveModelFreezeRule(rule objects.APIKeyAutoDisableRule) objects.APIKeyAutoDisableRule {
	rule.Action = objects.APIKeyAutoDisableActionTemporary
	rule.FreezeModel = true
	rule.DisableUntilCron = ""
	rule.DisableUntilTimezone = ""
	if rule.WindowMinutes <= 0 {
		rule.WindowMinutes = defaultAutoDisableWindowMinutes
	}
	duration := defaultAutoDisableCooldownMinutes
	if rule.DisableDurationMinutes != nil && *rule.DisableDurationMinutes > 0 {
		duration = *rule.DisableDurationMinutes
	}
	rule.DisableDurationMinutes = &duration
	return rule
}

func (svc *ChannelService) markChannelUnavailable(
	ctx context.Context,
	channelID int,
	responseStatusCode int,
	threshold int,
	actualCount int,
	expiresAt *time.Time,
	reason string,
) bool {
	ctx, cancel := xcontext.DetachWithTimeout(ctx, 10*time.Second)
	defer cancel()

	if reason == "" {
		reason = deriveErrorMessage(responseStatusCode)
	}

	// Avoid repeatedly disabling the same channel. The sixth-failure guard
	// must also upgrade any legacy temporary channel disable to permanent.
	eligible := channel.StatusEQ(channel.StatusEnabled)
	if expiresAt == nil && threshold == defaultPermanentModelFailureThreshold && actualCount >= defaultPermanentModelFailureThreshold {
		eligible = channel.Or(eligible, channel.AutoDisableExpiresAtNotNil())
	}
	update := svc.db.Channel.Update().
		Where(channel.ID(channelID), eligible).
		SetStatus(channel.StatusDisabled).
		SetErrorMessage(reason).
		SetAutoDisabledAt(time.Now())
	if expiresAt != nil {
		update.SetAutoDisableExpiresAt(*expiresAt)
	} else {
		update.ClearAutoDisableExpiresAt()
	}

	affected, err := update.Save(ctx)
	if err != nil {
		log.Error(ctx, "Failed to disable channel on unrecoverable error",
			log.Int("channel_id", channelID),
			log.Int("error_code", responseStatusCode),
			log.Cause(err),
		)

		return false
	}

	if affected == 0 {
		log.Debug(ctx, "Channel already disabled, skipping",
			log.Int("channel_id", channelID),
			log.Int("error_code", responseStatusCode),
		)

		// Another instance may have already disabled the channel in DB while this
		// instance still serves it from a stale in-memory cache. Force a local
		// refresh so candidate selection stops using the channel immediately.
		if err := svc.enabledChannelsCache.Load(ctx, true); err != nil {
			log.Warn(ctx, "Failed to refresh local cache for already-disabled channel",
				log.Int("channel_id", channelID),
				log.Cause(err),
			)
		}

		return true
	}

	log.Warn(ctx, "Channel disabled due to unrecoverable error",
		log.Int("channel_id", channelID),
		log.Int("error_code", responseStatusCode),
	)

	// Fetch the updated channel for webhook notification
	updatedChannel, err := svc.db.Channel.Get(ctx, channelID)
	if err != nil {
		log.Error(ctx, "Failed to fetch disabled channel for webhook notification",
			log.Int("channel_id", channelID),
			log.Cause(err),
		)
	} else {
		svc.asyncNotifyChannelAutoDisabled(ctx, ChannelAutoDisabledEvent{
			ChannelID:       updatedChannel.ID,
			ChannelName:     updatedChannel.Name,
			ChannelProvider: updatedChannel.Type.String(),
			ChannelBaseURL:  updatedChannel.BaseURL,
			ChannelStatus:   updatedChannel.Status.String(),
			StatusCode:      responseStatusCode,
			Threshold:       threshold,
			ActualCount:     actualCount,
			Reason:          reason,
			OccurredAt:      time.Now(),
		})
	}

	// Synchronously reload the local cache to immediately stop selecting this channel.
	// This avoids the debounce delay that could keep the disabled channel in the candidate pool.
	if err := svc.enabledChannelsCache.Load(ctx, true); err != nil {
		log.Warn(ctx, "Failed to synchronously reload channels after auto-disable",
			log.Int("channel_id", channelID),
			log.Cause(err),
		)
	}

	// Also notify other instances via the watcher for cross-instance cache invalidation.
	svc.asyncReloadChannels()
	return true
}

func (svc *ChannelService) asyncNotifyChannelAutoDisabled(ctx context.Context, event ChannelAutoDisabledEvent) {
	notifyCtx := context.WithoutCancel(ctx)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error(notifyCtx, "channel auto-disabled webhook notification panicked", log.Any("panic", r))
			}
		}()

		svc.WebhookNotifier.NotifyChannelAutoDisabled(notifyCtx, event)
	}()
}

// EvaluateAPIKeyRulesForFailure retains its historical name for callers that
// persist failures outside performance middleware. The original request model
// is required for actions; credentials are never the automatic action target.
func (svc *ChannelService) EvaluateAPIKeyRulesForFailure(
	ctx context.Context,
	channelID int,
	apiKey string,
	responseStatusCode int,
	errorMessage string,
	requestModel ...string,
) bool {
	if channelID == 0 {
		return false
	}

	model := ""
	if len(requestModel) > 0 {
		model = requestModel[0]
	}
	_, acted := svc.evaluateAutoDisableForFailure(ctx, &PerformanceRecord{
		ChannelID:          channelID,
		APIKey:             apiKey,
		ResponseStatusCode: responseStatusCode,
		ErrorMessage:       errorMessage,
		Model:              model,
	})
	return acted
}

func (svc *ChannelService) evaluateAutoDisableForFailure(ctx context.Context, perf *PerformanceRecord) (matched, acted bool) {
	ch := svc.GetEnabledChannel(perf.ChannelID)
	if ch == nil {
		return false, false
	}

	mode := ch.Policies.EffectiveAutoDisableMode()
	if mode == objects.APIKeyAutoDisableModeOff {
		return false, false
	}

	// Channel-scope and global-scope rules contribute to the same channel×model
	// permanent-freeze window, whose threshold and duration come from retry policy.
	policy := svc.SystemService.RetryPolicyOrDefault(ctx)
	threshold := policy.AutoDisableChannel.PermanentFailureThreshold
	if threshold <= 0 {
		threshold = defaultPermanentModelFailureThreshold
	}
	windowMinutes := policy.AutoDisableChannel.PermanentFailureWindowMinutes
	if windowMinutes <= 0 {
		windowMinutes = defaultPermanentModelFailureWindowMinutes
	}

	if mode == objects.APIKeyAutoDisableModeCustom {
		matched, acted = svc.evaluateRules(ctx, perf, ch.Policies.APIKeyAutoDisableRules, autoDisableScopeChannel, threshold, windowMinutes)
		if matched {
			return matched, acted
		}
	}

	if !policy.AutoDisableChannel.Enabled || len(policy.AutoDisableChannel.Rules) == 0 {
		return false, false
	}

	return svc.evaluateRules(ctx, perf, policy.AutoDisableChannel.Rules, autoDisableScopeGlobal, threshold, windowMinutes)
}

// checkAndHandleChannelAPIKeyRules evaluates the channel's own rules only. Tests
// and internal callers that already know they want the channel layer use this.
func (svc *ChannelService) checkAndHandleChannelAPIKeyRules(ctx context.Context, perf *PerformanceRecord) (matched, acted bool) {
	ch := svc.GetEnabledChannel(perf.ChannelID)
	if ch == nil {
		return false, false
	}

	policy := svc.SystemService.RetryPolicyOrDefault(ctx).AutoDisableChannel
	threshold := policy.PermanentFailureThreshold
	if threshold <= 0 {
		threshold = defaultPermanentModelFailureThreshold
	}
	windowMinutes := policy.PermanentFailureWindowMinutes
	if windowMinutes <= 0 {
		windowMinutes = defaultPermanentModelFailureWindowMinutes
	}
	return svc.evaluateRules(ctx, perf, ch.Policies.APIKeyAutoDisableRules, autoDisableScopeChannel, threshold, windowMinutes)
}

// evaluateRules counts the first matching rule once per failure. The action
// target is always the original request model, regardless of legacy flags.
func (svc *ChannelService) evaluateRules(ctx context.Context, perf *PerformanceRecord, rules []objects.APIKeyAutoDisableRule, scope autoDisableScope, permanentThreshold, permanentWindowMinutes int) (matched, acted bool) {
	for ruleIndex, storedRule := range rules {
		if !matchesAPIKeyRule(storedRule, perf) {
			continue
		}
		if strings.TrimSpace(perf.Model) == "" {
			return true, false
		}
		rule := effectiveModelFreezeRule(storedRule)
		ruleKey := apiKeyRuleCounterKey(autoDisableRuleIdentity(perf, rule), scope, ruleIndex, rule)
		return true, svc.evaluateWindowedRule(ctx, perf, rule, ruleKey, permanentThreshold, permanentWindowMinutes)
	}
	return false, false
}

// A successful request resets the failure streak for its exact channel/model
// pair. The reset is persisted so delayed failures and other server instances
// observe the same boundary.
func (svc *ChannelService) clearAutoDisableCountsOnSuccess(ctx context.Context, perf *PerformanceRecord) {
	if perf == nil || !perf.Success {
		return
	}
	if err := svc.recordChannelModelSuccessEvent(ctx, perf); err != nil {
		log.Error(ctx, "failed to persist channel model success reset", log.Cause(err), log.Int("channel_id", perf.ChannelID))
	}
}

func autoDisableRuleIdentity(perf *PerformanceRecord, _ objects.APIKeyAutoDisableRule) string {
	return fmt.Sprintf("channel-model:%d:%q", perf.ChannelID, perf.Model)
}

func apiKeyRuleCounterKey(identity string, scope autoDisableScope, ruleIndex int, rule objects.APIKeyAutoDisableRule) string {
	disableDurationMinutes := 0
	if rule.DisableDurationMinutes != nil {
		disableDurationMinutes = *rule.DisableDurationMinutes
	}

	return fmt.Sprintf(
		"%s:%s:rule:%d:%v:%v:%d:%s:%d:%d:%v:%q:%q",
		identity,
		scope,
		ruleIndex,
		rule.StatusCodes,
		rule.KeywordPatterns,
		rule.Times,
		rule.Action,
		disableDurationMinutes,
		rule.WindowMinutes,
		rule.FreezeModel,
		rule.DisableUntilCron,
		rule.DisableUntilTimezone,
	)
}

func matchesAPIKeyRule(rule objects.APIKeyAutoDisableRule, perf *PerformanceRecord) bool {
	if len(rule.StatusCodes) > 0 && !slices.Contains(rule.StatusCodes, perf.ResponseStatusCode) {
		return false
	}
	if len(rule.KeywordPatterns) == 0 {
		return true
	}
	if perf.ErrorMessage == "" {
		return false
	}

	lowerMessage := strings.ToLower(perf.ErrorMessage)
	for _, pattern := range rule.KeywordPatterns {
		if re := compiledAPIKeyRuleRegex(pattern); re != nil {
			if re.MatchString(perf.ErrorMessage) {
				return true
			}
			continue
		}
		// Patterns may be plain keywords or regular expressions. Treat syntax
		// that is not a valid expression as a case-insensitive literal keyword.
		if strings.Contains(lowerMessage, strings.ToLower(pattern)) {
			return true
		}
	}

	return false
}

func compiledAPIKeyRuleRegex(pattern string) *regexp.Regexp {
	cacheKey := "(?i)" + pattern
	if cached, ok := compiledAPIKeyRuleRegexes.Load(cacheKey); ok {
		re, _ := cached.(*regexp.Regexp)
		return re
	}

	re, err := regexp.Compile(cacheKey)
	if err != nil {
		compiledAPIKeyRuleRegexes.Store(cacheKey, (*regexp.Regexp)(nil))
		return nil
	}
	compiledAPIKeyRuleRegexes.Store(cacheKey, re)
	return re
}

// nextAPIKeyRuleCronOccurrence resolves when a disable_until_cron rule lets the
// credential back in: the first cron occurrence strictly after the failure. The
// absolute instant is stored on the disable record, so recovery does not depend
// on any instance re-evaluating the expression later.
func nextAPIKeyRuleCronOccurrence(rule objects.APIKeyAutoDisableRule, now time.Time) (time.Time, error) {
	loc := time.UTC

	if rule.DisableUntilTimezone != "" {
		parsed, err := time.LoadLocation(rule.DisableUntilTimezone)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid timezone %q: %w", rule.DisableUntilTimezone, err)
		}

		loc = parsed
	}

	expr, err := cronexpr.Parse(rule.DisableUntilCron)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid cron expression %q: %w", rule.DisableUntilCron, err)
	}

	next := expr.Next(now.In(loc))
	if next.IsZero() {
		return time.Time{}, fmt.Errorf("cron expression %q never fires again", rule.DisableUntilCron)
	}

	return next, nil
}

func (svc *ChannelService) executeMatchedRuleAction(ctx context.Context, perf *PerformanceRecord, rule objects.APIKeyAutoDisableRule, count int) bool {
	return svc.executeModelRuleAction(ctx, perf, effectiveModelFreezeRule(rule), count)
}

// autoDisableFailureSummary describes the effective rolling model window.
func autoDisableFailureSummary(rule objects.APIKeyAutoDisableRule, count int) string {
	return fmt.Sprintf("%d failures within %d minutes", count, effectiveModelFreezeRule(rule).WindowMinutes)
}

var autoDisableBearerSecret = regexp.MustCompile(`(?i)(bearer\s+)[a-zA-Z0-9._~+/=\-]+`)
var autoDisableNamedSecret = regexp.MustCompile(`(?i)((?:api[-_]?key|access[-_]?token|refresh[-_]?token|authorization)["']?\s*[:=]\s*["']?)[^"'&\s,}]+`)

// autoDisableErrorDetail persists the diagnostic message, not the raw response
// body appended for keyword matching. Providers sometimes echo credentials, so
// remove the selected key and common token forms before exposing the reason.
func autoDisableErrorDetail(perf *PerformanceRecord) string {
	message, _, _ := strings.Cut(perf.ErrorMessage, "\n")
	message = strings.TrimSpace(message)
	if message == "" {
		message = deriveErrorMessage(perf.ResponseStatusCode)
	}
	if perf.APIKey != "" && perf.APIKey != objects.OAuthCredentialRef {
		message = strings.ReplaceAll(message, perf.APIKey, "[REDACTED]")
	}
	message = autoDisableBearerSecret.ReplaceAllString(message, "${1}[REDACTED]")
	message = autoDisableNamedSecret.ReplaceAllString(message, "${1}[REDACTED]")
	message = strings.ToValidUTF8(message, "�")
	const maxErrorBytes = 2048
	if len(message) > maxErrorBytes {
		message = message[:maxErrorBytes]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
		message += "..."
	}
	if perf.ResponseStatusCode > 0 {
		return fmt.Sprintf("HTTP %d: %s", perf.ResponseStatusCode, message)
	}
	return message
}

func (svc *ChannelService) executeModelRuleAction(ctx context.Context, perf *PerformanceRecord, rule objects.APIKeyAutoDisableRule, count int) bool {
	return svc.executeModelRuleActionDetail(ctx, perf, rule, count, autoDisableErrorDetail(perf))
}

// executeModelRuleActionDetail lets persisted-window callers pass the error
// detail that was already redacted when the failure event was recorded.
func (svc *ChannelService) executeModelRuleActionDetail(ctx context.Context, perf *PerformanceRecord, rule objects.APIKeyAutoDisableRule, count int, detail string) bool {
	if strings.TrimSpace(perf.Model) == "" {
		return false
	}
	rule = effectiveModelFreezeRule(rule)
	duration := *rule.DisableDurationMinutes
	expiresAt := time.Now().Add(time.Duration(duration) * time.Minute)
	reason := fmt.Sprintf("Model cooldown: %s, auto-recover after %d minutes\n%s", autoDisableFailureSummary(rule, count), duration, detail)
	return svc.markChannelModelCooldownUntil(ctx, perf.ChannelID, perf.Model, reason, &expiresAt)
}

func (svc *ChannelService) executePermanentModelFailureGuard(ctx context.Context, perf *PerformanceRecord, windowMinutes, count, threshold int) bool {
	return svc.executePermanentModelFailureGuardDetail(ctx, perf, windowMinutes, count, threshold, autoDisableErrorDetail(perf))
}

// executePermanentModelFailureGuardDetail permanently freezes only the failing
// channel×model: the channel itself and its other models keep serving. It lets
// persisted-window callers pass the error detail that was already redacted when
// the failure event was recorded.
func (svc *ChannelService) executePermanentModelFailureGuardDetail(ctx context.Context, perf *PerformanceRecord, windowMinutes, count, threshold int, detail string) bool {
	if strings.TrimSpace(perf.Model) == "" {
		return false
	}
	reason := fmt.Sprintf("Permanently frozen model %q reached %d failures within %d minutes (threshold %d); manual recovery required\n%s", perf.Model, count, windowMinutes, threshold, detail)
	return svc.markChannelModelCooldownPermanent(ctx, perf.ChannelID, perf.Model, reason)
}

// evaluateWindowedRule retains the triggering failures instead of consuming
// them. A temporary freeze cannot conceal the safeguard failure, including failures
// arriving while its persistence action is already in flight. Every failure is
// persisted first (see channel_failure_window.go), so the rolling window counts
// survive restarts and merge across server instances; only the action claim
// stays in-process, and the persisted cooldown writes stay CAS-protected.
func (svc *ChannelService) evaluateWindowedRule(ctx context.Context, perf *PerformanceRecord, rule objects.APIKeyAutoDisableRule, ruleKey string, permanentThreshold, permanentWindowMinutes int) bool {
	rule = effectiveModelFreezeRule(rule)
	window := time.Duration(rule.WindowMinutes) * time.Minute
	now := time.Now()
	failedAt := perf.EndTime
	if failedAt.IsZero() || failedAt.After(now) {
		failedAt = now
	}
	windowCutoff := now.Add(-window)
	if !failedAt.After(windowCutoff) {
		return false
	}
	successAt, err := svc.latestChannelModelSuccessAt(ctx, perf.ChannelID, perf.Model)
	if err != nil {
		log.Error(ctx, "failed to load channel model success reset", log.Cause(err), log.Int("channel_id", perf.ChannelID))
		return false
	}
	if !successAt.IsZero() && !failedAt.After(successAt) {
		return false
	}
	if successAt.After(windowCutoff) {
		windowCutoff = successAt
	}
	windowKey := fmt.Sprintf("%s:window:%d", autoDisableRuleIdentity(perf, rule), rule.WindowMinutes)

	if err := svc.recordChannelFailureEvent(ctx, perf, ruleKey, windowKey, failedAt); err != nil {
		log.Error(ctx, "failed to persist channel failure event", log.Cause(err), log.Int("channel_id", perf.ChannelID))
		return false
	}

	snapshot, err := svc.countChannelFailureEvents(ctx, ruleKey, windowKey, windowCutoff)
	if err != nil {
		log.Error(ctx, "failed to count channel failure window", log.Cause(err), log.Int("channel_id", perf.ChannelID))
		return false
	}

	successAt, err = svc.latestChannelModelSuccessAt(ctx, perf.ChannelID, perf.Model)
	if err != nil {
		log.Error(ctx, "failed to reload channel model success reset", log.Cause(err), log.Int("channel_id", perf.ChannelID))
		return false
	}
	if !successAt.IsZero() && !failedAt.After(successAt) {
		return false
	}
	if successAt.After(windowCutoff) {
		windowCutoff = successAt
		snapshot, err = svc.countChannelFailureEvents(ctx, ruleKey, windowKey, windowCutoff)
		if err != nil {
			log.Error(ctx, "failed to recount channel failure window after success", log.Cause(err), log.Int("channel_id", perf.ChannelID))
			return false
		}
	}
	permanentCutoff := now.Add(-time.Duration(permanentWindowMinutes) * time.Minute)
	if successAt.After(permanentCutoff) {
		permanentCutoff = successAt
	}
	permanentSnapshot, err := svc.countChannelModelFailureEvents(ctx, perf.ChannelID, perf.Model, permanentCutoff)
	if err != nil {
		log.Error(ctx, "failed to count permanent model failure window", log.Cause(err), log.Int("channel_id", perf.ChannelID))
		return false
	}
	permanent := permanentSnapshot.windowTotal >= permanentThreshold
	if !permanent && snapshot.ruleCount < max(rule.Times, 1) {
		return false
	}
	if !svc.claimWindowedRuleAction(perf.ChannelID, windowKey) {
		return false
	}
	// A successful request may have completed while this failure was waiting
	// to acquire the action claim. Re-check its persisted boundary before freezing.
	successAt, err = svc.latestChannelModelSuccessAt(ctx, perf.ChannelID, perf.Model)
	if err != nil {
		svc.releaseWindowedRuleAction(perf.ChannelID, windowKey)
		log.Error(ctx, "failed to verify channel model success reset before freeze", log.Cause(err), log.Int("channel_id", perf.ChannelID))
		return false
	}
	if !successAt.IsZero() && !failedAt.After(successAt) {
		svc.releaseWindowedRuleAction(perf.ChannelID, windowKey)
		return false
	}

	// The event inserted above always belongs to the window, so snapshot.latest
	// is never nil in practice; keep the fallback for robustness anyway.
	failureDetail := autoDisableErrorDetail(perf)
	latestFailure := *perf
	if snapshot.latest != nil {
		latestFailure = performanceRecordFromFailureEvent(snapshot.latest)
		failureDetail = snapshot.latest.ErrorDetail
	}

	var acted bool
	if permanent {
		permanentFailure := latestFailure
		permanentDetail := failureDetail
		if permanentSnapshot.latest != nil {
			permanentFailure = performanceRecordFromFailureEvent(permanentSnapshot.latest)
			permanentDetail = permanentSnapshot.latest.ErrorDetail
		}
		acted = svc.executePermanentModelFailureGuardDetail(ctx, &permanentFailure, permanentWindowMinutes, permanentSnapshot.windowTotal, permanentThreshold, permanentDetail)
	} else {
		acted = svc.executeMatchedRuleAction(ctx, perf, rule, snapshot.ruleCount)
	}

	if !permanent {
		// The safeguard threshold may have been crossed while the model freeze held the
		// claim. Escalate before releasing it; do not require a seventh request.
		recount, err := svc.countChannelModelFailureEvents(ctx, perf.ChannelID, perf.Model, time.Now().Add(-time.Duration(permanentWindowMinutes)*time.Minute))
		if err != nil {
			log.Error(ctx, "failed to recount channel failure window", log.Cause(err), log.Int("channel_id", perf.ChannelID))
		} else if recount.windowTotal >= permanentThreshold {
			escalatedDetail := failureDetail
			escalatedFailure := latestFailure
			if recount.latest != nil {
				escalatedFailure = performanceRecordFromFailureEvent(recount.latest)
				escalatedDetail = recount.latest.ErrorDetail
			}
			escalated := svc.executePermanentModelFailureGuardDetail(ctx, &escalatedFailure, permanentWindowMinutes, recount.windowTotal, permanentThreshold, escalatedDetail)
			acted = acted || escalated
		}
	}

	svc.releaseWindowedRuleAction(perf.ChannelID, windowKey)
	return acted
}
