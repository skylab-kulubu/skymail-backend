package database

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The retention of recipients without an account (ADR-0062; hub sector audit
// 2026-11-24, 06 fix 3).

// fakeAccounts answers HasAccount from a map; an address not in it fails.
type fakeAccounts struct {
	mu      sync.Mutex
	answers map[string]bool
	asked   []string
}

func (f *fakeAccounts) HasAccount(_ context.Context, address string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, address)
	has, ok := f.answers[address]
	if !ok {
		return false, errors.New("keycloak down")
	}
	return has, nil
}

func (f *fakeAccounts) askedSorted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.asked...)
	sort.Strings(out)
	return out
}

const (
	rrClearedSend  = "71000000-0000-4000-8000-000000000001"
	rrFreshSend    = "71000000-0000-4000-8000-000000000002"
	rrMemberSend   = "71000000-0000-4000-8000-000000000003"
	rrOldApproval  = "72000000-0000-4000-8000-000000000001"
	rrNewApproval  = "72000000-0000-4000-8000-000000000002"
	rrOpenApproval = "72000000-0000-4000-8000-000000000003"
	rrOldList      = "73000000-0000-4000-8000-000000000001"
	rrRecentList   = "73000000-0000-4000-8000-000000000002"
	rrActiveList   = "73000000-0000-4000-8000-000000000003"
	rrMemberSub    = "74000000-0000-4000-8000-000000000001"
	rrTiedSub      = "74000000-0000-4000-8000-000000000002"
)

