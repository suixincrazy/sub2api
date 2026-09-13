-- Preserve operator-defined branding while updating untouched installations.
UPDATE settings
SET value = 'Sub2API Xray', updated_at = NOW()
WHERE key = 'site_name' AND value = 'Sub2API';
