package mailer

import (
	"context"
	"fmt"
	netmail "net/mail"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/msauth"
)

// Sender lanes (hub ticket skymail-bounce-isleme 10; Yusuf, 2026-10-20): a
// mail goes out from one of three shared mailboxes by what it is, each with
// its own display name, all with Reply-To the club's human address:
//
//   - hesap: account mail, what a person needs to reach their account (a
//     login or sign-up code, an address verification, a password reset): a
//     System template whose key is keycloak.* or account.* (AccountMail, the
//     same rule the suppression list exempts by).
//   - duyuru: the broadcast stream (ADR 0072 decision 2): a send whose
//     purpose is duyuru — a list, a Keycloak group, and by default a Forms
//     form's respondents, an approved send and a single mail an operator
//     asks tracking for. An operator's mail to an audience.
//   - bilgi: the rest of the transactional stream (purpose islem): Forms
//     confirmations, Mail onayı notifications, event and other system
//     notifications, and an operator's mail they declared transactional.
//
// The purpose (mail_tasks.purpose; Purpose*) picks the stream only while
// SKYMAIL_MAIL_PURPOSE is on; off, and for a send without one (queued before
// it, or while it was off), the queue priority does as before: the bulk lane
// is duyuru, the system lane bilgi. Account mail is hesap whatever its
// stream. Each lane has its own switch: off (the
// default) sends it the way SkyMail always has, SMTP AUTH as SMTP_USER from
// SMTP_FROM; graph sends it through Microsoft Graph sendMail without a copy in
// Sent Items; xoauth2 sends it over SMTP with an OAuth token. Both use the
// skymail-posta application's certificate (internal/msauth). A duyuru mail
// with a List-Unsubscribe link goes either way (list_unsubscribe.go): Graph
// sends it as MIME, which keeps the header and a copy in Sent Items.

// Lane is which sender mailbox a mail goes out from.
type Lane string

const (
	LaneHesap  Lane = "hesap"
	LaneBilgi  Lane = "bilgi"
	LaneDuyuru Lane = "duyuru"
)

// Lanes in the order they are configured and logged.
var Lanes = []Lane{LaneHesap, LaneBilgi, LaneDuyuru}

// Transport is how a lane's mail is sent.
type Transport string

const (
	// TransportOff sends the lane's mail as SkyMail always has: SMTP AUTH
	// (LOGIN, or PLAIN with SMTP_PLAIN) as SMTP_USER, from SMTP_FROM.
	TransportOff Transport = "off"
	// TransportGraph sends it with Microsoft Graph sendMail from the lane's
	// mailbox, saveToSentItems false.
	TransportGraph Transport = "graph"
	// TransportXOAUTH2 sends it over SMTP (SMTP_HOST:SMTP_PORT, STARTTLS)
	// with AUTH XOAUTH2 as the lane's mailbox.
	TransportXOAUTH2 Transport = "xoauth2"
)

// LaneConfig is one lane's switch and its From.
type LaneConfig struct {
	Transport Transport
	// From is the lane's display name and mailbox (SKYMAIL_FROM_<LANE>);
	// set when Transport is not off.
	From *netmail.Address
}

// SendLanes is how the e-mail channel picks its sender (SendLanesFromEnv).
// The zero value is SkyMail as it always was: every lane off, no Reply-To.
type SendLanes struct {
	Lanes map[Lane]LaneConfig
	// ReplyTo is SMTP_REPLY_TO, the Reply-To of every mail, every lane and
	// the off path too; nil, none.
	ReplyTo *netmail.Address
	// Credentials are the skymail-posta application's (GRAPH_*), read when
	// a lane is not off.
	Credentials *msauth.Credentials
	// ListUnsubscribe is SKYMAIL_LIST_UNSUBSCRIBE: the duyuru lane's mail
	// whose row has an unsubscribe link carries List-Unsubscribe and
	// List-Unsubscribe-Post (list_unsubscribe.go). Off by default.
	ListUnsubscribe bool
	// Purpose is SKYMAIL_MAIL_PURPOSE: a send's purpose, not its queue
	// priority, picks its stream (LaneOf), a send may name its purpose and
	// category, and a duyuru mail whose row has an unsubscribe link carries a
	// visible line with it when its body has none (footer.go). Off by default:
	// purposes are neither written nor read, and every mail goes as before.
	Purpose bool
}

