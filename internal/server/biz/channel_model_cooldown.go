package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/predicate"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcontext"
)

// HasActiveModelCooldown reports whether the request model is currently in a
// model-scoped cooldown on this channel. The channel itself stays enabled and
// keeps serving its other models.
func (ch *Channel) HasActiveModelCooldown(requestModel string, now time.Time) bool {
	if ch == nil || requestModel == "" {
		return false
	}

	for _, mc := range ch.ModelCooldowns {
		if mc.Model != requestModel {
			continue
		}
		if mc.ExpiresAt == nil || mc.ExpiresAt.After(now) {
			return true
		}
	}

	return false
}

// mutateChannelModelCooldowns serializes local writes and guards the stored
// JSON snapshot against concurrent writes from other instances. Timestamp-only
// optimistic locks are insufficient on databases with coarse timestamp precision.
func (svc *ChannelService) mutateChannelModelCooldowns(ctx context.Context, channelID int, apply func(*ent.Channel) ([]objects.ModelCooldown, bool)) (*ent.Channel, bool, error) {
	svc.modelCooldownsLock.Lock()
	defer svc.modelCooldownsLock.Unlock()
	client := svc.entFromContext(ctx)
	const maxAttempts = 8
	for range maxAttempts {
		ch, err := client.Channel.Get(ctx, channelID)
		if err != nil {
			return nil, false, fmt.Errorf("failed to get channel: %w", err)
		}
		next, changed := apply(ch)
		if !changed || slices.EqualFunc(next, ch.ModelCooldowns, equalModelCooldown) {
			return ch, false, nil
		}
		snapshot, err := modelCooldownsSnapshotPredicate(ch.ModelCooldowns)
		if err != nil {
			return nil, false, err
		}
		// UpdateOne can turn a zero-row CAS into success via ensureExists
		// under a MySQL repeatable-read transaction. Trust affected rows only.
		affected, err := client.Channel.Update().
			Where(channel.IDEQ(channelID), channel.StatusEQ(ch.Status), snapshot).
			SetModelCooldowns(next).Save(ctx)
		if err != nil {
			return nil, false, fmt.Errorf("failed to update channel model cooldowns: %w", err)
		}
		if affected == 0 {
			if ent.TxFromContext(ctx) != nil {
				// An outer repeatable-read snapshot cannot be refreshed here.
				// Fail the mutation so its owner rolls back and can retry.
				return nil, false, ErrChannelUpdatedConcurrently
			}
			continue
		}
		updated, err := client.Channel.Get(ctx, channelID)
		if err != nil {
			return nil, false, fmt.Errorf("failed to reload channel model cooldowns: %w", err)
		}
		svc.refreshModelCooldownsAfterWrite(ctx, channelID)
		return updated, true, nil
	}
	return nil, false, fmt.Errorf("channel model cooldowns changed concurrently after %d attempts", maxAttempts)
}

func equalModelCooldown(a, b objects.ModelCooldown) bool {
	expiryEqual := a.ExpiresAt == b.ExpiresAt ||
		(a.ExpiresAt != nil && b.ExpiresAt != nil && a.ExpiresAt.Equal(*b.ExpiresAt))
	return a.Model == b.Model && a.Reason == b.Reason && a.CreatedAt.Equal(b.CreatedAt) && expiryEqual
}

// refreshModelCooldownsAfterWrite is called with the model mutation lock held.
// Autocommit writes keep that lock until the serving cache has been refreshed.
// Transactional writes refresh only after commit, never leaking rolled-back
// state; each hook takes the lock after next.Commit so nested hooks cannot deadlock.
func (svc *ChannelService) refreshModelCooldownsAfterWrite(ctx context.Context, channelID int) {
	if tx := ent.TxFromContext(ctx); tx != nil {
		refreshBase := ent.NewContext(context.WithoutCancel(ctx), svc.db)
		tx.OnCommit(func(next ent.Committer) ent.Committer {
			return ent.CommitFunc(func(commitCtx context.Context, tx *ent.Tx) error {
				if err := next.Commit(commitCtx, tx); err != nil {
					return err
				}
				reloadCtx, cancel := xcontext.DetachWithTimeout(refreshBase, 10*time.Second)
				defer cancel()
				svc.modelCooldownsLock.Lock()
				defer svc.modelCooldownsLock.Unlock()
				svc.reloadChannelModelCooldowns(reloadCtx, channelID)
				return nil
			})
		})
		return
	}
	svc.reloadChannelModelCooldowns(ctx, channelID)
}

