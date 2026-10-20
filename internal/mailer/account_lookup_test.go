package mailer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// slowAccounts is Keycloak finding accounts by address: each search waits
// for delay (or its context), and it records how many ran at once. Addresses
// in fail fail; those in accounts have one.
type slowAccounts struct {
	delay    time.Duration
	accounts map[string]uuid.UUID
	fail     map[string]bool

	mu        sync.Mutex
	running   int
	most      int
	addresses []string
}

func (s *slowAccounts) VerifiedAccountByEmail(ctx context.Context, address string) (*uuid.UUID, error) {
	s.mu.Lock()
	s.running++
	if s.running > s.most {
		s.most = s.running
	}
	s.addresses = append(s.addresses, address)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running--
		s.mu.Unlock()
	}()
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.fail[address] {
		return nil, errors.New("keycloak did not answer")
	}
	if sub, ok := s.accounts[address]; ok {
		return &sub, nil
	}
	return nil, nil
}

func lookupMailer(accounts *slowAccounts) *mailerImpl {
	logger := zerolog.Nop()
	m := &mailerImpl{logger: &logger}
	if accounts != nil {
		m.tracking.Accounts = accounts
	}
	return m
}

// Keycloak is asked about each address once, at most accountLookups at a
// time; what it answers is kept, a failure leaves the address out.
func TestAccountsAreLookedUpAFewAtATime(t *testing.T) {
	accounts := &slowAccounts{delay: 20 * time.Millisecond, accounts: map[string]uuid.UUID{}, fail: map[string]bool{"k3@ornek.com": true}}
	var addresses []string
	for i := 0; i < 23; i++ {
		address := fmt.Sprintf("k%d@ornek.com", i)
		addresses = append(addresses, address)
		if i%2 == 0 {
			accounts.accounts[address] = uuid.New()
		}
	}
	found := lookupMailer(accounts).lookUpAccounts(context.Background(), addresses)
	if accounts.most > accountLookups || accounts.most < 2 {
		t.Errorf("%d searches ran at once; want between 2 and %d", accounts.most, accountLookups)
	}
	if len(accounts.addresses) != len(addresses) {
		t.Errorf("Keycloak asked %d times about %d addresses", len(accounts.addresses), len(addresses))
	}
	for i, address := range addresses {
		sub, answered := found[address]
		switch {
		case address == "k3@ornek.com":
			if answered {
				t.Errorf("%s failed but is answered %v", address, sub)
			}
		case i%2 == 0:
			if !answered || sub == nil || *sub != accounts.accounts[address] {
				t.Errorf("%s = %v, %v; want its account", address, sub, answered)
			}
		default:
			if !answered || sub != nil {
				t.Errorf("%s = %v, %v; want answered with no account", address, sub, answered)
			}
		}
	}
}

// A Keycloak too slow for the send's time leaves the addresses unanswered,
// and the send does not wait past it.
func TestAccountLookupsStopAtTheSendsTime(t *testing.T) {
	accounts := &slowAccounts{delay: time.Minute}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	found := lookupMailer(accounts).lookUpAccounts(ctx, []string{"a@ornek.com", "b@ornek.com", "c@ornek.com", "d@ornek.com", "e@ornek.com", "f@ornek.com", "g@ornek.com"})
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("waited %v for a Keycloak that does not answer", waited)
	}
	if len(found) != 0 {
		t.Errorf("unanswered addresses came back answered: %v", found)
	}
}

// Without Keycloak no address is answered: not "no account", unknown.
func TestNoKeycloakAnswersNoAddress(t *testing.T) {
	if found := lookupMailer(nil).lookUpAccounts(context.Background(), []string{"a@ornek.com"}); len(found) != 0 {
		t.Errorf("= %v; want nothing answered", found)
	}
}

// accountsBudget is the send's time for Keycloak.
func TestAccountLookupsHaveABudget(t *testing.T) {
	if accountsBudget <= 0 || accountsBudget > 5*time.Second || accountLookups < 1 || accountLookups > 5 {
		t.Errorf("budget %v, %d at once; want at most 5s and 1..5", accountsBudget, accountLookups)
	}
}
