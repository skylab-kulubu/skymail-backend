-- The sets go; the rows and every version stay as they are, so no mail
-- changes. Without them nothing is required of a body any more.
ALTER TABLE templates
    DROP CONSTRAINT IF EXISTS templates_required_variables_disjoint,
    DROP CONSTRAINT IF EXISTS templates_required_variable_names,
    DROP COLUMN IF EXISTS operator_required_variables,
    DROP COLUMN IF EXISTS contract_required_variables;

DROP FUNCTION IF EXISTS required_variable_names_valid(TEXT[]);
