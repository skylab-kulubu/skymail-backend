-- A Mail template's Required variables: the variables its body must keep
-- referencing, because the mail cannot do its job without them (the reset link,
-- the certificate's VerifyURL). Two sets, both on the template rather than on a
-- version:
--
--   * contract_required_variables — declared in the repo beside the Template
--     key, from the sending service's contract, and written only by the
--     Template seed. Like the key, SkyMail's panel cannot change it. Each
--     entry carries why the mail needs it: [{"name": "link", "reason": "…"}],
--     the reason null when the seed sent the name alone.
--   * operator_required_variables — names operators marked and can release.
--
-- They belong to the template because they constrain every version: a restored
-- or drafted version is checked against today's sets, so going back in history
-- cannot drop a variable the sender relies on, and versions need not repeat
-- them. A name is in at most one set; the contract's wins. Both are kept sorted
-- by name, byte by byte.

-- A variable name as a body reaches it with .Name: letters, digits and
-- underscores, not starting with a digit.
CREATE FUNCTION required_variable_names_valid(names TEXT[]) RETURNS BOOLEAN
    LANGUAGE sql
    IMMUTABLE
AS
$$
SELECT array_position(names, NULL) IS NULL
   AND NOT EXISTS (SELECT 1 FROM unnest(names) AS name WHERE name !~ '^[A-Za-z_][A-Za-z0-9_]{0,63}$')
$$;

-- The names of a contract set, in its order.
CREATE FUNCTION contract_variable_names(variables JSONB) RETURNS TEXT[]
    LANGUAGE sql
    IMMUTABLE
AS
$$
SELECT COALESCE(array_agg(variable ->> 'name' ORDER BY position), '{}')
FROM jsonb_array_elements(variables) WITH ORDINALITY AS entry(variable, position)
$$;

-- A contract set as the seed writes one: an array of objects, each with a
-- well-formed name no other entry has, and a reason that is text or null.
CREATE FUNCTION contract_variables_valid(variables JSONB) RETURNS BOOLEAN
    LANGUAGE sql
    IMMUTABLE
AS
$$
SELECT CASE
           WHEN jsonb_typeof(variables) IS DISTINCT FROM 'array' THEN false
           ELSE NOT EXISTS (SELECT 1
                            FROM jsonb_array_elements(variables) AS variable
                            WHERE jsonb_typeof(variable) <> 'object'
                               OR jsonb_typeof(variable -> 'name') IS DISTINCT FROM 'string'
                               OR COALESCE(jsonb_typeof(variable -> 'reason'), 'null') NOT IN ('string', 'null'))
               AND required_variable_names_valid(contract_variable_names(variables))
               AND (SELECT count(DISTINCT name) = count(*) FROM unnest(contract_variable_names(variables)) AS name)
           END
$$;

ALTER TABLE templates
    ADD COLUMN contract_required_variables JSONB NOT NULL DEFAULT '[]',
    ADD COLUMN operator_required_variables TEXT[] NOT NULL DEFAULT '{}',
    ADD CONSTRAINT templates_contract_required_variables_valid
        CHECK (contract_variables_valid(contract_required_variables)),
    ADD CONSTRAINT templates_operator_required_variables_valid
        CHECK (required_variable_names_valid(operator_required_variables)),
    ADD CONSTRAINT templates_required_variables_disjoint
        CHECK (NOT (contract_variable_names(contract_required_variables) && operator_required_variables));

COMMENT ON COLUMN templates.contract_required_variables IS
    'Required variables from the sending service''s contract, sorted by name, each with why the mail needs it. Written only by the Template seed; locked in the panel.';
COMMENT ON COLUMN templates.operator_required_variables IS
    'Required variables operators marked, sorted. Never shares a name with contract_required_variables.';
