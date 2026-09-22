ALTER TABLE templates
    DROP CONSTRAINT IF EXISTS templates_key_format;

ALTER TABLE templates
    DROP COLUMN IF EXISTS system,
    DROP COLUMN IF EXISTS key;