func modelCooldownsSnapshotPredicate(snapshot []objects.ModelCooldown) (predicate.Channel, error) {
	if snapshot == nil {
		// Older rows may contain SQL NULL or the JSON null literal.
		return channel.Or(channel.ModelCooldownsIsNil(), modelCooldownsJSONEqual("null")), nil
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("failed to encode channel model cooldown snapshot: %w", err)
	}
	return modelCooldownsJSONEqual(string(encoded)), nil
}

func modelCooldownsJSONEqual(encoded string) predicate.Channel {
	return func(s *sql.Selector) {
		s.Where(sql.P(func(b *sql.Builder) {
			column := s.C(channel.FieldModelCooldowns)
			switch b.Dialect() {
			case dialect.Postgres:
				b.Ident(column).WriteString("::jsonb = ").Arg(encoded).WriteString("::jsonb")
			case dialect.MySQL:
				b.Ident(column).WriteString(" = CAST(").Arg(encoded).WriteString(" AS JSON)")
			default:
				// SQLite drivers may store Ent's marshaled []byte as a BLOB.
				// Cast the stored value so both TEXT and BLOB rows compare.
				b.WriteString("CAST(").Ident(column).WriteString(" AS TEXT) = ").Arg(encoded)
			}
		}))
	}
}

func (svc *ChannelService) reloadChannelModelCooldowns(ctx context.Context, channelID int) {
	if err := svc.enabledChannelsCache.Load(ctx, true); err != nil {
		log.Warn(ctx, "Failed to synchronously reload channels after model cooldown change", log.Int("channel_id", channelID), log.Cause(err))
	}
	svc.asyncReloadChannels()
}

// markChannelModelCooldownUntil persists one channel-model freeze. A nil expiry
// requires manual recovery. Repeated failures do not extend an active freeze.
func (svc *ChannelService) markChannelModelCooldownUntil(ctx context.Context, channelID int, requestModel, reason string, expiresAt *time.Time) bool {
	if requestModel == "" {
		return false
	}
	ctx, cancel := xcontext.DetachWithTimeout(ctx, 10*time.Second)
	defer cancel()
	ch, changed, err := svc.mutateChannelModelCooldowns(ctx, channelID, func(ch *ent.Channel) ([]objects.ModelCooldown, bool) {
		if ch.Status != channel.StatusEnabled {
			return nil, false
		}
		for _, mc := range ch.ModelCooldowns {
			if mc.Model == requestModel && mc.IsActive() {
				return nil, false
			}
		}
		cooldowns := append(lo.Filter(ch.ModelCooldowns, func(mc objects.ModelCooldown, _ int) bool {
			return !mc.IsExpired()
		}), objects.ModelCooldown{Model: requestModel, Reason: reason, CreatedAt: time.Now(), ExpiresAt: expiresAt})
		return cooldowns, true
	})
	if err != nil {
		log.Error(ctx, "Failed to persist model cooldown", log.Int("channel_id", channelID), log.String("model", requestModel), log.Cause(err))
		return false
	}
	if ch.Status != channel.StatusEnabled {
		return false
	}
	if changed {
		log.Warn(ctx, "Channel model entered cooldown", log.Int("channel_id", channelID), log.String("model", requestModel), log.String("reason", reason))
	}
	return true
}

// markChannelModelCooldownPermanent persists the permanent safeguard freeze for
// one channel-model pair. Unlike the timed variant it upgrades an active
// temporary cooldown of the same model to permanent, so a threshold burst can
// never recover automatically after the temporary window elapses.
func (svc *ChannelService) markChannelModelCooldownPermanent(ctx context.Context, channelID int, requestModel, reason string) bool {
	if requestModel == "" {
		return false
	}
	ctx, cancel := xcontext.DetachWithTimeout(ctx, 10*time.Second)
	defer cancel()
	ch, changed, err := svc.mutateChannelModelCooldowns(ctx, channelID, func(ch *ent.Channel) ([]objects.ModelCooldown, bool) {
		if ch.Status != channel.StatusEnabled {
			return nil, false
		}
		for _, mc := range ch.ModelCooldowns {
			if mc.Model == requestModel && mc.IsActive() && mc.ExpiresAt == nil {
				// Already permanently frozen; never reset its reason.
				return nil, false
			}
		}
		// Drop expired entries and replace the model's active temporary freeze
		// with the permanent one. Other models keep their cooldowns untouched.
		cooldowns := append(lo.Filter(ch.ModelCooldowns, func(mc objects.ModelCooldown, _ int) bool {
			if mc.IsExpired() {
				return false
			}
			return mc.Model != requestModel
		}), objects.ModelCooldown{Model: requestModel, Reason: reason, CreatedAt: time.Now()})
		return cooldowns, true
	})
	if err != nil {
		log.Error(ctx, "Failed to persist permanent model cooldown", log.Int("channel_id", channelID), log.String("model", requestModel), log.Cause(err))
		return false
	}
	if ch.Status != channel.StatusEnabled {
		return false
	}
	if changed {
		log.Warn(ctx, "Channel model permanently frozen", log.Int("channel_id", channelID), log.String("model", requestModel), log.String("reason", reason))
	}
	return true
}

