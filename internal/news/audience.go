package news

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ResolvePath is core's News audience (core docs/news-audience.md; role
// audience:read on SkyMail's service account, bilet 04).
const ResolvePath = "/v1/audiences/resolve"

const (
	// pageSize is the most recipients core answers in one page.
	pageSize = 500
	// maxPages bounds one audience: 50 000 people, far past the club.
	maxPages = 100
	// maxPage bounds what is read of one page: 500 items, each an address,
	// a name, two labels and a link, fit many times over.
	maxPage = 4 << 20
)

// The audience a send asks for.
const (
	AudienceTypeMembers = "members"
	AudienceTypeTeams   = "teams"
)

var (
	// ErrTeamUnknown: core says a named team is no team (400
	// audience_team_unknown).
	ErrTeamUnknown = errors.New("news audience: a named team is no team")
	// ErrVisibilityNotEnabled: core mails only public records yet (400
	// audience_visibility_not_enabled).
	ErrVisibilityNotEnabled = errors.New("news audience: core sends only public records yet")
)

// TokenSource is SkyMail's service account token (KEYCLOAK_SERVICE_CLIENT_ID).
type TokenSource interface {
	ServiceToken(ctx context.Context) (string, error)
}

// AudienceRequest is what core is asked.
type AudienceRequest struct {
	Type     string   `json:"type"`
	Teams    []string `json:"teams,omitempty"`
	NewsKind string   `json:"newsKind"`
	// Visibility is the record's audience.
	Visibility string `json:"visibility"`
}

// Person is one recipient as core answers them.
type Person struct {
	UserID         string   `json:"userId"`
	Email          string   `json:"email"`
	Name           string   `json:"name"`
	Categories     []string `json:"categories"`
	Via            string   `json:"via"`
	Reason         string   `json:"reason"`
	CategoryLabel  string   `json:"categoryLabel"`
	UnsubscribeURL string   `json:"unsubscribeUrl"`
}

// Counts are core's numbers for the audience.
type Counts struct {
	Members   int `json:"members"`
	Followers int `json:"followers"`
	Total     int `json:"total"`
}

// Skipped are who core left out, by why.
type Skipped struct {
	NotMember      int `json:"notMember"`
	NotVisible     int `json:"notVisible"`
	Disabled       int `json:"disabled"`
	NoEmail        int `json:"noEmail"`
	Unverified     int `json:"unverified"`
	OptedOut       int `json:"optedOut"`
	DuplicateEmail int `json:"duplicateEmail"`
}

// Audience is the whole audience, every page read.
type Audience struct {
	People  []Person
	Counts  Counts
	Skipped Skipped
}

// Core asks core with SkyMail's service account token.
type Core struct {
	base   string
	tokens TokenSource
	http   *http.Client
}

// NewCore asks core at coreURL (CORE_URL). A nil client is a plain
// http.Client with a 10 second timeout; the caller's context bounds a call
// as well.
func NewCore(coreURL string, tokens TokenSource, client *http.Client) *Core {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Core{base: strings.TrimRight(coreURL, "/"), tokens: tokens, http: client}
}

type audiencePage struct {
	Items   *[]Person `json:"items"`
	Next    *string   `json:"next"`
	Counts  *Counts   `json:"counts"`
	Skipped *Skipped  `json:"skipped"`
}

// Count answers core's numbers only (?countOnly=true): the live count of
// SkyMail's form. No address is read.
func (c *Core) Count(ctx context.Context, asked AudienceRequest) (Counts, Skipped, error) {
	var page audiencePage
	if err := c.call(ctx, url.Values{"countOnly": {"true"}}, asked, &page); err != nil {
		return Counts{}, Skipped{}, err
	}
	if page.Counts == nil || page.Skipped == nil {
		return Counts{}, Skipped{}, errors.New("news audience: core's answer has no counts")
	}
	return *page.Counts, *page.Skipped, nil
}

// Resolve reads the whole audience, page by page. Any error other than
// ErrTeamUnknown and ErrVisibilityNotEnabled (core unreachable, 503, an
// answer off the contract) is no audience at all. Errors never name a
// person.
func (c *Core) Resolve(ctx context.Context, asked AudienceRequest) (Audience, error) {
	var audience Audience
	after := ""
	for range maxPages {
		query := url.Values{"limit": {strconv.Itoa(pageSize)}}
		if after != "" {
			query.Set("after", after)
		}
		var page audiencePage
		if err := c.call(ctx, query, asked, &page); err != nil {
			return Audience{}, err
		}
		if page.Items == nil || page.Counts == nil || page.Skipped == nil {
			return Audience{}, errors.New("news audience: core's answer has no items or counts")
		}
		audience.People = append(audience.People, *page.Items...)
		// Every page counts the whole audience on its snapshot: the last
		// page's numbers are the send's.
		audience.Counts, audience.Skipped = *page.Counts, *page.Skipped
		if page.Next == nil {
			return audience, nil
		}
		if *page.Next == "" || *page.Next == after {
			return Audience{}, errors.New("news audience: core's answer does not move to a next page")
		}
		after = *page.Next
	}
	return Audience{}, fmt.Errorf("news audience: more than %d pages", maxPages)
}

type problem struct {
	Code string `json:"code"`
}

func (c *Core) call(ctx context.Context, query url.Values, asked AudienceRequest, answer any) error {
	token, err := c.tokens.ServiceToken(ctx)
	if err != nil {
		return fmt.Errorf("news audience: service token: %w", err)
	}
	body, err := json.Marshal(asked)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+ResolvePath+"?"+query.Encode(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("news audience: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxPage+1))
	if err != nil {
		return fmt.Errorf("news audience: %w", err)
	}
	if len(raw) > maxPage {
		return errors.New("news audience: core's answer is too large")
	}
	if response.StatusCode != http.StatusOK {
		var refused problem
		_ = json.Unmarshal(raw, &refused)
		switch {
		case response.StatusCode == http.StatusBadRequest && refused.Code == "audience_team_unknown":
			return ErrTeamUnknown
		case response.StatusCode == http.StatusBadRequest && refused.Code == "audience_visibility_not_enabled":
			return ErrVisibilityNotEnabled
		}
		return fmt.Errorf("news audience: status %d %s", response.StatusCode, refused.Code)
	}
	if err := json.Unmarshal(raw, answer); err != nil {
		return errors.New("news audience: core's answer is off the contract")
	}
	return nil
}
