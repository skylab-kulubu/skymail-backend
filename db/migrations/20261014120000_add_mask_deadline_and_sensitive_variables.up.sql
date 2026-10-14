-- Two follow-ups of the #63 review (hub ticket iletisim-servisi 22).
--
-- mail_tasks.content_mask_failing_since: when a send's masking first failed
-- since it was last masked (A1). A sensitive send that has been failing to
-- mask for longer than the sweep's deadline (seven days) is emptied (not
-- kept) and logged as an alarm, whether its failures were counted or not: a
-- queue holding only broken sends counts none, and their raw mail would
-- otherwise be kept for the whole retention period. A send that failed before
-- this migration gets it on its next failure (a day at most).
--
-- templates.sensitive_variables: the variables a template's mail carries a
-- credential in, as the Template seed states them (R3). The sweep masks each
-- one whole, whatever its name or shape; null (the seed said nothing) leaves
-- it to the names and shapes, as before.
--
-- Both columns are new and nullable: no rewrite, and an image from before them
-- neither reads nor writes them (sqlc spells out the columns it reads).
-- lock_timeout as in 20261013140000.
SET lock_timeout = '5s';

ALTER TABLE mail_tasks
    ADD COLUMN content_mask_failing_since TIMESTAMPTZ;

COMMENT ON COLUMN mail_tasks.content_mask_failing_since IS
    'When masking the send first failed since it was last masked; past the sweep''s deadline the send is emptied (not kept).';

ALTER TABLE templates
    ADD COLUMN sensitive_variables TEXT[];

COMMENT ON COLUMN templates.sensitive_variables IS
    'The top-level variables whose values the content sweep masks whole in a sensitive send, as the Template seed states them; null: by name and shape only.';

RESET lock_timeout;
