-- A Mail template version is a snapshot of a Mail template (ADR-0046): its
-- subject, at most one source per Authoring mode, which of them is the Main
-- source, and the Main source's render as HTML and plain text — written with
-- who wrote it, an operator or a Template seed. A version with no published_at
-- is a draft.
--
-- The templates row keeps a copy of the published version: subject,
-- html_content and plain_text_content, and react_email_content for the old
-- panel, which edits JSX there. Publishing is copying a version onto the row,
-- and published_version_id says which version the copy is of. The send path
-- reads only the row, as it always has, so it never sees a draft. Rendering
-- stays in the browser and the Template seed: the server stores what they send.

CREATE TYPE authoring_mode AS ENUM ('jsx', 'visual', 'html');

CREATE TYPE template_author_kind AS ENUM ('operator', 'template_seed');

CREATE TABLE template_versions
(
    id                 UUID PRIMARY KEY              DEFAULT gen_random_uuid(),
    -- Templates are archived, never deleted (ADR-0042), and neither is their
    -- history: no cascade.
    template_id        UUID                 NOT NULL REFERENCES templates (id),
    -- 1, 2, 3… per template, in the order versions were written. Writers take
    -- the template row's lock before numbering, so the order is the write order.
    seq                INT                  NOT NULL CHECK (seq > 0),
    subject            TEXT                 NOT NULL,
    -- The subject a Template seed sent, beside the one its version got: until
    -- the seed's conflict rule (ticket 09) the by-key upsert keeps the row's
    -- subject on an existing key, so the two can differ. Null on an operator's
    -- version, and on the migration's first versions, whose seed payload is not
    -- known.
    requested_subject  TEXT,
    jsx_source         TEXT,
    visual_source      JSONB CHECK (jsonb_typeof(visual_source) = 'object'),
    html_source        TEXT,
    main_mode          authoring_mode       NOT NULL,
    -- The Main source's render, as the browser or the seed produced it.
    html_content       TEXT                 NOT NULL,
    plain_text_content TEXT                 NOT NULL,
    author_kind        template_author_kind NOT NULL,
    -- The Keycloak subject of the token that wrote the version, and the name
    -- it carried then: there is no user directory to look a name up in later.
    -- Both are null for the versions the migration below made from rows
    -- written before anyone recorded who wrote them.
    author_sub         TEXT,
    author_name        TEXT,
    created_at         TIMESTAMPTZ          NOT NULL DEFAULT NOW(),
    published_at       TIMESTAMPTZ,
    -- The published version a draft started from.
    base_version_id    UUID,
    -- Also the index a template's history is read newest first by.
    CONSTRAINT template_versions_template_seq UNIQUE (template_id, seq),
    -- Lets a reference name the template alongside the version, so the
    -- foreign keys below can require a version of the same template.
    CONSTRAINT template_versions_template_version UNIQUE (template_id, id),
    CONSTRAINT template_versions_main_source_present CHECK (
        (main_mode = 'jsx' AND jsx_source IS NOT NULL)
            OR (main_mode = 'visual' AND visual_source IS NOT NULL)
            OR (main_mode = 'html' AND html_source IS NOT NULL)
        ),
    CONSTRAINT template_versions_published_after_created CHECK (published_at IS NULL OR published_at >= created_at),
    CONSTRAINT template_versions_requested_subject_by_seed CHECK (requested_subject IS NULL OR author_kind = 'template_seed'),
    CONSTRAINT template_versions_base_same_template
        FOREIGN KEY (template_id, base_version_id) REFERENCES template_versions (template_id, id)
);

ALTER TABLE templates
    ADD COLUMN published_version_id UUID,
    ADD CONSTRAINT templates_published_version_same_template
        FOREIGN KEY (id, published_version_id) REFERENCES template_versions (template_id, id);

-- A version as a template's history lists it: everything but its sources and
-- render, and whether it is the one the row is a copy of — the one sent. The
-- one definition of "current", for the list and for reading one version.
CREATE VIEW template_version_summaries AS
SELECT v.id,
       v.template_id,
       v.seq,
       v.subject,
       v.requested_subject,
       v.main_mode,
       v.author_kind,
       v.author_sub,
       v.author_name,
       v.created_at,
       v.published_at,
       v.base_version_id,
       COALESCE(v.id = t.published_version_id, false)::boolean AS is_current
FROM template_versions v
         JOIN templates t ON t.id = v.template_id;

-- The JSX source react_email_content holds, or NULL when it holds none. The
-- Template seed writes a pointer comment there instead of the .tsx source —
-- "// Kaynak: skymail-frontend/emails/<key>.tsx — burada düzenlersen …" — and a
-- comment is not a source: nothing in it renders. So the rule is that content
-- is a JSX source when something other than whitespace is left once /* */
-- block comments and // line comments are taken out. Empty, blank and
-- comment-only content, the seed's pointer among them, is not one; anything
-- with code in it is, whatever comments or URLs surround the code.
--
-- Both kinds come out in one left-to-right pass, so whichever starts first
-- wins, as in JavaScript: "// eski /*" is a line comment, "/* // */" a block.
-- The block pattern cannot run past its first */ by construction. A lazy .*?
-- would not do: in PostgreSQL a pattern with a top-level | is greedy as a
-- whole, and "/* a */ code /* b */" would lose its code.
--
-- The rows written before versioning are read through it below, and so are the
-- rows the old panel and the by-key upsert write until they send each
-- Authoring mode's source themselves.
CREATE FUNCTION template_jsx_source(react_email_content TEXT) RETURNS TEXT
    LANGUAGE sql
    IMMUTABLE
    STRICT
AS
$$
SELECT CASE
           WHEN regexp_replace(react_email_content, '/\*([^*]|\*+[^*/])*\*+/|//[^\n]*', '', 'g') ~ '\S'
               THEN react_email_content
           END
$$;

-- Every existing template gets exactly one published first version from its
-- row. A template with a Template key was written by the Template seed, one
-- without by an operator; who exactly is not known. With no JSX source, the
-- Main source is HTML and that source is html_content. The version is dated
-- when the row was last written — for an archived template, when it was
-- archived — the closest the row comes to when its content was.
INSERT INTO template_versions (template_id, seq, subject, jsx_source, html_source, main_mode,
                               html_content, plain_text_content, author_kind, created_at, published_at)
SELECT t.id,
       1,
       t.subject,
       source.jsx,
       CASE WHEN source.jsx IS NULL THEN t.html_content END,
       CASE WHEN source.jsx IS NULL THEN 'html' ELSE 'jsx' END::authoring_mode,
       t.html_content,
       t.plain_text_content,
       CASE WHEN t.key IS NULL THEN 'operator' ELSE 'template_seed' END::template_author_kind,
       t.updated_at,
       t.updated_at
FROM templates t
         CROSS JOIN LATERAL (SELECT template_jsx_source(t.react_email_content) AS jsx) source;

UPDATE templates t
SET published_version_id = v.id
FROM template_versions v
WHERE v.template_id = t.id;
