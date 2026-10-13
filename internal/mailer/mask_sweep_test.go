package mailer_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// maskSends is a migrated database to which add queues sensitive sends, done
// an hour ago (each a second after the last), each of one recipient whose
// mail holds a code.
type maskSends struct {
	t        *testing.T
	db       *database.Store
	template uuid.UUID
	n        int
}

func newMaskSends(t *testing.T) *maskSends {
	t.Helper()
	ctx := context.Background()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	s := &maskSends{t: t, db: database.NewStore(postgres.Pool)}
	if err := postgres.Pool.QueryRow(ctx, `
		INSERT INTO templates (key, name, subject, html_content, plain_text_content, react_email_content, content_sensitive,
		                       created_at, updated_at)
		VALUES ('keycloak.personal-email-confirm', 'Kod', 'Kodun {{.code}}', '<p>{{.code}}</p>', '{{.code}}', '', true,
		        now() - interval '2 hours', now() - interval '2 hours')
		RETURNING id`).Scan(&s.template); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *maskSends) add(count int) []uuid.UUID {
	s.t.Helper()
	ctx := context.Background()
	var tasks []uuid.UUID
	for i := 0; i < count; i++ {
		code := fmt.Sprintf("%06d", 482910+s.n)
		var task uuid.UUID
		if err := s.db.Conn.QueryRow(ctx, `
			INSERT INTO mail_tasks (sent_by, template_id, body_variables, created_at, content_sensitive_at_send)
			VALUES ('s', $1, jsonb_build_object('code', $2::text), now() - interval '1 hour' + $3 * interval '1 second', 'true')
			RETURNING id`, s.template, code, s.n).Scan(&task); err != nil {
			s.t.Fatal(err)
		}
		if _, err := s.db.Conn.Exec(ctx, `
			INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, body_html, status, created_at)
			VALUES ($1, 'Ayşe Kaya', 'ayse@example.com', 'Kodun ' || $2, $2, '<p>' || $2 || '</p>', 'sent', now() - interval '1 hour')`,
			task, code); err != nil {
			s.t.Fatal(err)
		}
		s.n++
		tasks = append(tasks, task)
	}
	return tasks
}

type maskState struct {
	failures, unconfirmed int16
	masked, emptied       bool
}

func (s *maskSends) state(task uuid.UUID) maskState {
	s.t.Helper()
	var st maskState
	if err := s.db.Conn.QueryRow(context.Background(), `
		SELECT content_mask_failures, content_mask_unconfirmed, content_masked_at IS NOT NULL, content_cleared_at IS NOT NULL
		FROM mail_tasks WHERE id = $1`, task).Scan(&st.failures, &st.unconfirmed, &st.masked, &st.emptied); err != nil {
		s.t.Fatal(err)
	}
	return st
}

func (s *maskSends) run(retryAfter time.Duration) mailer.MaskResult {
	s.t.Helper()
	result, err := mailer.MaskSensitiveContent(context.Background(), s.db, mailer.MaskOptions{RetryAfter: retryAfter})
	if err != nil {
		s.t.Fatal(err)
	}
	return result
}

// failFor makes masking panic for the given sends.
func failFor(t *testing.T, tasks ...uuid.UUID) {
	broken := map[uuid.UUID]bool{}
	for _, task := range tasks {
		broken[task] = true
	}
	t.Cleanup(mailer.SetMaskSendHook(func(id uuid.UUID) {
		if broken[id] {
			panic("a mail the masking cannot take")
		}
	}))
}

