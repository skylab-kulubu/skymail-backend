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
//   - duyuru: the bulk lane (PriorityBulk): a send to a list, a Keycloak
//     group, a Forms form's respondents, an approved send, a bulk
//     notification type, a single mail an operator asks tracking for. An
//     operator's mail to an audience.
//   - bilgi: everything else, the system lane: Forms confirmations, Mail
//     onayı notifications, event and other system notifications.
//
// Account mail is hesap in either lane. Each lane has its own switch: off (the
// default) sends it the way SkyMail always has, SMTP AUTH as SMTP_USER from
// SMTP_FROM; graph sends it through Microsoft Graph sendMail without a copy in
// Sent Items (the decided path for hesap and bilgi); xoauth2 sends it over
// SMTP with an OAuth token (the decided path for duyuru, whose
// List-Unsubscribe header Graph cannot carry). Both use the skymail-posta
// application's certificate (internal/msauth).

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
// Anything else stops startup, so a typo cannot quietly send from the wrong
// mailbox.
func SendLanesFromEnv(getenv func(string) string) (SendLanes, error) {
	lanes := SendLanes{Lanes: make(map[Lane]LaneConfig, len(Lanes))}
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
// template keyed keycloak.* or account.*), whatever its lane; otherwise
// duyuru for the bulk lane and bilgi for the system lane. key and system are
// the send's template's (nil and false without one); priority is the queue
// row's.
func LaneOf(key *string, system bool, priority Priority) Lane {
	switch {
	case AccountMail(key, system):
		return LaneHesap
	case priority <= PriorityBulk:
		return LaneDuyuru
	default:
		return LaneBilgi
	}
}

// templateKeyOf reads a send's template's key and whether it is a System
// template; the mailer's reads mail_tasks and templates.
type templateKeyOf func(ctx context.Context, taskID uuid.UUID) (key *string, system bool, err error)

// databaseTemplateKey is templateKeyOf from the database; without one (a
// test's SendOnce), no template.
func (m *mailerImpl) databaseTemplateKey(ctx context.Context, taskID uuid.UUID) (*string, bool, error) {
	if m.db == nil {
		return nil, false, nil
	}
	row, err := m.db.GetMailTaskTemplateKey(ctx, taskID)
	if err != nil {
		return nil, false, err
	}
	return row.Key, row.System, nil
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
