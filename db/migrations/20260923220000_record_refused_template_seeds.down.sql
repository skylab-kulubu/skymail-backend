-- Whether a seed was refused goes; the templates and their versions stay as
-- they are.
ALTER TABLE templates
    DROP CONSTRAINT IF EXISTS templates_seed_refused_payload_sha256_hex,
    DROP CONSTRAINT IF EXISTS templates_seed_refused_rules_known,
    DROP CONSTRAINT IF EXISTS templates_seed_refusal_whole,
    DROP COLUMN IF EXISTS seed_refused_payload_sha256,
    DROP COLUMN IF EXISTS seed_refused_rules,
    DROP COLUMN IF EXISTS seed_refused_at;