// A send whose masking panics is that send's failure: the sweep goes on with
// the others and the process does not crash (#63 Ek 2, Y4). Another send was
// masked, so the failure is counted.
func TestAPanicMaskingOneSendIsThatSendsFailure(t *testing.T) {
	s := newMaskSends(t)
	tasks := s.add(2)
	failFor(t, tasks[0])

	result := s.run(time.Hour)
	if result.Failed != 1 || result.Sends != 1 || result.BreakerTripped || result.FailuresUnconfirmed {
		t.Fatalf("run = %+v, want one failure, counted, and one send masked", result)
	}
	if st := s.state(tasks[0]); st.failures != 1 || st.unconfirmed != 0 || st.masked {
		t.Errorf("panicking send: %+v; want one failure counted, not masked", st)
	}
	if st := s.state(tasks[1]); !st.masked {
		t.Error("the other send was not masked")
	}
}

// Five sends failing one after another stop the run; with no send masked the
// fault may be SkyMail's, so none of the failures is counted (#63 Ek 2, Y3).
func TestSendsFailingOneAfterAnotherTripTheBreaker(t *testing.T) {
	s := newMaskSends(t)
	tasks := s.add(7)
	failFor(t, tasks...)

	result := s.run(time.Hour)
	if !result.BreakerTripped || result.Failed != 5 || !result.FailuresUnconfirmed {
		t.Fatalf("run = %+v, want the breaker tripped after five failures, none counted", result)
	}
	for _, task := range tasks {
		if st := s.state(task); st.failures != 0 || st.masked || st.emptied {
			t.Errorf("%s: %+v; want no failure counted", task, st)
		}
	}
}

// Five broken sends at the head of the queue trip the breaker, but they are
// put back: the next run masks the sound send behind them, which shows the
// fault is theirs, and counts their failures; three such runs empty them (#63
// Ek 3, Z1).
func TestBrokenSendsAtTheHeadOfTheQueueHoldNoOtherBack(t *testing.T) {
	s := newMaskSends(t)
	broken := s.add(5)
	sound := s.add(1)[0]
	failFor(t, broken...)

	first := s.run(time.Millisecond)
	if !first.BreakerTripped || first.Sends != 0 || !first.FailuresUnconfirmed {
		t.Fatalf("first run = %+v, want the breaker tripped before the sound send, nothing counted", first)
	}
	if st := s.state(sound); st.masked {
		t.Fatal("the sound send was masked in the first run")
	}
	for _, task := range broken {
		if st := s.state(task); st.failures != 0 || st.unconfirmed != 1 {
			t.Errorf("broken %s after the first run: %+v; want one failure waiting", task, st)
		}
	}

	time.Sleep(50 * time.Millisecond)
	second := s.run(time.Millisecond)
	if second.Sends != 1 || second.FailuresUnconfirmed {
		t.Fatalf("second run = %+v, want the sound send masked and the failures counted", second)
	}
	if st := s.state(sound); !st.masked {
		t.Fatal("the sound send was not masked in the second run")
	}
	for _, task := range broken {
		if st := s.state(task); st.failures != 2 || st.unconfirmed != 0 {
			t.Errorf("broken %s after the second run: %+v; want two failures counted", task, st)
		}
	}

	// A third run, with a new sound send to mask, counts the third failure:
	// the broken sends are emptied.
	time.Sleep(50 * time.Millisecond)
	s.add(1)
	third := s.run(time.Millisecond)
	if third.Sends != 1 || third.NotKept != 5 {
		t.Fatalf("third run = %+v, want the new send masked and the five broken ones emptied", third)
	}
	for _, task := range broken {
		if st := s.state(task); !st.emptied || st.masked {
			t.Errorf("broken %s after the third run: %+v; want emptied (not kept)", task, st)
		}
	}
}

// A send put back after a failure is not tried before its time.
func TestAFailedSendWaitsForItsRetry(t *testing.T) {
	s := newMaskSends(t)
	task := s.add(1)[0]
	failFor(t, task)
	if result := s.run(time.Hour); result.Failed != 1 {
		t.Fatalf("first run = %+v", result)
	}
	if result := s.run(time.Hour); result.Failed != 0 {
		t.Fatalf("second run = %+v, want the send not tried before its retry", result)
	}
}
