-- Proof that one Erasure command finished (ADR-0051; account erasure spec
-- §2.3): core's deletion request id, when SkyMail finished it, and how many
-- rows each step changed. It holds no subject and no address, so it can be
-- kept as the deletion record for at least three years; no code path deletes
-- it. A command whose receipt is here is not done again: the stored answer is
-- given back as it was.
CREATE TABLE account_erasure_receipts
(
    request_id   UUID PRIMARY KEY,
    completed_at TIMESTAMPTZ NOT NULL,
    -- {"recipients_deleted": 1, …}: snake_case names, non-negative counts.
    counts       JSONB       NOT NULL,
    CONSTRAINT account_erasure_receipts_counts_object CHECK (jsonb_typeof(counts) = 'object')
);
