package keycloak

import (
	"context"
	"errors"
	"fmt"
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
	gc           *gocloak.GoCloak
	base         string
	clientID     string
	clientSecret string
	realm        string

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

func NewClient(realmURL, clientID, clientSecret string) Client {
	parts := strings.SplitN(realmURL, "/realms/", 2)
	baseURL := parts[0]
	realm := ""
	if len(parts) == 2 {
		realm = parts[1]
	}

	return &clientImpl{
		gc:           gocloak.NewClient(baseURL),
		base:         baseURL,
		clientID:     clientID,
		clientSecret: clientSecret,
		realm:        realm,
	}
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
			Get(c.base + "/admin/realms/" + c.realm + "/groups/" + groupID + "/children")
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
	groups, err := c.gc.GetGroupsByClientRole(ctx, token, c.realm, role, idOfClient)
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
