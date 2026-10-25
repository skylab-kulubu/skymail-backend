// Package consent reads what core says about contact consents (core
// docs/contact-consents.md; KVKK aydınlatma s6 §5.8, §5.10, §5.14) and which
// of a send's addresses belong to an account (core docs/news-audience.md,
// "Recipients"). SkyMail applies both as it takes rows to send (mailer,
// consent_gate.go).
package consent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// The purposes SkyMail reads the audience of.
const (
	// PurposePromotional: mails whose main purpose is promoting a product or
	// a service (s6 §5.10, row 13 b).
	PurposePromotional = "promotional"
	// PurposeEventInvitations: the club's other events and announcements to
	// a person without an account (s6 §5.14, row 14), on davet-v2.
	PurposeEventInvitations = "event_invitations"
	// TextInvitationsOnly is the event_invitations text that named future
	// event invitations alone: a grant given on it does not cover
	// announcements (core #289 review B6).
	TextInvitationsOnly = "davet-v1"
)

const (
	// AudiencePath is core's consent audience (role consent:audience:read).
	AudiencePath = "/v1/consents/audience"
	// ResolvePath is core's recipients:resolve (role audience:read).
	ResolvePath = "/internal/v1/audiences/recipients:resolve"
	// ResolveBatch is the most recipients core takes in one call.
	ResolveBatch = 1000

	audiencePage = 1000
	// maxAudiencePages bounds one read: 100 000 grants, far past the club.
	maxAudiencePages = 100
	// maxAnswer bounds what is read of one answer: 1000 items, each an
	// address and two links, fit many times over.
	maxAnswer = 4 << 20
	// maxProblem bounds what is read of a refusal for its code.
	maxProblem = 4 << 10
)