// A send's purpose (mail_tasks.purpose; ADR 0072 decision 2): which message
// stream its mail is in.
const (
	// PurposeIslem is the transactional stream (hesap@, bilgi@): no
	// preference, no unsubscribe record, no List-Unsubscribe, not tracked.
	PurposeIslem = "islem"
	// PurposeDuyuru is the broadcast stream (duyuru@): a category, the
	// preference, the unsubscribe record, the one-click header and a visible
	// link.
	PurposeDuyuru = "duyuru"
)

// SendPurpose is what a send says about its stream, as mail_tasks and
// mail_approvals keep it: the purpose, core's category of a duyuru send, an
// operator's statement that an islem send is about the recipient's own
// application, and a duyuru sender's statement that it carries no
// commercial content. The zero value is a send without a purpose, read by
// the rule of before.
type SendPurpose struct {
	Purpose                 *string
	Category                *string
	TransactionalStatement  *bool
	CommercialContentAbsent *bool
}

// PurposeEnv switches purposes on: off (the default, also when unset) or on.
const PurposeEnv = "SKYMAIL_MAIL_PURPOSE"

// purposeFromEnv reads SKYMAIL_MAIL_PURPOSE; anything but off, on or nothing
// stops startup.
func purposeFromEnv(getenv func(string) string) (bool, error) {
	switch raw := strings.ToLower(strings.TrimSpace(getenv(PurposeEnv))); raw {
	case "", "off":
		return false, nil
	case "on":
		return true, nil
	default:
		return false, fmt.Errorf("%s must be off or on, not %q", PurposeEnv, raw)
	}
}

// purposeOf is the purpose a mail's stream is read from: the send's while
// SKYMAIL_MAIL_PURPOSE is on, none (the rule of before) while it is off, so
// turning it off sends a mail queued while it was on as before too.
func (l SendLanes) purposeOf(purpose *string) *string {
	if !l.Purpose {
		return nil
	}
	return purpose
}

// Lane is lane's configuration: off when it has none.
func (l SendLanes) Lane(lane Lane) LaneConfig {
	if c, ok := l.Lanes[lane]; ok && c.Transport != "" {
		return c
	}
	return LaneConfig{Transport: TransportOff}
}

// uses says a lane is sent with transport.
func (l SendLanes) uses(transport Transport) bool {
	for _, lane := range Lanes {
		if l.Lane(lane).Transport == transport {
			return true
		}
	}
	return false
}

// anyOn says a lane is not off: only then is a mail's lane looked up.
func (l SendLanes) anyOn() bool {
	for _, lane := range Lanes {
		if l.Lane(lane).Transport != TransportOff {
			return true
		}
	}
	return false
}

// Environment names: SKYMAIL_SEND_HESAP, SKYMAIL_FROM_HESAP, and so on.
func sendEnv(lane Lane) string { return "SKYMAIL_SEND_" + strings.ToUpper(string(lane)) }
func fromEnv(lane Lane) string { return "SKYMAIL_FROM_" + strings.ToUpper(string(lane)) }

// ReplyToEnv is the Reply-To of every mail.
const ReplyToEnv = "SMTP_REPLY_TO"

