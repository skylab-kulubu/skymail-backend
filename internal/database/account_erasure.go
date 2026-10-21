package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Silinmiş kullanıcı (CONTEXT.md; ADR-0051): the one stand-in every erased
// person becomes wherever SkyMail keeps a record that names an actor. It is
// the same for everyone, so it links nothing, and nobody can sign in as it.
const (
	DeletedUserSubject = "00000000-0000-4000-8000-000000000000"
	DeletedUserName    = "Silinmiş kullanıcı"
	// The address of the one person a Mail onayı request keeps when the
	// erased person was the last it went to: .invalid never delivers.
	DeletedUserEmail = "silinmis-kullanici@invalid"
	// What a rejection's reason becomes: the reason cannot be empty.
	ErasedNote = "[silindi]"
)

// What an erasure reports it did, in the receipt's counts. Every key is always
// there, 0 when a step changed nothing.
const (
	AccountErasureRecipientsDeleted         = "recipients_deleted"
	AccountErasureListMembershipsDeleted    = "list_memberships_deleted"
	AccountErasureQueueRowsDeleted          = "queue_rows_deleted"
	AccountErasureQueueRowsCleared          = "queue_rows_cleared"
	AccountErasureBodiesCleared             = "bodies_cleared"
	AccountErasureVariablesCleared          = "variables_cleared"
	AccountErasureActorColumnsReplaced      = "actor_columns_replaced"
	AccountErasureNotesCleared              = "notes_cleared"
	AccountErasureChangesCleared            = "changes_cleared"
	AccountErasureApprovalRecipientsRemoved = "approval_recipients_removed"
	AccountErasureApprovalPlaceholders      = "approval_recipient_placeholders"
	AccountErasureApprovalSendLinksDeleted  = "approval_send_links_deleted"
	// Open and click tracking: the person's openings and clicks deleted,
	// and the tracked links whose address names them, or that only mail to
	// them had, emptied.
	AccountErasureTrackingEventsDeleted = "tracking_events_deleted"
	AccountErasureTrackingLinksCleared  = "tracking_links_cleared"
	// The suppression list: the person's suppressed addresses deleted.
	AccountErasureSuppressionsDeleted = "suppressions_deleted"
	// The in-app inbox: the person's own items, and anyone else's whose
	// text names them.
	AccountErasureInboxItemsDeleted       = "inbox_items_deleted"
	AccountErasureInboxItemsNamingDeleted = "inbox_items_naming_deleted"
)

// ErrAccountErasureInProgress says a send to the person, or another person's
// mail naming them, is still queued or going out. What could be done was
// committed; the same command, repeated, finishes once the mailer has.
var ErrAccountErasureInProgress = errors.New("account erasure waits for mail in flight")

// AccountErasure is one Erasure command: core's deletion request, the
// person's Keycloak subject and the addresses they had. The subject and the
// addresses are used to find rows and are never stored.
type AccountErasure struct {
	RequestID uuid.UUID
	Subject   string
	Emails    []string
}

// The proof an erasure finished is AccountErasureReceipt, the row of
// account_erasure_receipts that sqlc generates (models.go): request_id,
// completed_at and counts, no subject, no address. A repeated command is
// answered with it.

// StepCounts decodes the receipt's counts: how many rows each step changed,
// keyed by the AccountErasure… names. A receipt without counts has none.
func (r *AccountErasureReceipt) StepCounts() (map[string]int64, error) {
	var counts map[string]int64
	if len(r.Counts) > 0 {
		if err := json.Unmarshal(r.Counts, &counts); err != nil {
			return nil, fmt.Errorf("account erasure receipt counts: %w", err)
		}
	}
	if counts == nil {
		counts = map[string]int64{}
	}
	return counts, nil
}

// FindAccountErasureReceipt returns the receipt of a finished erasure, or nil.
func (s *Store) FindAccountErasureReceipt(ctx context.Context, requestID uuid.UUID) (*AccountErasureReceipt, error) {
	return findAccountErasureReceipt(ctx, s.Conn, requestID)
}

type receiptReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func findAccountErasureReceipt(ctx context.Context, db receiptReader, requestID uuid.UUID) (*AccountErasureReceipt, error) {
	receipt, err := scanAccountErasureReceipt(db.QueryRow(ctx, `
		SELECT request_id, completed_at, counts FROM account_erasure_receipts WHERE request_id = $1`, requestID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return receipt, err
}

func scanAccountErasureReceipt(row pgx.Row) (*AccountErasureReceipt, error) {
	var receipt AccountErasureReceipt
	if err := row.Scan(&receipt.RequestID, &receipt.CompletedAt, &receipt.Counts); err != nil {
		return nil, err
	}
	receipt.CompletedAt = receipt.CompletedAt.UTC()
	if _, err := receipt.StepCounts(); err != nil {
		return nil, err
	}
	return &receipt, nil
}

// EraseAccount erases one person from SkyMail (ADR-0051; account erasure
// spec §3.1), in one transaction under an advisory lock on the request id,
// and writes the receipt in it. A request that already has a receipt is not
// done again: its receipt is returned. Two calls for one request at once do
// the work once and return the same receipt.
//
// Rows are found by the subject (S), by the addresses (E, compared whole and
// case-insensitively) and, in free text, by E and by the full names SkyMail
// holds for the person (N): the names on S's actor rows and on E's recipient
// rows. A single-word name is never searched for; a namesake's text naming
// the same full name is cleared too, which is the accepted cost. A text,
// variables or changes value that names the person is cleared whole, never
// masked.
//
// A send still going out to the person, or another person's mail still queued
// or going out that names them, cannot be changed under the mailer. Then only
// the work that keeps the names findable is done — the rows keyed by name
// stay until the last call — it is committed, and ErrAccountErasureInProgress
// is returned; the next call finishes. A queued row to the person is deleted
// at once, so it is never sent.
func (s *Store) EraseAccount(ctx context.Context, erasure AccountErasure) (*AccountErasureReceipt, error) {
	var receipt *AccountErasureReceipt
	inProgress := false
	err := pgx.BeginFunc(ctx, s.Conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('skymail:account-erasure:' || $1::text, 0))`,
			erasure.RequestID); err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		existing, err := findAccountErasureReceipt(ctx, tx, erasure.RequestID)
		if err != nil {
			return fmt.Errorf("read receipt: %w", err)
		}
		if existing != nil {
			receipt = existing
			return nil
		}

		e, err := newEraser(ctx, tx, erasure)
		if err != nil {
			return err
		}
		// Source sends and decisions lock source -> approval -> queue. Erasure
		// uses that same order, before its existing queue/link steps.
		if err := e.lockAndInvalidateFormSources(ctx); err != nil {
			return err
		}
		// Event writers lock queue -> link (the click's FK). Take every queue
		// row erasure will touch before any link, including another person's
		// mail whose rendered body names this person.
		if err := e.lockQueueRows(ctx); err != nil {
			return err
		}
		if err := e.run(ctx, []erasureStep{closeTrackingTokensStep}); err != nil {
			return err
		}
		// Before the person's queued rows go: a send to them alone is told by
		// its rows.
		if err := e.clearTrackedLinks(ctx); err != nil {
			return err
		}
		if err := e.run(ctx, keepNamesSteps); err != nil {
			return err
		}
		busy, err := e.mailInFlight(ctx)
		if err != nil {
			return err
		}
		if busy {
			inProgress = true
			return nil
		}
		if err := e.scrubFormSources(ctx); err != nil {
			return err
		}
		if err := e.run(ctx, dropNamesSteps); err != nil {
			return err
		}
		if err := e.replaceApprovalRecipients(ctx); err != nil {
			return err
		}
		if err := e.run(ctx, dropAddressSteps); err != nil {
			return err
		}
		receipt, err = e.writeReceipt(ctx, erasure.RequestID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("account erasure: %w", err)
	}
	if inProgress {
		return nil, ErrAccountErasureInProgress
	}
	return receipt, nil
}

// eraser is one erasure's transaction and what it looks for: @emails, E
// lower-cased; @needles, E and N lower-cased, what free text is searched for;
// @subject, S.
type eraser struct {
	tx     pgx.Tx
	args   pgx.NamedArgs
	counts map[string]int64
}

func newEraser(ctx context.Context, tx pgx.Tx, erasure AccountErasure) (*eraser, error) {
	emails := erasure.Emails
	if emails == nil {
		emails = []string{}
	}
	args := pgx.NamedArgs{
		"subject":       erasure.Subject,
		"deleted_sub":   DeletedUserSubject,
		"deleted_name":  DeletedUserName,
		"deleted_email": DeletedUserEmail,
		"erased_note":   ErasedNote,
	}

	// The names are read before anything is changed. Postgres lower-cases
	// both sides, so the addresses and the text they are looked for in are
	// folded the same way. Whitespace inside a name is collapsed; a name with
	// no space in it is one word and is not searched for, and neither is the
	// stand-in's own name.
	var lowered, names []string
	if err := tx.QueryRow(ctx, `
		WITH e AS (SELECT DISTINCT lower(btrim(x)) AS email
		           FROM unnest(@emails::text[]) AS x
		           WHERE btrim(x) <> ''),
		     held AS (SELECT full_name AS name FROM recipients WHERE lower(email) IN (SELECT email FROM e)
		              UNION ALL
		              SELECT recipient_full_name FROM mail_queue WHERE lower(recipient_email) IN (SELECT email FROM e) OR recipient_sub::text = @subject
		              UNION ALL
		              SELECT full_name FROM mail_approval_recipients WHERE lower(email) IN (SELECT email FROM e)
                          UNION ALL
                          SELECT full_name FROM mail_form_source_recipients WHERE lower(email) IN (SELECT email FROM e)
		              UNION ALL
		              SELECT submitter_name FROM mail_approvals WHERE submitter_sub = @subject
		              UNION ALL
		              SELECT actor_name FROM mail_approval_events WHERE actor_sub = @subject
		              UNION ALL
		              SELECT author_name FROM template_versions WHERE author_sub = @subject
		              UNION ALL
		              SELECT actor_name FROM template_content_sensitive_events WHERE actor_sub = @subject
		              UNION ALL
		              SELECT actor_name FROM sent_content_views WHERE actor_sub = @subject),
		     full_names AS (SELECT DISTINCT lower(btrim(regexp_replace(name, '\s+', ' ', 'g'))) AS name
		                    FROM held
		                    WHERE name IS NOT NULL)
		SELECT (SELECT COALESCE(array_agg(email ORDER BY email), '{}') FROM e),
		       (SELECT COALESCE(array_agg(name ORDER BY name), '{}')
		        FROM full_names
		        WHERE strpos(name, ' ') > 0
		          AND name <> lower(@deleted_name))`,
		pgx.NamedArgs{"emails": emails, "subject": erasure.Subject, "deleted_name": DeletedUserName},
	).Scan(&lowered, &names); err != nil {
		return nil, fmt.Errorf("read what to look for: %w", err)
	}
	args["emails"] = lowered
	args["needles"] = append(append([]string{}, lowered...), names...)

	counts := map[string]int64{}
	for _, key := range []string{
		AccountErasureRecipientsDeleted, AccountErasureListMembershipsDeleted, AccountErasureQueueRowsDeleted,
		AccountErasureQueueRowsCleared, AccountErasureBodiesCleared, AccountErasureVariablesCleared,
		AccountErasureActorColumnsReplaced, AccountErasureNotesCleared, AccountErasureChangesCleared,
		AccountErasureApprovalRecipientsRemoved, AccountErasureApprovalPlaceholders, AccountErasureApprovalSendLinksDeleted,
		AccountErasureTrackingEventsDeleted, AccountErasureTrackingLinksCleared, AccountErasureSuppressionsDeleted,
		AccountErasureInboxItemsDeleted, AccountErasureInboxItemsNamingDeleted,
	} {
		counts[key] = 0
	}
	return &eraser{tx: tx, args: args, counts: counts}, nil
}

// nameIn is the SQL condition that a text expression contains one of the
// needles, case-insensitively. strpos, not LIKE: an address or a name is
// matched as it is, with no wildcard in it.
func nameIn(text string) string {
	return "EXISTS (SELECT 1 FROM unnest(@needles::text[]) AS needle WHERE strpos(lower(" + text + "), needle) > 0)"
}

// A queue row's rendered mail names the person.
var mailNames = "(" + nameIn("q.subject") + " OR " + nameIn("q.body") + " OR " + nameIn("COALESCE(q.body_html, '')") + ")"

// A queue row is to the person when it went to one of their addresses or,
// when the send knew who it went to (mail_queue.recipient_sub), to their
// subject: an address core no longer lists is still theirs. Never NULL, so
// NOT toPerson is the rows to anyone else, a row without a subject included.
const toPerson = "(lower(q.recipient_email) = ANY (@emails::text[]) OR COALESCE(q.recipient_sub::text = @subject::text, false))"

// erasureStep is one statement; the rows it changes are added to count,
// unless it has none.
type erasureStep struct {
	name  string
	count string
	sql   string
}

func (e *eraser) run(ctx context.Context, steps []erasureStep) error {
	for _, step := range steps {
		tag, err := e.tx.Exec(ctx, step.sql, e.args)
		if err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
		if step.count != "" {
			e.counts[step.count] += tag.RowsAffected()
		}
	}
	return nil
}

// A Forms source is immutable except for erasure. Invalidating it prevents a
// pending decision from quietly sending a smaller, different snapshot.
func (e *eraser) lockAndInvalidateFormSources(ctx context.Context) error {
	rows, err := e.tx.Query(ctx, `SELECT fs.id FROM mail_form_sources fs
      WHERE fs.owner_sub=@subject OR EXISTS(SELECT 1 FROM mail_form_source_recipients r WHERE r.source_id=fs.id AND lower(r.email)=ANY(@emails::text[]))
       OR `+nameIn("fs.form_title")+` ORDER BY fs.id FOR UPDATE`, e.args)
	if err != nil {
		return fmt.Errorf("lock Forms sources: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	e.args["form_sources"] = ids
	if len(ids) == 0 {
		return nil
	}
	tag, err := e.tx.Exec(ctx, `UPDATE mail_form_sources SET invalidated_at=now() WHERE id=ANY(@form_sources::uuid[]) AND invalidated_at IS NULL`, e.args)
	if err != nil {
		return fmt.Errorf("invalidate Forms sources: %w", err)
	}
	if tag.RowsAffected() > 0 {
		e.counts["form_sources_invalidated"] += tag.RowsAffected()
	}
	return nil
}

func (e *eraser) scrubFormSources(ctx context.Context) error {
	tag, err := e.tx.Exec(ctx, `UPDATE mail_form_sources SET owner_sub=@deleted_sub WHERE owner_sub=@subject`, e.args)
	if err != nil {
		return fmt.Errorf("Forms source owner: %w", err)
	}
	e.counts[AccountErasureActorColumnsReplaced] += tag.RowsAffected()
	if _, err = e.tx.Exec(ctx, `UPDATE mail_form_sources fs SET form_title=@erased_note WHERE `+nameIn("fs.form_title"), e.args); err != nil {
		return fmt.Errorf("Forms source title: %w", err)
	}
	tag, err = e.tx.Exec(ctx, `DELETE FROM mail_form_source_recipients WHERE lower(email)=ANY(@emails::text[])`, e.args)
	if err != nil {
		return fmt.Errorf("Forms source recipients: %w", err)
	}
	if tag.RowsAffected() > 0 {
		e.counts["form_source_recipients_removed"] += tag.RowsAffected()
	}
	return nil
}

// lockQueueRows follows the same queue-before-link order as click writers.
// The second condition covers bodies cleared below: those rows can belong to
// another recipient whose click also needs a link being erased. Sorting the
// overlapping queue sets gives concurrent erasures the same lock order.
func (e *eraser) lockQueueRows(ctx context.Context) error {
	_, err := e.tx.Exec(ctx, `SELECT q.id FROM mail_queue q
 WHERE `+toPerson+` OR (q.status IN ('sent', 'failed') AND `+mailNames+`)
 ORDER BY q.id FOR UPDATE`, e.args)
	if err != nil {
		return fmt.Errorf("lock queue before tracked links: %w", err)
	}
	return nil
}

var closeTrackingTokensStep = erasureStep{
	// Close tokens before deleting events. This takes the queue-row
	// lock shared with public writers, so an in-flight event commits
	// before the following DELETE takes its fresh snapshot.
	name: "tracking tokens of mail to the person",
	sql: `UPDATE mail_queue q
		      SET tracking_disabled_at = now()
		      WHERE q.tracking_token IS NOT NULL
		        AND q.tracking_disabled_at IS NULL
		        AND ` + toPerson,
}

// keepNamesSteps change nothing the names are read from, so a call that
// stops for mail in flight leaves the next one able to find what it waited
// for.
var keepNamesSteps = []erasureStep{
	{
		// Open and click tracking: the openings and clicks of mail to the
		// person, whoever sent it.
		name:  "tracking events of mail to the person",
		count: AccountErasureTrackingEventsDeleted,
		sql: `DELETE FROM mail_tracking_events e
		      USING mail_queue q
		      WHERE e.queue_id = q.id
		        AND ` + toPerson,
	},

	{
		// Before the person's queued rows go: a send to the person alone —
		// Keycloak's reset and verification mails among them — holds values
		// for them only, the link included, whether or not they name them.
		name:  "send variables",
		count: AccountErasureVariablesCleared,
		sql: `UPDATE mail_tasks t
		      SET body_variables = '{}'::jsonb
		      WHERE t.body_variables <> '{}'::jsonb
		        AND (` + nameIn("t.body_variables::text") + `
		             OR (EXISTS (SELECT 1 FROM mail_queue q WHERE q.task_id = t.id)
		                 AND NOT EXISTS (SELECT 1 FROM mail_queue q WHERE q.task_id = t.id AND NOT ` + toPerson + `)))`,
	},
	{
		name:  "approval variables",
		count: AccountErasureVariablesCleared,
		sql: `UPDATE mail_approvals
		      SET body_variables = '{}'::jsonb
		      WHERE body_variables <> '{}'::jsonb
		        AND ` + nameIn("body_variables::text"),
	},
	{
		// The person's inbox (ADR 0070), whatever its age: items are kept
		// by the subject alone. The subject is a canonical UUID (the
		// handler checks it), so it is compared as one and the feed index
		// finds the rows.
		name:  "the person's inbox items",
		count: AccountErasureInboxItemsDeleted,
		sql:   `DELETE FROM inbox_items WHERE user_id = @subject::uuid`,
	},
	{
		// Anyone else's inbox item whose text names the person: an item is
		// a short notice, so it goes whole rather than being emptied.
		name:  "inbox items naming the person",
		count: AccountErasureInboxItemsNamingDeleted,
		sql: `DELETE FROM inbox_items i
		      WHERE (` + nameIn("i.title") + ` OR ` + nameIn("i.summary") + ` OR ` + nameIn("COALESCE(i.link, '')") + `)`,
	},
	{
		// Queue residue: never sent. A row the dispatcher has just taken is
		// processing by now, and is waited for below.
		name:  "queued rows to the person",
		count: AccountErasureQueueRowsDeleted,
		sql: `DELETE FROM mail_queue q
		      WHERE ` + toPerson + `
		        AND COALESCE(q.status::text, 'pending') = 'pending'`,
	},
	{
		// Another person's mail that names the person, once it has gone out
		// or failed for good: the recipient and the counts stay.
		name:  "bodies naming the person",
		count: AccountErasureBodiesCleared,
		sql: `UPDATE mail_queue q
		      SET subject = '', body = '', body_html = NULL
		      WHERE q.status IN ('sent', 'failed')
		        AND NOT ` + toPerson + `
		        AND ` + mailNames,
	},
	{
		// Changes recorded before a request could go to several people hold
		// {recipient_email, recipient_full_name}; later ones {recipients:
		// [{email, full_name}]}. Both are text to search.
		name:  "approval changes",
		count: AccountErasureChangesCleared,
		sql: `UPDATE mail_approval_events
		      SET changes = NULL
		      WHERE changes IS NOT NULL
		        AND ` + nameIn("changes::text"),
	},
	{
		name:  "approval notes",
		count: AccountErasureNotesCleared,
		sql: `UPDATE mail_approval_events
		      SET note = CASE WHEN kind = 'rejected' THEN @erased_note::text END
		      WHERE note IS NOT NULL
		        AND NOT (kind = 'rejected' AND note = @erased_note::text)
		        AND (actor_sub = @subject OR ` + nameIn("note") + `)`,
	},
	{
		name:  "senders",
		count: AccountErasureActorColumnsReplaced,
		sql:   `UPDATE mail_tasks SET sent_by = @deleted_sub WHERE sent_by = @subject`,
	},
	{
		name:  "template archivers",
		count: AccountErasureActorColumnsReplaced,
		sql:   `UPDATE templates SET archived_by = @deleted_sub WHERE archived_by = @subject`,
	},
	{
		name:  "list archivers",
		count: AccountErasureActorColumnsReplaced,
		sql:   `UPDATE mailing_lists SET archived_by = @deleted_sub WHERE archived_by = @subject`,
	},
}

// clearTrackedLinks empties the address of every tracked link that names
// the person or that only mail to them had (open and click tracking). A
// tracked link is one every recipient of its send had, so it names the
// person when a variable put them in it and the send went to them alone, or
// when the send names them to everyone. Either way the link then says it was
// removed. An address is searched for E and N as it is and URL-decoded (a
// name's blanks as + or %20, its Turkish letters and the @ as %XX), which
// SQL cannot do. It changes nothing the names are read from.
func (e *eraser) clearTrackedLinks(ctx context.Context) error {
	rows, err := e.tx.Query(ctx, `
		SELECT l.id,
		       l.url,
		       EXISTS (SELECT 1 FROM mail_queue q WHERE q.task_id = l.task_id)
		           AND NOT EXISTS (SELECT 1 FROM mail_queue q WHERE q.task_id = l.task_id AND NOT `+toPerson+`)
		FROM mail_task_links l
		WHERE l.url IS NOT NULL`, e.args)
	if err != nil {
		return fmt.Errorf("tracked links: %w", err)
	}
	needles, _ := e.args["needles"].([]string)
	var cleared []int64
	for rows.Next() {
		var id int64
		var link string
		var alone bool
		if err := rows.Scan(&id, &link, &alone); err != nil {
			rows.Close()
			return fmt.Errorf("tracked links: %w", err)
		}
		if alone || linkNames(link, needles) {
			cleared = append(cleared, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("tracked links: %w", err)
	}
	if len(cleared) == 0 {
		return nil
	}
	tag, err := e.tx.Exec(ctx, `
		UPDATE mail_task_links
		SET url = NULL, url_cleared_reason = 'erased'
		WHERE id = ANY ($1::bigint[])`, cleared)
	if err != nil {
		return fmt.Errorf("tracked links: %w", err)
	}
	e.counts[AccountErasureTrackingLinksCleared] += tag.RowsAffected()
	return nil
}

// linkNames reports whether an address holds one of the needles (addresses
// and full names), as written or URL-decoded. Both sides are lower-cased
// here: the needles come from Postgres's lower(), which under the C ctype
// leaves letters such as İ and Ş as they are.
func linkNames(link string, needles []string) bool {
	forms := []string{strings.ToLower(link)}
	if decoded, err := url.QueryUnescape(link); err == nil {
		forms = append(forms, strings.ToLower(decoded))
	}
	if decoded, err := url.PathUnescape(link); err == nil {
		forms = append(forms, strings.ToLower(decoded))
	}
	for _, form := range forms {
		for _, needle := range needles {
			if needle != "" && strings.Contains(form, strings.ToLower(needle)) {
				return true
			}
		}
	}
	return false
}

// mailInFlight says whether mail the erasure must change is still with the
// mailer: a row to the person it is sending now, or another person's row
// naming the person that is queued or being sent. The mailer sends what the
// row holds; clearing it under the mailer would send an empty mail or have
// the mailer's outcome write over the erasure.
func (e *eraser) mailInFlight(ctx context.Context) (bool, error) {
	var busy bool
	err := e.tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM mail_queue q WHERE q.status = 'processing' AND `+toPerson+`)
		    OR EXISTS (SELECT 1
		               FROM mail_queue q
		               WHERE q.status IN ('pending', 'processing')
		                 AND NOT `+toPerson+`
		                 AND `+mailNames+`)`, e.args).Scan(&busy)
	if err != nil {
		return false, fmt.Errorf("mail in flight: %w", err)
	}
	return busy, nil
}

// dropNamesSteps replace the subject where it carries a name.
var dropNamesSteps = []erasureStep{
	{
		name:  "suppression removers",
		count: AccountErasureActorColumnsReplaced,
		sql: `UPDATE mail_suppression_removals
		      SET actor_sub = @deleted_sub
		      WHERE actor_sub = @subject`,
	},
	{
		name:  "approval actors",
		count: AccountErasureActorColumnsReplaced,
		sql: `UPDATE mail_approval_events
		      SET actor_sub = @deleted_sub, actor_name = @deleted_name
		      WHERE actor_sub = @subject`,
	},
	{
		name:  "approval submitters",
		count: AccountErasureActorColumnsReplaced,
		sql: `UPDATE mail_approvals
		      SET submitter_sub = @deleted_sub,
		          submitter_name = @deleted_name,
		          submitter_email = NULL,
		          submitter_email_unverified = false
		      WHERE submitter_sub = @subject`,
	},
	{
		name:  "template version authors",
		count: AccountErasureActorColumnsReplaced,
		sql: `UPDATE template_versions
		      SET author_sub = @deleted_sub, author_name = @deleted_name
		      WHERE author_sub = @subject`,
	},
	{
		name:  "template content_sensitive changes",
		count: AccountErasureActorColumnsReplaced,
		sql: `UPDATE template_content_sensitive_events
		      SET actor_sub = @deleted_sub, actor_name = @deleted_name
		      WHERE actor_sub = @subject`,
	},
	{
		name:  "openings of sensitive content",
		count: AccountErasureActorColumnsReplaced,
		sql: `UPDATE sent_content_views
		      SET actor_sub = @deleted_sub, actor_name = @deleted_name
		      WHERE actor_sub = @subject`,
	},
}

// replaceApprovalRecipients takes the person out of every Mail onayı
// request's people, keeping what the request promises:
//
//   - a request goes to a list or to at least one person (the deferred
//     check_mail_approval trigger): a request the person was the last one of
//     gets one Silinmiş kullanıcı instead;
//   - task_ids[i] is the send to recipients[i], paired by position: where a
//     send was queued to the person, Silinmiş kullanıcı takes their place.
//     An address is in a request once, so it can take one place per request;
//     a second of the person's addresses there goes with its send's link —
//     the send itself stays in mail_tasks.
func (e *eraser) replaceApprovalRecipients(ctx context.Context) error {
	tag, err := e.tx.Exec(ctx, `
		UPDATE mail_approval_recipients r
		SET email = @deleted_email, full_name = @deleted_name
		FROM (SELECT DISTINCT ON (p.approval_id) p.approval_id, p.position
		      FROM mail_approval_recipients p
		               JOIN mail_approval_tasks s ON s.approval_id = p.approval_id AND s.position = p.position
		      WHERE lower(p.email) = ANY (@emails::text[])
		        AND NOT EXISTS (SELECT 1
		                        FROM mail_approval_recipients o
		                        WHERE o.approval_id = p.approval_id
		                          AND lower(o.email) = @deleted_email)
		      ORDER BY p.approval_id, p.position) pick
		WHERE r.approval_id = pick.approval_id
		  AND r.position = pick.position`, e.args)
	if err != nil {
		return fmt.Errorf("approval recipients in place: %w", err)
	}
	e.counts[AccountErasureApprovalRecipientsRemoved] += tag.RowsAffected()
	e.counts[AccountErasureApprovalPlaceholders] += tag.RowsAffected()

	tag, err = e.tx.Exec(ctx, `
		DELETE FROM mail_approval_tasks s
		USING mail_approval_recipients p
		WHERE s.approval_id = p.approval_id
		  AND s.position = p.position
		  AND lower(p.email) = ANY (@emails::text[])`, e.args)
	if err != nil {
		return fmt.Errorf("approval send links: %w", err)
	}
	e.counts[AccountErasureApprovalSendLinksDeleted] += tag.RowsAffected()

	rows, err := e.tx.Query(ctx, `
		DELETE FROM mail_approval_recipients
		WHERE lower(email) = ANY (@emails::text[])
		RETURNING approval_id`, e.args)
	if err != nil {
		return fmt.Errorf("approval recipients: %w", err)
	}
	emptied, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return fmt.Errorf("approval recipients: %w", err)
	}
	e.counts[AccountErasureApprovalRecipientsRemoved] += int64(len(emptied))
	if len(emptied) == 0 {
		return nil
	}

	args := pgx.NamedArgs{"approvals": emptied, "deleted_email": DeletedUserEmail, "deleted_name": DeletedUserName}
	tag, err = e.tx.Exec(ctx, `
		INSERT INTO mail_approval_recipients (approval_id, position, email, full_name)
		SELECT a.id, 1, @deleted_email, @deleted_name
		FROM mail_approvals a
		WHERE a.id = ANY (@approvals::uuid[])
		  AND a.mail_list_id IS NULL
		  AND NOT EXISTS (SELECT 1 FROM mail_approval_recipients r WHERE r.approval_id = a.id)`, args)
	if err != nil {
		return fmt.Errorf("approval placeholders: %w", err)
	}
	e.counts[AccountErasureApprovalPlaceholders] += tag.RowsAffected()
	return nil
}

