package mailer_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// maskStore is a migrated database with n sensitive sends, done an hour ago,
// each of one recipient whose mail holds a code.
func maskStore(t *testing.T, n int) (*database.Store, []uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	var templateID uuid.UUID
	if err := postgres.Pool.QueryRow(ctx, `
		INSERT INTO templates (key, name, subject, html_content, plain_text_content, react_email_content, content_sensitive,
		                       created_at, updated_at)
		VALUES ('keycloak.personal-email-confirm', 'Kod', 'Kodun {{.code}}', '<p>{{.code}}</p>', '{{.code}}', '', true,
		        now() - interval '2 hours', now() - interval '2 hours')
		RETURNING id`).Scan(&templateID); err != nil {
		t.Fatal(err)
	}
	var tasks []uuid.UUID
	for i := 0; i < n; i++ {
		code := fmt.Sprintf("%06d", 482910+i)
		var task uuid.UUID
		if err := postgres.Pool.QueryRow(ctx, `
			INSERT INTO mail_tasks (sent_by, template_id, body_variables, created_at, content_sensitive_at_send)
			VALUES ('s', $1, jsonb_build_object('code', $2::text), now() - interval '1 hour' + $3 * interval '1 second', 'true')
			RETURNING id`, templateID, code, i).Scan(&task); err != nil {
			t.Fatal(err)
		}
		if _, err := postgres.Pool.Exec(ctx, `
			INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, body_html, status, created_at)
			VALUES ($1, 'Ayşe Kaya', 'ayse@example.com', 'Kodun ' || $2, $2, '<p>' || $2 || '</p>', 'sent', now() - interval '1 hour')`,
			task, code); err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task)
	}
	return database.NewStore(postgres.Pool), tasks
}

func failuresOf(t *testing.T, db *database.Store, task uuid.UUID) (failures int16, masked bool) {
	t.Helper()
	if err := db.Conn.QueryRow(context.Background(),
		`SELECT content_mask_failures, content_masked_at IS NOT NULL FROM mail_tasks WHERE id = $1`, task).Scan(&failures, &masked); err != nil {
		t.Fatal(err)
	}
	return failures, masked
}

// A send whose masking panics is that send's failure: the sweep goes on with
// the others and the process does not crash (#63 Ek 2, Y4).
func TestAPanicMaskingOneSendIsThatSendsFailure(t *testing.T) {
	db, tasks := maskStore(t, 2)
	restore := mailer.SetMaskSendHook(func(id uuid.UUID) {
		if id == tasks[0] {
			panic("a mail the masking cannot take")
		}
	})
	defer restore()

	result, err := mailer.MaskSensitiveContent(context.Background(), db, mailer.MaskOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Failed != 1 || result.Sends != 1 || result.BreakerTripped {
		t.Fatalf("run = %+v, want one failure and one send masked", result)
	}
	if failures, masked := failuresOf(t, db, tasks[0]); failures != 1 || masked {
		t.Errorf("panicking send: failures %d, masked %v; want 1, false", failures, masked)
	}
	if _, masked := failuresOf(t, db, tasks[1]); !masked {
		t.Error("the other send was not masked")
	}
}

// Five sends failing one after another is SkyMail's fault, not theirs: the
// run stops and counts none of the failures, so a fault across the service
// does not empty everyone's content (#63 Ek 2, Y3).
func TestSendsFailingOneAfterAnotherTripTheBreaker(t *testing.T) {
	db, tasks := maskStore(t, 7)
	restore := mailer.SetMaskSendHook(func(uuid.UUID) { panic("disk full") })
	defer restore()

	result, err := mailer.MaskSensitiveContent(context.Background(), db, mailer.MaskOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.BreakerTripped || result.Failed != 5 {
		t.Fatalf("run = %+v, want the breaker tripped after five failures", result)
	}
	for _, task := range tasks {
		if failures, masked := failuresOf(t, db, task); failures != 0 || masked {
			t.Errorf("%s: failures %d, masked %v; want none counted", task, failures, masked)
		}
	}
}
