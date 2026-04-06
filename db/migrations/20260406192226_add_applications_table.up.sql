CREATE TABLE applications
(
    id            UUID PRIMARY KEY     DEFAULT gen_random_uuid(),
    name          TEXT        NOT NULL,
    token_version INT         NOT NULL DEFAULT 0,
    owner_id      TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
