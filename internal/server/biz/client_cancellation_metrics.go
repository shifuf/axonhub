package biz

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/looplj/axonhub/internal/log"
)

// evaluateClientCancellationForFailure records eligible cancellations in a
// sliding window per channel, original request model, project and window size.
// It is independent of HTTP errors and the permanent-channel failure breaker.
func (svc *ChannelService) evaluateClientCancellationForFailure(ctx context.Context, perf *PerformanceRecord) bool {
	if perf == nil || perf.ChannelID <= 0 || !perf.Canceled || strings.TrimSpace(perf.Model) == "" {
		return false
	}

	// A non-stream abort or a stream that never received a first token says
	// nothing about the upstream channel. Preserve that eligibility boundary.
	if !perf.Stream || perf.FirstTokenTime == nil {
		return false
	}

	retryPolicy := svc.SystemService.RetryPolicyOrDefault(ctx)
	policy, _ := retryPolicy.ClientCancellationPolicy.EffectiveClientCancellationPolicy(perf.ProjectID)
	if !policy.Enabled {
		return false
	}

	threshold := max(policy.Threshold, 1)
	windowMinutes := max(policy.WindowMinutes, 1)
	cooldownMinutes := max(policy.CooldownMinutes, 1)
	// Credentials, action, threshold and cooldown duration do not partition the
	// evidence. All keys serving the same pair/project/window share one count.
	counterKey := fmt.Sprintf("%s:project:%d:window:%d", cancellationCounterIdentity(perf), perf.ProjectID, windowMinutes)

	svc.apiKeyErrorCountsLock.Lock()
	// Timestamp after taking the lock so concurrent append order is chronological.
	now := time.Now()
	svc.pruneClientCancellationWindowsLocked(now, 0)
	if svc.cancellationWindow[perf.ChannelID] == nil {
		svc.cancellationWindow[perf.ChannelID] = make(map[string][]time.Time)
	}
	// Keeping the latest threshold samples is sufficient to decide whether at
	// least threshold cancellations remain in the window. A hot, frozen pair
	// must not accumulate an unbounded timestamp slice.
	timestamps := svc.cancellationWindow[perf.ChannelID][counterKey]
	if len(timestamps) >= threshold {
		timestamps = append(timestamps[:0], timestamps[len(timestamps)-threshold+1:]...)
	}
	timestamps = append(timestamps, now)
	svc.cancellationWindow[perf.ChannelID][counterKey] = timestamps
	count := len(timestamps)
	shouldAct := count >= threshold && !svc.cancellationActionsInFlight[perf.ChannelID][counterKey]
	if shouldAct {
		if svc.cancellationActionsInFlight[perf.ChannelID] == nil {
			svc.cancellationActionsInFlight[perf.ChannelID] = make(map[string]bool)
		}
		svc.cancellationActionsInFlight[perf.ChannelID][counterKey] = true
	}
	svc.apiKeyErrorCountsLock.Unlock()

	if !shouldAct {
		return false
	}

	acted := svc.executeClientCancellationCooldown(ctx, perf, count, windowMinutes, cooldownMinutes, policy.Action)

	svc.apiKeyErrorCountsLock.Lock()
	delete(svc.cancellationActionsInFlight[perf.ChannelID], counterKey)
	if len(svc.cancellationActionsInFlight[perf.ChannelID]) == 0 {
		delete(svc.cancellationActionsInFlight, perf.ChannelID)
	}
	// A freeze never consumes its evidence. Another cancellation in the same
	// rolling window can still trigger after expiry/manual unfreeze, while the
	// model-freeze helper preserves an already-active freeze's original expiry.
	svc.apiKeyErrorCountsLock.Unlock()

	if acted {
		log.Warn(ctx, "channel model entered cancellation cooldown",
			log.Int("channel_id", perf.ChannelID),
			log.String("model", perf.Model),
			log.Int("project_id", perf.ProjectID),
			log.Int("threshold", threshold),
			log.Int("cooldown_minutes", cooldownMinutes),
			log.String("action", policy.Action),
		)
	}

	return acted
}

func (svc *ChannelService) executeClientCancellationCooldown(
	ctx context.Context,
	perf *PerformanceRecord,
	count int,
	windowMinutes int,
	cooldownMinutes int,
	_ string,
) bool {
	// Never broaden a missing model into a credential or whole-channel action,
	// even when called directly with an old, empty or otherwise invalid action.
	if perf == nil || perf.ChannelID <= 0 || strings.TrimSpace(perf.Model) == "" {
		return false
	}

	return svc.markChannelModelCooldown(ctx, perf.ChannelID, perf.Model, count, windowMinutes, cooldownMinutes)
}

// clearCancellationCountsOnSuccess is retained for the performance-recording
// hook, but a success is not an eraser for the cancellation rolling window.
// Only already-expired evidence is eligible for garbage collection.
func (svc *ChannelService) clearCancellationCountsOnSuccess(perf *PerformanceRecord) {
	if perf == nil || perf.ChannelID <= 0 {
		return
	}

	svc.apiKeyErrorCountsLock.Lock()
	defer svc.apiKeyErrorCountsLock.Unlock()
	svc.pruneClientCancellationWindowsLocked(time.Now(), perf.ChannelID)
}

// pruneClientCancellationWindowsLocked requires apiKeyErrorCountsLock. A zero
// channelID sweeps idle identities across channels on cancellation traffic;
// success only needs to prune its own channel. These maps are in-memory only.
func (svc *ChannelService) pruneClientCancellationWindowsLocked(now time.Time, channelID int) {
	for id, windows := range svc.cancellationWindow {
		if channelID > 0 && id != channelID {
			continue
		}
		for key, timestamps := range windows {
			// The window is the final key segment; quoted original model names
			// may contain arbitrary delimiters without colliding with it.
			windowMinutes := maxClientCancellationCooldownMinutes
			if i := strings.LastIndex(key, ":window:"); i >= 0 {
				if minutes, err := strconv.Atoi(key[i+len(":window:"):]); err == nil {
					windowMinutes = min(max(minutes, 1), maxClientCancellationCooldownMinutes)
				}
			}
			windowStart := now.Add(-time.Duration(windowMinutes) * time.Minute)
			recent := timestamps[:0]
			for _, ts := range timestamps {
				if ts.After(windowStart) {
					recent = append(recent, ts)
				}
			}
			if len(recent) == 0 {
				delete(windows, key)
			} else {
				windows[key] = recent
			}
		}
		if len(windows) == 0 {
			delete(svc.cancellationWindow, id)
		}
	}
}

func cancellationCounterIdentity(perf *PerformanceRecord) string {
	return fmt.Sprintf("model:%q", perf.Model)
}