// recipientRetentionStore is a migrated database with:
//
//   - a send whose content the content sweep emptied, to a guest (no
//     account), a guest whose error quotes their address, a member known by
//     address only (Keycloak has them), a member known by subject, an address
//     another row ties to a subject, and an address Keycloak does not answer
//     for;
//   - a send whose content is still kept, to the guest;
//   - a rejected Mail onayı request untouched for 400 days, to two guests
//     and the member; one untouched for 10 days; a pending one;
//   - a manual list archived 400 days ago (a person only it holds, a person
//     the active list holds too), one archived 100 days ago, an active one.
func recipientRetentionStore(t *testing.T) *Store {
	t.Helper()
	s := retentionStore(t)
	ctx := context.Background()
	tx, err := s.Conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The requests' template versions are left out: their foreign keys and
	// deferred checks are off while seeding only.
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO mail_tasks (id, sent_by, template_id, body_variables, created_at, content_cleared_at) VALUES
		    ('`+rrClearedSend+`', 'sender', '`+retentionTemplate+`', '{}', now() - interval '400 days', now() - interval '35 days'),
		    ('`+rrFreshSend+`', 'sender', '`+retentionTemplate+`', '{}', now() - interval '20 days', NULL),
		    ('`+rrMemberSend+`', 'sender', '`+retentionTemplate+`', '{}', now() - interval '20 days', NULL);
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, recipient_sub, subject, body, status, error) VALUES
		    ('`+rrClearedSend+`', 'Konuk Bir', 'Konuk1@Guest.example', NULL, '', '', 'sent', NULL),
		    ('`+rrClearedSend+`', 'Konuk İki', 'konuk2@guest.example', NULL, '', '', 'failed',
		     'send failed: 550 <konuk2@guest.example> mailbox unavailable'),
		    ('`+rrClearedSend+`', 'Üye Adresle', 'uye@member.example', NULL, '', '', 'sent', NULL),
		    ('`+rrClearedSend+`', 'Üye Kimlikle', 'kimlikli@member.example', '`+rrMemberSub+`', '', '', 'sent', NULL),
		    ('`+rrClearedSend+`', 'Bağlı', 'bagli@member.example', NULL, '', '', 'failed', 'smtp_rejected: RCPT TO 550 5.1.1'),
		    ('`+rrClearedSend+`', 'Bilinmeyen', 'bilinmeyen@guest.example', NULL, '', '', 'sent', NULL),
		    ('`+rrFreshSend+`', 'Konuk Bir', 'konuk1@guest.example', NULL, 'Duyuru', 'x', 'sent', NULL),
		    ('`+rrMemberSend+`', 'Bağlı', 'bagli@member.example', '`+rrTiedSub+`', 'Duyuru', 'x', 'sent', NULL);
		INSERT INTO mail_approvals (id, submitter_sub, template_id, template_version_id, body_variables,
		                            created_at, submitted_at, deadline_at, updated_at, state) VALUES
		    ('`+rrOldApproval+`', 'sub', '`+retentionTemplate+`', gen_random_uuid(), '{}',
		     now() - interval '410 days', now() - interval '410 days', now() - interval '403 days', now() - interval '400 days', 'rejected'),
		    ('`+rrNewApproval+`', 'sub', '`+retentionTemplate+`', gen_random_uuid(), '{}',
		     now() - interval '20 days', now() - interval '20 days', now() - interval '13 days', now() - interval '10 days', 'rejected'),
		    ('`+rrOpenApproval+`', 'sub', '`+retentionTemplate+`', gen_random_uuid(), '{}',
		     now() - interval '500 days', now() - interval '500 days', now() + interval '1 day', now() - interval '500 days', 'pending');
		INSERT INTO mail_approval_recipients (approval_id, position, email, full_name) VALUES
		    ('`+rrOldApproval+`', 1, 'konuk1@guest.example', 'Konuk Bir'),
		    ('`+rrOldApproval+`', 2, 'uye@member.example', 'Üye Adresle'),
		    ('`+rrOldApproval+`', 3, 'konuk3@guest.example', 'Konuk Üç'),
		    ('`+rrNewApproval+`', 1, 'konuk1@guest.example', 'Konuk Bir'),
		    ('`+rrOpenApproval+`', 1, 'konuk1@guest.example', 'Konuk Bir');
		INSERT INTO mailing_lists (id, name, archived_at) VALUES
		    ('`+rrOldList+`', 'Eski etkinlik', now() - interval '400 days'),
		    ('`+rrRecentList+`', 'Yakın etkinlik', now() - interval '100 days'),
		    ('`+rrActiveList+`', 'Açık liste', NULL);
		INSERT INTO recipients (id, full_name, email) VALUES
		    ('75000000-0000-4000-8000-000000000001', 'Yalnız Eski', 'yalniz@guest.example'),
		    ('75000000-0000-4000-8000-000000000002', 'İki Listede', 'iki@guest.example'),
		    ('75000000-0000-4000-8000-000000000003', 'Yakın', 'yakin@guest.example');
		INSERT INTO mailing_list_recipients (mail_list_id, recipient_id) VALUES
		    ('`+rrOldList+`', '75000000-0000-4000-8000-000000000001'),
		    ('`+rrOldList+`', '75000000-0000-4000-8000-000000000002'),
		    ('`+rrActiveList+`', '75000000-0000-4000-8000-000000000002'),
		    ('`+rrRecentList+`', '75000000-0000-4000-8000-000000000003');`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func newRecipientAccounts() *fakeAccounts {
	return &fakeAccounts{answers: map[string]bool{
		"konuk1@guest.example": false,
		"konuk2@guest.example": false,
		"konuk3@guest.example": false,
		"uye@member.example":   true,
		// bilinmeyen@guest.example: Keycloak fails for it.
	}}
}

// recipientState is every row the run may change, as text.
func recipientState(t *testing.T, s *Store) []string {
	t.Helper()
	var state []string
	for _, sql := range []string{
		`SELECT 'queue ' || task_id || ' ' || recipient_full_name || ' <' || recipient_email || '> ' || COALESCE(error, '-') FROM mail_queue`,
		`SELECT 'task ' || id || ' ' || COALESCE((recipient_retention_at IS NOT NULL)::text, 'false') FROM mail_tasks`,
		`SELECT 'approval ' || approval_id || ' ' || position || ' ' || full_name || ' <' || email || '> ' || (cleared_at IS NOT NULL)::text FROM mail_approval_recipients`,
		`SELECT 'request ' || id || ' ' || (recipient_retention_at IS NOT NULL)::text FROM mail_approvals`,
		`SELECT 'member ' || mail_list_id || ' ' || recipient_id FROM mailing_list_recipients`,
		`SELECT 'recipient ' || id || ' ' || email FROM recipients`,
	} {
		rows, err := s.Conn.Query(context.Background(), sql)
		if err != nil {
			t.Fatal(err)
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		state = append(state, lines...)
	}
	sort.Strings(state)
	return state
}

func runRecipientRetention(t *testing.T, s *Store, opts RecipientRetentionOptions) *RecipientRetentionReport {
	t.Helper()
	if opts.ContentRetentionDays == 0 {
		opts.ContentRetentionDays = 365
	}
	report, err := s.RunRecipientRetention(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// A dry run changes nothing and counts what apply changes; apply empties the
// rows of people without an account only, deletes the people of the list
// archived a year ago, and writes both receipts. A member's row — known by
// subject, tied to one elsewhere, or found in Keycloak — is never touched,
// nor is an address Keycloak did not answer for. A repeat changes nothing.
func TestRecipientRetentionEmptiesOnlyRowsOfPeopleWithoutAnAccount(t *testing.T) {
	s := recipientRetentionStore(t)
	ctx := context.Background()
	before := recipientState(t, s)

	accounts := newRecipientAccounts()
	dry := runRecipientRetention(t, s, RecipientRetentionOptions{Mode: RecipientRetentionDryRun, Accounts: accounts})
	if after := recipientState(t, s); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("the dry run changed rows:\nbefore %v\nafter  %v", before, after)
	}
	want := RecipientRetentionCounts{
		Lists: 1, ListMembershipsDeleted: 2, RecipientsDeleted: 1,
		Sends: 1, QueueRowsCleared: 2, SendsDecided: 0,
		Approvals: 1, ApprovalRecipientsCleared: 2, ApprovalsDecided: 1,
		Addresses: 6, AddressesTied: 1, AddressesWithAccount: 1, AddressesWithoutAccount: 3, AddressesUnknown: 1,
	}
	if dry.Counts != want {
		t.Errorf("dry run counts\n got %+v\nwant %+v", dry.Counts, want)
	}
	if dry.Status != "partial" {
		t.Errorf("dry run status %q, want partial (Keycloak did not answer for one address)", dry.Status)
	}
	// Keycloak is asked only about addresses SkyMail does not tie to a
	// subject, each once.
	wantAsked := []string{"bilinmeyen@guest.example", "konuk1@guest.example", "konuk2@guest.example", "konuk3@guest.example", "uye@member.example"}
	if got := accounts.askedSorted(); strings.Join(got, ",") != strings.Join(wantAsked, ",") {
		t.Errorf("Keycloak asked about %v, want %v", got, wantAsked)
	}

	apply := runRecipientRetention(t, s, RecipientRetentionOptions{Mode: RecipientRetentionApply, Accounts: newRecipientAccounts()})
	if apply.Counts != want {
		t.Errorf("apply counts\n got %+v\nwant %+v", apply.Counts, want)
	}
	if apply.Status != "partial" {
		t.Errorf("apply status %q, want partial", apply.Status)
	}

	got := strings.Join(recipientState(t, s), "\n")
	for _, line := range []string{
		// The guests' rows are empty; the codes of an error stay, its
		// address is masked.
		"queue " + rrClearedSend + "  <> -",
		"queue " + rrClearedSend + "  <> send failed: 550 <••••••> mailbox unavailable",
		// The member found in Keycloak, the member known by subject, the
		// address tied to a subject, the address Keycloak did not answer
		// for: as they were.
		"queue " + rrClearedSend + " Üye Adresle <uye@member.example> -",
		"queue " + rrClearedSend + " Üye Kimlikle <kimlikli@member.example> -",
		"queue " + rrClearedSend + " Bağlı <bagli@member.example> smtp_rejected: RCPT TO 550 5.1.1",
		"queue " + rrClearedSend + " Bilinmeyen <bilinmeyen@guest.example> -",
		// A send whose content is kept keeps its people.
		"queue " + rrFreshSend + " Konuk Bir <konuk1@guest.example> -",
		// Not decided while Keycloak owes an answer: looked at again.
		"task " + rrClearedSend + " false",
		// The request over for a year: its guests emptied in place, the
		// member kept; decided.
		"approval " + rrOldApproval + " 1  <> true",
		"approval " + rrOldApproval + " 2 Üye Adresle <uye@member.example> false",
		"approval " + rrOldApproval + " 3  <> true",
		"request " + rrOldApproval + " true",
		// A request over for 10 days, and a pending one: as they were.
		"approval " + rrNewApproval + " 1 Konuk Bir <konuk1@guest.example> false",
		"approval " + rrOpenApproval + " 1 Konuk Bir <konuk1@guest.example> false",
		// The list archived a year ago keeps no one; the person the active
		// list holds stays there. The list archived 100 days ago keeps its.
		"member " + rrActiveList + " 75000000-0000-4000-8000-000000000002",
		"member " + rrRecentList + " 75000000-0000-4000-8000-000000000003",
		"recipient 75000000-0000-4000-8000-000000000002 iki@guest.example",
		"recipient 75000000-0000-4000-8000-000000000003 yakin@guest.example",
	} {
		if !strings.Contains(got, line) {
			t.Errorf("missing %q in\n%s", line, got)
		}
	}
	if strings.Contains(got, "yalniz@guest.example") || strings.Contains(got, "member "+rrOldList) {
		t.Errorf("the list archived a year ago still keeps people:\n%s", got)
	}
	if n := queryCount(t, s, `SELECT count(*) FROM mail_queue WHERE task_id = $1 AND recipient_email ILIKE 'konuk%'`, uuid.MustParse(rrClearedSend)); n != 0 {
		t.Errorf("%d guest rows of the emptied send still hold an address", n)
	}

	// The receipts: counts and times only.
	var receipts int64
	if err := s.Conn.QueryRow(ctx, `SELECT count(*) FROM recipient_retention_runs WHERE status = 'partial' AND finished_at IS NOT NULL
	                                AND NOT counts::text ILIKE '%@%'`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if receipts != 2 {
		t.Errorf("%d receipts without addresses, want 2", receipts)
	}

	// Keycloak answers now: the last address is decided, nothing else
	// changes.
	answered := newRecipientAccounts()
	answered.answers["bilinmeyen@guest.example"] = true
	again := runRecipientRetention(t, s, RecipientRetentionOptions{Mode: RecipientRetentionApply, Accounts: answered})
	if again.Status != "ok" || again.Counts.QueueRowsCleared != 0 || again.Counts.ApprovalRecipientsCleared != 0 ||
		again.Counts.ListMembershipsDeleted != 0 || again.Counts.SendsDecided != 1 || again.Counts.Approvals != 0 {
		t.Errorf("repeat: status %q counts %+v; want ok, only the send decided", again.Status, again.Counts)
	}
	last := runRecipientRetention(t, s, RecipientRetentionOptions{Mode: RecipientRetentionApply, Accounts: answered})
	if last.Counts != (RecipientRetentionCounts{}) {
		t.Errorf("a decided run still found %+v", last.Counts)
	}
}

// Without Keycloak no address of a send or a request is emptied; the
// archived list's people still go a year on.
func TestRecipientRetentionWithoutKeycloakKeepsEveryAddress(t *testing.T) {
	s := recipientRetentionStore(t)
	report := runRecipientRetention(t, s, RecipientRetentionOptions{Mode: RecipientRetentionApply})
	if report.Counts.QueueRowsCleared != 0 || report.Counts.ApprovalRecipientsCleared != 0 ||
		report.Counts.AddressesUnknown != 5 || report.Counts.ListMembershipsDeleted != 2 || report.Status != "partial" {
		t.Errorf("status %q counts %+v; want partial, 5 unknown, nothing emptied, 2 memberships deleted", report.Status, report.Counts)
	}
}

// The brake: an apply that would change more rows than it allows changes
// nothing (refused_large); the dry run says would_refuse_large.
func TestRecipientRetentionBrakeRefusesALargeApply(t *testing.T) {
	s := recipientRetentionStore(t)
	before := recipientState(t, s)
	dry := runRecipientRetention(t, s, RecipientRetentionOptions{Mode: RecipientRetentionDryRun, Accounts: newRecipientAccounts(), BrakeMaxRows: 1})
	if dry.Status != "would_refuse_large" {
		t.Errorf("dry run status %q, want would_refuse_large", dry.Status)
	}
	apply := runRecipientRetention(t, s, RecipientRetentionOptions{Mode: RecipientRetentionApply, Accounts: newRecipientAccounts(), BrakeMaxRows: 1})
	if apply.Status != "refused_large" {
		t.Errorf("apply status %q, want refused_large", apply.Status)
	}
	if after := recipientState(t, s); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("a refused apply changed rows")
	}
}

// One run at a time across replicas, and once a day per mode.
func TestRecipientRetentionRunsOnceAtATimeAndOnceADay(t *testing.T) {
	s := recipientRetentionStore(t)
	ctx := context.Background()
	conn, err := s.Conn.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, RecipientRetentionLockName); err != nil {
		t.Fatal(err)
	}
	_, err = s.RunRecipientRetention(ctx, RecipientRetentionOptions{Mode: RecipientRetentionDryRun, ContentRetentionDays: 365})
	if !errors.Is(err, ErrRecipientRetentionLocked) {
		t.Errorf("run while another holds the lock: %v, want ErrRecipientRetentionLocked", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, RecipientRetentionLockName); err != nil {
		t.Fatal(err)
	}
	conn.Release()

	opts := RecipientRetentionOptions{Mode: RecipientRetentionDryRun, ContentRetentionDays: 365, Accounts: newRecipientAccounts(), OnlyIfDue: true}
	if _, err := s.RunRecipientRetention(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunRecipientRetention(ctx, opts); !errors.Is(err, ErrRecipientRetentionNotDue) {
		t.Errorf("second run the same day: %v, want ErrRecipientRetentionNotDue", err)
	}
	// Another mode is due at once.
	opts.Mode = RecipientRetentionApply
	if _, err := s.RunRecipientRetention(ctx, opts); err != nil {
		t.Errorf("apply after a dry run: %v", err)
	}
}

// An emptied Mail onayı row keeps its place, and the request can still be
// resubmitted to new people: the address CHECK and the once-per-request index
// leave emptied rows out.
func TestEmptiedApprovalRowsKeepTheirPlace(t *testing.T) {
	s := recipientRetentionStore(t)
	ctx := context.Background()
	runRecipientRetention(t, s, RecipientRetentionOptions{Mode: RecipientRetentionApply, Accounts: newRecipientAccounts()})
	if _, err := s.Conn.Exec(ctx, `INSERT INTO mail_approval_recipients (approval_id, position, email, full_name) VALUES ($1, 4, '', '')`,
		uuid.MustParse(rrOldApproval)); err == nil {
		t.Error("an empty address without cleared_at was written")
	}
	q := New(s.Conn)
	if err := pgx.BeginFunc(ctx, s.Conn, func(tx pgx.Tx) error {
		if err := q.WithTx(tx).ClearMailApprovalRecipients(ctx, uuid.MustParse(rrOldApproval)); err != nil {
			return err
		}
		return q.WithTx(tx).AddMailApprovalRecipients(ctx, AddMailApprovalRecipientsParams{
			ApprovalID: uuid.MustParse(rrOldApproval), Emails: []string{"yeni@guest.example"}, FullNames: []string{"Yeni"},
		})
	}); err != nil {
		t.Fatalf("resubmitting a request whose people were emptied: %v", err)
	}
}

func TestRecipientRetentionSettings(t *testing.T) {
	env := func(v map[string]string) func(string) string { return func(k string) string { return v[k] } }
	for raw, want := range map[string]RecipientRetentionMode{"": RecipientRetentionDryRun, "dry-run": RecipientRetentionDryRun, "off": RecipientRetentionOff, " apply ": RecipientRetentionApply} {
		got, err := RecipientRetentionModeFromEnv(env(map[string]string{RecipientRetentionModeEnv: raw}))
		if err != nil || got != want {
			t.Errorf("%q: %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"purge", "on", "Apply"} {
		if _, err := RecipientRetentionModeFromEnv(env(map[string]string{RecipientRetentionModeEnv: raw})); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
	if n, err := RecipientRetentionMaxRowsFromEnv(env(nil)); err != nil || n != DefaultRecipientRetentionMaxRows {
		t.Errorf("unset brake: %d, %v", n, err)
	}
	for _, raw := range []string{"0", "-1", "x", "10000001"} {
		if _, err := RecipientRetentionMaxRowsFromEnv(env(map[string]string{RecipientRetentionMaxRowsEnv: raw})); err == nil {
			t.Errorf("brake %q accepted", raw)
		}
	}
}
