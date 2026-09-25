-- The functions follow the caller's search_path again, as they were written.
ALTER FUNCTION check_mail_approval() RESET search_path;
ALTER FUNCTION contract_variables_valid(JSONB) RESET search_path;
ALTER FUNCTION contract_variable_names(JSONB) RESET search_path;
ALTER FUNCTION required_variable_names_valid(TEXT[]) RESET search_path;
ALTER FUNCTION template_jsx_source(TEXT) RESET search_path;
ALTER FUNCTION mail_task_status(UUID) RESET search_path;
