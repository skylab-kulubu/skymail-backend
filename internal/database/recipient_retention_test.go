package database

import (
	"context"
	"errors"
	"slices"
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
	rrFormApproval = "72000000-0000-4000-8000-000000000004"
	rrSendForm     = "76000000-0000-4000-8000-000000000001"
	rrRequestForm  = "76000000-0000-4000-8000-000000000002"
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
//     for; two rows of a News record to the two guests (the record's "bir
//     kez" index holds them); sent from a Forms snapshot of a guest and the
//     member;
//   - a send whose content is still kept, to the guest;
//   - a rejected Mail onayı request untouched for 400 days, to two guests
//     and the member, whose variables hold a guest's and the member's
//     address and whose edit changed a third guest into the member; one
//     untouched for 10 days; a pending one; a rejected one made from a Forms
//     snapshot of a guest, untouched for 400 days;
//   - a manual list archived 400 days ago (a person only it holds, a person
//     the active list holds too, the member), one archived 100 days ago, an
//     active one.
func recipientRetentionStore(t *testing.T) *Store {
	t.Helper()
	s := retentionStore(t)
	ctx := context.Background()
	tx, err := s.Conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The requests' deferred checks are off while seeding only.
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO template_versions (id, template_id, seq, name, subject, html_source, main_mode, html_content,
		                               plain_text_content, author_kind, created_at)
		VALUES ('78000000-0000-4000-8000-000000000001', '`+retentionTemplate+`', 1, 'Duyuru', 'Duyuru', '<p>x</p>', 'html', '<p>x</p>',
		        'x', 'operator', now() - interval '500 days');
		INSERT INTO mail_form_sources (id, owner_sub, form_id, form_title, prepared_at, expires_at, response_count, recipient_count,
		                               skipped_count, duplicate_count, snapshot_hash, consumed_at, consumed_task_id, consumed_approval_id, consume_request_hash) VALUES
		    ('`+rrSendForm+`', 'sub', gen_random_uuid(), 'Etkinlik', now() - interval '401 days', now() - interval '400 days', 2, 2, 0, 0,
		     decode(repeat('00', 32), 'hex'), now() - interval '400 days', '`+rrClearedSend+`', NULL, decode(repeat('00', 32), 'hex')),
		    ('`+rrRequestForm+`', 'sub', gen_random_uuid(), 'Başvuru', now() - interval '411 days', now() - interval '410 days', 1, 1, 0, 0,
		     decode(repeat('00', 32), 'hex'), now() - interval '410 days', NULL, '`+rrFormApproval+`', decode(repeat('00', 32), 'hex'));
		INSERT INTO mail_form_source_recipients (source_id, position, email, full_name) VALUES
		    ('`+rrSendForm+`', 1, 'form1@guest.example', 'Form Bir'),
		    ('`+rrSendForm+`', 2, 'uye@member.example', 'Üye Formda'),
		    ('`+rrRequestForm+`', 1, 'form2@guest.example', 'Form İki');
		INSERT INTO mail_tasks (id, sent_by, template_id, body_variables, created_at, content_cleared_at, form_source_id) VALUES
		    ('`+rrClearedSend+`', 'sender', '`+retentionTemplate+`', '{}', now() - interval '400 days', now() - interval '35 days', '`+rrSendForm+`');
		INSERT INTO mail_tasks (id, sent_by, template_id, body_variables, created_at, content_cleared_at) VALUES
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
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, recipient_sub, subject, body, status, source_ref) VALUES
		    ('`+rrClearedSend+`', 'Haber Bir', 'konuk1@guest.example', NULL, '', '', 'sent', 'news:duyuru'),
		    ('`+rrClearedSend+`', 'Haber İki', 'konuk2@guest.example', NULL, '', '', 'sent', 'news:duyuru');
		INSERT INTO mail_approvals (id, submitter_sub, template_id, template_version_id, body_variables,
		                            created_at, submitted_at, deadline_at, updated_at, state, form_source_id) VALUES
		    ('`+rrFormApproval+`', 'sub', '`+retentionTemplate+`', '78000000-0000-4000-8000-000000000001', '{}',
		     now() - interval '410 days', now() - interval '410 days', now() - interval '403 days', now() - interval '400 days', 'rejected', '`+rrRequestForm+`');
		INSERT INTO mail_approvals (id, submitter_sub, template_id, template_version_id, body_variables,
		                            created_at, submitted_at, deadline_at, updated_at, state) VALUES
		    ('`+rrOldApproval+`', 'sub', '`+retentionTemplate+`', '78000000-0000-4000-8000-000000000001',
		     '{"iletisim": "Yazın: Konuk3@guest.example.", "kulup": "uye@member.example"}',
		     now() - interval '410 days', now() - interval '410 days', now() - interval '403 days', now() - interval '400 days', 'rejected'),
		    ('`+rrNewApproval+`', 'sub', '`+retentionTemplate+`', '78000000-0000-4000-8000-000000000001', '{}',
		     now() - interval '20 days', now() - interval '20 days', now() - interval '13 days', now() - interval '10 days', 'rejected'),
		    ('`+rrOpenApproval+`', 'sub', '`+retentionTemplate+`', '78000000-0000-4000-8000-000000000001', '{}',
		     now() - interval '500 days', now() - interval '500 days', now() + interval '1 day', now() - interval '500 days', 'pending');
		INSERT INTO mail_approval_recipients (approval_id, position, email, full_name) VALUES
		    ('`+rrOldApproval+`', 1, 'konuk1@guest.example', 'Konuk Bir'),
		    ('`+rrOldApproval+`', 2, 'uye@member.example', 'Üye Adresle'),
		    ('`+rrOldApproval+`', 3, 'konuk3@guest.example', 'Konuk Üç'),
		    ('`+rrNewApproval+`', 1, 'konuk1@guest.example', 'Konuk Bir'),
		    ('`+rrOpenApproval+`', 1, 'konuk1@guest.example', 'Konuk Bir');
		INSERT INTO mail_approval_events (approval_id, seq, kind, actor_sub, actor_name, changes, created_at) VALUES
		    ('`+rrOldApproval+`', 1, 'edited', 'approver', 'Onaylayan',
		     '[{"field": "recipients", "name": "Alıcılar", "before": [{"email": "eski@guest.example", "full_name": "Eski Konuk"}],
		        "after": [{"email": "uye@member.example", "full_name": "Üye Adresle"}]}]', now() - interval '401 days');
		INSERT INTO mailing_lists (id, name, archived_at) VALUES
		    ('`+rrOldList+`', 'Eski etkinlik', now() - interval '400 days'),
		    ('`+rrRecentList+`', 'Yakın etkinlik', now() - interval '100 days'),
		    ('`+rrActiveList+`', 'Açık liste', NULL);
		INSERT INTO recipients (id, full_name, email) VALUES
		    ('75000000-0000-4000-8000-000000000001', 'Yalnız Eski', 'yalniz@guest.example'),
		    ('75000000-0000-4000-8000-000000000002', 'İki Listede', 'iki@guest.example'),
		    ('75000000-0000-4000-8000-000000000003', 'Yakın', 'yakin@guest.example'),
		    ('75000000-0000-4000-8000-000000000004', 'Üye Listede', 'uye@member.example');
		INSERT INTO mailing_list_recipients (mail_list_id, recipient_id) VALUES
		    ('`+rrOldList+`', '75000000-0000-4000-8000-000000000001'),
		    ('`+rrOldList+`', '75000000-0000-4000-8000-000000000002'),
		    ('`+rrOldList+`', '75000000-0000-4000-8000-000000000004'),
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
		"yalniz@guest.example": false,
		"iki@guest.example":    false,
		"form1@guest.example":  false,
		"form2@guest.example":  false,
		"eski@guest.example":   false,
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
		`SELECT 'recipient ' || id || ' ' || email || ' ' || (account_at IS NOT NULL)::text FROM recipients`,
		`SELECT 'form ' || source_id || ' ' || position || ' ' || full_name || ' <' || email || '> ' || (cleared_at IS NOT NULL)::text FROM mail_form_source_recipients`,
		`SELECT 'vars ' || id || ' ' || body_variables::text FROM mail_approvals`,
		`SELECT 'changes ' || approval_id || ' ' || changes::text FROM mail_approval_events WHERE changes IS NOT NULL`,
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
		Approvals: 2, ApprovalRecipientsCleared: 2, ApprovalJSONMasked: 2, ApprovalsDecided: 2,
		FormRecipientsCleared: 2, AccountRowsMarked: 5,
		Addresses: 11, AddressesTied: 1, AddressesWithAccount: 1, AddressesWithoutAccount: 8, AddressesUnknown: 1,
	}
	if dry.Counts != want {
		t.Errorf("dry run counts\n got %+v\nwant %+v", dry.Counts, want)
	}
	if dry.Status != "partial" {
		t.Errorf("dry run status %q, want partial (Keycloak did not answer for one address)", dry.Status)
	}
	// Keycloak is asked only about addresses SkyMail does not tie to a
	// subject, each once.
	wantAsked := []string{"bilinmeyen@guest.example", "eski@guest.example", "form1@guest.example", "form2@guest.example",
		"iki@guest.example", "konuk1@guest.example", "konuk2@guest.example", "konuk3@guest.example", "uye@member.example",
		"yalniz@guest.example"}
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
		// A News record's rows keep their addresses: the record's "bir
		// kez" index holds them (emptied, the two would be one).
		"queue " + rrClearedSend + " Haber Bir <konuk1@guest.example> -",
		"queue " + rrClearedSend + " Haber İki <konuk2@guest.example> -",
		// A send whose content is kept keeps its people.
		"queue " + rrFreshSend + " Konuk Bir <konuk1@guest.example> -",
		// The Forms snapshot of the send: the guest emptied in place, the
		// member kept.
		"form " + rrSendForm + " 1  <> true",
		"form " + rrSendForm + " 2 Üye Formda <uye@member.example> false",
		// The request made from a Forms snapshot: its guest emptied.
		"form " + rrRequestForm + " 1  <> true",
		"request " + rrFormApproval + " true",
		// The old request's variables and changes: the guests' addresses
		// masked (a changed recipient's name with them), the member's kept.
		`vars ` + rrOldApproval + ` {"kulup": "uye@member.example", "iletisim": "Yazın: ••••••."}`,
		`changes ` + rrOldApproval + ` [{"name": "Alıcılar", "after": [{"email": "uye@member.example", "full_name": "Üye Adresle"}], "field": "recipients", "before": [{"email": "••••••", "full_name": ""}]}]`,
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
		// The list archived a year ago keeps only the member (Yusuf,
		// 2026-11-24: a list's archive does not touch accounts); the person
		// the active list holds stays there. The list archived 100 days ago
		// keeps its.
		"member " + rrOldList + " 75000000-0000-4000-8000-000000000004",
		"recipient 75000000-0000-4000-8000-000000000004 uye@member.example true",
		"member " + rrActiveList + " 75000000-0000-4000-8000-000000000002",
		"member " + rrRecentList + " 75000000-0000-4000-8000-000000000003",
		"recipient 75000000-0000-4000-8000-000000000002 iki@guest.example false",
		"recipient 75000000-0000-4000-8000-000000000003 yakin@guest.example false",
	} {
		if !strings.Contains(got, line) {
			t.Errorf("missing %q in\n%s", line, got)
		}
	}
	if strings.Contains(got, "yalniz@guest.example") || strings.Contains(got, "member "+rrOldList+" 75000000-0000-4000-8000-000000000001") ||
		strings.Contains(got, "member "+rrOldList+" 75000000-0000-4000-8000-000000000002") {
		t.Errorf("the list archived a year ago still keeps people without an account:\n%s", got)
	}
	if strings.Contains(got, "eski@guest.example") || strings.Contains(got, "Eski Konuk") || strings.Contains(got, "form1@") || strings.Contains(got, "form2@") {
		t.Errorf("a guest's address or name is left:\n%s", got)
	}
	if n := queryCount(t, s, `SELECT count(*) FROM mail_queue WHERE task_id = $1 AND recipient_email ILIKE 'konuk%' AND source_ref IS NULL`, uuid.MustParse(rrClearedSend)); n != 0 {
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
	// Only the address it did not answer for is asked about again: what
	// the run found is kept on the rows.
	againWant := RecipientRetentionCounts{Sends: 1, SendsDecided: 1, AccountRowsMarked: 1, Addresses: 1, AddressesWithAccount: 1}
	if again.Status != "ok" || again.Counts != againWant {
		t.Errorf("repeat: status %q counts\n got %+v\nwant %+v", again.Status, again.Counts, againWant)
	}
	if got := answered.askedSorted(); strings.Join(got, ",") != "bilinmeyen@guest.example" {
		t.Errorf("repeat asked Keycloak about %v, want only bilinmeyen@guest.example", got)
	}
	last := runRecipientRetention(t, s, RecipientRetentionOptions{Mode: RecipientRetentionApply, Accounts: answered})
	if last.Counts != (RecipientRetentionCounts{}) {
		t.Errorf("a decided run still found %+v", last.Counts)
	}

	// A list person whose address changes forgets what the run found.
	if _, err := s.Conn.Exec(ctx, `UPDATE recipients SET email = 'yeni@member.example' WHERE id = '75000000-0000-4000-8000-000000000004'`); err != nil {
		t.Fatal(err)
	}
	if n := queryCount(t, s, `SELECT count(*) FROM recipients WHERE id = '75000000-0000-4000-8000-000000000004' AND account_at IS NULL`); n != 1 {
		t.Errorf("a changed address kept account_at")
	}
}

