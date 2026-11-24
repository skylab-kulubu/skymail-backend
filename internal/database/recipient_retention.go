package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The retention of recipients without an account (ADR-0062; hub sector audit
// 2026-11-24, 06 fix 3; docs/data-lifecycle.md, "Recipients without an
// account"; README, "Hesapsız alıcıların saklaması"):
//
//   - the people of a manual list (mailing_list_recipients, and a recipients
//     row no other list holds) go a year after the list was archived;
//   - the address and name a send keeps of a recipient without an account
//     (mail_queue.recipient_email, recipient_full_name) are emptied once the
//     send's content is gone (MAIL_CONTENT_RETENTION_DAYS, the content
//     sweep's mail_tasks.content_cleared_at), and the address is masked in
//     the row's error;
//   - so are those a Mail onayı request keeps (mail_approval_recipients),
//     once the request is over (approved, rejected, declined or expired) and
//     neither it nor its sends changed for MAIL_CONTENT_RETENTION_DAYS.
//
// A member's rows live with the account (ADR-0062: "üyede hesapla birlikte")
// and go with Account erasure: a row is emptied only when it names no subject
// (recipient_sub is null), SkyMail never tied its address to one (another
// queue row or a suppression), and Keycloak answered that no user has the
// address as primary, school or personal e-mail. A Keycloak that does not
// answer leaves the row as it is, for the next run.

// RecipientRetentionMode is what the run does: SKYMAIL_RECIPIENT_RETENTION.
type RecipientRetentionMode string

const (
	// RecipientRetentionOff runs nothing.
	RecipientRetentionOff RecipientRetentionMode = "off"
	// RecipientRetentionDryRun runs every rule in transactions it rolls
	// back: it counts what apply would change and changes nothing. The
	// default.
	RecipientRetentionDryRun RecipientRetentionMode = "dry-run"
	// RecipientRetentionApply changes the rows.
	RecipientRetentionApply RecipientRetentionMode = "apply"
)

// RecipientRetentionModeEnv names the variable that holds the mode.
const RecipientRetentionModeEnv = "SKYMAIL_RECIPIENT_RETENTION"

// RecipientRetentionModeFromEnv reads SKYMAIL_RECIPIENT_RETENTION: off,
// dry-run (or unset) or apply. Anything else stops startup, so a typo never
// turns destruction on or off.
func RecipientRetentionModeFromEnv(getenv func(string) string) (RecipientRetentionMode, error) {
	raw := strings.TrimSpace(getenv(RecipientRetentionModeEnv))
	switch RecipientRetentionMode(raw) {
	case "", RecipientRetentionDryRun:
		return RecipientRetentionDryRun, nil
	case RecipientRetentionOff, RecipientRetentionApply:
		return RecipientRetentionMode(raw), nil
	}
	return "", fmt.Errorf("%s must be off, dry-run or apply, not %q", RecipientRetentionModeEnv, raw)
}

// RecipientRetentionMaxRowsEnv names the brake: the most list memberships,
// recipients, queue rows or request people one apply may change before it
// changes nothing (refused_large).
const RecipientRetentionMaxRowsEnv = "SKYMAIL_RECIPIENT_RETENTION_MAX_ROWS"

// DefaultRecipientRetentionMaxRows is the brake when the variable is unset.
const DefaultRecipientRetentionMaxRows = 50000

// maxRecipientRetentionMaxRows bounds the brake: a typo with extra zeros
// must not turn it off.
const maxRecipientRetentionMaxRows = 10000000

// RecipientRetentionMaxRowsFromEnv reads SKYMAIL_RECIPIENT_RETENTION_MAX_ROWS,
// a whole number from 1 to 10,000,000; unset is
// DefaultRecipientRetentionMaxRows. Anything else stops startup.
func RecipientRetentionMaxRowsFromEnv(getenv func(string) string) (int64, error) {
	raw := strings.TrimSpace(getenv(RecipientRetentionMaxRowsEnv))
	if raw == "" {
		return DefaultRecipientRetentionMaxRows, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 || n > maxRecipientRetentionMaxRows {
		return 0, fmt.Errorf("%s must be a whole number from 1 to %d, not %q", RecipientRetentionMaxRowsEnv, maxRecipientRetentionMaxRows, raw)
	}
	return n, nil
}

// RecipientRetentionLockName names the session advisory lock one run holds:
// one run at a time across replicas.
const RecipientRetentionLockName = "skymail-backend recipient retention"

// ErrRecipientRetentionLocked is returned when another run holds the lock.
var ErrRecipientRetentionLocked = errors.New("recipient retention: another run holds the lock")

// ErrRecipientRetentionNotDue is returned by a run with OnlyIfDue that
// finds, holding the lock, that a run of its mode is not due.
var ErrRecipientRetentionNotDue = errors.New("recipient retention: not due")

const (
	// recipientRetentionLockTimeout bounds how long one list's, send's or
	// request's transaction waits for a row another transaction holds; past
	// it that one is left for the next run.
	recipientRetentionLockTimeout = "5s"
	// recipientRetentionBatch is the most sends, and the most requests, one
	// run looks at; the rest wait for the next day's run.
	recipientRetentionBatch = 2000
	// recipientRetentionLookups is the most addresses one run asks Keycloak
	// about; the rest wait (addresses_deferred).
	recipientRetentionLookups = 2000
	// recipientRetentionLookupsAtOnce is how many Keycloak asks run at once.
	recipientRetentionLookupsAtOnce = 4
	// recipientRetentionLookupBudget bounds all of a run's Keycloak asks.
	recipientRetentionLookupBudget = 5 * time.Minute
)

// AccountFinder says whether an address is someone's account
// (keycloak.AccountFinder).
type AccountFinder interface {
	HasAccount(ctx context.Context, address string) (bool, error)
}

// RecipientRetentionOptions say how to run.
type RecipientRetentionOptions struct {
	// Mode is dry-run or apply.
	Mode RecipientRetentionMode
	// ContentRetentionDays is MAIL_CONTENT_RETENTION_DAYS: how long a Mail
	// onayı request that is over keeps its people. Sends follow the content
	// sweep, which empties their content after the same days.
	ContentRetentionDays int
	// Accounts answers whether an address is someone's account. Nil: no one
	// is asked, and no address of a send or a request is emptied.
	Accounts AccountFinder
	// BrakeMaxRows replaces DefaultRecipientRetentionMaxRows when positive.
	BrakeMaxRows int64
	// OnlyIfDue makes the run check, once it holds the lock, that a run of
	// its mode is due, and do nothing otherwise: the schedule's runs.
	OnlyIfDue bool
}

// RecipientRetentionCounts is what a run changed (apply) or would change (a
// dry run, a refused apply). Counts only: no person, address or name.
type RecipientRetentionCounts struct {
	// Manual lists archived a year ago or more that still had people.
	Lists                  int64 `json:"lists"`
	ListMembershipsDeleted int64 `json:"list_memberships_deleted"`
	// recipients rows no list holds any more.
	RecipientsDeleted int64 `json:"recipients_deleted"`
	// Sends whose content is gone and whose people were not decided yet.
	Sends            int64 `json:"sends"`
	QueueRowsCleared int64 `json:"queue_rows_cleared"`
	// Sends all of whose people are decided now (recipient_retention_at).
	SendsDecided int64 `json:"sends_decided"`
	// Mail onayı requests over for MAIL_CONTENT_RETENTION_DAYS.
	Approvals                 int64 `json:"approvals"`
	ApprovalRecipientsCleared int64 `json:"approval_recipients_cleared"`
	ApprovalsDecided          int64 `json:"approvals_decided"`
	// Distinct addresses of those sends and requests with no subject.
	Addresses int64 `json:"addresses"`
	// Of them: tied to a subject in SkyMail (another queue row, a
	// suppression), or Account erasure's stand-in.
	AddressesTied int64 `json:"addresses_tied"`
	// Keycloak found a user with the address.
	AddressesWithAccount int64 `json:"addresses_with_account"`
	// Keycloak found none: these are emptied.
	AddressesWithoutAccount int64 `json:"addresses_without_account"`
	// Keycloak did not answer, or there is no Keycloak: kept for now.
	AddressesUnknown int64 `json:"addresses_unknown"`
	// Past the run's lookup limit: kept for the next run.
	AddressesDeferred int64 `json:"addresses_deferred"`
	// Lists, sends or requests whose transaction failed (the run is then
	// partial).
	Failed int64 `json:"failed"`
}

func (c *RecipientRetentionCounts) addChanges(o RecipientRetentionCounts) {
	c.Lists += o.Lists
	c.ListMembershipsDeleted += o.ListMembershipsDeleted
	c.RecipientsDeleted += o.RecipientsDeleted
	c.Sends += o.Sends
	c.QueueRowsCleared += o.QueueRowsCleared
	c.SendsDecided += o.SendsDecided
	c.Approvals += o.Approvals
	c.ApprovalRecipientsCleared += o.ApprovalRecipientsCleared
	c.ApprovalsDecided += o.ApprovalsDecided
	c.Failed += o.Failed
}

func (c RecipientRetentionCounts) refusedBy(brake int64) bool {
	return c.ListMembershipsDeleted > brake || c.RecipientsDeleted > brake ||
		c.QueueRowsCleared > brake || c.ApprovalRecipientsCleared > brake
}

// RecipientRetentionReport is what a run did; its receipt is the
// recipient_retention_runs row RunID.
type RecipientRetentionReport struct {
	RunID int64
	Mode  RecipientRetentionMode
	// ok; partial (a list, send or request failed, or Keycloak did not
	// answer for an address); refused_large (apply: the brake refused,
	// nothing changed); would_refuse_large (a dry run whose apply the brake
	// would refuse); failed.
	Status string
	Counts RecipientRetentionCounts
}

const recipientRetentionDueSQL = `
	SELECT NOT EXISTS (SELECT 1 FROM recipient_retention_runs
	                   WHERE mode = $1 AND status IN ('ok', 'partial', 'refused_large', 'would_refuse_large')
	                     AND started_at > now() - interval '23 hours')`

// RecipientRetentionDue reports whether a run of mode is due: none ended ok,
// partial or refused in the last 23 hours. A failed run does not count;
// changing the mode makes a run of the new mode due at once.
func (s *Store) RecipientRetentionDue(ctx context.Context, mode RecipientRetentionMode) (bool, error) {
	var due bool
	err := s.Conn.QueryRow(ctx, recipientRetentionDueSQL, string(mode)).Scan(&due)
	return due, err
}

// RunRecipientRetention runs the rules once, holding the advisory lock, and
// writes its receipt. Apply first runs the dry run; when a count passes the
// brake it changes nothing (refused_large). Each list, send and request is
// one transaction of its own; one that fails is counted and left for the
// next run.
func (s *Store) RunRecipientRetention(ctx context.Context, opts RecipientRetentionOptions) (*RecipientRetentionReport, error) {
	switch opts.Mode {
	case RecipientRetentionDryRun, RecipientRetentionApply:
	default:
		return nil, fmt.Errorf("recipient retention: mode %q runs nothing", opts.Mode)
	}
	if opts.ContentRetentionDays < 1 {
		return nil, errors.New("recipient retention: no content retention days")
	}
	brake := opts.BrakeMaxRows
	if brake <= 0 {
		brake = DefaultRecipientRetentionMaxRows
	}

	conn, err := s.Conn.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("recipient retention: %w", err)
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, RecipientRetentionLockName).Scan(&locked); err != nil {
		return nil, fmt.Errorf("recipient retention lock: %w", err)
	}
	if !locked {
		return nil, ErrRecipientRetentionLocked
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, RecipientRetentionLockName)
	}()

	if opts.OnlyIfDue {
		var due bool
		if err := conn.QueryRow(ctx, recipientRetentionDueSQL, string(opts.Mode)).Scan(&due); err != nil {
			return nil, fmt.Errorf("recipient retention due: %w", err)
		}
		if !due {
			return nil, ErrRecipientRetentionNotDue
		}
	}

	report := &RecipientRetentionReport{Mode: opts.Mode}
	var now time.Time
	if err := s.Conn.QueryRow(ctx, `
		INSERT INTO recipient_retention_runs (mode) VALUES ($1) RETURNING id, now()`,
		string(opts.Mode)).Scan(&report.RunID, &now); err != nil {
		return nil, fmt.Errorf("recipient retention receipt: %w", err)
	}

	refused := false
	runErr := func() error {
		plan, err := s.recipientRetentionPlan(ctx, now, opts)
		if err != nil {
			return err
		}
		report.Counts = plan.counts
		// The dry run, which apply runs first.
		changes, err := s.recipientRetentionPass(ctx, plan, now, false)
		report.Counts.addChanges(changes)
		if err != nil {
			return err
		}
		// The brake decides on the dry run's counts, and only those.
		refused = report.Counts.refusedBy(brake)
		if opts.Mode == RecipientRetentionApply && !refused {
			report.Counts = plan.counts
			changes, err = s.recipientRetentionPass(ctx, plan, now, true)
			report.Counts.addChanges(changes)
		}
		return err
	}()

	switch {
	case runErr != nil:
		report.Status = "failed"
	case refused && opts.Mode == RecipientRetentionApply:
		report.Status = "refused_large"
	case refused:
		report.Status = "would_refuse_large"
	case report.Counts.Failed > 0 || report.Counts.AddressesUnknown > 0:
		report.Status = "partial"
	default:
		report.Status = "ok"
	}
	counts, err := json.Marshal(report.Counts)
	if err != nil {
		return nil, err
	}
	if _, err := s.Conn.Exec(context.WithoutCancel(ctx), `
		UPDATE recipient_retention_runs SET status = $2, finished_at = clock_timestamp(), counts = $3::jsonb WHERE id = $1`,
		report.RunID, report.Status, string(counts)); err != nil && runErr == nil {
		runErr = fmt.Errorf("recipient retention receipt: %w", err)
	}
	if runErr != nil {
		return report, fmt.Errorf("recipient retention: %w", runErr)
	}
	return report, nil
}

