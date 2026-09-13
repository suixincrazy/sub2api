package schema

import (
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// ProxySource stores user-owned upstream node subscription sources.
type ProxySource struct {
	ent.Schema
}

func (ProxySource) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "proxy_sources"},
	}
}

func (ProxySource) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
		mixins.SoftDeleteMixin{},
	}
}

func (ProxySource) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("owner_user_id").
			Optional().
			Nillable(),
		field.String("name").
			MaxLen(100).
			NotEmpty(),
		field.String("subscription_url").
			SchemaType(map[string]string{dialect.Postgres: "text"}).
			NotEmpty(),
		field.Bool("is_public").
			Default(false).
			Comment("Whether proxies imported from this source are selectable by all users."),
		field.Int("refresh_interval_minutes").
			Default(1440),
		field.Time("last_synced_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.String("last_sync_status").
			MaxLen(20).
			Default("never"),
		field.String("last_sync_error").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "text"}),
		field.Int("last_imported_count").
			Default(0),
		// New fields are appended so the existing ordinals stay put.
		field.Bool("sync_enabled").
			Default(true).
			Comment("Whether the scheduler may refresh this source automatically."),
		field.Int64("sub_traffic_used").
			Default(0).
			Comment("Upload+download reported by the subscription-userinfo header, in bytes."),
		field.Int64("sub_traffic_total").
			Default(0).
			Comment("Plan quota reported by the subscription-userinfo header, in bytes; 0 means unknown."),
		field.Time("sub_expires_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).
			Comment("Plan expiry reported by the subscription-userinfo header."),
		field.Time("sub_info_updated_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).
			Comment("When the subscription-userinfo snapshot above was last refreshed."),
	}
}

func (ProxySource) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("owner_user_id"),
		index.Fields("owner_user_id", "deleted_at"),
		index.Fields("last_sync_status"),
	}
}
