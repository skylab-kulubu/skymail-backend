// Package bouncereader reads the delivery status notifications (NDRs) that
// come back to SkyMail's sender mailboxes and hands each one to
// internal/bounce (hub ticket skymail-bounce-isleme 04; e-skylab ADR 0073;
// README, "Geri dönen posta ve bastırma listesi").
//
// Every PollInterval it reads each mailbox's Inbox through Microsoft Graph's
// delta query (only what is new since the last poll), fetches a new item's
// MIME ($value), and gives it to bounce.Process (on) or bounce.Evaluate
// (dry-run) in memory: the message is never stored. The application's token
// is the skymail-posta certificate's (internal/msauth); its right to read the
// mailboxes comes from Exchange RBAC for Applications (Application Mail.Read,
// scoped to the sender mailboxes), so it can read nothing else and change
// nothing. One process polls at a time (a Postgres advisory lock).
//
// Nothing logged or stored here holds a person's address or a report's text:
// the mailboxes are the club's own, the counts are counts, an error is an
// HTTP status and Graph's error code.
package bouncereader

import (
	"fmt"
	"net/mail"
	"strings"

	"github.com/skylab-kulubu/skymail-backend/internal/msauth"
)

// Mode is SKYMAIL_BOUNCE_READER.
type Mode string

const (
	// ModeOff (or unset): nothing reads the mailboxes; nothing else is read
	// from the environment.
	ModeOff Mode = "off"
	// ModeDryRun reads and counts what each report would do, and writes
	// nothing but its own place in the mailbox and the counts.
	ModeDryRun Mode = "dry-run"
	// ModeOn records the reports and suppresses an address a permanent
	// failure was reported for.
	ModeOn Mode = "on"
)

// The environment the reader is configured from.
const (
	EnvMode      = "SKYMAIL_BOUNCE_READER"
	EnvMailboxes = "SKYMAIL_BOUNCE_MAILBOXES"
)

// maxMailboxes bounds SKYMAIL_BOUNCE_MAILBOXES: the club has three sender
// mailboxes.
const maxMailboxes = 10

// Config is how the reader runs: its mode, the mailboxes it reads (lower-case
// addresses) and the application's credentials.
type Config struct {
	Mode        Mode
	Mailboxes   []string
	Credentials *msauth.Credentials
}

// ConfigFromEnv reads SKYMAIL_BOUNCE_READER: off (or unset), dry-run or on;
// any other value is an error that stops startup. Unless off, it needs
// SKYMAIL_BOUNCE_MAILBOXES (the sender mailboxes, comma-separated bare
// addresses: hesap@, bilgi@ and the duyuru mailbox) and the application's
// GRAPH_* credentials, the sender lanes' own.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	raw := strings.TrimSpace(getenv(EnvMode))
	switch Mode(raw) {
	case "", ModeOff:
		return Config{Mode: ModeOff}, nil
	case ModeDryRun, ModeOn:
	default:
		return Config{}, fmt.Errorf("%s must be off, dry-run or on, not %q", EnvMode, raw)
	}
	mailboxes, err := parseMailboxes(getenv(EnvMailboxes))
	if err != nil {
		return Config{}, err
	}
	creds, err := msauth.CredentialsFromEnv(getenv)
	if err != nil {
		return Config{}, fmt.Errorf("%s=%s: %w", EnvMode, raw, err)
	}
	return Config{Mode: Mode(raw), Mailboxes: mailboxes, Credentials: creds}, nil
}

// parseMailboxes reads SKYMAIL_BOUNCE_MAILBOXES: bare addresses, no display
// names, each once.
func parseMailboxes(value string) ([]string, error) {
	var mailboxes []string
	seen := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		address, err := mail.ParseAddress(part)
		if err != nil || address.Name != "" || address.Address != part {
			return nil, fmt.Errorf("%s must be bare addresses separated by commas (hesap@bildirim.yildizskylab.com,…), not %q", EnvMailboxes, part)
		}
		lower := strings.ToLower(address.Address)
		if seen[lower] {
			continue
		}
		seen[lower] = true
		mailboxes = append(mailboxes, lower)
	}
	if len(mailboxes) == 0 {
		return nil, fmt.Errorf("%s is required unless %s is off", EnvMailboxes, EnvMode)
	}
	if len(mailboxes) > maxMailboxes {
		return nil, fmt.Errorf("%s names %d mailboxes, at most %d", EnvMailboxes, len(mailboxes), maxMailboxes)
	}
	return mailboxes, nil
}
