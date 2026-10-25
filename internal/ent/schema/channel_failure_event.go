package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// ChannelFailureEvent stores one auto-disable rule failure per row so the
// rolling failure window survives restarts and is shared by every server
// instance. Counting is a plain COUNT over the composite indexes, so concurrent
// instances merge naturally instead of keeping isolated in-process counters.
type ChannelFailureEvent struct {
	ent.Schema
}

func (ChannelFailureEvent) Mixin() []ent.Mixin {
	return []ent.Mixin{
		TimeMixin{}, // Provides created_at, updated_at
	}
}

func (ChannelFailureEvent) Annotations() []schema.Annotation {
	return []schema.Annotation{
		// Internal housekeeping table; intentionally not exposed via GraphQL.
		entgql.Skip(entgql.SkipAll),
	}
}

func (ChannelFailureEvent) Indexes() []ent.Index {
	return []ent.Index{
		// Hot path: count the failures of one rule inside the rolling window.
		index.Fields("rule_key_hash", "failed_at").
			StorageKey("channel_failure_events_by_rule_key_hash_failed_at"),
		// The model window aggregates every matching rule of a channel/model pair
		// for the sixth-failure permanent guard.
		index.Fields("window_key_hash", "failed_at").
			StorageKey("channel_failure_events_by_window_key_hash_failed_at"),
		// Periodic cleanup deletes everything older than the longest window.
		index.Fields("failed_at").
			StorageKey("channel_failure_events_by_failed_at"),
	}
}

func (ChannelFailureEvent) Fields() []ent.Field {
	return []ent.Field{
		field.Int("channel_id").
			Comment("Channel that produced the failure"),
		field.String("model").
			MaxLen(255).
			Comment("Original request model used for rule evaluation"),
		// The counter keys encode the full rule identity (channel, model, scope,
		// rule index and every rule field). They are hashed for indexing because
		// keyword patterns make their length unbounded.
		field.String("rule_key").
			MaxLen(1024).
			Comment("Full auto-disable rule counter key; truncated when longer"),
		field.String("rule_key_hash").
			MaxLen(64).
			Comment("SHA-256 hex digest of the rule counter key"),
		field.String("window_key").
			MaxLen(1024).
			Comment("Shared channel/model/window key; truncated when longer"),
		field.String("window_key_hash").
			MaxLen(64).
			Comment("SHA-256 hex digest of the shared window key"),
		field.Int("status_code").
			Default(0).
			Comment("HTTP status code of the failed request"),
		field.Text("error_detail").
			Comment("Redacted one-line error detail captured for the failure reason"),
		field.Time("failed_at").
			Comment("When the failed request finished; failures may arrive out of order"),
	}
}