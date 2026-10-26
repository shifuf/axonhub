package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channelfailureevent"
	"github.com/looplj/axonhub/internal/log"
	"unicode/utf8"
)

const (
	// maxAutoDisableWindowMinutes matches the maximum rule and permanent-freeze
	// windows (7 days).
	maxAutoDisableWindowMinutes = 10080
	// channelFailureEventRetention keeps a full extra day beyond the longest
	// window so clock skew between instances cannot drop countable failures.
	channelFailureEventRetention = maxAutoDisableWindowMinutes + 1440
)

// channelFailureEventSnapshot is the persisted view of one rolling window:
// the rule-specific count, the shared channel/model/window total and the most
// recent failure inside the window.
type channelFailureEventSnapshot struct {
	ruleCount   int
	windowTotal int
	latest      *ent.ChannelFailureEvent
}

// channelFailureEventHash indexes the counter keys, whose unbounded keyword
// patterns make a direct string index unsafe across dialects.
func channelFailureEventHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func channelModelSuccessMarkerKey(channelID int, model string) string {
	return fmt.Sprintf("auto-disable-success-reset:channel:%d:model:%q", channelID, model)
}

// recordChannelModelSuccessEvent persists the latest successful request time as
// a reset boundary. Keeping the boundary in the same shared table makes the
// failure streak consistent across restarts and server instances, and prevents
// delayed failure reports from before the success from re-entering the window.
func (svc *ChannelService) recordChannelModelSuccessEvent(ctx context.Context, perf *PerformanceRecord) error {
	if perf == nil || perf.ChannelID <= 0 || strings.TrimSpace(perf.Model) == "" {
		return nil
	}

	successAt := perf.EndTime
	now := time.Now()
	if successAt.IsZero() || successAt.After(now) {
		successAt = now
	}

	markerKey := channelModelSuccessMarkerKey(perf.ChannelID, perf.Model)
	markerHash := channelFailureEventHash(markerKey)
	client := svc.entFromContext(ctx)
	latest, err := client.ChannelFailureEvent.Query().Where(
		channelfailureevent.ChannelID(perf.ChannelID),
		channelfailureevent.Model(perf.Model),
		channelfailureevent.RuleKeyHash(markerHash),
	).Order(ent.Desc(channelfailureevent.FieldFailedAt, channelfailureevent.FieldID)).First(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return fmt.Errorf("load channel model success marker: %w", err)
	}
	if err == nil {
		if !successAt.After(latest.FailedAt) {
			return nil
		}
		// Keep the update monotonic across server instances: the timestamp
		// comparison must be part of the SQL UPDATE, not only this prior read.
		return client.ChannelFailureEvent.Update().
			Where(
				channelfailureevent.ID(latest.ID),
				channelfailureevent.FailedAtLT(successAt),
			).
			SetFailedAt(successAt).
			Exec(ctx)
	}

	return client.ChannelFailureEvent.Create().
		SetChannelID(perf.ChannelID).
		SetModel(perf.Model).
		SetRuleKey(markerKey).
		SetRuleKeyHash(markerHash).
		SetWindowKey(markerKey).
		SetWindowKeyHash(markerHash).
		SetStatusCode(0).
		SetErrorDetail("").
		SetFailedAt(successAt).
		Exec(ctx)
}

func (svc *ChannelService) latestChannelModelSuccessAt(ctx context.Context, channelID int, model string) (time.Time, error) {
	markerHash := channelFailureEventHash(channelModelSuccessMarkerKey(channelID, model))
	marker, err := svc.entFromContext(ctx).ChannelFailureEvent.Query().Where(
		channelfailureevent.ChannelID(channelID),
		channelfailureevent.Model(model),
		channelfailureevent.RuleKeyHash(markerHash),
	).Order(ent.Desc(channelfailureevent.FieldFailedAt, channelfailureevent.FieldID)).First(ctx)
	if ent.IsNotFound(err) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("load latest channel model success: %w", err)
	}
	return marker.FailedAt, nil
}

// truncateChannelFailureKey keeps the stored plaintext key bounded; truncation
// only affects observability, never counting, because counting uses the hash.
func truncateChannelFailureKey(key string) string {
	const maxChannelFailureKeyBytes = 1024
	if len(key) <= maxChannelFailureKeyBytes {
		return key
	}
	truncated := key[:maxChannelFailureKeyBytes]
	// Avoid splitting a UTF-8 sequence; the key embeds quoted model names.
	for !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated
}

// recordChannelFailureEvent persists one failure row. The insert is atomic, so
// concurrent instances that evaluate the same channel merge into one window
// instead of keeping isolated in-process counters.
func (svc *ChannelService) recordChannelFailureEvent(ctx context.Context, perf *PerformanceRecord, ruleKey, windowKey string, failedAt time.Time) error {
	return svc.entFromContext(ctx).ChannelFailureEvent.Create().
		SetChannelID(perf.ChannelID).
		SetModel(perf.Model).
		SetRuleKey(truncateChannelFailureKey(ruleKey)).
		SetRuleKeyHash(channelFailureEventHash(ruleKey)).
		SetWindowKey(truncateChannelFailureKey(windowKey)).
		SetWindowKeyHash(channelFailureEventHash(windowKey)).
		SetStatusCode(perf.ResponseStatusCode).
		SetErrorDetail(autoDisableErrorDetail(perf)).
		SetFailedAt(failedAt).
		Exec(ctx)
}