// codePattern is the shape of core's problem codes: a refusal's code is kept
// only when it is one, never free text.
var codePattern = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,79}$`)

// TokenSource is SkyMail's service account token (KEYCLOAK_SERVICE_CLIENT_ID).
type TokenSource interface {
	ServiceToken(ctx context.Context) (string, error)
}

// Item is one open, confirmed grant of the audience.
type Item struct {
	// Email is the address the grant mails, trimmed and lower-cased.
	Email       string
	TextVersion string
	// WithdrawURL ends every open grant of the purpose that mails Email;
	// it goes into the mail's footer and List-Unsubscribe.
	WithdrawURL string
	// RenewalDue: the grant is due its renewal question
	// (event_invitations only).
	RenewalDue bool
}

// Client asks core with SkyMail's service account token.
type Client struct {
	base   string
	tokens TokenSource
	http   *http.Client
}

// NewClient asks core at coreURL (CORE_URL). A nil client is a plain
// http.Client with a 10 second timeout; the caller's context bounds a call
// as well.
func NewClient(coreURL string, tokens TokenSource, client *http.Client) *Client {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{base: strings.TrimRight(coreURL, "/"), tokens: tokens, http: client}
}

// NormalizeAddress is an address as core compares it: trimmed, lower-case.
func NormalizeAddress(address string) string {
	return strings.ToLower(strings.TrimSpace(address))
}

type audienceAnswer struct {
	Items *[]struct {
		Email       string `json:"email"`
		TextVersion string `json:"textVersion"`
		WithdrawURL string `json:"withdrawUrl"`
		RenewalDue  bool   `json:"renewalDue"`
	} `json:"items"`
	Next *string `json:"next"`
}

// Audience reads every open, confirmed grant of purpose, page by page. Any
// error (core unreachable, 503 while CONTACT_CONSENT_KEY is unset, an answer
// off the contract) is no audience at all. Errors never name a person.
func (c *Client) Audience(ctx context.Context, purpose string) ([]Item, error) {
	var items []Item
	after := ""
	for page := 0; page < maxAudiencePages; page++ {
		query := url.Values{"purpose": {purpose}, "limit": {fmt.Sprint(audiencePage)}}
		if after != "" {
			query.Set("after", after)
		}
		var answer audienceAnswer
		if err := c.call(ctx, http.MethodGet, AudiencePath+"?"+query.Encode(), nil, &answer); err != nil {
			return nil, fmt.Errorf("consent audience: %w", err)
		}
		if answer.Items == nil {
			return nil, errors.New("consent audience: core's answer has no items")
		}
		for _, item := range *answer.Items {
			email := NormalizeAddress(item.Email)
			if email == "" {
				return nil, errors.New("consent audience: core's answer has an item without an address")
			}
			items = append(items, Item{Email: email, TextVersion: item.TextVersion, WithdrawURL: item.WithdrawURL, RenewalDue: item.RenewalDue})
		}
		if answer.Next == nil {
			return items, nil
		}
		if *answer.Next == "" || *answer.Next == after {
			return nil, errors.New("consent audience: core's answer does not move to a next page")
		}
		after = *answer.Next
	}
	return nil, fmt.Errorf("consent audience: more than %d pages", maxAudiencePages)
}

// The reasons core gives for allowed false (core docs/news-audience.md,
// "Decision").
const (
	ReasonOptedOut  = "opted_out"
	ReasonInactive  = "inactive"
	ReasonAmbiguous = "ambiguous"
)

// ErrCategoryUnknown is core's 400 recipients_resolve_category_unknown: the
// send's team category names a Group that is no team's own.
var ErrCategoryUnknown = errors.New("recipients resolve: the category names no team")

// Recipient is one recipient asked about: by Keycloak subject when the send
// knows it, else by address. Exactly one is set.
type Recipient struct {
	Email string `json:"email,omitempty"`
	Sub   string `json:"sub,omitempty"`
}

// Result is core's answer about one recipient (ADR 0072 decision 3).
type Result struct {
	// Matched: the recipient is an account's (by sub, or a verified address).
	// Not matched, SkyMail's own record and link apply.
	Matched bool
	// Sub is the account's subject; empty for ambiguous.
	Sub string
	// Allowed: the mail may go, with UnsubscribeURL, the person's one-click
	// link for exactly the category asked.
	Allowed        bool
	Reason         string
	UnsubscribeURL string
}

type resolveRequest struct {
	Channel    string      `json:"channel"`
	Category   string      `json:"category"`
	Recipients []Recipient `json:"recipients"`
}

type resolvedItem struct {
	Matched        bool    `json:"matched"`
	Sub            string  `json:"sub"`
	Allowed        bool    `json:"allowed"`
	Reason         *string `json:"reason"`
	UnsubscribeURL string  `json:"unsubscribeUrl"`
}

type resolveAnswer struct {
	Results *[]resolvedItem `json:"results"`
}

// Resolve asks core, in one call, about at most ResolveBatch recipients for
// category (core docs/news-audience.md, "Recipients"): one result per
// recipient, in the order asked. Every error is no answer for anyone, and
// names no person: ErrCategoryUnknown for a team category core does not
// know; any other refusal, core not answering, or an answer off the contract
// (a short list, a reason core does not give, an allowed result without its
// link) is not knowing.
func (c *Client) Resolve(ctx context.Context, category string, recipients []Recipient) ([]Result, error) {
	items, err := c.resolve(ctx, category, recipients)
	if err != nil {
		return nil, err
	}
	results := make([]Result, len(items))
	for i, got := range items {
		if !got.Matched {
			continue
		}
		result := Result{Matched: true, Sub: got.Sub, Allowed: got.Allowed}
		if got.Reason != nil {
			result.Reason = *got.Reason
		}
		switch {
		case result.Allowed && (got.UnsubscribeURL == "" || result.Reason != ""):
			return nil, errors.New("recipients resolve: an allowed result without its link")
		case result.Allowed:
			result.UnsubscribeURL = got.UnsubscribeURL
		case result.Reason != ReasonOptedOut && result.Reason != ReasonInactive && result.Reason != ReasonAmbiguous:
			return nil, errors.New("recipients resolve: a reason core does not give")
		}
		results[i] = result
	}
	return results, nil
}

// resolve is one recipients:resolve call: core's answer about each
// recipient, in the order asked.
func (c *Client) resolve(ctx context.Context, category string, recipients []Recipient) ([]resolvedItem, error) {
	if len(recipients) == 0 || len(recipients) > ResolveBatch {
		return nil, fmt.Errorf("recipients resolve: %d recipients, want 1 to %d", len(recipients), ResolveBatch)
	}
	var answer resolveAnswer
	err := c.call(ctx, http.MethodPost, ResolvePath, resolveRequest{Channel: "email", Category: category, Recipients: recipients}, &answer)
	var refused *StatusError
	if errors.As(err, &refused) && refused.Status == http.StatusBadRequest && refused.Code == "recipients_resolve_category_unknown" {
		return nil, ErrCategoryUnknown
	}
	if err != nil {
		return nil, fmt.Errorf("recipients resolve: %w", err)
	}
	if answer.Results == nil || len(*answer.Results) != len(recipients) {
		return nil, errors.New("recipients resolve: core did not answer about every recipient asked about")
	}
	return *answer.Results, nil
}

// Matched says, for each of addresses (normalized; each asked once), whether
// it is an account's verified address (core's address directory: the primary
// e-mail, a YTÜ-linked school address, a verified personal one; an address of
// two accounts counts as matched). category is the send's core category
// (club:duyuru when the send names none). Any error is no answer for anyone.
func (c *Client) Matched(ctx context.Context, category string, addresses []string) (map[string]bool, error) {
	unique := make([]string, 0, len(addresses))
	matched := make(map[string]bool, len(addresses))
	seen := make(map[string]bool, len(addresses))
	for _, address := range addresses {
		address = NormalizeAddress(address)
		if address != "" && !seen[address] {
			seen[address] = true
			unique = append(unique, address)
		}
	}
	for start := 0; start < len(unique); start += ResolveBatch {
		batch := unique[start:min(start+ResolveBatch, len(unique))]
		asked := make([]Recipient, len(batch))
		for i, address := range batch {
			asked[i] = Recipient{Email: address}
		}
		results, err := c.resolve(ctx, category, asked)
		if err != nil {
			return nil, err
		}
		for i, result := range results {
			matched[batch[i]] = result.Matched
		}
	}
	return matched, nil
}

// StatusError is core answering something other than 200: its status and,
// when its body is a problem document, its code. Never a person.
type StatusError struct {
	Status int
	Code   string
}

func (e *StatusError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("core answered %d %s", e.Status, e.Code)
	}
	return fmt.Sprintf("core answered %d", e.Status)
}

// call sends one request to core and decodes a 200 answer into answer.
func (c *Client) call(ctx context.Context, method, path string, body, answer any) error {
	token, err := c.tokens.ServiceToken(ctx)
	if err != nil {
		return fmt.Errorf("service token: %w", err)
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("core did not answer: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		refused := &StatusError{Status: response.StatusCode}
		var problem struct {
			Code string `json:"code"`
		}
		if json.NewDecoder(io.LimitReader(response.Body, maxProblem)).Decode(&problem) == nil && codePattern.MatchString(problem.Code) {
			refused.Code = problem.Code
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxAnswer))
		return refused
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAnswer)).Decode(answer); err != nil {
		return errors.New("core's answer is not the contract's JSON")
	}
	return nil
}