// markChannelModelCooldown records the client-cancellation variant of a model freeze.
func (svc *ChannelService) markChannelModelCooldown(ctx context.Context, channelID int, requestModel string, count, windowMinutes, cooldownMinutes int) bool {
	expiresAt := time.Now().Add(time.Duration(cooldownMinutes) * time.Minute)
	reason := fmt.Sprintf("Client cancellation cooldown: %d cancellations within %d minutes, auto-recover after %d minutes", max(count, 1), windowMinutes, cooldownMinutes)
	return svc.markChannelModelCooldownUntil(ctx, channelID, requestModel, reason, &expiresAt)
}

// ClearChannelModelCooldown manually unfreezes one model and returns the channel.
func (svc *ChannelService) ClearChannelModelCooldown(ctx context.Context, channelID int, requestModel string) (*ent.Channel, error) {
	ctx, cancel := xcontext.DetachWithTimeout(ctx, 10*time.Second)
	defer cancel()
	updated, changed, err := svc.mutateChannelModelCooldowns(ctx, channelID, func(ch *ent.Channel) ([]objects.ModelCooldown, bool) {
		remaining := lo.Filter(ch.ModelCooldowns, func(mc objects.ModelCooldown, _ int) bool { return mc.Model != requestModel })
		return remaining, len(remaining) != len(ch.ModelCooldowns)
	})
	if err != nil {
		return nil, err
	}
	if changed {
		log.Info(ctx, "Cleared channel model cooldown", log.Int("channel_id", channelID), log.String("model", requestModel))
	}
	return updated, nil
}

// FreezeChannelModel manually freezes a channel-model pair. A non-positive
// duration requires manual recovery; refreezing moves the existing expiry.
func (svc *ChannelService) FreezeChannelModel(ctx context.Context, channelID int, requestModel string, durationMinutes int) (*ent.Channel, error) {
	if requestModel == "" {
		return nil, fmt.Errorf("model is required")
	}
	ctx, cancel := xcontext.DetachWithTimeout(ctx, 10*time.Second)
	defer cancel()
	now := time.Now()
	var expiresAt *time.Time
	if durationMinutes > 0 {
		expires := now.Add(time.Duration(durationMinutes) * time.Minute)
		expiresAt = &expires
	}
	updated, _, err := svc.mutateChannelModelCooldowns(ctx, channelID, func(ch *ent.Channel) ([]objects.ModelCooldown, bool) {
		remaining := make([]objects.ModelCooldown, 0, len(ch.ModelCooldowns)+1)
		replaced := false
		for _, mc := range ch.ModelCooldowns {
			if mc.Model != requestModel || mc.IsExpired() {
				remaining = append(remaining, mc)
				continue
			}
			remaining = append(remaining, objects.ModelCooldown{Model: mc.Model, Reason: "Manually frozen by user", CreatedAt: mc.CreatedAt, ExpiresAt: expiresAt})
			replaced = true
		}
		if !replaced {
			remaining = append(remaining, objects.ModelCooldown{Model: requestModel, Reason: "Manually frozen by user", CreatedAt: now, ExpiresAt: expiresAt})
		}
		return remaining, true
	})
	if err != nil {
		return nil, err
	}
	log.Info(ctx, "Channel model frozen manually", log.Int("channel_id", channelID), log.String("model", requestModel), log.Int("duration_minutes", durationMinutes))
	return updated, nil
}

// cleanupChannelExpiredModelCooldowns drops elapsed freezes without overwriting
// concurrent freezes or resurrecting a model that was manually unfrozen.
func (svc *ChannelService) cleanupChannelExpiredModelCooldowns(ctx context.Context, channelID int) (bool, int, error) {
	removed := 0
	_, changed, err := svc.mutateChannelModelCooldowns(ctx, channelID, func(ch *ent.Channel) ([]objects.ModelCooldown, bool) {
		remaining := lo.Filter(ch.ModelCooldowns, func(mc objects.ModelCooldown, _ int) bool { return !mc.IsExpired() })
		removed = len(ch.ModelCooldowns) - len(remaining)
		return remaining, removed > 0
	})
	return changed, removed, err
}
