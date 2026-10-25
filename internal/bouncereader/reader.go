package bouncereader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"
	"github.com/skylab-kulubu/skymail-backend/internal/bounce"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/msauth"
)

// PollInterval is how often the reader reads the mailboxes (hub spec §2:
// a webhook would need a public endpoint; five minutes is soon enough).
const PollInterval = 5 * time.Minute

// maxPages bounds one poll of one mailbox: a first poll reads the whole
// Inbox, which the mailbox's retention tag keeps to 30 days.
const maxPages = 400

// Counts is what the reports one poll read came to, by bounce.Outcome, and
// how many could not be read (bounce.ErrUnreadable, bounce.ErrNoID, larger
// than MaxMIME).
type Counts struct {
	Reports        int64
	NotDSN         int64
	Unmatched      int64
	Hard           int64
	Soft           int64
	Delay          int64
	OtherRecipient int64
	Duplicate      int64
	NoFailure      int64
	Unreadable     int64
}

func (c *Counts) add(outcome bounce.Outcome) {
	switch outcome {
	case bounce.OutcomeNotDSN:
		c.NotDSN++
	case bounce.OutcomeUnmatched:
		c.Unmatched++
	case bounce.OutcomeHard:
		c.Hard++
	case bounce.OutcomeSoft:
		c.Soft++
	case bounce.OutcomeDelay:
		c.Delay++
	case bounce.OutcomeOtherRecipient:
		c.OtherRecipient++
	case bounce.OutcomeDuplicate:
		c.Duplicate++
	case bounce.OutcomeNoFailure:
		c.NoFailure++
	}
}

// Reader polls the sender mailboxes. Make it with New.
type Reader struct {
	store  *database.Store
	fqdn   string
	config Config
	graph  *graphClient
	every  time.Duration
	now    func() time.Time
	logger zerolog.Logger
}

// Option changes a Reader (tests).
type Option func(*Reader)

// WithGraph reads Graph at base with tokens and client instead of
// DefaultGraphURL and the credentials' token.
func WithGraph(base string, tokens tokenSource, client *http.Client) Option {
	return func(r *Reader) {
		r.graph.base = strings.TrimRight(base, "/")
		r.graph.tokens = tokens
		if client != nil {
			r.graph.http = client
		}
	}
}

// WithLogger writes to logger.
func WithLogger(logger zerolog.Logger) Option { return func(r *Reader) { r.logger = logger } }

// WithInterval polls every interval instead of PollInterval.
func WithInterval(interval time.Duration) Option { return func(r *Reader) { r.every = interval } }