// dropAddressSteps remove the person's addresses, last: the names on these
// rows are what the steps before them searched for.
var dropAddressSteps = []erasureStep{
	{
		// The person's suppressed addresses: by address, by subject, and by
		// the mail to them that was refused last — before that mail's
		// address is cleared below. Their bounces stay with that mail, which
		// keeps no address: codes only.
		name:  "suppressions",
		count: AccountErasureSuppressionsDeleted,
		sql: `DELETE FROM mail_suppressions s
		      WHERE s.address = ANY (@emails::text[])
		         OR COALESCE(s.recipient_sub::text = @subject::text, false)
		         OR s.last_queue_id IN (SELECT q.id FROM mail_queue q WHERE ` + toPerson + `)`,
	},
	{
		// Sent and failed mail to the person stays as a row, so every count
		// of a send stays; everything in it about them goes.
		name:  "mail to the person",
		count: AccountErasureQueueRowsCleared,
		sql: `UPDATE mail_queue q
		      SET recipient_full_name = @deleted_name,
		          recipient_email = '',
		          subject = '',
		          body = '',
		          body_html = NULL,
		          error = NULL,
		          recipient_sub = NULL
		      WHERE q.status IN ('sent', 'failed')
		        AND ` + toPerson,
	},
	{
		name:  "list memberships",
		count: AccountErasureListMembershipsDeleted,
		sql: `DELETE FROM mailing_list_recipients m
		      USING recipients r
		      WHERE m.recipient_id = r.id
		        AND lower(r.email) = ANY (@emails::text[])`,
	},
	{
		name:  "recipients",
		count: AccountErasureRecipientsDeleted,
		sql:   `DELETE FROM recipients WHERE lower(email) = ANY (@emails::text[])`,
	},
}

func (e *eraser) writeReceipt(ctx context.Context, requestID uuid.UUID) (*AccountErasureReceipt, error) {
	counts, err := json.Marshal(e.counts)
	if err != nil {
		return nil, err
	}
	receipt, err := scanAccountErasureReceipt(e.tx.QueryRow(ctx, `
		INSERT INTO account_erasure_receipts (request_id, completed_at, counts)
		VALUES ($1, clock_timestamp(), $2::jsonb)
		RETURNING request_id, completed_at, counts`, requestID, string(counts)))
	if err != nil {
		return nil, fmt.Errorf("write receipt: %w", err)
	}
	return receipt, nil
}
