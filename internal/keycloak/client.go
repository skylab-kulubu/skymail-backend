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
	root, err := c.gc.GetGroup(ctx, token, c.realm, id)
	if err != nil {
		return nil, err
	}
	groups, err := c.collectGroups(ctx, token, []*gocloak.Group{root})
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
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
