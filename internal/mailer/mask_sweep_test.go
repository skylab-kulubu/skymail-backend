package mailer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
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
// fault is theirs, and counts their failures — that run's only, not the first
// run's, which masked nothing (#63 Ek 4, A2); three such runs empty them (#63
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
		if st := s.state(task); st.failures != 1 || st.unconfirmed != 1 {
			t.Errorf("broken %s after the second run: %+v; want the second run's failure counted, the first's not", task, st)
		}
	}

	// Two more runs, each with a new sound send to mask, count the second and
	// the third failure: the broken sends are emptied.
	for run := 3; run <= 4; run++ {
		time.Sleep(50 * time.Millisecond)
		s.add(1)
		result := s.run(time.Millisecond)
		want := 0
		if run == 4 {
			want = 5
		}
		if result.Sends != 1 || result.NotKept != want {
			t.Fatalf("run %d = %+v, want the new send masked and %d broken ones emptied", run, result, want)
		}
	}
	for _, task := range broken {
		if st := s.state(task); !st.emptied || st.masked {
			t.Errorf("broken %s after the fourth run: %+v; want emptied (not kept)", task, st)
		}
	}
}

// A fault across SkyMail fails every masking for a while: its runs mask
// nothing and count nothing. Once it is over, a run that masks a send counts
// only its own failures, never the ones the fault left waiting: the sends
// are not emptied for it (#63 Ek 4, A2).
func TestAFaultAcrossSkyMailCountsNothingOnceItIsOver(t *testing.T) {
	s := newMaskSends(t)
	waiting := s.add(2)
	stop := mailer.SetMaskSendHook(func(uuid.UUID) { panic("SkyMail is broken") })
	for run := 1; run <= 3; run++ {
		if result := s.run(time.Millisecond); result.Failed != 2 || !result.FailuresUnconfirmed {
			t.Fatalf("run %d during the fault = %+v", run, result)
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	for _, task := range waiting {
		if st := s.state(task); st.failures != 0 || st.unconfirmed != 3 {
			t.Fatalf("%s during the fault: %+v; want three failures waiting, none counted", task, st)
		}
	}

	// The fault is over; the waiting sends are not due yet, a new one is.
	if _, err := s.db.Conn.Exec(context.Background(),
		`UPDATE mail_tasks SET content_mask_retry_at = now() + interval '1 hour' WHERE id = ANY($1)`, waiting); err != nil {
		t.Fatal(err)
	}
	s.add(1)
	if result := s.run(time.Millisecond); result.Sends != 1 || result.NotKept != 0 {
		t.Fatalf("first run after the fault = %+v, want the new send masked and nothing emptied", result)
	}
	for _, task := range waiting {
		if st := s.state(task); st.failures != 0 || st.emptied {
			t.Errorf("%s after the fault: %+v; want nothing counted, not emptied", task, st)
		}
	}
}

// After a fault across SkyMail, a run that masks a send and sees another
// fail again counts that one failure only: a send the run did not try keeps
// its failures from the fault uncounted (#65 review, K1).
func TestARunCountsOnlyTheSendsItTried(t *testing.T) {
	s := newMaskSends(t)
	sends := s.add(2)
	held, again := sends[0], sends[1]
	stop := mailer.SetMaskSendHook(func(uuid.UUID) { panic("SkyMail is broken") })
	if result := s.run(time.Millisecond); result.Failed != 2 || !result.FailuresUnconfirmed {
		t.Fatalf("run during the fault = %+v", result)
	}
	stop()

	// held is not due in the next run; again fails again; a new send is masked.
	if _, err := s.db.Conn.Exec(context.Background(),
		`UPDATE mail_tasks SET content_mask_retry_at = now() + interval '1 hour' WHERE id = $1`, held); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	failFor(t, again)
	s.add(1)
	if result := s.run(time.Millisecond); result.Sends != 1 || result.Failed != 1 || result.FailuresUnconfirmed {
		t.Fatalf("run after the fault = %+v, want one send masked and one failure counted", result)
	}
	if st := s.state(held); st.failures != 0 {
		t.Errorf("a send not tried in the run was counted: %+v", st)
	}
	if st := s.state(again); st.failures != 1 {
		t.Errorf("the send that failed again: %+v; want one failure counted", st)
	}
}

// failingSince is when the masking of task started failing, or nil.
func (s *maskSends) failingSince(task uuid.UUID) *time.Time {
	s.t.Helper()
	var since *time.Time
	if err := s.db.Conn.QueryRow(context.Background(),
		`SELECT content_mask_failing_since FROM mail_tasks WHERE id = $1`, task).Scan(&since); err != nil {
		s.t.Fatal(err)
	}
	return since
}

// failedDaysAgo makes the masking of tasks have failed since days ago, their
// next try an hour away.
func (s *maskSends) failedDaysAgo(days int, tasks ...uuid.UUID) {
	s.t.Helper()
	if _, err := s.db.Conn.Exec(context.Background(), `
		UPDATE mail_tasks
		SET content_mask_failing_since = now() - make_interval(days => $2),
		    content_mask_retry_at = now() + interval '1 hour'
		WHERE id = ANY($1)`, tasks, days); err != nil {
		s.t.Fatal(err)
	}
}

// The first failure starts a send's deadline, a later one does not move it,
// and masking the send ends it.
func TestASendsDeadlineStartsAtItsFirstFailure(t *testing.T) {
	s := newMaskSends(t)
	task := s.add(1)[0]
	stop := mailer.SetMaskSendHook(func(uuid.UUID) { panic("broken") })
	s.run(time.Millisecond)
	first := s.failingSince(task)
	if first == nil {
		t.Fatal("the first failure did not start the deadline")
	}
	time.Sleep(50 * time.Millisecond)
	s.run(time.Millisecond)
	if again := s.failingSince(task); again == nil || !again.Equal(*first) {
		t.Fatalf("a later failure moved the deadline: %v, then %v", first, again)
	}
	stop()
	time.Sleep(50 * time.Millisecond)
	if result := s.run(time.Millisecond); result.Sends != 1 {
		t.Fatalf("run = %+v, want the send masked", result)
	}
	if since := s.failingSince(task); since != nil {
		t.Errorf("masked, the send still fails since %v", since)
	}
}

// A sensitive send whose masking has kept failing for longer than the
// deadline is emptied, its failures counted or not, even by a run that masks
// nothing — a queue of broken sends alone (#63 Ek 4, A1) — and logged as an
// alarm; one failing for less is kept.
func TestASendFailingPastTheDeadlineIsEmptied(t *testing.T) {
	s := newMaskSends(t)
	sends := s.add(2)
	old, recent := sends[0], sends[1]
	failFor(t, old, recent)
	s.run(time.Millisecond)
	s.failedDaysAgo(8, old)
	s.failedDaysAgo(6, recent)

	var logged bytes.Buffer
	before := log.Logger
	log.Logger = zerolog.New(&logged)
	t.Cleanup(func() { log.Logger = before })

	result := s.run(time.Millisecond)
	if result.PastDeadline != 1 || result.NotKept != 1 || result.Sends != 0 {
		t.Fatalf("run = %+v, want one send emptied past its deadline", result)
	}
	if st := s.state(old); !st.emptied || st.masked {
		t.Errorf("send failing for eight days: %+v; want emptied (not kept)", st)
	}
	var subject, body string
	if err := s.db.Conn.QueryRow(context.Background(),
		`SELECT subject, body FROM mail_queue WHERE task_id = $1`, old).Scan(&subject, &body); err != nil {
		t.Fatal(err)
	}
	if subject != "" || body != "" {
		t.Errorf("the emptied send's row still holds %q / %q", subject, body)
	}
	if st := s.state(recent); st.emptied {
		t.Errorf("send failing for six days: %+v; want kept", st)
	}

	var line struct {
		Level, Reason, TaskID string
	}
	found := false
	for _, raw := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		var fields map[string]any
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			continue
		}
		if fields["event"] == mailer.MaskAttentionEvent {
			found = true
			line.Level, _ = fields["level"].(string)
			line.Reason, _ = fields["reason"].(string)
			line.TaskID, _ = fields["task_id"].(string)
		}
	}
	if !found || line.Level != "error" || line.Reason != "deadline" || line.TaskID != old.String() {
		t.Errorf("alarm line = %+v (found %v), want an error with reason deadline for %s; log:\n%s", line, found, old, logged.String())
	}
	if strings.Contains(logged.String(), "482910") {
		t.Errorf("the log holds the content:\n%s", logged.String())
	}
}

