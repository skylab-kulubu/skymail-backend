-- Team mail (ADR-0067 addendum of 2026-11-08; hub tickets ekip-postasi 03 and
-- 04): a Leader's Mail onayı request to one or more of the Teams core says
-- they lead. team_ids holds the picked Teams in path order, a picked Sub-team
-- of a picked Team already dropped; each is its own send once approved, with
-- its own category (team:<id>:duyuru). NULL: not team mail.
--
-- The request's mail_list_id is the first Team's group, so it still goes to
-- exactly one audience as check_mail_approval() holds it (a Keycloak group),
-- and an image from before this one, which still runs during a start-first
-- deploy, reads it as a send to that group. SKYMAIL_TEAM_SEND is off until
-- the new image runs, so no such request exists before.
--
-- Expand only: a nullable column is a catalog change, and the CHECK reads
-- only the new column on the rows that have it (none yet).
SET lock_timeout = '5s';

ALTER TABLE mail_approvals
    ADD COLUMN team_ids UUID[];

ALTER TABLE mail_approvals
    ADD CONSTRAINT mail_approvals_team_ids CHECK (
        team_ids IS NULL
        OR (cardinality(team_ids) BETWEEN 1 AND 50
            AND array_position(team_ids, NULL) IS NULL
            AND mail_list_id = team_ids[1]
            AND form_source_id IS NULL));

COMMENT ON COLUMN mail_approvals.team_ids IS
    'Team mail: the picked Teams (Keycloak group ids) in path order, each its own send with category team:<id>:duyuru; mail_list_id is the first. NULL: not team mail.';

RESET lock_timeout;
