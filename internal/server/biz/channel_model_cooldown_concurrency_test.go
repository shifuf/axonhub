package biz

import (
	"context"
	"fmt"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache/live"
)

// The hook interleaves a committed write from another service after the first
// service reads its snapshot but before it saves. No timing or goroutines are
// required to reproduce a cross-instance lost update deterministically.
func TestChannelService_ModelCooldownWritesPreserveConcurrentChanges(t *testing.T) {
	for _, operation := range []string{"automatic freeze", "manual freeze", "manual unfreeze", "expiry cleanup", "cleanup after unfreeze"} {
		t.Run(operation, func(t *testing.T) {
			rule := objects.APIKeyAutoDisableRule{Times: 1, Action: objects.APIKeyAutoDisableActionPermanent}
			ctx, first, ch := newWindowRuleTestService(t, rule)
			initial := []objects.ModelCooldown{
				{Model: "expired", CreatedAt: time.Now().Add(-time.Hour), ExpiresAt: lo.ToPtr(time.Now().Add(-time.Minute))},
				{Model: "existing", Reason: "manual", CreatedAt: time.Now()},
			}
			_, err := first.db.Channel.UpdateOneID(ch.ID).SetModelCooldowns(initial).Save(ctx)
			require.NoError(t, err)
			second := newTestChannelService(first.db)
			t.Cleanup(second.Stop)
			interleaved := false
			first.db.Channel.Use(func(next ent.Mutator) ent.Mutator {
				return ent.MutateFunc(func(ctx context.Context, mutation ent.Mutation) (ent.Value, error) {
					if !interleaved {
						interleaved = true
						var err error
						if operation == "cleanup after unfreeze" {
							_, err = second.ClearChannelModelCooldown(ctx, ch.ID, "existing")
						} else {
							_, err = second.FreezeChannelModel(ctx, ch.ID, "concurrent", 0)
						}
						if err != nil {
							return nil, err
						}
					}
					return next.Mutate(ctx, mutation)
				})
			})
			switch operation {
			case "automatic freeze":
				require.True(t, first.markChannelModelCooldownUntil(ctx, ch.ID, "new", "automatic", nil))
			case "manual freeze":
				_, err = first.FreezeChannelModel(ctx, ch.ID, "new", 0)
				require.NoError(t, err)
			case "manual unfreeze":
				_, err = first.ClearChannelModelCooldown(ctx, ch.ID, "existing")
				require.NoError(t, err)
			default:
				cleaned, _, err := first.cleanupChannelExpiredModelCooldowns(ctx, ch.ID)
				require.NoError(t, err)
				require.True(t, cleaned)
			}
			require.True(t, interleaved)
			updated, err := first.db.Channel.Get(ctx, ch.ID)
			require.NoError(t, err)
			models := lo.Map(updated.ModelCooldowns, func(mc objects.ModelCooldown, _ int) string { return mc.Model })
			if operation == "cleanup after unfreeze" {
				require.NotContains(t, models, "existing", "cleanup must not resurrect a manually cleared freeze")
			} else {
				require.Contains(t, models, "concurrent", "a concurrent model freeze must not be overwritten")
			}
			if operation == "automatic freeze" || operation == "manual freeze" {
				require.Contains(t, models, "new")
			}
			if operation == "manual unfreeze" {
				require.NotContains(t, models, "existing")
			}
		})
	}
}

func TestChannelService_ModelCooldownSnapshotMatchesSQLiteStorage(t *testing.T) {
	ctx, svc, ch := newWindowRuleTestService(t, objects.APIKeyAutoDisableRule{Times: 1, Action: objects.APIKeyAutoDisableActionPermanent})
	snapshot, err := svc.db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	guard, err := modelCooldownsSnapshotPredicate(snapshot.ModelCooldowns)
	require.NoError(t, err)
	blob, err := svc.db.Channel.Query().Where(channel.IDEQ(ch.ID), func(s *sql.Selector) {
		s.Where(sql.ExprP("typeof(model_cooldowns) = 'blob'"))
	}).Exist(ctx)
	require.NoError(t, err)
	t.Logf("SQLite stores the JSON snapshot as BLOB: %v", blob)
	matches, err := svc.db.Channel.Query().Where(channel.IDEQ(ch.ID), guard).Exist(ctx)
	require.NoError(t, err)
	require.True(t, matches, "the optimistic guard must match the untouched JSON value")
}

