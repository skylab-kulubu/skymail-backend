package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/skymail-backend/internal/config"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

// queueCloseRestoredCommandName runs one step of a restore from backup
// instead of the server:
//
//	skymail-backend queue-close-restored --before <RFC3339> [--apply]
//
// A dump taken before an Account erasure brings back the mail still queued
// when it was taken, mail to people erased since among it, and core's replay
// (account erasure spec §8) cannot find that mail: core keeps no addresses.
// So the queue the dump held is closed without sending. Every pending or
// processing row queued before --before becomes failed with
// restoreNotSentError; rows queued since are left to go out. Without --apply
// it only counts. It reads the environment the server reads, so it runs in
// SkyMail's own container through docker exec (docs/data-lifecycle.md, Backup
// and restore).
const queueCloseRestoredCommandName = "queue-close-restored"

// restoreNotSentError is the error a closed row keeps. A send's page shows it
// beside each recipient the restore closed.
const restoreNotSentError = "restore: gönderilmedi"

const queueCloseRestoredExample = "2026-09-26T09:00:00Z"

// runQueueCloseRestoredFromEnv is the command as the binary runs it.
func runQueueCloseRestoredFromEnv(args []string, out io.Writer) int {
	if err := config.LoadEnv(); err != nil {
		fmt.Fprintf(out, "%s: reading the environment: %v\n", queueCloseRestoredCommandName, err)
		return 1
	}
	return runQueueCloseRestored(args, config.Value, out)
}

// runQueueCloseRestored reads --before and --apply, checks the sender and
// does the work on DATABASE_URL, the server's own database login (ADR-0050's
// alternating logins take the owner role on their own). It prints counts and
// times only, never an address, a name or a mail. It returns the exit code:
// 0 done, 1 refused or failed, 2 a usage error.
func runQueueCloseRestored(args []string, getenv func(string) string, out io.Writer) int {
	flags := flag.NewFlagSet(queueCloseRestoredCommandName, flag.ContinueOnError)
	flags.SetOutput(out)
	before := flags.String("before", "", "the restore instant, RFC3339 (for example "+queueCloseRestoredExample+"): rows queued before it are closed")
	apply := flags.Bool("apply", false, "close the rows; without it the command only counts them. Needs MAIL_SENDER=paused")
	flags.Usage = func() {
		fmt.Fprintf(out, "usage: skymail-backend %s --before <RFC3339> [--apply]\n", queueCloseRestoredCommandName)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(out, "%s: unexpected argument %q\n", queueCloseRestoredCommandName, flags.Arg(0))
		return 2
	}
	cutoff, err := parseRestoreInstant(*before, time.Now())
	if err != nil {
		fmt.Fprintf(out, "%s: %v\n", queueCloseRestoredCommandName, err)
		return 2
	}

	// The sender must not be running beside the change: a worker would send
	// a row this is closing, or write its own outcome over it. The command
	// runs in the service's container, so it sees the service's MAIL_SENDER.
	paused, sender := senderState(getenv)
	if *apply && !paused {
		fmt.Fprintf(out, "%s: --apply refused: %s. Set MAIL_SENDER=paused on SkyMail and deploy first; a dry run works either way.\n",
			queueCloseRestoredCommandName, sender)
		return 1
	}

	databaseURL := strings.TrimSpace(getenv("DATABASE_URL"))
	if databaseURL == "" {
		fmt.Fprintf(out, "%s needs DATABASE_URL\n", queueCloseRestoredCommandName)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(out, "%s: database: cannot open the connection pool\n", queueCloseRestoredCommandName)
		return 1
	}
	defer pool.Close()
	store := database.NewStore(pool)

	if !*apply {
		counted, err := store.CountRestoredQueueItems(ctx, cutoff)
		if err != nil {
			fmt.Fprintf(out, "%s: database: %v\n", queueCloseRestoredCommandName, err)
			return 1
		}
		fmt.Fprintf(out, "%s: dry run, nothing changed. Add --apply to close these rows.\n", queueCloseRestoredCommandName)
		printRestoredQueue(out, cutoff, restoredQueue{
			pending:    counted.Pending,
			processing: counted.Processing,
			tasks:      counted.Tasks,
			oldest:     counted.Oldest,
			newest:     counted.Newest,
			undated:    counted.Undated,
			left:       counted.LeftQueued,
		})
		if !paused {
			fmt.Fprintf(out, "note: %s; --apply refuses until MAIL_SENDER=paused.\n", sender)
		}
		return 0
	}

	var closed database.CloseRestoredQueueItemsRow
	var left database.CountRestoredQueueItemsRow
	err = store.InTx(ctx, func(q *database.Queries) error {
		var err error
		closed, err = q.CloseRestoredQueueItems(ctx, database.CloseRestoredQueueItemsParams{
			Before: cutoff,
			Error:  restoreNotSentError,
		})
		if err != nil {
			return err
		}
		left, err = q.CountRestoredQueueItems(ctx, cutoff)
		return err
	})
	if err != nil {
		fmt.Fprintf(out, "%s: database, nothing changed: %v\n", queueCloseRestoredCommandName, err)
		return 1
	}
	fmt.Fprintf(out, "%s: closed without sending, error %q.\n", queueCloseRestoredCommandName, restoreNotSentError)
	printRestoredQueue(out, cutoff, restoredQueue{
		pending:    closed.Pending,
		processing: closed.Processing,
		tasks:      closed.Tasks,
		oldest:     closed.Oldest,
		newest:     closed.Newest,
		undated:    closed.Undated,
		left:       left.LeftQueued,
	})
	return 0
}