// SendLanesFromEnv reads the sender lanes. Each SKYMAIL_SEND_<LANE> is off
// (also when unset), graph or xoauth2; a lane that is not off needs
// SKYMAIL_FROM_<LANE>, a display name and address ("SKY LAB Hesap
// <hesap@bildirim.yildizskylab.com>"), and the application's credentials
// (GRAPH_TENANT_ID, GRAPH_CLIENT_ID, GRAPH_CLIENT_KEY, GRAPH_CLIENT_CERT).
// SMTP_REPLY_TO, optional, is an address with or without a display name.
// SKYMAIL_LIST_UNSUBSCRIBE and SKYMAIL_MAIL_PURPOSE are off (also when unset)
// or on. Anything else stops startup, so a typo cannot quietly send from the
// wrong mailbox.
func SendLanesFromEnv(getenv func(string) string) (SendLanes, error) {
	lanes := SendLanes{Lanes: make(map[Lane]LaneConfig, len(Lanes))}
	listUnsubscribe, err := listUnsubscribeFromEnv(getenv)
	if err != nil {
		return SendLanes{}, err
	}
	lanes.ListUnsubscribe = listUnsubscribe
	if lanes.Purpose, err = purposeFromEnv(getenv); err != nil {
		return SendLanes{}, err
	}
	for _, lane := range Lanes {
		raw := strings.ToLower(strings.TrimSpace(getenv(sendEnv(lane))))
		transport := Transport(raw)
		switch transport {
		case "", TransportOff:
			lanes.Lanes[lane] = LaneConfig{Transport: TransportOff}
			continue
		case TransportGraph, TransportXOAUTH2:
		default:
			return SendLanes{}, fmt.Errorf("%s must be off, graph or xoauth2, not %q", sendEnv(lane), raw)
		}
		from := strings.TrimSpace(getenv(fromEnv(lane)))
		if from == "" {
			return SendLanes{}, fmt.Errorf("%s=%s needs %s, the lane's display name and mailbox", sendEnv(lane), transport, fromEnv(lane))
		}
		address, err := netmail.ParseAddress(from)
		if err != nil {
			return SendLanes{}, fmt.Errorf("%s is not an address (Name <mailbox@domain>): %w", fromEnv(lane), err)
		}
		lanes.Lanes[lane] = LaneConfig{Transport: transport, From: address}
	}
	if raw := strings.TrimSpace(getenv(ReplyToEnv)); raw != "" {
		address, err := netmail.ParseAddress(raw)
		if err != nil {
			return SendLanes{}, fmt.Errorf("%s is not an address: %w", ReplyToEnv, err)
		}
		lanes.ReplyTo = address
	}
	if lanes.anyOn() {
		creds, err := msauth.CredentialsFromEnv(getenv)
		if err != nil {
			return SendLanes{}, fmt.Errorf("a sender lane is on: %w", err)
		}
		lanes.Credentials = creds
	}
	return lanes, nil
}

// LaneOf is the sender lane of a mail: hesap for account mail (a System
// template keyed keycloak.* or account.*), whatever its stream; otherwise its
// purpose's (ADR 0072 decision 2): duyuru for duyuru, bilgi for islem. A send
// without a purpose (nil: queued before purposes, or while
// SKYMAIL_MAIL_PURPOSE was off, or read while it is off) is read by the rule
// of before: duyuru for the bulk lane, bilgi for the system lane. key and
// system are the send's template's (nil and false without one); priority is
// the queue row's. The queue priority is the send budget's, not the
// stream's: an operator's islem send to a Forms source waits in the bulk lane
// and goes out from bilgi@.
func LaneOf(purpose, key *string, system bool, priority Priority) Lane {
	switch {
	case AccountMail(key, system):
		return LaneHesap
	case purpose != nil && *purpose == PurposeDuyuru:
		return LaneDuyuru
	case purpose != nil && *purpose == PurposeIslem:
		return LaneBilgi
	case priority <= PriorityBulk:
		return LaneDuyuru
	default:
		return LaneBilgi
	}
}

// sendLane is what a mail's lane is read from: the send's template's key,
// whether it is a System template, and the send's purpose.
type sendLane struct {
	Key     *string
	System  bool
	Purpose *string
}

// templateKeyOf reads a send's sendLane; the mailer's reads mail_tasks and
// templates.
type templateKeyOf func(ctx context.Context, taskID uuid.UUID) (sendLane, error)

// databaseTemplateKey is templateKeyOf from the database; without one (a
// test's SendOnce), no template and no purpose.
func (m *mailerImpl) databaseTemplateKey(ctx context.Context, taskID uuid.UUID) (sendLane, error) {
	if m.db == nil {
		return sendLane{}, nil
	}
	row, err := m.db.GetMailTaskTemplateKey(ctx, taskID)
	if err != nil {
		return sendLane{}, err
	}
	return sendLane{Key: row.Key, System: row.System, Purpose: row.Purpose}, nil
}

// WithSendLanes sends each mail from its lane's mailbox the way the lane
// says (SendLanesFromEnv). Without it every lane is off.
func WithSendLanes(lanes SendLanes) Option {
	return func(m *mailerImpl) {
		m.lanes = lanes
		if lanes.Credentials == nil {
			return
		}
		if lanes.uses(TransportGraph) {
			m.graphTokens = msauth.NewTokenSource(lanes.Credentials, msauth.ScopeGraph)
		}
		if lanes.uses(TransportXOAUTH2) {
			m.exchangeTokens = msauth.NewTokenSource(lanes.Credentials, msauth.ScopeExchange)
		}
	}
}

// tokenSource is a scope's access token (msauth.TokenSource).
type tokenSource interface {
	Token(ctx context.Context) (string, error)
	Invalidate(token string)
}
