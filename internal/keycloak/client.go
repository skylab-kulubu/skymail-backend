package keycloak

import (
	"context"
	"errors"
	"fmt"
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
	return c.gc.GetGroups(ctx, token, c.realm, gocloak.GetGroupsParams{Max: &max})
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
	max := 10000
	return c.gc.GetGroupMembers(ctx, token, c.realm, id, gocloak.GetGroupsParams{Max: &max})
}