// A send that is not sensitive is never emptied for a deadline.
func TestTheDeadlineEmptiesOnlySensitiveSends(t *testing.T) {
	s := newMaskSends(t)
	task := s.add(1)[0]
	ctx := context.Background()
	if _, err := s.db.Conn.Exec(ctx, `
		UPDATE templates SET key = 'club.notice', content_sensitive = false WHERE id = $1;`, s.template); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Conn.Exec(ctx,
		`UPDATE mail_tasks SET content_sensitive_at_send = 'false' WHERE id = $1`, task); err != nil {
		t.Fatal(err)
	}
	s.failedDaysAgo(30, task)
	if result := s.run(time.Millisecond); result.PastDeadline != 0 {
		t.Fatalf("run = %+v, want nothing emptied", result)
	}
	if st := s.state(task); st.emptied {
		t.Errorf("a send that is not sensitive was emptied: %+v", st)
	}
}

// The sweep masks a template's listed sensitive variables whatever their
// names: a code of letters alone, a pass phrase (#63 review, R3).
func TestTheSweepMasksATemplatesListedVariables(t *testing.T) {
	s := newMaskSends(t)
	ctx := context.Background()
	if _, err := s.db.Conn.Exec(ctx, `
		UPDATE templates
		SET subject = 'Kodun {{.harfler}}', html_content = '<p>{{.harfler}} {{.ilkAdim}}</p>',
		    plain_text_content = '{{.harfler}} {{.ilkAdim}}', sensitive_variables = '{harfler,ilkAdim}',
		    updated_at = now() - interval '2 hours'
		WHERE id = $1`, s.template); err != nil {
		t.Fatal(err)
	}
	var task uuid.UUID
	if err := s.db.Conn.QueryRow(ctx, `
		INSERT INTO mail_tasks (sent_by, template_id, body_variables, created_at, content_sensitive_at_send)
		VALUES ('s', $1, '{"harfler":"kxqpzmwt","ilkAdim":"elma-armut-kiraz-nar"}', now() - interval '1 hour', 'true')
		RETURNING id`, s.template).Scan(&task); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Conn.Exec(ctx, `
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, body_html, status, created_at)
		VALUES ($1, 'Ayşe Kaya', 'ayse@example.com', 'Kodun kxqpzmwt', 'kxqpzmwt elma-armut-kiraz-nar',
		        '<p>kxqpzmwt elma-armut-kiraz-nar</p>', 'sent', now() - interval '1 hour')`, task); err != nil {
		t.Fatal(err)
	}
	if result := s.run(time.Hour); result.Sends != 1 {
		t.Fatalf("run = %+v, want the send masked", result)
	}
	var kept string
	if err := s.db.Conn.QueryRow(ctx, `
		SELECT q.subject || q.body || coalesce(q.body_html, '') || mt.body_variables::text
		FROM mail_queue q JOIN mail_tasks mt ON mt.id = q.task_id WHERE q.task_id = $1`, task).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"kxqpzmwt", "elma-armut-kiraz-nar"} {
		if strings.Contains(kept, secret) {
			t.Errorf("the database still holds %q: %s", secret, kept)
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
