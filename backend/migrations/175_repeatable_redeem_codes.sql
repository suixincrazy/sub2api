ALTER TABLE redeem_codes ADD COLUMN IF NOT EXISTS max_uses INT NOT NULL DEFAULT 1;
ALTER TABLE redeem_codes ADD COLUMN IF NOT EXISTS used_count INT NOT NULL DEFAULT 0;

UPDATE redeem_codes
SET used_count = CASE WHEN status = 'used' THEN 1 ELSE 0 END,
    max_uses = GREATEST(max_uses, 1)
WHERE used_count = 0;

ALTER TABLE redeem_codes DROP CONSTRAINT IF EXISTS redeem_codes_max_uses_positive;
ALTER TABLE redeem_codes ADD CONSTRAINT redeem_codes_max_uses_positive CHECK (max_uses >= 1);
ALTER TABLE redeem_codes DROP CONSTRAINT IF EXISTS redeem_codes_used_count_range;
ALTER TABLE redeem_codes ADD CONSTRAINT redeem_codes_used_count_range CHECK (used_count >= 0 AND used_count <= max_uses);

CREATE TABLE IF NOT EXISTS redeem_code_usages (
    id BIGSERIAL PRIMARY KEY,
    redeem_code_id BIGINT NOT NULL REFERENCES redeem_codes(id) ON DELETE CASCADE,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    used_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT redeem_code_usages_code_user_unique UNIQUE (redeem_code_id, user_id)
);

INSERT INTO redeem_code_usages (redeem_code_id, user_id, used_at)
SELECT id, used_by, COALESCE(used_at, NOW())
FROM redeem_codes
WHERE used_by IS NOT NULL
ON CONFLICT (redeem_code_id, user_id) DO NOTHING;

CREATE INDEX IF NOT EXISTS idx_redeem_code_usages_code_id ON redeem_code_usages(redeem_code_id);
CREATE INDEX IF NOT EXISTS idx_redeem_code_usages_user_id ON redeem_code_usages(user_id);
CREATE INDEX IF NOT EXISTS idx_redeem_codes_owner_usage ON redeem_codes(owner_user_id, used_count);
