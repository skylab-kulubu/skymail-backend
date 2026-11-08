// Package teams asks core which Teams a person leads, for Team mail
// (ADR-0067 addendum of 2026-11-08; hub tickets ekip-postasi 03 and 04).
//
// Core says it (GET /internal/v1/users/{sub}/teams, core
// docs/team-delegation.md "A person's Teams"), read from Keycloak's current
// membership with ADR-0065's rule: SkyMail reads neither Keycloak nor the
// token for it, and when core does not answer there is no Team mail; SkyMail
// never falls back to a reading of its own.
package teams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
)

// SwitchEnv opens Team mail: off (the default, also when unset) or on. It is
// SkyMail's own switch, not core's TEAM_DELEGATION.
const SwitchEnv = "SKYMAIL_TEAM_SEND"

// PersonTeamsPath is core's route; %s is the person's sub.
const PersonTeamsPath = "/internal/v1/users/%s/teams"

// RoleLeader is the role of a row of core's answer for a Team the person
// leads (and every Sub-team below one they lead).
const RoleLeader = "leader"

const (
	// CacheTTL is how long a person's answer is used by the routes' seat
	// check (Reader.Led); the checks that decide a send ask afresh.
	CacheTTL = 60 * time.Second
	// maxAnswer bounds what is read of core's answer: a person's Teams, a
	// few hundred bytes each.
	maxAnswer = 1 << 20
	// maxCached bounds the cache: past it, it starts over.
	maxCached = 10000
	// RetryAfter is the Retry-After of a request refused because core did
	// not answer.
	RetryAfter = 30
)

// ErrLeadershipUnavailable refuses Team mail when core did not say which
// Teams the person leads. Nothing else of SkyMail depends on core here.
var ErrLeadershipUnavailable = apperrors.New("team_mail.leadership_unavailable",
	"core did not say which Teams you lead, so Team mail cannot go on right now. Nothing was changed. Try again in a moment.",
	http.StatusServiceUnavailable).WithRetryAfter(RetryAfter)

// errUnavailable wraps every way core may fail to answer.
var errUnavailable = errors.New("teams: core did not say which Teams the person leads")

// DisplayName is a Team's name as core gives it.
type DisplayName struct {
	TR string `json:"tr"`
	EN string `json:"en,omitempty"`
}

// Team is one Team the person leads.
type Team struct {
	ID          uuid.UUID
	Path        string
	Code        string
	DisplayName DisplayName
}

// Reader says which Teams a person leads, in path order.
type Reader interface {
	// Led may answer from what core said up to CacheTTL ago: the routes'
	// seat check.
	Led(ctx context.Context, sub string) ([]Team, error)
	// LedNow asks core afresh: submitting, resubmitting and approving.
	LedNow(ctx context.Context, sub string) ([]Team, error)
}

// SwitchFromEnv reads SKYMAIL_TEAM_SEND; anything but off or on stops
// startup.
func SwitchFromEnv(getenv func(string) string) (bool, error) {
	switch raw := strings.ToLower(strings.TrimSpace(getenv(SwitchEnv))); raw {
	case "", "off":
		return false, nil
	case "on":
		return true, nil
	default:
		return false, fmt.Errorf("%s must be off or on, not %q", SwitchEnv, raw)
	}
}

// TokenSource is SkyMail's service account token (KEYCLOAK_SERVICE_CLIENT_ID,
// core's client role audience:read).
type TokenSource interface {
	ServiceToken(ctx context.Context) (string, error)
}

// Core asks core with SkyMail's service account token.
type Core struct {
	base   string
	tokens TokenSource
	http   *http.Client
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	teams []Team
	at    time.Time
}

// NewCore asks core at coreURL (CORE_URL). A nil client is a plain
// http.Client with a 5 second timeout; the caller's context bounds a call as
// well.
func NewCore(coreURL string, tokens TokenSource, client *http.Client) *Core {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &Core{base: strings.TrimRight(coreURL, "/"), tokens: tokens, http: client, now: time.Now, cache: map[string]cached{}}
}

