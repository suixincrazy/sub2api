-- Convert one-shot scheduled tests to persistent probe/keepalive schedules.
-- Keep cron_expression for older API clients; intervals now control scheduling.
ALTER TABLE scheduled_test_plans
    ADD COLUMN IF NOT EXISTS probe_interval_seconds INTEGER NOT NULL DEFAULT 2 CHECK (probe_interval_seconds BETWEEN 1 AND 86400),
    ADD COLUMN IF NOT EXISTS keepalive_interval_seconds INTEGER NOT NULL DEFAULT 60 CHECK (keepalive_interval_seconds BETWEEN 1 AND 86400),
    ADD COLUMN IF NOT EXISTS keepalive_max_interval_seconds INTEGER NOT NULL DEFAULT 90 CHECK (keepalive_max_interval_seconds BETWEEN keepalive_interval_seconds AND 86400),
    ADD COLUMN IF NOT EXISTS last_status VARCHAR(20) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS consecutive_failures INTEGER NOT NULL DEFAULT 0;

UPDATE scheduled_test_plans SET next_run_at = CASE WHEN enabled THEN NOW() ELSE NULL END;
