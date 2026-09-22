-- A Mail template's Required variables: the variables its body must keep
-- referencing, because the mail cannot do its job without them (the reset link,
-- the certificate's VerifyURL). Two sets, both on the template rather than on a
-- version:
--
--   * contract_required_variables — declared in the repo beside the Template
--     key, from the sending service's contract, and written only by the
--     Template seed. Like the key, SkyMail's panel cannot change it.
--   * operator_required_variables — marked and released by operators.
--
-- They belong to the template because they constrain every version: a restored
-- or drafted version is checked against today's sets, so going back in history
-- cannot drop a variable the sender relies on, and versions need not repeat
-- them. A name is in at most one set; the contract's wins.

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

ALTER TABLE templates
    ADD COLUMN contract_required_variables TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN operator_required_variables TEXT[] NOT NULL DEFAULT '{}',
    ADD CONSTRAINT templates_required_variable_names
        CHECK (required_variable_names_valid(contract_required_variables)
            AND required_variable_names_valid(operator_required_variables)),
    ADD CONSTRAINT templates_required_variables_disjoint
        CHECK (NOT (contract_required_variables && operator_required_variables));

COMMENT ON COLUMN templates.contract_required_variables IS
    'Required variables from the sending service''s contract, sorted. Written only by the Template seed; locked in the panel.';
COMMENT ON COLUMN templates.operator_required_variables IS
    'Required variables operators marked, sorted. Never shares a name with contract_required_variables.';