// countChannelFailureEvents counts the persisted failures of one rule and of
// the shared channel/model/window aggregate, plus the latest failure that the
// action reason should describe.
func (svc *ChannelService) countChannelFailureEvents(ctx context.Context, ruleKey, windowKey string, cutoff time.Time) (channelFailureEventSnapshot, error) {
	client := svc.entFromContext(ctx)
	ruleCount, err := client.ChannelFailureEvent.Query().
		Where(
			channelfailureevent.RuleKeyHash(channelFailureEventHash(ruleKey)),
			channelfailureevent.FailedAtGT(cutoff),
		).
		Count(ctx)
	if err != nil {
		return channelFailureEventSnapshot{}, fmt.Errorf("count rule failures: %w", err)
	}

	windowTotal, err := client.ChannelFailureEvent.Query().
		Where(
			channelfailureevent.WindowKeyHash(channelFailureEventHash(windowKey)),
			channelfailureevent.FailedAtGT(cutoff),
		).
		Count(ctx)
	if err != nil {
		return channelFailureEventSnapshot{}, fmt.Errorf("count window failures: %w", err)
	}

	latest, err := client.ChannelFailureEvent.Query().
		Where(
			channelfailureevent.WindowKeyHash(channelFailureEventHash(windowKey)),
			channelfailureevent.FailedAtGT(cutoff),
		).
		Order(ent.Desc(channelfailureevent.FieldFailedAt, channelfailureevent.FieldID)).
		First(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return channelFailureEventSnapshot{}, fmt.Errorf("load latest failure: %w", err)
	}

	return channelFailureEventSnapshot{ruleCount: ruleCount, windowTotal: windowTotal, latest: latest}, nil
}

// countChannelModelFailureEvents counts failures by channel and original request
// model across all matching rules, independently of their individual cooldown windows.
func (svc *ChannelService) countChannelModelFailureEvents(ctx context.Context, channelID int, model string, cutoff time.Time) (channelFailureEventSnapshot, error) {
	successAt, err := svc.latestChannelModelSuccessAt(ctx, channelID, model)
	if err != nil {
		return channelFailureEventSnapshot{}, err
	}
	if successAt.After(cutoff) {
		cutoff = successAt
	}
	query := svc.entFromContext(ctx).ChannelFailureEvent.Query().Where(
		channelfailureevent.ChannelID(channelID),
		channelfailureevent.Model(model),
		channelfailureevent.RuleKeyHashNEQ(channelFailureEventHash(channelModelSuccessMarkerKey(channelID, model))),
		channelfailureevent.FailedAtGT(cutoff),
	)
	count, err := query.Count(ctx)
	if err != nil {
		return channelFailureEventSnapshot{}, fmt.Errorf("count channel model failures: %w", err)
	}
	latest, err := svc.entFromContext(ctx).ChannelFailureEvent.Query().Where(
		channelfailureevent.ChannelID(channelID),
		channelfailureevent.Model(model),
		channelfailureevent.RuleKeyHashNEQ(channelFailureEventHash(channelModelSuccessMarkerKey(channelID, model))),
		channelfailureevent.FailedAtGT(cutoff),
	).Order(ent.Desc(channelfailureevent.FieldFailedAt, channelfailureevent.FieldID)).First(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return channelFailureEventSnapshot{}, fmt.Errorf("load latest channel model failure: %w", err)
	}
	return channelFailureEventSnapshot{windowTotal: count, latest: latest}, nil
}

// performanceRecordFromFailureEvent rebuilds enough of the original record for
// the action reason. The stored detail is already redacted, so the raw API key
// is never restored.
func performanceRecordFromFailureEvent(event *ent.ChannelFailureEvent) PerformanceRecord {
	return PerformanceRecord{
		ChannelID:          event.ChannelID,
		Model:              event.Model,
		ResponseStatusCode: event.StatusCode,
		ErrorMessage:       event.ErrorDetail,
		EndTime:            event.FailedAt,
	}
}

// claimWindowedRuleAction keeps at most one cooldown action in flight per
// instance for a channel/model/window. Cross-instance duplicates remain safe:
// the persisted cooldown writes are CAS-protected and idempotent.
func (svc *ChannelService) claimWindowedRuleAction(channelID int, windowKey string) bool {
	svc.apiKeyErrorCountsLock.Lock()
	defer svc.apiKeyErrorCountsLock.Unlock()

	if svc.windowedRuleActionsInFlight == nil {
		svc.windowedRuleActionsInFlight = make(map[int]map[string]bool)
	}
	if svc.windowedRuleActionsInFlight[channelID] == nil {
		svc.windowedRuleActionsInFlight[channelID] = make(map[string]bool)
	}
	if svc.windowedRuleActionsInFlight[channelID][windowKey] {
		return false
	}
	svc.windowedRuleActionsInFlight[channelID][windowKey] = true
	return true
}

func (svc *ChannelService) releaseWindowedRuleAction(channelID int, windowKey string) {
	svc.apiKeyErrorCountsLock.Lock()
	defer svc.apiKeyErrorCountsLock.Unlock()

	if svc.windowedRuleActionsInFlight[channelID] != nil {
		delete(svc.windowedRuleActionsInFlight[channelID], windowKey)
	}
}

// purgeExpiredChannelFailureEvents removes rows older than the retention bound.
// Older rows can never influence a window again, so deleting them keeps the
// table bounded without waiting for a full channel deletion.
func (svc *ChannelService) purgeExpiredChannelFailureEvents(ctx context.Context, now time.Time) {
	cutoff := now.Add(-time.Duration(channelFailureEventRetention) * time.Minute)
	deleted, err := svc.entFromContext(ctx).ChannelFailureEvent.Delete().
		Where(channelfailureevent.FailedAtLT(cutoff)).
		Exec(ctx)
	if err != nil {
		log.Error(ctx, "failed to purge expired channel failure events", log.Cause(err))
		return
	}
	if deleted > 0 {
		log.Info(ctx, "purged expired channel failure events", log.Int("deleted", deleted))
	}
}