// Without Keycloak no address is emptied and no list person goes: in doubt
// the row stays.
func TestRecipientRetentionWithoutKeycloakKeepsEveryAddress(t *testing.T) {
	s := recipientRetentionStore(t)
	before := recipientState(t, s)
	report := runRecipientRetention(t, s, RecipientRetentionOptions{Mode: RecipientRetentionApply})
	if report.Counts.QueueRowsCleared != 0 || report.Counts.ApprovalRecipientsCleared != 0 || report.Counts.FormRecipientsCleared != 0 ||
		report.Counts.ApprovalJSONMasked != 0 || report.Counts.AddressesUnknown != 10 || report.Counts.ListMembershipsDeleted != 0 ||
		report.Status != "partial" {
		t.Errorf("status %q counts %+v; want partial, 10 unknown, nothing emptied or deleted", report.Status, report.Counts)
	}
	// Only the address SkyMail ties to a subject is marked.
	if report.Counts.AccountRowsMarked != 1 {
		t.Errorf("%d rows marked, want 1 (the tied address)", report.Counts.AccountRowsMarked)
	}
	after := recipientState(t, s)
	if len(after) != len(before) {
		t.Errorf("rows went: before %d, after %d", len(before), len(after))
	}
}

// A run asks Keycloak about a bounded number of addresses, send by send, and
// keeps what it found on the rows: with sends at the head whose people all
// have an account, the queue still moves on, one send a run, and no address
// is asked about twice. The oldest send, larger than the limit, is asked
// about in part and finished the next run.
func TestRecipientRetentionMovesOnPastAccountsWithASmallLookupLimit(t *testing.T) {
	s := retentionStore(t)
	ctx := context.Background()
	sends := []struct {
		id     string
		people []string
	}{
		{"77000000-0000-4000-8000-000000000001", []string{"a1@member.example", "a2@member.example", "g1@guest.example"}},
		{"77000000-0000-4000-8000-000000000002", []string{"a3@member.example", "g2@guest.example"}},
		{"77000000-0000-4000-8000-000000000003", []string{"a4@member.example", "g3@guest.example"}},
	}
	accounts := &fakeAccounts{answers: map[string]bool{}}
	for i, send := range sends {
		if _, err := s.Conn.Exec(ctx, `
			INSERT INTO mail_tasks (id, sent_by, template_id, body_variables, created_at, content_cleared_at)
			VALUES ($1, 'sender', $2, '{}', now() - interval '400 days', now() - make_interval(days => 40 - $3::int))`,
			uuid.MustParse(send.id), uuid.MustParse(retentionTemplate), i); err != nil {
			t.Fatal(err)
		}
		for _, address := range send.people {
			accounts.answers[address] = strings.HasPrefix(address, "a")
			if _, err := s.Conn.Exec(ctx, `
				INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, status)
				VALUES ($1, 'Kişi', $2, '', '', 'sent')`, uuid.MustParse(send.id), address); err != nil {
				t.Fatal(err)
			}
		}
	}
	decided := func() int {
		return queryCount(t, s, `SELECT count(*) FROM mail_tasks WHERE recipient_retention_at IS NOT NULL`)
	}
	opts := RecipientRetentionOptions{Mode: RecipientRetentionApply, Accounts: accounts, LookupLimit: 2}
	for run, want := range []int{0, 1, 2, 3} {
		report := runRecipientRetention(t, s, opts)
		if got := decided(); got != want {
			t.Fatalf("run %d: %d sends decided, want %d (counts %+v)", run+1, got, want, report.Counts)
		}
		if len(accounts.asked) > 2*(run+1) {
			t.Fatalf("run %d asked about %d addresses in all, more than the limit allows", run+1, len(accounts.asked))
		}
	}
	asked := accounts.askedSorted()
	if len(asked) != 7 || len(slices.Compact(asked)) != 7 {
		t.Errorf("Keycloak asked about %v; want each of the 7 addresses once", asked)
	}
	if n := queryCount(t, s, `SELECT count(*) FROM mail_queue WHERE recipient_email LIKE 'g%'`); n != 0 {
		t.Errorf("%d guests keep their address", n)
	}
	if n := queryCount(t, s, `SELECT count(*) FROM mail_queue WHERE recipient_email LIKE 'a%' AND recipient_account_at IS NOT NULL`); n != 4 {
		t.Errorf("%d members marked, want 4", n)
	}
}

func TestMaskJSONAddresses(t *testing.T) {
	mask := map[string]bool{"konuk@guest.example": true}
	got, changed, err := maskJSONAddresses([]byte(`{"a": "Konuk@Guest.example.", "b": ["uye@member.example", 3.50, {"email": "konuk@guest.example", "full_name": "Konuk"}], "c": "<b>x</b>"}`), mask)
	if err != nil || !changed {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	want := `{"a":"••••••.","b":["uye@member.example",3.50,{"email":"••••••","full_name":""}],"c":"<b>x</b>"}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if _, changed, _ := maskJSONAddresses([]byte(`{"a": "uye@member.example"}`), mask); changed {
		t.Error("a document without a masked address changed")
	}
	found, err := jsonAddresses([]byte(`{"x": "Yaz: A@B.example. ya da c@d.example", "k@e.example": 1}`))
	if err != nil || strings.Join(found, ",") != "a@b.example,c@d.example" {
		t.Errorf("addresses %v, %v", found, err)
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