// Led answers from the cache when core answered for sub within CacheTTL;
// otherwise as LedNow. A failure is never cached.
func (c *Core) Led(ctx context.Context, sub string) ([]Team, error) {
	c.mu.Lock()
	hit, ok := c.cache[sub]
	c.mu.Unlock()
	if ok && c.now().Sub(hit.at) < CacheTTL {
		return hit.teams, nil
	}
	return c.LedNow(ctx, sub)
}

// LedNow asks core and keeps the answer for Led.
func (c *Core) LedNow(ctx context.Context, sub string) ([]Team, error) {
	led, err := c.ask(ctx, sub)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) >= maxCached {
		c.cache = map[string]cached{}
	}
	c.cache[sub] = cached{teams: led, at: c.now()}
	return led, nil
}

type answerRow struct {
	ID          string       `json:"id"`
	Path        string       `json:"path"`
	Code        string       `json:"code"`
	DisplayName *DisplayName `json:"displayName"`
	Role        string       `json:"role"`
}

type answer struct {
	Teams *[]answerRow `json:"teams"`
}

func (c *Core) ask(ctx context.Context, sub string) ([]Team, error) {
	if _, err := uuid.Parse(sub); err != nil {
		return nil, fmt.Errorf("%w: the sub is no id", errUnavailable)
	}
	token, err := c.tokens.ServiceToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: service token: %v", errUnavailable, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+fmt.Sprintf(PersonTeamsPath, url.PathEscape(sub)), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUnavailable, err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		// The address carries the person's sub, which logs never hold.
		var failed *url.Error
		if errors.As(err, &failed) {
			err = fmt.Errorf("%s: %w", failed.Op, failed.Err)
		}
		return nil, fmt.Errorf("%w: %v", errUnavailable, err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxAnswer+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUnavailable, err)
	}
	if len(raw) > maxAnswer {
		return nil, fmt.Errorf("%w: the answer is too large", errUnavailable)
	}
	if response.StatusCode != http.StatusOK {
		var refused struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(raw, &refused)
		return nil, fmt.Errorf("%w: status %d %s", errUnavailable, response.StatusCode, refused.Code)
	}
	var got answer
	if err := json.Unmarshal(raw, &got); err != nil || got.Teams == nil {
		return nil, fmt.Errorf("%w: the answer is off the contract", errUnavailable)
	}
	return leaderRows(*got.Teams)
}

// leaderRows is the leader rows of core's answer, in path order. A row off
// the contract fails the whole answer: half an answer is no answer.
func leaderRows(rows []answerRow) ([]Team, error) {
	led := []Team{}
	seen := map[uuid.UUID]bool{}
	for _, row := range rows {
		id, err := uuid.Parse(row.ID)
		path := strings.TrimSpace(row.Path)
		if err != nil || !strings.HasPrefix(path, "/") || (row.Role != RoleLeader && row.Role != "member") {
			return nil, fmt.Errorf("%w: a row is off the contract", errUnavailable)
		}
		if row.Role != RoleLeader || seen[id] || LeaderGroupPath(path) {
			continue
		}
		seen[id] = true
		team := Team{ID: id, Path: path, Code: row.Code}
		if row.DisplayName != nil {
			team.DisplayName = *row.DisplayName
		}
		led = append(led, team)
	}
	sortByPath(led)
	return led, nil
}

// LeaderGroupPath says path is a Leader group's own (its last part LIDERLER
// or KOORDINATORLER): a group that is never a Team, and never Team mail's
// audience on its own. Core never answers one; SkyMail holds it too.
func LeaderGroupPath(path string) bool {
	last := path[strings.LastIndex(path, "/")+1:]
	return last == "LIDERLER" || last == "KOORDINATORLER"
}

func sortByPath(teams []Team) {
	slices.SortStableFunc(teams, func(a, b Team) int { return strings.Compare(a.Path, b.Path) })
}

// Within says path is below (or is) parent in the tree of paths.
func Within(path, parent string) bool {
	return path == parent || strings.HasPrefix(path, parent+"/")
}
