ALTER TABLE templates
    ADD COLUMN key TEXT UNIQUE,
    ADD COLUMN system BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE templates
    ADD CONSTRAINT templates_key_format
        CHECK (key IS NULL OR key ~ '^[a-z0-9][a-z0-9.-]{1,62}[a-z0-9]$');
