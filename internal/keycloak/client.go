package keycloak

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Nerzal/gocloak/v13"
)

type Client interface {
	ListGroups(ctx context.Context) ([]*gocloak.Group, error)
	GetGroup(ctx context.Context, id string) (*gocloak.Group, error)
	GetGroupMembers(ctx context.Context, id string) ([]*gocloak.User, error)
	// ClientRoleMembers lists the enabled users who hold role on the client
	// whose clientId is clientID: directly, or through a group they or a
	// group above theirs is in. Each is listed once. A role given through a
	// composite role is not followed.
	ClientRoleMembers(ctx context.Context, clientID, role string) ([]*gocloak.User, error)
}

type clientImpl struct {
	gc *gocloak.GoCloak
	// adminBase is where Admin REST (/admin/realms/<realm>/…) and the
	// service account's token request go.
	adminBase    string
	clientID     string
	clientSecret string
	realm        string

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

// NewClient reads the realm realmURL (KEYCLOAK_REALM_URL,
// <base>/realms/<realm>) names through Admin REST with the service account
// clientID. Admin REST and the service account's token request go to
// adminURL (KEYCLOAK_ADMIN_URL, e.g. Keycloak inside the Docker network) when
// it is not empty, and to realmURL's base when it is. Keycloak names its
// public address in iss whichever address issued the token, and SkyMail
// never checks this token itself: Keycloak does.
func NewClient(realmURL, adminURL, clientID, clientSecret string) Client {
	parts := strings.SplitN(realmURL, "/realms/", 2)
	baseURL := parts[0]
	realm := ""
	if len(parts) == 2 {
		realm = parts[1]
	}
	if admin := keycloakBase(adminURL); admin != "" {
		baseURL = admin
	}

	return &clientImpl{
		// gocloak sends the token request and its own Admin REST calls to
		// baseURL.
		gc:           gocloak.NewClient(baseURL),
		adminBase:    baseURL,
		clientID:     clientID,
		clientSecret: clientSecret,
		realm:        realm,
	}
}

// keycloakBase is a Keycloak base URL without surrounding blanks, trailing
// slashes or a /realms/<realm> suffix.
func keycloakBase(raw string) string {
	base := strings.TrimRight(strings.TrimSpace(raw), "/")
	if before, _, found := strings.Cut(base, "/realms/"); found {
		base = strings.TrimRight(before, "/")
	}
	return base
}

// ParseAdminURL checks KEYCLOAK_ADMIN_URL and answers the base SkyMail sends
// Admin REST to: "" when it is unset or blank, otherwise an absolute http(s)
// URL without trailing slashes or a /realms/<realm> suffix, e.g.
// http://keycloak:8080. A context path is kept. The errors name the
// variable, never its value.
func ParseAdminURL(raw string) (string, error) {
	base := keycloakBase(raw)
	if base == "" {
		return "", nil
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", errors.New("KEYCLOAK_ADMIN_URL is not a URL")
	}
	if scheme := strings.ToLower(u.Scheme); (scheme != "http" && scheme != "https") || u.Host == "" || u.Opaque != "" {
		return "", errors.New("KEYCLOAK_ADMIN_URL must be an absolute http or https URL, e.g. http://keycloak:8080")
	}
	if u.User != nil {
		return "", errors.New("KEYCLOAK_ADMIN_URL must not carry credentials")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("KEYCLOAK_ADMIN_URL must not have a query or a fragment")
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "admin" || segment == "realms" {
			return "", errors.New("KEYCLOAK_ADMIN_URL is Keycloak's base URL; SkyMail adds /admin/realms/<realm> itself")
		}
	}
	return base, nil
}

func (c *clientImpl) getToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != "" && time.Now().Before(c.tokenExpiry) {
		return c.token, nil
	}

	jwt, err := c.gc.LoginClient(ctx, c.clientID, c.clientSecret, c.realm)
	if err != nil {
		return "", fmt.Errorf("keycloak: login failed: %w", err)
	}

	c.token = jwt.AccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(jwt.ExpiresIn-30) * time.Second)
	return c.token, nil
}

func (c *clientImpl) ListGroups(ctx context.Context) ([]*gocloak.Group, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}
	max := 1000
	full := true
	tops, err := c.gc.GetGroups(ctx, token, c.realm, gocloak.GetGroupsParams{Max: &max, Full: &full})
	if err != nil {
		return nil, err
	}
	return c.collectGroups(ctx, token, tops)
}