func TestModelCooldownSnapshotPredicateDialects(t *testing.T) {
	for _, driver := range []string{dialect.SQLite, dialect.Postgres, dialect.MySQL} {
		t.Run(driver, func(t *testing.T) {
			guard, err := modelCooldownsSnapshotPredicate([]objects.ModelCooldown{})
			require.NoError(t, err)
			query := sql.Dialect(driver).Select("*").From(sql.Table(channel.Table))
			guard(query)
			statement, args := query.Query()
			require.Equal(t, []any{"[]"}, args)
			switch driver {
			case dialect.Postgres:
				require.Contains(t, statement, `::jsonb = $1::jsonb`)
			case dialect.MySQL:
				require.Contains(t, statement, ` = CAST(? AS JSON)`)
			default:
				require.Contains(t, statement, ` AS TEXT) = ?`)
			}
		})
	}
}

func TestChannelService_ModelCooldownSnapshotMatchesNullStorage(t *testing.T) {
	for _, sqlNull := range []bool{false, true} {
		t.Run(fmt.Sprintf("sql-null=%v", sqlNull), func(t *testing.T) {
			ctx, svc, ch := newWindowRuleTestService(t, objects.APIKeyAutoDisableRule{Times: 1, Action: objects.APIKeyAutoDisableActionPermanent})
			update := svc.db.Channel.UpdateOneID(ch.ID)
			if sqlNull {
				update.ClearModelCooldowns()
			} else {
				update.SetModelCooldowns(nil)
			}
			_, err := update.Save(ctx)
			require.NoError(t, err)
			require.True(t, svc.markChannelModelCooldownUntil(ctx, ch.ID, "new", "automatic", nil))
			stored, err := svc.db.Channel.Get(ctx, ch.ID)
			require.NoError(t, err)
			require.Len(t, stored.ModelCooldowns, 1)
		})
	}
}

func TestChannelService_ModelCooldownRefreshIncludesEveryCommittedWrite(t *testing.T) {
	ctx, svc, ch := newWindowRuleTestService(t, objects.APIKeyAutoDisableRule{Times: 1, Action: objects.APIKeyAutoDisableActionPermanent})
	readStarted := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	svc.enabledChannelsCache.Stop()
	refreshes := 0
	svc.enabledChannelsCache = live.NewCache(live.Options[[]*Channel]{
		Name: "model-cooldown-refresh-test", InitialValue: []*Channel{buildChannel(ch, nil)}, RefreshInterval: time.Hour,
		RefreshFunc: func(ctx context.Context, current []*Channel, lastUpdate time.Time) ([]*Channel, time.Time, bool, error) {
			stored, err := svc.entFromContext(ctx).Channel.Get(ctx, ch.ID)
			if err != nil {
				return current, lastUpdate, false, err
			}
			refreshes++
			if refreshes == 1 {
				close(readStarted)
				<-release
			}
			return []*Channel{buildChannel(stored, nil)}, time.Now(), true, nil
		},
	})
	secondCommitted := make(chan struct{})
	mutations := 0
	svc.db.Channel.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, mutation ent.Mutation) (ent.Value, error) {
			result, err := next.Mutate(ctx, mutation)
			mutations++
			if mutations == 2 {
				close(secondCommitted)
			}
			return result, err
		})
	})
	freeze := func(model string, done chan error) {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("freeze panicked: %v", r)
			}
		}()
		_, err := svc.FreezeChannelModel(ctx, ch.ID, model, 0)
		done <- err
	}
	a, b := make(chan error, 1), make(chan error, 1)
	go freeze("a", a)
	select {
	case <-readStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first refresh did not read")
	}
	go freeze("b", b)
	// A correct lock keeps B's write behind A's paused refresh. Before the fix,
	// B commits immediately and joins A's now-stale singleflight result.
	select {
	case <-secondCommitted:
	case <-time.After(time.Second):
	}
	close(release)
	released = true
	for _, done := range []chan error{a, b} {
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("freeze did not return")
		}
	}
	require.True(t, svc.GetEnabledChannel(ch.ID).HasActiveModelCooldown("a", time.Now()))
	require.True(t, svc.GetEnabledChannel(ch.ID).HasActiveModelCooldown("b", time.Now()), "a successful freeze must be visible in the serving cache before returning")
}