// parseRestoreInstant reads --before: required, RFC3339 with its offset, and
// not later than now. A cutoff in the future would close mail queued after
// the restore too, which is left to go out.
func parseRestoreInstant(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("--before is required: the restore instant, RFC3339 (for example %s)", queueCloseRestoredExample)
	}
	cutoff, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("--before %q is not RFC3339 with an offset (for example %s)", raw, queueCloseRestoredExample)
	}
	if cutoff.After(now) {
		return time.Time{}, fmt.Errorf("--before %s is in the future: it is the instant the restore began", cutoff.UTC().Format(time.RFC3339))
	}
	return cutoff, nil
}

// senderState says whether MAIL_SENDER, read as the server reads it, pauses
// the sender, and how it reads, for a message.
func senderState(getenv func(string) string) (bool, string) {
	raw := strings.TrimSpace(getenv("MAIL_SENDER"))
	sender, err := mailer.SenderFromEnv(getenv)
	switch {
	case err != nil:
		return false, fmt.Sprintf("MAIL_SENDER is %q, neither on nor paused", raw)
	case sender == mailer.SenderPaused:
		return true, "MAIL_SENDER is paused"
	case raw == "":
		return false, "MAIL_SENDER is not set, so the sender is on"
	default:
		return false, `MAIL_SENDER is "on"`
	}
}

// restoredQueue is what a dry run found or an apply closed: the rows by the
// status they had, the sends they belong to, when the oldest and newest were
// queued, and how many of the queue's rows were queued since and left alone.
type restoredQueue struct {
	pending, processing, tasks, undated, left int64
	oldest, newest                            *time.Time
}

// printRestoredQueue writes the same lines for a dry run and an apply.
func printRestoredQueue(out io.Writer, cutoff time.Time, rows restoredQueue) {
	fmt.Fprintf(out, "before: %s\n", cutoff.UTC().Format(time.RFC3339))
	fmt.Fprintf(out, "pending: %d\n", rows.pending)
	fmt.Fprintf(out, "processing: %d\n", rows.processing)
	fmt.Fprintf(out, "tasks: %d\n", rows.tasks)
	fmt.Fprintf(out, "oldest created_at: %s\n", formatQueuedAt(rows.oldest))
	fmt.Fprintf(out, "newest created_at: %s\n", formatQueuedAt(rows.newest))
	if rows.undated > 0 {
		fmt.Fprintf(out, "without created_at, counted in: %d\n", rows.undated)
	}
	fmt.Fprintf(out, "queued at or after --before, left alone: %d\n", rows.left)
}

func formatQueuedAt(at *time.Time) string {
	if at == nil {
		return "-"
	}
	return at.UTC().Format(time.RFC3339)
}
