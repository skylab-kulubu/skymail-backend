package tracking

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Why a row of a send that asked for tracking is not tracked
// (mail_queue.tracking_skip_reason).
const (
	// SkipFlagOff: the row was queued tracked, and SKYMAIL_TRACKING was off
	// when it went out; it went out clean and its token is closed.
	SkipFlagOff = "flag_off"
	// SkipNoAccount: SkyMail knows only the address (a mailing list's
	// recipient, a person named by address): no Keycloak account to ask
	// about.
	SkipNoAccount = "no_account"
	// The reasons core gives (ticket 16).
	SkipPreferenceOff = "preference_off"
	SkipNotMember     = "not_member"
	SkipInactive      = "inactive"
	// SkipPreferenceUnavailable: core did not answer in time, or not as the
	// contract says, so no one in the send is tracked.
	SkipPreferenceUnavailable = "preference_unavailable"
	// SkipNotAllowed: core said no with a reason SkyMail does not know.
	SkipNotAllowed = "not_allowed"
)

// SkipReasonOf is the skip reason of core's reason for not allowing someone.
func SkipReasonOf(coreReason string) string {
	switch coreReason {
	case SkipPreferenceOff, SkipNotMember, SkipInactive:
		return coreReason
	default:
		return SkipNotAllowed
	}
}

// Decision is whether core lets a person be tracked, and if not why.
type Decision struct {
	Allowed bool
	Reason  string
}

// Preferences says who may be tracked. A person is in the answer exactly
// when they were asked about; any error means no answer for anyone.
type Preferences interface {
	Resolve(ctx context.Context, subs []uuid.UUID) (map[uuid.UUID]Decision, error)
}

// Accounts finds the Keycloak account of a recipient SkyMail knows only by
// address (a person a send names one by one; Yusuf, 2026-10-20): the one
// enabled account whose primary e-mail is the address, compared whole and
// case-insensitively, and verified. No such account, or more than one, is
// nil: an address is never guessed. Secondary attributes (a school address)
// are not read.
type Accounts interface {
	VerifiedAccountByEmail(ctx context.Context, address string) (*uuid.UUID, error)
}

// TokenSource is SkyMail's service account token (KEYCLOAK_SERVICE_CLIENT_ID).
type TokenSource interface {
	ServiceToken(ctx context.Context) (string, error)
}

const (
	// ResolvePath is core's tracking eligibility route (ticket 16).
	ResolvePath = "/internal/v1/communication-preferences/tracking:resolve"
	// ResolveBatch is the most people core takes in one call.
	ResolveBatch = 1000
	// maxResolveAnswer bounds what is read of one answer: 1000 people, each
	// a subject, a flag and a short reason, fit many times over.
	maxResolveAnswer = 1 << 20
)

// CoreClient asks core's tracking:resolve whom it lets be tracked: its
// account is enabled, it is a Member, and its communication preference (the
// account center's setting, opt-out by default, T1) does not say no. It sends
// SkyMail's service account token; core checks its audience and its core
// client role communication-preferences:read (ticket 17).
type CoreClient struct {
	endpoint string
	tokens   TokenSource
	http     *http.Client
}

// NewCoreClient asks core at coreURL (CORE_URL). A nil client is a plain
// http.Client with a 10 second timeout; the caller's context bounds a call
// as well.
func NewCoreClient(coreURL string, tokens TokenSource, client *http.Client) *CoreClient {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &CoreClient{endpoint: strings.TrimRight(coreURL, "/") + ResolvePath, tokens: tokens, http: client}
}

type resolveRequest struct {
	Subs []string `json:"subs"`
}

type resolveAnswer struct {
	Results *[]struct {
		Sub     string  `json:"sub"`
		Allowed bool    `json:"allowed"`
		Reason  *string `json:"reason"`
	} `json:"results"`
}

// Resolve asks core about each of subs once, ResolveBatch at a time. The
// answer must have every person asked about exactly once and no one else;
// anything less is an error. Errors never name a person.
func (c *CoreClient) Resolve(ctx context.Context, subs []uuid.UUID) (map[uuid.UUID]Decision, error) {
	unique := make([]uuid.UUID, 0, len(subs))
	seen := make(map[uuid.UUID]bool, len(subs))
	for _, sub := range subs {
		if !seen[sub] {
			seen[sub] = true
			unique = append(unique, sub)
		}
	}
	decisions := make(map[uuid.UUID]Decision, len(unique))
	for start := 0; start < len(unique); start += ResolveBatch {
		end := min(start+ResolveBatch, len(unique))
		if err := c.resolveBatch(ctx, unique[start:end], decisions); err != nil {
			return nil, err
		}
	}
	return decisions, nil
}

func (c *CoreClient) resolveBatch(ctx context.Context, batch []uuid.UUID, decisions map[uuid.UUID]Decision) error {
	token, err := c.tokens.ServiceToken(ctx)
	if err != nil {
		return fmt.Errorf("tracking preferences: service token: %w", err)
	}
	asked := make([]string, len(batch))
	for i, sub := range batch {
		asked[i] = sub.String()
	}
	body, err := json.Marshal(resolveRequest{Subs: asked})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("tracking preferences: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("tracking preferences: core did not answer: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResolveAnswer))
		return fmt.Errorf("tracking preferences: core answered %d", response.StatusCode)
	}
	var answer resolveAnswer
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResolveAnswer)).Decode(&answer); err != nil {
		return errors.New("tracking preferences: core's answer is not the contract's JSON")
	}
	if answer.Results == nil {
		return errors.New("tracking preferences: core's answer has no results")
	}

	wanted := make(map[uuid.UUID]bool, len(batch))
	for _, sub := range batch {
		wanted[sub] = true
	}
	answered := 0
	for _, result := range *answer.Results {
		sub, err := uuid.Parse(result.Sub)
		if err != nil {
			return errors.New("tracking preferences: core's answer names something that is not a subject")
		}
		if !wanted[sub] {
			return errors.New("tracking preferences: core's answer names someone not asked about, or someone twice")
		}
		delete(wanted, sub)
		answered++
		decision := Decision{Allowed: result.Allowed}
		if !result.Allowed {
			reason := ""
			if result.Reason != nil {
				reason = *result.Reason
			}
			decision.Reason = SkipReasonOf(reason)
		}
		decisions[sub] = decision
	}
	if answered != len(batch) {
		return fmt.Errorf("tracking preferences: core answered about %d of the %d people asked about", answered, len(batch))
	}
	return nil
}
