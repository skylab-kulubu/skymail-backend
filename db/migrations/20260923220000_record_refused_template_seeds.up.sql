-- A Template seed that would overwrite an operator's change is refused unless
-- forced (ADR-0047), and a refused seed writes nothing — not even a version: it
-- is not part of the template's history, and an unpublished seed version would
-- also count as "the last seed" and let the next one through. What is kept is
-- that it was refused, on the template row, so the panel can say a repo change
-- is waiting on a decision:
--
--   * seed_refused_at — when the content the seed asked to write was first
--     refused; a refusal of the same content again keeps it, other content
--     restarts it;
--   * seed_refused_rules — which of the conflict rules held, the last time;
--   * seed_refused_payload_sha256 — a hash of that content, which is how the
--     same content is told apart from other content.
--
-- The next seed that goes through, forced or not, clears all three.
ALTER TABLE templates
    ADD COLUMN seed_refused_at             TIMESTAMPTZ,
    ADD COLUMN seed_refused_rules          TEXT[],
    ADD COLUMN seed_refused_payload_sha256 TEXT,
    ADD CONSTRAINT templates_seed_refusal_whole CHECK (
        (seed_refused_at IS NULL) = (seed_refused_rules IS NULL)
            AND (seed_refused_at IS NULL) = (seed_refused_payload_sha256 IS NULL)
        ),
    ADD CONSTRAINT templates_seed_refused_rules_known CHECK (
        cardinality(seed_refused_rules) > 0
            AND seed_refused_rules <@ ARRAY ['published_by_operator', 'newer_operator_version', 'operator_subject']
        ),
    ADD CONSTRAINT templates_seed_refused_payload_sha256_hex CHECK (seed_refused_payload_sha256 ~ '^[0-9a-f]{64}$');
