package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChannelPricingDerivedMultipliersMigration(t *testing.T) {
	content, err := FS.ReadFile("229_channel_pricing_derived_multipliers.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")

	columns := []string{"completion_multiplier", "cache_creation_multiplier", "cache_read_multiplier"}
	for _, column := range columns {
		require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS "+column+" NUMERIC(12,6)")

		// 约束是 >= 0 而不是 228 里那批的 > 0：倍率 0 是合法配置（把某档白送），
		// 只有负倍率会算出负费用倒找钱。
		name := "channel_model_pricing_" + strings.TrimSuffix(column, "_multiplier") + "_multiplier_non_negative"
		require.Contains(t, sql, "conname = '"+name+"' AND conrelid = 'channel_model_pricing'::regclass")
		require.Contains(t, sql, "ADD CONSTRAINT "+name+" CHECK ("+column+" IS NULL OR "+column+" >= 0)")
	}

	// 只动 channel_model_pricing。cache_read_multiplier 这个列名在 228 里已经被
	// channel_pricing_intervals 用掉了，且那边语义不同（乘各档基础价）；本迁移若顺手
	// 碰了那张表，两套语义就会在同一列上打架。
	require.NotContains(t, sql, "ALTER TABLE channel_pricing_intervals")
}