// New is the reader of config's mailboxes; fqdn is SMTP_FQDN, the domain of
// SkyMail's Message-IDs.
func New(store *database.Store, fqdn string, config Config, opts ...Option) *Reader {
	r := &Reader{
		store:  store,
		fqdn:   fqdn,
		config: config,
		graph:  &graphClient{base: DefaultGraphURL, http: &http.Client{}},
		every:  PollInterval,
		now:    time.Now,
		logger: zerolog.Nop(),
	}
	if config.Credentials != nil {
		r.graph.tokens = msauth.NewTokenSource(config.Credentials, msauth.ScopeGraph)
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Run polls at once and then every interval until ctx ends. Off, it returns
// at once.
func (r *Reader) Run(ctx context.Context) {
	if r.config.Mode != ModeDryRun && r.config.Mode != ModeOn {
		return
	}
	ticker := time.NewTicker(r.every)
	defer ticker.Stop()
	for {
		if err := r.Poll(ctx); err != nil && ctx.Err() == nil {
			r.logger.Warn().Err(err).Msg("bounce reader: the poll failed; the next one tries again")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Poll reads every mailbox once, holding the reader's advisory lock; another
// process holding it skips this poll. A mailbox that fails is recorded with
// its error and the others are still read.
func (r *Reader) Poll(ctx context.Context) error {
	ran, err := r.store.WithBounceReaderLock(ctx, func(ctx context.Context) error {
		var failed []string
		for _, mailbox := range r.config.Mailboxes {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := r.pollMailbox(ctx, mailbox); err != nil {
				failed = append(failed, mailbox)
			}
		}
		if len(failed) > 0 {
			return fmt.Errorf("bounce reader: %d of %d mailboxes failed", len(failed), len(r.config.Mailboxes))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !ran {
		r.logger.Debug().Msg("bounce reader: another process is polling")
	}
	return nil
}

// pollMailbox reads what is new in a mailbox's Inbox since its delta link
// and records the poll: the new delta link only when it read to the end, so
// a poll that failed half way reads those items again (a report recorded
// before is a duplicate).
func (r *Reader) pollMailbox(ctx context.Context, mailbox string) error {
	mode := string(r.config.Mode)
	link, err := r.store.GetBounceReaderDeltaLink(ctx, database.GetBounceReaderDeltaLinkParams{Mailbox: mailbox, Mode: mode})
	if errors.Is(err, pgx.ErrNoRows) {
		link, err = nil, nil
	}
	if err != nil {
		return r.failed(ctx, mailbox, Counts{}, fmt.Errorf("database: %w", err), "database")
	}
	first := r.graph.firstDelta(mailbox)
	next := first
	if link != nil && *link != "" {
		next = *link
	}

	var counts Counts
	var delta string
	for pages := 0; delta == ""; pages++ {
		if pages == maxPages {
			return r.failed(ctx, mailbox, counts, errors.New("bounce reader: too many delta pages in one poll"), "too many pages")
		}
		page, err := r.graph.delta(ctx, next)
		var refused *graphError
		if errors.As(err, &refused) && refused.status == http.StatusGone && next != first {
			// The delta link is too old (syncStateNotFound): Graph asks for a
			// sync from the start. The Inbox is read again; what was recorded
			// before is a duplicate.
			r.logger.Info().Str("mailbox", mailbox).Msg("bounce reader: Graph no longer knows the delta link; reading the Inbox from the start")
			next = first
			continue
		}
		if err != nil {
			return r.failed(ctx, mailbox, counts, err, summary(err))
		}
		for _, item := range page.Value {
			if item.Removed != nil || item.ID == "" {
				continue
			}
			if err := r.read(ctx, mailbox, item, &counts); err != nil {
				return r.failed(ctx, mailbox, counts, err, summary(err))
			}
		}
		switch {
		case page.Delta != "":
			delta = page.Delta
		case page.Next != "":
			next = page.Next
		default:
			return r.failed(ctx, mailbox, counts, errors.New("graph: a delta page without a next or a delta link"), "graph: no next link")
		}
	}

	if err := r.store.RecordBounceReaderPoll(ctx, pollParams(mailbox, mode, &delta, r.now(), nil, counts)); err != nil {
		r.logger.Warn().Err(err).Str("mailbox", mailbox).Msg("bounce reader: the poll could not be recorded; the next one reads its items again")
		return err
	}
	if counts.Reports > 0 {
		r.logger.Info().Str("mailbox", mailbox).Str("mode", mode).
			Int64("reports", counts.Reports).Int64("dsn_degil", counts.NotDSN).Int64("eslesmedi", counts.Unmatched).
			Int64("kalici", counts.Hard).Int64("yumusak", counts.Soft).Int64("gecikme", counts.Delay).
			Int64("alici_farkli", counts.OtherRecipient).Int64("tekrar", counts.Duplicate).
			Int64("diger", counts.NoFailure).Int64("okunamadi", counts.Unreadable).
			Msg("bounce reader: read the mailbox")
	}
	return nil
}

// read fetches one item and gives it to bounce. An item that cannot be read
// is counted and passed over; Graph's or the database's failure stops the
// poll.
func (r *Reader) read(ctx context.Context, mailbox string, item message, counts *Counts) error {
	raw, err := r.graph.mime(ctx, mailbox, item.ID)
	switch {
	case errors.Is(err, errGone):
		return nil
	case errors.Is(err, errTooLarge):
		counts.Reports++
		counts.Unreadable++
		return nil
	case err != nil:
		return err
	}
	counts.Reports++
	ndr := bounce.NDR{ID: item.InternetMessageID, ReceivedAt: item.ReceivedDateTime, MIME: bytes.NewReader(raw)}
	var outcome bounce.Outcome
	if r.config.Mode == ModeOn {
		outcome, err = bounce.Process(ctx, r.store, r.fqdn, ndr)
	} else {
		outcome, err = bounce.Evaluate(ctx, r.store, r.fqdn, ndr)
	}
	switch {
	case errors.Is(err, bounce.ErrUnreadable), errors.Is(err, bounce.ErrNoID):
		counts.Unreadable++
		return nil
	case err != nil:
		return fmt.Errorf("database: %w", err)
	}
	counts.add(outcome)
	return nil
}

// failed records a poll that stopped (the delta link stays) and logs it.
func (r *Reader) failed(ctx context.Context, mailbox string, counts Counts, err error, summary string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	r.logger.Warn().Err(err).Str("mailbox", mailbox).Str("mode", string(r.config.Mode)).
		Msg("bounce reader: the mailbox could not be read; the next poll tries again from where it left off")
	if recordErr := r.store.RecordBounceReaderPoll(ctx, pollParams(mailbox, string(r.config.Mode), nil, r.now(), &summary, counts)); recordErr != nil {
		r.logger.Warn().Err(recordErr).Str("mailbox", mailbox).Msg("bounce reader: the failed poll could not be recorded")
	}
	return err
}

// maxSummary bounds a poll's error in the state row.
const maxSummary = 200

// summary is what a poll's error leaves in the state row: Graph's status and
// code, the kind of failure, or this package's own words; never a report's
// text (bounce's errors do not quote it) nor a token endpoint's description
// (msauth's do not either).
func summary(err error) string {
	var refused *graphError
	text := err.Error()
	switch {
	case errors.As(err, &refused):
		text = refused.Error()
	case strings.HasPrefix(text, "database: "):
		text = "database"
	}
	if len(text) > maxSummary {
		text = text[:maxSummary]
	}
	return strings.ToValidUTF8(text, "")
}

func pollParams(mailbox, mode string, delta *string, at time.Time, lastError *string, c Counts) database.RecordBounceReaderPollParams {
	return database.RecordBounceReaderPollParams{
		Mailbox: mailbox, Mode: mode, DeltaLink: delta, PolledAt: at, LastError: lastError,
		Reports: c.Reports, NotDsn: c.NotDSN, Unmatched: c.Unmatched, Hard: c.Hard, Soft: c.Soft, Delay: c.Delay,
		OtherRecipient: c.OtherRecipient, Duplicate: c.Duplicate, NoFailure: c.NoFailure, Unreadable: c.Unreadable,
	}
}