// recipientRetentionPlan is what one run looks at, found once and used by
// the dry run and by apply alike.
type recipientRetentionPlan struct {
	lists     []uuid.UUID
	sends     []uuid.UUID
	approvals []uuid.UUID
	// Lower-cased addresses whose rows are emptied.
	withoutAccount []string
	// Lower-cased addresses that are someone's: kept.
	withAccount []string
	// MAIL_CONTENT_RETENTION_DAYS, which approvalDue reads.
	days   int
	counts RecipientRetentionCounts
}

// recipientRetentionPlan finds the lists, sends and requests due and decides
// their addresses.
func (s *Store) recipientRetentionPlan(ctx context.Context, now time.Time, opts RecipientRetentionOptions) (*recipientRetentionPlan, error) {
	plan := &recipientRetentionPlan{days: opts.ContentRetentionDays}
	args := pgx.NamedArgs{"now": now, "days": opts.ContentRetentionDays, "batch": recipientRetentionBatch}
	var err error
	if plan.lists, err = collectIDs(ctx, s, `
		SELECT l.id FROM mailing_lists l
		WHERE l.archived_at <= @now::timestamptz - interval '1 year'
		  AND EXISTS (SELECT 1 FROM mailing_list_recipients m WHERE m.mail_list_id = l.id)
		ORDER BY l.archived_at, l.id`, args); err != nil {
		return nil, fmt.Errorf("lists due: %w", err)
	}
	if plan.sends, err = collectIDs(ctx, s, `
		SELECT t.id FROM mail_tasks t
		WHERE t.content_cleared_at IS NOT NULL AND t.recipient_retention_at IS NULL
		ORDER BY t.content_cleared_at, t.id
		LIMIT @batch::int`, args); err != nil {
		return nil, fmt.Errorf("sends due: %w", err)
	}
	if plan.approvals, err = collectIDs(ctx, s, `
		SELECT a.id FROM mail_approvals a
		WHERE `+approvalDue+`
		ORDER BY a.updated_at, a.id
		LIMIT @batch::int`, args); err != nil {
		return nil, fmt.Errorf("requests due: %w", err)
	}

	// Their addresses with no subject, and of those the ones SkyMail
	// already ties to a subject.
	rows, err := s.Conn.Query(ctx, `
		SELECT DISTINCT lower(btrim(q.recipient_email)) FROM mail_queue q
		WHERE q.task_id = ANY (@sends::uuid[]) AND q.recipient_sub IS NULL AND btrim(q.recipient_email) <> ''
		  AND q.status IN ('sent', 'failed')
		UNION
		SELECT lower(btrim(r.email)) FROM mail_approval_recipients r
		WHERE r.approval_id = ANY (@approvals::uuid[]) AND r.cleared_at IS NULL AND btrim(r.email) <> ''`,
		pgx.NamedArgs{"sends": plan.sends, "approvals": plan.approvals})
	if err != nil {
		return nil, fmt.Errorf("addresses: %w", err)
	}
	addresses, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("addresses: %w", err)
	}
	sort.Strings(addresses)
	plan.counts.Addresses = int64(len(addresses))
	tied := map[string]bool{DeletedUserEmail: true}
	if len(addresses) > 0 {
		rows, err = s.Conn.Query(ctx, `
			SELECT lower(btrim(q.recipient_email)) FROM mail_queue q
			WHERE q.recipient_sub IS NOT NULL AND lower(btrim(q.recipient_email)) = ANY (@addresses::text[])
			UNION
			SELECT s.address FROM mail_suppressions s
			WHERE s.recipient_sub IS NOT NULL AND s.address = ANY (@addresses::text[])`,
			pgx.NamedArgs{"addresses": addresses})
		if err != nil {
			return nil, fmt.Errorf("addresses tied to a subject: %w", err)
		}
		found, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, fmt.Errorf("addresses tied to a subject: %w", err)
		}
		for _, address := range found {
			tied[address] = true
		}
	}

	var ask []string
	for _, address := range addresses {
		switch {
		case tied[address]:
			plan.counts.AddressesTied++
			plan.withAccount = append(plan.withAccount, address)
		case len(ask) >= recipientRetentionLookups:
			plan.counts.AddressesDeferred++
		default:
			ask = append(ask, address)
		}
	}
	if opts.Accounts == nil {
		plan.counts.AddressesUnknown += int64(len(ask))
		return plan, nil
	}
	answers := lookUpRecipientAccounts(ctx, opts.Accounts, ask)
	for _, address := range ask {
		has, answered := answers[address]
		switch {
		case !answered:
			plan.counts.AddressesUnknown++
		case has:
			plan.counts.AddressesWithAccount++
			plan.withAccount = append(plan.withAccount, address)
		default:
			plan.counts.AddressesWithoutAccount++
			plan.withoutAccount = append(plan.withoutAccount, address)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return plan, nil
}

// approvalDue is a Mail onayı request whose people the run may look at: it
// is over, nothing happened to it for @days, the content of every send it
// queued is gone, and the run has not decided its people since it last
// changed.
const approvalDue = `a.state IN ('approved', 'rejected', 'declined', 'expired')
		  AND a.updated_at <= @now::timestamptz - make_interval(days => @days::int)
		  AND (a.recipient_retention_at IS NULL OR a.recipient_retention_at < a.updated_at)
		  AND NOT EXISTS (SELECT 1 FROM mail_approval_tasks s JOIN mail_tasks t ON t.id = s.task_id
		                  WHERE s.approval_id = a.id AND t.content_cleared_at IS NULL)`

func collectIDs(ctx context.Context, s *Store, sql string, args pgx.NamedArgs) ([]uuid.UUID, error) {
	rows, err := s.Conn.Query(ctx, sql, args)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// lookUpRecipientAccounts asks accounts about each address, a few at once
// and all within recipientRetentionLookupBudget. An address it did not
// answer for is left out.
func lookUpRecipientAccounts(ctx context.Context, accounts AccountFinder, addresses []string) map[string]bool {
	answers := make(map[string]bool, len(addresses))
	if len(addresses) == 0 {
		return answers
	}
	ask, cancel := context.WithTimeout(ctx, recipientRetentionLookupBudget)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, recipientRetentionLookupsAtOnce)
	for _, address := range addresses {
		if ask.Err() != nil {
			break
		}
		wg.Add(1)
		slots <- struct{}{}
		go func(address string) {
			defer wg.Done()
			defer func() { <-slots }()
			has, err := accounts.HasAccount(ask, address)
			if err != nil {
				return
			}
			mu.Lock()
			answers[address] = has
			mu.Unlock()
		}(address)
	}
	wg.Wait()
	return answers
}

// recipientRetentionPass runs the rules on each list, send and request,
// committing each one's transaction when commit is true and rolling it back
// otherwise.
func (s *Store) recipientRetentionPass(ctx context.Context, plan *recipientRetentionPlan, now time.Time, commit bool) (RecipientRetentionCounts, error) {
	var total RecipientRetentionCounts
	args := pgx.NamedArgs{
		"now":             now,
		"days":            plan.days,
		"without_account": nonNil(plan.withoutAccount),
		"with_account":    nonNil(plan.withAccount),
	}
	groups := []struct {
		ids   []uuid.UUID
		key   string
		rules []recipientRetentionRule
	}{
		{plan.lists, "list", archivedListRules},
		{plan.sends, "task", sendRecipientRules},
		{plan.approvals, "approval", approvalRecipientRules},
	}
	for _, group := range groups {
		for _, id := range group.ids {
			if err := ctx.Err(); err != nil {
				return total, err
			}
			args[group.key] = id
			counts, err := s.recipientRetentionOne(ctx, group.rules, args, commit)
			if err != nil {
				if ctx.Err() != nil {
					return total, ctx.Err()
				}
				total.Failed++
				continue
			}
			total.addChanges(counts)
		}
		delete(args, group.key)
	}
	return total, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// recipientRetentionRule is one statement of a list's, send's or request's
// transaction. A rule with stop ends the transaction, changing nothing, when
// it touches no row: what was due is not any more.
type recipientRetentionRule struct {
	sql   string
	stop  bool
	count func(*RecipientRetentionCounts, int64)
}

// archivedListRules: the people of a manual list archived a year ago or more
// (@list). A recipients row goes too once no list holds it.
var archivedListRules = []recipientRetentionRule{
	{
		sql: `SELECT l.id FROM mailing_lists l
		      WHERE l.id = @list AND l.archived_at <= @now::timestamptz - interval '1 year'
		      FOR UPDATE`,
		stop:  true,
		count: func(c *RecipientRetentionCounts, n int64) { c.Lists += n },
	},
	{
		// The list's recipients rows, by id, before anything is decided
		// about them: a list adding one of them now waits, and the next
		// statement sees what it wrote.
		sql: `SELECT r.id FROM recipients r JOIN mailing_list_recipients m ON m.recipient_id = r.id
		      WHERE m.mail_list_id = @list
		      ORDER BY r.id FOR UPDATE OF r`,
	},
	{
		// A recipients row no other list holds goes, and its membership of
		// this list with it (ON DELETE CASCADE).
		sql: `DELETE FROM recipients r USING mailing_list_recipients m
		      WHERE m.mail_list_id = @list AND m.recipient_id = r.id
		        AND NOT EXISTS (SELECT 1 FROM mailing_list_recipients o WHERE o.recipient_id = r.id AND o.mail_list_id <> @list)`,
		count: func(c *RecipientRetentionCounts, n int64) {
			c.RecipientsDeleted += n
			c.ListMembershipsDeleted += n
		},
	},
	{
		// The memberships of people other lists still hold.
		sql:   `DELETE FROM mailing_list_recipients WHERE mail_list_id = @list`,
		count: func(c *RecipientRetentionCounts, n int64) { c.ListMembershipsDeleted += n },
	},
}

// sendRecipientRules: the recipients without an account of a send whose
// content is gone (@task). The rows are locked first, by id, as everything
// that changes them does (tracking retention, Account erasure).
var sendRecipientRules = []recipientRetentionRule{
	{
		sql: `SELECT t.id FROM mail_tasks t
		      WHERE t.id = @task AND t.content_cleared_at IS NOT NULL AND t.recipient_retention_at IS NULL`,
		stop:  true,
		count: func(c *RecipientRetentionCounts, n int64) { c.Sends += n },
	},
	{
		sql: `SELECT q.id FROM mail_queue q
		      WHERE q.task_id = @task AND q.recipient_sub IS NULL AND btrim(q.recipient_email) <> ''
		      ORDER BY q.id FOR UPDATE`,
	},
	{
		// The address and the name go; the error keeps its codes, with any
		// address in it masked (a row from before errors held codes only).
		sql: `UPDATE mail_queue q
		      SET recipient_email = '', recipient_full_name = '',
		          error = regexp_replace(q.error, '` + addressPattern + `', '••••••', 'g')
		      WHERE q.task_id = @task AND q.recipient_sub IS NULL AND btrim(q.recipient_email) <> ''
		        AND q.status IN ('sent', 'failed')
		        AND lower(btrim(q.recipient_email)) = ANY (@without_account::text[])`,
		count: func(c *RecipientRetentionCounts, n int64) { c.QueueRowsCleared += n },
	},
	{
		// Decided: no row is still to go out, and every address left is
		// someone's.
		sql: `UPDATE mail_tasks t SET recipient_retention_at = @now::timestamptz
		      WHERE t.id = @task AND t.recipient_retention_at IS NULL
		        AND NOT EXISTS (SELECT 1 FROM mail_queue q
		                        WHERE q.task_id = t.id
		                          AND (q.status IN ('pending', 'processing')
		                               OR (q.recipient_sub IS NULL AND btrim(q.recipient_email) <> ''
		                                   AND NOT lower(btrim(q.recipient_email)) = ANY (@with_account::text[]))))`,
		count: func(c *RecipientRetentionCounts, n int64) { c.SendsDecided += n },
	},
}

// approvalRecipientRules: the people without an account of a Mail onayı
// request that is over (@approval). The request is locked first, as its
// routes lock it.
var approvalRecipientRules = []recipientRetentionRule{
	{
		sql: `SELECT a.id FROM mail_approvals a
		      WHERE a.id = @approval AND ` + approvalDue + `
		      FOR UPDATE OF a`,
		stop:  true,
		count: func(c *RecipientRetentionCounts, n int64) { c.Approvals += n },
	},
	{
		sql: `UPDATE mail_approval_recipients r
		      SET email = '', full_name = '', cleared_at = @now::timestamptz
		      WHERE r.approval_id = @approval AND r.cleared_at IS NULL
		        AND lower(btrim(r.email)) = ANY (@without_account::text[])`,
		count: func(c *RecipientRetentionCounts, n int64) { c.ApprovalRecipientsCleared += n },
	},
	{
		sql: `UPDATE mail_approvals a SET recipient_retention_at = @now::timestamptz
		      WHERE a.id = @approval
		        AND NOT EXISTS (SELECT 1 FROM mail_approval_recipients r
		                        WHERE r.approval_id = a.id AND r.cleared_at IS NULL
		                          AND NOT lower(btrim(r.email)) = ANY (@with_account::text[]))`,
		count: func(c *RecipientRetentionCounts, n int64) { c.ApprovalsDecided += n },
	},
}

// addressPattern finds an e-mail address in a row's error text.
const addressPattern = `[^[:space:]<>"'(),;:\[\]]+@[^[:space:]<>"'(),;:\[\]]+`

// recipientRetentionOne runs rules in one transaction.
func (s *Store) recipientRetentionOne(ctx context.Context, rules []recipientRetentionRule, args pgx.NamedArgs, commit bool) (RecipientRetentionCounts, error) {
	var counts RecipientRetentionCounts
	tx, err := s.Conn.Begin(ctx)
	if err != nil {
		return counts, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '`+recipientRetentionLockTimeout+`'`); err != nil {
		return counts, err
	}
	for _, rule := range rules {
		tag, err := tx.Exec(ctx, rule.sql, args)
		if err != nil {
			return counts, err
		}
		n := tag.RowsAffected()
		if rule.count != nil {
			rule.count(&counts, n)
		}
		if rule.stop && n == 0 {
			return RecipientRetentionCounts{}, nil
		}
	}
	if commit {
		return counts, tx.Commit(ctx)
	}
	return counts, nil
}