func (c *clientImpl) collectGroups(ctx context.Context, token string, gs []*gocloak.Group) ([]*gocloak.Group, error) {
	seen := map[string]struct{}{}
	out := make([]*gocloak.Group, 0)
	var walk func([]*gocloak.Group) error
	walk = func(nodes []*gocloak.Group) error {
		for _, g := range nodes {
			if g == nil || g.ID == nil {
				continue
			}
			if _, ok := seen[*g.ID]; ok {
				continue
			}
			seen[*g.ID] = struct{}{}
			out = append(out, g)
			if g.SubGroups != nil {
				nested := make([]*gocloak.Group, 0, len(*g.SubGroups))
				for i := range *g.SubGroups {
					nested = append(nested, &(*g.SubGroups)[i])
				}
				if err := walk(nested); err != nil {
					return err
				}
			}
			kids, err := c.children(ctx, token, *g.ID)
			if err != nil {
				return err
			}
			if err := walk(kids); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(gs); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *clientImpl) children(ctx context.Context, token, groupID string) ([]*gocloak.Group, error) {
	out := make([]*gocloak.Group, 0)
	first := 0
	const pageSize = 100
	for {
		var page []*gocloak.Group
		resp, err := c.gc.GetRequestWithBearerAuth(ctx, token).
			SetResult(&page).
			SetQueryParams(map[string]string{
				"first":               strconv.Itoa(first),
				"max":                 strconv.Itoa(pageSize),
				"briefRepresentation": "false",
			}).
			Get(c.adminBase + "/admin/realms/" + c.realm + "/groups/" + groupID + "/children")
		if err != nil {
			return nil, err
		}
		if resp.IsError() {
			if resp.StatusCode() == 404 {
				return []*gocloak.Group{}, nil
			}
			return nil, errors.New("keycloak: children failed")
		}
		out = append(out, page...)
		if len(page) < pageSize {
			return out, nil
		}
		first += len(page)
	}
}

func (c *clientImpl) GetGroup(ctx context.Context, id string) (*gocloak.Group, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}

	group, err := c.gc.GetGroup(ctx, token, c.realm, id)
	if err != nil {
		var apiErr gocloak.APIError
		if errors.As(err, &apiErr) && apiErr.Code == 404 {
			return nil, nil
		}
		return nil, err
	}
	return group, nil
}

func (c *clientImpl) GetGroupMembers(ctx context.Context, id string) ([]*gocloak.User, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}
	return c.groupMembers(ctx, token, id, map[string]struct{}{})
}

// groupMembers lists the members of a group and of every group below it, each
// once; seen carries the users already listed, so several groups can be
// gathered into one list.
func (c *clientImpl) groupMembers(ctx context.Context, token, id string, seen map[string]struct{}) ([]*gocloak.User, error) {
	root, err := c.gc.GetGroup(ctx, token, c.realm, id)
	if err != nil {
		return nil, err
	}
	groups, err := c.collectGroups(ctx, token, []*gocloak.Group{root})
	if err != nil {
		return nil, err
	}
	out := make([]*gocloak.User, 0)
	max := 10000
	for _, g := range groups {
		if g == nil || g.ID == nil {
			continue
		}
		members, err := c.gc.GetGroupMembers(ctx, token, c.realm, *g.ID, gocloak.GetGroupsParams{Max: &max})
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			if m == nil || m.ID == nil {
				continue
			}
			if _, ok := seen[*m.ID]; ok {
				continue
			}
			seen[*m.ID] = struct{}{}
			out = append(out, m)
		}
	}
	return out, nil
}

func (c *clientImpl) ClientRoleMembers(ctx context.Context, clientID, role string) ([]*gocloak.User, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}
	clients, err := c.gc.GetClients(ctx, token, c.realm, gocloak.GetClientsParams{ClientID: &clientID})
	if err != nil {
		return nil, err
	}
	if len(clients) == 0 || clients[0] == nil || clients[0].ID == nil {
		return nil, fmt.Errorf("keycloak: no client %q", clientID)
	}
	idOfClient := *clients[0].ID

	seen := map[string]struct{}{}
	holders := make([]*gocloak.User, 0)
	const pageSize = 100
	for first := 0; ; first += pageSize {
		page, err := c.gc.GetUsersByClientRoleName(ctx, token, c.realm, idOfClient, role, gocloak.GetUsersByRoleParams{
			First: gocloak.IntP(first),
			Max:   gocloak.IntP(pageSize),
		})
		if err != nil {
			return nil, err
		}
		for _, u := range page {
			if u == nil || u.ID == nil {
				continue
			}
			if _, ok := seen[*u.ID]; ok {
				continue
			}
			seen[*u.ID] = struct{}{}
			holders = append(holders, u)
		}
		if len(page) < pageSize {
			break
		}
	}

	// A group's role mapping reaches every group below it, which is what
	// groupMembers walks.
	groups, err := c.roleGroups(ctx, token, idOfClient, role)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if g == nil || g.ID == nil {
			continue
		}
		members, err := c.groupMembers(ctx, token, *g.ID, seen)
		if err != nil {
			return nil, err
		}
		holders = append(holders, members...)
	}

	enabled := make([]*gocloak.User, 0, len(holders))
	for _, u := range holders {
		if u.Enabled != nil && !*u.Enabled {
			continue
		}
		enabled = append(enabled, u)
	}
	return enabled, nil
}

// roleGroups lists the groups a client role is mapped to, page by page as the
// role's users are read.
func (c *clientImpl) roleGroups(ctx context.Context, token, idOfClient, role string) ([]*gocloak.Group, error) {
	out := make([]*gocloak.Group, 0)
	const pageSize = 100
	for first := 0; ; first += pageSize {
		var page []*gocloak.Group
		resp, err := c.gc.GetRequestWithBearerAuth(ctx, token).
			SetResult(&page).
			SetQueryParams(map[string]string{
				"first":               strconv.Itoa(first),
				"max":                 strconv.Itoa(pageSize),
				"briefRepresentation": "true",
			}).
			Get(c.adminBase + "/admin/realms/" + c.realm + "/clients/" + idOfClient + "/roles/" + url.PathEscape(role) + "/groups")
		if err != nil {
			return nil, err
		}
		if resp.IsError() {
			return nil, fmt.Errorf("keycloak: groups of role %q: %s", role, resp.Status())
		}
		out = append(out, page...)
		if len(page) < pageSize {
			return out, nil
		}
	}
}