func TestChannelService_ModelCooldownRefreshRunsOnlyAfterTransactionCommit(t *testing.T) {
	for _, commit := range []bool{false, true} {
		t.Run(fmt.Sprintf("commit=%v", commit), func(t *testing.T) {
			ctx, svc, ch := newWindowRuleTestService(t, objects.APIKeyAutoDisableRule{Times: 1, Action: objects.APIKeyAutoDisableActionPermanent})
			svc.enabledChannelsCache.Stop()
			svc.enabledChannelsCache = live.NewCache(live.Options[[]*Channel]{
				Name: "model-cooldown-tx-test", InitialValue: []*Channel{buildChannel(ch, nil)}, RefreshInterval: time.Hour,
				RefreshFunc: func(ctx context.Context, current []*Channel, lastUpdate time.Time) ([]*Channel, time.Time, bool, error) {
					stored, err := svc.entFromContext(ctx).Channel.Get(ctx, ch.ID)
					if err != nil {
						return current, lastUpdate, false, err
					}
					return []*Channel{buildChannel(stored, nil)}, time.Now(), true, nil
				},
			})
			tx, err := svc.db.Tx(ctx)
			require.NoError(t, err)
			defer tx.Rollback()
			txCtx := ent.NewContext(ent.NewTxContext(ctx, tx), tx.Client())
			_, err = svc.FreezeChannelModel(txCtx, ch.ID, "new", 0)
			require.NoError(t, err)
			_, err = svc.FreezeChannelModel(txCtx, ch.ID, "second", 0)
			require.NoError(t, err)
			require.False(t, svc.GetEnabledChannel(ch.ID).HasActiveModelCooldown("new", time.Now()), "uncommitted state must not leak into the serving cache")
			if commit {
				require.NoError(t, tx.Commit())
			} else {
				require.NoError(t, tx.Rollback())
			}
			require.Equal(t, commit, svc.GetEnabledChannel(ch.ID).HasActiveModelCooldown("new", time.Now()))
			require.Equal(t, commit, svc.GetEnabledChannel(ch.ID).HasActiveModelCooldown("second", time.Now()))
		})
	}
}

func TestChannelService_ModelCooldownZeroRowsInTransactionIsConflict(t *testing.T) {
	ctx, svc, ch := newWindowRuleTestService(t, objects.APIKeyAutoDisableRule{Times: 1, Action: objects.APIKeyAutoDisableActionPermanent})
	svc.db.Channel.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, mutation ent.Mutation) (ent.Value, error) {
			if mutation.Op().Is(ent.OpUpdateOne) {
				return ch, nil
			}
			return 0, nil
		})
	})
	tx, err := svc.db.Tx(ctx)
	require.NoError(t, err)
	defer tx.Rollback()
	txCtx := ent.NewContext(ent.NewTxContext(ctx, tx), tx.Client())
	_, err = svc.FreezeChannelModel(txCtx, ch.ID, "new", 0)
	require.ErrorIs(t, err, ErrChannelUpdatedConcurrently, "zero affected rows cannot be acknowledged as an applied freeze")
}
