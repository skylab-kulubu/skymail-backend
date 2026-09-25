-- Every function in the schema finds what it calls whatever the caller's
-- search_path. pg_dump's output empties search_path while pg_restore loads
-- the rows, and the templates' CHECKs call contract_variables_valid(), which
-- called contract_variable_names() and required_variable_names_valid()
-- without a schema: every dump with a template in it failed to restore with
-- "function contract_variable_names(jsonb) does not exist". The trigger
-- function and mail_task_status() name tables the same way, and would fail
-- the same in any session whose search_path left public out.
--
-- Since 10.3, PostgreSQL's release notes ask that functions run by pg_dump
-- and pg_restore "not assume anything about what search path they are invoked
-- under". Each function here gets its own, with CREATE FUNCTION's SET clause.
-- That works the same for the SQL and the PL/pgSQL functions, leaves every
-- body as it was written (down only resets it), and covers names added to
-- these bodies later, which schema-qualifying each call would not.
-- pg_catalog comes first, as it does implicitly, so nothing in public can
-- stand in for a built-in. The functions that only call built-ins are pinned
-- too, so the rule is simply every function. A SQL function with a SET clause
-- is no longer inlined into its caller, which costs these small checks
-- nothing that matters.
ALTER FUNCTION mail_task_status(UUID) SET search_path = pg_catalog, public;
ALTER FUNCTION template_jsx_source(TEXT) SET search_path = pg_catalog, public;
ALTER FUNCTION required_variable_names_valid(TEXT[]) SET search_path = pg_catalog, public;
ALTER FUNCTION contract_variable_names(JSONB) SET search_path = pg_catalog, public;
ALTER FUNCTION contract_variables_valid(JSONB) SET search_path = pg_catalog, public;
ALTER FUNCTION check_mail_approval() SET search_path = pg_catalog, public;
