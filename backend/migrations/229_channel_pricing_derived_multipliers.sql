-- 派生定价倍率：只维护一个提示价，补全/缓存创建/缓存读取按倍率跟着走。
-- 例：提示 $4/MTok + 补全倍率 5 + 缓存创建倍率 1.25 + 缓存读取倍率 0.2
--     => 补全 $20/MTok、缓存创建 $5/MTok、缓存读取 $0.80/MTok
--
-- 与 channel_pricing_intervals 上的同名列语义不同：这三列乘的是「有效提示价」，
-- 区间那三列乘的是各档自己的基础价。两者互不影响。
--
-- 全部可空。NULL = 未配置 = 走原有绝对价路径，行为与本迁移前逐位一致。
ALTER TABLE channel_model_pricing
    ADD COLUMN IF NOT EXISTS completion_multiplier NUMERIC(12,6),
    ADD COLUMN IF NOT EXISTS cache_creation_multiplier NUMERIC(12,6),
    ADD COLUMN IF NOT EXISTS cache_read_multiplier NUMERIC(12,6);

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'channel_model_pricing_completion_multiplier_non_negative' AND conrelid = 'channel_model_pricing'::regclass) THEN
        ALTER TABLE channel_model_pricing
            ADD CONSTRAINT channel_model_pricing_completion_multiplier_non_negative
            CHECK (completion_multiplier IS NULL OR completion_multiplier >= 0);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'channel_model_pricing_cache_creation_multiplier_non_negative' AND conrelid = 'channel_model_pricing'::regclass) THEN
        ALTER TABLE channel_model_pricing
            ADD CONSTRAINT channel_model_pricing_cache_creation_multiplier_non_negative
            CHECK (cache_creation_multiplier IS NULL OR cache_creation_multiplier >= 0);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'channel_model_pricing_cache_read_multiplier_non_negative' AND conrelid = 'channel_model_pricing'::regclass) THEN
        ALTER TABLE channel_model_pricing
            ADD CONSTRAINT channel_model_pricing_cache_read_multiplier_non_negative
            CHECK (cache_read_multiplier IS NULL OR cache_read_multiplier >= 0);
    END IF;
END $$;

COMMENT ON COLUMN channel_model_pricing.completion_multiplier IS
    'Completion price = effective prompt price x this multiplier; ignored when output_price is set';
COMMENT ON COLUMN channel_model_pricing.cache_creation_multiplier IS
    'Cache-creation price = effective prompt price x this multiplier; ignored when cache_write_price is set';
COMMENT ON COLUMN channel_model_pricing.cache_read_multiplier IS
    'Cache-read price = effective prompt price x this multiplier; ignored when cache_read_price is set';
