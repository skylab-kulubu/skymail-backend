package handlers

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
)

// An address Postgres's lower() finds twice where Go's strings.ToLower did
// not is still the caller's duplicate: a 400, not a 500. Any other violation
// passes through as it came.
func TestADuplicatePostgresFindsIsTheCallersDuplicate(t *testing.T) {
	clash := &pgconn.PgError{Code: "23505", ConstraintName: "mail_approval_recipients_email_once"}
	var appErr *apperrors.AppError
	if err := duplicateRecipientsError(clash); !errors.As(err, &appErr) || appErr.Code != "validation.error" {
		t.Fatalf("a clash on the address index = %v, want validation.error", err)
	}
	if got, _ := json.Marshal(appErr.Params["errors"]); string(got) != `[{"field":"recipients","code":"duplicate"}]` {
		t.Errorf("errors = %s", got)
	}

	other := &pgconn.PgError{Code: "23505", ConstraintName: "mail_approval_recipients_pkey"}
	if err := duplicateRecipientsError(other); err != error(other) {
		t.Errorf("another unique violation = %v, want it unchanged", err)
	}
	if err := duplicateRecipientsError(nil); err != nil {
		t.Errorf("no error = %v", err)
	}
}
